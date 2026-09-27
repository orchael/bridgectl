# MAR-95 bounded session observation and instructions

Bridge can request `observe_session` over the existing control WebSocket with
`request_id`, `organization_id`, `installation_id`, `session_id`,
`after_sequence`, `max_events` (1–128), and `max_bytes` (1024–32768).
The correlated `session_activity` contains a `window` with `events`,
`next_sequence`, `gap`, and `instruction_supported`, or a bounded error `code`.
The window's JSON (not the envelope) fits the requested byte limit.

Each event has `sequence`, `at`, `kind`, `summary`. Replay is from a separate
local provider-session buffer: 128 records, 32 KiB, 512 bytes per summary,
15-minute lifetime. It never depends on control connectivity. Sequence numbers
increase for the lifetime of the provider session. Readers get an explicit gap
when their cursor predates retained data. The buffer is not durable across
provider/daemon restart; recovered sessions cannot execute provider operations.
The existing Bolt journal stores PTY chunks, so it is not an eligible source.
No observation writes are added to telemetry or Bridge's database.

Codex's existing owner-connection relay supplies the events. An explicit
allowlist produces static labels for turn start/completion, assistant-message
activity, command execution, file change, tool calls and interaction status.
Arbitrary provider text, command arguments, tool output, reasoning, environment,
credentials, paths and file contents are excluded. Unknown item types and other
threads (including ephemeral threads) are ignored. This initial version is an
activity view, not a rendered transcript.

`action=instruct` extends the MAR-66 command ledger, with an empty pending ID
and a bounded UTF-8 `text`. All other command identity/expiry fields are required.
The Supervisor rejects an existing writer, recovered/stopped sessions and
unsupported providers, and holds the existing session lock during dispatch.
Codex sends `turn/start` to the existing idle owner thread, or `turn/steer` with
`expectedTurnId` to a working turn; it never starts a session or changes sandbox,
approval, model or filesystem settings. Protocol fields are verified against
`codex app-server generate-json-schema` from the installed binary.

Injected RPC IDs have a reserved random prefix and their replies are consumed
by the relay, not sent to the TUI. The operation waits for an actual RPC result.
The durable command claim is made before I/O. A timeout/disconnect produces an
unknown outcome which is never automatically resubmitted. Retry identity binds
organization, installation, session, user, action and payload through the existing
HMAC ledger. Instruction text is not stored in that ledger.

The hello advertises observation/instruction adapter capabilities. The activity
window reports whether the specific provider currently has an owning connection.
The provider still revalidates live thread state at dispatch, and pending input
or approval must use the original request-bound response operation.
