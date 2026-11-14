# Session Resource Supervisor

## Summary

Long-lived Bridge sessions must not imply long-lived CPU entitlement.

The session retention timeout may be 24 hours or longer, while bridgectl independently
manages the resources consumed by the agent process tree. Each agent session is placed
in its own Linux cgroup v2. Idle sessions can be frozen without losing process memory
or terminal state, and hard machine-level limits prevent abandoned agents from starving
the host.

Bridge owns policy and desired state. bridgectl owns local enforcement. Local enforcement
continues when the Bridge control plane is unavailable.

## Supported platforms

| Platform | Resource supervision |
|---|---|
| Linux with cgroup v2 | Native, fully supported |
| Windows | Run bridgectl and agents inside WSL2. Native Windows processes are not supported by the cgroup supervisor. |
| macOS | No cgroup v2. Session lifecycle remains supported, but cgroup freeze/limits require a future platform-specific implementation. |

Windows documentation and installers must make the WSL2 requirement explicit whenever
resource-supervised sessions are enabled.

## Minimum supported machine

The supervisor must work on a **2 vCPU / 8 GiB RAM** host.

Defaults must be derived from detected capacity rather than assuming a large AI desktop.
For the minimum machine:

- reserve at least 1.5 GiB RAM for the OS, bridgectl, and supporting services;
- make no more than 6 GiB available to the aggregate agent pool by default;
- preserve enough CPU capacity for bridgectl and the OS to remain responsive;
- allow active agents to burst when capacity is available;
- do not statically divide CPU or memory equally between sessions;
- reject or queue new work when it cannot start without violating the reserve;
- prefer hibernating eligible idle sessions before refusing new work.

The exact reserve should remain configurable. Larger hosts should scale from detected
capacity while retaining a host reserve.

## Session lifecycle

```text
ACTIVE
  |
  | no user/tool/terminal activity and low CPU
  v
IDLE
  |
  | idle threshold reached
  v
HIBERNATED
  |
  | input / explicit resume
  +------------------------> ACTIVE
  |
  | retention expires
  v
TERMINATED
```

Recommended initial defaults:

- IDLE eligibility: 5 minutes without meaningful activity;
- HIBERNATE eligibility: 15 minutes idle;
- session retention: 24 hours;
- never freeze while a tool call or tracked child process is actively consuming CPU.

Retention and hibernation are independent settings.

## Activity detection

Do not use wall-clock inactivity alone. Track:

- last user input;
- last agent output;
- PTY activity;
- provider/tool activity where available;
- CPU usage for the full process tree;
- child process activity.

A session is hibernation-eligible only when it is idle and its process tree has remained
below the configured CPU threshold for the settling window.

## cgroup layout

On Linux/WSL2 create a hierarchy owned by the bridgectl user service:

```text
bridgectl.slice/
  agent-pool/
    <session-id>/
```

Each session cgroup contains the agent and all descendants. Use cgroup v2 controls:

- `cgroup.freeze` for hibernate/resume;
- `cpu.weight` and, where necessary, `cpu.max`;
- `memory.high` for pressure/reclaim signalling;
- `memory.max` as a final per-session safety boundary;
- `pids.max` to prevent runaway process creation;
- `memory.events`, `cpu.stat`, and PSI where available for pressure decisions.

Do not use SIGSTOP/SIGCONT as the primary mechanism because the cgroup freezer applies
atomically to the whole process tree.

## Machine-level admission control

Before starting a session, the local supervisor evaluates current host and agent-pool
capacity.

```text
start requested
      |
capacity available? ---- yes ---> start ACTIVE
      |
      no
      v
hibernate eligible idle sessions
      |
capacity available? ---- yes ---> start ACTIVE
      |
      no
      v
QUEUED / RESOURCE_CONSTRAINED
```

On a 2 vCPU / 8 GiB machine, one expensive active agent may legitimately consume most
of the agent pool. A second or third session may exist but should be idle, hibernated,
or queued. Session count is not a capacity guarantee.

## Memory pressure policy

Freezing eliminates CPU consumption but retains memory. The supervisor therefore reacts
to host pressure independently:

1. lower priority of idle sessions;
2. freeze eligible idle sessions;
3. allow the kernel to reclaim clean pages from frozen sessions;
4. when the configured critical-memory threshold is crossed, terminate the oldest
   hibernated session(s), after persisting the terminal/session history;
5. refuse new sessions if the host reserve still cannot be protected.

Initial implementation should not use CRIU. Checkpointing arbitrary agent CLIs, PTYs,
MCP connections, sockets, browsers, and subprocesses adds substantial restore complexity.

## Bridge-visible states

Expose enough local state for Bridge to display:

- RUNNING
- WAITING
- IDLE
- HIBERNATED
- QUEUED
- RESOURCE_CONSTRAINED
- FINISHED

Resource metadata should include CPU, resident memory, hibernation timestamp, and the
reason for a resource transition.

A message sent to a hibernated session must first thaw its cgroup, wait for the process
tree to become runnable, then deliver input. Resume should be transparent to clients.

## Configuration

Add a resource-supervisor configuration block. Names are illustrative until wired into
the existing config model:

```yaml
resources:
  enabled: true
  host_memory_reserve: 1536Mi
  agent_pool_memory_max: auto
  idle_after: 5m
  hibernate_after: 15m
  retention: 24h
  idle_cpu_percent: 2
  idle_cpu_window: 2m
  critical_memory_available_percent: 10
  pressure_memory_available_percent: 20
  pids_max_per_session: 512
```

`auto` must respect the host reserve and work on the 2 vCPU / 8 GiB minimum host.

## Failure behaviour

- Loss of Bridge connectivity does not disable local resource enforcement.
- Failure to create or manage cgroups must be visible and fail safely rather than
  silently pretending limits are active.
- A bridgectl restart reconciles existing managed cgroups where possible.
- Hibernated sessions remain subject to the retention policy.
- Resource telemetry must not require the hosted Bridge service.

## Implementation phases

### Phase 1: Linux/WSL2 supervisor

1. Introduce a platform-neutral resource supervisor interface.
2. Implement the Linux cgroup v2 backend.
3. Put every newly spawned session process tree in a session cgroup.
4. Collect CPU, memory, PID, and pressure metrics.
5. Implement freeze/thaw and lifecycle transitions.
6. Add machine-level admission control and host reserve enforcement.
7. Reconcile cgroups after daemon restart.

### Phase 2: Bridge integration

1. Extend session status/resource metadata exposed by the public API.
2. Report HIBERNATED, QUEUED, and RESOURCE_CONSTRAINED states.
3. Resume a hibernated session automatically before accepting input.
4. Add explicit resume/hibernate/terminate operations where useful.

### Phase 3: operator UX

1. Show resource state in `bridgectl session list`.
2. Explain why a session is queued or hibernated.
3. Document tuning for small and large hosts.
4. Document WSL2 setup for Windows.

## Required tests

Unit and integration tests must cover:

- cgroup creation and cleanup per session;
- descendants remain in the session cgroup;
- freeze and thaw of a process tree;
- activity prevents premature hibernation;
- idle sessions transition to HIBERNATED;
- input resumes a hibernated session before delivery;
- a runaway session cannot consume beyond configured safety limits;
- admission control protects the host reserve;
- oldest hibernated sessions are selected first under critical memory pressure;
- Bridge disconnection does not disable local enforcement;
- restart reconciliation;
- unsupported/native-Windows behaviour is explicit;
- WSL2 is detected as a Linux cgroup environment;
- a constrained CI/smoke scenario modelling **2 vCPU and 8 GiB RAM** can run multiple
  sessions without exhausting the configured host reserve.

## Acceptance criteria

The feature is complete when a 2 vCPU / 8 GiB Linux or WSL2 host can retain multiple
sessions for 24 hours while abandoned sessions consume no CPU after hibernation, active
sessions cannot violate the configured machine reserve, new work is queued instead of
starving the host, and any hibernated session can transparently resume when the user
interacts with it.
