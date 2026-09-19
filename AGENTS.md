# Repository Guidelines

## Project Structure & Module Organization
Core binaries live in `cmd/`: `cmd/bridge` (daemon) and `cmd/bridge-ca` (certificate tooling). Service contracts are in `proto/bridge/v1`, with generated Go stubs in `gen/bridge/v1` (regenerate, do not hand-edit). Runtime internals are organized under `internal/` by domain: `auth`, `bridge`, `config`, `pki`, `provider`, and `server`. Public SDK code is in `pkg/bridgeclient`. Supporting artifacts live in `config/`, `scripts/`, `certs/`, and integration scenarios in `e2e/`.

## Build, Test, and Development Commands
- `make setup`: Run at the start of every session (agent sessions included). Persists the Go tools directory on `PATH` and installs/registers the `pre-commit` and `pre-push` git hooks from `.pre-commit-config.yaml`, so local checks (`gofmt`, `goimports`, `golangci-lint`, `go test -race`) actually run before a commit or push instead of only in CI.
- `make build`: Generates protobuf stubs, then builds `bin/bridgectl` and `bin/bridge-ca`.
- `make proto`: Regenerates Go code from `proto/bridge/v1/bridge.proto`.
- `make test`: Runs all Go tests with race detection (`go test -race -count=1 ./...`).
- `make test-cover`: Produces `coverage.out` and `coverage.html`.
- `make lint`: Runs `golangci-lint` across the module.
- `make fmt`: Applies `gofmt -s -w .` and `goimports -w .`.
- `make dev-setup`: Builds binaries and generates development certificates.

## Coding Style & Naming Conventions
Use standard Go formatting and imports (`make fmt` before commits). Keep packages focused and lower-case; exported identifiers use `CamelCase`, unexported use `camelCase`. Prefer descriptive file names aligned to domain behavior (for example, `supervisor.go`, `interceptors.go`). Never hand-edit files under `gen/`; they are generated from `proto/`. To update them, edit the relevant `.proto` file and run `make proto`, then commit the resulting changes in `gen/` together with the proto change.

## Testing Guidelines
Write table-driven unit tests beside implementation files with `_test.go` suffix (for example, `internal/bridge/eventbuf_test.go`). Favor deterministic tests and include race-safe behavior checks for concurrent code paths. Run `make test` locally before opening a PR; use `make test-cover` when changing critical auth, session, or provider flows.

## Commit & Pull Request Guidelines
History follows concise, imperative subjects (for example, `Add gRPC server...`, `Fix data races...`). Keep commits scoped to a single logical change. When a proto change regenerates files under `gen/`, always include those regenerated files in the same commit as the `.proto` change — never leave them as unstaged modifications. PRs should include:
- A short problem/solution summary.
- Linked issue or task reference when available.
- Test evidence (`make test`, and e2e notes when relevant).
- Config or operational impact (ports, certs, auth behavior).

## Security & Configuration Tips
Treat `config/bridge.yaml` as local-dev plaintext mode only. For realistic environments, use `config/bridge-dev.yaml` with mTLS and JWT keys from `certs/`. Never commit private keys, tokens, or environment-specific secrets.

## Installed agent rules

Created by Ballast. Do not edit this section.

Read and follow these rule files in `.codex/rules/` when they apply:

- `.codex/rules/common/local-dev-badges.md` — Rules for common/local-dev-badges
- `.codex/rules/common/local-dev-env.md` — Rules for common/local-dev-env
- `.codex/rules/common/local-dev-license.md` — Rules for common/local-dev-license
- `.codex/rules/common/local-dev-mcp.md` — Rules for common/local-dev-mcp
- `.codex/rules/common/docs.md` — Rules for common/docs
- `.codex/rules/common/cicd.md` — Rules for common/cicd
- `.codex/rules/common/observability.md` — Rules for common/observability
- `.codex/rules/common/publishing-api.md` — Rules for common/publishing-api
- `.codex/rules/common/publishing-apps.md` — Rules for common/publishing-apps
- `.codex/rules/common/publishing-apt.md` — Rules for common/publishing-apt
- `.codex/rules/common/publishing-brew.md` — Rules for common/publishing-brew
- `.codex/rules/common/publishing-cli.md` — Rules for common/publishing-cli
- `.codex/rules/common/publishing-libraries.md` — Rules for common/publishing-libraries
- `.codex/rules/common/publishing-sdks.md` — Rules for common/publishing-sdks
- `.codex/rules/common/publishing-web.md` — Rules for common/publishing-web
- `.codex/rules/common/git-hooks.md` — Rules for common/git-hooks
- `.codex/rules/typescript/typescript-linting.md` — Rules for typescript/linting
- `.codex/rules/typescript/typescript-logging.md` — Rules for typescript/logging
- `.codex/rules/typescript/typescript-testing.md` — Rules for typescript/testing
- `.codex/rules/go/go-linting.md` — Rules for go/linting
- `.codex/rules/go/go-logging.md` — Rules for go/logging
- `.codex/rules/go/go-testing.md` — Rules for go/testing
- `.codex/rules/docker/docker-linting.md` — Rules for docker/linting
- `.codex/rules/docker/docker-logging.md` — Rules for docker/logging
- `.codex/rules/docker/docker-testing.md` — Rules for docker/testing

## Installed skills

Created by Ballast. Do not edit this section.

Read and use these skill files in `.codex/skills/` when they are relevant:

- `.codex/skills/github-health-check/SKILL.md` — run a comprehensive GitHub repository health check covering CI status, code quality, branch hygiene, and repo configuration
- `.codex/skills/github-pr-copilot-cycle/SKILL.md` — create or update a GitHub PR, request Copilot review, triage and fix Copilot comments, push fixes, check CI, and repeat up to three cycles
