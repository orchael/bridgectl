# AI Agent Bridge - Product Requirements Document

## 1. Overview

The AI Agent Bridge is a standalone server and Go SDK that provides a secure, zero-trust communication layer between control-plane systems and AI agent processes running on local or remote machines.

It enables two primary consumers to orchestrate AI agents:

- **prd-manager-control-plane** - A Go HTTP API server that manages PRD projects, sessions, and agent workflows via Slack and CLI interfaces. Currently manages agent subprocesses in-process (via `internal/agent/Manager`).
- **ndara-ai-orchestrator** - A Go gRPC-based orchestrator that coordinates repo-level AI agents across machines using mTLS + JWT authentication. Dispatches tasks and streams events from agent daemons.

The bridge replaces direct in-process agent management with a networked, provider-agnostic daemon boundary that both projects can integrate via a shared Go SDK.

---

## 2. Problem Statement

### Current Limitations

1. **prd-manager-control-plane** manages agent subprocesses in-process (`exec.CommandContext`). Agent crashes can crash the API server. There is no reconnect/replay for event streams, no multi-provider support, and no remote agent capability.

2. **ndara-ai-orchestrator** has a well-designed gRPC + mTLS architecture for remote agent daemons, but the agent daemon (`agentd`) is a stub that does not actually supervise AI agent processes. It lacks provider adapters (codex, claude, opencode) and subprocess lifecycle management.

3. Neither project has a shared, reusable agent bridge. Each would need to independently solve agent subprocess management, provider abstraction, event streaming, and security.

### What the Bridge Solves

- **Failure isolation**: Agent crashes do not crash the control-plane processes.
- **Reconnect/replay**: Event streams support sequence-based resume.
- **Provider abstraction**: Codex, Claude, and OpenCode behind a single gRPC API.
- **Local + remote agents**: Same protocol for localhost and cross-network agents.
- **Zero-trust security**: Per-project CAs with cross-signing, mTLS, and short-lived JWTs.
- **Shared infrastructure**: One implementation, two consumers, consistent behavior.

---

## 3. Goals

- Provide a standalone bridge daemon that supervises AI agent subprocess lifecycles.
- Expose a gRPC API for session management, command routing, and event streaming.
- Support codex, claude, and opencode providers at launch.
- Implement zero-trust security using per-project CAs with cross-signing, mTLS, and JWTs.
- Support both local and remote agent communication from day one.
- Ship a Go SDK (`bridgeclient`) for integration by consumer projects.
- Provide durable per-session pub/sub replay so SDK clients can reconnect and receive events they missed while disconnected (while bridge process is alive).
- Provide a CLI tool for CA/cert management (`bridge-ca`).
- Enable AI agents to build web and mobile applications by running sessions inside the user's login environment with access to display servers, emulators, and user credentials.
- Support concurrent human observation of and interjection into active agent sessions without stopping the agent.

---

## 4. Non-Goals

- A first-party web UI is deferred but not excluded.
- AI model routing or selection (consumers decide which provider to use).
- Acting as a CI/CD system.
- SDKs for languages other than Go (protobuf definitions available for future stub generation).

---

## 5. Users

- **prd-manager-control-plane** - Integrates via Go SDK to replace `internal/agent/Manager` with bridge-backed agent sessions.
- **ndara-ai-orchestrator** - Integrates via Go SDK to add real agent subprocess management behind its existing `AgentDaemon` gRPC service.
- **DevOps/Platform Engineers** - Deploy and operate bridge daemons on agent host machines.
- **Security Engineers** - Configure and audit the zero-trust PKI infrastructure.
- **Human Operators** - Engineers who need to observe a running agent session in real time, optionally inject a correction or question, then return control to the automated flow without restarting the session.

---

## 6. Architecture

### 6.1 Components

```
┌─────────────────────┐
│  prd-manager-       │
│  control-plane      │
│  (HTTP API)         │
└────────┬────────────┘
         │ Go SDK (bridgeclient)
         │ gRPC + mTLS + JWT
         ▼
┌────────────────────────────────────────────────────────┐
│                   AI Agent Bridge                       │
│                   (bridge daemon)                       │
│                                                         │
│  ┌──────────────┐  ┌──────────────┐  ┌──────────────┐  │
│  │ codex adapter│  │claude adapter│  │opencode adapt.│  │
│  └──────┬───────┘  └──────┬───────┘  └──────┬────────┘  │
│         │ stdio           │ stdio           │ stdio     │
│         ▼                 ▼                 ▼           │
│  ┌──────────┐      ┌──────────┐      ┌──────────┐      │
│  │ codex    │      │ claude   │      │ opencode │      │
│  │ process  │      │ process  │      │ process  │      │
│  └──────────┘      └──────────┘      └──────────┘      │
│                                                         │
│  ┌─────────────┐  ┌──────────────┐  ┌──────────────┐   │
│  │ Session     │  │ Event Ring   │  │ Provider     │   │
│  │ Supervisor  │  │ Buffer       │  │ Registry     │   │
│  └─────────────┘  └──────────────┘  └──────────────┘   │
│                                                         │
│  ┌─────────────┐  ┌──────────────┐                      │
│  │ mTLS Server │  │ JWT Verifier │                      │
│  └─────────────┘  └──────────────┘                      │
└────────────────────────────────────────────────────────┘
                                ▲
                                │ Go SDK (bridgeclient)
                                │ gRPC + mTLS + JWT
                       ┌────────┴────────────┐
                       │  ndara-ai-          │
                       │  orchestrator       │
                       │  (gRPC)             │
                       └─────────────────────┘
```

### 6.2 Operational Modes

#### User Session Server (`bridgectl server start [--listen]`)

The bridge runs as the login user inside an interactive or graphical session. This is the only deployment mode. There is no separate system daemon.

| Property | Behaviour |
|---|---|
| Binary | `cmd/bridgectl` |
| Process context | Login session of the operating user |
| Windowing access | Full — inherits `$DISPLAY`, `$WAYLAND_DISPLAY`, `$XDG_RUNTIME_DIR` |
| Credential source | Inherits the user's shell environment and native CLI auth |
| Local access | Unix socket at `~/.config/bridgectl/server.sock`, no auth |
| Remote access | TCP with auto-generated mTLS + JWT (`--listen <addr>`) |
| Startup | Systemd user service (Linux) or LaunchAgent (macOS) |
| Persistence | Optional BoltDB session store (`--db-path`) |
| Intended operator | Any user who needs to run AI agents locally or expose them remotely |

**Why the login session is required**: AI agent CLIs (claude, codex, opencode, gemini) are user-space programs that need access to home directories, auth tokens, and on graphically-capable machines, running display servers and device emulators. A system daemon running as a service account cannot provide this without recreating the user's environment, which reintroduces all the trust problems mTLS is designed to eliminate.

**Remote access model**: when `--listen` is set, the server binds to the specified TCP address and generates PKI material in `~/.config/bridgectl/certs/` on first start. SDK clients authenticate with mTLS + JWT. Human operators authenticate using OIDC via `bridgectl server issue-client --oidc` (see Security Architecture). The server must be reachable via WireGuard or Tailscale; it must not be exposed to the public internet.

Remote operator CLI acceptance criteria:
- `bridgectl session` remote subcommands allow operators to override the TLS server name when a bridge server certificate is issued for a DNS name that differs from the dial address.
- Remote credential discovery prefers the current host's client certificate before falling back to other client certificates in the cert directory.
- `bridgectl server start` auto-loads the first available per-user config from `~/.config/bridgectl/bridge.yaml` or `$XDG_CONFIG_HOME/bridgectl/config.yaml` when `--config` is omitted, so local and remote operator commands target the same configured daemon.
- Same-machine `bridgectl` commands discover an already-running secure TCP server from local state without requiring remote host, certificate, key, or JWT flags; wildcard bind addresses are recorded as loopback dial targets for local clients.
- `bridgectl session list --remote` surfaces an actionable retry hint when JWT authentication fails and a `jwt-signing.key` in the current directory may be the intended credential.
- `bridgectl client renew` handles Step CA deployments where the HTTPS serving certificate chain differs from the Step CA root used for bridge certificate issuance, while rejecting expired client certificates before attempting renewal.

**Session persistence**: with `--db-path`, the server writes session metadata and PTY output chunks to a BoltDB file. On restart, `LoadHistory()` rehydrates sessions so SDK clients can reconnect and replay events they missed.

Session shutdown acceptance criteria:
- When the user-session server receives process termination from its service manager, it stops accepting new work, gracefully terminates active provider sessions, and persists each session's terminal state before closing the session store.
- Graceful server shutdown sends each active provider session its normal termination signal and waits up to the configured provider grace period inside a bounded server shutdown deadline before using forced termination as a fallback.
- Sessions stopped during graceful server shutdown must reload after restart as terminal session history, not as daemon-orphaned failures, so clients can reconnect and replay persisted output/state.

#### Human Interjection

Human operators can observe and interact with any running session without stopping it. The session model distinguishes two roles:

**Observer** (`--observe` flag on `bridgectl session attach`): receives the live PTY output stream and the full replay buffer. Multiple observers may be attached simultaneously. Observers cannot write input to the session.

**Active Writer**: exactly one client holds the active writer slot at a time. The writer can send input via `WriteInput` and resize the terminal via `ResizeSession`. By default, the client that calls `StartSession` becomes the active writer. A human operator can claim the writer slot with `bridgectl session attach --take-over <id>`, which transitions the previous writer to observer role. Returning control is done with Ctrl-] (detach key), which releases the writer slot and restores the previous active client.

Writer transition protocol:
1. Human runs `bridgectl session attach --take-over <id>`.
   The CLI opens an observer stream and waits for the server's `ATTACHED` event before calling `ClaimWriter` with force enabled. Input and resize requests begin only after the claim succeeds; a failed claim is returned as a command error.
2. Server sends a `WRITER_CLAIMED` event to all observers including the SDK.
3. Human types, resizes, or reads.
4. Human presses Ctrl-] or `bridgectl session attach --release <id>`.
5. Server sends a `WRITER_RELEASED` event to all observers.
6. SDK may re-claim the writer slot via `ClaimWriter` RPC.

Takeover acceptance criteria:
- A CLI takeover replaces an existing writer without a spurious permission error, preserves the previous client's observer stream, and can send input to the running provider.
- Once writer access is ready, the CLI synchronizes the session PTY with the current terminal size, including a resize that occurred while attachment or claiming was pending.
- Ctrl-] releases the new writer slot and exits cleanly. Failed attachment or writer claims return a command error and must not start forwarding terminal input.
- Failed observer attachments also return a command error.
- A stream that ends before acknowledging attachment returns a command error. Failed writer startup discards queued terminal input before restoring the terminal, so keystrokes cannot be interpreted by the parent shell.

---

### 6.3 Deliverables

| Deliverable | Description |
|---|---|
| `cmd/bridgectl` | User-session bridge server and CLI |
| `cmd/bridge-ca` | CA and certificate management CLI |
| `pkg/bridgeclient` | Go SDK for consumer integration (gRPC client) |
| `pkg/bridgelib` | Embeddable Go library (no separate gRPC server) |
| Debian/Ubuntu package distribution | Signed `.deb` packages, apt repository metadata, install helper, and Ubuntu install docs |
| `proto/bridge/v1` | Protobuf service definitions |
| `internal/bridge` | Session supervisor, provider adapters, event buffer |
| `internal/auth` | mTLS, JWT, and zero-trust security primitives |
| `internal/pki` | CA management, cross-signing, cert generation |

---

## 7. Security Model: Zero-Trust PKI

### 7.1 Trust Architecture

Each project (prd-manager, ndara-orchestrator, bridgectl) operates its own Certificate Authority. Trust between projects is established through cross-signing.

```
Project A CA                    Project B CA
    │                               │
    ├── Project A server cert       ├── Project B server cert
    ├── Project A client cert       ├── Project B client cert
    │                               │
    └── Cross-signs Project B CA ◄──┘
        (and vice versa)
```

### 7.2 Certificate Hierarchy

```
bridge-ca (root)
├── Bridge Server Certificate
│   └── SAN: bridge host FQDN/IP
├── Bridge Client Certificates (one per consumer)
│   ├── prd-manager client cert
│   └── ndara-orchestrator client cert
└── Cross-signed CA certificates
    ├── prd-manager CA (cross-signed by bridge CA)
    └── ndara-orchestrator CA (cross-signed by bridge CA)
```

### 7.3 Authentication Layers

**Layer 1: mTLS (Transport)**
- All gRPC connections require mutual TLS.
- Server presents a certificate signed by its project CA.
- Client presents a certificate signed by its project CA.
- Both sides verify against the cross-signed trust bundle.
- No `InsecureSkipVerify` anywhere.
- Minimum TLS 1.3.

**Layer 2: JWT (Application)**
- Every RPC call includes a short-lived JWT in gRPC metadata (`authorization: Bearer <token>`).
- JWTs are signed with Ed25519 keys (not HS256 shared secrets).
- Claims include: `sub` (caller identity), `project_id`, `aud` (bridge), `iat`, `exp`.
- TTL: 5 minutes maximum.
- Bridge verifies signature, expiry, audience, and issuer.

**Layer 3: Authorization (Policy)**
- Per-session authorization: caller must have `project_id` claim matching the session's project.
- `repo_path` validation: must be under a configured allowed-paths list.
- Per-project and global session limits enforced.
- Provider capability checks before session start.

### 7.4 Key Management

- Ed25519 keypairs for JWT signing (one per consumer project).
- RSA 4096 or ECDSA P-384 for TLS certificates.
- CA keys stored encrypted at rest (passphrase-protected PEM).
- Certificate rotation: certs expire after 90 days; automated renewal via `bridge-ca renew`.
- Revocation: CRL distribution point served by bridge daemon.

#### Startup Client Registry

When a bridge server is configured for Step CA-backed remote access, operators may declare known client issuers in the YAML config so the server attempts to trust their JWT public keys during startup.

Acceptance criteria:

- `step_ca.clients` entries identify a JWT `issuer` and an optional `key_path` for that issuer's Ed25519 public key.
- If `key_path` is omitted, the server checks `certs/jwt-clients/<issuer>.pub` under the bridge state directory.
- Optional client public keys that are missing, unreadable, or invalid are logged and skipped so servers can start before every client has enrolled.
- Entries marked `required: true` fail startup if their public key is missing or invalid.
- Startup-loaded client keys are added to the same JWT verifier used by normal RPC authentication.
- Step CA certificate trust does not replace JWT trust; every configured client still needs a bridge JWT public key.

### 7.5 Defense in Depth

- Bridge binds to configurable interface (default: private/localhost).
- Rate limiting on session creation and command input.
- Input size limits (max command text size).
- Secret redaction in event streams and logs.
- Audit logging of all authentication and authorization decisions.
- No plaintext traffic under any configuration.

---

## 7.6 Debian/Ubuntu Distribution

The bridge must be installable on supported Ubuntu hosts through a signed apt repository so operators can install and upgrade it with standard package-management workflows instead of building from source.

### Packaging Scope

- Ship a Debian package named `bridgectl`.
- Initial supported targets:
  - Ubuntu `24.04` (`noble`) on `amd64`
  - Ubuntu `25.04` (`plucky`) on `amd64`
- Install package contents to conventional system locations:
  - `bridgectl` and `bridge-ca` binaries in `/usr/bin`
  - default config in `/etc/bridgectl/bridge.yaml`
  - systemd user unit in `/usr/lib/systemd/user/bridge.service`
- Post-install script prints instructions for `systemctl --user enable --now bridge`.
- No system user or group is created; the bridge runs as the login user.
- Provide a default packaged config that allows the server to start on a fresh host without bundled provider CLIs or API keys.
- Provider CLIs and their API credentials remain operator-managed prerequisites and must be documented separately from the package install flow.
- Provider CLIs that are expected to use native self-updaters must be installed into a user-owned runtime directory. The packaged helper defaults to `$XDG_DATA_HOME/bridgectl/providers`, or `$HOME/.local/share/bridgectl/providers` when `XDG_DATA_HOME` is unset.
- `/opt/bridgectl` is reserved for explicit root-controlled provider runtimes. Native provider self-updaters must not target this directory unless they run through a controlled privileged updater.
- `runtime.provider_root` accepts absolute paths after environment expansion for `$HOME`, `${HOME}`, `$XDG_DATA_HOME`, and `${XDG_DATA_HOME}` so per-user provider roots can be documented without hard-coding a login name.

### Publishing and Hosting

- Release automation must build `.deb` artifacts from release tags using the existing release workflow pattern.
- The apt repository must be published in a GitHub-hosted location with the full Debian repository structure (`dists/`, `pool/`, `Packages`, `Release`, `InRelease`).
- Repository metadata and packages must be signed with a GPG key stored in GitHub Actions secrets.
- Release artifacts must also be attached to the GitHub release for direct inspection/download.

### Installation Flow

- Provide an `install.sh` helper that:
  - installs the repository signing key into `/etc/apt/keyrings`
  - writes the apt source list entry
  - runs `apt-get update`
  - installs `bridgectl`
- Installation documentation must include both the helper-script path and the equivalent manual apt commands.

### Acceptance Criteria

- A release tag builds a signed `.deb` for each supported Ubuntu target.
- The published apt repository is consumable with standard `apt` commands on supported Ubuntu releases.
- A clean Ubuntu host can install `bridgectl`, start the systemd service, and pass a basic daemon health check.
- A clean Ubuntu container can run the packaged provider runtime installer as a non-root login user and install provider CLIs into the user-owned runtime directory without mutating `/opt/bridgectl`.
- The release workflow includes smoke coverage that validates apt installation in containers and on an EC2 host.
- `README.md` and `docs/` describe package installation, runtime prerequisites, and service behavior accurately.

## 7.7 External Deployment Profiles

The bridge package is deployment-neutral. Consumer platforms are responsible for authoring their own systemd drop-ins, credential injection, provisioning guides, and readiness checks. The package ships `bridge-example.yaml` as a reference config; all deployment-specific orchestration lives outside this repository.

## 7.8 Published Container Runtime

The published container image must support an environment-only startup path for
operators that already provide an external container or VM isolation boundary.
The image is responsible for making its bundled provider CLIs discoverable and
for starting the bridge with a container-appropriate default configuration when
no command-line arguments are supplied.

### Container Startup Contract

- Running the image with no command arguments starts `bridgectl server start`
  using the bundled Docker configuration.
- `BRIDGE_CONFIG` remains the override for operators that want to mount or
  generate a custom bridge configuration.
- When Docker command arguments are supplied, the entrypoint performs normal
  runtime initialization and then executes those arguments as the non-root
  `bridge` user.
- The bundled `codex`, `claude`, `opencode`, and `gemini` CLIs are exposed on
  `PATH` inside the image.
- Provider authentication may be supplied through environment variables or
  provider-native credential files. Codex accepts `OPENAI_API_KEY`,
  `CODEX_API_KEY`, `CODEX_AUTH`, `CODEX_HOME/auth.json`, or the default
  `~/.codex/auth.json`.
- Codex account-token expiry should surface as an actionable operator error
  that points to refreshing the desktop's `auth.json` instead of exposing only
  the provider's raw token expiry payload.

### Provider-Scoped Unprotected Mode

Provider sessions remain protected by default. Operators may opt a provider into
its native permissive mode by setting the provider-specific bridge environment
variable to a boolean true value:

| Provider | Environment variable | Provider behavior |
|---|---|---|
| `codex` | `BRIDGE_CODEX_UNPROTECTED` | Adds `--dangerously-bypass-approvals-and-sandbox`. |
| `claude` | `BRIDGE_CLAUDE_UNPROTECTED` | Adds `--dangerously-skip-permissions`. |
| `opencode` | `BRIDGE_OPENCODE_UNPROTECTED` | Adds `--auto`. |
| `gemini` | `BRIDGE_GEMINI_UNPROTECTED` | Adds `--yolo`. |

Boolean parsing follows Go boolean syntax. Invalid values fail closed with a
clear provider startup error. The provider-specific setting applies to both
startup probes and launched sessions so health checks reflect the runtime mode
that user sessions will use.

### Acceptance Criteria

- A container can start the bridge with only runtime environment variables and
  no mounted provider YAML.
- `codex`, `claude`, `opencode`, and `gemini` are available on `PATH` in the
  published-style image.
- Docker command arguments are honored after entrypoint initialization.
- Provider unprotected mode is opt-in, provider-scoped, defaults to protected
  behavior, and fails closed on invalid boolean values.
- Manual Docker e2e coverage starts a published-style bridge container and uses
  the Go SDK from a separate test-client container to verify:
  - Codex protected mode does not write a marker under a disposable repo's
    `.git` directory.
  - Codex unprotected mode writes the marker under `.git`.
  - Claude protected mode does not write a marker under `.git`.
  - Claude unprotected mode writes the marker under `.git`.

---

## 8. gRPC API Contract (v1)

### 8.1 Service Definition

```protobuf
syntax = "proto3";
package bridge.v1;

service BridgeService {
  // Session lifecycle
  rpc StartSession(StartSessionRequest) returns (StartSessionResponse);
  rpc StopSession(StopSessionRequest) returns (StopSessionResponse);
  rpc GetSession(GetSessionRequest) returns (GetSessionResponse);
  rpc ListSessions(ListSessionsRequest) returns (ListSessionsResponse);

  // Command routing
  rpc SendInput(SendInputRequest) returns (SendInputResponse);

  // Event streaming
  rpc StreamEvents(StreamEventsRequest) returns (stream SessionEvent);

  // Writer handoff (multi-observer model)
  rpc ClaimWriter(ClaimWriterRequest) returns (ClaimWriterResponse);
  rpc ReleaseWriter(ReleaseWriterRequest) returns (ReleaseWriterResponse);

  // Health
  rpc Health(HealthRequest) returns (HealthResponse);

  // Provider discovery
  rpc ListProviders(ListProvidersRequest) returns (ListProvidersResponse);
}
```

### 8.2 Session Lifecycle

**StartSession**
```
Request:
  project_id: string (required)
  session_id: string (required, caller-generated UUID)
  repo_path: string (required, absolute path on bridge host)
  provider: string (required: "codex" | "claude" | "opencode")
  agent_opts: map<string, string> (optional, provider-specific)

Response:
  session_id: string
  status: SessionStatus (STARTING | RUNNING | FAILED)
  created_at: google.protobuf.Timestamp
```

**StopSession**
```
Request:
  session_id: string (required)
  force: bool (optional, skip graceful shutdown)

Response:
  status: SessionStatus (STOPPING | STOPPED)
```

**GetSession**
```
Request:
  session_id: string

Response:
  session_id: string
  project_id: string
  provider: string
  status: SessionStatus
  created_at: google.protobuf.Timestamp
  stopped_at: google.protobuf.Timestamp (if applicable)
  error: string (if FAILED)
```

### 8.3 Command Routing

**SendInput**
```
Request:
  session_id: string (required)
  text: string (required, max 64KB)
  idempotency_key: string (optional)

Response:
  accepted: bool
  seq: uint64 (assigned sequence number)
```

### 8.4 Event Streaming

**StreamEvents**
```
Request:
  session_id: string (required)
  after_seq: uint64 (optional, resume from sequence)

Response (stream):
  SessionEvent:
    seq: uint64
    timestamp: google.protobuf.Timestamp
    session_id: string
    project_id: string
    provider: string
    type: EventType
    stream: string ("system" | "stdout" | "stderr")
    text: string
    done: bool
    error: string
```

**EventType enum:**
- `SESSION_STARTED`
- `SESSION_STOPPED`
- `SESSION_FAILED`
- `STDOUT`
- `STDERR`
- `INPUT_RECEIVED`
- `BUFFER_OVERFLOW`

### 8.5 Health

**Health**
```
Request: (empty)

Response:
  status: string ("serving" | "not_serving")
  providers: repeated ProviderHealth
    provider: string
    available: bool
    error: string        // reason if unavailable: missing credential, binary not found, probe failure, etc.
```

Provider availability reflects binary presence, credential availability, and daemon-startup probe results:

| Condition | `available` | `error` |
|---|---|---|
| Binary found, all required env vars set | `true` | `""` |
| Binary found, required env var missing | `false` | `"required env var CLAUDE_CODE_OAUTH_TOKEN not set"` |
| Binary not found or not executable | `false` | `"binary not found: claude"` |
| Startup probe timed out or failed (daemon only) | `false` | `"startup probe failed: ..."` |

Startup probe failures are recorded on the provider at daemon boot via `SetUnavailable` and returned by every subsequent `Health()` call until the daemon restarts. The local server (`bridgectl`) does not run startup probes, so this condition applies to the daemon only.

The daemon `status` field reflects overall daemon health, not aggregate provider health. A daemon with zero available providers still returns `"serving"` — callers must inspect per-provider entries to determine which providers can accept sessions.

---

## 9. Provider Adapter Contract

### 9.1 Interface

```go
type Provider interface {
    ID() string
    Start(ctx context.Context, cfg SessionConfig) (SessionHandle, error)
    Send(handle SessionHandle, text string) error
    Stop(handle SessionHandle) error
    Events(handle SessionHandle) <-chan Event
    Health(ctx context.Context) error
}

type SessionConfig struct {
    ProjectID  string
    SessionID  string
    RepoPath   string
    Options    map[string]string
}

type SessionHandle interface {
    ID() string
    PID() int
}
```

### 9.2 Providers at Launch

| Provider | Binary | Communication | Notes |
|---|---|---|---|
| `codex` | `codex` | stdio (stdin/stdout/stderr) | OpenAI Codex CLI |
| `claude` | `claude` | stdio (stdin/stdout/stderr) | Anthropic Claude CLI |
| `opencode` | `opencode` | stdio (stdin/stdout/stderr) | OpenCode CLI |

Each adapter:
- Spawns the provider binary as a child process.
- Pipes stdin for input, reads stdout/stderr for events.
- Monitors process health (PID alive check).
- Handles graceful shutdown (SIGTERM, then SIGKILL after timeout).
- Reports provider availability via `Health()`.

---

## 10. Event Model

### 10.1 Event Envelope

```go
type Event struct {
    Seq       uint64    // monotonic per session
    Timestamp time.Time
    ProjectID string
    SessionID string
    Provider  string
    Type      EventType
    Stream    string    // "system", "stdout", "stderr"
    Text      string
    Done      bool
    Error     string
}
```

### 10.2 Ring Buffer

- Bounded ring buffer per session (default: 10,000 events).
- When buffer is full, oldest events are dropped and a `BUFFER_OVERFLOW` event is emitted.
- `StreamEvents` with `after_seq` replays from buffer, then switches to live streaming.
- Buffer is released when session is removed (after stop + configurable retention period).

### 10.3 Durable SDK Replay Requirement

- The bridge must maintain a per-session outbound event queue that continues to enqueue agent events even when no SDK stream is connected.
- Delivery semantics are at-least-once per `(project_id, session_id, subscriber_id)`.
- The SDK must identify itself with a stable `subscriber_id` and resume using last acknowledged sequence.
- On reconnect, the bridge replays all queued events with `seq > ack_seq` in order, then switches to live tailing.
- If queued events exceed retention capacity, bridge emits `BUFFER_OVERFLOW` and resumes from the earliest retained sequence.
- Scope: guaranteed replay is required while the bridge daemon remains running and session is active; restart durability remains out of scope for this phase.

---

## 11. Go SDK (`bridgeclient`)

### 11.1 Package Structure

```
pkg/bridgeclient/
├── client.go          // Main client type
├── options.go         // Client configuration
├── session.go         // Session operations
├── events.go          // Event subscription
├── auth.go            // mTLS + JWT credential setup
└── errors.go          // Typed errors
```

### 11.2 Client API

```go
// Create a client with zero-trust credentials
client, err := bridgeclient.New(
    bridgeclient.WithTarget("bridge.example.com:9445"),
    bridgeclient.WithMTLS(bridgeclient.MTLSConfig{
        CACertPath: "certs/ca-bundle.crt",  // includes cross-signed CAs
        CertPath:   "certs/client.crt",
        KeyPath:    "certs/client.key",
    }),
    bridgeclient.WithJWT(bridgeclient.JWTConfig{
        PrivateKeyPath: "certs/jwt-signing.key",
        Issuer:         "prd-manager",
        Audience:       "bridge",
    }),
)

// Start a session
resp, err := client.StartSession(ctx, &bridge.StartSessionRequest{
    ProjectId: "my-project",
    SessionId: uuid.NewString(),
    RepoPath:  "/repos/my-project",
    Provider:  "claude",
})

// Send input
_, err = client.SendInput(ctx, &bridge.SendInputRequest{
    SessionId: sessionID,
    Text:      "review the code in main.go",
})

// Stream events (with reconnect/resume)
stream, err := client.StreamEvents(ctx, &bridge.StreamEventsRequest{
    SessionId: sessionID,
    AfterSeq:  lastSeenSeq,
})
for {
    event, err := stream.Recv()
    if err != nil { break }
    // process event
}
```

### 11.3 Built-in Resilience

- Automatic retry with exponential backoff for transient failures.
- JWT auto-renewal before expiry.
- Connection keepalive and health checking.
- Configurable timeouts per operation.
- Subscriber cursor tracking (`subscriber_id`, `ack_seq`) for reconnect + missed-event replay.

---

## 12. CLI Tool: `bridge-ca`

### 12.1 Commands

```bash
# Initialize a new CA for this project
bridge-ca init --name "bridgectl" --out certs/

# Generate server certificate for the bridge daemon
bridge-ca issue --type server --cn "bridge.example.com" \
    --san "bridge.example.com,192.168.1.10" \
    --ca certs/ca.crt --ca-key certs/ca.key \
    --out certs/bridge

# Generate client certificate for a consumer project
bridge-ca issue --type client --cn "prd-manager" \
    --ca certs/ca.crt --ca-key certs/ca.key \
    --out certs/prd-manager-client

# Cross-sign another project's CA
bridge-ca cross-sign \
    --signer-ca certs/ca.crt --signer-key certs/ca.key \
    --target-ca ../prd-manager-control-plane/certs/ca.crt \
    --out certs/prd-manager-ca-cross-signed.crt

# Build a trust bundle (own CA + all cross-signed CAs)
bridge-ca bundle \
    --ca certs/ca.crt \
    --cross-signed certs/prd-manager-ca-cross-signed.crt \
    --cross-signed certs/ndara-ca-cross-signed.crt \
    --out certs/ca-bundle.crt

# Generate Ed25519 keypair for JWT signing
bridge-ca jwt-keygen --out certs/jwt-signing

# Renew expiring certificates
bridge-ca renew --cert certs/bridge.crt --ca certs/ca.crt --ca-key certs/ca.key

# Verify a certificate chain
bridge-ca verify --cert certs/bridge.crt --bundle certs/ca-bundle.crt
```

---

## 13. Runtime/Policy Configuration

```yaml
# bridge.yaml
server:
  listen: "0.0.0.0:9445"

tls:
  ca_bundle: "certs/ca-bundle.crt"
  cert: "certs/bridge.crt"
  key: "certs/bridge.key"

auth:
  jwt_public_keys:
    - issuer: "prd-manager"
      key_path: "certs/prd-manager-jwt.pub"
    - issuer: "ndara-orchestrator"
      key_path: "certs/ndara-jwt.pub"
  jwt_audience: "bridge"
  jwt_max_ttl: "5m"

sessions:
  max_per_project: 5
  max_global: 20
  idle_timeout: "30m"
  stop_grace_period: "10s"
  event_buffer_size: 10000

input:
  max_size_bytes: 65536

providers:
  codex:
    binary: "codex"
    args: ["--quiet"]
    startup_timeout: "30s"
  claude:
    binary: "claude"
    args: ["--print", "--verbose"]
    startup_timeout: "30s"
  opencode:
    binary: "opencode"
    args: []
    startup_timeout: "30s"

allowed_paths:
  - "/home/*/repos"
  - "/opt/projects"

logging:
  level: "info"
  format: "json"
  redact_patterns:
    - "(?i)(api[_-]?key|token|secret|password)\\s*[:=]\\s*\\S+"
```

---

## 14. Integration Plans

### 14.1 prd-manager-control-plane Integration

**Current state**: `internal/agent/Manager` directly spawns agent subprocesses via `exec.CommandContext`. Events are emitted in-process via callback.

**Target state**: Replace `agent.Manager` usage in `httpapi/server.go` with `bridgeclient.Client`. The bridge client connects to a bridge daemon (local or remote) over gRPC+mTLS+JWT.

Changes required:
1. Add `pkg/bridgeclient` as a Go module dependency.
2. Add bridge configuration to server config (`bridge_target`, cert paths, JWT key path).
3. Replace `s.agentManager.Start(...)` calls with `bridgeClient.StartSession(...)`.
4. Replace `s.agentManager.Send(...)` calls with `bridgeClient.SendInput(...)`.
5. Replace `s.agentManager.Stop(...)` calls with `bridgeClient.StopSession(...)`.
6. Replace `s.agentManager.SetEventHandler(...)` with a goroutine calling `bridgeClient.StreamEvents(...)` and forwarding events to the existing `onAgentEvent` handler.
7. Add `provider` field to session-related API requests.
8. Keep existing Slack/CLI fan-out logic unchanged.

### 14.2 ndara-ai-orchestrator Integration

**Current state**: `agentd` implements the `AgentDaemon` gRPC service with stub handlers. `RunTask` logs the request but does not actually execute agent tasks. `StreamEvents` sends hardcoded demo events.

**Target state**: `agentd` uses `bridgeclient.Client` to connect to a local or remote bridge daemon. `RunTask` maps to `StartSession` + `SendInput`. `StreamEvents` maps to bridge `StreamEvents`.

Changes required:
1. Add `pkg/bridgeclient` as a Go module dependency.
2. In `agentd/main.go`, create a `bridgeclient.Client` connected to the local bridge daemon.
3. `RunTask` handler: call `bridgeClient.StartSession(...)` with the task's repo context, then `bridgeClient.SendInput(...)` with the task text.
4. `StreamEvents` handler: proxy `bridgeClient.StreamEvents(...)` to the caller, mapping bridge events to the existing `orch.v1.Event` protobuf.
5. `CancelRun` handler: call `bridgeClient.StopSession(...)`.
6. Reuse existing mTLS + JWT auth layers (the bridge has its own independent auth; `agentd` authenticates to the bridge as a separate trust boundary).

---

## 15. Success Criteria

1. Both consumer projects can start, command, and stop codex/claude/opencode sessions through the bridge.
2. Agent subprocess crashes do not affect the bridge daemon or consumer processes.
3. Event streams support reconnect/replay via sequence offsets.
4. All communication is encrypted with mTLS; all operations require valid JWTs.
5. Cross-signed CA trust enables secure communication between independently managed projects.
6. Session limits and policy guards are enforced.
7. Provider unavailability is detected and reported gracefully.
8. SDK reconnect receives all unseen queued session events in order (or explicit overflow signal if retention is exceeded).

---

## 16. Future Scope (Deferred)

- Additional providers: `gemini`, `droid`
- SDKs for Python, TypeScript (generated from protobuf)
- SPIFFE/SPIRE integration for workload identity
- Web UI for bridge status and session management
- CRL/OCSP for real-time certificate revocation
- Multi-bridge clustering and session migration
- Policy-as-code (OPA/Rego) for authorization decisions
