# Crew integration

[Orchael Crew](https://github.com/orchael/crew) uses bridgectl as its provider-neutral coding-agent runtime. Crew owns deterministic workflow state, retries, and handoff policy; bridgectl continues to own provider subprocess/session lifecycle.

Crew should not launch Codex, Claude Code, OpenCode, or Gemini directly.

## Existing API surface Crew uses

The current `BridgeService` API already exposes the primitives Crew needs for the first implementation:

1. `Health` and `ListProviders` verify the daemon and requested provider are ready.
2. `StartSession` creates the coding-agent session in the run workspace.
3. `AttachSession` gives Crew replayable output plus live session events.
4. `WriteInput` sends the initial task and later CI-remediation instructions.
5. `GetSession` provides current status, exit code, sequence range, provider, and repository path.
6. `StopSession` ends a session when Crew cancels or terminates a run.

Crew should attach as `ATTACH_ROLE_WRITER` while sending a task. Other UIs and observers should attach read-only unless an explicit human takeover occurs.

## Correlation conventions

Use Crew's stable run ID everywhere possible:

| bridgectl field | Crew value |
| --- | --- |
| `project_id` | Belowdecks project/work-packet scope when available, otherwise Crew run ID |
| `session_id` | Crew run ID or `crew-<run-id>` |
| `client_id` | `crew-controller-<instance-id>` |
| `repo_path` | run-specific clean worktree path supplied by ai-desktops |
| `provider` | `codex` or `claude` for the first Crew release |

The Crew run stores the returned bridgectl session ID as an external reference. Re-running the same work packet creates a new Crew run and therefore a new coding session.

## Initial task flow

```text
Crew
  |
  | Health / ListProviders
  v
bridgectl
  |
  | StartSession(run workspace, provider)
  v
agent process
  ^
  | AttachSession(WRITER)
  | WriteInput(work packet prompt)
  |
Crew
```

Crew watches `AttachSession` until `ATTACH_EVENT_TYPE_SESSION_EXIT`, then confirms final state through `GetSession`.

A successful agent process exit does **not** mean a Crew run passed. Crew separately controls local verification, GitHub CI, acceptance verification, and evidence stages.

## CI remediation

When GitHub CI fails and Crew still has retry budget, Crew sends a compact remediation packet back to the same agent session when the provider/session can continue. If the process already exited, Crew may start a replacement session in the same run workspace while retaining the same Crew run ID.

The remediation packet should contain mechanical evidence rather than a free-form conclusion, for example:

```text
CI round 1 failed.

Job: integration
Step: go test ./...
Commit: abc123

Failure evidence:
<bounded log excerpt or evidence reference>

Repair the failure, run the repository verification command, commit, and push.
Do not merge the pull request.
```

Crew, not the agent, decides whether another CI round is allowed. The initial Crew policy permits at most two CI rounds.

## Replay and evidence

`AttachSession` sequence numbers and replay support let Crew disconnect without losing the ability to reconstruct the session output. Crew should retain bounded output/evidence references rather than copying an unlimited terminal transcript into its database.

If `ATTACH_EVENT_TYPE_REPLAY_GAP` occurs, Crew records an evidence gap and continues from the oldest available sequence. A replay gap must not silently be presented as a complete transcript.

## Human takeover

Crew should release the writer slot before a human takes over an active session. A UI can attach as an observer first and use `ClaimWriter` only after the Crew controller has stopped sending input. This prevents two writers from steering the same coding agent concurrently.

## Authentication

Crew should use the existing bridgectl mTLS/JWT enrollment model. Do not add Crew-specific API keys to the bridge protocol.

The ai-desktops environment should provide Crew with the bridge address and a service identity/reference needed to connect; it should not return raw provider secrets through the Crew API.

## What bridgectl does not own

The following stay outside bridgectl:

- Belowdecks work-packet state
- ai-desktops workspace leasing
- Ballast verification policy
- GitHub CI retry count
- Pilot acceptance verification
- evidence bundle assembly
- human PR approval
- EOS planning/prioritization

This keeps bridgectl reusable as an agent runtime rather than turning it into an SDLC orchestrator.

## MVP validation checklist

Before Crew treats the integration as production-ready, validate both `codex` and `claude` with the same sequence:

- provider reported available
- session starts in a supplied repository worktree
- task can be sent through `WriteInput`
- output survives disconnect/reconnect through replay
- process exit and exit code are observable
- Crew can stop a cancelled run
- human observer can attach without taking the writer slot

If those checks pass, no new bridgectl RPC is required for the Crew MVP.