---
title: CLI Reference
---

The main CLI is `bridgectl`. Build it with `make build-cli`.

## Updates

Update an apt or Homebrew installation with its package manager:

```bash
bridgectl update
bridgectl update --version 1.5.0
```

The command verifies that the running binary belongs to the `bridgectl` apt
package or Homebrew cask before changing anything. Apt refreshes package lists
and installs the latest available package, or the exact requested version if
it exists in a configured repository; an explicit version may downgrade the
package. After refreshing apt metadata, it compares the installed version with
the selected repository version and skips installation when they match or the
installed version is newer.
Homebrew refreshes its tap, compares the installed cask with the tap version,
and skips installation when the selected version is already installed. For an exact
Homebrew version, bridgectl creates a temporary cask
from that release's archive and checksum, then installs it through Homebrew.
Manual and source builds are not self-updated. Finish active sessions before
updating: the package manager may unload an installed service. Restart the
service afterward. On macOS, an existing launch agent plist is updated to the
new Caskroom binary path while keeping its environment; the command prints
the `launchctl` commands to reload it after sessions finish.

To run Bridge login and doctor after updating, add an `update` section to the
same repository `.bridgectl.yaml` file used for pre-agent `setup` scripts:

```yaml
version: 1
setup:
  - echo "repository ready"
update:
  login: true
  doctor: true
  bridge: https://bridge.orchael.com
```

Both checks default to `false`, and the Bridge origin defaults to
`https://bridge.orchael.com`. Use `--config <path>` for another repository setup file,
or override individual settings with `--login`, `--doctor`, and `--bridge`.
Login uses the existing enrollment unless you explicitly re-enroll with
`bridgectl login --force`. Doctor checks the installed CLI and current local
enrollment. A failing login or doctor check makes `update` exit with an error.

Update the npm agent CLIs in the user provider runtime:

```bash
bridgectl update agents                   # Claude, Codex, OpenCode
bridgectl update agents codex             # latest published Codex
bridgectl update agents claude --version 2.1.296
bridgectl update agents all --claude-version 2.1.296 --codex-version 0.162.1
bridgectl update agents all --runtime-dir /opt/bridgectl
```

The command queries npm for exact package versions and installs them in a
sibling staging directory with `corepack pnpm add --save-exact --prod`. It
verifies selected binaries before replacing the live runtime files and restores
the previous files if the replacement fails. Concurrent updates to the same
runtime wait on a lock so neither update discards the other. When the runtime
directory is missing or empty, the command creates a minimal `package.json`
and installs the selected agents. An existing directory with other files but
no `package.json` is rejected to avoid overwriting a partial runtime. The user
running the command must be able to write there. Pause new session launches
while the runtime files are replaced.
The default directory is `runtime.provider_root` from the discovered server
config when set, otherwise `$XDG_DATA_HOME/bridgectl/providers` or
`~/.local/share/bridgectl/providers`. Configure `runtime.provider_root` if
you use a custom directory so the daemon launches the updated CLIs.
`--version` requires one agent; an all-agent update can pin individual agents
with `--claude-version`, `--codex-version`, or `--opencode-version` while the
others use npm's latest version. Gemini uses the native `agy` binary and is
outside this npm update command. Re-running the packaged
`install-provider-runtime` script restores the versions pinned in that
package's manifest.

## Run

The quickest way to start working. Auto-starts a local server if needed, creates a session, and attaches your terminal:

```bash
bridgectl run [directory]
bridgectl run --provider codex ~/repos/my-project
bridgectl run --timeout 1h .
```

| Flag | Short | Default | Description |
| --- | --- | --- | --- |
| `--provider` | `-p` | `claude` | AI provider: `claude`, `codex`, `opencode`, `gemini`, `echo`. |
| `--timeout` | `-t` | `30m` | Session timeout. |
| `--no-tty` | | `false` | Run without a terminal (for scripting and tests). |

The directory argument defaults to `.` if omitted. Press **Ctrl-]** to detach without stopping the session.

## Server Commands

```bash
bridgectl server init
bridgectl server start
bridgectl server status
bridgectl server stop
bridgectl server issue-client --name <client-name>
bridgectl server renew-cert
```

Common server flags:

| Flag | Description |
| --- | --- |
| `--listen <addr>` | Enables secure TCP mode, for example `0.0.0.0:9445` or a Tailscale IP. |
| `--san <name>` | Adds DNS names or IP addresses to the server certificate. Repeat or pass comma-separated values. |
| `--config <path>` | Loads a YAML config. If omitted, the server checks user config locations. |
| `--db-path <path>` | Enables persisted session metadata and PTY history. |
| `--step-ca-url <url>` | Uses Step CA for server certificate issuance. |
| `--step-ca-root <path>` | Root certificate for the Step CA. |
| `--step-ca-provisioner <name>` | Step CA provisioner, such as `bridge-jwk` or `acme`. |

## Client Credential Commands

```bash
bridgectl client init \
  --step-ca-url https://ca-host:9443 \
  --provisioner bridge-jwk \
  --target bridge-host:9445

bridgectl client enroll \
  --target bridge-host:9445 \
  --ca ~/.config/bridgectl/certs/step-ca-root.crt \
  --cert ~/.config/bridgectl/certs/<name>.crt \
  --key ~/.config/bridgectl/certs/<name>.key

bridgectl client renew
```

`client init` obtains an mTLS certificate. When `--target` is set, it also enrolls a JWT public key with the target bridge server.

## Session Commands

`session list` displays runtime `STATUS` and provider `INTERACTION` separately.
Claude and Codex enable structured reporting by default. See
[Session Status](../guides/session-status.md) for the states and capabilities.
The automatically configured `session report-claude-hook --config <private-file>`
helper accepts Claude hook JSON on stdin and reports only metadata to the local
session observer; it does not grant permissions or answer questions.

```bash
bridgectl session list
bridgectl session watch <session-id>
bridgectl session attach <session-id>
bridgectl session attach --take-over <session-id>
bridgectl session attach --release <session-id>
bridgectl session stop <session-id>
bridgectl session diagnose <session-id>
bridgectl session diagnose <session-id> --json
```

Only one client controls (writes to) a session at a time. If another client,
such as a Bridge web tab or another terminal, already controls it,
`session attach` exits with:

```text
Another client is controlling this session.
Watch instead:   bridgectl session attach --observe <session-id>
Take control:    bridgectl session attach --take-over <session-id>
```

(`--remote <host>` is included in both commands when you used it.) While
attached, control changes are reported in plain words:
`You now control this session.` after a successful `--take-over`,
`Another client took control. You are watching.` in the client that lost
control, `Another client now controls this session.` for watchers, and
`No client controls this session now.` when the controlling client releases it.
Client IDs are logged only at debug level.

When stdout is a terminal, `session attach` and `session watch` clear the
local screen once the server confirms the attachment, so text left by the shell
cannot mix with the redrawn session. If earlier output is no longer available
(a replay gap), the client clears the screen again and draws the server's
current-screen checkpoint, followed by live output. The notice
`[bridgectl] Earlier session output was no longer available, so the current
screen was redrawn.` is printed on stderr after the terminal is restored, not
inside the application's screen. When stdout is not a terminal, no control
bytes are written and the notice is printed immediately on stderr.

Remote session commands accept:

| Flag | Description |
| --- | --- |
| `--remote <host[:port]>` | Remote bridge host. Port defaults to `9445`. |
| `--cert <path>` | Client mTLS certificate override. |
| `--key <path>` | Client private key override. |
| `--jwt-key <path>` | JWT signing private key override. |
| `--server-name <name>` | TLS server name override when dialing by IP or alternate DNS name. |

### session diagnose

`session diagnose` reports what the local bridgectl server currently believes
about one session, for developers and AI agents debugging a mismatch with
Bridge or another replica. The daemon builds the report (`DiagnoseSession` RPC)
from its Supervisor state plus the control client's local status and revision
files. It does not read telemetry, Bridge, terminal output or logs, and it never
infers interaction state. Against a daemon that predates the RPC (for example
one not yet restarted after an upgrade), the command builds the same report
locally from `GetSession`.

- It is a **snapshot of current authoritative state, not an event replay.**
- It works the same when Bridge is unavailable or not enrolled, opens no port
  and sends nothing to Bridge. With `--remote <host>` the report describes that
  machine's daemon (its control status and revisions); `bridgectl_version` is the
  daemon's version.
- **Privacy boundary:** the output contains no PTY output, transcript, prompts,
  responses, chain-of-thought, environment variables, credentials, OAuth
  material, filesystem contents or paths, session `error` text, control
  `last_error`, or client IDs (writer presence is a boolean). The only
  provider-supplied free text is the pending-request `id` (an opaque
  identifier, length-bounded, control characters removed). Provider pending
  *summaries* are deliberately **not** included, because they can contain
  working directories, full commands and credentials; only a boolean
  `summary_available` is reported.
- Unknown stays unknown: a value bridgectl cannot report is `null` (or
  `"unknown"` for enums), never a guess.
- Failure (malformed or unknown session ID, server not running) exits non-zero.
  With `--json` stdout carries `{"schema_version":1,"error":{"code":...,"message":...}}`;
  `code` is one of `invalid_session_id`, `session_not_found`,
  `server_unavailable`, `internal`.

Example `--json` output (pretty-printed here; the real output is one line):

```json
{
  "schema_version": 1,
  "bridgectl_version": "v1.2.3",
  "session_id": "11111111-1111-4111-8111-111111111111",
  "provider": "claude",
  "project_id": "proj",
  "status": "running",
  "exit_code": null,
  "created_at": "2026-03-01T10:00:00Z",
  "stopped_at": null,
  "interaction_state": "waiting_for_approval",
  "interaction_capability": {
    "interaction_state_supported": true,
    "approval_state_supported": true,
    "pending_summary_supported": true,
    "remote_response_supported": true,
    "structured_approval_supported": true
  },
  "interaction_updated_at": "2026-03-01T10:01:00Z",
  "interaction_last_report_at": "2026-03-01T10:02:00Z",
  "pending_request": {"id": "req-1", "id_sha256": "sha256:3f2a…", "type": "approval", "kind": "command", "summary_available": true},
  "lifecycle_revision_wire": 4,
  "interaction_revision_wire": 3,
  "interaction_revision_local": 7,
  "active_writer": true,
  "observer_count": 2,
  "control": {"state": "connected", "updated_at": "2026-03-01T10:59:50Z", "last_connected_at": "2026-03-01T10:59:50Z", "stale": false}
}
```

| Field | Meaning |
| --- | --- |
| `schema_version` | Always first. Fields of a published version never change; incompatible changes bump it. |
| `bridgectl_version` | Version of the bridgectl daemon that built the report. |
| `session_id`, `provider`, `project_id` | Session identity. |
| `status` | Supervisor `SessionState`: `starting`, `running`, `attached`, `stopping`, `stopped`, `failed` (`unknown` if unreported). |
| `exit_code` | Process exit code once recorded, else `null`. |
| `created_at`, `stopped_at` | RFC 3339 UTC, or `null`. |
| `interaction_state` | `working`, `waiting_for_input`, `waiting_for_approval`, `idle`, `unknown`. Independent of `status`. |
| `interaction_capability` | The five provider capability flags. `null` = capability unknown (server reported none); all `false` = explicitly unsupported, so `interaction_state` can only be `unknown`. |
| `interaction_updated_at` | Last change of interaction state or pending-request identity. Not a session-wide "last activity" time. |
| `interaction_last_report_at` | Last authoritative provider report, including repeats. |
| `pending_request` | `null`, or `{id, id_sha256, type (input\|approval), kind, summary_available}`. `id_sha256` is `sha256:` plus the digest of the original, untruncated request ID (`id` itself is bounded and sanitized). `kind` is a provider-assigned category from a fixed vocabulary: `command`, `file_change`, `tool`, `question`, `other`, or `unknown` (provider did not classify it; any other provider value is reported as `other`). `summary_available` is true when the provider supplied a summary; its text is never included. |
| `lifecycle_revision_wire` | Lifecycle revision last allocated by the Bridge control client for this session; `null` if it has none (not enrolled, or forgotten after a terminal state). |
| `interaction_revision_wire` | The interaction revision Bridge compares; `null` likewise. |
| `interaction_revision_local` | The Supervisor's own interaction revision. Not restart-durable and a different sequence from the wire revisions; never compare them to each other. |
| `active_writer` | Whether a writer is attached (boolean only). |
| `observer_count` | Read-only observers attached. |
| `control.state` | Local control status file: `not_provisioned`, `connecting`, `connected`, `disconnected`, `auth_rejected`, `unavailable`, or `unknown` (no file, unreadable, unrecognized). |
| `control.updated_at` | When that status last changed or heartbeated. |
| `control.last_connected_at` | Last time the control client was observed connected; kept across later failures and daemon restarts. `null` if never recorded (including status files from older bridgectl versions). |
| `control.stale` | `updated_at` is older than two minutes. A stale `connected` entry means no live client is behind it. |

Without `--json`, the same fields are printed as a short sectioned report
rendered from the identical model.

## bridge-ca (Deprecated)

:::warning Deprecated
`bridge-ca` is deprecated. Use `bridgectl server start` (auto-PKI) or [Step CA integration](../security/step-ca) for all new deployments. `bridge-ca` will be removed in the next major release. See [#154](https://github.com/orchael/bridgectl/issues/154).
:::

`bridge-ca` is a standalone certificate management CLI. It was the primary way to set up PKI before `bridgectl server start` gained auto-PKI support and Step CA integration was added.

For new deployments:
- **Single machine / development**: `bridgectl server start` auto-generates a self-signed CA and server/client certificates.
- **Multi-machine / production**: Use [Step CA integration](../security/step-ca) for automated enrollment, short-lived certificates, and renewal.
- **Existing enterprise CA**: Use the [filesystem provider](../security/existing-ca) with certificates from your PKI.

The `cross-sign` and `verify` subcommands remain available during the deprecation period as they have no equivalent in the current auto-PKI or Step CA workflows.

```bash
bridge-ca init          # Initialize a new ECDSA P-384 CA
bridge-ca issue         # Issue a server or client certificate
bridge-ca cross-sign    # Cross-sign an external CA for multi-tenant trust
bridge-ca bundle        # Build a trust bundle from multiple CA certs
bridge-ca jwt-keygen    # Generate an Ed25519 keypair for JWT signing
bridge-ca verify        # Verify a certificate against a trust bundle
```
