# Kubernetes scheduled agent findings to Alertmanager

This example runs a non-TTY bridgectl agent from a Kubernetes CronJob, validates a structured JSON response, and POSTs confirmed findings to Alertmanager's v2 API. Alertmanager routes them to Slack using your existing receiver configuration.

## Setup

1. Build a pinned image from the repository root: `docker build -f examples/k8s-alertmanager/Dockerfile -t YOUR_REGISTRY/bridgectl-monitor:PINNED_TAG .` and push it.
2. Create the `bridgectl-monitor-provider` Kubernetes Secret containing `OPENAI_API_KEY` (or adapt the manifest for Claude).
3. Supply an application checkout through a read-only PVC at `/workspace`. Adapt the prompt to your application and make sure the agent can access the required diagnostics.
4. Replace the image and PVC placeholders in `cronjob.yaml`, then `kubectl apply -f examples/k8s-alertmanager/cronjob.yaml`.
5. Configure Alertmanager to route alerts with `source=bridgectl-cron` to an existing Slack receiver. Keep Slack credentials in Alertmanager, not in the CronJob.

## Behavior and caveats

- The agent runs as an ephemeral local bridgectl non-TTY session, not as an attachment to an existing desktop session.
- The runner requires exactly one fenced JSON block and validates issue IDs, severity, and annotation lengths.
- Alert fingerprints use stable `service`, `issue_id`, `severity`, `source`, and `alertname` labels.
- Alerts use an expiry time (90 minutes by default). A healthy run does **not** immediately resolve a previous alert; it expires if not refreshed. Keep TTL greater than the schedule interval plus expected delays.
- An agent failure, malformed JSON or Alertmanager HTTP failure fails the Job. Configure Kubernetes Job-failure monitoring separately so a failed monitor does not look like a healthy application.
- This example is not production-hardened. Review outbound network access, workload identity, agent permissions, token costs, image pinning, alert quality, and provider CLI output compatibility before deployment.
- Do not expose raw model output or secrets as alert labels. Alertmanager's routing and Slack receiver must already exist.

To test locally, use `kubectl create job --from=cronjob/bridgectl-monitor bridgectl-monitor-manual`, then inspect its logs and Alertmanager's active alerts.
