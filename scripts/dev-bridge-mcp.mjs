// Development-only stdio adapter for the local Bridge Docker stack.
// The service secret stays in the web container, never in agent configuration.
import { execFileSync, spawn } from 'node:child_process';
import { Buffer } from 'node:buffer';
import { readFileSync } from 'node:fs';
import { resolve, dirname } from 'node:path';
import process from 'node:process';
import { fileURLToPath } from 'node:url';

const script = fileURLToPath(import.meta.url);
const root = resolve(dirname(script), '..');
const allowedTools = new Set(['list_attention', 'inspect_session', 'diagnose_session']);
const uuid = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/i;

// Runs inside the existing web container using its existing signing identity.
const forwardScript = String.raw`
const {createHash, createHmac, randomUUID} = require('node:crypto');
const {readFileSync} = require('node:fs');
(async () => {
  const {user, organization, message, bridgeURL} = JSON.parse(readFileSync(0, 'utf8'));
  const base = new URL(process.env.BRIDGE_API_URL || 'http://api:8080');
  if (!['api', 'localhost', '127.0.0.1'].includes(base.hostname) ||
      base.protocol !== 'http:' || base.username || base.password ||
      new URL(process.env.BRIDGE_PUBLIC_URL).origin !== bridgeURL) throw new Error();
  const body = JSON.stringify(message), path = '/v1/mcp';
  const timestamp = String(Math.floor(Date.now() / 1000)), requestId = randomUUID();
  const canonical = ['POST', path, timestamp, user, organization, requestId,
    createHash('sha256').update(body).digest('hex')].join('\n');
  const signature = createHmac('sha256', process.env.BRIDGE_SERVICE_SECRET)
    .update(canonical).digest('hex');
  const response = await fetch(new URL(path, base), {
    method: 'POST', body, redirect: 'error', signal: AbortSignal.timeout(20000),
    headers: {'content-type': 'application/json', 'accept': 'application/json',
      'x-bridge-user': user, 'x-bridge-organization': organization,
      'x-bridge-timestamp': timestamp, 'x-bridge-request-id': requestId,
      'x-bridge-signature': signature}
  });
  if (!response.ok) throw new Error();
  if (response.status !== 202) process.stdout.write(await response.text());
})().catch(() => { process.stderr.write('Bridge MCP request failed\n'); process.exitCode = 1; });
`;

export function options(env = process.env) {
  return {
    stateDir: resolve(env.DEV_STATE_DIR || resolve(root, '.dev/bridgectl')),
    web: env.DEV_MCP_WEB_CONTAINER || 'bridge-web-1',
    db: env.DEV_MCP_DB_CONTAINER || 'bridge-db-1',
    database: env.DEV_MCP_DATABASE || 'bridge',
  };
}

function docker(args, input) {
  try {
    return execFileSync('docker', args, {
      input, encoding: 'utf8', timeout: 25000, maxBuffer: 2 * 1024 * 1024,
      stdio: ['pipe', 'pipe', 'pipe'],
    }).trim();
  } catch {
    // Do not echo subprocess output, SQL, signing headers, or credentials.
    throw new Error('Local Bridge MCP unavailable. Check Docker access, the Bridge dev containers, and dev enrollment.');
  }
}

export function connect(config, run = docker) {
  let enrollment;
  try {
    enrollment = JSON.parse(readFileSync(resolve(config.stateDir, 'bridge-enrollment.json'), 'utf8'));
  } catch {
    throw new Error('Dev enrollment missing. Run make dev-login LOGIN_ARGS="--bridge https://bridge.orchael.dev --no-browser" first.');
  }
  const { organization_id: organization, installation_id: installation, bridge_url: bridgeURL } = enrollment;
  if (!uuid.test(organization) || !uuid.test(installation) || bridgeURL !== 'https://bridge.orchael.dev') {
    throw new Error('Local Bridge MCP requires a valid bridge.orchael.dev enrollment.');
  }
  const query = `SELECT approved_by FROM device_authorizations WHERE installation_id='${installation}' AND organization_id='${organization}' AND approved_by IS NOT NULL ORDER BY created_at DESC LIMIT 1`;
  const user = run(['exec', config.db, 'psql', '-X', '-v', 'ON_ERROR_STOP=1', '-U', 'postgres', '-d', config.database, '-Atc', query]);
  if (!uuid.test(user)) throw new Error('No enrollment approver found in the local Bridge database. Re-enroll with make dev-login.');
  return {
    installation,
    forward(message) {
      const output = run(['exec', '-i', config.web, 'node', '-e', forwardScript],
        JSON.stringify({ user, organization, bridgeURL, message }));
      return output ? JSON.parse(output) : undefined;
    },
  };
}

function rpcError(id, code, message) {
  return { jsonrpc: '2.0', id, error: { code, message } };
}

export function dispatch(line, forward) {
  let message;
  try { message = JSON.parse(line); } catch { return rpcError(null, -32700, 'Parse error'); }
  if (!message || Array.isArray(message) || message.jsonrpc !== '2.0' || typeof message.method !== 'string' ||
      ('id' in message && typeof message.id !== 'string' && typeof message.id !== 'number')) {
    return rpcError(null, -32600, 'Invalid request');
  }
  // No response to notifications, including cancellation; this adapter has no subscriptions.
  if (!('id' in message)) return undefined;
  const { id, method } = message;
  if (!['initialize', 'ping', 'tools/list', 'tools/call'].includes(method)) {
    return rpcError(id, -32601, 'Method not found');
  }
  if (method === 'tools/call' && !allowedTools.has(message.params?.name)) {
    return rpcError(id, -32602, 'Only session discovery, inspection, and diagnostics are available');
  }
  try {
    const response = forward(message);
    if (!response || response.jsonrpc !== '2.0' || response.id !== id) throw new Error();
    if (method === 'tools/list' && response.result) {
      response.result.tools = response.result.tools.filter(tool => allowedTools.has(tool.name));
    }
    return response;
  } catch {
    return rpcError(id, -32603, 'Local Bridge MCP request failed; run make dev-mcp-check');
  }
}

export function agentArgs(agent, config, extra = []) {
  const server = {
    command: process.execPath, args: [script, 'serve'],
    env: { DEV_STATE_DIR: config.stateDir, DEV_MCP_WEB_CONTAINER: config.web,
      DEV_MCP_DB_CONTAINER: config.db, DEV_MCP_DATABASE: config.database },
  };
  if (agent === 'claude') return ['--mcp-config', JSON.stringify({ mcpServers: { bridge_dev: server } }), ...extra];
  if (agent !== 'codex') throw new Error('Expected codex or claude');
  // JSON strings/arrays are also valid TOML strings/arrays for these values.
  const args = [];
  for (const [key, value] of Object.entries(server)) {
    if (key === 'env') {
      for (const [name, entry] of Object.entries(value)) args.push('-c', `mcp_servers.bridge_dev.env.${name}=${JSON.stringify(entry)}`);
    } else args.push('-c', `mcp_servers.bridge_dev.${key}=${JSON.stringify(value)}`);
  }
  return [...args, ...extra];
}

async function serve(forward) {
  // MCP stdio is newline-delimited JSON. Bound input before parsing or forwarding.
  let buffer = Buffer.alloc(0);
  for await (const chunk of process.stdin) {
    buffer = Buffer.concat([buffer, chunk]);
    let newline;
    while ((newline = buffer.indexOf(10)) !== -1) {
      if (newline > 65536) throw new Error('MCP request exceeds 64 KiB');
      const response = dispatch(buffer.subarray(0, newline).toString('utf8'), forward);
      buffer = buffer.subarray(newline + 1);
      if (response) process.stdout.write(`${JSON.stringify(response)}\n`);
    }
    if (buffer.length > 65536) throw new Error('MCP request exceeds 64 KiB');
  }
}

async function main() {
  const [mode = 'serve', ...extra] = process.argv.slice(2);
  const config = options();
  if (mode === 'codex' || mode === 'claude') {
    const enabled = process.env.DEV_MCP !== '0';
    if (enabled) check(connect(config));
    const child = spawn(mode, enabled ? agentArgs(mode, config, extra) : extra, { stdio: 'inherit' });
    for (const signal of ['SIGINT', 'SIGTERM', 'SIGHUP']) process.on(signal, () => child.kill(signal));
    child.on('error', () => { process.stderr.write(`Unable to launch ${mode}\n`); process.exitCode = 1; });
    child.on('exit', (code, signal) => { process.exitCode = code ?? (signal ? 1 : 0); });
    return;
  }
  if (!['serve', 'check'].includes(mode)) throw new Error('Usage: dev-bridge-mcp.mjs serve|check|codex|claude');
  const client = connect(config);
  if (mode === 'serve') await serve(client.forward);
  else {
    check(client);
    if (process.env.DEV_MCP_SESSION) {
      const response = client.forward({ jsonrpc: '2.0', id: 3, method: 'tools/call', params: {
        name: 'diagnose_session', arguments: { installationId: client.installation, sessionId: process.env.DEV_MCP_SESSION },
      } });
      process.stdout.write(`${JSON.stringify(response)}\n`);
      if (response.error || response.result?.isError) process.exitCode = 1;
    }
  }
}

function check(client) {
  for (const [id, method] of [[1, 'initialize'], [2, 'tools/list']]) {
    const response = dispatch(JSON.stringify({ jsonrpc: '2.0', id, method,
      ...(method === 'initialize' ? { params: { protocolVersion: '2025-06-18', capabilities: {}, clientInfo: { name: 'bridgectl-dev-check', version: '1' } } } : {}),
    }), client.forward);
    if (response.error || (method === 'tools/list' && !response.result?.tools?.some(t => t.name === 'diagnose_session'))) {
      throw new Error('Bridge MCP check failed. Check the dev containers and enrollment membership.');
    }
  }
  process.stderr.write('Bridge dev MCP ready: list_attention, inspect_session, diagnose_session\n');
}

if (process.argv[1] && resolve(process.argv[1]) === script) {
  main().catch(error => { process.stderr.write(`${error.message}\n`); process.exitCode = 1; });
}
