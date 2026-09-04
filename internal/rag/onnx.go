package rag

import (
	"context"
	"fmt"
	"math"
	"os"
	"runtime"
	"sync"

	"github.com/daulet/tokenizers"
	ort "github.com/yalue/onnxruntime_go"
)

// ONNXEmbedder runs the embedding model in this process, on the CPU.
//
// Anthropic has no embedding API, so a RAG path either runs a model locally or takes a
// dependency on a second vendor. In-process costs nothing per query and needs no second
// API key. What it costs instead is cgo: ONNX Runtime is a native library and the
// tokenizer is a Rust static library, so the build needs both present and the binary is
// no longer statically linked or trivially cross-compiled. See docs/retrieval.md for
// what that measured out to.
//
// Both native libraries are safe to call concurrently — ORT's Run is documented
// thread-safe and the Rust tokenizer encodes through an immutable reference — and
// TestONNXEmbedderIsConcurrencySafe checks it under -race rather than trusting the
// documentation, because the benchmark puts a thousand goroutines through here at once.
type ONNXEmbedder struct {
	tokenizer  *tokenizers.Tokenizer
	session    *ort.DynamicAdvancedSession
	dimensions int

	queryPrefix   string
	passagePrefix string

	closeOnce sync.Once
}

type ONNXOptions struct {
	ModelPath     string
	TokenizerPath string
	Dimensions    int
	QueryPrefix   string
	PassagePrefix string
	// SharedLibraryPath points at libonnxruntime; empty means "guess from the platform".
	SharedLibraryPath string
}

// e5 is trained at 512 tokens. Anything longer is truncated rather than rejected: an
// over-long FAQ answer should still be findable.
const maxSequenceLength = 512

var (
	ortOnce sync.Once
	ortErr  error
)

// initORT initialises the ONNX Runtime environment exactly once per process. It is
// global state in the C library, and initialising it twice is an error.
func initORT(libraryPath string) error {
	ortOnce.Do(func() {
		if libraryPath == "" {
			libraryPath = defaultSharedLibraryPath()
		}
		if libraryPath != "" {
			ort.SetSharedLibraryPath(libraryPath)
		}
		ortErr = ort.InitializeEnvironment()
	})
	return ortErr
}

func defaultSharedLibraryPath() string {
	if p := os.Getenv("ONNXRUNTIME_LIB_PATH"); p != "" {
		return p
	}
	candidates := []string{
		"third_party/onnxruntime/lib/libonnxruntime.so",
		"/usr/local/lib/libonnxruntime.so",
	}
	if runtime.GOOS == "darwin" {
		candidates = []string{
			"third_party/onnxruntime/lib/libonnxruntime.dylib",
			"/opt/homebrew/opt/onnxruntime/lib/libonnxruntime.dylib",
			"/usr/local/opt/onnxruntime/lib/libonnxruntime.dylib",
		}
	}
	for _, c := range candidates {
		if _, err := os.Stat(c); err == nil {
			return c
		}
	}
	return ""
}

func NewONNXEmbedder(opts ONNXOptions) (*ONNXEmbedder, error) {
	if err := initORT(opts.SharedLibraryPath); err != nil {
		return nil, fmt.Errorf("initialise ONNX Runtime (set ONNXRUNTIME_LIB_PATH, or run `make deps`): %w", err)
	}

	raw, err := os.ReadFile(opts.TokenizerPath)
	if err != nil {
		return nil, fmt.Errorf("read tokenizer %s: %w", opts.TokenizerPath, err)
	}
	tokenizer, err := tokenizers.FromBytesWithTruncation(raw, maxSequenceLength, tokenizers.TruncationDirectionRight)
	if err != nil {
		return nil, fmt.Errorf("load tokenizer %s: %w", opts.TokenizerPath, err)
	}

	session, err := ort.NewDynamicAdvancedSession(opts.ModelPath,
		[]string{"input_ids", "attention_mask", "token_type_ids"},
		[]string{"last_hidden_state"}, nil)
	if err != nil {
		tokenizer.Close()
		return nil, fmt.Errorf("load embedding model %s: %w", opts.ModelPath, err)
	}

	return &ONNXEmbedder{
		tokenizer:     tokenizer,
		session:       session,
		dimensions:    opts.Dimensions,
		queryPrefix:   opts.QueryPrefix,
		passagePrefix: opts.PassagePrefix,
	}, nil
}

func (e *ONNXEmbedder) Dimensions() int { return e.dimensions }

func (e *ONNXEmbedder) EmbedQuery(ctx context.Context, query string) ([]float32, error) {
	vectors, err := e.embed(ctx, []string{e.queryPrefix + query})
	if err != nil {
		return nil, err
	}
	return vectors[0], nil
}

func (e *ONNXEmbedder) EmbedPassages(ctx context.Context, passages []string) ([][]float32, error) {
	prefixed := make([]string, len(passages))
	for i, p := range passages {
		prefixed[i] = e.passagePrefix + p
	}
	return e.embed(ctx, prefixed)
}

func (e *ONNXEmbedder) Close() error {
	e.closeOnce.Do(func() {
		e.session.Destroy()
		e.tokenizer.Close()
	})
	return nil
}

// embed tokenises, runs one batched forward pass, mean-pools over the unmasked
// positions and L2-normalises. Mean pooling is what e5 was trained with; taking the
// [CLS] vector instead would produce plausible-looking numbers that rank badly.
func (e *ONNXEmbedder) embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return nil, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	batch := len(texts)
	encodings := make([]tokenizers.Encoding, batch)
	longest := 0
	for i, text := range texts {
		enc := e.tokenizer.EncodeWithOptions(text, true,
			tokenizers.WithReturnAttentionMask(), tokenizers.WithReturnTypeIDs())
		if len(enc.IDs) == 0 {
			return nil, fmt.Errorf("tokenizer produced no tokens for input %d", i)
		}
		encodings[i] = enc
		if len(enc.IDs) > longest {
			longest = len(enc.IDs)
		}
	}

	// Padded positions carry attention_mask 0, so they contribute nothing to the mean.
	ids := make([]int64, batch*longest)
	mask := make([]int64, batch*longest)
	types := make([]int64, batch*longest)
	for i, enc := range encodings {
		for j := range enc.IDs {
			ids[i*longest+j] = int64(enc.IDs[j])
			mask[i*longest+j] = int64(enc.AttentionMask[j])
		}
	}

	shape := ort.NewShape(int64(batch), int64(longest))
	inputIDs, err := ort.NewTensor(shape, ids)
	if err != nil {
		return nil, err
	}
	defer inputIDs.Destroy()
	attention, err := ort.NewTensor(shape, mask)
	if err != nil {
		return nil, err
	}
	defer attention.Destroy()
	tokenTypes, err := ort.NewTensor(shape, types)
	if err != nil {
		return nil, err
	}
	defer tokenTypes.Destroy()

	output, err := ort.NewEmptyTensor[float32](ort.NewShape(int64(batch), int64(longest), int64(e.dimensions)))
	if err != nil {
		return nil, err
	}
	defer output.Destroy()

	if err := e.session.Run(
		[]ort.Value{inputIDs, attention, tokenTypes},
		[]ort.Value{output}); err != nil {
		return nil, fmt.Errorf("embedding forward pass: %w", err)
	}

	hidden := output.GetData()
	vectors := make([][]float32, batch)
	for i := range vectors {
		vec := make([]float32, e.dimensions)
		var counted float32
		for j := 0; j < longest; j++ {
			if mask[i*longest+j] == 0 {
				continue
			}
			counted++
			offset := (i*longest + j) * e.dimensions
			for d := 0; d < e.dimensions; d++ {
				vec[d] += hidden[offset+d]
			}
		}
		var norm float64
		for d := range vec {
			vec[d] /= counted
			norm += float64(vec[d]) * float64(vec[d])
		}
		norm = math.Sqrt(norm)
		if norm > 0 {
			for d := range vec {
				vec[d] /= float32(norm)
			}
		}
		vectors[i] = vec
	}
	return vectors, nil
}
