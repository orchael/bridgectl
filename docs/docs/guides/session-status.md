---
title: Session Status for Claude and Codex
---

Claude and Codex sessions report structured interaction status by default while
keeping their native terminal interface. Reporting works locally: no hosted
dashboard, enrollment, telemetry collector, or telemetry opt-in is required.

## Start and inspect sessions

After upgrading bridgectl, restart the daemon when your sessions have finished:

```bash
bridgectl server stop
bridgectl server start
bridgectl run --provider claude ~/repos/my-project
# In another terminal:
bridgectl session list
```

Use `bridgectl run --provider codex ~/repos/my-project` for Codex. Authenticate
with the provider's native login first. Existing sessions must be restarted to
gain the new observer; attaching to an old terminal does not install it.

`session list` prints separate `STATUS` and `INTERACTION` columns. `attached`
means a terminal client is attached, and does not say whether the agent is busy.

| Interaction | Meaning |
| --- | --- |
| `working` | The provider reports active processing. |
| `waiting_for_input` | A structured question or input request is pending. |
| `waiting_for_approval` | A permission decision is pending. |
| `idle` | The current turn has finished, or a session is ready for a new prompt. |
| `unknown` | No usable structured signal has arrived, or the signal is unavailable. |

A completed response that includes a question in prose is still `idle` unless
the provider raises a structured request. Output text, silence, question marks,
and telemetry's inferred questions do not set this status.

## Claude: local lifecycle hooks

The `claude` provider supplies a temporary `--settings` file containing command
hooks. Each calls the installed daemon executable's
`bridgectl session report-claude-hook --config <private-file>` helper. This command
reads one Claude hook JSON object from stdin, removes content fields, and sends
the remaining metadata to an authenticated loopback receiver for that session.
It is installed automatically; you normally do not invoke it yourself.

| Claude signal | Reported behavior |
| --- | --- |
| `SessionStart` | Ready/idle; binds the native session identity. Resume and clear reset prior state. |
| `UserPromptSubmit`, ordinary `PreToolUse` | Working. |
| `PreToolUse` for `AskUserQuestion` | Waiting for input with a stable request ID. |
| `PermissionRequest` | Waiting for approval (input for `AskUserQuestion`). |
| `PostToolUse`, `PostToolUseFailure` | Clears the matching request and reports working unless another request is pending. |
| `Elicitation`, `ElicitationResult` | Raises and resolves MCP input requests. |
| `Stop`, `Notification: idle_prompt` | Idle after a completed turn; never fabricates a pending question. |
| `StopFailure`, `SessionEnd` | Unknown interaction; process exit independently updates runtime status. |

Subagent completions cannot clear a parent's pending request. Native tool IDs
correlate tool events; the preceding tool event supplies the identity omitted by
Claude's `PermissionRequest` hook. A per-session keyed fingerprint of structured
tool arguments distinguishes concurrent calls of the same tool; argument values
never leave the helper. Pending IDs exposed to clients are opaque.

The helper never reads transcripts or returns a permission decision. Questions
and approvals are answered in the attached Claude terminal. Remote response and
structured approval capabilities remain false for this observer.

User/project hooks remain in place. An existing CLI `--settings` file or JSON
object is merged into the temporary file, including its hooks. The source file
is never modified. Temporary files use private permissions and are removed when
the session ends. The helper uses a one-second HTTP timeout and no HTTP proxy or
redirects; observation cannot wait indefinitely on a remote service.

Use the repository-pinned Claude Code version (see `package.json`) or a compatible
version supporting these [hook events](https://code.claude.com/docs/en/hooks).
The generated command uses Claude's default POSIX shell (Git Bash on Windows).
An administrator disabling hooks, a user setting `disableAllHooks`, or an
incompatible CLI can prevent signals; bridgectl does not override that policy.
Permission resolution is observed at tool completion, so an approved long-running
tool can retain its waiting status until `PostToolUse` arrives. Sandboxed network
permission dialogs that emit only a notification are not tracked as identified
requests. These hooks do not provide a complete permission-response protocol.

## Codex: observe the owning app-server connection

The `codex` provider starts a loopback `codex app-server` companion and connects
the native TUI through a local protocol relay using `codex --remote`. The relay
observes the same structured status and request identities as the TUI. The
`codex-app-server` provider name remains a compatible alias.

| Codex signal | Interaction |
| --- | --- |
| Thread `active` | Working. |
| `waitingOnUserInput` | Waiting for input. |
| `waitingOnApproval` | Waiting for approval. |
| Thread `idle` | Idle. |
| `notLoaded`, `systemError`, unrecognized status | Unknown. |

Start/resume/fork responses establish the owned thread; unrelated threads do not
change the session. Provider notifications clear pending requests. A local input
write alone is never proof that a request was resolved.

Use a Codex CLI supporting `app-server --listen` and `--remote` (the pinned version
is in `package.json`). Configured binaries, a Node launcher script, working
directory, model/terminal arguments, and native authentication are preserved.
Global config and feature overrides are passed to the companion as well.
Loss of the observed connection resets interaction to unknown and removes remote
response capability. Companion startup failure fails the session explicitly instead of silently
falling back to an unobserved terminal. Both child processes stop with the session.
See the [Codex app-server protocol](https://developers.openai.com/codex/app-server/)
for the upstream event contract.

## Configuration and compatibility mode

Auto-detection and explicit provider configurations both enable structured
reporting for `claude` and `codex`:

```yaml
providers:
  claude:
    binary: claude
    args: ["--verbose"]
  codex:
    binary: codex
    args: ["--no-alt-screen"]
```

For an older CLI or a manually managed external app server, explicitly opt out:

```yaml
providers:
  codex:
    binary: codex
    transport: stdio
```

`transport: stdio` also works for Claude. It preserves the plain terminal
behavior and reports `unknown` with no interaction capability. There is no silent
downgrade. Custom provider IDs and `stream_json: true` retain their existing
behavior rather than being assumed to speak the Claude/Codex terminal protocol.

## Consume status from any client

`GetSession` and `ListSessions` expose `SessionInteraction` in the public protobuf
and Go SDK. No dashboard-specific API is needed:

```go
session, err := client.GetSession(ctx, &bridgev1.GetSessionRequest{SessionId: id})
if err != nil {
    return err
}
interaction := session.GetInteraction()
fmt.Println(interaction.GetState(), interaction.GetSource())
if request := interaction.GetPendingRequest(); request != nil {
    fmt.Println(request.GetId(), request.GetType())
}
```

The response includes capability flags, revision, transition time, last activity
time, and an optional pending request. Repeated evidence preserves transition
time; a different pending request changes the revision even if the state stays
the same. Older daemons omit `interaction`; clients should display `unknown`.
Runtime `stopped`/`failed` and disconnected clients should take precedence over
last-known interaction status in a UI. Attaching/detaching does not clear a wait.

An optional configured control-plane exporter forwards this same provider-neutral
state. Its endpoint and credentials are configured separately; the provider
adapters contain no service URL or account identity.

## Troubleshooting and verification

- Restart the daemon after installing the new binary, then start a new session.
- Check `claude --version` or `codex --version` against the pinned versions.
- If Claude stays unknown, inspect `/hooks` for `report-claude-hook`, check whether
  hooks are disabled, and verify that the daemon executable still exists and is
  executable. Do not print/share the private observer configuration or token.
- If local `session list` is correct but a dashboard is stale, troubleshoot the
  configured export connection; provider reporting itself is working.
- Question/answer telemetry remains an independent analytics feature. Enabling
  it does not enable or repair structured reporting.

Deterministic tests cover lifecycle, request correlation, privacy, authentication,
settings preservation, and default selection:

```bash
go test -race ./internal/claudehooks ./internal/provider ./internal/localserver ./internal/server
```

An optional authenticated Claude terminal acceptance test asks a fixed color
question and checks waiting → working → idle:

```bash
make build-cli
BRIDGECTL_HOOK_TEST_BINARY="$PWD/bin/bridgectl" \
  go test ./internal/claudehooks -run TestClaudeRealTUIQuestion -v -count=1
```
