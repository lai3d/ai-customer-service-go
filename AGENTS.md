# Repository Guidelines

## Project Structure & Module Organization

`cmd/server/main.go` wires the application. `internal/` contains chat orchestration (`chat`), retrieval and embeddings (`rag`), provider clients (`llm`), HTTP/SSE endpoints (`httpapi`), tools, configuration, storage, cost accounting, and observability. Tests live beside source as `*_test.go`; `internal/testsupport` supplies shared database helpers. The embedded demo UI is `internal/httpapi/web/index.html`. Decision records and measurements live in `docs/`; bilingual FAQ data lives in `corpus/faq.json`.

## Build, Test, and Development Commands

Use Go 1.26.1 or newer and a running Docker daemon.

- `make deps`: fetch native tokenizer libraries and the embedding model; on macOS, also install ONNX Runtime with `brew install onnxruntime`.
- `make build`: compile `bin/server`.
- `docker compose up -d postgres jaeger`: start local dependencies.
- `make run`: build and run the server on port 8081, loading `.env`.
- `make test`: run all default tests without a chat API key.
- `make test-race`: run the suite with race detection.
- `make fmt` / `make lint`: apply `gofmt` / check formatting and run `go vet`.
- `make bench`: run opt-in load measurements in separate processes.

## Coding Style & Naming Conventions

Use `gofmt` formatting, including tab indentation. Keep package names lowercase, exported identifiers in PascalCase, and unexported identifiers in camelCase. Prefer ordinary Go functions and explicit interfaces. Preserve the turn ordering in `chat.Service.Turn`: retrieved passages must never enter conversation memory. Each `llm.Client.Stream` invocation represents one model call and returns its usage, including on errors.

## Testing Guidelines

Use Go's `testing` package, `httptest` for provider protocols, and Testcontainers with real pgvector for database integration. Name tests `Test<Behavior>`; target one with `go test ./internal/chat/ -run TestRetrievedPassagesNeverEnterMemory -v`. Install dependencies before interpreting skipped embedding tests. CI requires lint and default tests; no numeric coverage threshold is configured. Assert observable behavior at the relevant boundary.

## Commit & Pull Request Guidelines

History uses descriptive imperative subjects such as “Add a Chinese README”; follow that style. PRs should explain the problem, resulting behavior, and validation performed; link relevant issues and include screenshots for UI changes. Keep both READMEs' section structures aligned. Re-run measurements before updating reported numbers.

## Security & Configuration

Copy `.env.example` to `.env`; never commit credentials or print interpolated Compose configuration. Declare new Compose settings explicitly. Never edit `corpus/faq.json`: comparison with the Java implementation requires identical bytes.
