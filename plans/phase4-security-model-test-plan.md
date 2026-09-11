Phase 4 Security Model — Test Plan

## Overview

This plan covers manual and automated testing of all features introduced in the
`feat/phase4-security-model` branch. Each section has a checkbox so progress can
be tracked as testing proceeds.

The branch introduces:

- **Tier-2 PKI**: Step CA integration for server certificate issuance
- **OIDC client enrollment**: browser-based login for human operator credentials
- **Writer slot release notification**: `WRITER_RELEASED` broadcast on detach
- **Dual-CA architecture**: local CA for CLI credentials + Step CA root in bundle
- **Re-attachment safety**: stale channel cleanup on same-clientID reconnect

All tests should run on **Linux (Docker)**, **Linux (desktop)**, and
**macOS (desktop)**. A Windows section is included for future coverage.

---

## Prerequisites

| Requirement | Where |
|---|---|
| Go toolchain (1.26+) | local machine and Docker image |
| `make`, `docker`, `docker compose` | local machine |
| `bridgectl` binary | built via `make build` or `go build ./cmd/bridgectl` |
| Step CA server + OIDC provider | Tier-2 tests only (Sections 6, 7) |
| `step` CLI | Tier-2 tests only — install from https://smallstep.com/cli/ |

---

## How to Run

### Docker (Linux container)

All tests that do not require a real Step CA instance or interactive terminal
can run inside the existing e2e Docker harness:

```bash
# Full CLI e2e suite (includes new Phase 4 tests once added)
make test-cli-e2e-docker

# Unit tests with race detection
docker run --rm -v "$PWD":/src -w /src golang:1.26-bookworm \
  go test -v -race -count=1 -timeout 120s ./...
```

### Linux / macOS desktop (native)

```bash
# Unit tests
make test

# CLI e2e tests
make test-cli-e2e

# Coverage
make test-cover
```

### Environment isolation

All CLI e2e tests use `BRIDGECTL_STATE_DIR` pointed at a temp directory
(`testStateDir(t)`) so they never touch `~/.config/bridgectl/`. This works
identically in Docker and on the desktop.

---

## Section 1: Unit Tests and Coverage

- [x] **1.1 — Run unit tests with race detection** *(completed — macOS desktop)*
  - Command: `make test`
  - Expected: all tests pass, no race conditions detected
  - Platforms: Docker, Linux desktop, macOS desktop
  - Result: all 19 packages pass with `-race -count=1`, including e2e/bridgectl (20 tests)

- [x] **1.2 — Run test coverage and check thresholds** *(completed — macOS desktop)*
  - Command: `make test-cover && make test-cover-maintained`
  - Expected: coverage meets 75% threshold for maintained packages
  - Check new code coverage specifically:
    - `internal/localserver/pki.go` (Step CA + OIDC paths) — 78.8%
    - `internal/bridge/supervisor.go` (Detach return value) — 80.3%
    - `internal/pki/bundle.go` (AppendBundle) — 65.6%
  - Platforms: Docker, Linux desktop, macOS desktop
  - Result: overall 77.9% meets 75.0% threshold

- [x] **1.3 — Verify new PKI unit tests pass individually** *(completed — macOS desktop)*
  - Command: `go test -v -run TestEnsurePKI_StepCA ./internal/localserver/`
  - Command: `go test -v -run TestIssueClientCertViaOIDC ./internal/localserver/`
  - Command: `go test -v -run TestCopyFile ./internal/localserver/`
  - Expected: all 9 new tests pass
  - Platforms: Docker, Linux desktop, macOS desktop
  - Result: all 9 tests pass (4 StepCA + 4 OIDC + 1 CopyFile) with race detection on macOS

---

## Section 2: Tier-1 Auto-PKI (Backward Compatibility)

These tests verify that the default (no Step CA) path is unchanged.

- [x] **2.1 — Auto-PKI generation on first start** *(completed — macOS desktop, e2e test added)*
  - Steps:
    1. `export BRIDGECTL_STATE_DIR=$(mktemp -d)`
    2. `bridgectl server start --listen 127.0.0.1:0`
    3. Check `$BRIDGECTL_STATE_DIR/certs/` contains:
       `ca.crt`, `ca.key`, `server.crt`, `server.key`, `local-client.crt`,
       `local-client.key`, `jwt-signing.key`, `jwt-signing.pub`, `ca-bundle.crt`
    4. Health check: `bridgectl server status` shows running
    5. `bridgectl server stop`
  - Expected: all files created, server starts and stops cleanly
  - Platforms: Linux desktop, macOS desktop
  - Docker: covered by existing `TestSecureModeStartStop` in e2e suite
  - E2E: `TestAutoPKIGeneratesAllFiles` added — verifies all 9 files, 0600 perms on private keys, health check

- [x] **2.2 — Auto-PKI idempotency** *(completed — macOS desktop, e2e test added)*
  - Steps:
    1. Start server (auto-PKI generated)
    2. Note `ca-bundle.crt` modification time
    3. Stop server
    4. Start server again with same state dir
    5. Check `ca-bundle.crt` modification time is unchanged
  - Expected: second start reuses existing certs, no regeneration
  - Platforms: Linux desktop, macOS desktop
  - Docker: covered by `TestEnsurePKI_Idempotent` unit test
  - E2E: `TestAutoPKIIdempotentAcrossRestart` added — byte-for-byte comparison of ca-bundle.crt and ca.crt across restart, health check after restart

- [x] **2.3 — Tier-1 client certificate issuance** *(completed — macOS desktop, e2e test added)*
  - Steps:
    1. Start server in secure mode
    2. `bridgectl server issue-client --name testclient`
    3. Check `$STATE_DIR/certs/clients/testclient/` contains:
       `testclient.crt`, `testclient.key`, `jwt-signing.key`
    4. Connect using issued credentials — health check succeeds
    5. Stop server
  - Expected: client cert issued and usable
  - Platforms: Linux desktop, macOS desktop
  - Docker: covered by `TestIssueClientCert` unit test
  - E2E: `TestIssuedClientCertCanConnect` added — issues cert, verifies file layout + jwt-clients registration, restarts server, connects with issued creds (mTLS + per-client JWT), health check passes

- [x] **2.4 — Client name validation** *(completed — macOS desktop, e2e test added)*
  - Test each case and verify the expected result:
    - `--name ../escape` → error (path traversal)
    - `--name foo/bar` → error (slash in name)
    - `--name .hidden` → error (leading dot)
    - `--name valid-client_1.0` → success
    - `--name a` → success (single character)
  - Platforms: Docker, Linux desktop, macOS desktop
  - Docker: covered by `TestIssueClientCert_RejectsPathTraversal` unit test
  - E2E: `TestClientNameValidation` added — rejects 5 invalid names (path traversal, slash, dot, empty, spaces), accepts 5 valid names

---

## Section 3: Step CA Flag Validation

These tests verify error handling when Step CA flags are used incorrectly.
They do NOT require a running Step CA instance.

- [x] **3.1 — Missing --step-ca-root when --step-ca-url is set** *(completed — macOS desktop, e2e test added)*
  - Command: `bridgectl server start --listen 127.0.0.1:0 --step-ca-url https://ca.example.com`
  - Expected: error message indicating `--step-ca-root` is required
  - Platforms: Docker, Linux desktop, macOS desktop
  - E2E: `TestStepCAMissingRoot` — verifies `localserver.Start()` returns error containing "step-ca-root is required"

- [x] **3.2 — Nonexistent --step-ca-root path** *(completed — macOS desktop, e2e test added)*
  - Command: `bridgectl server start --listen 127.0.0.1:0 --step-ca-url https://ca.example.com --step-ca-root /nonexistent/root.crt`
  - Expected: error about missing root certificate file
  - Platforms: Docker, Linux desktop, macOS desktop
  - E2E: `TestStepCANonexistentRoot` — verifies error contains "copy Step CA root"

- [x] **3.3 — step CLI not on PATH** *(completed — macOS desktop, e2e test added)*
  - Steps:
    1. Ensure `step` is not installed (or remove from PATH)
    2. Create a dummy root cert: `echo "dummy" > /tmp/root.crt`
    3. `bridgectl server start --listen 127.0.0.1:0 --step-ca-url https://ca.example.com --step-ca-root /tmp/root.crt`
  - Expected: error message mentioning `step` CLI not found with link to
    https://smallstep.com/cli/
  - Platforms: Docker, Linux desktop, macOS desktop
  - Docker: covered by `TestEnsurePKI_StepCASkipsAutoGen` unit test
  - E2E: `TestStepCAMissingStepCLI` — creates dummy root, sets PATH to empty dir, verifies error mentions "step" and "smallstep.com/cli"

- [x] **3.4 — OIDC flag validation (missing combos)** *(completed — macOS desktop, e2e tests added)*
  - Test each case:
    - `--oidc-provider https://accounts.google.com` without `--step-ca-url` → error
    - `--oidc-provider https://accounts.google.com --step-ca-url https://ca.example.com` without `--step-ca-root` → error
    - All three flags but no `--name` → error
    - All three flags but `step` not on PATH → error with install instructions
  - Platforms: Docker, Linux desktop, macOS desktop
  - Docker: covered by `TestIssueClientCertViaOIDC_Validation` unit test
  - E2E: `TestOIDCFlagValidation` — table-driven test covering 5 cases (missing step-ca-url, missing oidc-provider, missing step-ca-root, invalid client name, step CLI not on PATH)
  - E2E: `TestOIDCMissingNameFlag` — runs CLI binary without --name, verifies Cobra rejects it

---

## Section 4: Writer Slot Release Notification

- [x] **4.1 — WRITER_RELEASED on writer disconnect** *(completed — macOS desktop, e2e test added)*
  - Steps:
    1. Start server (local or secure mode)
    2. Start an echo session
    3. Client A: attach as writer (ACTIVE role)
    4. Client B: attach as observer (OBSERVER role)
    5. Client A: disconnect (close stream)
    6. Client B: verify `WRITER_RELEASED` event received
  - Expected: observer receives the event promptly after writer disconnects
  - Platforms: Docker (via e2e test), Linux desktop, macOS desktop
  - E2E: `TestWriterReleasedOnDisconnect` — verifies observer receives WRITER_RELEASED with correct writer_client_id when writer disconnects

- [x] **4.2 — Writer eviction via ClaimWriter broadcasts notification** *(completed — macOS desktop, e2e test added)*
  - Steps:
    1. Start server, start echo session
    2. Client A: attach as writer
    3. Client B: attach as observer
    4. Client C: attach as observer, then `ClaimWriter(force=true)` to evict Client A
    5. Client B: verify `WRITER_CLAIMED` event received with Client C's ID
    6. Client A: verify it receives notification of eviction
  - Expected: all parties notified of writer change
  - Platforms: Docker (via e2e test), Linux desktop, macOS desktop
  - E2E: `TestWriterEvictionBroadcastsEvents` — 3 clients; verifies observer and evicted writer both receive WRITER_RELEASED + WRITER_CLAIMED with correct client IDs

- [x] **4.3 — Observer can claim writer after release** *(completed — macOS desktop, e2e test added)*
  - Steps:
    1. Start server, start echo session
    2. Client A: attach as writer, then release (ReleaseWriter)
    3. Client B (observer): call `ClaimWriter(force=false)`
    4. Client B: verify it can now `WriteInput` successfully
  - Expected: observer transitions to writer cleanly
  - Platforms: Docker (via e2e test), Linux desktop, macOS desktop
  - E2E: `TestObserverClaimsWriterAfterRelease` — writer releases voluntarily, observer claims with force=false, then writes input successfully

---

## Section 5: Re-attachment and Terminal Behavior

- [x] **5.1 — Same clientID re-attachment** *(completed — macOS desktop, e2e tests added)*
  - Steps:
    1. Start server, start echo session
    2. Attach with clientID "test-client" as observer
    3. Attach again with same clientID "test-client"
    4. Verify warning logged about re-attachment
    5. Verify the session continues to work (write/read)
    6. Verify no goroutine leak (check with `-race`)
  - Expected: stale channel closed, new attachment works, no race
  - Platforms: Docker, Linux desktop, macOS desktop
  - Note: Writer conflict check runs before re-attachment cleanup, so re-attach
    as observer and then ClaimWriter to reclaim the slot (matches real reconnect flow)
  - E2E: `TestSameClientIDReattachment` — same clientID observer re-attach triggers stale channel cleanup (confirmed by "client re-attaching" warning in logs), new stream receives ATTACHED event, race detection clean
  - E2E: `TestReattachmentAsObserverThenClaimWriter` — writer re-attaches as observer (stale writer channel cleaned up), reclaims writer via ClaimWriter(force=false), writes input successfully

- [ ] **5.2 — Terminal raw mode for all roles**
  - Steps:
    1. Start server, start echo session
    2. `bridgectl session attach <id>` (writer) — verify raw terminal mode
    3. `bridgectl session attach --observe <id>` (observer) — verify raw terminal mode
    4. Ctrl+C works in both modes
  - Expected: both writer and observer use raw terminal
  - Note: this is a manual interactive test, not automatable in Docker
  - Platforms: Linux desktop, macOS desktop

---

## Section 6: Tier-2 Step CA Integration

> **Prerequisite**: a running Step CA server with an OIDC provisioner.
> Skip this section if Step CA is not available.

- [x] **6.1 — Server start with Step CA** *(completed — macOS desktop, e2e test added with fake step binary)*
  - Steps:
    1. `bridgectl server start --listen 127.0.0.1:9445 --step-ca-url https://<ca>:443 --step-ca-root /path/to/root.crt`
    2. Verify server cert was obtained from Step CA (check `server.crt` issuer)
    3. Verify local CA was still generated (`ca.crt` exists, is self-signed)
    4. Verify `ca-bundle.crt` contains BOTH Step CA root AND local CA
    5. Health check passes with local-client credentials
  - Expected: dual-CA architecture works, server starts cleanly
  - Platforms: Linux desktop, macOS desktop
  - Note: e2e test uses fake `step` binary (placeholder certs), so full TLS
    health check requires a real Step CA. The file layout and dual-CA bundle
    structure are fully verified.
  - E2E: `TestStepCADualCAArchitecture` — verifies ca-bundle.crt starts with Step CA root + contains local CA PEM, all PKI files exist (server cert/key, local CA, local-client, JWT keypair)

- [x] **6.2 — Step CA idempotency** *(completed — macOS desktop, e2e test added with fake step binary)*
  - Steps:
    1. Start server with Step CA flags, stop it
    2. Start again with same state dir
    3. Verify `ca-bundle.crt` is not regenerated
  - Expected: second start is a no-op for PKI
  - Platforms: Linux desktop, macOS desktop
  - E2E: `TestStepCAIdempotency` — verifies bundle is byte-for-byte identical after second EnsurePKI call, even when root file is overwritten between calls

- [x] **6.3 — Tier-1 client cert still works against Step CA server** *(completed — macOS desktop, e2e test added with fake step binary)*
  - Steps:
    1. Start server with Step CA
    2. `bridgectl server issue-client --name sdk-client` (no `--oidc-provider`)
    3. Connect with issued credentials
    4. Health check succeeds
  - Expected: Tier-1 client issuance works even when server uses Step CA
  - Platforms: Linux desktop, macOS desktop
  - Note: full TLS connect + health check requires real Step CA; e2e test
    verifies cert file layout and JWT key registration.
  - E2E: `TestStepCATier1ClientIssuance` — initializes PKI with Step CA, issues Tier-1 client cert via local CA, verifies file layout and jwt-clients registration

---

## Section 7: OIDC Client Enrollment

> **Prerequisite**: Step CA with OIDC provisioner (Google, GitHub, Okta, etc.).
> Skip this section if not available.

- [x] **7.1 — OIDC client enrollment happy path** *(completed — macOS desktop, e2e test added with fake step binary)*
  - Steps:
    1. Start server with Step CA
    2. `bridgectl server issue-client --name human1 --oidc-provider https://accounts.google.com --step-ca-url <url> --step-ca-root <path>`
    3. Browser opens for OIDC login — complete authentication
    4. Verify `$STATE_DIR/certs/clients/human1/` contains:
       `human1.crt`, `human1.key`, `jwt-signing.key`
    5. Verify server-side JWT public key registered in `jwt-clients/` dir
    6. Connect with OIDC-issued credentials — health check succeeds
  - Expected: full OIDC flow works end-to-end
  - Platforms: Linux desktop (with browser), macOS desktop
  - Note: e2e test uses fake `step` binary; real OIDC browser login requires
    a live Step CA + OIDC provider.
  - E2E: `TestOIDCEnrollmentHappyPath` — verifies cert/key/JWT file layout and server-side jwt-clients registration

- [ ] **7.2 — OIDC cert is short-lived**
  - Steps:
    1. After 7.1, inspect `human1.crt` with `openssl x509 -in human1.crt -text`
    2. Check validity period
  - Expected: certificate has ~24 hour validity (per Step CA provisioner config)
  - Platforms: Linux desktop, macOS desktop
  - Note: requires real Step CA; fake step binary writes placeholder certs

- [x] **7.3 — Per-client JWT isolation** *(completed — macOS desktop, e2e test added with fake step binary)*
  - Steps:
    1. Issue two OIDC clients: `human1` and `human2`
    2. Verify each has its own `jwt-signing.key` (different key material)
    3. Verify each has its own entry in `jwt-clients/`
    4. `human1` credentials cannot impersonate `human2`
  - Expected: JWT keys are independent per client
  - Platforms: Linux desktop, macOS desktop
  - E2E: `TestOIDCPerClientJWTIsolation` — issues two clients, verifies unique JWT key material and independent jwt-clients registration

---

## Section 8: Documentation and Config Accuracy

- [x] **8.1 — Example config reflects two-tier model** *(completed — reviewed)*
  - File: `packaging/examples/bridge-example.yaml`
  - Verify:
    - Comments describe `--listen` as sufficient for secure mode
    - `tls` and `auth` fields are documented as optional for local use
    - No references to removed "SECURITY MODES" section
  - Result: all checks pass, no changes needed

- [x] **8.2 — Plan document accuracy** *(completed — fixed)*
  - File: `plans/user-session-and-human-interjection.md`
  - Verify:
    - Phase 4 section references "Tier 1" and "Tier 2" naming — correct
    - `AttachRole` enum used `ATTACH_ROLE_ACTIVE` — **fixed to `ATTACH_ROLE_WRITER`** to match proto
    - Step CA is described as optional — correct
  - Fix: renamed `ATTACH_ROLE_ACTIVE` → `ATTACH_ROLE_WRITER` at lines 149, 268

- [x] **8.3 — Security architecture matches implementation** *(completed — fixed)*
  - File: `plans/security-architecture.md`
  - Verify:
    - File layout diagram was missing `jwt-clients/` subdirectory — **fixed**
    - CLI flag names match implementation — correct
    - Dual-CA architecture described accurately — correct
    - Revocation path referenced wrong directory — **fixed** (`clients/<name>/` → `jwt-clients/<name>.pub`)
  - Fixes: added `jwt-clients/` to file layout diagram, corrected revocation path

---

## Section 9: E2E Test Updates

The existing CLI e2e tests (`e2e/bridgectl/cli_test.go`) cover server start/stop,
session lifecycle, attach, secure mode, and insecure client rejection. The Phase 4
features need new e2e test coverage.

### 9.1 — New e2e tests to add

Each test below should be added to `e2e/bridgectl/cli_test.go` following the
existing patterns: `testStateDir(t)` for isolation, `localserver.Start()` for
server lifecycle, `bridgeclient.New()` for SDK calls, `require`/`assert` for
assertions.

- [ ] **9.1.1 — `TestWriterReleasedNotification`**
  - Start server → start echo session → attach writer → attach observer →
    disconnect writer → verify observer receives `WRITER_RELEASED` event
  - Pattern: similar to `TestSessionAttachAndInput` but with two clients

- [ ] **9.1.2 — `TestClaimWriterForce`**
  - Start server → start echo session → client A attaches as writer →
    client B calls `ClaimWriter(force=true)` → verify client B can write →
    verify client A's `WriteInput` returns `PERMISSION_DENIED`
  - Pattern: two `bridgeclient.New()` instances, concurrent attach

- [ ] **9.1.3 — `TestObserverCannotWrite`**
  - Start server → start echo session → attach as observer →
    `WriteInput` returns `PERMISSION_DENIED`
  - Pattern: simple single-client test

- [ ] **9.1.4 — `TestReattachSameClientID`**
  - Start server → start echo session → attach with clientID X →
    attach again with clientID X → verify session works, no error
  - Pattern: sequential attach calls

- [x] **9.1.5 — `TestIssuedClientCertCanConnect`** *(completed — macOS desktop)*
  - Start secure server → stop → `IssueClientCert` → restart server (loads
    JWT key) → create new client with issued certs → health check succeeds
  - Pattern: extends `secureClient()` helper with issued (not local) creds
  - Also covers file layout assertions and jwt-clients registration

- [ ] **9.1.6 — `TestStepCAFlagValidation`**
  - Attempt `localserver.Start()` with `StepCAURL` but no `StepCARootPath` →
    verify error returned
  - Attempt with both flags but `step` not on PATH → verify error
  - Pattern: table-driven negative test

### 9.2 — Running e2e tests on each platform

#### Docker (Linux container)

The existing infrastructure supports this. All new tests run via:

```bash
make test-cli-e2e-docker
```

This uses `e2e/bridgectl/Dockerfile` (Go 1.26-bookworm) and
`e2e/bridgectl/docker-compose.yml`. No changes needed to the Docker setup
for tests that do not require Step CA.

- [ ] **9.2.1 — Verify `make test-cli-e2e-docker` passes with new tests**
  - Expected: all existing + new tests pass in Docker
  - The Docker image has Go + race detector + temp dirs available

#### Linux desktop

```bash
make test-cli-e2e
```

- [ ] **9.2.2 — Verify `make test-cli-e2e` passes on Linux desktop**
  - Expected: all tests pass natively
  - Note: secure mode tests require unix socket support (always available on Linux)

#### macOS desktop

```bash
make test-cli-e2e
```

- [x] **9.2.3 — Verify `make test-cli-e2e` passes on macOS desktop** *(completed)*
  - Expected: all tests pass natively
  - Note: tests already skip Windows-only paths (`runtime.GOOS == "windows"`)
  - Result: 20/20 tests pass (16 existing + 4 new Section 2 tests), 6.2s, race detection enabled

### 9.3 — Test code patterns to follow

When writing new e2e tests, follow these conventions from the existing suite:

```go
// Guard for long-running tests
if testing.Short() {
    t.Skip("skipping in short mode")
}

// Isolated state dir (never touches ~/.config/bridgectl/)
stateDir := testStateDir(t)

// Server lifecycle
srv, err := localserver.Start(localserver.Config{
    StateDir: stateDir,
    // For secure mode:
    // ListenAddr: "127.0.0.1:0",
    // ServerSANs: []string{"127.0.0.1"},
})
require.NoError(t, err)
defer srv.Stop()

// Client via SDK
client, err := bridgeclient.New(bridgeclient.WithTarget(srv.Target()))
require.NoError(t, err)
defer func() { _ = client.Close() }()

// For secure mode, use secureClient() helper
client := secureClient(t, target, stateDir)

// Timeouts
ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
defer cancel()
```

For multi-client tests (writer + observer), create two separate
`bridgeclient.New()` instances with distinct client IDs. Use goroutines
for concurrent attach streams and `sync.WaitGroup` or channels for
coordination.

### 9.4 — Step CA e2e tests (deferred)

Tests requiring a real Step CA instance (Sections 6 and 7) cannot run in the
standard Docker e2e harness without adding a Step CA container to the compose
stack. Options for future work:

- [ ] **9.4.1 — Add `step-ca` service to e2e docker-compose (future)**
  - Add `smallstep/step-ca` container to `e2e/bridgectl/docker-compose.yml`
  - Pre-configure with a test OIDC provisioner (or a JWK provisioner for
    non-interactive testing)
  - Add `TestStepCAServerStart` and `TestStepCAIssueClient` tests guarded
    by a build tag (`//go:build stepca`)
  - Makefile target: `make test-cli-e2e-stepca-docker`

---

## Section 10: Windows Testing (Future)

Windows support for secure mode is not implemented. The existing tests already
guard against this:

```go
if runtime.GOOS == "windows" {
    t.Skip("secure mode not supported on Windows")
}
```

When Windows support is added, the following areas need coverage:

- [ ] **10.1 — Local mode (named pipes instead of unix sockets)**
  - Verify `bridgectl server start` works on Windows with named pipes
  - Verify `IsServerRunning()` and `DiscoverTarget()` work
  - Verify session lifecycle (start, list, attach, stop)

- [ ] **10.2 — Secure mode on Windows**
  - Verify auto-PKI generation works (file paths, permissions)
  - Verify mTLS + JWT authentication
  - Verify client cert issuance

- [ ] **10.3 — Step CA on Windows**
  - Verify `step` CLI integration works on Windows
  - Verify OIDC browser flow launches correctly

- [ ] **10.4 — CI for Windows**
  - Add a Windows runner to `.github/workflows/ci.yml`
  - Run at minimum: `go test -race ./...` and `make test-cli-e2e`
  - Guard new secure-mode tests with `runtime.GOOS` checks

### Windows e2e infrastructure

When ready, add a Windows test path:

```yaml
# .github/workflows/ci.yml — future addition
go-test-windows:
  runs-on: windows-latest
  steps:
    - uses: actions/checkout@v4
    - uses: actions/setup-go@v5
      with:
        go-version-file: go.mod
    - run: go test -v -race -count=1 ./...
    - run: go test -v -count=1 -race -timeout 120s ./e2e/bridgectl/
```

Docker-based e2e on Windows would require Windows containers or WSL2,
which adds complexity. Start with native Go tests on a Windows runner
and defer containerized testing.

---

## Summary Checklist

| Section | Tests | Docker | Linux | macOS | Windows | Status |
|---|---|---|---|---|---|---|
| 1. Unit tests & coverage | 1.1–1.3 | yes | yes | yes | future | **done** (all pass, 77.9% coverage, macOS verified) |
| 2. Tier-1 auto-PKI | 2.1–2.4 | partial | yes | yes | future | **done** (e2e tests added, macOS verified) |
| 3. Step CA flag validation | 3.1–3.4 | yes | yes | yes | future | **done** (e2e tests added, macOS verified) |
| 4. Writer slot release | 4.1–4.3 | yes | yes | yes | future | **done** (e2e tests added, macOS verified) |
| 5. Re-attachment & terminal | 5.1–5.2 | partial | yes | yes | future | **5.1 done** (e2e tests added); 5.2 manual |
| 6. Tier-2 Step CA | 6.1–6.3 | future | manual | manual | future | **done** (stub tests; full TLS needs real Step CA) |
| 7. OIDC enrollment | 7.1–7.3 | future | manual | manual | future | **7.1, 7.3 done** (stub tests); 7.2 needs real Step CA |
| 8. Documentation | 8.1–8.3 | n/a | any | any | n/a | **done** (8.1 pass, 8.2 + 8.3 fixed) |
| 9. E2E test updates | 9.1–9.4 | yes | yes | yes | future | 9.1.5 done, 9.2.3 done |
| 10. Windows | 10.1–10.4 | — | — | — | future | future |
