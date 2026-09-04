//go:build benchmark

// Package benchmark measures what the runtime does with a thousand in-flight requests.
//
// Excluded from the normal build: it measures a machine rather than asserting a
// behaviour. Run it with `make bench`.
//
// The parameters match the Java implementation's exactly, so the numbers can be put
// side by side: 1000 concurrent requests, a stubbed model with a fixed 1000 ms delay,
// the full production request path -- validation, conversation memory in Postgres,
// query embedding, a vector search, metrics -- and one fresh conversation per request.
package benchmark

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"runtime/pprof"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lai3d/ai-customer-service-go/internal/chat"
	"github.com/lai3d/ai-customer-service-go/internal/config"
	"github.com/lai3d/ai-customer-service-go/internal/cost"
	"github.com/lai3d/ai-customer-service-go/internal/httpapi"
	"github.com/lai3d/ai-customer-service-go/internal/llm"
	"github.com/lai3d/ai-customer-service-go/internal/obs"
	"github.com/lai3d/ai-customer-service-go/internal/rag"
	"github.com/lai3d/ai-customer-service-go/internal/testsupport"
	"github.com/lai3d/ai-customer-service-go/internal/tools"
)

const (
	concurrency = 1000
	modelDelay  = 1000 * time.Millisecond
	dims        = 384
)

var pool *pgxpool.Pool

func TestMain(m *testing.M) {
	ctx := context.Background()
	p, stop, err := testsupport.StartPostgres(ctx, dims)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	pool = p
	code := m.Run()
	stop()
	os.Exit(code)
}

// slowModel is the stub. An LLM call is mostly waiting, and a real one would add cost,
// network variance and rate limits to a measurement about scheduling.
type slowModel struct{}

func (slowModel) Provider() string { return "stub" }
func (slowModel) Model() string    { return "stub-model" }
func (slowModel) Stream(ctx context.Context, _ llm.Request, onText func(string) error) (llm.Result, error) {
	select {
	case <-time.After(modelDelay):
	case <-ctx.Done():
		return llm.Result{}, ctx.Err()
	}
	if err := onText("ok"); err != nil {
		return llm.Result{}, err
	}
	return llm.Result{Text: "ok", StopReason: "end_turn",
		Usage: llm.Usage{InputTokens: 1000, OutputTokens: 10}}, nil
}

// varyingModel is the same stub with a realistic spread instead of a constant.
//
// A fixed delay flatters every runtime, because nothing ever queues behind something
// slow: every request is identical, so no request waits on another's tail. Real model
// latency is heavy-tailed -- most turns are quick, a few are several seconds -- and that
// is what makes a scheduler's behaviour visible.
//
// 300 ms plus an exponential with a 700 ms mean: same 1000 ms mean as the fixed run, a
// median near 785 ms, and a tail that reaches several seconds. Capped at 8 s so one
// unlucky draw does not dominate the wall time.
type varyingModel struct{}

func (varyingModel) Provider() string { return "stub" }
func (varyingModel) Model() string    { return "stub-model" }
func (varyingModel) Stream(ctx context.Context, _ llm.Request, onText func(string) error) (llm.Result, error) {
	delay := 300*time.Millisecond + time.Duration(rand.ExpFloat64()*float64(700*time.Millisecond))
	if delay > 8*time.Second {
		delay = 8 * time.Second
	}
	select {
	case <-time.After(delay):
	case <-ctx.Done():
		return llm.Result{}, ctx.Err()
	}
	if err := onText("ok"); err != nil {
		return llm.Result{}, err
	}
	return llm.Result{Text: "ok", StopReason: "end_turn",
		Usage: llm.Usage{InputTokens: 1000, OutputTokens: 10}}, nil
}

// stubEmbedder isolates the cost of the in-process embedding model. Everything else in
// the path is identical between the two runs.
type stubEmbedder struct{}

func (stubEmbedder) EmbedQuery(context.Context, string) ([]float32, error) {
	v := make([]float32, dims)
	v[0] = 1
	return v, nil
}
func (stubEmbedder) EmbedPassages(_ context.Context, passages []string) ([][]float32, error) {
	out := make([][]float32, len(passages))
	for i := range out {
		out[i] = make([]float32, dims)
		out[i][0] = 1
	}
	return out, nil
}
func (stubEmbedder) Dimensions() int { return dims }
func (stubEmbedder) Close() error    { return nil }

type result struct {
	name           string
	delayNote      string
	wall           time.Duration
	requestsPerSec float64
	p50, p95, p99  time.Duration
	peakGoroutines int
	threadsBefore  int
	threadsPeak    int
	failures       int
}

// The two variants run in separate processes, selected by BENCH_EMBEDDER, and `make
// bench` invokes both.
//
// That is not tidiness. The only OS-thread count Go exposes -- the threadcreate profile
// -- counts threads *created* over the life of the process and never decreases, so a
// second variant in the same process inherits the first one's threads and reads 112
// before it has served a single request. The Java implementation hit the same class of
// mistake from the other direction: its test-context cache kept two servers alive, and
// the idle one's 200-thread pool was counted against whichever run went second.
func TestConcurrencyUnderLoad(t *testing.T) {
	root := repoRoot(t)

	newONNX := func() rag.Embedder {
		e, err := rag.NewONNXEmbedder(rag.ONNXOptions{
			ModelPath:     filepath.Join(root, "model-cache/multilingual-e5-small/model.onnx"),
			TokenizerPath: filepath.Join(root, "model-cache/multilingual-e5-small/tokenizer.json"),
			Dimensions:    dims,
			QueryPrefix:   "query: ",
			PassagePrefix: "passage: ",
		})
		if err != nil {
			t.Skipf("embedding model not present; run `make deps`: %v", err)
		}
		return e
	}

	switch os.Getenv("BENCH_EMBEDDER") {
	case "stub":
		report(t, run(t, "stubbed embedding", func() rag.Embedder { return stubEmbedder{} }))
	case "varying":
		// Not comparable with the rows above or with the Java implementation's
		// published numbers, which all use a fixed delay. It is a better model of
		// reality and a worse basis for a side-by-side.
		r := runWith(t, "ONNX, varying model delay", newONNX, varyingModel{})
		r.delayNote = "300ms + Exp(mean 700ms) model delay, capped at 8s"
		report(t, r)
		t.Log("wall and req/s are not throughput here: with a heavy tail the wall time " +
			"is the slowest single request. p50 and p95 are the numbers that mean something.")
	case "bounded":
		// The production wrapper, not a copy of it: the whole point is to measure what
		// the deployed configuration does.
		limit := runtime.GOMAXPROCS(0)
		report(t, run(t, fmt.Sprintf("ONNX, bounded to %d", limit), func() rag.Embedder {
			return rag.NewBounded(newONNX(), limit)
		}))
	default:
		report(t, run(t, "in-process ONNX embedding", newONNX))
	}
}

func run(t *testing.T, name string, newEmbedder func() rag.Embedder) result {
	return runWith(t, name, newEmbedder, slowModel{})
}

func runWith(t *testing.T, name string, newEmbedder func() rag.Embedder, model llm.Client) result {
	t.Helper()
	ctx := context.Background()

	embedder := newEmbedder()
	defer embedder.Close()

	vectors := rag.NewStore(pool)
	if _, err := rag.Ingest(ctx, filepath.Join(repoRoot(t), "corpus/faq.json"), embedder, vectors); err != nil {
		t.Fatal(err)
	}
	if _, err := pool.Exec(ctx, `DELETE FROM chat_memory`); err != nil {
		t.Fatal(err)
	}

	service := chat.NewService(
		chat.NewMemory(pool, 40),
		rag.NewRetriever(embedder, vectors, 8, 0),
		model,
		cost.NewBudget(0, 20_000),
		obs.NewMetrics(),
		1024,
		tools.NewOrderLookup(), tools.NewSupportTickets(20_000),
	)

	mux := http.NewServeMux()
	httpapi.NewServer(service, config.Chat{
		MaxMessageLength: 4000, MaxConversationIDLength: 64, KeepAliveInterval: time.Second,
	}).Routes(mux)
	server := httptest.NewServer(mux)
	defer server.Close()

	// Let the process settle before taking the baseline: ingestion has just run, and
	// on the ONNX pass it ran a batched forward pass through cgo.
	runtime.GC()
	time.Sleep(500 * time.Millisecond)

	res := result{name: name, threadsBefore: osThreads()}

	// Sampling runs in its own goroutine. Whole-process counts are what Go exposes --
	// there is no per-component thread pool to interrogate the way Tomcat's could be --
	// so the load driver shares this process and these numbers are an upper bound. That
	// is the reason for the second run: the difference between the two isolates what
	// the in-process embedding model costs in OS threads, which is the only part of
	// this path that blocks a thread rather than parking a goroutine.
	stopSampling := make(chan struct{})
	var samplingDone sync.WaitGroup
	samplingDone.Add(1)
	go func() {
		defer samplingDone.Done()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-stopSampling:
				return
			case <-ticker.C:
				if g := runtime.NumGoroutine(); g > res.peakGoroutines {
					res.peakGoroutines = g
				}
				if th := osThreads(); th > res.threadsPeak {
					res.threadsPeak = th
				}
			}
		}
	}()

	client := &http.Client{
		Timeout: 120 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        concurrency,
			MaxIdleConnsPerHost: concurrency,
			MaxConnsPerHost:     concurrency,
		},
	}

	latencies := make([]time.Duration, concurrency)
	failures := make([]int32, concurrency)
	var wg sync.WaitGroup
	gate := make(chan struct{})

	for i := range concurrency {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-gate
			start := time.Now()
			// A fresh conversation per request: no shared history, no lock contention
			// that belongs to the test rather than the service.
			body := fmt.Sprintf(`{"conversationId":"bench-%d","message":"how much is delivery?"}`, i)
			resp, err := client.Post(server.URL+"/api/v1/chat", "application/json", strings.NewReader(body))
			latencies[i] = time.Since(start)
			if err != nil {
				failures[i] = 1
				return
			}
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				failures[i] = 1
			}
			_, _ = resp.Body.Read(make([]byte, 1))
		}(i)
	}

	started := time.Now()
	close(gate)
	wg.Wait()
	res.wall = time.Since(started)

	close(stopSampling)
	samplingDone.Wait()

	for _, f := range failures {
		res.failures += int(f)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	res.p50 = latencies[len(latencies)*50/100]
	res.p95 = latencies[len(latencies)*95/100]
	res.p99 = latencies[len(latencies)*99/100]
	res.requestsPerSec = float64(concurrency) / res.wall.Seconds()
	return res
}

// osThreads is the number of OS threads the Go runtime has created. It only grows, so
// it is the peak rather than the current count -- which is what matters here.
func osThreads() int { return pprof.Lookup("threadcreate").Count() }

func report(t *testing.T, results ...result) {
	t.Helper()
	note := results[0].delayNote
	if note == "" {
		note = modelDelay.String() + " fixed stubbed model delay"
	}
	t.Logf("%d concurrent requests, %s, GOMAXPROCS=%d, %d CPUs",
		concurrency, note, runtime.GOMAXPROCS(0), runtime.NumCPU())
	t.Logf("%-28s %8s %8s %9s %9s %9s %11s %9s %8s",
		"run", "wall", "req/s", "p50", "p95", "p99", "goroutines", "threads", "failed")
	for _, r := range results {
		t.Logf("%-28s %8v %8.0f %9v %9v %9v %11d %4d->%-4d %8d",
			r.name,
			r.wall.Round(time.Millisecond), r.requestsPerSec,
			r.p50.Round(time.Millisecond), r.p95.Round(time.Millisecond), r.p99.Round(time.Millisecond),
			r.peakGoroutines, r.threadsBefore, r.threadsPeak, r.failures)
	}
	for _, r := range results {
		if r.failures > 0 {
			t.Errorf("%s: %d of %d requests failed; the numbers above are not a measurement "+
				"of a working service", r.name, r.failures, concurrency)
		}
	}
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for range 5 {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		dir = filepath.Dir(dir)
	}
	t.Fatal("could not find the repository root")
	return ""
}
