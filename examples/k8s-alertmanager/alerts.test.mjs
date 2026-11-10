import assert from "node:assert/strict";
import { test } from "node:test";
import { createAlerts } from "./alerts.mjs";

test("a healthy run still posts a dated cluster marker", () => {
  const first = createAlerts({ issues: [] }, "mca-cluster", "bridgectl-monitor",
    new Date("2026-11-10T07:30:00Z"), 90);
  const second = createAlerts({ issues: [] }, "mca-cluster", "bridgectl-monitor",
    new Date("2026-11-10T08:00:00Z"), 90);

  assert.equal(first.issueCount, 0);
  assert.equal(first.alerts.length, 1);
  assert.deepEqual(first.alerts[0].labels, second.alerts[0].labels);
  assert.equal(first.alerts[0].labels.severity, "info");
  assert.equal(first.alerts[0].labels.cluster, "mca-cluster");
  assert.equal(first.alerts[0].labels.namespace, "bridgectl-monitor");
  assert.match(first.alerts[0].annotations.summary, /2026-11-10T07:30:00.000Z/);
  assert.equal(second.alerts[0].annotations.completed_at, "2026-11-10T08:00:00.000Z");
});

test("confirmed issues are posted alongside the run marker", () => {
  const result = createAlerts({ issues: [{
    id: "node not ready", severity: "warning",
    summary: "Node unavailable", evidence: "Ready=False for ten minutes",
  }] }, "mca-cluster", "bridgectl-monitor", new Date("2026-11-10T07:30:00Z"), 90);

  assert.equal(result.issueCount, 1);
  assert.equal(result.alerts.length, 2);
  assert.equal(result.alerts[0].labels.alertname, "BridgectlMonitorRun");
  assert.equal(result.alerts[1].labels.alertname, "OrchaelScheduledFinding");
  assert.equal(result.alerts[1].labels.issue_id, "node-not-ready");
});
