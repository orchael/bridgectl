import assert from "node:assert/strict";
import { test } from "node:test";
import { summarizeCluster } from "./snapshot.mjs";

test("includes a Running but unready StatefulSet pod with readiness timing", () => {
  const pod = {
    metadata: { namespace: "db", name: "postgres-0" },
    status: {
      phase: "Running",
      conditions: [{ type: "Ready", status: "False", reason: "ContainersNotReady",
        lastTransitionTime: "2026-11-09T10:00:00Z" }],
      containerStatuses: [{ name: "postgres", ready: false, restartCount: 0,
        state: { running: {} } }],
    },
  };
  const snapshot = summarizeCluster([], [pod], [], "monitor", "2026-11-09T10:10:00Z");
  assert.equal(snapshot.pods.length, 1);
  assert.equal(snapshot.pods[0].ready[0].lastTransitionTime, "2026-11-09T10:00:00Z");
  assert.equal(snapshot.pods[0].containers[0].ready, false);
});

test("omits an intentionally scaled-to-zero deployment", () => {
  const deployment = (replicas) => ({
    metadata: { namespace: "apps", name: "worker" },
    spec: replicas === undefined ? {} : { replicas },
    status: { availableReplicas: 0 },
  });
  const zero = summarizeCluster([], [], [deployment(0)], "monitor");
  assert.deepEqual(zero.deployments, []);
  const omitted = summarizeCluster([], [], [deployment(undefined)], "monitor");
  assert.deepEqual(omitted.deployments, [{
    namespace: "apps", name: "worker", desired: 1, available: 0,
  }]);
});
