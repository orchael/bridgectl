import { spawn } from "node:child_process";
import { readFile } from "node:fs/promises";

const required = (name) => {
  if (!process.env[name]) throw new Error(`Missing ${name}`);
  return process.env[name];
};
const project = process.env.PROJECT_DIR || "/workspace";
const provider = process.env.PROVIDER || "codex";
const timeoutMs = Number(process.env.AGENT_TIMEOUT_MS || 600000);
const ttlMinutes = Number(process.env.ALERT_TTL_MINUTES || 90);
const maxOutput = 1024 * 1024;
const prompt = await readFile("/config/prompt.md", "utf8");
const alertmanager = required("ALERTMANAGER_URL").replace(/\/$/, "");
if (!["codex", "claude"].includes(provider)) throw new Error("Unsupported provider");
if (!Number.isFinite(ttlMinutes) || ttlMinutes < 1) throw new Error("Invalid alert TTL");
const child = spawn("bridgectl", ["session", "start", "--no-tty", "--provider", provider, "--project", "scheduled-monitor", project], {
  stdio: ["pipe", "pipe", "pipe"],
  env: process.env,
});
let stdout = "", stderr = "";
const collect = (stream, setter) => stream.on("data", (chunk) => {
  setter(chunk.toString());
  if (stdout.length + stderr.length > maxOutput) child.kill("SIGKILL");
});
collect(child.stdout, (s) => { stdout += s; });
collect(child.stderr, (s) => { stderr += s; });
child.stdin.end(prompt);
const timer = setTimeout(() => child.kill("SIGKILL"), timeoutMs);
const exit = await new Promise((resolve, reject) => {
  child.on("error", reject);
  child.on("close", resolve);
});
clearTimeout(timer);
if (exit !== 0) throw new Error(`Agent exited ${exit}: ${stderr.slice(-500)}`);

// The provider's response must contain exactly one fenced JSON payload.
// Fail closed rather than forwarding unvalidated free-form model output.
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
