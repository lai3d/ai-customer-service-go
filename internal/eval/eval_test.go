//go:build eval

// Package eval measures whether the answers are still right.
//
// Retrieval is measured (20/20 paraphrases, 4/4 cross-lingual) and nothing measured the
// answers. A prompt edit, a model upgrade or a corpus change could make the product worse
// and every test would stay green, because every test asserts on a stub. For something
// customers talk to, this is the measurement that decides whether it is usable.
//
// It calls the real model and costs real money, so it is opt-in: `make eval`.
package eval_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode"

	"github.com/lai3d/ai-customer-service-go/internal/chat"
	"github.com/lai3d/ai-customer-service-go/internal/config"
	"github.com/lai3d/ai-customer-service-go/internal/cost"
	"github.com/lai3d/ai-customer-service-go/internal/llm"
	"github.com/lai3d/ai-customer-service-go/internal/obs"
	"github.com/lai3d/ai-customer-service-go/internal/rag"
	"github.com/lai3d/ai-customer-service-go/internal/testsupport"
	"github.com/lai3d/ai-customer-service-go/internal/tools"
)

// Case is one question and what must be true of the answer.
//
// Every assertion is mechanical. Numbers are the strongest: a wrong number is a
// hallucination and unambiguous, where a wrong tone is an opinion. What this shape cannot
// express is the point of docs/evaluation.md.
type Case struct {
	ID       string `json:"id"`
	Question string `json:"question"`
	// Language the reply must be in: "en", "zh", or empty for either.
	Language string `json:"language"`
	// MustContain: every one of these, case-insensitively.
	MustContain []string `json:"mustContain"`
	// MustContainAny: one group per requirement; any member of a group satisfies it.
	MustContainAny [][]string `json:"mustContainAny"`
	MustNotContain []string   `json:"mustNotContain"`
	// Tools that must have been called, by name.
	Tools []string `json:"tools"`
	// NoTools asserts the turn made no tool call at all.
	NoTools bool `json:"noTools"`
	// Grounded asserts the answer admits it does not know rather than inventing.
	Grounded bool   `json:"grounded"`
	Why      string `json:"why"`
}

type suite struct {
	CorpusVersion string `json:"corpusVersion"`
	Cases         []Case `json:"cases"`
}

// uncertainty is what "it said it did not know" looks like, in both languages.
//
// This is the most brittle check here and it is the one to distrust first: it measures
// phrasing, which is the failure mode this repository has already recorded twice. It is
// paired with mustNotContain on the specific fabrication, which measures the defect
// instead -- and when a case can be written with only the negative, it is.
var uncertainty = []string{
	"i don't have", "i do not have", "not something i can", "no information",
	"cannot confirm", "can't confirm", "not covered", "don't know", "do not know",
	"unable to", "not able to", "human agent", "contact support", "our team",
	"没有相关", "查不到", "无法确认", "不清楚", "没有这方面", "人工客服", "联系客服", "不便",
}

func TestAnswerQuality(t *testing.T) {
	ctx := context.Background()
	root := repoRoot(t)
	control := os.Getenv("EVAL_WITHOUT_RETRIEVAL") != ""

	raw, err := os.ReadFile(filepath.Join(root, "internal", "eval", "cases.json"))
	if err != nil {
		t.Fatal(err)
	}
	var s suite
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatal(err)
	}

	// An eval measuring answers against a corpus it has not read is measuring nothing.
	corpus, err := rag.LoadCorpus(filepath.Join(root, "corpus", "faq.json"))
	if err != nil {
		t.Fatal(err)
	}
	if corpus.Version != s.CorpusVersion {
		t.Fatalf("cases.json was written against corpus %q and the corpus is now %q; "+
			"re-read the entries and update the facts before trusting a score",
			s.CorpusVersion, corpus.Version)
	}

	cfg, err := config.Load()
	if err != nil {
		t.Fatalf("configuration: %v (this test calls the real model; set the provider's key)", err)
	}
	client, err := llm.New(cfg.Chat)
	if err != nil {
		t.Fatal(err)
	}

	embedder, err := rag.NewONNXEmbedder(rag.ONNXOptions{
		ModelPath:     filepath.Join(root, "model-cache", "multilingual-e5-small", "model.onnx"),
		TokenizerPath: filepath.Join(root, "model-cache", "multilingual-e5-small", "tokenizer.json"),
		Dimensions:    384,
		QueryPrefix:   "query: ",
		PassagePrefix: "passage: ",
	})
	if err != nil {
		t.Fatalf("embedding model: %v (run `make deps`)", err)
	}
	defer embedder.Close()

	pool, stop, err := testsupport.StartPostgres(ctx, 384)
	if err != nil {
		t.Fatal(err)
	}
	defer stop()

	vectors := rag.NewStore(pool)
	// The negative control, and it is permanent rather than a patch someone applied once:
	// a score of 100% means nothing unless the same harness can be shown to produce a bad
	// one. EVAL_WITHOUT_RETRIEVAL leaves the corpus out, so the model answers from what it
	// knows about shops in general instead of from this shop's policies. If that still
	// scores well, the eval is measuring the model's plausibility rather than this
	// system's grounding, and every number it produces is worthless.
	if control {
		t.Log("EVAL_WITHOUT_RETRIEVAL: the corpus is not ingested; this run is the control")
	} else if _, err := rag.Ingest(ctx, filepath.Join(root, "corpus", "faq.json"), embedder, vectors); err != nil {
		t.Fatal(err)
	}

	tickets := &testsupport.FakeTickets{}
	service := chat.NewService(
		chat.NewMemory(pool, 40),
		rag.NewRetriever(embedder, vectors, 8, 0),
		client,
		cost.NewBudget(0, 20_000),
		obs.NewMetrics(),
		chat.NewRecorder(pool),
		cfg.Chat.MaxTokens,
		tools.NewOrderLookup(), tools.NewSupportTickets(tickets),
	)

	var passed, failed int
	var inputTokens, outputTokens int64
	var costUSD float64
	started := time.Now()

	for _, c := range s.Cases {
		c := c
		t.Run(c.ID, func(t *testing.T) {
			// A conversation of its own per case: memory carrying an answer between two
			// cases would make the score depend on the order they run in.
			turnCtx, cancel := context.WithTimeout(ctx, 90*time.Second)
			defer cancel()

			var answer strings.Builder
			var called []string
			err := service.Turn(turnCtx, "eval-"+c.ID, c.Question, func(e chat.Event) {
				switch e.Type {
				case chat.EventMessage:
					answer.WriteString(e.Text)
				case chat.EventTool:
					called = append(called, e.Tool.Name)
				case chat.EventUsage:
					inputTokens += e.Usage.InputTokens
					outputTokens += e.Usage.OutputTokens
					costUSD += e.Usage.CostUSD
				}
			})
			if err != nil {
				failed++
				t.Fatalf("the turn failed: %v", err)
			}

			problems := check(c, answer.String(), called)
			if len(problems) == 0 {
				passed++
				return
			}
			failed++
			// The answer is printed on failure and only on failure. Reading why it failed
			// without seeing what it said is guesswork, and printing every answer buries
			// the failures in a wall of correct ones.
			report := t.Errorf
			if control {
				// The control is meant to fail. Reporting its cases as errors would make
				// a successful demonstration look like a broken suite.
				report = t.Logf
			}
			report("%s\n  why this case exists: %s\n  tools: %v\n  answer: %s",
				strings.Join(problems, "\n"), c.Why, called, excerpt(answer.String()))
		})
	}

	total := passed + failed
	rate := 0.0
	if total > 0 {
		rate = float64(passed) / float64(total) * 100
	}
	t.Logf("\n%d/%d cases passed (%.1f%%) in %s\n"+
		"%d input + %d output tokens, estimated $%.4f, model %s\n",
		passed, total, rate, time.Since(started).Round(time.Second),
		inputTokens, outputTokens, costUSD, client.Model())

	// A floor rather than a target. The number that matters is the one in
	// docs/evaluation.md, updated by running this; the floor is what stops a change
	// making things quietly worse.
	const floor = 90.0
	if control {
		// The control is expected to score badly. Failing it would say the harness works.
		t.Logf("control run: %.1f%% with no corpus", rate)
		return
	}
	if rate < floor {
		t.Errorf("the pass rate is %.1f%%, below the %.0f%% floor", rate, floor)
	}
}

func check(c Case, answer string, called []string) []string {
	var problems []string
	lower := strings.ToLower(answer)

	for _, want := range c.MustContain {
		if !strings.Contains(lower, strings.ToLower(want)) {
			problems = append(problems, fmt.Sprintf("missing %q", want))
		}
	}
	for _, group := range c.MustContainAny {
		found := false
		for _, want := range group {
			if strings.Contains(lower, strings.ToLower(want)) {
				found = true
				break
			}
		}
		if !found {
			problems = append(problems, fmt.Sprintf("none of %v appears", group))
		}
	}
	for _, unwanted := range c.MustNotContain {
		if strings.Contains(lower, strings.ToLower(unwanted)) {
			problems = append(problems, fmt.Sprintf("contains %q, which is invented or wrong", unwanted))
		}
	}

	if c.Language != "" {
		if got := language(answer); got != c.Language {
			problems = append(problems,
				fmt.Sprintf("answered in %s, and the question was asked in %s", got, c.Language))
		}
	}

	for _, want := range c.Tools {
		if !contains(called, want) {
			problems = append(problems, fmt.Sprintf("did not call %s (called %v)", want, called))
		}
	}
	if c.NoTools && len(called) > 0 {
		problems = append(problems, fmt.Sprintf("called %v for a question needing no tool", called))
	}

	if c.Grounded {
		found := false
		for _, signal := range uncertainty {
			if strings.Contains(lower, signal) {
				found = true
				break
			}
		}
		if !found {
			problems = append(problems,
				"answered confidently about something the corpus does not cover")
		}
	}
	return problems
}

// language reports "zh" when enough of the answer is CJK. Counting characters rather than
// matching phrases: the ratio is what actually distinguishes the two, and a phrase list
// would measure the wording again.
func language(s string) string {
	var cjk, letters int
	for _, r := range s {
		switch {
		case unicode.Is(unicode.Han, r):
			cjk++
			letters++
		case unicode.IsLetter(r):
			letters++
		}
	}
	if letters == 0 {
		return "en"
	}
	if float64(cjk)/float64(letters) > 0.2 {
		return "zh"
	}
	return "en"
}

func contains(haystack []string, needle string) bool {
	for _, h := range haystack {
		if h == needle {
			return true
		}
	}
	return false
}

func excerpt(s string) string {
	s = strings.Join(strings.Fields(s), " ")
	if len(s) > 400 {
		return s[:400] + "..."
	}
	return s
}

func repoRoot(t *testing.T) string {
	t.Helper()
	dir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			t.Fatal("no go.mod above the working directory")
		}
		dir = parent
	}
}
