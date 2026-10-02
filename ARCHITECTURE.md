# AI Agent Bridge - Architecture

## Overview

The AI Agent Bridge is a standalone gRPC daemon and Go SDK that provides a secure, zero-trust communication layer between control-plane systems and AI agent processes. It manages AI agent subprocess lifecycles (codex, claude, opencode, gemini, and custom providers) and exposes a unified API for session management, command routing, and event streaming.

## System Context

The AI Agent Bridge sits between consumer applications and AI agent processes. Consumer applications (orchestrators, control planes, CLI tools, web services) connect via gRPC with mTLS + JWT authentication. Each consumer project gets its own CA and JWT signing key, with cross-signing enabling multi-tenant trust.

Browser-based applications cannot speak gRPC directly. The `packages/bridge-client-node` package provides a Node.js WebSocket adapter layer that translates between browser clients (using the JSON WebSocket protocol) and the gRPC daemon. A React hook (`useBridgeSession`) is included for the browser side.

```
┌──────────────────────────────────────────────────────────────────────┐
│                       Consumer Applications                          │
│   Orchestrators    Control Planes    CLI Tools    Web Services        │
│                                                                      │
│   Connect via: bridgeclient (Go SDK) or any gRPC client              │
│   Auth: mTLS (per-project CA + cross-signing) + JWT (Ed25519)        │
└──────────────────────┬───────────────────────────────────────────────┘
                       │
                       │  gRPC + mTLS + JWT
                       ▼
┌──────────────────────────────────────────────────────────────────────┐
│                      AI Agent Bridge Daemon                          │
│                      (cmd/bridge)                                    │
│                                                                      │
│  ┌────────────┐  ┌────────────────┐  ┌───────────────────────────┐  │
│  │ gRPC Server│  │ JWT Verifier   │  │ mTLS (TLS 1.3)           │  │
│  │            │  │ (multi-issuer) │  │ (RequireAndVerifyClient)  │  │
│  └─────┬──────┘  └────────────────┘  └───────────────────────────┘  │
│        │                                                             │
│  ┌─────▼──────────────────────────────────────────────────────────┐  │
│  │                    Session Supervisor                           │  │
│  │  - Start/Stop/WriteInput/Get/List                              │  │
│  │  - Policy enforcement (limits, path validation)                │  │
│  │  - PTY output forwarding to per-session ByteBuffers            │  │
│  │  - Single-client attach enforcement (Attach/Detach)            │  │
│  └─────┬──────────────────────────────────────────────────────────┘  │
│        │                                                             │
│  ┌─────▼──────────────────────────────────────────────────────────┐  │
│  │                    Provider Registry                           │  │
│  │  ┌──────────┐  ┌──────────┐  ┌──────────┐  ┌─────────────┐  │  │
│  │  │  codex   │  │  claude  │  │ opencode │  │ claude-chat │  │  │
│  │  │  (pty)   │  │  (pty)   │  │  (pty)   │  │(stream-json)│  │  │
│  │  └────┬─────┘  └────┬─────┘  └────┬─────┘  └──────┬──────┘  │  │
│  └───────┼─────────────┼─────────────┼────────────────┼──────────┘  │
│          │             │             │                │              │
│    ┌─────▼───┐   ┌─────▼───┐  ┌─────▼───┐   ┌───────▼───┐         │
│    │  codex  │   │  claude │  │opencode │   │  claude   │         │
│    │ process │   │ process │  │ process │   │  process  │         │
│    │  (pty)  │   │  (pty)  │  │  (pty)  │   │(stream-json│         │
│    └─────────┘   └─────────┘  └─────────┘   └───────────┘         │
└──────────────────────────────────────────────────────────────────────┘
```

## Component Architecture

### cmd/bridge (Daemon)

Entry point for the bridge daemon process. Responsibilities:
- Load YAML configuration
- Register provider adapters from config
- Initialize mTLS + JWT auth (optional for dev mode)
- Start gRPC server
- Graceful shutdown on SIGINT/SIGTERM

### cmd/bridge-ca (CLI Tool)

Certificate Authority management tool. Subcommands:
- `init` - Generate a new ECDSA P-384 CA
- `issue` - Issue server or client certificates
- `cross-sign` - Cross-sign external project CAs
- `bundle` - Build trust bundles
- `jwt-keygen` - Generate Ed25519 JWT signing keypairs
- `verify` - Verify certificate chains

### internal/server (gRPC Service)

Implements `bridge.v1.BridgeService`:

| RPC | Description |
|-----|-------------|
| `StartSession` | Create and start agent process via provider |
| `StopSession` | Graceful/force stop agent process |
| `GetSession` | Query session metadata and state |
| `ListSessions` | List sessions (optionally by project) |
| `SendInput` | Write text to agent stdin |
| `StreamEvents` | Replay + live stream session events |
| `Health` | Provider availability check |
| `ListProviders` | Enumerate registered providers |

**Rate limiting** (`ratelimit.go`)
- Token-bucket rate limiter keyed by arbitrary string (client ID, session ID, or "global")
- Three independent limiters per server instance: global RPS, per-client StartSession, per-session SendInput

**Input validation** (`validate.go`)
- UUID format enforcement on session/project IDs
- String length bounds on all text fields
- Applied before any business logic to return clean gRPC `INVALID_ARGUMENT` errors

### internal/bridge (Core Logic)

**Supervisor** (`supervisor.go`)
- Manages session lifecycle state machine
- Delegates to provider adapters for process management
- Enforces policy (session limits, path validation, input size)
- Forwards provider events to per-session ring buffers

**ByteBuffer** (`bytebuf.go`)
- Bounded byte-based ring buffer per session
- Monotonic sequence numbers for ordering and replay
- `Append(payload)` — add a PTY output chunk; evicts oldest chunks when byte capacity is reached
- `After(afterSeq)` — return all chunks with `Seq > afterSeq` for replay
- `OldestSeq()` / `LastSeq()` — query current buffer bounds
- Default capacity: 8 MB per session

**Single-client attach model** (`supervisor.go`)
- Only one client may be attached to a session at a time (`Attach` / `Detach`)
- `Attach(sessionID, clientID, afterSeq)` returns an `AttachState`:
  - `Replay []OutputChunk` — buffered chunks from `afterSeq` onwards
  - `Live <-chan OutputChunk` — live channel for new PTY output
  - `ReplayGap bool` — true if `afterSeq` was evicted; replay restarts from oldest retained chunk
  - `OldestSeq`, `LastSeq` — buffer extent at attach time
- Client-side cursor tracking is the SDK's responsibility (`CursorStore` in `pkg/bridgeclient`)

**Provider Interface** (`provider.go`)
- `ID() → string`
- `Start(ctx, config) → SessionHandle`
- `Stop(handle)`
- `Send(handle, text)`
- `Events(handle) → <-chan Event`
- `Health(ctx) → error`

**Event types** include standard lifecycle events plus two signalling events emitted by provider adapters:
- `EventTypeAgentReady` — agent process is initialised and ready for input
- `EventTypeResponseComplete` — agent has finished responding to the last input

**Registry** (`registry.go`)
- Register providers by ID
- Lookup and health-check all providers

**Policy** (`policy.go`)
- Per-project and global session limits
- Repo path allowlist (glob patterns)
- Input size limits

### internal/provider (Adapters)

**StdioProvider** (`stdio.go`)
- Shared subprocess adapter used by all providers
- Spawns process with `exec.CommandContext`
- Graceful shutdown: SIGTERM to process group → grace period → SIGKILL
- Environment filtering (strips sensitive variables: AWS credentials, Slack/Discord tokens, `CLAUDECODE`)
- Buffered event channel

Supports two distinct I/O modes, selected per provider config:

**PTY mode** (default) — attaches a pseudo-terminal via `creack/pty`. Used by `claude`, `codex`, `opencode`, and `gemini`. Uses a configurable `prompt_pattern` regex to detect the shell prompt:
- First prompt match → emit `AGENT_READY`
- Subsequent prompt matches after output → emit `RESPONSE_COMPLETE`

**stream-json mode** (`stream_json: true`) — parses the Claude Code CLI's `--output-format stream-json` NDJSON protocol. Extracts text from `assistant` content blocks and uses `result` events to emit `RESPONSE_COMPLETE`. `AGENT_READY` is emitted immediately on start since the process reads from stdin without a prompt. This mode is used by `claude-chat`.

Provider-specific adapters set binary name and default args:
- `codex.go` → `codex --quiet`
- `claude.go` → `claude --verbose`
- `opencode.go` → `opencode`

Additional providers (`gemini`, `claude-chat`, etc.) are configured purely via YAML without a dedicated Go file; they are instantiated dynamically from `ProviderConfig` at daemon startup.

### internal/auth (Security)

**mTLS** (`mtls.go`)
- TLS 1.3 minimum
- Server: `RequireAndVerifyClientCert`
- Client: presents cert, verifies server against CA bundle
- No `InsecureSkipVerify`

**JWT** (`jwt.go`)
- Ed25519 (EdDSA) signing — not HS256 shared secrets
- Multi-issuer verifier (one public key per consumer project)
- Max TTL enforcement (reject overly long-lived tokens)
- Claims: `sub`, `project_id`, `aud`, `iss`, `iat`, `exp`

**Interceptors** (`interceptors.go`)
- Unary + stream gRPC interceptors
- Extract Bearer token from `authorization` metadata
- Verify and inject claims into context
- Health endpoint exempted from auth

**Audit interceptors** (`audit.go`)
- Unary + stream interceptors that log every RPC outcome
- Records: method, project_id, session_id, mTLS caller CN, JWT subject, result code
- Warnings on errors, info on success

**Peer helpers** (`peer.go`)
- Extracts the mTLS client certificate Common Name from gRPC peer context

### internal/pki (Certificate Management)

- ECDSA P-384 CA generation
- Server/client certificate issuance (90-day validity)
- Cross-signing for multi-project trust
- Trust bundle assembly
- Ed25519 JWT keypair generation

### internal/redact (Log Redaction)

- Compiles a list of regex patterns from config (`logging.redact_patterns`)
- `Redact(text) → string` replaces all matches with `[REDACTED]`
- Applied to log output to prevent API keys and secrets from appearing in logs

### pkg/bridgeclient (Go SDK)

Public API for consumer integration:
- `New(opts...)` → `*Client`
- Session operations: Start, Stop, Get, List, SendInput
- Event streaming with automatic reconnect + backoff (`retry.go`)
- mTLS + auto-renewing JWT credentials
- Typed errors mapped from gRPC status codes

**CursorStore** (`cursor_store.go`) — pluggable interface for persisting the last acknowledged event sequence number per session/subscriber, enabling durable resume across process restarts:
- `MemoryCursorStore` — in-process storage (default)
- `FileCursorStore` — JSON file backed, survives process restart

### packages/bridge-client-node (Node.js / Browser)

For applications where gRPC is not available (browsers, some edge runtimes), this package provides a WebSocket adapter layer:

```
React App (Browser)
    ↕ WebSocket (JSON protocol)
Next.js / Go HTTP server   ← bridge-client-node or go-websocket-integration
    ↕ gRPC
bridgectl daemon
```

**`BridgeGrpcClient`** — Node.js gRPC client using `@grpc/grpc-js`. Loads the proto file dynamically at runtime. Exposes the same operations as the Go SDK with an async generator for `streamEvents`.

**`createBridgeWebSocketHandler`** — `ws.WebSocketServer` factory. Each WebSocket connection gets a dedicated gRPC client. Translates the JSON WebSocket protocol to gRPC calls and streams events back as JSON. Cancels in-flight streams on disconnect.

**`createNextJsBridgeRoute`** — Pages Router API route helper. Attaches the WebSocket server to the underlying Node.js HTTP server on first call (survives hot reload).

**`useBridgeSession`** — React hook using the native `WebSocket` API. Manages connection lifecycle with exponential backoff reconnect. Returns `{ startSession, sendInput, stopSession, streamEvents, events, status, error }`.

**WebSocket JSON protocol** — same protocol supported by both the Node.js package and the Go HTTP integration (`docs/go-websocket-integration.md`). All messages are JSON-encoded tagged unions with a `type` field.

See [`packages/bridge-client-node/README.md`](../packages/bridge-client-node/README.md) and [`docs/go-websocket-integration.md`](../docs/go-websocket-integration.md) for details.

## Security Architecture

### Zero-Trust Model

Each consumer project runs its own CA. The bridge cross-signs consumer CAs to build a unified trust bundle, enabling multi-tenant mTLS without sharing private keys.

```
┌─────────────┐         ┌─────────────┐         ┌─────────────┐
│ Project A   │         │   bridge    │         │ Project B   │
│     CA      │         │     CA      │         │     CA      │
└──────┬──────┘         └──────┬──────┘         └──────┬──────┘
       │                       │                       │
       │  cross-sign ◄────────►│◄────────► cross-sign  │
       │                       │                       │
       ▼                       ▼                       ▼
┌──────────────┐        ┌──────────────┐        ┌──────────────┐
│ Project A    │  mTLS  │   bridge     │  mTLS  │ Project B    │
│ client cert  │───────►│ server cert  │◄───────│ client cert  │
└──────────────┘  JWT   └──────────────┘  JWT   └──────────────┘
```

### Authentication Flow

1. **TLS Handshake**: Client presents certificate signed by its project CA. Bridge verifies against cross-signed CA trust bundle.
2. **JWT Verification**: Client sends `authorization: Bearer <token>` in gRPC metadata. Bridge verifies Ed25519 signature, audience, issuer, expiry, and max TTL.
3. **Authorization**: Bridge checks JWT `project_id` claim matches the requested session's project.

### Certificate Setup (multi-project example)

```bash
# 1. Each project initializes its own CA
bridge-ca init --name my-app --out my-app/certs/
bridge-ca init --name bridgectl --out bridge/certs/

# 2. Bridge cross-signs consumer CAs
bridge-ca cross-sign \
  --signer-ca bridge/certs/ca.crt --signer-key bridge/certs/ca.key \
  --target-ca my-app/certs/ca.crt \
  --out bridge/certs/my-app-cross.crt

# 3. Build trust bundle
bridge-ca bundle --out bridge/certs/ca-bundle.crt \
  bridge/certs/ca.crt \
  bridge/certs/my-app-cross.crt

# 4. Issue certs
bridge-ca issue --type server --cn bridge.local --san "bridge.local,127.0.0.1" \
  --ca bridge/certs/ca.crt --ca-key bridge/certs/ca.key --out bridge/certs/

bridge-ca issue --type client --cn my-app \
  --ca my-app/certs/ca.crt --ca-key my-app/certs/ca.key \
  --out my-app/certs/

# 5. Generate JWT keys
bridge-ca jwt-keygen --out my-app/certs/jwt-signing
```

## Data Flow

### Session Lifecycle

```
Consumer                    Bridge Daemon                Provider Process
   │                            │                            │
   │── StartSession ──────────►│                            │
   │                            │── exec.CommandContext ────►│
   │                            │◄── stdout/stderr (lines) ──│
   │◄── StartSessionResponse ──│                            │
   │                            │                            │
   │── SendInput ─────────────►│                            │
   │                            │── stdin.Write ────────────►│
   │◄── SendInputResponse ────│                            │
   │                            │◄── stdout (response) ─────│
   │                            │── ByteBuffer.Append ───┐   │
   │                            │                        │   │
   │── StreamEvents ──────────►│                        │   │
   │◄── replay from buffer ────│◄───────────────────────┘   │
   │◄── live events ───────────│                            │
   │                            │                            │
   │── StopSession ───────────►│                            │
   │                            │── SIGTERM ────────────────►│
   │                            │   (grace period)           │
   │                            │── SIGKILL (if needed) ───►│
   │◄── StopSessionResponse ──│                            │
```

### Event Replay + Live Streaming

```
Client calls AttachSession(session_id, client_id="sdk-1", after_seq=42):

Server calls supervisor.Attach("session-1", "sdk-1", afterSeq=42):
  Returns AttachState:
    Replay: [seq:43, seq:44]  (all chunks with Seq > 42 from ByteBuffer)
    Live:   <-chan OutputChunk (new PTY output as it arrives)
    ReplayGap: false          (seq:42 was still in the buffer)

ByteBuffer: [..., seq:40, seq:41, seq:42, seq:43, seq:44]
                                           ▲
                                  replay starts here (after_seq=42)

Server stream:
1. Send ATTACHED event (OldestSeq, LastSeq, cols, rows)
2. If ReplayGap: send REPLAY_GAP event (seq:42 was evicted; replay
   restarts from OldestSeq — client should re-render from scratch)
3. Replay: send seq:43, seq:44 as OUTPUT events (replay=true)
4. Switch to live: stream new PTY chunks as they arrive (replay=false)
5. On session exit: send SESSION_EXIT event
6. On client disconnect: supervisor.Detach() clears attached client

Cursor tracking is client-side:
  SDK saves last received seq via CursorStore (MemoryCursorStore or
  FileCursorStore). On reconnect, client passes saved seq as after_seq.
  Note: FileCursorStore is only useful within a single server lifetime
  (see issue #6 for durable session persistence roadmap).
```

## Configuration

```yaml
server:
  listen: "0.0.0.0:9445"        # gRPC listen address

tls:
  ca_bundle: "certs/ca-bundle.crt"  # Trust bundle
  cert: "certs/bridge.crt"          # Server certificate
  key: "certs/bridge.key"           # Server private key

auth:
  jwt_public_keys:                  # One per consumer project
    - issuer: "project-a"
      key_path: "certs/project-a-jwt.pub"
    - issuer: "project-b"
      key_path: "certs/project-b-jwt.pub"
  jwt_audience: "bridge"
  jwt_max_ttl: "5m"

sessions:
  max_per_project: 5
  max_global: 20
  idle_timeout: "24h"
  stop_grace_period: "10s"
  event_buffer_size: 8388608   # per-session ByteBuffer capacity in bytes (8 MB)

input:
  max_size_bytes: 65536             # Maximum SendInput text size

rate_limits:
  global_rps: 50                    # Global RPC rate limit
  global_burst: 100
  start_session_per_client_rps: 1   # Per-client StartSession throttle
  start_session_per_client_burst: 3
  send_input_per_session_rps: 5     # Per-session SendInput throttle
  send_input_per_session_burst: 20

providers:
  codex:
    binary: "codex"
    args: ["--quiet"]
    startup_timeout: "30s"
    required_env: ["OPENAI_API_KEY"]
  claude:
    binary: "node"
    args: ["./node_modules/@anthropic-ai/claude-code/cli.js", "--verbose"]
    startup_timeout: "30s"
    required_env: ["CLAUDE_CODE_OAUTH_TOKEN"]
  claude-chat:
    binary: "node"
    args: ["./node_modules/@anthropic-ai/claude-code/cli.js",
           "--dangerously-skip-permissions", "--verbose",
           "--output-format", "stream-json", "--input-format", "stream-json"]
    startup_timeout: "30s"
    required_env: ["CLAUDE_CODE_OAUTH_TOKEN"]
    stream_json: true               # Parse Claude Code NDJSON protocol
  opencode:
    binary: "opencode"
    args: []
    startup_timeout: "30s"
    required_env: ["OPENAI_API_KEY"]
    prompt_pattern: "❯"            # Regex to detect shell prompt
  gemini:
    binary: "gemini"
    args: []
    startup_timeout: "30s"
    prompt_pattern: "^\\s*>\\s*$"

allowed_paths: []                   # Repo path allowlist (glob patterns); empty = allow all

logging:
  level: "info"                     # debug | info | warn | error
  format: "json"                    # json | text
  redact_patterns:                  # Regex patterns applied to log output
    - "(?i)(api[_-]?key|token|secret|password)\\s*[:=]\\s*\\S+"
```

## Integration Points

### Consumer Application Integration

Any application can integrate with the bridge using the Go SDK (`pkg/bridgeclient`) or any gRPC client:

```go
// Create a bridge-backed client with mTLS + JWT
client, _ := bridgeclient.New(
    bridgeclient.WithTarget("bridge.local:9445"),
    bridgeclient.WithMTLS(bridgeclient.MTLSConfig{...}),
    bridgeclient.WithJWT(bridgeclient.JWTConfig{...}),
)

// Manage agent sessions remotely
client.StartSession(ctx, &bridgev1.StartSessionRequest{
    ProjectId: projectID,
    SessionId: sessionID,
    RepoPath:  repoPath,
    Provider:  "claude",
})
client.WriteInput(ctx, &bridgev1.WriteInputRequest{
    SessionId: sessionID,
    ClientId:  clientID,   // must match the client_id used in AttachSession
    Data:      []byte(prompt),
})

// Attach and stream PTY output with client-side cursor tracking
stream, _ := client.AttachSession(ctx, &bridgev1.AttachSessionRequest{
    SessionId: sessionID,
    ClientId:  "my-client",  // stable across reconnects; pass AfterSeq to resume
})
stream.RecvAll(ctx, func(ev *bridgev1.AttachSessionEvent) error {
    // Process event; cursor is tracked client-side via CursorStore
    return nil
})

client.StopSession(ctx, &bridgev1.StopSessionRequest{
    SessionId: sessionID,
})
```

## Directory Structure

```
bridgectl/
├── cmd/
│   ├── bridge/           # Daemon entry point
│   └── bridge-ca/        # CA management CLI
├── proto/bridge/v1/      # Protobuf service definitions
├── gen/bridge/v1/        # Generated Go stubs
├── pkg/bridgeclient/     # Public Go SDK
├── packages/
│   └── bridge-client-node/  # Node.js gRPC→WebSocket adapter + React hook
├── internal/
│   ├── auth/             # mTLS + JWT + audit interceptors
│   ├── pki/              # CA management
│   ├── bridge/           # Supervisor, ByteBuffer, Policy, Registry
│   ├── provider/         # Stdio/PTY/stream-json adapter + provider implementations
│   ├── redact/           # Log output redaction
│   ├── config/           # YAML config loading + env var injection
│   └── server/           # gRPC service implementation + rate limiting + validation
├── docs/                 # Integration guides (go-websocket-integration.md)
├── e2e/                  # End-to-end test harness (docker-compose)
├── examples/             # Example consumer programs (interactive PTY example)
├── config/               # Default configuration
├── certs/                # Generated certs (gitignored)
└── scripts/              # Dev setup scripts
```
