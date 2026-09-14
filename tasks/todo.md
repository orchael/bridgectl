# Remote Step CA E2E failure reporting (2026-09-11)

Mode: Autonomous localized test-harness bug fix. Governing requirement: PRD
container acceptance criteria for remote Step CA E2E.

- [x] Reproduce false success with a fake Docker command and regression tests.
- [x] Align the client with the initialized JWK provisioner and propagate setup,
  wait, and container failures while preserving cleanup.
- [x] Run harness regressions, Compose validation, and the real Docker E2E.

Scope: Make target and test client; no production authentication changes.
Risk: a fast-exiting client disappears from the default Compose `ps` output;
include stopped containers. Keep detached startup because CA init is a one-shot
service. Rollback: revert the harness edits; no persistent data migration.

Live verification exposed a second enrollment mismatch after certificate issuance
succeeded: server startup defaults to a certificate for `server`, but the
client dials `bridge-server`. Extend the harness fix to use the Compose service
DNS name covered by that certificate; no TLS verification changes.

After enrollment succeeded, SDK tests failed because their expected local
`ca-bundle.crt` was missing. Client init uses the supplied Step CA root path;
copy that public root into the SDK test fixture's expected bundle location.

Evidence:
- `TestMakeTargetResult` reproduced false success before the fix; all nine cases
  pass with `go test -race -count=1 ./e2e/remote-stepca`.
- `make test-remote-stepca` exits 0 after certificate issuance, JWT enrollment,
  health, provider listing, and echo-session checks; cleanup completes. The
  Claude/Codex session checks skip without credentials, and the host-only Make
  regression skips inside the runtime image, which has no `make` binary.
- The first live rerun correctly printed `FAILED (exit 1)` for the certificate
  SAN mismatch, proving container failure propagation against real Docker.
- `make lint` passes with zero issues. Compose config, shell syntax, formatting,
  and `git diff --check` pass.
- `make test` fails in the existing
  `TestCLISuite/TestRepoSetupConfigEnvironmentPropagation` at its five-second
  repository-setup timeout. An isolated race-test rerun reproduces this failure;
  other packages passed. This separate timeout is outside the harness fix.

PR preparation: a fresh `make test` run passed the full race-test suite, including
the previously timing-out repository-setup test. `make test-cover-maintained`
passed with 78.8% coverage against the 75% gate.

Copilot cycle 1 (PR #222, commit `b06425d`): no review threads were published,
but the review body identified a valid coverage gap (score 2). Strengthened the
stopped-client case to require `ps -a -q`, waiting on the discovered client, and
propagation of its distinct exit code 23. All nine race-tested cases pass.

Final Docker run log: `/tmp/bridgectl-remote-stepca-verification.log`.
Rollback remains limited to these harness and documentation edits; disposable
Compose containers and volumes were removed by the successful Make target.

# Codex Authentication Lifecycle (2026-09-10)

User approved security-sensitive implementation following review. Governing
requirements: PRD CA-1 through CA-5. Scope: provider auth source selection,
desktop-local mutable credentials, and session-aware health. Parent project
owns secret reload and live desktop E2E integration.

- [x] Define PRD lifecycle and rotation contract before implementation.
- [x] Demonstrate failing account preservation, precedence, home isolation,
  prepared health, and native API-key fallback regression tests.
- [x] Implement common source resolution and atomic bootstrap.
- [x] Run provider/full Go tests, race checks, and coverage; build E2E binary.

Evidence: `TestCodexLifecycle*` failed before implementation for all five
original defects; startup-probe and rotation diagnostic regressions also failed
before their fixes. `go test ./internal/provider -count=1` passed.
`GOFLAGS=-buildvcs=false go test -race -count=1
-coverprofile=/tmp/bridgectl-codex-auth-coverage-fixed.out ./...` passed, including
the local CLI E2E suite. Provider coverage is 81.2%; whole-repo instrumented
coverage is 43.8% including generated code, examples, and binaries (existing
coverage gap, not claimed as meeting the global 75% rule). An earlier sandbox
run failed due to read-only Go cache/VCS stamping; rerunning with approved cache
access and VCS stamping disabled resolved those environment failures.
Linux amd64 E2E binary: `/tmp/bridgectl-codex-auth-linux-amd64`.

Credential-environment hardening: strengthened account and API fallback tests
to assert variables are absent, not merely empty. Both failed before changing
child preparation to remove all CODEX_AUTH, CODEX_API_KEY, and OPENAI_API_KEY
entries. This avoids native CLI presence checks overriding the selected file.

PR review follow-up: effective Windows `USERPROFILE` is now supported when
`HOME` is absent, without importing the daemon's account into a prepared
environment. Platform-home regression failed before the helper was added.
CI lint installation failed because goimports@latest required Go 1.26 while
the workflow uses Go 1.25.7; pin goimports to the repository's x/tools v0.44.0
(its module declares Go 1.25.0). Copilot Windows rename/Geteuid objections were
checked against Go source: Windows Rename uses MOVEFILE_REPLACE_EXISTING and
syscall.Geteuid exists there and returns -1.

Review follow-up: added regressions confirming health rejects a home nested
under a regular file, while an unwritable missing bootstrap directory fails
authoritatively in command preparation before process launch. Documented that
read-only health verifies source availability, not future write success;
permission prediction would incorrectly reject owned directories that
preparation intentionally repairs with chmod. Both focused tests passed.

Risk: account token refresh/revocation remains owned by Codex and remote login
service; structural validation cannot prove server acceptance. Rollback: restore
previous binary; retain existing auth files and secret values. Never restore the
old seed over a refreshed file during rollback.

# Issue 57 Implementation Plan

## Governing PRD

- Update `PRD.md` with a Debian/Ubuntu distribution section covering package contents, supported Ubuntu releases, apt repository hosting, signing, installation flow, and smoke-test evidence requirements.

## Scope

- Add Debian packaging for `bridgectl` using `nfpm`.
- Extend the release workflow to build `.deb` artifacts, publish them into a signed apt repository, and attach release artifacts.
- Add an `install.sh` helper and Ubuntu installation docs.
- Add packaging-focused tests and smoke coverage.
- Add an EC2 smoke test flow that provisions a host, installs the apt package, validates the service, and tears the host down.

## Constraints

- Do not replace the existing tag-driven `publish.yml` workflow; extend it.
- Keep package contents honest: ship the bridge binaries, systemd unit, and default config, but document provider CLIs as separate runtime prerequisites.
- Support Ubuntu `24.04 noble` and `26.04 resolute` for the initial implementation.
- Keep the apt repo hosting inside GitHub-native infrastructure.

## Tradeoffs

- Prefer `nfpm` over `dpkg-buildpackage` to keep packaging metadata small and repo-local.
- Prefer GitHub Pages for the apt repo over third-party hosted repositories to avoid new vendor dependencies.
- Limit package architectures to `amd64` first; defer `arm64` until the packaging path is proven.

## Risks

- GPG signing and apt metadata generation can fail in CI in ways that are hard to diagnose without explicit smoke coverage.
- Systemd behavior differs between container tests and real hosts, so an EC2 validation step is needed for service verification.
- The existing daemon expects external provider CLIs and secrets; the package and service docs must not imply a turnkey production install.

## Test Strategy

- Add unit coverage for generated packaging metadata helpers where practical.
- Add workflow-level smoke commands that build the `.deb`, generate repo metadata, and install from the local repo inside Ubuntu containers.
- Add an EC2 smoke script/workflow that installs from the published apt repo and validates the systemd service and gRPC health path.

## Rollback Strategy

- Revert the apt publish job from `publish.yml`.
- Remove the published apt repo branch contents or stop updating them.
- Users can continue using the existing GitHub release and container installation paths.

## Execution Checklist

- [x] Update `PRD.md` with apt distribution requirements and acceptance criteria.
- [x] Add the smallest failing packaging test(s) for required release metadata/files.
- [x] Add packaging assets (`nfpm`, systemd unit, config/install assets).
- [x] Extend release automation to build `.deb`, sign/publish apt metadata, and upload artifacts.
- [x] Add local/container smoke coverage for install-from-repo.
- [x] Add EC2 smoke automation for install and service validation.
- [x] Update `README.md` and docs for Ubuntu installation and runtime expectations.
- [x] Run targeted verification and capture evidence.
- [x] Record any new lessons in `tasks/lessons.md`.

---

# Telemetry retention defaults (2026-09-14)

Mode: Approval-Required operator/runtime default change, explicitly authorized
by the user. Governing requirement: PRD §17.2 (`TEL-110`).

Scope and acceptance criteria:
- [x] Change the bridge telemetry flush default from 1 second to 10 seconds.
- [x] Set collector retention to 100 segments at the 10 MiB default, providing
  approximately 1 GiB of bounded local-volume capacity.
- [x] Keep bridge outbox retention independently configurable and unchanged.
- [x] Prove defaults through unit tests and validate the collector Compose file.
- [x] Update examples and operator documentation.

Risks and rollback:
- A longer flush interval can delay delivery of low-volume active telemetry by
  up to 10 seconds, while shutdown still performs an explicit final flush.
- Revert the default values or override `flush_interval` and
  `TELEMETRY_MAX_SEGMENTS` for an immediate operator rollback.

Evidence:
- The new config and collector-default tests failed against the prior `1s` and
  `128` defaults, then passed with `10s` and `100`.
- `docker compose -f telemetry/docker-compose.yml config` rendered
  `--max-segments 100` and the existing named volume.
- Focused race tests passed for `internal/config`, `internal/telemetry`, and
  `cmd/bridgectl`.
- `make test`, `make build`, and
  `docker build --check -f telemetry/Dockerfile .` passed.

# Disk-budget telemetry retention (2026-09-14)

Mode: Approval-Required storage/config contract change, explicitly authorized
by the user. Governing requirement: PRD §17.2 (`TEL-110`).

Scope and acceptance criteria:
- [x] Replace `max_segments`/`--max-segments` with the human-readable
  `max_disk_space`/`--max-disk-space` across bridge and collector configuration.
- [x] Default bridge-local and collector-local spools to exactly 1 GB while
  accepting decimal and binary byte-size suffixes.
- [x] Count active and immutable segment bytes toward the steady-state budget
  and evict the oldest immutable segments until usage is within it.
- [x] Enforce the byte budget when opening an existing spool as well as while
  appending, sealing, and accepting remote segments.
- [x] Reject invalid budgets and segments that cannot fit within the configured
  per-segment or total-disk limits.
- [x] Update unit/E2E fixtures, Compose, container defaults, and documentation;
  run the full verification suite.

Risks and rollback:
- This intentionally removes the count-based configuration contract; stale
  `max_segments` keys must fail configuration decoding instead of being ignored.
- A spool may contain many small files up to the byte budget; filesystem inode
  monitoring remains an operator responsibility.
- Roll back by reverting the contract change. Operators can lower
  `max_disk_space` or `TELEMETRY_MAX_DISK_SPACE` without changing segment format.

Evidence:
- The new tests first failed against count-based retention and the removed
  settings, then passed with aggregate byte enforcement and `1GB` defaults.
- Focused race tests passed for config, telemetry, local-server integration,
  and the collector CLI.
- `make test`, `make lint`, and `make build` passed; maintained coverage is
  81.0%, above the required 75%.
- Both Compose files rendered successfully, the collector Dockerfile check
  reported no warnings, and `make test-e2e-live-telemetry` passed through the
  real bridge, durable gRPC outbox, collector, and collector-owned volume.
- The first E2E image build exhausted the host disk. With explicit approval,
  2.321 GB of unused Docker build cache was removed; no images, containers,
  named volumes, or telemetry data were removed. The retry passed.


# CLI Security Follow-ups (from PR #92 Copilot review)

Cert lifecycle items moved to GitHub Issues: #225 (cert renewal), #224 (SAN mismatch detection).

Previously listed items that were already fixed or addressed in later PRs:
TestMain cleanup, Echo test assertion, Docker E2E cleanup trap, Windows status
message, pki_test.go portability.

## Remaining

- [ ] **GoReleaser Windows target**: `.goreleaser.yaml` includes `windows` for the CLI but `internal/bridge/supervisor.go` uses Unix-only APIs (`syscall.Kill`, `Setpgid`, `creack/pty`) and the CLI has unguarded PTY paths. The provider package now compiles on Windows (build-tag stubs in `stdio_pty_windows.go`), but the full `cmd/bridgectl` binary still fails cross-compilation. Either add build tags across `internal/bridge` and CLI PTY paths, or remove the Windows release target.
# Startup Step CA Client Registry

Mode: Approval-Required, approved by user request on 2026-08-09.

Governing PRD section: `7.3 Authentication Layers` and `7.4 Key Management`.

Scope:
- Add startup configuration for Step CA-backed client issuers whose JWT public keys may already be present on the server.
- Keep mTLS trust and JWT trust separate; Step CA verifies client certificates, bridge config loads JWT public keys.
- Preserve existing `auth.jwt_public_keys` behavior for required explicit keys.
- Add tests, documentation, and smoke coverage.

Plan:
- [x] Update PRD with startup client registry acceptance criteria.
- [x] Add config parsing and validation tests for `step_ca.clients`.
- [x] Add startup key-loading tests for optional and required clients.
- [x] Implement config and server startup loading.
- [x] Document the operator workflow.
- [x] Extend smoke coverage.
- [x] Run formatting, unit tests, coverage, smoke tests.
- [x] Open PR and assign Copilot.

Rollback:
- Remove `step_ca.clients` entries from config. Existing `auth.jwt_public_keys` and `certs/jwt-clients/*.pub` startup loading continue to work.

Evidence:
- `go test ./internal/config ./internal/localserver ./e2e/cmd/smoke`
- `go test ./...`
- `GOCACHE=/home/marka/.cache/go-build GOMODCACHE=/home/marka/go/pkg/mod scripts/check-go-coverage.sh` -> coverage 78.3%, threshold 75.0%.
- `make smoke` -> passed using `-issuer smoke-step-client`, which is loaded from `step_ca.clients` in `config/bridge-smoke.yaml`.
- Copilot review comments addressed: optional configured clients now skip any key load error unless `required: true`; issuer validation error text now mentions the leading alphanumeric requirement.

Note:
- `scripts/test-go-coverage.sh` initially failed under sandboxed `/tmp` caches due blocked module downloads, then failed under elevated network because the filesystem was full. The maintained coverage gate passed after pruning Docker build cache and using existing home Go caches.

---

# User-Owned Provider Runtime

Mode: Approval-Required, approved by user request on 2026-08-14.

Governing PRD section: `7.6 Debian/Ubuntu Distribution`.

Scope:
- Make the packaged provider runtime installer default to a user-owned runtime directory for self-updating provider CLIs.
- Preserve `/opt/bridgectl` as an explicit root-controlled runtime path for pinned provider installs.
- Update package docs and examples so native provider updaters do not target root-owned `/opt` by default.
- Add unit coverage for config expansion/validation and Linux e2e coverage for the packaged installer path.

Constraints:
- The bridge package must still boot without provider CLIs or API keys.
- Do not install third-party provider CLIs during `apt install`.
- Keep existing `runtime.provider_root` semantics for absolute paths and relative provider binary/arg resolution.
- Do not require root for user-owned provider runtime installs.

Tradeoffs:
- User-owned runtime directories fit fast-moving native provider updaters but reduce package-level version pinning.
- Root-owned `/opt/bridgectl` remains useful for reproducible deployments that accept privileged updates.

Risks:
- Provider runtime installs rely on Node.js being present for unprivileged runs; root-only Node bootstrap must not obscure that requirement.
- `$HOME` and `XDG_DATA_HOME` expansion must be deterministic and fail clearly when it cannot produce an absolute path.

Test Strategy:
- Add config unit tests proving `runtime.provider_root` expands `$HOME`, `${HOME}`, `$XDG_DATA_HOME`, and rejects unresolved or relative values.
- Add installer e2e coverage in an Ubuntu container proving a non-root user can install the provider runtime into a user-owned directory and verify stubbed provider CLIs.
- Run focused Go tests plus the new Linux e2e script.

Rollback:
- Set `INSTALL_DIR=/opt/bridgectl` when running `/usr/lib/bridgectl/install-provider-runtime`.
- Revert the installer default and docs if user-owned provider self-updates are no longer supported.

Execution Checklist:
- [x] Update `PRD.md` with user-owned provider runtime acceptance criteria.
- [x] Add failing config unit tests for runtime root expansion.
- [x] Add Linux e2e coverage for non-root provider runtime installation.
- [x] Implement installer default path and root/user behavior.
- [x] Update Ubuntu install docs and packaged example config.
- [x] Run formatting, unit tests, and Linux e2e verification.
- [x] Record evidence and any lessons.

Evidence:
- `go test ./internal/config -run TestLoadRuntimeProviderRoot -count=1` failed before implementation because `${HOME}` and `$XDG_DATA_HOME` were not expanded.
- `go test ./internal/config -count=1` -> passed.
- `go test ./...` -> passed.
- `SUITE=noble scripts/smoke-provider-runtime-user.sh` -> passed with Docker escalation; verified non-root install to `/home/ubuntu/.local/share/bridgectl/providers` and no `/opt/bridgectl` directory.

---

# Issue 180 Environment-Only Container Startup

Mode: Approval-Required, approved by user request on 2026-08-16.

Governing PRD section: `7.8 Published Container Runtime`.

Scope:
- Make the published-style Docker image usable from environment variables without mounted provider YAML.
- Expose all bundled provider CLIs on `PATH`.
- Add provider-scoped unprotected mode for Codex, Claude, OpenCode, and Gemini with protected behavior as the default.
- Make the Docker entrypoint honor supplied command arguments after initialization.
- Add manual Docker SDK e2e coverage proving Codex and Claude protected/unprotected behavior against a disposable repo `.git` path.

Constraints:
- Do not enable unprotected mode globally or by default.
- Invalid unprotected env values must fail closed.
- Live unprotected provider e2e tests are manual only because they use credentials and permissive agent modes.
- The e2e repo is disposable and must not mount the host checkout as the provider workspace.

Tradeoffs:
- Provider-specific env vars keep the public API explicit and avoid inventing a broad provider override language.
- Applying unprotected args to startup probes makes health reflect session behavior, at the cost of surfacing invalid env values earlier.
- `.git` marker writes give a concrete protected-path signal, but the live tests remain slower and credential-dependent.

Risks:
- Provider CLIs may change permissive-mode flags; docs and e2e coverage need to catch drift.
- Claude bypass mode has root/sandbox constraints, so the container must keep running providers as the non-root `bridge` user.
- Manual e2e runs spend live provider credits and can be flaky if provider services are degraded.

Test Strategy:
- Add unit tests for provider-scoped unprotected argument parsing and failure paths.
- Add Docker entrypoint behavior coverage where practical.
- Add manual Docker Compose e2e coverage that starts the bridge image and uses the Go SDK to verify Codex and Claude protected/unprotected `.git` marker behavior.
- Run focused Go tests, formatting, and non-live test suites locally; document any skipped live e2e evidence.

Rollback:
- Unset all `BRIDGE_<PROVIDER>_UNPROTECTED` env vars to restore protected behavior.
- Override `BRIDGE_CONFIG` to a mounted custom config if the bundled Docker config is not desired.
- Revert the Docker symlinks and entrypoint arg dispatch if command override behavior causes operational issues.

Execution Checklist:
- [x] Update `PRD.md` with published container runtime requirements and acceptance criteria.
- [x] Add failing unit tests for provider-scoped unprotected mode.
- [x] Implement provider-scoped unprotected mode for sessions and startup probes.
- [x] Expose bundled CLIs on `PATH` in the Docker image.
- [x] Default the entrypoint to the Docker config and honor supplied Docker args.
- [x] Add manual Docker SDK e2e coverage for Codex/Claude protected and unprotected modes.
- [x] Update docs for environment-only container startup and provider-scoped unprotected env vars.
- [x] Run targeted verification and capture evidence.
- [x] Record lessons from implementation or verification gaps.

Evidence:
- `go test ./internal/provider -run 'TestBuildCommand.*Unprotected|TestProbeArgsUseProviderScopedUnprotectedMode' -count=1` failed before implementation because `commandArgsForProvider` did not exist.
- `go test ./internal/provider -count=1` -> passed.
- `go test -c -tags e2e -o /tmp/e2e-suite-check ./e2e/cmd/e2e-test` -> passed.
- `go test ./...` -> passed.
- `bash -n docker-entrypoint.sh e2e/scripts/test-entrypoint.sh` -> passed.
- `docker compose -f e2e/docker-compose.yml config` -> passed.
- `docker compose -f e2e/docker-compose.yml -f e2e/docker-compose.unprotected.yml config` -> passed.
- `docker build -t bridgectl:issue-180 .` -> passed.
- `docker run --rm --entrypoint sh bridgectl:issue-180 -lc 'command -v codex && command -v claude && command -v opencode && command -v gemini'` -> passed.
- `docker run --rm bridgectl:issue-180 id -un` -> passed; command args executed as `bridge` after initialization.
- Detached default-start smoke with `docker run -d --name issue180-default bridgectl:issue-180` stayed running and logged `secure (mTLS+JWT on [::]:9445)`.
- `env-secrets aws -s /bridgectl/e2e -- make test-e2e-unprotected` -> passed. Protected pass verified Claude and Codex did not write `.git` markers; unprotected pass verified Claude and Codex wrote provider-specific `.git` markers through SDK-started sessions.

# Session takeover attachment order (2026-09-10)

Mode: Autonomous; localized CLI ordering fix authorized by the user. Governing requirement: PRD §6.2 Human Interjection.

Plan:
- [x] Reproduce the actual CLI takeover failure with a PTY and isolated echo session.
- [x] Wait for ATTACHED before claiming and enable input/resize only after success.
- [x] Verify takeover, old-writer observation, input, detach, and failure behavior.
- [x] Run formatting, full race tests, lint, and the maintained coverage gate.
PR workflow: open a PR, request Copilot, address feedback, and record final review/CI evidence in the PR.

Scope: CLI sequencing and regression coverage; preserve server permission checks and SDK contracts. Risk: starting input too early or losing claim errors in stream handling. Rollback: revert the CLI change; no deployment or data migration is involved.

Evidence:
- Before the fix, `go test ./e2e/bridgectl -run 'TestCLISuite/TestCLITakeover$' -count=1` failed with `claim writer: permission denied`.
- Missing-session regression initially failed because the CLI printed NotFound but exited successfully.
- `go test ./e2e/bridgectl -run 'TestCLISuite/TestCLITakeover' -race -count=1` passed the initial three cases after the fix; the final suite includes four matching tests with EOF and server-cancellation subcases.
- `make fmt`, `make test`, and `make lint` passed; lint reported zero issues.
- `make test-cover-maintained` passed at 78.2% (75% minimum).
- `pnpm --dir docs build` passed.
- Rollback scope verified: only CLI behavior changes; server authorization, SDK contracts, and persistent data are unchanged. The reported desktop was not modified.

PR #218 review cycle 1:
- Scored both Copilot threads 2 (behavior fix with validation): observer attachment errors and terminal resize synchronization.
- Added assertions that failed before the review fixes, then passed for observer NotFound errors and initial terminal dimensions. Delayed the fake server's ATTACHED event to verify a claim cannot precede acknowledgement.
- CI's lint tooling install failed before lint ran: goimports@latest requires Go 1.26, while CI uses Go 1.25.7. Pinned goimports to v0.44.0, matching go.mod and supporting Go 1.25.

PR #218 review cycle 2:
- Scored both Copilot threads 2 (localized behavior fixes with regression coverage): queued terminal input on startup failure and EOF before attachment acknowledgement.
- Both new assertions failed before the fixes: a poll found queued input after rejected takeover, and unacknowledged EOF exited successfully.
- Flush pending terminal input before restoring a writer terminal when startup never became ready, and return an attachment error for unacknowledged EOF.
- After the cycle 2 fixes, all four takeover cases passed with race detection; `make test` and `make lint` passed again.

PR #218 review cycle 3:
- Server-originated Canceled before ATTACHED: score 2, fixed by checking the local context rather than classifying every Canceled status as a user cancellation; added a failing-then-passing CLI regression.
- Illumos build tag: score 0, no change. `GOOS=illumos GOARCH=amd64 go list -f '{{.GoFiles}}' ./cmd/bridgectl` selects `terminal_input_sysv.go` because illumos satisfies Solaris build tags.
- Also addressed review notes by serializing dimension sampling and resize RPCs, discarding unread input on every writer exit regardless of reader startup timing, and clarifying the historical test count.

Final CI coverage integration:
- Codecov patch coverage initially reported 0% because the e2e suite built a CLI without coverage instrumentation, even though it exercised the changed code.
- The coverage job now instruments the real CLI, collects subprocess counters in a temporary directory, and appends their atomic profile to the Go test profile. This measures the existing end-to-end assertions without lowering coverage gates.
- `make test-cover` passed with subprocess instrumentation: `attachSession` is 77.4% covered and the Linux input-discard helper is 100% covered. Lint and shell syntax checks passed.

# PR #230 question telemetry review (2026-09-13)

Mode: Approval-Required feature/security-sensitive telemetry work; explicitly
authorized by the user. Governing requirements: PRD §17 (`TEL-001`–`TEL-006`).

Scope:
- Rebase the four PR #230 changes conceptually onto current `main` in an isolated
  worktree and keep live Supervisor wiring/reporting out of scope for #231.
- Correct lint, privacy, persistence-permission, and coverage defects in the
  telemetry foundation.
- Add a deterministic black-box e2e test for question → answer → JSONL → feedback.

Risks and tradeoffs:
- PTY reads/writes are arbitrary chunks, not logical prompts. The foundation
  will state its framing boundary explicitly; #231 must frame/deduplicate live
  provider streams before calling it.
- Making persistence asynchronous without a lifecycle/flush contract can lose
  events. That integration behavior remains in #231 rather than adding a hidden
  goroutine to the foundation.
- Telemetry can contain credentials. Redaction must precede persistence and
  fingerprinting, and existing file permissions must be hardened.

Test strategy:
- First add and run the package-level e2e test against `main` to prove the
  telemetry package/pipeline is absent.
- Add focused table-driven unit tests for classification, decisions, redaction,
  JSONL errors/permissions/concurrency, deterministic feedback, and edge cases.
- Run race tests, the dedicated e2e target, maintained coverage, lint, and the
  full repository test suite.

Rollback:
- Revert the new telemetry package, e2e test/target, and PRD section. No runtime
  wiring, migration, or external data is introduced by #230.

Execution checklist:
- [x] Review PR #230 and issues #231/#232; identify scope and current CI failures.
- [x] Create `review/pr-230` from current `main` in a separate worktree.
- [x] Add governing PRD requirements before implementation.
- [x] Add the failing e2e proof.
- [x] Apply and correct the PR #230 foundation.
- [x] Run focused and full verification with coverage and lint evidence.
- [x] Record final review findings, evidence, and reusable lessons.

Review outcome and evidence:
- Original PR CI: `go-lint` failed on unchecked `Close` plus two formatting
  errors; Codecov reported 73.43% patch coverage and 0% for `jsonl.go`.
- The first `make test-telemetry-e2e` run against `main` failed because
  `internal/telemetry` did not exist. After applying PR #230 unchanged, it
  failed because `Authorization: Bearer top-secret-1` persisted the token.
- A later permission-classification assertion failed because a realistic
  `Do you want me to run /tmp/build-42?` prompt was labeled confirmation.
- The final e2e test passes across question detection, per-session answer
  correlation, default redaction, JSONL persistence, file permissions, stable
  aggregation, decisions, and schema-v1 export.
- `make test-telemetry-e2e` -> passed with race detection.
- `go test -race -count=1 -coverprofile=/tmp/telemetry-cover.out
  ./internal/telemetry` -> passed; 99.0% statement coverage.
- `make lint` -> passed; zero issues.
- `make test-cover-maintained` -> passed; 80.9% overall, above 75%.
- `make test` -> passed with race detection across all packages. The isolated
  worktree required `GOFLAGS=-buildvcs=false`; socket-based existing tests were
  run with loopback access outside the filesystem sandbox.
- After fast-forwarding new test-only commits from `main`, the full parallel
  race suite reproduced an existing boundary-tight repo-setup e2e timeout: its
  setup and context budgets were both only five seconds from the relevant
  operation. Increasing the test-only budgets to 10/20 seconds preserves the
  behavior under test and removes load-sensitive failure.
- `go build ./cmd/...` -> passed with the same VCS-stamping workaround.
- Follow-up review found that direct `make build` still failed because Go
  discovered a non-repository `/tmp/.git` sandbox marker before interrogating
  the linked worktree. The Make build commands now pass `-buildvcs=false`; the
  explicit `main.version` ldflag remains the authoritative build version. They
  also use the same writable Go-cache fallback as the repository test scripts.
- No Supervisor/config/CLI runtime integration was added; rollback remains a
  source-only revert with no persisted schema migration.

# Issue #231 live question telemetry (2026-09-13)

Mode: Approval-Required telemetry/runtime behavior change, explicitly authorized
by the user. Governing requirements: PRD §17.2 (`TEL-101`–`TEL-107`).

Plan:
- [x] Define live telemetry acceptance criteria and retain local-only storage.
- [x] Add failing config, framing, async failure-isolation, reporting, CLI, and
  Supervisor integration tests.
- [x] Implement a bounded asynchronous collector with deterministic flush.
- [x] Wire session lifecycle, output framing, and authorized input framing into
  Supervisor without changing provider I/O behavior.
- [x] Add telemetry YAML configuration and local-server lifecycle ownership.
- [x] Add rolling-window `telemetry report` and generic schema-v1 JSON export.
- [x] Add Claude/Codex prompt fixtures and tests.
- [x] Add and run a real bridge Docker Compose telemetry e2e.
- [x] Run formatting, lint, race tests, coverage, build, and document evidence.

Constraints and risks:
- Never perform filesystem/network sink work on provider I/O goroutines.
- Keep the queue bounded; dropping telemetry is preferable to blocking a
  session. Surface drop/write counts in logs without logging event text.
- Preserve immediate raw PTY input writes; framing operates on a copy.
- Flush only within the existing bounded server shutdown lifecycle.
- Keep shared collectors, S3, and generic performance findings in #232.

Rollback:
- Disable `telemetry.enabled` to remove all live capture without changing
  sessions. Reverting #231 removes only optional local events/CLI reporting;
  there is no database or remote-service migration.

Evidence:
- Focused race tests for config, telemetry, Supervisor integration, the CLI,
  and local path expansion passed.
- `make test-e2e-live-telemetry` passed against the real Compose bridge and a
  separate client container, validating start/question/accepted-answer/end
  events from the shared mode-`0600` JSONL file. A later display-only formatter
  rerun exhausted the host Docker filesystem while recompiling images; it did
  not reach the test phase or invalidate the completed e2e run.
- `make build` passed from the linked `/tmp` worktree.
- `make lint` passed with zero issues.
- `make test` passed the full race suite.
- `make test-cover-maintained` passed at 81.3% overall coverage (75% required);
  `internal/telemetry` measured 88.4%.

## Issue #231 telemetry filtering, collector, and retention follow-up (2026-09-13)

Mode: Approval-Required data/runtime and cross-service change, explicitly
authorized by the user. Governing requirements: PRD §17.2 (`TEL-108`–`TEL-110`).

Plan:
- [x] Define event-filter, collector, and bounded-storage acceptance criteria.
- [x] Add failing tests for kind validation/filtering and JSONL rotation reads.
- [x] Add failing HTTP collector and delivery tests with bounded request sizes.
- [x] Implement filter-aware live capture and rotating private JSONL storage.
- [x] Implement the collector command and asynchronous bridge HTTP delivery.
- [x] Add a standalone collector Compose stack with a persistent volume.
- [x] Update the real bridge Docker e2e to cross the collector service boundary.
- [x] Document operator configuration, security boundary, and retention math.
- [x] Run formatting, lint, race, coverage, build, Compose, and Docker e2e gates.

Constraints and risks:
- Filtering must occur before local or remote delivery; excluded kinds must not
  consume collector storage.
- Collector requests are redacted normalized events, never raw PTY streams.
- Collector downtime may drop bounded telemetry but must not affect sessions.
- The listener is unauthenticated in this increment and must remain loopback or
  private-network only; public exposure is unsupported.
- Storage is bounded by approximately `max_file_size_bytes * max_files`, plus
  at most one oversized event.

Rollback:
- Remove `collector_url` to return to local rotating JSONL, or disable telemetry
  entirely. Existing JSONL files remain readable and require no migration.

Evidence:
- Focused race tests passed for kind filtering, validation, HTTP delivery,
  request limits, rotation, retained-file ordering, and collector CLI parsing.
- The standalone `telemetry/docker-compose.yml` built and started a non-root,
  read-only-root collector; its health check returned success, a smoke event
  returned HTTP 202, and the in-container report read the volume-backed event.
- `make test-e2e-live-telemetry` passed across separate bridge, collector, and
  client containers. The volume contained exactly the configured `question`
  and `answer` records with a matching fingerprint and accepted decision.
- Both Compose files passed `docker compose config` validation.
- `docker build --check -f telemetry/Dockerfile .` completed with no warnings.
- `make test` passed the full race suite; `make test-cover-maintained` passed at
  81.6% overall (75% required), with `internal/telemetry` at 87.9%.
- `make lint` passed with zero issues and `make build` passed.

# Durable gRPC Telemetry and S3 Collector

Mode: Approval-Required, approved by user on 2026-09-14.

Governing PRD: `17.2 Live Question Telemetry (#231)`, TEL-111 through TEL-113.

Scope and decisions:
- Replace per-event HTTP delivery with acknowledged bidirectional gRPC batches.
- Use one bounded segmented JSONL spool for bridge outbox, collector volume
  retention, and collector-to-S3 upload staging.
- Acknowledge only after collector durable persistence; retry stable segment IDs
  idempotently across disconnects and restarts.
- Use standard AWS configuration and require the operator/E2E harness to supply
  an existing bucket.
- Evict oldest segments at configured storage limits and report the loss.

Risks:
- Telemetry contains redacted but still potentially sensitive context; spool
  files remain private and transport requires an explicit private/insecure or
  TLS choice.
- Network ambiguity can cause retries; stable segment IDs must prevent duplicate
  collector files and S3 objects.
- S3 failures can exhaust the collector spool; failure must remain observable
  without blocking agent sessions.

Test strategy:
- Unit tests first for segment rotation/eviction/recovery, idempotent acceptance,
  acknowledgement/replay, and S3 upload retention on failure.
- In-process gRPC integration test for disconnect and resend behavior.
- Opt-in real-S3 E2E using a unique key prefix in an operator-created bucket.
- Existing volume Compose E2E remains green through the same spool path.

Rollback:
- Clear `telemetry.collector_target` to return to local segmented persistence.
- Run the collector without S3 flags to retain segments only in its volume.
- Revert the gRPC service/config fields to restore the earlier HTTP collector.

Execution checklist:
- [x] Update PRD and record approved architecture.
- [x] Add failing segmented-spool and gRPC contract tests.
- [x] Implement bounded durable segmented spool and idempotent acceptance.
- [x] Implement bridge gRPC sender, acknowledgements, retry, and shutdown.
- [x] Implement collector S3 uploader and CLI/Compose configuration.
- [x] Add opt-in real-S3 E2E without bucket lifecycle permissions.
- [x] Update examples and operator documentation.
- [x] Regenerate protobufs and run formatting, lint, unit, race, coverage, and E2E checks.
- [x] Record evidence and lessons.

Evidence:
- Spool, gRPC collector, forwarder-retry, complete-outage retention, and S3
  failure-retention tests failed before their implementations and now pass
  under the race detector.
- `make build` passed with regenerated protobuf and gRPC stubs.
- `make test` passed the full race-enabled Go suite.
- `make lint` passed with zero issues.
- `make test-cover-maintained` passed at 80.8% (75% required), with
  `internal/telemetry` at 81.0%.
- `make test-e2e-live-telemetry` passed across the real bridge, gRPC collector,
  and client containers, verifying correlated question/answer data from the
  collector's segmented volume.
- Both Compose files passed `docker compose config`; the standalone collector
  image built, started as non-root with a read-only root filesystem, and its
  gRPC health check returned `SERVING`.
- `make up-collector`, `make down-collector`, `make ps-collector`, and
  `make logs-collector` provide consistent operator entrypoints; the down target
  preserves the named volume.
- Each operator Compose stack has a matching reset target that asks for explicit
  confirmation before running `down -v` and deleting its volumes.
- `TestTelemetryGRPCToS3` compiles and skips unless
  `BRIDGECTL_TELEMETRY_S3_BUCKET` names an operator-created bucket. The live AWS
  proof remains for the operator to run once that bucket exists.

Observed failure and correction:
- The first Docker rerun correctly failed because the bridge outbox targeted a
  root-owned repository volume. The outbox now uses a dedicated bridge-state
  volume whose mount point the entrypoint assigns to the non-root bridge user.
- A subsequent build exhausted the host filesystem. With explicit approval,
  4.675 GB of unused Docker build cache was removed; no images, containers,
  volumes, or source files were deleted.

## Full bidirectional interaction capture (2026-09-14)

Mode: Approval-Required security-sensitive data expansion, explicitly approved
by the user. Governing requirement: PRD §17.2 (`TEL-114`).

Scope and decisions:
- [x] Define an opt-in full-interaction contract without changing safe defaults.
- [x] Add failing tests for `provider_output`, `user_input`, thinking streams,
  sequencing, redaction, invalid UTF-8 omission, and the `all` selector.
- [x] Capture every authorized input and provider output chunk before semantic
  framing while retaining derived question/answer events.
- [x] Extend collector validation, CLI filters, configuration, documentation,
  example configuration, and Docker E2E coverage.
- [x] Run focused race tests, full tests, coverage, lint, build, Compose
  validation, and the live telemetry Docker E2E.
- [x] Record evidence, failure paths, rollback, and reusable lessons.

Risks and constraints:
- Full transcripts can contain credentials, personal data, proprietary code,
  and model reasoning. Capture remains disabled unless stream kinds are
  explicitly selected; redaction must happen before all persistence/network
  boundaries, and invalid opaque bytes must never bypass inspection.
- PTY and structured-provider reads are chunks rather than logical turns.
  Preserve their source ordering and stream identity while continuing to emit
  framed semantic question/answer events for analysis.
- Telemetry remains off the provider I/O path and bounded. Full capture may
  increase queue drops and spool eviction, which must remain observable without
  delaying sessions.

Rollback:
- Remove `provider_output` and `user_input` (or `all`) from `telemetry.kinds` to
  return immediately to derived telemetry without changing stored segment
  format or collector deployment. Existing full-stream segments remain subject
  to the configured volume/S3 retention policy.

Evidence:
- The initial focused tests failed because the stream kinds, typed provider
  streams, sequencing fields, and `all` expansion did not exist. Malformed
  collector events were then proven to fail only after correcting their outer
  segment IDs so the intended validation path was actually reached.
- Focused race tests pass for full bidirectional capture, normal/thinking stream
  identity, authorized input, monotonic sequences, secret/ANSI removal, invalid
  UTF-8 omission, collector validation, and configuration/CLI parsing.
- `make test-e2e-live-telemetry` passed through the real bridge, durable gRPC
  outbox, collector, and collector-owned volume. It persisted an ordered eight
  event session containing lifecycle, provider output, question, user input,
  answer, and subsequent provider output records.
- `make test` passed the full race-enabled repository suite; `make lint`
  reported zero issues; `make build` passed; and maintained coverage was 80.9%
  overall with `internal/telemetry` at 81.5% (75% required).
- The first Docker build was stopped when the host reached 100% usage. Removing
  only 3.424 GB of unused build cache restored enough space; no images,
  containers, named volumes, or telemetry data were deleted. The subsequent
  E2E passed and removed only its isolated test volumes.
- The operator config at `~/.config/bridgectl/bridge.yaml` now selects `all`
  with redacted text enabled and remains mode `0600`; a collector rebuild and
  server restart are required before the running processes use the new schema.

---
