---
title: Debugging High CPU Usage
---

This guide walks through diagnosing a `bridgectl server start` process (or
any long-running bridgectl daemon) that's using more CPU than expected,
using the real investigation that found and fixed three O(n²) bugs in
`internal/telemetry` (see the worked example at the end).

## 1. Confirm what's actually running

`htop` and similar tools can show a process's OS threads as separate rows
when thread view is enabled, which looks exactly like "multiple server
processes" at a glance. Check with `ps -eLf`, which has a distinct column
for thread ID (`LWP`) vs process ID (`PID`):

```bash
ps -eLf | grep "server start"
```

If every row shares the same `PID` (fourth column differs — that's the
thread ID), you have one process with several busy threads, not several
processes. Don't chase a "duplicate process" theory until you've ruled this
out.

## 2. Get a syscall-level signal before reaching for a profiler

`strace -c` (needs root or `CAP_SYS_PTRACE`) summarizes syscalls across all
threads over a short window and is often enough to point at the right
subsystem without touching the binary:

```bash
sudo timeout 2 strace -f -c -p <pid>
```

Look for syscalls firing at a suspicious, regular cadence — e.g. `fchmodat`
at ~10/sec matching a 100ms poll loop, or heavy `futex`/`sched_yield`/`SIGURG`
churn (Go's async-preemption signature, usually meaning a goroutine is doing
a lot of CPU-bound work without yielding). This narrows down *where* to look
before you need a CPU profile.

## 3. Turn on pprof

bridgectl has an opt-in pprof endpoint, off by default so the installed
service never exposes runtime internals. Set `BRIDGECTL_PPROF_ADDR` (loopback
only, no auth) before starting the server:

```bash
BRIDGECTL_PPROF_ADDR=127.0.0.1:6061 bin/bridgectl server start --config ...
```

While the high-CPU condition is reproducing, capture a profile and a
goroutine dump:

```bash
go tool pprof -top -nodecount=25 bin/bridgectl \
  "http://127.0.0.1:6061/debug/pprof/profile?seconds=20"

curl -s "http://127.0.0.1:6061/debug/pprof/goroutine?debug=1" > goroutines.txt
```

Read the goroutine dump first — if every goroutine is blocked in a
recognizable wait (`internal/poll.runtime_pollWait`, `syscall.Read`,
`pidfdWait`, a ticker's `select`), there's no stuck/spinning goroutine, and
the CPU is going into real work when data arrives. That's a signal to look
at the CPU profile's `cum%`/`flat%` columns for the actual hot call chain,
not to keep hunting for a busy-loop.

## 4. Reproduce safely with the repo-local dev server

Never experiment against an installed/production bridgectl service — it may
be serving real sessions. Build and run this repo's binary with isolated
state instead (see ["Developing bridgectl when bridgectl is already
installed"](../../../README.md#developing-bridgectl-when-bridgectl-is-already-installed)
in the repo README for the full isolation story):

```bash
make build-cli
make dev-server-start          # isolated BRIDGECTL_STATE_DIR, pprof already wired in
make dev-session-codex DEV_REPO=/path/to/repo   # or dev-session-claude
```

`make dev-server-start` always sets `BRIDGECTL_PPROF_ADDR=127.0.0.1:6061`
(see `DEV_PPROF_ADDR` in the `Makefile`), so you get profiling for free. Once
a session is attached and reproducing the load:

```bash
make dev-server-cpu-profile     # 30s CPU profile, opens interactively
make dev-server-goroutines      # full goroutine dump
```

If you need the session to actually authenticate against your real Bridge
account (e.g. to reproduce `control`/`telemetry` wiring, which only
activates once `managed_by_bridge` config is present), log in against the
**isolated** state dir — never the production one:

```bash
BRIDGECTL_STATE_DIR="$(pwd)/.dev/bridgectl" ./bin/bridgectl login --no-browser
```

⚠️ **Before running that `login` command**, make sure
`.dev/bridgectl/bridge.yaml` exists. `bridgectl login`'s config-path
auto-discovery (`defaultServerConfigPath` in `cmd/bridgectl/server.go`)
checks, in order: `<BRIDGECTL_STATE_DIR>/bridge.yaml`, then
`$XDG_CONFIG_HOME/bridgectl/config.yaml`, then
`~/.config/bridgectl/config.yaml` — **the real installed service's config**.
If the first candidate doesn't exist yet, login falls through to the last
one and will rewrite its `control.credential_file` /
`telemetry.collector_credential_file` to point at your isolated dev
credentials, breaking the installed service's reporting on its next reload
or restart. Seed the isolated file first (a copy of
`config/bridge-repo-dev.yaml` works) so the safe candidate always wins.

## 5. Narrow a hot function to an algorithmic bug, not just "slow code"

Once a profile points at a specific function, don't stop at "this function
is slow" — check what grows the input to that function across repeated
calls. The question that matters: **does this function's cost scale with
the total history buffered so far, or only with the new data in this
call?** A function that re-scans, re-copies, or re-validates an
accumulating buffer from the start on every call is O(n²) in total bytes
processed, even if each individual call looks cheap in isolation.

A cheap way to confirm this empirically: write a throwaway test (or `go run`
scratch program) that feeds the function many small increments and prints
elapsed time at a few sizes (e.g. 5k, 20k, 80k, 320k). Total time scaling by
the same factor as the size (4x the size → ~4x the time) means it's linear;
total time scaling by that factor *squared* (4x the size → ~16x the time)
means it's quadratic — chase that down before concluding the fix worked.

## Worked example: three compounding O(n²) bugs

A production `bridgectl server start` was reported pinning a CPU core on a
2-vCPU host while a single interactive codex/claude session was attached.
`ps -eLf` showed one process with ~10 threads (not "3 processes", despite
that being the initial report — `htop`'s thread view was the cause). A live
pprof CPU profile taken during the attached session showed 100% of sampled
time inside:

```
readLoop → appendChunk → ObserveProviderChunk → observeInteraction
  → nextInteractionBoundary / ansiSequenceEnd / utf8.Valid
```

`observeInteraction` (`internal/telemetry/live.go`) buffers PTY output
until it finds a record boundary (`\n`/`\r`). A full-screen-redraw TUI
(exactly what an interactive coding agent's UI is) repositions the cursor
with ANSI escapes instead of emitting newlines, so the buffer can grow for
a long stretch with no boundary. Three independent costs in that path all
scaled with the *total accumulated buffer*, not the incoming chunk:

1. `nextInteractionBoundary` rescanned from byte 0 on every call.
2. The UTF-8 "desync" check (`utf8.Valid(pending.data)`) re-validated the
   whole buffer on every call.
3. `trimFrameBuffer` (`internal/telemetry/framing.go`) — the dominant
   cost — removed one rune at a time from an oversized buffer, re-encoding
   the entire remaining rune slice back to a string on every iteration just
   to recheck its byte length.

A first pass fixed (1) with a resume cursor and bounded (2) to a fixed
buffer size to avoid re-scanning. Code review (GitHub Copilot, on the PR)
correctly flagged that bounding (2) by size silently changed behavior once
a buffer exceeded that size — a large valid record followed by invalid
bytes should split into two telemetry records, not merge into one
`invalid_utf8` omission — and that the resume cursor for (1) didn't help a
*single* escape sequence that itself grows across many chunks (e.g. a long
CSI parameter list delivered a few bytes at a time), since the sequence
terminator search still rescanned the whole open sequence from its start
every call. Both became proper incremental state instead of a size bound:
`advanceUTF8State` tracks only the (at most 3-byte) dangling UTF-8 tail
across calls, and `ansiSequenceEndFrom` resumes the terminator search from
where it left off within an open escape sequence.

Committed regression tests cover both the steady-state case (many short,
complete chunks with no boundary — `TestLiveCollectorManyChunksWithoutBoundaryStaysLinear`,
5,000 chunks) and the single-long-open-sequence case
(`TestLiveCollectorSingleLongIncompleteANSISequenceStaysLinear`, 50,000
one-byte chunks), both completing in milliseconds. A manual (uncommitted)
scaling probe during development fed up to 320,000 chunks: pre-fix, that
pattern didn't finish within a 90-second timeout at roughly a quarter of
that size; post-fix, the full 320,000 completed in ~120ms — useful for
seeing the asymptotic difference directly, but the committed tests above
are the ones CI actually runs.

See PR history for `internal/telemetry/live.go` and
`internal/telemetry/framing.go` for the full diff and test coverage.
