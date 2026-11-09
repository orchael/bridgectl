Perform a generic health check of this Kubernetes cluster using the supplied
read-only snapshot. Check node readiness, pods that are failing or stuck, and
deployments with unavailable replicas. Distinguish an ongoing failure from a
short-lived rollout or completed Job. Report only actionable findings supported
by the snapshot. Do not infer health for resources absent from the snapshot.
Do not disclose credentials or personal data.

Return exactly one fenced JSON block tagged json and no other fenced blocks.
Its object must have an issues array. Each issue must have id, severity,
summary, and evidence strings.
Use "critical" only for urgent confirmed failures, otherwise "warning". Use
stable issue IDs across runs so Alertmanager can deduplicate. Return
{"issues":[]} when no issue is found.
