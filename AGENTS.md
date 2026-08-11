# Repository Guidelines

## Project Structure & Module Organization

Relata is a Go modular monolith. `cmd/relata/` contains the executable and administrative commands. Core packages live under `internal/`; domain types and ports are in `internal/domain/`, capability packages include `ingest`, `search`, and `analysis`, and MongoDB/Qdrant adapters live in `internal/storage/`. Templates and assets are under `internal/web/`. Unit tests sit beside their packages; tagged end-to-end tests are in `tests/`. Project documentation and ADRs live in `docs/`; deployment files are in `deploy/`.

## Build, Test, and Development Commands

- `make setup`: download Go modules, install development tools, and create `.env` from `.env.example`.
- `make compose-up`: start MongoDB, Qdrant, and Ollama; run `make models` once.
- `make dev`: run the application from source at `http://127.0.0.1:8080`.
- `make build`: create the static binary at `bin/relata`.
- `make test`: run unit tests with the race detector; `make test-short` is faster.
- `make test-integration`: run tagged integration tests using disposable service containers.
- `make check`: run formatting, linting, and unit-test checks.

## Coding Style & Naming Conventions

Target Go 1.26.5 or newer. Use tabs as emitted by `gofmt`; run `make fmt` before committing. `make lint` applies `go vet` and `staticcheck` when installed. Follow Go naming: exported identifiers use `PascalCase`, internal identifiers use `camelCase`, and packages are short lowercase nouns. Domain ports must not depend on HTTP, database, or provider-specific types. Treat model output and documents as untrusted input.

## Testing Guidelines

Use Go's `testing` package and name cases `TestBehavior`; prefer table-driven subtests where useful. Keep unit tests beside implementation as `*_test.go`. Add cross-service tests to `tests/` with the `integration` build tag. Run `make test-security` for auth, workspace isolation, input-handling, or rendering changes. Use `make test-cover` for coverage and `make fuzz` after parser or validation changes. There is no fixed coverage threshold, but changed behavior needs regression coverage.

## Commit & Pull Request Guidelines

History contains only `first commit`, so no mature convention exists. Use a concise imperative subject such as `Harden upload filename validation` and keep commits focused. Pull requests should explain the effect, approach, and tests run; link issues or ADRs. Include screenshots for template/CSS changes and call out configuration, migration, security, or API impacts.

## Security & Configuration

Never commit `.env`, tokens, keys, uploaded data, or build artifacts. Add settings to `.env.example`, validate safe defaults, redact secrets from logs, and preserve workspace filtering in storage operations.
