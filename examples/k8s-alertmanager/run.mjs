import { spawn } from "node:child_process";
import { mkdtemp, readFile, writeFile } from "node:fs/promises";
import { request } from "node:https";
import { join } from "node:path";
import { tmpdir } from "node:os";
import { summarizeCluster } from "./snapshot.mjs";

const required = (name) => {
  if (!process.env[name]) throw new Error(`Missing ${name}`);
  return process.env[name];
};
const provider = process.env.PROVIDER || "codex";
const timeoutMs = Number(process.env.AGENT_TIMEOUT_MS || 600000);
const ttlMinutes = Number(process.env.ALERT_TTL_MINUTES || 90);
const monitorNamespace = required("MONITOR_NAMESPACE");
const maxOutput = 1024 * 1024;
if (provider !== "codex") throw new Error("This batch example requires codex");
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
    const snapshot = summarizeCluster(nodes, pods, deployments, monitorNamespace);
    const serialized = JSON.stringify(snapshot);
    if (serialized.length > 32768) throw new Error("Cluster snapshot exceeds 32 KiB");
    return serialized;
}

const prompt = await readFile("/config/prompt.md", "utf8");
const evidence = await clusterSnapshot();
const projectDir = process.env.PROJECT_DIR || "/home/bridge/workspace";
const alertmanager = required("ALERTMANAGER_URL").replace(/\/$/, "");
const codexHome = await mkdtemp(join(tmpdir(), "monitor-codex-"));
await writeFile(join(codexHome, "auth.json"), required("CODEX_AUTH"), { mode: 0o600 });
const reportPath = join(codexHome, "last-message.txt");
const childEnv = { ...process.env, CODEX_HOME: codexHome };
delete childEnv.CODEX_AUTH;
const child = spawn("codex", ["exec", "--sandbox", "read-only", "--skip-git-repo-check",
  "--ephemeral", "--output-last-message", reportPath, "-C", projectDir,
  `${prompt}\nKubernetes snapshot JSON:\n${evidence}`], {
  stdio: ["ignore", "pipe", "pipe"],
  env: childEnv,
});
let stdout = "", stderr = "", exceeded = false, timedOut = false;
child.stdout.on("data", (chunk) => {
  stdout += chunk.toString();
  if (stdout.length + stderr.length > maxOutput) { exceeded = true; child.kill("SIGKILL"); }
});
child.stderr.on("data", (chunk) => {
  stderr += chunk.toString();
  if (stdout.length + stderr.length > maxOutput) { exceeded = true; child.kill("SIGKILL"); }
});
const timer = setTimeout(() => { timedOut = true; child.kill("SIGKILL"); }, timeoutMs);
const exit = await new Promise((resolve, reject) => {
  child.on("error", reject);
  child.on("close", resolve);
});
clearTimeout(timer);
if (timedOut || exceeded || exit !== 0) {
  throw new Error(`Agent failed (exit=${exit}, timeout=${timedOut}, outputLimit=${exceeded}): ${stderr.slice(-500)}`);
}
const report = JSON.parse((await readFile(reportPath, "utf8")).trim());
if (!Array.isArray(report.issues) || report.issues.length > 20) throw new Error("Invalid issues array");
const now = new Date();
const endsAt = new Date(now.getTime() + ttlMinutes * 60000).toISOString();
const clean = (s, max) => {
  if (typeof s !== "string" || !s.trim() || s.length > max) throw new Error("Invalid finding field");
  return s.trim();
};
const alerts = report.issues.map((issue) => {
  const id = clean(issue.id, 100).toLowerCase()
    .replace(/[^a-z0-9_.:-]+/g, "-").replace(/^-+|-+$/g, "");
  if (!id) throw new Error("Invalid issue id");
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
