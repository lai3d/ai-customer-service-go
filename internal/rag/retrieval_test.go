package rag_test

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/lai3d/ai-customer-service-go/internal/rag"
	"github.com/lai3d/ai-customer-service-go/internal/testsupport"
)

// Retrieval quality, measured against the real embedding model and a real pgvector.
// No API key is involved: everything up to the model call is testable, and this is
// where a silent regression -- a changed corpus, a different embedding model, a lost
// prefix -- would otherwise show up only as vaguer answers in production.
//
// The queries are the ones the Java implementation measures, verbatim, so the two sets
// of numbers can be compared. They deliberately avoid the corpus wording in both
// languages: matching a question to its own text proves nothing about a customer
// describing a problem in their own words.

const (
	topK      = 8
	threshold = 0
	dims      = 384
)

var (
	embedder      *rag.ONNXEmbedder
	embedderErr   error
	sharedPool    *pgxpool.Pool
	sharedStore   *rag.Store
	corpusPath    string
	sharedRetrier *rag.Retriever
)

// One container and one 470 MB model for the whole package. Both are expensive to
// create and neither is mutated by a test that does not ingest.
func TestMain(m *testing.M) {
	root, err := os.Getwd()
	if err != nil {
		panic(err)
	}
	for range 5 {
		if _, err := os.Stat(filepath.Join(root, "go.mod")); err == nil {
			break
		}
		root = filepath.Dir(root)
	}
	corpusPath = filepath.Join(root, "corpus/faq.json")
	model := filepath.Join(root, "model-cache/multilingual-e5-small/model.onnx")
	tokenizer := filepath.Join(root, "model-cache/multilingual-e5-small/tokenizer.json")

	if _, err := os.Stat(model); err != nil {
		fmt.Fprintln(os.Stderr, "embedding model not present; run `make deps`")
		os.Exit(0)
	}
	embedder, embedderErr = rag.NewONNXEmbedder(rag.ONNXOptions{
		ModelPath:     model,
		TokenizerPath: tokenizer,
		Dimensions:    dims,
		QueryPrefix:   "query: ",
		PassagePrefix: "passage: ",
	})
	if embedderErr != nil {
		fmt.Fprintf(os.Stderr, "load embedding model: %v\n", embedderErr)
		os.Exit(1)
	}
	defer embedder.Close()

	ctx := context.Background()
	pool, stop, err := testsupport.StartPostgres(ctx, dims)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%v\n", err)
		os.Exit(1)
	}
	defer stop()

	sharedPool = pool
	sharedStore = rag.NewStore(pool)
	sharedRetrier = rag.NewRetriever(embedder, sharedStore, topK, threshold)
	if _, err := rag.Ingest(ctx, corpusPath, embedder, sharedStore); err != nil {
		fmt.Fprintf(os.Stderr, "ingest corpus: %v\n", err)
		os.Exit(1)
	}

	code := m.Run()
	stop()
	embedder.Close()
	os.Exit(code)
}

type fixture struct {
	retriever *rag.Retriever
	store     *rag.Store
	corpus    string
	embedder  rag.Embedder
}

func newFixture(t *testing.T) fixture {
	t.Helper()
	return fixture{
		retriever: sharedRetrier,
		store:     sharedStore,
		corpus:    corpusPath,
		embedder:  embedder,
	}
}

func sharedEmbedder(t *testing.T) *rag.ONNXEmbedder {
	t.Helper()
	return embedder
}

func TestEnglishParaphraseRetrievesTheRightEntryFirst(t *testing.T) {
	f := newFixture(t)
	cases := []struct{ query, want string }{
		{"I want to send something back, is it too late after three weeks?", "returns-window"},
		{"how much do I pay for delivery", "shipping-cost"},
		{"my card was rejected at checkout", "payment-declined"},
		{"when can I talk to a real person", "support-hours"},
		{"my parcel showed up broken", "returns-damaged"},
		{"can I get a different size instead", "returns-exchange"},
		{"can I still change where it gets delivered", "shipping-address-change"},
		{"I forgot my password", "account-password"},
		{"do you send orders overseas", "shipping-international"},
		{"I was billed twice", "payment-double-charge"},
	}
	assertTopHits(t, f, cases)
}

func TestChineseParaphraseRetrievesTheRightEntryFirst(t *testing.T) {
	f := newFixture(t)
	cases := []struct{ query, want string }{
		{"我想退货，过了三个星期还来得及吗", "returns-window"},
		{"运费多少钱", "shipping-cost"},
		{"刷卡付款失败了", "payment-declined"},
		{"怎么才能找到人工客服", "support-hours"},
		{"包裹到的时候是坏的", "returns-damaged"},
		{"下单之后还能改地址吗", "shipping-address-change"},
		{"密码忘了怎么办", "account-password"},
		{"能寄到国外吗", "shipping-international"},
		{"同一笔订单扣了两次钱", "payment-double-charge"},
		{"想换个大一号的", "returns-exchange"},
	}
	assertTopHits(t, f, cases)
}

func assertTopHits(t *testing.T, f fixture, cases []struct{ query, want string }) {
	t.Helper()
	for _, tc := range cases {
		t.Run(tc.query, func(t *testing.T) {
			passages, err := f.retriever.Retrieve(context.Background(), tc.query)
			if err != nil {
				t.Fatalf("retrieve: %v", err)
			}
			if len(passages) == 0 {
				t.Fatalf("no passages for %q", tc.query)
			}
			if passages[0].EntryID != tc.want {
				t.Errorf("top hit for %q is %s (%.4f), want %s",
					tc.query, passages[0].EntryID, passages[0].Score, tc.want)
			}
		})
	}
}

func TestChineseQuestionPrefersTheChinesePassage(t *testing.T) {
	f := newFixture(t)
	for _, query := range []string{"运费多少钱", "包裹到的时候是坏的", "密码忘了怎么办"} {
		passages, err := f.retriever.Retrieve(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		if passages[0].Language != "zh" {
			t.Errorf("%q matched a %s passage first; a same-language passage is a shorter "+
				"distance for the model to travel", query, passages[0].Language)
		}
	}
}

// The real test of a multilingual model, and it cannot be observed on the full corpus:
// same-language matches score high enough that all eighteen Chinese passages outrank
// every English one. Isolating the English half is what shows whether cross-lingual
// retrieval works at all, which is what matters for an entry nobody has translated yet.
func TestChineseQuestionFindsTheEnglishPassageWhenOnlyEnglishExists(t *testing.T) {
	f := newFixture(t)
	cases := []struct{ query, want string }{
		{"运费多少钱", "shipping-cost"},
		{"包裹到的时候是坏的", "returns-damaged"},
		{"密码忘了怎么办", "account-password"},
		{"能寄到国外吗", "shipping-international"},
	}
	for _, tc := range cases {
		passages, err := f.retriever.RetrieveIn(context.Background(), tc.query, "en")
		if err != nil {
			t.Fatal(err)
		}
		if len(passages) == 0 || passages[0].EntryID != tc.want {
			t.Errorf("[zh->en] %q: got %v, want %s", tc.query, first(passages), tc.want)
		}
	}
}

// The measurement behind "no similarity threshold is worth setting with this model".
//
// The Java implementation measured 20 relevant questions against 4 off-topic ones, found
// the weakest relevant match 0.006 above the strongest off-topic one, called that too
// thin to tune against, and kept the threshold as "a floor for degenerate input". This
// run reproduces its weakest relevant score to four decimal places and then takes the
// two samples it did not: ten off-topic questions instead of four, and fifteen
// degenerate inputs. Both land above the weakest real match. All three populations
// overlap, so the floor was not a floor either.
//
// The sample size is the whole lesson. With the first four degenerate inputs the
// strongest scored 0.8119 and a floor at 0.82 looked defensible; the eleventh input --
// three Chinese full stops -- scores 0.8417. Four samples is not a measurement, which is
// the same mistake in the other direction.
func TestNoSimilarityThresholdIsUseful(t *testing.T) {
	f := newFixture(t)

	relevant := []string{
		"I want to send something back, is it too late after three weeks?",
		"how much do I pay for delivery", "my card was rejected at checkout",
		"when can I talk to a real person", "my parcel showed up broken",
		"can I get a different size instead", "can I still change where it gets delivered",
		"I forgot my password", "do you send orders overseas", "I was billed twice",
		"我想退货，过了三个星期还来得及吗", "运费多少钱", "刷卡付款失败了",
		"怎么才能找到人工客服", "包裹到的时候是坏的", "下单之后还能改地址吗",
		"密码忘了怎么办", "能寄到国外吗", "同一笔订单扣了两次钱", "想换个大一号的",
	}
	offTopic := []string{
		"who won the world cup in 2022", "how do I cook rice",
		"what is the capital of France", "recommend me a good film",
		"how do I write a for loop in Go",
		"给我讲个笑话", "明天天气怎么样", "推荐一部电影",
		"怎么做红烧肉", "你们招聘工程师吗",
	}
	degenerate := []string{
		"...", "?", "   ", "aaaaaaaa", "!!!", "。。。", "1234567890", "asdfgh", "、",
		"😀😀", ".", "\n\n", "qqqq wwww", "零", "-",
	}

	relevantLow, relevantLowQ := extreme(t, f, relevant, false)
	offTopicHigh, offTopicHighQ := extreme(t, f, offTopic, true)
	degenerateHigh, degenerateHighQ := extreme(t, f, degenerate, true)

	t.Logf("relevant    n=%-3d weakest   %.4f  %q", len(relevant), relevantLow, relevantLowQ)
	t.Logf("off-topic   n=%-3d strongest %.4f  %q", len(offTopic), offTopicHigh, offTopicHighQ)
	t.Logf("degenerate  n=%-3d strongest %.4f  %q", len(degenerate), degenerateHigh, degenerateHighQ)

	if relevantLow > offTopicHigh {
		t.Errorf("relevant and off-topic scores have separated (%.4f > %.4f): a threshold "+
			"may be a real relevance filter again -- re-measure rather than adjusting this "+
			"number until it passes", relevantLow, offTopicHigh)
	}
	if relevantLow > degenerateHigh {
		t.Errorf("degenerate input now scores below every real match (%.4f > %.4f): a floor "+
			"is worth setting again, somewhere between them", relevantLow, degenerateHigh)
	}
}

// extreme returns the lowest (or highest) top-1 score across a set of queries.
func extreme(t *testing.T, f fixture, queries []string, highest bool) (float64, string) {
	t.Helper()
	best, bestQuery := 1.0, ""
	if highest {
		best = -1.0
	}
	for _, q := range queries {
		s := topScore(t, f, q)
		if (highest && s > best) || (!highest && s < best) {
			best, bestQuery = s, q
		}
	}
	return best, bestQuery
}

func TestEveryLanguageOfEveryEntryIsIndexed(t *testing.T) {
	f := newFixture(t)
	n, err := f.store.Count(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if n != 36 {
		t.Errorf("indexed %d documents, want 36 (18 entries in 2 languages)", n)
	}
}

// Appending on every boot is the obvious bug, and it does not merely waste space:
// duplicates crowd out distinct passages inside the top-k window.
func TestReingestingReplacesRatherThanDuplicates(t *testing.T) {
	f := newFixture(t)
	ctx := context.Background()
	before, err := f.store.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for range 2 {
		if _, err := rag.Ingest(ctx, f.corpus, f.embedder, f.store); err != nil {
			t.Fatal(err)
		}
	}
	after, err := f.store.Count(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("corpus grew from %d to %d documents across re-ingestion", before, after)
	}
}

// ONNX Runtime documents Run as thread-safe and the Rust tokenizer encodes through an
// immutable reference, but the benchmark puts a thousand goroutines through this at
// once, and "the documentation says so" is not a measurement. Run under -race.
func TestONNXEmbedderIsConcurrencySafe(t *testing.T) {
	e := sharedEmbedder(t)
	ctx := context.Background()

	want, err := e.EmbedQuery(ctx, "包裹到的时候是坏的")
	if err != nil {
		t.Fatal(err)
	}

	const goroutines = 64
	var wg sync.WaitGroup
	errs := make(chan error, goroutines)
	for i := range goroutines {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			got, err := e.EmbedQuery(ctx, "包裹到的时候是坏的")
			if err != nil {
				errs <- err
				return
			}
			for d := range got {
				if got[d] != want[d] {
					errs <- errDivergent{goroutine: i, dim: d, got: got[d], want: want[d]}
					return
				}
			}
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

type errDivergent struct {
	goroutine, dim int
	got, want      float32
}

func (e errDivergent) Error() string {
	return "concurrent embedding diverged"
}

func topScore(t *testing.T, f fixture, query string) float64 {
	t.Helper()
	passages, err := f.retriever.Retrieve(context.Background(), query)
	if err != nil {
		t.Fatal(err)
	}
	if len(passages) == 0 {
		return 0
	}
	return passages[0].Score
}

func first(passages []rag.Passage) string {
	if len(passages) == 0 {
		return "<nothing>"
	}
	return passages[0].EntryID
}
