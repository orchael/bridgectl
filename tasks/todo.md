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

# CLI Security Follow-ups (from PR #92 Copilot review)

## Deferred — cert lifecycle

- [ ] **Cert renewal**: Leaf certs (server, local-client) have 90-day validity but `EnsurePKI` uses `ca.crt` as sentinel and never regenerates them. Add expiry check (e.g. warn at 14 days, auto-regenerate at expiry) so secure mode doesn't silently break after 90 days.
- [ ] **SAN mismatch detection**: If the user restarts with different `--san` values, the existing server cert is reused even though its SANs don't cover the new names. Compare requested SANs against the cert's actual SANs and regenerate if they differ.

## Lower priority

- [ ] **Windows status message**: `server.go:71` says "unix socket" in local mode but Windows uses TCP localhost. Make the status text platform-aware.
- [ ] **pki_test.go portability**: `TestLoadPKIMaterial` hard-codes Unix-style `/tmp/...` paths. Use `filepath.Join` so it passes on Windows.
- [ ] **TestMain cleanup**: `defer os.RemoveAll(dir)` in `TestMain` never runs because `os.Exit(m.Run())` terminates first. Capture exit code, clean up, then exit.
- [ ] **Echo test assertion**: `TestSessionAttachAndInput` conditionally asserts `if got != ""` — silently passes when echo fails. Should require the expected output.

## Deferred — pre-existing / out of scope

- [ ] **GoReleaser Windows target**: `.goreleaser.yaml` includes `windows` for the CLI but `internal/provider/stdio.go` uses Unix-only APIs (`syscall.Kill`, `creack/pty`). Either add Windows build tags to the provider package or remove the Windows release target. (Pre-existing on parent branch.)
- [ ] **Docker E2E cleanup trap**: `scripts/test-cli-e2e-docker.sh` doesn't install a trap for Ctrl-C — compose stack can be left behind on interrupt.
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
