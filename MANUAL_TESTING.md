# Manual Testing Guide

Walk through this when you want to exercise a locally compiled `bridgectl`
by hand — login, `doctor`, and the session lifecycle (`start` / `attach` /
`stop`) — against any of three targets:

- **Local** — no Bridge at all, fully standalone.
- **Production Bridge** — `https://bridge.orchael.com`.
- **Custom/dev Bridge** — any other hostname you control (a self-hosted
  instance, a staging deploy, `https://bridge.orchael.dev`, etc.).

## Why isolation matters (read this first)

`bridgectl login` and `bridgectl server start` resolve their config file by
checking `$BRIDGECTL_STATE_DIR/bridge.yaml` first, then falling back to your
real per-user config at `~/.config/bridgectl/config.yaml` (or
`$XDG_CONFIG_HOME/bridgectl/config.yaml`) if nothing exists at the state-dir
location yet. That fallback is intentional — it's what lets a real
installation keep its config even when state and config live in different
places — but it means **setting `BRIDGECTL_STATE_DIR` alone does not fully
isolate a test run**. If a real config already exists at the XDG default
(for example, this machine already runs `bridgectl` as a desktop/system
service), an isolated-looking test login will silently find and overwrite
*that* file's `control:`/`telemetry:` blocks with test credentials, and can
register a brand-new installation against your real Bridge org in the
process.

**Rule: always isolate `BRIDGECTL_STATE_DIR`, `HOME`, and `XDG_CONFIG_HOME`
together.** Never isolate one without the others — an inherited
`XDG_CONFIG_HOME` takes precedence over `HOME`, so isolating `HOME` alone is
not enough.

Session testing needs two terminals talking to the *same* isolated
environment, so use a **fixed, predictable path** per test pass rather than
`mktemp -d` (a random path can't be typed into a second terminal). Two
blocks:

**Setup — run once, in your first terminal, to start a clean test pass:**

```bash
export BRIDGECTL_TEST_DIR="${TMPDIR:-/tmp}/bridgectl-manual-test"
rm -rf "$BRIDGECTL_TEST_DIR"   # wipe any previous test pass
export BRIDGECTL_STATE_DIR="$BRIDGECTL_TEST_DIR/state"
export HOME="$BRIDGECTL_TEST_DIR/home"
export XDG_CONFIG_HOME="$BRIDGECTL_TEST_DIR/home/.config"
mkdir -p "$BRIDGECTL_STATE_DIR" "$XDG_CONFIG_HOME" "$BRIDGECTL_TEST_DIR/project"
```

An inherited `XDG_CONFIG_HOME` from your real shell would otherwise take
precedence over `$HOME` in `bridgectl`'s config resolution, letting it find
and overwrite your real config even with `$HOME` isolated — pin it into the
scratch directory explicitly, don't just rely on isolating `$HOME`.

**Join — run in every *additional* terminal for the same pass** (identical
path, no `rm -rf` — that would delete the state the first terminal is
actively using):

```bash
export BRIDGECTL_TEST_DIR="${TMPDIR:-/tmp}/bridgectl-manual-test"
export BRIDGECTL_STATE_DIR="$BRIDGECTL_TEST_DIR/state"
export HOME="$BRIDGECTL_TEST_DIR/home"
export XDG_CONFIG_HOME="$BRIDGECTL_TEST_DIR/home/.config"
```

Only re-run **Setup** (which wipes and recreates the directory) when you
want a clean slate for a new scenario; every other terminal in that same
pass just runs **Join**.

To verify isolation actually took effect before you log in, confirm there's
no accidental config waiting at the fallback location:

```bash
ls "$BRIDGECTL_STATE_DIR"/bridge.yaml 2>/dev/null && echo "already has local config (fine)"
ls "$XDG_CONFIG_HOME"/bridgectl/config.yaml 2>/dev/null && echo "UNEXPECTED: found a config here — stop and check HOME/XDG_CONFIG_HOME"
```

The second `ls` must find nothing. If it does, `$HOME`/`$XDG_CONFIG_HOME`
aren't actually pointed at your fresh scratch directory — check the exports above before
going any further.

## Build the binary

```bash
make build-cli
./bin/bridgectl --version
```

Everything below invokes `./bin/bridgectl` from the repo root. If you `cd`
elsewhere, use the absolute path (`$(pwd)/bin/bridgectl` captured before you
changed directories).

## Scenario A — Local only (no Bridge)

Standalone mode needs no login at all.

```bash
# fresh isolated env (see above), then:
./bin/bridgectl doctor
# expect:
#   server        - not configured
#   enrollment    - not logged in
```

**Session start → attach → stop**, using two terminals:

Terminal 1 (ran **Setup** above):
```bash
cd "$BRIDGECTL_TEST_DIR/project"
/path/to/repo/bin/bridgectl session start --provider echo .
# prints session output; Ctrl-] to detach without stopping
# on detach it prints: "Reattach with: bridgectl session attach <session-id>"
```

Terminal 2 — run **Join** from above first, then:
```bash
cd /path/to/repo
./bin/bridgectl session list
# note the SESSION ID column
./bin/bridgectl session attach <session-id>
# Ctrl-] to detach again
./bin/bridgectl session attach --take-over <session-id>   # forcibly reclaim the writer slot
./bin/bridgectl session watch <session-id>                # read-only observer, no input
./bin/bridgectl session stop <session-id>
./bin/bridgectl session list
# session should no longer be listed
```

`--provider echo` needs no external CLI or API key and is the fastest way
to exercise the session lifecycle. Swap in `--provider claude` or
`--provider codex` if you want to test a real provider and have the
corresponding CLI/credentials available.

Check the server itself:
```bash
./bin/bridgectl server status
./bin/bridgectl server stop
```

## Scenario B — Production Bridge (bridge.orchael.com)

Run **Setup** from above (fresh pass), then:

```bash
./bin/bridgectl login https://bridge.orchael.com --name manual-test --no-browser
```

`--no-browser` prints the authorization URL and code instead of trying to
open a browser — needed in a sandbox/SSH session with no display, and
convenient either way since you approve the device code from whatever
browser is handy. Open the printed URL, approve it, and wait for:

```
✓ Bridge authorization complete
✓ Installation registered
✓ Organization selected
✓ Telemetry configured
✓ Reporting configuration activated
✓ Bridge control connected
```

Then:
```bash
./bin/bridgectl whoami
./bin/bridgectl doctor
# expect: server, network, enrollment, organization, installation, telemetry,
# control all reporting ✓ (network depends on real connectivity)
```

Run the same **session start → attach → stop** sequence as Scenario A:
`session start --provider echo .` in this terminal, then run **Join** in a
second terminal before `session list` / `attach` / `stop` there.

Clean up when done testing against production:
```bash
./bin/bridgectl logout
./bin/bridgectl server stop
```

Re-running `login` a second time without `--force` should short-circuit:
```bash
./bin/bridgectl login https://bridge.orchael.com --no-browser
# Already logged into Bridge.
# Use --force to re-enroll.
```
(and still activates/reconnects control — confirm `doctor` reports
`control ✓` afterward).

## Scenario C — Custom/dev Bridge hostname

Same as Scenario B, pointing at whatever HTTPS origin you're testing
(a self-hosted Bridge, a staging deploy, or the `.dev` instance referenced
in `docs/docs/guides/bridge-enrollment.md`):

Run **Setup** from above (fresh pass), then:

```bash
./bin/bridgectl login https://your-custom-bridge.example --name manual-test --no-browser
# or, equivalently:
BRIDGECTL_BRIDGE_URL=https://your-custom-bridge.example ./bin/bridgectl login --no-browser
```

`--organization "Exact Org Name"` (or `BRIDGECTL_ORGANIZATION`) skips
Bridge's organization picker if you already know which org to join.

Then run the identical `whoami` / `doctor` / `session start` / `session
attach` / `session stop` / `logout` sequence from Scenario B.

## Quick reference

| Step | Local | Production/Custom Bridge |
| --- | --- | --- |
| Isolate env | `BRIDGECTL_STATE_DIR` + `HOME` + `XDG_CONFIG_HOME` | same |
| Build | `make build-cli` | same |
| Enroll | — (skip) | `bridgectl login <url> --no-browser [--name ...] [--organization ...]` |
| Check status | `bridgectl doctor` | `bridgectl whoami` + `bridgectl doctor` |
| Start a session | `bridgectl session start --provider echo .` | same |
| List sessions | `bridgectl session list` | same |
| Attach | `bridgectl session attach <id>` (`--take-over`, `--release`, or `session watch <id>` for read-only) | same |
| Stop | `bridgectl session stop <id>` (`-f` to force) | same |
| Server status/stop | `bridgectl server status` / `bridgectl server stop` | same |
| Tear down enrollment | — | `bridgectl logout` |

## Cleanup

Each scenario's state lives entirely under its own `$BRIDGECTL_TEST_DIR`:

```bash
./bin/bridgectl server stop 2>/dev/null
rm -rf "$BRIDGECTL_TEST_DIR"
```

Nothing outside that directory should ever have been touched. If `doctor`
or `login` output ever references a path outside `$BRIDGECTL_TEST_DIR`,
stop — the isolation env wasn't actually exported in that shell.
