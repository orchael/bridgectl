import assert from 'node:assert/strict';
import { test } from 'node:test';
import { mkdtempSync, writeFileSync, rmSync } from 'node:fs';
import { execFileSync } from 'node:child_process';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { agentArgs, connect, dispatch, options } from './dev-bridge-mcp.mjs';

const organization = '752cd43e-8cd8-41ae-9b10-2d9480e834e9';
const installation = 'd98adb31-633e-4e32-8b88-b79c1b2cd339';
const user = '11111111-1111-4111-8111-111111111111';
const rpc = (method, params) => JSON.stringify({ jsonrpc: '2.0', id: 7, method, params });

test('only read-only discovery, inspection and diagnostics can reach Bridge', () => {
  let calls = 0;
  const forward = message => {
    calls++;
    return { jsonrpc: '2.0', id: message.id, result: { tools: [
      { name: 'diagnose_session' }, { name: 'list_sessions' }, { name: 'list_attention' }, { name: 'inspect_session' },
      { name: 'decide_pending_approval' }, { name: 'respond_pending_input' },
    ] } };
  };
  assert.deepEqual(dispatch(rpc('tools/list'), forward).result.tools.map(t => t.name),
    ['diagnose_session', 'list_sessions', 'list_attention', 'inspect_session']);
  for (const name of ['decide_pending_approval', 'respond_pending_input', 'unknown']) {
    assert.equal(dispatch(rpc('tools/call', { name }), forward).error.code, -32602);
  }
  assert.equal(calls, 1);
  dispatch(rpc('tools/call', { name: 'diagnose_session', arguments: { installationId: installation, sessionId: user } }), forward);
  assert.equal(calls, 2);
});

test('protocol errors are bounded and notifications produce no response', () => {
  const unexpected = () => { throw new Error('secret sentinel'); };
  assert.equal(dispatch('{', unexpected).error.code, -32700);
  for (const value of [null, [], {}, { jsonrpc: '2.0', method: 'ping', id: null }]) {
    assert.equal(dispatch(JSON.stringify(value), unexpected).error.code, -32600);
  }
  assert.equal(dispatch('{"jsonrpc":"2.0","method":"notifications/initialized"}', unexpected), undefined);
  assert.equal(dispatch(rpc('unsupported'), unexpected).error.code, -32601);
  assert.equal(dispatch(rpc('ping'), unexpected).error.code, -32603);
  assert.doesNotMatch(JSON.stringify(dispatch(rpc('ping'), unexpected)), /sentinel/);
  assert.equal(dispatch(rpc('ping'), () => ({ jsonrpc: '2.0', id: 8, result: {} })).error.code, -32603);
});

test('enrollment is pinned to development and UUID validation precedes SQL', t => {
  const stateDir = mkdtempSync(join(tmpdir(), 'bridge-mcp-'));
  t.after(() => rmSync(stateDir, { recursive: true, force: true }));
  const config = options({ DEV_STATE_DIR: stateDir });
  const write = changes => writeFileSync(join(stateDir, 'bridge-enrollment.json'), JSON.stringify({
    organization_id: organization, installation_id: installation, bridge_url: 'https://bridge.orchael.dev', ...changes,
  }));
  let calls = 0;
  const run = (args, input) => {
    calls++;
    if (args.includes('psql')) {
      assert.ok(args.includes('ON_ERROR_STOP=1'));
      assert.match(args.at(-1), new RegExp(installation));
      return user;
    }
    const request = JSON.parse(input);
    assert.equal(request.user, user);
    assert.equal(request.organization, organization);
    assert.equal(args[2], 'bridge-web-1');
    return JSON.stringify({ jsonrpc: '2.0', id: request.message.id, result: {} });
  };
  assert.throws(() => connect(config, run), /enrollment missing/);
  write({ bridge_url: 'https://bridge.orchael.com' });
  assert.throws(() => connect(config, run), /valid bridge.orchael.dev/);
  write({ organization_id: "'; SELECT 1; --" });
  assert.throws(() => connect(config, run), /valid bridge.orchael.dev/);
  assert.equal(calls, 0);
  write({});
  const client = connect(config, run);
  assert.equal(client.installation, installation);
  assert.equal(client.forward({ id: 42 }).id, 42);
  assert.equal(calls, 2);
  assert.throws(() => connect(config, () => ''), /No enrollment approver/);
});

test('launch configuration preserves paths and stays scoped to the child agent', () => {
  const config = options({ DEV_STATE_DIR: '/tmp/repo with spaces/state', DEV_MCP_WEB_CONTAINER: 'custom-web' });
  const claude = agentArgs('claude', config, ['--version']);
  assert.equal(claude[0], '--mcp-config');
  const server = JSON.parse(claude[1]).mcpServers.bridge_dev;
  assert.equal(server.env.DEV_STATE_DIR, config.stateDir);
  assert.equal(server.env.DEV_MCP_WEB_CONTAINER, 'custom-web');
  assert.ok(server.args[0].startsWith('/'));
  assert.equal(claude.at(-1), '--version');
  const codex = agentArgs('codex', config, ['mcp', 'list']);
  assert.ok(codex.includes('mcp_servers.bridge_dev.env.DEV_STATE_DIR="/tmp/repo with spaces/state"'));
  assert.deepEqual(codex.slice(-2), ['mcp', 'list']);
  assert.throws(() => agentArgs('other', config));
});

test('session listing forwards optional status unchanged', () => {
  for (const args of [{}, { status: 'running' }, { status: 'stopped' }]) {
    const response = dispatch(rpc('tools/call', { name: 'list_sessions', arguments: args }), message => {
      assert.deepEqual(message.params, { name: 'list_sessions', arguments: args });
      return { jsonrpc: '2.0', id: message.id, result: { content: [] } };
    });
    assert.equal(response.error, undefined);
  }
});

test('Make preserves state paths containing spaces before changing directories', () => {
  for (const stateDir of ['/tmp/repo with spaces/state', 'repo with spaces/state']) {
    const output = execFileSync('make', ['--no-print-directory', '-s', '-f', 'Makefile', '-f', '-', 'print-mcp-path', `DEV_STATE_DIR=${stateDir}`], {
      input: 'print-mcp-path:\n\t@$(DEV_MCP_ENV) printenv DEV_STATE_DIR\n', encoding: 'utf8',
    }).trim();
    assert.equal(output, stateDir.startsWith('/') ? stateDir : `${process.cwd()}/${stateDir}`);
  }
});
