import { spawn } from "node:child_process";
import { readFile } from "node:fs/promises";
import { request } from "node:https";

const required = (name) => {
  if (!process.env[name]) throw new Error(`Missing ${name}`);
  return process.env[name];
};
const provider = process.env.PROVIDER || "codex";
const timeoutMs = Number(process.env.AGENT_TIMEOUT_MS || 600000);
const ttlMinutes = Number(process.env.ALERT_TTL_MINUTES || 90);
const maxOutput = 1024 * 1024;
if (!["codex", "claude"].includes(provider)) throw new Error("Unsupported provider");
if (!Number.isFinite(timeoutMs) || timeoutMs < 1000) throw new Error("Invalid agent timeout");
if (!Number.isFinite(ttlMinutes) || ttlMinutes < 1) throw new Error("Invalid alert TTL");

// Gather bounded, read-only evidence. No Secret, ConfigMap, or event contents
// are sent to the provider.
async function clusterSnapshot() {
  const token = (await readFile("/var/run/secrets/kubernetes.io/serviceaccount/token", "utf8")).trim();
  const ca = await readFile("/var/run/secrets/kubernetes.io/serviceaccount/ca.crt");
  const host = required("KUBERNETES_SERVICE_HOST");
  const port = process.env.KUBERNETES_SERVICE_PORT || "443";
  const get = (path) => new Promise((resolve, reject) => {
    const req = request({ hostname: host, port, path: `${path}?limit=500`,
      ca, headers: { authorization: `Bearer ${token}` }, timeout: 15000 }, (response) => {
      if (response.statusCode !== 200) {
        response.resume();
        reject(new Error(`Kubernetes API ${path}: HTTP ${response.statusCode}`));
        return;
      }
      let body = "";
      response.on("data", (chunk) => {
        body += chunk;
        if (body.length > 2 * 1024 * 1024) req.destroy(new Error("Kubernetes response too large"));
      });
      response.on("end", () => {
        try {
          const result = JSON.parse(body);
          if (result.metadata?.continue) throw new Error(`Kubernetes API ${path}: too many objects`);
          resolve(result.items);
        } catch (error) { reject(error); }
      });
    });
    req.on("timeout", () => req.destroy(new Error(`Kubernetes API ${path}: timeout`)));
    req.on("error", reject);
    req.end();
  });
    const [nodes, pods, deployments] = await Promise.all([
      get("/api/v1/nodes"),
      get("/api/v1/pods"),
      get("/apis/apps/v1/deployments"),
    ]);
    const snapshot = {
      observedAt: new Date().toISOString(),
      nodes: nodes.map((n) => ({
        name: n.metadata.name,
        conditions: n.status.conditions?.filter((c) => c.status !== "False" || c.type === "Ready")
          .map((c) => ({ type: c.type, status: c.status, reason: c.reason })),
      })),
      pods: pods.filter((p) => p.status.phase !== "Running" && p.status.phase !== "Succeeded" ||
        p.status.containerStatuses?.some((c) => c.state?.waiting || c.state?.terminated && c.state.terminated.exitCode !== 0))
        .map((p) => ({
          namespace: p.metadata.namespace, name: p.metadata.name, phase: p.status.phase,
          containers: p.status.containerStatuses?.map((c) => ({
            name: c.name, ready: c.ready, restarts: c.restartCount,
            waiting: c.state?.waiting?.reason, terminated: c.state?.terminated?.reason,
          })),
        })),
      deployments: deployments.filter((d) => (d.status.availableReplicas || 0) < (d.spec.replicas || 1))
        .map((d) => ({
          namespace: d.metadata.namespace, name: d.metadata.name,
          desired: d.spec.replicas || 1, available: d.status.availableReplicas || 0,
        })),
    };
    const serialized = JSON.stringify(snapshot);
    if (serialized.length > 32768) throw new Error("Cluster snapshot exceeds 32 KiB");
    return serialized;
}

const prompt = await readFile("/config/prompt.md", "utf8");
const evidence = await clusterSnapshot();
const alertmanager = required("ALERTMANAGER_URL").replace(/\/$/, "");
const child = spawn("bridgectl", ["session", "start", "--no-tty", "--provider", provider,
  "--project", "scheduled-monitor", process.env.PROJECT_DIR || "/home/bridge/workspace"], {
  stdio: ["pipe", "pipe", "pipe"],
  env: process.env,
});
let stdout = "", stderr = "", responseReady = false, exceeded = false, timedOut = false;
const completeResponse = () => {
  const matches = [...stdout.matchAll(/```json\s*([\s\S]*?)\s*```/g)];
  if (matches.length === 1 && !responseReady) {
    responseReady = true;
    // The non-TTY CLI stops its session on stdin EOF. Wait for the complete
    // assistant response before sending EOF, or it cancels the running turn.
    child.stdin.end();
  }
};
child.stdout.on("data", (chunk) => {
  stdout += chunk.toString();
  if (stdout.length + stderr.length > maxOutput) { exceeded = true; child.kill("SIGKILL"); }
  else completeResponse();
});
child.stderr.on("data", (chunk) => {
  stderr += chunk.toString();
  if (stdout.length + stderr.length > maxOutput) { exceeded = true; child.kill("SIGKILL"); }
});
child.stdin.write(`${prompt}\nCluster snapshot (read-only Kubernetes API):\n${evidence}\n`);
const timer = setTimeout(() => { timedOut = true; child.kill("SIGKILL"); }, timeoutMs);
const exit = await new Promise((resolve, reject) => {
  child.on("error", reject);
  child.on("close", resolve);
});
clearTimeout(timer);
if (timedOut || exceeded || exit !== 0 || !responseReady) {
  throw new Error(`Agent failed (exit=${exit}, timeout=${timedOut}, outputLimit=${exceeded}, response=${responseReady}): ${stderr.slice(-500)}`);
}
const matches = [...stdout.matchAll(/```json\s*([\s\S]*?)\s*```/g)];
if (matches.length !== 1) throw new Error("Expected one fenced JSON response");
const report = JSON.parse(matches[0][1]);
if (!Array.isArray(report.issues) || report.issues.length > 20) throw new Error("Invalid issues array");
const now = new Date();
const endsAt = new Date(now.getTime() + ttlMinutes * 60000).toISOString();
const clean = (s, max) => {
  if (typeof s !== "string" || !s.trim() || s.length > max) throw new Error("Invalid finding field");
  return s.trim();
};
const alerts = report.issues.map((issue) => {
  const id = clean(issue.id, 100);
  if (!/^[a-zA-Z0-9_.:-]+$/.test(id)) throw new Error("Invalid issue id");
  if (!["warning", "critical"].includes(issue.severity)) throw new Error("Invalid severity");
  return {
    labels: {
      alertname: "OrchaelScheduledFinding",
      service: clean(required("MONITOR_SERVICE"), 80),
      issue_id: id,
      severity: issue.severity,
      source: "bridgectl-cron",
    },
    annotations: {
      summary: clean(issue.summary, 200),
      description: clean(issue.evidence, 1200),
    },
    startsAt: now.toISOString(),
    endsAt,
  };
});
if (alerts.length) {
  const response = await fetch(`${alertmanager}/api/v2/alerts`, {
    method: "POST",
    headers: { "content-type": "application/json" },
    body: JSON.stringify(alerts),
    signal: AbortSignal.timeout(15000),
  });
  if (!response.ok) throw new Error(`Alertmanager returned HTTP ${response.status}`);
}
console.log(JSON.stringify({ issueCount: alerts.length, endsAt }));
