Perform a generic health check of this Kubernetes cluster using only the JSON
snapshot appended below. Do not run shell commands or tools. Check node readiness, pods that are failing or stuck, and
deployments with unavailable replicas. Distinguish an ongoing failure from a
short-lived rollout or completed Job. Report only actionable findings supported
by the snapshot. Do not infer health for resources absent from the snapshot.
Do not disclose credentials or personal data.

Return only a JSON object with an issues array and no Markdown fences. Each
issue must have id, severity, summary, and evidence strings.
Use a short, stable, lowercase slug for id (letters, digits, hyphens only).
Use "critical" only for urgent confirmed failures, otherwise "warning". Use
stable issue IDs across runs so Alertmanager can deduplicate. Return
{"issues":[]} when no issue is found.
