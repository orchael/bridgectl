# Scheduled Kubernetes health check to Alertmanager

This example runs a read-only cluster health check every 30 minutes with a
non-TTY bridgectl Codex session. The runner lists nodes, pods, and deployments
through the Kubernetes API, gives a bounded snapshot to the agent, validates
its JSON findings, and posts alerts to Alertmanager v2. It never reads Secrets,
ConfigMaps, or event contents.

## Installation

The MCA demo lives in `markcallen/mca-gitops` at
`apps/bridgectl-monitor` and uses the pinned v1.4.10 bridgectl image. Argo CD
creates the namespace, read-only service account and ClusterRole, CronJob,
ConfigMap, and ExternalSecret. That ExternalSecret reads the existing MCA Bosun
Codex credential from AWS Secrets Manager; it contains no credential value in
Git. Alertmanager is at
`alertmanager-operated.observability.svc:9093` and already routes alerts to
`#feed-k8s-alerts`.

For another cluster, either use that GitOps layout or:

1. Build and push the derived image from the repository root with
   `docker build -f examples/k8s-alertmanager/Dockerfile -t YOUR_REGISTRY/bridgectl-monitor:PINNED_TAG .`.
2. Create a namespace and a Secret named `bridgectl-monitor-provider` with a
   `CODEX_AUTH` key, or adapt the manifest to an approved credential source.
3. Create a ConfigMap named `bridgectl-monitor` with `prompt.md` from this
   directory, replace the image, namespace, and Alertmanager URL placeholders
   in `cronjob.yaml`, then apply it.
4. Run a one-off Job from the CronJob and inspect its completion, logs, and
   Alertmanager's active alerts before relying on the schedule.

The service account needs only cluster-wide `list` on nodes, pods, and
deployments. The runner's workspace is an ephemeral `emptyDir` because this
check does not inspect a repository. The agent process shares the pod's
service account token, so keep the RBAC read-only and use a trusted provider.

## Behavior

- A complete fenced JSON response is required before the runner closes
  bridgectl stdin. Closing stdin earlier stops an unfinished agent turn.
- The snapshot is limited to 500 objects per kind and 32 KiB after filtering.
  Exceeding a limit fails the Job rather than silently omitting resources.
- Alerts have stable `service`, `issue_id`, `severity`, `source`, and
  `alertname` labels. They expire after 90 minutes if not refreshed; a
  healthy run does not immediately resolve a previous alert.
- Agent, API, parsing, and Alertmanager failures exit nonzero. Monitor failed
  Jobs separately so a failed health check is visible.
- The snapshot covers node readiness, abnormal pods, and unavailable
  deployments. It does not cover every Kubernetes resource or prove that
  workloads are healthy beyond those signals.
