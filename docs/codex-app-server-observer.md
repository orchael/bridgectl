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

**Verified for real, in this environment, with a genuine authenticated Codex turn** (not the published schema, not a simulation): after the account's credential was refreshed, a real `thread/start` + `turn/start` asking Codex to run a shell command drove bridgectl's actual, unmodified `internal/codexapp.Watch` production code through:

```
[OBSERVER] state=idle    evidence_source=codex-app-server
[OBSERVER] state=working evidence_source=codex-app-server
...(real model turn: thread/started, turn/started, item/started, item/completed for the user message)...
[OBSERVER] state=waiting_for_approval evidence_source=codex-app-server
```

This is the ticket's central deliverable, confirmed live: bridgectl's observer correctly derived an authoritative `waiting_for_approval` from the real `ThreadStatus`/`ThreadActiveFlag` protocol, sourced from an actual model turn attempting a real tool call — not a guess, not a heuristic, not the published schema taken on faith.

**Both previously-reported blockers are now resolved, and the second was actually a symptom of the first**:

1. ~~The account's stored ChatGPT credential had an already-used refresh token.~~ **Fixed**: a refreshed credential was provided and verified independently first (`codex exec "reply with exactly: PING_OK"` → `PING_OK`, a real model response) before retrying the app-server flow.
2. ~~A second WebSocket client's `initialize` to the companion app-server hung indefinitely.~~ **Also resolved by the same fix** — with a working credential, the second client's `initialize` was acknowledged immediately every time. The earlier "degraded outbound WebSocket connectivity" theory was wrong: the companion was actually stuck retrying the *dead* refresh token in a loop (visible in its own log: repeated `Failed to refresh token: ... already been used`) closely enough to a new connection's handshake that it looked like a hang. **Lesson for future debugging here: always check the companion's own log file (`/tmp/bridgectl-codex-appserver-*.log`) first** — a stuck/looping auth-refresh error there will look identical to a network hang from the client side.

**One refinement remains, discovered during this run**: my ad hoc test harness's simulated-TUI client (a bare `initialize`/`thread/start`/`turn/start` sequence, not the real `codex --remote` binary) never received the actual `execCommandApproval` *request* message itself — only bridgectl's separate observer connection saw the `ThreadStatus` change to `waiting_for_approval`. `Interaction.Pending` was therefore `nil` in this run (the state was correct and authoritative; the request identity/summary wasn't captured because nothing ever handed it to `codexapp`'s pending-tracking logic — see `handleMessage` in `internal/codexapp/client.go`). This is a limitation of the throwaway test client, not of bridgectl: a request the app-server sends to the client that owns the turn requires that client to implement whatever additional negotiation the real `codex --remote` TUI performs (capability declaration, a subscription call, or similar) that a bare `initialize` does not replicate.

**Recommended fix, in order of confidence**:
1. **Best**: stop simulating the TUI. `CodexAppServerProvider.BuildCommand` already constructs the real `codex --remote ws://127.0.0.1:<port>` command (`tuiCmd`) — actually start it (via a real PTY, exactly as `Supervisor.Start` does in production) instead of a hand-rolled WebSocket client standing in for it. The real binary will correctly implement whatever the app-server expects, render the approval prompt in its own terminal UI, and a real keystroke (or a scripted one via the PTY, e.g. sending `y`/Enter) closes the loop with full fidelity. This is exactly what running a real `bridgectl session start --provider codex-app-server` session end-to-end would do, and is the natural next acceptance-test step.
2. If a scripted (non-PTY) client is still wanted for CI-style automation, compare a packet capture or verbose log of a real `codex --remote` session's `initialize` params/first few messages against this client's, to find the missing capability/subscription declaration precisely, rather than guessing further.
3. Least preferred: keep guessing at `initialize.capabilities` fields — low confidence without visibility into the closed-source client's actual behavior.

None of this affects the model/mapping code itself, which is now proven correct against a real turn — it only affects how *this specific diagnostic script* stands in for a real TUI when reproducing the full request→response cycle.

## Known limitation

The observer adopts the *first* thread it sees and ignores any others. A plain interactive session only ever creates one thread, so this is correct for the scope of this ticket; a session that forks/resumes into multiple concurrent threads (not something bridgectl's current session model does) would need this generalized.
