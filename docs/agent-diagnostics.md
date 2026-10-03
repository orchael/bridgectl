# Agent diagnostic contract

bridgectl is authoritative for local agent execution and session state. This contract gives operators and AI agents a bounded, machine-readable view of that truth without exposing transcripts, credentials, environment variables, or filesystem contents.

## CLI contract

Implemented as:

```bash
bridgectl session diagnose <session-id>
bridgectl session diagnose <session-id> --json
```

The command MUST read the current Supervisor/session APIs rather than reconstructing state from telemetry or Bridge.

The JSON form is the stable integration surface. Start with `schema_version: 1` and include:

- session identity, provider and project ID
- runtime status, using the Supervisor's `SessionState` values (`starting`, `running`, `attached`, `stopping`, `stopped`, `failed`) plus the exit code once the process has exited
- interaction state (`working`, `waiting_for_input`, `waiting_for_approval`, `idle`, `unknown`) and the five interaction capability flags
- lifecycle and interaction revisions, each labelled with its source (see Revisions below)
- `created_at`; `interaction_updated_at` (`Interaction.UpdatedAt`, last change of state or pending identity) and `interaction_last_report_at` (`Interaction.LastActivityAt`, last authoritative report, even a repeat). Neither is a session-wide "last activity" time, and the names must not imply one
- pending request identity/type, a fixed-vocabulary `kind`, and `summary_available` (a boolean). Provider summary text is never emitted: Codex approval summaries embed the working directory and full command
- active-writer presence, without client secrets
- local control connection status when available: the persisted `bridge-control-status.json` state and its `updated_at`, which is the time of the last state change (or heartbeat), `last_connected_at` (recorded by the control client, `null` if never recorded), plus whether that file is older than `bridgecontrol.StatusStaleAfter`. Report `unknown` when the status path is not configured.
- the bridgectl version and diagnostic schema version

Free-form strings that the Supervisor or control client store (`SessionInfo.Error`, the control status `last_error`) and `RepoPath` are not in the list above. Include them only as a fixed-vocabulary code or after an explicit redaction decision, because they can carry provider stderr or filesystem paths.

The JSON field names, types and enum values are part of the contract once published. They are defined in a single Go type (`internal/diagnose.Report`) with golden-file tests (`internal/diagnose/testdata/`). The user-facing field reference and an example response are in `docs/docs/reference/cli.md` under `session diagnose`.

An unknown session ID is a distinct error with a non-zero exit code (for `--json`, a JSON error object with `schema_version`), not an empty snapshot. Like `bridgectl doctor`, failure exits non-zero. Error codes: `invalid_session_id`, `session_not_found`, `server_unavailable`, `internal`; the error object carries only a fixed message and never echoes server error text or caller input.

Do not include PTY output, prompts, responses, chain-of-thought, environment variables, credentials, filesystem contents, OAuth material, or arbitrary provider payloads.

## Semantics

A diagnostic snapshot answers **what bridgectl believes now**. It is not an event replay. Bridge remains an eventually consistent coordination replica and may compare its state with this snapshot.

Unknown and unsupported are explicit states. Never infer interaction state from runtime status, connectivity, terminal text, CPU usage, or elapsed time.

Revisions retain their existing meanings. Consumers may use them to identify stale replicas but MUST NOT assume lifecycle and interaction revisions share one sequence.

There are two interaction revisions today. `bridge.Interaction.Revision` is Supervisor-local and is not restart-durable. The wire revision that Bridge compares is allocated by `internal/bridgecontrol` (`RevisionStore`, persisted at `RevisionPath`), as is the session lifecycle revision, which the Supervisor does not hold at all. Report them as separate fields (for example `interaction_revision_local` and `interaction_revision_wire`, `lifecycle_revision_wire`), and `null` when the control client is not configured or has forgotten the session. Do not present a Supervisor-local value under a name Bridge also uses for its wire value.

## Verification

Tests should construct Supervisor sessions and assert that JSON diagnostics agree with the existing public session/status APIs. Runtime status and interaction state are independent axes, so cover them separately: every `SessionState` (`starting`, `running`, `attached`, `stopping`, `stopped`, `failed`) and every interaction state (`working`, `waiting_for_input`, `waiting_for_approval`, `idle`, `unknown`) where the provider supports it, including a provider with no interaction capability, which must report `unknown`.

Security tests must prove forbidden raw fields cannot enter serialized diagnostics.

## Bridge integration notes

- The daemon builds the report (`DiagnoseSession` RPC → `diagnose.Build` over `GetSession`-equivalent state plus `diagnose.LoadInputs`), so local and `--remote` callers get one implementation. `Build` is a pure function over plain data, so the control client can serve the same `Report` over its existing outbound connection (the remaining gating item for Bridge); the report JSON is already a single transport-independent document. This change adds no inbound port, no Bridge dependency, and no automatic upload.
- **Control channel.** Bridge can request this report over its existing outbound connection: it sends `diagnose_session` (`request_id`, `organization_id`, `installation_id`, `session_id`) and bridgectl replies `session_diagnostic` (`request_id` plus `report`, or a fixed `code`: `invalid_request|unsupported|not_found|unavailable`). The capability is advertised as `diagnose_session` in `hello`; requests for another organization/installation are rejected before anything is read; the report is the daemon's `server.DiagnosticReportJSON`, the same builder as the `DiagnoseSession` RPC; it is capped at 16 KiB and error text never crosses the channel. No new inbound port is opened and nothing is sent unless Bridge asks.
- Bridge should compare `interaction_revision_wire` / `lifecycle_revision_wire` with its replica; `interaction_revision_local` is for local debugging only.
- `control` reads a persisted file, so it can lag reality by up to the heartbeat interval; it is not a live reachability probe.
- `pending_request.kind` (`command|file_change|tool|question|other|unknown`) is provider-assigned from structured data (Codex request shape, Claude hook tool name) and is the safe replacement for summary text. It is local-only: it is not on the Bridge control wire.
- `pending_request.id_sha256` is the digest of the original, untruncated request id, so consumers that hold the same id (Bridge) compare fingerprints rather than the bounded `id`.
- Deferred: a fixed-vocabulary code for `SessionInfo.Error` / control `last_error`.
- There is deliberately no session-wide "last activity" time: the only authoritative timestamps are the interaction ones, and a provider activity buffer is bounded (15 minutes) and not part of `SessionInfo`.

## Follow-up

Bridge's diagnostic API/MCP should consume this contract and compare the authoritative snapshot with its registry, telemetry/analysis metadata and API representation. Production access should use the existing outbound control relationship; Bridge must not open an inbound port on developer machines.
