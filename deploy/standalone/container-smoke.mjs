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
let password;
let cookie;
let csrf;
let recoveredPassword;
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
function refreshBoundPort() {
  const port = docker(['port', container, '8787/tcp']);
  assert.match(port, /^127\.0\.0\.1:\d+$/);
  base = `http://${port}`;
}
function startupFailure(message) {
  // Only this freshly-created labelled fixture; never dump operator env/token.
  const state = docker(['inspect', '--format', '{{json .State}}', container]);
  const logs = spawnSync('docker', ['logs', container], { encoding: 'utf8', timeout: 45000 });
  const safeLogs = `${logs.stdout ?? ''}${logs.stderr ?? ''}`.replace(/[a-f0-9]{64}/gi, '[redacted]');
  console.error(`Isolated fixture ${message}: state=${state}; current bound URL=${base}; logs=${safeLogs}`);
  throw new Error(message);
}
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
  assert.ok(refusedExisting, 'initialization must not overwrite an existing admin password');

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
  refreshBoundPort();
  for (let i = 0; i < 40; i++) {
    try { if ((await request('/healthz')).ok) break; } catch { /* startup */ }
    if (i === 39) startupFailure('isolated test container did not become healthy');
    await sleep(250);
  }
  docker(['exec', container, '/salcara-hub-launcher', '-healthcheck']);
  const health = await (await request('/healthz')).json();
  assert.equal(health.product, 'salcara-hub-standalone', 'must not run another edition');
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

  // Read ONLY the new isolated fixture password. Never print it or use an operator's file.
  tempDir = mkdtempSync(join(tmpdir(), 'salcara-hub-standalone-test-'));
  const testPasswordPath = join(tempDir, 'admin-initial-login.txt');
  docker(['cp', `${container}:/data/admin-initial-login.txt`, testPasswordPath]);
  assert.equal(statSync(testPasswordPath).mode & 0o777, 0o600);
  password = readFileSync(testPasswordPath, 'utf8').trim();
  assert.match(password, /^[A-Za-z0-9_-]{43}$/);
  const origin = 'https://relay.example.com';
  const login = async value => {
    const result = await request('/salcara-hub/_admin/v1/auth/login', {
      method: 'POST', headers: { Origin: origin, 'Content-Type': 'application/json' },
      body: JSON.stringify({ password: value }),
    });
    assert.equal(result.status, 200);
    const header = result.headers.get('set-cookie');
    assert.match(header, /HttpOnly/i); assert.match(header, /Secure/i);
    assert.match(header, /SameSite=Strict/i); assert.match(header, /Path=\/salcara-hub\//i);
    cookie = header.split(';')[0]; csrf = (await result.json()).csrf_token;
    assert.match(csrf, /^[A-Za-z0-9_-]{32,128}$/);
  };
  await login(password);
  let adminHeaders = { Cookie: cookie, Origin: origin, 'X-Salcara-CSRF': csrf };
  assert.equal((await request('/salcara-hub/_admin/v1/state', { headers: { Authorization: `Bearer ${password}` } })).status, 401,
    'administrator Bearer authentication was removed');
  const noCSRF = await request('/salcara-hub/_admin/v1/resource-mode', {
    method: 'POST', headers: { Cookie: cookie, Origin: origin, 'Content-Type': 'application/json' },
    body: JSON.stringify({ mode: 'balanced', confirm: true }),
  });
  assert.equal(noCSRF.status, 403);
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
  assert.equal(updateStatus.current_version, config.Labels['org.opencontainers.image.version']);
  // Status does not fetch the public feed. Never apply a real
  // network update or use an operator publisher private key in this smoke.
  const changeMode = async (mode, confirm) => request('/salcara-hub/_admin/v1/resource-mode', {
    method: 'POST', headers: { Cookie: cookie, Origin: origin, 'X-Salcara-CSRF': csrf, 'Content-Type': 'application/json' },
    body: JSON.stringify({ mode, confirm }),
  });
  assert.equal((await changeMode('balanced', false)).status, 400);
  assert.equal((await changeMode('balanced', true)).status, 200);
  const stats = JSON.parse(docker(['stats', '--no-stream', '--format', '{{json .}}', container]));
  console.log(`Isolated idle container only (not a capacity promise): memory=${stats.MemUsage}, CPU=${stats.CPUPerc}, PIDs=${stats.PIDs}.`);
  docker(['stop', '--time', '30', container]);
  docker(['start', container]);
  // Docker may allocate a different ephemeral host port after stop/start.
  // Read the new binding instead of probing the former, now closed port.
  refreshBoundPort();
  for (let i = 0; i < 40; i++) {
    try { if ((await request('/healthz')).ok) break; } catch { /* startup */ }
    if (i === 39) startupFailure('restart failed');
    await sleep(250);
  }
  assert.equal((await request('/salcara-hub/_admin/v1/state', { headers: adminHeaders })).status, 401,
    'process restart revokes in-memory login sessions');
  await login(password);
  adminHeaders = { Cookie: cookie, Origin: origin, 'X-Salcara-CSRF': csrf };
  const restarted = await request('/salcara-hub/_admin/v1/state', { headers: adminHeaders });
  assert.equal(restarted.status, 200, 'password is preserved across restart');
  assert.equal((await restarted.json()).resource_mode, 'balanced', 'restart must preserve the selected resource mode');
  assert.equal((await changeMode('economy', true)).status, 200);
  const oldCookie = cookie;
  const newPassword = randomBytes(32).toString('base64url');
  const changed = await request('/salcara-hub/_admin/v1/auth/password', {
    method: 'POST', headers: { ...adminHeaders, 'Content-Type': 'application/json' },
    body: JSON.stringify({ current_password: password, new_password: newPassword }),
  });
  assert.equal(changed.status, 200);
  assert.equal((await request('/salcara-hub/_admin/v1/state', { headers: { Cookie: oldCookie } })).status, 401);
  const rejected = await request('/salcara-hub/_admin/v1/auth/login', {
    method: 'POST', headers: { Origin: origin, 'Content-Type': 'application/json' }, body: JSON.stringify({ password }),
  });
  assert.equal(rejected.status, 401, 'old password cannot log in after change');
  await login(newPassword);
  let initialFileRemoved = false;
  try { docker(['cp', `${container}:/data/admin-initial-login.txt`, join(tempDir, 'must-not-exist')]); }
  catch { initialFileRemoved = true; }
  assert.ok(initialFileRemoved, 'password change removes container initial plaintext file');
  let refusedLiveReset = false;
  try { docker(['run', '--rm', '--network', 'none', '--read-only', ...mount, ...env, image, '-reset-admin-key']); }
  catch { refusedLiveReset = true; }
  assert.ok(refusedLiveReset, 'offline recovery must refuse a live data writer');
  docker(['stop', '--time', '30', container]);
  docker(['run', '--rm', '--network', 'none', '--read-only', ...mount, ...env, image, '-reset-admin-key']);
  docker(['start', container]);
  refreshBoundPort();
  for (let i = 0; i < 40; i++) {
    try { if ((await request('/healthz')).ok) break; } catch { /* startup */ }
    if (i === 39) startupFailure('restart after offline recovery failed');
    await sleep(250);
  }
  const recoveredPath = join(tempDir, 'recovered-initial-login.txt');
  docker(['cp', `${container}:/data/admin-initial-login.txt`, recoveredPath]);
  assert.equal(statSync(recoveredPath).mode & 0o777, 0o600);
  recoveredPassword = readFileSync(recoveredPath, 'utf8').trim();
  assert.match(recoveredPassword, /^[A-Za-z0-9_-]{43}$/);
  assert.notEqual(recoveredPassword, newPassword);
  assert.equal((await request('/salcara-hub/_admin/v1/state', { headers: { Cookie: cookie } })).status, 401);
  const obsoleteLogin = await request('/salcara-hub/_admin/v1/auth/login', {
    method: 'POST', headers: { Origin: origin, 'Content-Type': 'application/json' }, body: JSON.stringify({ password: newPassword }),
  });
  assert.equal(obsoleteLogin.status, 401, 'the previous key is revoked after offline recovery');
  await login(recoveredPassword);
  const recoveredState = await request('/salcara-hub/_admin/v1/state', { headers: { Cookie: cookie } });
  assert.equal(recoveredState.status, 200);
  assert.equal((await recoveredState.json()).resource_mode, 'economy', 'offline key recovery preserves saved service configuration');
  const logResult = spawnSync('docker', ['logs', container], { encoding: 'utf8', timeout: 45000 });
  assert.equal(logResult.status, 0, 'isolated test logs must be readable');
  assert.ok(![password, newPassword, recoveredPassword, cookie, csrf].some(secret => (logResult.stdout + logResult.stderr).includes(secret)),
    'passwords and session secrets must never appear in container logs');
  console.log('Standalone container smoke passed: isolated management key login, secure cookie/CSRF, change/revocation, offline recovery, persistence, non-root/read-only launcher and update status.');
} finally {
  password = cookie = csrf = recoveredPassword = undefined;
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
