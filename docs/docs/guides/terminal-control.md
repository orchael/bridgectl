---
title: Terminal Watch and Attach
---

Native PTY sessions, including Claude and Codex, can be watched without sending
input and attached when an operator needs to type. Watch is read only; an
attached writer can send keyboard bytes and resize the PTY. Only one writer can
own a session at a time. Structured interaction status is independent of these
terminal roles and does not change just because someone starts watching.

## CLI and browser clients

Use `bridgectl session watch <session-id>` to observe and
`bridgectl session attach <session-id>` to interact. Release the existing writer
before attaching from another client. The [web example](web-ui.md) demonstrates
xterm.js output rendering and observer/writer roles over the public gRPC API.
The example's Go HTTP server implements the browser transport; xterm.js runs
only in the browser.

## Outbound control transport

The optional outbound control adapter also supports terminal access. Its endpoint
and credential come from configuration/enrollment, with no hard-coded service
address in the terminal implementation. Updated daemons advertise
`terminal_session: true` in hello capabilities. Restart the daemon after updating
the binary so the capability is advertised.

A compatible authenticated gateway sends `terminal_request` and receives
`terminal_result` on the existing connection. Requests contain `request_id`,
`organization_id`, `installation_id`, `session_id`, a user/tab-scoped `client_id`,
`action`, `after_sequence` and `expires_at` (at most 30 seconds in the future).

| Action | Behavior |
| --- | --- |
| `watch` | Read retained output after the cursor; never claim a writer or resize. |
| `attach` | Acquire the existing single-writer slot, without forced takeover. |
| `input` | Write base64 `data` to the PTY; requires this client's writer lease. |
| `resize` | Set `cols` and `rows`; requires this client's writer lease. |
| `detach` | Release this client's writer lease; other writers are unaffected. |

Successful results contain a `window` with base64 `data`, `next_sequence`,
`gap`, `ended`, `writer`, `cols` and `rows`. Feed decoded bytes directly to the
terminal emulator, then advance the cursor. Serialize requests to preserve input
and output order. Do not automatically retry uncertain input. A `gap` means
retained output expired: reset the emulator and display the available history.
The window contains up to 64 KiB; input is limited to 16 KiB per request.
Resize accepts 2–500 columns and 1–300 rows. Stream-JSON providers do not have a
native PTY and are unavailable through this terminal path.

An optional `code` reports errors such as `writer_conflict`, `not_attached`,
`unsupported`, `invalid_request`, `busy`, `session_ended` or `unavailable`.
Treat failures as read-only until the operator explicitly attaches again.

An attached client sets `keep_writer: true` on watch refreshes to renew its lease;
ordinary watch requests release any lease held by that same client. Gateways
must authorize lease renewal with the same permission as attach/input.
Writer leases expire after 15 seconds without renewal and are released
when the control connection closes. At most 64 leases are held per connection.
Gateways must authorize every operation, bind client IDs to authenticated users
and tabs, prevent replay, and check current organization/installation membership.
Only explicit, authorized attach requests should enable browser input; watching
must never forward xterm-generated input or resize the remote PTY.

Terminal access intentionally sends real output, including any prompts, tool
output or other text visible in the session. The adapter keeps no extra transcript
store; output comes from the daemon's existing bounded replay buffer. Configure
gateway retention and access policies appropriately for its users. The structured
status feed remains separate and does not include this terminal output.
