// Keep the provider's evidence narrow: status fields only, no object specs,
// environment variables, Secret data, or event messages.
export function summarizeCluster(nodes, pods, deployments, monitorNamespace, observedAt = new Date().toISOString()) {
  return {
    observedAt,
    nodes: nodes.map((n) => ({
      name: n.metadata.name,
      conditions: n.status.conditions?.filter((c) => c.status !== "False" || c.type === "Ready")
        .map((c) => ({
          type: c.type, status: c.status, reason: c.reason,
          lastTransitionTime: c.lastTransitionTime,
        })),
    })),
    pods: pods.filter((p) => {
      if (p.metadata.namespace === monitorNamespace) return false;
      const ready = p.status.conditions?.find((c) => c.type === "Ready");
      return p.status.phase !== "Running" && p.status.phase !== "Succeeded" ||
        ready?.status === "False" ||
        p.status.containerStatuses?.some((c) =>
          !c.ready || c.state?.waiting || c.state?.terminated && c.state.terminated.exitCode !== 0);
    }).map((p) => ({
      namespace: p.metadata.namespace, name: p.metadata.name, phase: p.status.phase,
      ready: p.status.conditions?.filter((c) => c.type === "Ready").map((c) => ({
        status: c.status, reason: c.reason, lastTransitionTime: c.lastTransitionTime,
      })),
      containers: p.status.containerStatuses?.map((c) => ({
        name: c.name, ready: c.ready, restarts: c.restartCount,
        waiting: c.state?.waiting?.reason, terminated: c.state?.terminated?.reason,
      })),
    })),
    deployments: deployments.filter((d) =>
      (d.status.availableReplicas || 0) < (d.spec.replicas ?? 1))
      .map((d) => ({
        namespace: d.metadata.namespace, name: d.metadata.name,
        desired: d.spec.replicas ?? 1, available: d.status.availableReplicas || 0,
      })),
  };
}
