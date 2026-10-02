---
name: debug-bridgectl-cpu
description: Diagnose high CPU usage in bridgectl (server start, the installed bridgectl.service, or an attached session) — distinguish threads from processes, profile safely with the isolated dev server, and narrow a hot function to an algorithmic cause instead of guessing.
---

# Debugging bridgectl high CPU usage

Use this whenever bridgectl is reported using unexpectedly high CPU —
"server start is using all the CPU," "N bridgectl processes pinning a
core," a sluggish attached session, etc. Full background and a worked
example (three compounding O(n²) bugs found this way): `docs/docs/guides/debugging-high-cpu.md`.
Condensed rules for this same workflow also live in `AGENTS.md` under
"Debugging High CPU / Performance Issues."

## Step 0 — never touch the production/installed service

Don't run experiments against, restart, or attach pprof to an installed
production `bridgectl` service — it may be serving real sessions, possibly
including the very conversation you're having (if a terminal-attached
coding session happens to be managed by it). Everything below uses this
repo's own build with fully isolated state. If stopping or modifying the
production service is genuinely necessary, that's the user's call —
explain the consequences and ask before doing it, don't just do it.

## Step 1 — confirm what's actually running

If the report sounds like "N processes," check before assuming a
multi-instance bug:

```bash
ps -eLf | grep "server start"
```

Look at the PID column (not the LWP/thread-ID column, and not how many rows
there are) — `htop`'s thread view shows one process's OS threads as
separate rows with the identical command line. A real report of "3
bridgectl server start processes using up all the CPU" turned out to be one
process with ~10 threads.

## Step 2 — cheap syscall-level signal (optional, needs root/ptrace)

```bash
sudo timeout 2 strace -f -c -p <pid>
```

Look for syscalls firing at a suspiciously regular cadence (e.g. a poll
loop), or heavy `futex`/`sched_yield`/`SIGURG` churn — Go's
async-preemption signature, usually meaning a goroutine is doing real
CPU-bound work, not necessarily stuck. This narrows down where to look
before reaching for a profiler.

## Step 3 — build this repo's binary and start the isolated dev server

```bash
make build-cli
make dev-server-start
```

This always enables the pprof endpoint (`BRIDGECTL_PPROF_ADDR=127.0.0.1:6061`,
loopback-only) and runs under `BRIDGECTL_STATE_DIR=.dev/bridgectl` — fully
isolated from `~/.config/bridgectl` (the installed service's real state).
Check status any time with `make dev-server-status`.

## Step 4 — reproduce the load

- `make dev-session-codex DEV_REPO=/path/to/repo` or `dev-session-claude`
  runs a real agent session through the dev build.
- If the report is specific to `control`/`telemetry` wiring (only active
  once `managed_by_bridge` config is present — a much simpler, lower-CPU
  code path otherwise), run `make dev-login` first. **Never** run
  `bin/bridgectl login` directly against an isolated `BRIDGECTL_STATE_DIR`
  without this wrapper: its config-path auto-discovery falls through to
  `~/.config/bridgectl/config.yaml` — the **production** config — when
  `.dev/bridgectl/bridge.yaml` doesn't exist yet, and would silently
  rewrite its `control`/`telemetry` credential paths, breaking the
  installed service's reporting on its next reload or restart.
  `make dev-login` seeds the safe file first.
- An interactive session is awkward to drive headlessly (`--no-tty` needs
  stdin kept open across multiple inputs via a FIFO or similar, which is
  fragile to script). If you need sustained real interactive load, ask the
  user to run the session themselves in their own terminal while you watch.

## Step 5 — capture and read a profile

```bash
make dev-server-goroutines      # read this FIRST
make dev-server-cpu-profile     # 30s CPU profile, opens interactively (go tool pprof)
```

If every goroutine in the dump is blocked in a recognizable wait
(`internal/poll.runtime_pollWait`, `syscall.Read`, `pidfdWait`, a ticker's
`select`), there's no stuck/spinning goroutine — CPU is going into real
work when data arrives. Go straight to the CPU profile's `cum%`/`flat%` hot
call chain instead of hunting for a busy-loop.

For a quick non-interactive top-N view instead of the interactive pprof
shell:

```bash
go tool pprof -top -nodecount=25 bin/bridgectl "http://127.0.0.1:6061/debug/pprof/profile?seconds=20"
```

## Step 6 — narrow a hot function to an algorithmic cause

When a profile names a hot function, don't stop at "this is slow." Ask:
does its cost scale with the *total accumulated history* processed so far,
or only with the new input in this call? A function that re-scans,
re-copies, or re-validates a growing buffer from the start on every call is
O(n²) in total bytes processed, even though each individual call looks
cheap in isolation. This exact pattern caused three compounding bugs in
`internal/telemetry/live.go` and `internal/telemetry/framing.go` — see the
guide's worked example for the full story.

Confirm empirically: write a throwaway test (a `zzz_*_test.go` file is easy
to delete before committing) that calls the suspect function many times at
a few increasing sizes (e.g. 5k/20k/80k/320k) and prints elapsed time.
Total time scaling by the same factor as the size (4x size → ~4x time) is
linear; by that factor *squared* (4x size → ~16x time) is quadratic. Build
the test scenario from how the real caller actually behaves, not an
artificially extreme edge case — e.g. a single giant *incomplete* escape
sequence turned out to be a worse test than many short, *complete*
sequences with no newline, because that's what a real full-screen-redraw
TUI actually produces.

## Step 7 — verify the fix, including live

1. Fix the code; add a regression test using the realistic scenario from
   step 6.
2. Run the full test suite for the touched packages, plus `-race`.
3. Rebuild and verify live:
   ```bash
   make build-cli && make dev-server-restart
   ```
   (Not a separate `dev-server-stop` + `dev-server-start` — see the comment
   on `dev-server-restart` for the race it avoids.) Repeat steps 4-5 and
   confirm the hot path either disappears from the profile or drops to a
   proportionate, non-quadratic cost.

## Known footguns

- Don't run `bridgectl login` against an isolated state dir without
  `make dev-login` (Step 4).
- Don't stop/restart an installed production service without the user's
  explicit, informed confirmation.
- A relative Markdown link from `docs/docs/guides/*.md` to a repo-root file
  (like `README.md`) breaks the Docusaurus `docs-build` CI check — that
  docs site can't resolve links outside its own content tree. Use an
  absolute `https://github.com/<owner>/<repo>#anchor` link instead (see
  `docs/docs/getting-started/*.md` for existing examples).
