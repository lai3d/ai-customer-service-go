# The embedding model runs in this process, so the build has native dependencies:
# ONNX Runtime (loaded at runtime) and a Rust tokenizer (linked at build time).
# `make deps` fetches both, plus the model. Everything lands in gitignored directories.

GO ?= go
BIN := bin/server

.PHONY: deps build run test test-race bench lint fmt clean

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
bench:
	BENCH_EMBEDDER=onnx $(GO) test -tags=benchmark -v -count=1 -timeout 20m -run TestConcurrencyUnderLoad ./internal/benchmark/
	BENCH_EMBEDDER=bounded $(GO) test -tags=benchmark -v -count=1 -timeout 20m -run TestConcurrencyUnderLoad ./internal/benchmark/
	BENCH_EMBEDDER=stub $(GO) test -tags=benchmark -v -count=1 -timeout 20m -run TestConcurrencyUnderLoad ./internal/benchmark/

lint:
	$(GO) vet ./...
	gofmt -l . | grep -v '^third_party/' | (! grep .) || (echo "gofmt -w the files above"; exit 1)

fmt:
	gofmt -w $$(find . -name '*.go' -not -path './third_party/*')

clean:
	rm -rf bin
