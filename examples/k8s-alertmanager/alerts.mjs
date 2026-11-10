const clean = (value, max) => {
  if (typeof value !== "string" || !value.trim() || value.length > max) {
    throw new Error("Invalid finding field");
  }
  return value.trim();
};

export function createAlerts(report, serviceName, namespaceName, now, ttlMinutes) {
  if (!Array.isArray(report?.issues) || report.issues.length > 20) {
    throw new Error("Invalid issues array");
  }
  const service = clean(serviceName, 80);
  const namespace = clean(namespaceName, 63);
  const startsAt = now.toISOString();
  const endsAt = new Date(now.getTime() + ttlMinutes * 60000).toISOString();

  // Stable labels refresh one visible run marker instead of creating a new
  // Alertmanager fingerprint every 30 minutes. The time belongs in annotations.
  const heartbeat = {
    labels: {
      alertname: "BridgectlMonitorRun",
      service,
      cluster: service,
      namespace,
      issue_id: "check-completed",
      severity: "info",
      source: "bridgectl-cron",
    },
    annotations: {
      summary: `Cluster health check completed for ${service} at ${startsAt}`,
      description: `Completed successfully with ${report.issues.length} confirmed finding(s).`,
      completed_at: startsAt,
    },
    startsAt,
    endsAt,
  };

  const findings = report.issues.map((issue) => {
    const id = clean(issue.id, 100).toLowerCase()
      .replace(/[^a-z0-9_.:-]+/g, "-").replace(/^-+|-+$/g, "");
    if (!id) throw new Error("Invalid issue id");
    if (!["warning", "critical"].includes(issue.severity)) throw new Error("Invalid severity");
    return {
      labels: {
        alertname: "OrchaelScheduledFinding",
        service,
        issue_id: id,
        severity: issue.severity,
        source: "bridgectl-cron",
      },
      annotations: {
        summary: clean(issue.summary, 200),
        description: clean(issue.evidence, 1200),
      },
      startsAt,
      endsAt,
    };
  });
  return { alerts: [heartbeat, ...findings], issueCount: findings.length, endsAt };
}
