# Request-bound remote response (MAR-66)

Bridge may send a `respond` command only for an existing pending input request.
This is not a remote terminal or a generic task-submission API. Plain PTY
providers are unsupported. Codex app-server advertises remote response only
for a captured, current, single, non-secret input question. Structured command
approvals use the separate bounded decision path described below. Multiple-question forms are left for a later
typed response interface instead of guessing where one free-form answer belongs.

## Provider evidence

Real Codex 0.153.4 testing exposed an MAR-85 gap: a second WebSocket observer
receives thread status broadcasts but not the TUI owner's pending request.
The provider now puts a loopback WebSocket relay between the unmodified TUI and
its companion app-server. It forwards the normal protocol and observes the
owning connection's request IDs and question IDs. The active thread is bound to
the TUI's own non-ephemeral start/resume/fork RPC response; startup history
reads and ephemeral background work (such as title generation) cannot select
or replace it. Only one TUI connection may
use that relay. Responses are JSON-RPC results for the exact original request,
using the provider's structured answer map; no terminal bytes or new turns are
generated. The TUI continues receiving the provider's resolution notification.

The question field in the real protocol is `question`, not `text`. Requests
from unrelated threads are rejected. If status arrives before request metadata,
the later request adds its identity without inventing a new waiting state.
Quiet waiting no longer ends merely because no notification arrives in 60s.

`WriteInput` still does not resolve interaction state. Remote response likewise
leaves the pending request intact until a later provider transition; the provider
tracks submission separately to reject duplicate answers. A stale RPC answer can
never become a new terminal command or text for another turn.

## Writer ownership

`Supervisor.RespondToInput` validates text, active runtime and exact pending
request, then requires the existing writer slot to be empty. It holds the same
session lock used by attach/claim/stop for the bounded provider write (maximum
five seconds). Existing writers receive no eviction, demotion or interruption.
Callers must release their local writer before remote response is available.
Remote failures neither stop the session nor disable direct local input.

## Command identity and replay

The outbound control client's authenticated hello supplies its installation and
organization scope. A command must match both and carry a user, session,
pending request, action, UUID command ID and deadline. The allowed actions
are `respond` and `approve`; response text is at most 16 KiB and excludes
terminal control characters. Approval carries a decision enum instead of text.

Before invoking Supervisor, the client durably creates and fsyncs a mode-0600
claim in `bridge-control-commands`. It stores only a credential-keyed HMAC and
the result, never response text. Same-ID/same-payload retries return that result;
changed payloads conflict. A pre-write crash or ambiguous write retains an
`unknown` claim and never automatically resubmits. A second command ID for the
same already-submitted provider request is rejected independently by the
provider. Recovered sessions are not controllable.

The ledger is capped at 10,000 entries and fails closed if full or unavailable.
Retain it until retiring the installation state; do not delete claims to retry
an uncertain command. Credential rotation makes old fingerprints conflict rather
than risking replay. Local operation remains available even when remote delivery
or ledger persistence fails.

## Verification

Automated tests cover local writer conflict, runtime/request validation,
cross-installation/organization commands, changed payloads, malformed/oversized
text, restart replay, uncertain outcomes and the transport-acceptance invariant.

`MAR66_LIVE=1 go test ./internal/provider -run '^TestMAR66RealTUIWaiting$' -v`
runs the real TUI, reaches waiting input, submits a structured answer, rejects
a duplicate and requires authoritative working then idle.

For the full development-browser cycle, explicitly supply the enrolled
development credential file:

```
MAR66_BRIDGE_LIVE=1 MAR66_CREDENTIAL_FILE=/path/to/bridge-credentials.json \
  go test ./internal/provider -run '^TestMAR66BridgeLive$' -v -timeout 15m
```

That test creates a real Supervisor session, releases its bootstrap writer,
publishes state through `wss://control.bridge.orchael.dev/v1/control`, and waits
for the Bridge browser to respond. It never answers on the browser's behalf.
Use Bridge's opt-in `respond.spec.ts` acceptance test on
`https://bridge.orchael.dev`. Production endpoint defaults are unchanged.

Final repeat after thread-isolation fixes passed on 2026-09-25 with session
`mar66-live-184693b1-5603-40dc-b8e9-150bd1fd4f8c` and pending request
`call_6KRaUGsUuZgTnvvoSb0iXNSi`. Real background-thread traffic was present;
the main session remained selected. The browser response/retry test passed,
and the Supervisor required provider acknowledgement, idle and one dispatch.


## Second slice: structured command approvals

`Supervisor.DecideApproval` borrows the same unowned writer slot as input
responses and rejects a local writer conflict. The command adapter accepts only
`action: "approve"` with empty text and `decision: "accept" | "cancel"`. These
mean approve once or reject and stop the turn; persistent grants are unsupported.
The durable ledger binds the decision along with all existing command identities.
Unknown delivery is never automatically retried, and accepted transport does not
clear pending state.

The Codex relay supports only complete, bounded command-execution approval
previews with a working directory. It validates owning thread, turn, item and
original JSON-RPC request identity; the public pending ID hashes all four.
Network-specific approvals, additional permission profiles, file changes and
legacy schemas remain non-actionable. `availableDecisions`, when provided, must
include both `accept` and `cancel`. The exact original RPC receives a structured
`{decision}` result; no terminal bytes or new turns are created. A local TUI
response makes remote submission ineligible before it is forwarded, while local
operation continues to use Codex's own resolution rules.

Run a real development approval cycle with `MAR66_BRIDGE_LIVE=1`,
`MAR66_APPROVAL_LIVE=1` and the existing `MAR66_CREDENTIAL_FILE`, then use the Bridge
approval Playwright test. Set `MAR66_APPROVAL_DECISION=cancel` for the rejection
cycle; cancellation may transition straight to idle. Credentials must not be
printed or committed. Unsupported approval requests remain locally actionable.

The 2026-09-26 real Bridge browser acceptance exercised both `accept` and
`cancel` with Codex 0.153.4. Sessions `mar66-live-6d5895fd-212a-4bc0-9406-f19a4d450b62`
and `mar66-live-44729ab3-85df-4a69-996d-085ebe3b3e82` respectively reached
waiting-for-approval, received the remote decision, and emitted authoritative
working/idle transitions. Both harness runs passed and asserted one dispatch
including the browser's identical-command retry. See the paired Bridge PR #18
for UI/audit evidence; bridgectl PR #254 contains the provider path.
