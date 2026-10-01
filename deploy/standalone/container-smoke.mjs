// Isolated CI smoke test. It creates only labelled salcara-hub-standalone-test-* resources.
// No production service, model API, existing credential or remote server is used.
import assert from 'node:assert/strict';
import { execFileSync, spawnSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { mkdtempSync, readFileSync, rmSync, statSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';

const image = process.argv[2] || 'salcara-hub-standalone:ci';
const suffix = randomBytes(5).toString('hex');
const container = `salcara-hub-standalone-test-${suffix}`;
const volume = `${container}-data`;
const label = 'salcara.test=standalone';
let madeContainer = false;
let madeVolume = false;
let tempDir;
let token;
const docker = (args, options = {}) => execFileSync('docker', args, {
  encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], timeout: 45000, ...options,
}).trim();
const sleep = (ms) => new Promise((resolve) => setTimeout(resolve, ms));
const env = ['-e', 'SALCARA_HUB_PUBLIC_URL=https://relay.example.com/salcara-hub',
  '-e', 'GOMEMLIMIT=32MiB', '-e', 'GOMAXPROCS=2'];
const mount = ['--mount', `type=volume,src=${volume},dst=/data`];

async function request(path, options = {}) {
  return fetch(`${base}${path}`, { ...options, signal: AbortSignal.timeout(5000) });
}
let base;
try {
  const config = JSON.parse(docker(['image', 'inspect', image]))[0].Config;
  assert.equal(config.User, '65532:65532');
  assert.deepEqual(config.Entrypoint, ['/salcara-hub-launcher']);
  assert.ok(config.Healthcheck.Test.includes('-healthcheck'));
  docker(['volume', 'create', '--label', label, volume]);
  madeVolume = true;
  docker(['run', '--rm', '--network', 'none', '--read-only', ...mount, ...env, image, '-init']);
  let refusedExisting = false;
  try {
    docker(['run', '--rm', '--network', 'none', '--read-only', ...mount, ...env, image, '-init']);
  } catch { refusedExisting = true; }
  assert.ok(refusedExisting, 'initialization must not overwrite an existing admin token');

  docker(['run', '-d', '--name', container, '--label', label, '--read-only',
    '--cap-drop=ALL', '--security-opt=no-new-privileges',
    '--pids-limit=64', '--memory=384m', '--cpus=1',
    '--tmpfs', '/tmp:rw,noexec,nosuid,nodev,size=16m,mode=1777',
    '--publish', '127.0.0.1::8787', ...mount, ...env, image]);
  madeContainer = true;
  const runtime = JSON.parse(docker(['inspect', container]))[0];
  assert.equal(runtime.HostConfig.ReadonlyRootfs, true);
  assert.equal(runtime.HostConfig.Memory, 384 * 1024 * 1024);
  assert.equal(runtime.HostConfig.NanoCpus, 1_000_000_000);
  assert.equal(runtime.HostConfig.PidsLimit, 64);
  assert.ok(runtime.HostConfig.CapDrop.includes('ALL'));
  assert.ok(runtime.HostConfig.SecurityOpt.some(value => /^no-new-privileges(?:=true)?$/.test(value)));
  const port = docker(['port', container, '8787/tcp']);
  assert.match(port, /^127\.0\.0\.1:\d+$/);
  base = `http://${port}`;
  for (let i = 0; i < 40; i++) {
    try { if ((await request('/healthz')).ok) break; } catch { /* startup */ }
    if (i === 39) throw new Error('isolated test container did not become healthy');
    await sleep(250);
  }
  docker(['exec', container, '/salcara-hub-launcher', '-healthcheck']);
  assert.equal((await request('/salcara-hub/v1/ping')).status, 200);
  assert.equal((await request('/v1/ping')).status, 404, 'must not claim model API /v1');
  const page = await request('/salcara-hub/admin/');
  assert.equal(page.status, 200);
  assert.equal(page.headers.get('x-frame-options'), 'SAMEORIGIN');
  assert.match(page.headers.get('content-security-policy') || '', /frame-ancestors 'self'/);
  const clean = await request('/salcara-hub/admin/?token=fake-sub2api-token&user_id=123', { redirect: 'manual' });
  assert.equal(clean.status, 303);
  assert.equal(clean.headers.get('location'), '/salcara-hub/admin/');
  assert.equal((await request('/salcara-hub/_admin/v1/state')).status, 401);

  // Read ONLY the new isolated test token, never an operator's token. Never print it.
  tempDir = mkdtempSync(join(tmpdir(), 'salcara-hub-standalone-test-'));
  const testTokenPath = join(tempDir, 'admin-token');
  docker(['cp', `${container}:/data/admin-token`, testTokenPath]);
  assert.equal(statSync(testTokenPath).mode & 0o777, 0o600);
  token = readFileSync(testTokenPath, 'utf8').trim();
  assert.match(token, /^[a-f0-9]{64}$/);
  const adminHeaders = { Authorization: `Bearer ${token}` };
  const stateResponse = await request('/salcara-hub/_admin/v1/state', { headers: adminHeaders });
  assert.equal(stateResponse.status, 200);
  const state = await stateResponse.json();
  assert.equal(state.resource_mode, 'economy');
  assert.deepEqual(state.resource_modes.map(item => item.id).sort(), ['balanced', 'economy', 'performance']);
  const statusResponse = await request('/salcara-hub/_admin/v1/update/status', { headers: adminHeaders });
  assert.equal(statusResponse.status, 200);
  const updateStatus = await statusResponse.json();
  assert.equal(updateStatus.configured, true, 'same-container launcher must be configured');
  assert.equal(updateStatus.current_version, state.standalone_version);
  // Status does not fetch the unpublished public feed. Never apply a real
  // network update or use an operator publisher private key in this smoke.
  const changeMode = async (mode, confirm) => request('/salcara-hub/_admin/v1/resource-mode', {
    method: 'POST', headers: { ...adminHeaders, 'Content-Type': 'application/json' },
    body: JSON.stringify({ mode, confirm }),
  });
  assert.equal((await changeMode('balanced', false)).status, 400);
  assert.equal((await changeMode('balanced', true)).status, 200);
  const stats = JSON.parse(docker(['stats', '--no-stream', '--format', '{{json .}}', container]));
  console.log(`Isolated idle container only (not a capacity promise): memory=${stats.MemUsage}, CPU=${stats.CPUPerc}, PIDs=${stats.PIDs}.`);
  docker(['stop', '--time', '30', container]);
  docker(['start', container]);
  for (let i = 0; i < 40; i++) {
    try { if ((await request('/healthz')).ok) break; } catch { /* startup */ }
    if (i === 39) throw new Error('restart failed');
    await sleep(250);
  }
  const restarted = await request('/salcara-hub/_admin/v1/state', { headers: adminHeaders });
  assert.equal(restarted.status, 200,
    'restart must preserve the existing admin identity');
  assert.equal((await restarted.json()).resource_mode, 'balanced', 'restart must preserve the selected resource mode');
  assert.equal((await changeMode('economy', true)).status, 200);
  const logResult = spawnSync('docker', ['logs', container], { encoding: 'utf8', timeout: 45000 });
  assert.equal(logResult.status, 0, 'isolated test logs must be readable');
  assert.ok(!(logResult.stdout + logResult.stderr).includes(token), 'new test token must never appear in container logs');
  console.log('Standalone container smoke passed: one-container launcher, non-root/read-only, safe init, health, prefix/iframe isolation, admin auth, three resource modes and persistence, update status without fetching a feed.');
} finally {
  token = undefined;
  if (madeContainer) {
    const owned = docker(['inspect', '--format', '{{index .Config.Labels "salcara.test"}}', container]);
    assert.equal(owned, 'standalone');
    docker(['stop', '--time', '30', container]);
    docker(['rm', container]);
  }
  if (madeVolume) {
    const owned = docker(['volume', 'inspect', '--format', '{{index .Labels "salcara.test"}}', volume]);
    assert.equal(owned, 'standalone');
    docker(['volume', 'rm', volume]);
  }
  if (tempDir) {
    assert.ok(tempDir.startsWith(join(tmpdir(), 'salcara-hub-standalone-test-')));
    rmSync(tempDir, { recursive: true, force: true });
  }
}
