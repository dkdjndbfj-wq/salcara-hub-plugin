import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const web = new URL('../../internal/standalone/web/', import.meta.url);
const source = await readFile(new URL('servers.js', web), 'utf8');
const markup = await readFile(new URL('index.html', web), 'utf8');
// All addresses, host keys, credentials and artifacts here are in-memory test fixtures.
const fixturePassword = 'synthetic-ssh-password-only';
const fingerprint = 'SHA256:synthetic-fixture-host-key';
const host = (extra = {}) => ({ id: 'host-a', name: 'Fixture host', host: '192.0.2.10', port: 22, username: 'root', auth_type: 'password', fingerprint, platform: 'linux', arch: 'amd64', can_deploy: true, deployed: false, last_checked_at: Date.now(), ...extra });
const state = (extra = {}) => ({ revision: 1, active_id: '', hosts: [], jobs: [], artifacts: { amd64: true, arm64: false }, ...extra });
const check = (extra = {}) => ({ check_id: 'fixture-check', expires_at: Date.now() + 600000, platform: 'linux', arch: 'amd64', can_deploy: true, checks: ['systemd ready', 'sudo ready'], warnings: ['Configure public HTTPS separately.'], ...extra });
const plan = (extra = {}) => ({ plan_id: 'fixture-plan', expires_at: Date.now() + 600000, host_id: 'host-a', origin: 'https://origin.example/salcara-hub', arch: 'amd64', artifact_sha256: 'a'.repeat(64), changes: ['Install Edge service'], warnings: ['Loopback listener only'], ...extra });
const job = (extra = {}) => ({ id: 'job-a', host_id: 'host-a', status: 'running', created_at: Date.now(), updated_at: Date.now(), steps: [{ at: Date.now(), message: 'Preparing service' }], ...extra });
async function settle() { for (let i = 0; i < 8; i++) await new Promise(resolve => setImmediate(resolve)); }
function deferred() { let resolve; const promise = new Promise(done => { resolve = done; }); return { promise, resolve }; }
class Element {
  constructor(tag = 'div') { this.tagName = tag.toUpperCase(); this.value = ''; this.checked = false; this.disabled = false; this.hidden = false; this.open = false; this.children = []; this.dataset = {}; this.handlers = new Map(); this.content = ''; }
  set textContent(value) { this.content = String(value); this.children = []; }
  get textContent() { return this.content + this.children.map(child => child.textContent || '').join(''); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.children = children; this.content = ''; }
  addEventListener(type, handler) { const handlers = this.handlers.get(type) || []; handlers.push(handler); this.handlers.set(type, handlers); }
  showModal() { this.open = true; }
  close() { this.open = false; for (const handler of this.handlers.get('close') || []) handler({ preventDefault() {} }); }
  async emit(type) { if (this.disabled && type === 'click') return; for (const handler of this.handlers.get(type) || []) await handler({ preventDefault() {} }); await settle(); }
}
function findButton(element, label) { if (element.tagName === 'BUTTON' && element.textContent === label) return element; for (const child of element.children) { const found = findButton(child, label); if (found) return found; } return null; }
function browser() {
  const ids = new Map([...markup.matchAll(/id="([^"]+)"/g)].map(match => [match[1], new Element()]));
  const replies = [], calls = [], timers = new Set(), events = new Map(); let epoch = 1, authenticated = true;
  const window = {}, document = { visibilityState: 'visible', getElementById: id => { assert.ok(ids.has(id), `unknown HTML id ${id}`); return ids.get(id); }, createElement: tag => new Element(tag), addEventListener: (event, handler) => events.set(event, handler) };
  vm.runInContext(source, vm.createContext({ window, document, URL, Date, AbortController, TextEncoder, setTimeout: (callback, delay) => { const timer = { callback, delay }; timers.add(timer); return timer; }, clearTimeout: timer => timers.delete(timer) }), { filename: 'servers.js' });
  const controller = window.SalcaraServers({ request: async (path, method = 'GET', body, anonymous, signal) => { calls.push({ path, method, body, signal }); assert.ok(replies.length, `unexpected request ${path}`); const reply = await replies.shift(); if (reply instanceof Error) throw reply; return reply; }, getEpoch: () => epoch, isAuthenticated: () => authenticated, isBusy: () => false });
  const b = { ids, replies, calls, timers, controller, $: id => ids.get(id),
    async open(value = state()) { replies.push(value); controller.setVisible(true); await settle(); },
    async click(id) { await ids.get(id).emit('click'); }, async submit(id) { await ids.get(id).emit('submit'); },
    async refresh(value = state()) { replies.push(value); await controller.refresh(); await settle(); },
    async fireTimer() { const timer = timers.values().next().value; assert.ok(timer); timers.delete(timer); timer.callback(); await settle(); },
    async visibility(hidden) { document.visibilityState = hidden ? 'hidden' : 'visible'; events.get('visibilitychange')?.(); await settle(); },
    logout() { epoch++; authenticated = false; controller.reset(); }, login() { epoch++; authenticated = true; },
    async discover() { ids.get('servers-name').value = 'Fixture host'; ids.get('servers-host').value = '192.0.2.10'; ids.get('servers-password').value = fixturePassword; replies.push({ fingerprint, algorithm: 'ssh-ed25519' }); await b.click('servers-discover'); },
    async trust() { ids.get('servers-trust').checked = true; await ids.get('servers-trust').emit('change'); },
    async check(value = check()) { await b.discover(); await b.trust(); replies.push(value); await b.submit('servers-host-form'); },
    async plan(value = plan()) { await findButton(ids.get('servers-hosts'), '部署方案').emit('click'); ids.get('servers-origin').value = 'https://origin.example/salcara-hub'; await ids.get('servers-origin').emit('input'); replies.push(value); await b.submit('servers-plan-form'); },
  }; return b;
}

test('servers are idle until visible and discovery sends no authentication material', async () => {
  const b = browser(); await settle(); assert.equal(b.calls.length, 0);
  await b.open(); await b.discover();
  assert.deepEqual(JSON.parse(JSON.stringify(b.calls.at(-1).body)), { host: '192.0.2.10', port: 22 });
  assert.equal(b.$('servers-password').value, fixturePassword);
  assert.match(b.$('servers-fingerprint').textContent, /SHA256:/);
  assert.equal(b.$('servers-check').disabled, true);
  await b.submit('servers-host-form'); assert.equal(b.calls.length, 2);
  await b.trust(); assert.equal(b.$('servers-check').disabled, false);
  assert.equal(b.timers.size, 0);
});

test('authentication clears credentials before completion and save uses only check token and original revision', async () => {
  const b = browser(); await b.open(); await b.discover(); await b.trust();
  const pending = deferred(); b.replies.push(pending.promise); await b.submit('servers-host-form');
  assert.equal(b.$('servers-password').value, ''); assert.equal(b.$('servers-host').disabled, true);
  assert.equal(b.calls.at(-1).body.password, fixturePassword); assert.equal(b.calls.at(-1).body.fingerprint, fingerprint);
  pending.resolve(check()); await settle();
  assert.equal(b.$('servers-check-panel').hidden, false); assert.equal(b.$('servers-save').disabled, false);
  await b.refresh(state({ revision: 2 }));
  b.replies.push(new Error('revision_conflict')); await b.click('servers-save');
  assert.deepEqual(JSON.parse(JSON.stringify(b.calls.at(-1).body)), { check_id: 'fixture-check', expected_revision: 1 });
  assert.match(b.$('servers-notice').textContent, /配置已变化/);
  assert.doesNotMatch(b.$('servers-check-panel').textContent + b.$('servers-notice').textContent, /synthetic-ssh-password/);
});

test('editing connection or authentication invalidates both trust and check preview', async () => {
  const b = browser(); await b.open(); await b.check();
  b.$('servers-username').value = 'operator'; await b.$('servers-username').emit('input');
  assert.equal(b.$('servers-fingerprint-panel').hidden, true); assert.equal(b.$('servers-check-panel').hidden, true);
  assert.equal(b.$('servers-trust').checked, false); assert.equal(b.$('servers-save').disabled, true);
  await b.click('servers-save'); assert.equal(b.calls.length, 3);
});

test('private key and passphrase are sent only on trusted check and cannot be echoed by an error', async () => {
  const b = browser(); await b.open();
  b.$('servers-auth-type').value = 'key'; await b.$('servers-auth-type').emit('change');
  await b.discover();
  b.$('servers-private-key').value = 'synthetic-private-key-only'; b.$('servers-passphrase').value = 'synthetic-passphrase';
  await b.$('servers-private-key').emit('input');
  assert.equal(b.$('servers-check').disabled, true);
  b.replies.push({ fingerprint }); await b.click('servers-discover'); await b.trust();
  b.replies.push(new Error('synthetic-private-key-only synthetic-passphrase')); await b.submit('servers-host-form');
  const body = b.calls.at(-1).body;
  assert.equal(body.auth_type, 'key'); assert.equal(body.private_key, 'synthetic-private-key-only'); assert.equal(Object.hasOwn(body, 'password'), false);
  assert.equal(b.$('servers-private-key').value, ''); assert.equal(b.$('servers-passphrase').value, ''); assert.equal(b.$('servers-password').value, '');
  assert.doesNotMatch(b.$('servers-notice').textContent, /synthetic-private|synthetic-passphrase/);
});

test('expired checks and plans cannot be saved or deployed', async () => {
  const b = browser(); await b.open(state({ hosts: [host()] })); await b.check(check({ expires_at: Date.now() - 1 }));
  assert.equal(b.$('servers-save').disabled, true); await b.click('servers-save');
  await b.plan(plan({ expires_at: Date.now() - 1 }));
  assert.equal(b.$('servers-deploy').disabled, true); await b.click('servers-deploy');
  assert.ok(b.calls.every(call => !['servers/save','servers/deploy'].includes(call.path)));
});

test('saving a checked host clears the form without starting deployment', async () => {
  const b = browser(); await b.open(); await b.check();
  b.replies.push(state({ revision: 2, hosts: [host()] })); await b.click('servers-save');
  assert.match(b.$('servers-hosts').textContent, /Fixture host.*预检通过/);
  assert.equal(b.$('servers-host').value, ''); assert.equal(b.$('servers-check-panel').hidden, true);
  assert.equal(b.calls.at(-1).path, 'servers/save'); assert.equal(b.timers.size, 0);
});

test('deployment requires an explicit current plan, retains its revision, and reports acceptance as running', async () => {
  const b = browser(); await b.open(state({ hosts: [host()] })); await b.plan();
  assert.equal(b.calls.at(-1).path, 'servers/plan'); assert.equal(b.$('servers-plan-preview').hidden, false);
  assert.match(b.$('servers-plan-changes').textContent, /Install Edge/); assert.equal(b.$('servers-deploy').disabled, false);
  await b.refresh(state({ revision: 2, hosts: [host()] }));
  b.replies.push(state({ revision: 3, hosts: [host()], jobs: [job()] })); await b.click('servers-deploy');
  assert.deepEqual(JSON.parse(JSON.stringify(b.calls.at(-1).body)), { plan_id: 'fixture-plan', expected_revision: 1 });
  assert.equal(b.$('servers-plan-dialog').open, false); assert.match(b.$('servers-jobs').textContent, /部署中/);
  assert.match(b.$('servers-notice').textContent, /已受理.*不代表安装完成/);
  assert.equal(b.timers.size, 1); assert.equal(b.timers.values().next().value.delay, 5000);
});

test('changing the origin invalidates the approval and failed deployment never automatically retries', async () => {
  const b = browser(); await b.open(state({ hosts: [host()] })); await b.plan();
  b.$('servers-origin').value = 'https://new-origin.example/salcara-hub'; await b.$('servers-origin').emit('input');
  assert.equal(b.$('servers-deploy').disabled, true); await b.click('servers-deploy');
  b.replies.push(plan({ origin: 'https://new-origin.example/salcara-hub' })); await b.submit('servers-plan-form');
  b.replies.push(new Error('request_timeout')); await b.click('servers-deploy');
  assert.equal(b.$('servers-deploy').disabled, true); assert.equal(b.$('servers-plan-preview').hidden, true);
  assert.match(b.$('servers-plan-notice').textContent, /不会自动重试/);
  await b.click('servers-deploy'); assert.equal(b.calls.filter(call => call.path === 'servers/deploy').length, 1);
});

test('running jobs poll only while visible, stop on completion, and interrupted tasks require remote inspection', async () => {
  const b = browser(); await b.open(state({ hosts: [host()], jobs: [job()] })); assert.equal(b.timers.size, 1);
  await b.visibility(true); assert.equal(b.timers.size, 0);
  b.replies.push(state({ hosts: [host()], jobs: [job()] })); await b.visibility(false); assert.equal(b.timers.size, 1);
  b.replies.push(state({ hosts: [host({ deployed: true })], jobs: [job({ status: 'succeeded' })] })); await b.fireTimer();
  assert.equal(b.timers.size, 0); assert.match(b.$('servers-jobs').textContent, /部署完成/);
  await b.refresh(state({ hosts: [host()], jobs: [job({ status: 'interrupted' })] }));
  assert.match(b.$('servers-jobs').textContent, /请先核对远端/); assert.equal(b.timers.size, 0);
  assert.ok(b.calls.every(call => call.method === 'GET'));
});

test('active or busy hosts cannot be deleted; activation and record deletion use explicit revision', async () => {
  const b = browser(); await b.open(state({ active_id: 'host-a', hosts: [host({ deployed: true })] }));
  assert.equal(findButton(b.$('servers-hosts'), '移除记录').disabled, true);
  b.replies.push(state({ revision: 2, hosts: [host({ deployed: true })] })); await b.click('servers-use-environment');
  assert.deepEqual(JSON.parse(JSON.stringify(b.calls.at(-1).body)), { id: '', expected_revision: 1 });
  await findButton(b.$('servers-hosts'), '移除记录').emit('click');
  assert.equal(b.calls.length, 2, 'opening confirmation does not delete');
  b.replies.push(state({ revision: 3 })); await b.submit('servers-delete-form');
  assert.deepEqual(JSON.parse(JSON.stringify(b.calls.at(-1).body)), { id: 'host-a', expected_revision: 2 });
  assert.match(b.$('servers-notice').textContent, /远端程序保持原状/);
});

test('page exit aborts a pending check, clears private form data and ignores its late reply', async () => {
  const b = browser(); await b.open(); await b.discover(); await b.trust();
  const pending = deferred(); b.replies.push(pending.promise); await b.submit('servers-host-form');
  const signal = b.calls.at(-1).signal; b.controller.setVisible(false);
  assert.equal(signal.aborted, true); assert.equal(b.$('servers-password').value, ''); assert.equal(b.$('servers-host').value, '');
  pending.resolve(check()); await settle(); assert.equal(b.$('servers-check-panel').hidden, true);
  await b.open(state()); assert.equal(b.$('servers-save').disabled, true);
});

test('logout rejects late state and operation replies from the previous session', async () => {
  const b = browser(); await b.open();
  const pending = deferred(); b.replies.push(pending.promise); const reading = b.controller.refresh(); await settle();
  const signal = b.calls.at(-1).signal; b.logout(); assert.equal(signal.aborted, true);
  pending.resolve(state({ hosts: [host({ name: 'late private host' })] })); await reading;
  assert.doesNotMatch(b.$('servers-hosts').textContent, /late private host/); assert.equal(b.timers.size, 0);
  b.login(); await b.open(); await b.discover(); await b.trust();
  const checking = deferred(); b.replies.push(checking.promise); await b.submit('servers-host-form'); b.logout(); b.login(); await b.open();
  checking.resolve(check()); await settle(); assert.equal(b.$('servers-check-panel').hidden, true);
});

test('state failure stops polling and disables writes while retaining explicit stale-state notice', async () => {
  const b = browser(); await b.open(state({ jobs: [job()] }));
  b.replies.push(new Error('offline')); await b.fireTimer();
  assert.equal(b.timers.size, 0); assert.equal(b.$('servers-discover').disabled, true);
  assert.match(b.$('servers-notice').textContent, /上次结果/);
});
