# Codex app-server interaction-state observer (MAR-85)

## Why this exists

`codex` run interactively (the plain `codex`/`CodexProvider`) is pure PTY: bridgectl has no structured channel into it, so it can only ever report `unknown` interaction state (see `docs/bridge-control-client.md`). Codex's real authoritative signal is its `app-server` JSON-RPC protocol (`codex app-server`), which exposes a `ThreadStatus` for every session with exactly the values this ticket needs: `idle`, `active` (with `activeFlags` including `waitingOnApproval` / `waitingOnUserInput`), `notLoaded`, `systemError`.

This is **not** a remote-control feature. It does not change who can type into a Codex session, and it does not implement Answer/Approve (MAR-66). It exists purely so bridgectl can *observe* the same authoritative status the real interactive TUI already has, without touching how a user interacts with it.

## Design: two processes, one interactive session

```
bridgectl (Supervisor)
  │
  ├── PTY child: `codex --remote ws://127.0.0.1:<port>`   ← the user's real, unmodified TUI
  │
  └── companion child: `codex app-server --listen ws://127.0.0.1:<port>`
         ▲
         └── bridgectl's own read-only WebSocket client (internal/codexapp)
             watches thread/started + thread/status/changed,
             never sends turn/start or an approval/input response
```

`codex` supports `--remote <ws://host:port | unix://path>` to connect its TUI to an *out-of-process* app-server instead of spawning an embedded one. `codex app-server --listen <URL>` runs that out-of-process server and accepts multiple clients. `internal/provider.CodexAppServerProvider.BuildCommand`:

1. Picks a free local TCP port (`net.Listen("127.0.0.1:0")`, then close — a local, single-host, small TOCTOU race, acceptable here).
2. Starts `codex app-server --listen ws://127.0.0.1:<port>` as its own `exec.CommandContext(sessionCtx, ...)` child, applying the same Codex auth resolution (`applyCodexHomeAuth`) as the plain provider.
3. Polls the port with a plain TCP dial (`waitForPort`) until the app-server is accepting connections, bounded by `portReadyTimeout` (15s) — never falls back to a plain PTY session silently; a failure here fails the session start outright.
4. Records `{port, cmd}` keyed by session ID, and arranges cleanup (`Process.Kill` + `Wait`) when `sessionCtx` is cancelled — the same lifecycle signal Supervisor already uses for every other per-session resource.
5. Returns `codex --remote ws://127.0.0.1:<port>` as the actual PTY command. From here on, Supervisor's existing PTY plumbing (attach, WriteInput, resize, replay buffer) is completely unchanged — it's driving the TUI exactly as it would drive plain `codex`.

`Supervisor.startInteractionWatcher` (a new, provider-agnostic extension point — `bridge.InteractionWatcher`) then calls `CodexAppServerProvider.WatchInteraction`, which opens a **second**, independent connection to the same app-server port and hands it to `internal/codexapp.Watch`. That package:

- Sends `initialize`.
- Adopts the first `thread/started` (or `thread/status/changed`) it sees as *the* thread for this session (a plain interactive session only ever has one).
- Observes `execCommandApproval` / `applyPatchApproval` / `item/commandExecution/requestApproval` / `item/fileChange/requestApproval` / `item/permissions/requestApproval` / `item/tool/requestUserInput` requests **only** to capture a stable ID and a short, privacy-safe summary (the command line or changed file paths — never raw output) — it never responds to them.
- Translates every `ThreadStatus` into a `bridge.Interaction`:

  | ThreadStatus | Interaction.State |
  |---|---|
  | `idle` | `idle` |
  | `active`, `activeFlags` has `waitingOnApproval` | `waiting_for_approval` (+ the last-seen approval request as `Pending`) |
  | `active`, `activeFlags` has `waitingOnUserInput` | `waiting_for_input` (+ the last-seen input request as `Pending`) |
  | `active`, no relevant flag | `working` |
  | `notLoaded` / `systemError` / anything else | `unknown` |

  A transition *away* from a waiting flag (to plain `active` or to `idle`) is itself the authoritative evidence that clears `Pending` — never inferred from bridgectl writing bytes anywhere.

## Protocol evidence (captured from the real binary, not guessed)

Verified directly against a real `codex app-server` (version 0.153.4) in this environment, with **no OpenAI/ChatGPT credentials configured** — `thread/start` and its `idle` status need none:

- **Framing**: newline-delimited JSON-RPC 2.0 over the transport (stdio by default, `ws://`/`unix://` with `--listen`). No `Content-Length` header framing.
- `initialize` request/response and an unsolicited `remoteControl/status/changed` notification that can legitimately arrive before the `initialize` response.
- `thread/start` response and the matching `thread/started` notification, including `status: {"type":"idle"}` and the full `approvalPolicy`/`sandbox` shape.
- A real 401 from `wss://api.openai.com/v1/responses` only happens once a *turn* is actually started (a real model call) — confirming `thread/start`/`idle` genuinely need no credentials, but a real approval/input request (which only happens mid-turn) does.
- `ThreadStatus`/`ThreadActiveFlag` enum values (`idle`/`active`/`notLoaded`/`systemError`, `waitingOnApproval`/`waitingOnUserInput`) — taken from the published schema (`codex app-server generate-json-schema --experimental`), not observed live (no credentials to reach a real waiting state), but the schema is generated from the same binary version, not guessed from docs.
- Multi-client `ws://` transport: a real `codex app-server --listen ws://127.0.0.1:<port>` process accepted a second, independent client connection concurrently with a first — this is `internal/provider/codex_app_server_test.go`'s real-binary integration test, and it passes in this environment.
- `unix://` transport (`--listen unix://...`) failed with `Operation not permitted` in this sandbox even with the sandbox's own protections relaxed — an environment-specific restriction, not a protocol issue. `ws://127.0.0.1` was used instead throughout; it is also more portable (no socket-file permission/cleanup concerns).

## What is verified vs. what is not

**Verified for real, in this environment**: the companion `codex app-server` process starts, becomes reachable, accepts bridgectl's observer connection, and a real `thread/start` (issued by a test standing in for the TUI) produces a `bridge.Interaction` with the correct state and `Evidence.Source == "codex-app-server"` — see `TestCodexAppServer_RealBinary_CompanionStartsAndObserverSeesIdle`.

**Not verified**: an actual `waiting_for_approval`/`waiting_for_input` transition from a real model turn, because that requires real OpenAI/ChatGPT credentials this environment does not have (`OPENAI_API_KEY` / `CODEX_API_KEY` / `CODEX_AUTH` all unset; `codex doctor` confirms no credentials found). The `ThreadStatus`/`ThreadActiveFlag` mapping for that case is grounded in the published protocol schema, not a live observation, and should be the first thing re-verified against a real authenticated Codex approval/input prompt before this is relied on in production.

## Known limitation

The observer adopts the *first* thread it sees and ignores any others. A plain interactive session only ever creates one thread, so this is correct for the scope of this ticket; a session that forks/resumes into multiple concurrent threads (not something bridgectl's current session model does) would need this generalized.
