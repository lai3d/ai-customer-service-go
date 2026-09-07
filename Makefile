# The embedding model runs in this process, so the build has native dependencies:
# ONNX Runtime (loaded at runtime) and a Rust tokenizer (linked at build time).
# `make deps` fetches both, plus the model. Everything lands in gitignored directories.

GO ?= go
BIN := bin/server

.PHONY: deps build run test test-race bench eval eval-control lint fmt clean check-rules

deps:
	./scripts/fetch-deps.sh

build:
	$(GO) build -o $(BIN) ./cmd/server

run: build
	set -a && [ -f .env ] && . ./.env; set +a; $(BIN)

# The full suite. No API key: everything up to the model call is testable, and
# Testcontainers supplies a real pgvector. Docker must be running.
test:
	$(GO) test ./...

test-race:
	$(GO) test -race ./...

# Excluded from `make test` because it measures a machine rather than asserting a
# behaviour. See docs/benchmark.md.
# Two processes, deliberately: the only OS-thread count Go exposes never decreases, so
# a second variant in the same process inherits the first one's threads.
# The answer-quality regression set. Calls the real model, so it costs real money --
# roughly $1-2 a run at Opus 5 prices for 35 cases -- and is opt-in for that reason.
# Needs the provider's key in the environment and Docker for the database.
# See docs/evaluation.md for the last measured score and for what it cannot see.
eval:
	$(GO) test -tags=eval -count=1 -v -timeout 30m -run TestAnswerQuality ./internal/eval/

# The negative control: the same cases with no corpus ingested. A score of 100% means
# nothing unless the same harness can be shown to produce a bad one, and this is how.
# Expected to score badly and to exit 0; the number is the result.
eval-control:
	EVAL_WITHOUT_RETRIEVAL=1 $(GO) test -tags=eval -count=1 -v -timeout 30m -run TestAnswerQuality ./internal/eval/

bench:
	BENCH_EMBEDDER=onnx $(GO) test -tags=benchmark -v -count=1 -timeout 20m -run TestConcurrencyUnderLoad ./internal/benchmark/
	BENCH_EMBEDDER=bounded $(GO) test -tags=benchmark -v -count=1 -timeout 20m -run TestConcurrencyUnderLoad ./internal/benchmark/
	BENCH_EMBEDDER=varying $(GO) test -tags=benchmark -v -count=1 -timeout 20m -run TestConcurrencyUnderLoad ./internal/benchmark/
	BENCH_EMBEDDER=stub $(GO) test -tags=benchmark -v -count=1 -timeout 20m -run TestConcurrencyUnderLoad ./internal/benchmark/

# The alert rules, through Prometheus's own promtool in a container: PromQL that parses,
# and every alert seen to fire on data that should trip it. `go test ./internal/deployment`
# already checks them against the metrics the code emits; this is the half that needs
# Prometheus itself, which is why it is opt-in rather than part of `make test`.
check-rules:
	./scripts/check-rules.sh

lint:
	$(GO) vet ./...
	gofmt -l . | grep -v '^third_party/' | (! grep .) || (echo "gofmt -w the files above"; exit 1)

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './third_party/*')

clean:
	rm -rf bin
