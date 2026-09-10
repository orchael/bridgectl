# Examples

These examples show how to connect to an `bridgectl` server, start AI agent sessions, and interact with them. All examples auto-discover credentials from `~/.config/bridgectl/` — no flags to memorise.

## Prerequisites

Start the bridge server first:

```bash
# Local mode (auto TLS, no Step CA required)
bridgectl server start

# Or with Step CA for remote access — see docs/step-ca.md
```

For **remote** examples (`chat-ca`, `sessions --remote`, `web --remote`), you need a client certificate enrolled with the remote server.

### Obtaining a client certificate

If you have a Step CA instance running (e.g. started by `bridgectl server start --step-ca-url`), you can obtain a client certificate directly:

```bash
bridgectl client init --step-ca-url https://your-step-ca:443
```

The command will:
1. Fetch the CA root certificate (saved to `~/.config/bridgectl/certs/step-ca-root.crt`).
2. Discover available provisioners and let you choose one.
3. Issue a client certificate and key (saved to `~/.config/bridgectl/certs/<name>.crt` and `<name>.key`).

After the certificate is issued, enroll it with the remote bridge server:

```bash
bridgectl client enroll \
  --target <host>:9445 \
  --ca ~/.config/bridgectl/certs/step-ca-root.crt \
  --cert ~/.config/bridgectl/certs/<name>.crt \
  --key ~/.config/bridgectl/certs/<name>.key
```

Or combine both steps by passing `--target` to `client init`:

```bash
bridgectl client init --step-ca-url https://your-step-ca:443 --target <host>:9445
```

---

## `examples/chat` — Interactive session (local)

Start a new Claude Code session on the local bridge and attach your terminal:

```bash
go run ./examples/chat <repo-path>
# or
make chat-claude CHAT_REPO=/path/to/repo
make chat-opencode CHAT_REPO=/path/to/repo
make chat-codex CHAT_REPO=/path/to/repo
make chat-gemini CHAT_REPO=/path/to/repo
```

**Flags:**
- `--state-dir` — override state directory (default: `~/.config/bridgectl`)
- `--project` — project ID (default: `local`)
- `--provider` — provider name (default: `claude`)
- `--timeout` — session timeout (default: `30m`)

---

## `examples/chat-ca` — Interactive session (remote via Step CA)

Same as `chat` but connects to a remote bridge server. Credentials are auto-discovered from `~/.config/bridgectl/certs/`.

```bash
go run ./examples/chat-ca --remote macbook.ts.net <repo-path>
# or
make chat-ca-claude CHAT_REPO=/path/to/repo CHAT_CA_REMOTE=macbook.ts.net
make chat-ca-opencode CHAT_REPO=/path/to/repo CHAT_CA_REMOTE=macbook.ts.net
```

**Flags:**
- `--remote` — remote hostname or `host:port` (required; defaults to port 9445)
- `--state-dir` — override state directory
- `--cert`, `--key`, `--jwt-key` — manual credential overrides (optional)
- `--project`, `--provider`, `--timeout`

---

## `examples/orchestrator` — LLM-driven agent orchestration

An OpenAI-powered orchestrator that drives an AI agent on a remote machine to task completion autonomously. It starts the agent, sends a task, then watches the output — periodically asking an LLM whether the agent is done, still working, or stuck. If stuck, the orchestrator re-attaches and sends corrective input, then detaches again. It repeats until the LLM declares the task complete.

```bash
go run ./examples/orchestrator \
  --machine macbook.ts.net \
  --task "Run the test suite and fix any failing tests" \
  /home/dev/myproject
# or
make orchestrator-claude \
  ORCHESTRATOR_MACHINE=macbook.ts.net \
  ORCHESTRATOR_REPO=/home/dev/myproject \
  ORCHESTRATOR_TASK="Run the test suite and fix any failing tests"
```

**Environment:**
- `OPENAI_API_KEY` — required for the orchestrator LLM

**Flags:**
- `--machine` — remote hostname or `host:port` (required; port defaults to 9445)
- `--task` — task description to send to the agent (required)
- `--provider` — AI agent provider: `claude` | `opencode` | `codex` | `gemini` (default: `claude`)
- `--project` — bridge project ID (default: `local`)
- `--model` — OpenAI model for the orchestrator (default: `gpt-5.6`)
- `--interval` — how often to analyze buffered output (default: `15s`)
- `--timeout` — total session lifetime (default: `30m`)
- `--state-dir` — credential directory (default: `~/.config/bridgectl`)

**Orchestration loop:**

```
StartSession → WriteInput(task)
      ↓
  [ watch loop ]
  AttachSession(OBSERVER) → buffer output for --interval
      ↓
  OpenAI: analyze(task, buffered_output)
      ↓
  DONE    → StopSession → exit
  WORKING → continue watching
  STUCK   → WriteInput(corrective) → reset buffer → continue watching
```

**Files:**
- `main.go` — flags, signal handling, wiring
- `loop.go` — orchestration state machine (watch → analyze → act)
- `analyzer.go` — OpenAI analysis: classifies output as done/working/stuck
- `buffer.go` — thread-safe rolling output buffer with sequence cursor
- `client.go` — remote bridge client construction (mTLS + JWT)

---

## `examples/sessions` — List, watch, and attach to sessions

Three subcommands for managing existing sessions on local or remote servers:

```bash
# List all sessions
go run ./examples/sessions list
go run ./examples/sessions list --remote macbook.ts.net

# Watch a session (read-only output stream)
go run ./examples/sessions watch <session-id>
go run ./examples/sessions watch --remote macbook.ts.net <session-id>

# Attach to a session (claim the writer slot)
go run ./examples/sessions attach <session-id>
go run ./examples/sessions attach --remote macbook.ts.net <session-id>
go run ./examples/sessions attach --take-over <session-id>  # force writer claim
```

**Makefile shortcuts:**

```bash
make sessions-list
make sessions-list SESSIONS_REMOTE=macbook.ts.net

make sessions-watch SESSIONS_WATCH_ID=<id>
make sessions-watch SESSIONS_WATCH_ID=<id> SESSIONS_REMOTE=macbook.ts.net

make sessions-attach SESSIONS_ATTACH_ID=<id>
make sessions-attach SESSIONS_ATTACH_ID=<id> SESSIONS_REMOTE=macbook.ts.net
```

**Notes:**
- `watch` streams output to stdout as a read-only observer.
- `attach` checks for an active writer first; if one exists, it suggests `watch` or `--take-over`.
- Press `Ctrl+\` to detach from `attach` without stopping the session; `Ctrl+C` sends interrupt to the session.

---

## `examples/web` — Web UI (Go server + Vite/React frontend)

A browser-based interface for listing, starting, watching, and interacting with sessions. The Go server proxies all bridge API calls; the React frontend renders session output in an xterm.js terminal.

### Development mode (hot reload)

Run two terminals — `air` watches Go files and restarts the server on changes, while Vite handles frontend hot reload:

```bash
# Terminal 1 — Go API server (auto-reloads on Go file changes)
cd examples/web && air -- --port 8080

# Terminal 2 — Vite dev server (proxies /api → :8080)
cd examples/web/ui && pnpm install && pnpm dev
```

Then open `http://localhost:5173`.

> **Note:** [air](https://github.com/air-verse/air) is required for dev mode (`go install github.com/air-verse/air@latest`). If you prefer not to install it, use `cd examples/web/server && go run . --port 8080` instead — you'll just need to restart manually after Go changes.

### Production mode

```bash
cd examples
# Build the Vite app and Go server
cd web/ui && pnpm install && pnpm build
cd web/server && go build -o ../web .

# Serve the built UI + API from :8080
cd web && ./web --port 8080 --vite-port 0
```

Then open `http://localhost:8080`.

### Flags

```
--port       HTTP server port (default: 8080)
--vite-port  Vite dev server port to proxy to; 0 = serve built ui/dist/ (default: 5173)
```

### Connecting to a remote server

The web UI has a **Remote host** field in the header. Enter a hostname (e.g. `macbook.ts.net`) and all API calls will be routed to that remote bridge server using credentials from `~/.config/bridgectl/certs/`. Leave it empty to use the local server.

---

## Provider Notes

| Provider | Required auth |
|---|---|
| `claude` | `CLAUDE_CODE_OAUTH_TOKEN` |
| `opencode` | depends on opencode config |
| `codex` | `OPENAI_API_KEY` |
| `gemini` | OAuth2 credentials (`~/.gemini/oauth_creds.json`) — install `agy` via `curl -fsSL https://antigravity.google/cli/install.sh \| bash` |

Set secrets in your `.env` file or via `env-secrets`:

```bash
cp .env.example .env
$EDITOR .env
```
