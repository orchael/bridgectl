# Scheduled Kubernetes health check to Alertmanager

This example runs a read-only cluster health check once an hour with
`codex exec` in a pinned bridgectl runtime image. The runner lists nodes, pods, and deployments
through the Kubernetes API, sends a bounded snapshot to the agent, validates
its JSON findings, and posts alerts to Alertmanager v2. Every successful run
posts an informational `BridgectlMonitorRun` alert with the cluster name,
UTC completion time, and confirmed finding count, including when the count is
zero. It never reads Secrets,
ConfigMaps, or event contents.

## Installation

The MCA demo lives in `markcallen/mca-gitops` at
`apps/bridgectl-monitor` and uses the pinned v1.4.10 bridgectl image. Argo CD
creates the namespace, read-only service account and ClusterRole, CronJob,
ConfigMap, and ExternalSecret. That ExternalSecret reads the existing MCA Bosun
Codex credential from AWS Secrets Manager; it contains no credential value in
Git. Alertmanager is at
`alertmanager-operated.observability.svc:9093` and already routes alerts to
`#feed-k8s-alerts`. The GitOps copy of `run.mjs`, `snapshot.mjs`,
`alerts.mjs`, and `prompt.md` should match this directory byte for byte;
MCA-specific settings live in its GitOps manifests.

For another cluster, either use that GitOps layout or:

1. Build and push the derived image from the repository root with
   `docker build -f examples/k8s-alertmanager/Dockerfile -t YOUR_REGISTRY/bridgectl-monitor:PINNED_TAG .`.
2. Create a namespace and a Secret named `bridgectl-monitor-provider` with a
   `CODEX_AUTH` key, or adapt the manifest to an approved credential source.
3. Create a ConfigMap named `bridgectl-monitor` with `prompt.md` from this
   directory, replace the image, namespace, and Alertmanager URL placeholders
   in `cronjob.yaml`, then apply it.
4. Run a one-off Job from the CronJob and inspect its completion, logs, and
   Alertmanager's active alerts before relying on the schedule. To send the
   run summary to Slack, route `alertname=BridgectlMonitorRun` and
   `source=bridgectl-cron` to a Slack receiver with an hourly repeat interval.

The service account needs only cluster-wide `list` on nodes, pods, and
deployments. The runner's workspace is an ephemeral `emptyDir` because this
check does not inspect a repository. Mount it under `/home/bridge` because
bridgectl restricts sessions to the agent's home by default. The agent process shares the pod's
service account token, so keep the RBAC read-only and use a trusted provider.
The pinned runtime image's `bridge` user is UID/GID 1001.

## Behavior

- Codex runs in noninteractive read-only mode without tool calls and writes its final JSON
  response to a file. The interactive `bridgectl session start --no-tty`
  path stopped on stdin EOF and stayed idle with TUI input in live Jobs;
  do not use it as a one-shot Codex runner.
- The in-container read-only shell sandbox could not start `bwrap` on MCA,
  so the agent assesses the supplied snapshot directly instead of reading a
  file through a shell tool.
- The snapshot is limited to 500 objects per kind and 32 KiB after filtering.
  Exceeding a limit fails the Job rather than silently omitting resources.
  Pods in the monitor's own namespace are excluded to avoid alerting on its
  prior failed runs; monitor failed Jobs separately.
- Alerts have stable `service`, `issue_id`, `severity`, `source`, and
  `alertname` labels. The run marker has a separate alert name so a warning
  cannot inhibit it; its time is an annotation, not a label, so each run
  refreshes one Alertmanager alert. Alerts expire after 90 minutes if not
  refreshed; a healthy run does not immediately resolve a previous finding.
- MCA Alertmanager sends the run marker through its existing Slack receiver
  to `#feed-k8s-alerts` with an hourly repeat interval. Alertmanager shows
  the latest completion time; it is not a per-run history store.
- Agent, API, parsing, and Alertmanager failures exit nonzero. Monitor failed
  Jobs separately so a failed health check is visible.
- The snapshot covers node readiness, abnormal or unready pods (including
  readiness transition times), and unavailable
  deployments. It does not cover every Kubernetes resource or prove that
  workloads are healthy beyond those signals.
