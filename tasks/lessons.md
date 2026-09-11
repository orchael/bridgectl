# Lessons

## 2026-09-11 Remote Step CA E2E False Success
- Incident/bug: Client enrollment requested `admin` although the test CA created `bridge-jwk`; the Make target still printed `PASSED` after the client exited 1.
- Root cause pattern: `docker wait` prints the container status to stdout and can return success itself for a failed container. Detached setup errors were also overwritten, and default Compose `ps` can omit an already-exited client.
- Follow-on failures: The client dialed `bridge-server` although the server certificate covered `server`; SDK tests expected a local CA bundle that client init does not create when given an external root path.
- Preventative rule: Align enrollment with CA initialization, dial a certificate SAN, supply the SDK trust bundle, capture the container status explicitly, include stopped containers during lookup, and preserve setup errors through cleanup.
- Validation added: `TestMakeTargetResult` exercises success, client failure, early exit, build/startup/lookup/wait errors, missing containers, and invalid wait output using a fake Docker executable against the real Make target.

## 2026-09-10 CLI Takeover Stream Ordering
- Incident/bug: `session attach --take-over` claimed the writer slot before opening its observer stream, returning `permission denied`.
- Root cause pattern: SDK `AttachSession` constructs a lazy wrapper; `RecvAll` performs the actual RPC. Existing handoff tests attached through the SDK and never exercised CLI ordering.
- Preventative rule: Gate dependent session RPCs on the server's `ATTACHED` event and test operator workflows through the real CLI with a PTY.
- Validation added: CLI takeover regression reproduces the original error, then verifies writer transfer, input, observer continuity, and detach; rejected-claim coverage checks error propagation and input/resize gating.

## 2026-03-30 PTY Transport Test Execution
- Incident/bug: A provider unit test that executed a temp script failed inside the sandbox with `operation not permitted`.
- Root cause pattern: Tests that shell out in this environment can fail for sandbox reasons unrelated to application logic.
- Early signal missed: The first version of the startup-probe test assumed subprocess execution was always allowed in unit tests.
- Preventative rule: Keep unit tests for provider construction/path assembly pure where possible; reserve subprocess behavior checks for integration/e2e layers.
- Validation added (test/check/alert): Replaced the exec-heavy unit test with a pure command-construction test and kept PTY behavior validation in the higher-level smoke path.

## 2026-04-23 Apt Package Smoke Harness
- Incident/bug: The first Debian package smoke test installed successfully but still failed the health check.
- Root cause pattern: Docker port publishing cannot reach a service that is intentionally bound to `127.0.0.1` inside the container.
- Early signal missed: The packaged default config is localhost-only by design, but the first smoke harness assumed host-to-container access over a published port.
- Preventative rule: When a packaged service defaults to loopback-only binding, run health verification inside the target environment or through an explicit tunnel instead of relying on Docker port publishing.
- Validation added (test/check/alert): Updated `scripts/smoke-apt-local.sh` to execute the gRPC healthcheck inside each Ubuntu container, matching packaged service behavior.

## 2026-08-14 Detached Docker Smoke Setup
- Incident/bug: The provider runtime smoke test tried to exec into a detached Ubuntu container before package/user setup had completed.
- Root cause pattern: `docker run -d` returns after the container starts, not after an inline bootstrap script reaches its steady state.
- Early signal missed: The first harness assumed a missing packaged file meant package contents were wrong, but the generated `.deb` contained the file.
- Preventative rule: Detached container smoke tests must create and wait on an explicit readiness marker before running assertions or follow-up exec commands.
- Validation added (test/check/alert): `scripts/smoke-provider-runtime-user.sh` waits for `/tmp/provider-runtime-smoke-ready` before running the non-root provider runtime installer.

## 2026-08-16 Docker Config Certificate Name Alignment
- Incident/bug: Changing the container default config to `bridge-docker.yaml` initially made default image startup fail because the entrypoint generated `bridge.crt` while the Docker config expected `bridge.local.crt`.
- Root cause pattern: Docker entrypoint-generated filenames are part of the config contract; changing the default config without checking generated certificate names creates a startup-only failure.
- Early signal missed: Compose overrides had been setting `BRIDGE_CN=bridge.local`, masking the mismatch in normal compose-based development.
- Preventative rule: When changing default container config or certificate CN defaults, run a no-args detached image startup smoke and verify the container stays running.
- Validation added (test/check/alert): `docker run -d --name issue180-default bridgectl:issue-180` stayed running after aligning `BRIDGE_CN`, `BRIDGE_CLIENT_CN`, and SAN defaults with `bridge-docker.yaml`.

## 2026-08-16 Live Provider TUI E2E Input
- Incident/bug: The first unprotected-mode e2e harness treated echoed prompt text as completion and sent Claude's prompt plus Enter in one PTY write, leaving Claude's TUI composer unsubmitted.
- Root cause pattern: Interactive provider CLIs echo prompts and can treat pasted text differently from a separate submit key, so transcript literals alone are not a reliable proof of action.
- Early signal missed: Codex and Claude transcripts showed the prompt in the composer with zero tokens, but the test advanced because the completion marker was present in the echoed prompt.
- Preventative rule: For live provider e2e tests, prove behavior through external state first, then use transcript markers only as secondary evidence; send provider-specific submit keys as separate PTY writes when needed.
- Validation added (test/check/alert): `env-secrets aws -s /bridgectl/e2e -- make test-e2e-unprotected` passed with protected and unprotected Codex/Claude `.git` marker checks.
