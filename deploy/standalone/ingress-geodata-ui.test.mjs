import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';
const web = new URL('../../internal/standalone/web/', import.meta.url);
const markup = await readFile(new URL('index.html', web), 'utf8');
const scripts = Object.fromEntries(await Promise.all(['ingress', 'geodata'].map(async name => [name, await readFile(new URL(name + '.js', web), 'utf8')])));
async function settle() { for (let i = 0; i < 8; i++) await new Promise(resolve => setImmediate(resolve)); }
function deferred() { let resolve; const promise = new Promise(r => resolve = r); return { promise, resolve }; }
class Element {
  constructor(tag = 'div') { this.tagName = tag; this.value = ''; this.checked = false; this.children = []; this.handlers = new Map(); this.content = ''; }
  set textContent(value) { this.content = String(value); this.children = []; }
  get textContent() { return this.content + this.children.map(e => e.textContent).join(''); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.children = children; this.content = ''; }
  addEventListener(type, callback) { this.handlers.set(type, callback); }
  async emit(type) { if (this.disabled) return; this.handlers.get(type)?.({ preventDefault() {} }); await settle(); }
}
const ingress = (extra = {}) => ({ configured: true, revision: 4, enabled: true, default_priority: 100, canonical_url: 'https://hub.example/salcara-hub/v1', endpoints: [{ id: 'entry-1', name: '备用入口', url: 'https://edge.example/salcara-hub/v1', enabled: true, priority: 200, status: 'verified' }], ...extra });
const pool = (id = 'controller-a') => ({ configured: true, controller_id: id, geo_available: true, nodes: [{ id: 'one', name: '测试节点' }] });
const geo = (extra = {}) => ({ controller_id: 'controller-a', revision: 2, enabled: true, installed: true, month: '2026-11', auto_update: true, groups: [], ...extra });
function browser(kind) {
  const ids = new Map([...markup.matchAll(/id="([^"]+)"/g)].map(m => [m[1], new Element()]));
  const replies = [], calls = [], timers = new Set(), events = new Map(); let epoch = 1, authenticated = true, refreshes = 0;
  const document = { visibilityState: 'visible', getElementById: id => { assert.ok(ids.has(id), id); return ids.get(id); }, createElement: tag => new Element(tag), addEventListener: (t, cb) => events.set(t, cb) };
  const window = {}, context = vm.createContext({ window, document, Date, URL, AbortController, TextEncoder, setTimeout: callback => { timers.add(callback); return callback; }, clearTimeout: id => timers.delete(id) });
  vm.runInContext(scripts[kind], context);
  const api = window[kind === 'ingress' ? 'SalcaraIngress' : 'SalcaraGeodata']({ request: async (...args) => { calls.push(args); assert.ok(replies.length, 'unexpected request'); const value = await replies.shift(); if (value instanceof Error) throw value; return value; }, getEpoch: () => epoch, isAuthenticated: () => authenticated, isBusy: () => false, refreshPool: () => { refreshes++; } });
  return { api, calls, replies, timers, ids, document, get refreshes() { return refreshes; }, $: id => ids.get(id), async open(value = kind === 'ingress' ? ingress() : geo()) { replies.push(value); if (kind === 'ingress') api.setVisible(true); else api.setContext(pool(), true); await settle(); }, async visibility(hidden) { document.visibilityState = hidden ? 'hidden' : 'visible'; events.get('visibilitychange')?.(); await settle(); }, logout() { epoch++; authenticated = false; api.reset(); } };
}
test('ingress uses a separate CAS revision and does not publish unsaved edits', async () => {
  const b = browser('ingress'); await b.open();
  await b.$('ingress-add').emit('click');
  assert.equal(b.$('ingress-list').children.length, 2); assert.equal(b.$('ingress-verify').disabled, true);
  const fields = b.$('ingress-list').children[1].children[1].children;
  fields[0].children[1].value = '入口二'; fields[1].children[1].value = 'https://backup.example/salcara-hub';
  b.replies.push(ingress({ revision: 5 })); await b.$('ingress-form').emit('submit');
  assert.equal(b.calls[1][0], 'ingress/save'); assert.equal(b.calls[1][2].expected_revision, 4); assert.equal(b.calls[1][2].endpoints.length, 2);
  assert.equal(b.calls[1][2].endpoints[1].url, 'https://backup.example/salcara-hub');
});
test('ingress lost mutation result locks writes and never retries POST', async () => {
  const b = browser('ingress'); await b.open(); b.replies.push(new Error('timeout')); await b.$('ingress-verify').emit('click');
  assert.equal(b.calls.filter(c => c[1] === 'POST').length, 1); assert.equal(b.$('ingress-verify').disabled, true); assert.equal(b.timers.size, 0);
  assert.match(b.$('ingress-notice').textContent, /重新读取/);
});
test('ingress enabled without verified endpoints does not claim availability', async () => {
  const b = browser('ingress'); await b.open(ingress({ endpoints: [] }));
  assert.match(b.$('ingress-status').textContent, /等待可用入口/);
});
test('ingress unconfigured HTTPS and stale logout replies cannot enable writes', async () => {
  const b = browser('ingress'); await b.open(ingress({ configured: false })); assert.equal(b.$('ingress-add').disabled, true);
  const pending = deferred(); b.replies.push(pending.promise); void b.api.refresh(); await settle(); b.logout(); pending.resolve(ingress()); await settle();
  assert.equal(b.$('ingress-list').children.length, 0); assert.equal(b.$('ingress-save').disabled, true);
});
test('ingress verification polling ends with real result and never continues in background', async () => {
  const b = browser('ingress'); await b.open(ingress({ verification: { running: true, total: 1, completed: 0, succeeded: 0, failed: 0 } }));
  assert.equal(b.timers.size, 1); assert.equal(b.$('ingress-save').disabled, true);
  await b.visibility(true); assert.equal(b.timers.size, 0);
  b.replies.push(ingress({ verification: { running: false, total: 1, completed: 1, succeeded: 0, failed: 1 } })); await b.visibility(false);
  assert.match(b.$('ingress-progress').textContent, /未通过 1/); assert.equal(b.timers.size, 0);
});
test('geodata target changes discard late old-controller state', async () => {
  const b = browser('geodata'), pending = deferred(); b.replies.push(pending.promise); b.api.setContext(pool(), true); await settle();
  b.replies.push(geo({ controller_id: 'controller-b', month: 'new-controller' })); b.api.setContext(pool('controller-b'), true); await settle();
  pending.resolve(geo({ month: 'old-controller' })); await settle();
  assert.match(b.$('geo-version').textContent, /new-controller/); assert.doesNotMatch(b.$('geo-version').textContent, /old-controller/);
});
test('geodata installation uses current target and independent revision, refreshes pool after completion', async () => {
  const b = browser('geodata'); await b.open();
  b.replies.push(geo({ run: { id: 'job', running: true, kind: 'install', total: 2, completed: 0 } })); await b.$('geo-install').emit('click');
  assert.equal(b.calls[1][2].expected_revision, 2); assert.equal(b.calls[1][2].expected_controller_id, 'controller-a'); assert.equal(b.$('geo-install').disabled, true);
  b.replies.push(geo({ revision: 3, run: { id: 'job', running: false, kind: 'install', total: 2, completed: 2 } }));
  const timer = b.timers.values().next().value; b.timers.delete(timer); timer(); await settle();
  assert.equal(b.refreshes, 1); assert.equal(b.timers.size, 0); assert.equal(b.$('geo-observe').disabled, false);
});
test('geodata expired data cannot start observation and unknown errors remain fixed text', async () => {
  const b = browser('geodata'); await b.open(geo({ expired: true, last_error: 'https://secret.example/token' }));
  assert.equal(b.$('geo-observe').disabled, true); assert.match(b.$('geo-status').textContent, /过期/); assert.doesNotMatch(b.$('geo-progress').textContent, /secret/);
});
test('geodata hidden pending mutation cannot strand UI or repopulate old session', async () => {
  const b = browser('geodata'); await b.open(); const pending = deferred(); b.replies.push(pending.promise); void b.$('geo-install').emit('click'); await settle();
  await b.visibility(true); b.replies.push(geo({ revision: 3 })); await b.visibility(false);
  pending.resolve(geo({ month: 'stale-result' })); await settle();
  assert.equal(b.$('geo-install').disabled, false); assert.doesNotMatch(b.$('geo-version').textContent, /stale-result/);
});
test('new modules are loaded before app use and embedded routes exist', async () => {
  assert.ok(markup.indexOf('./geodata.js') < markup.indexOf('./network.js')); assert.ok(markup.indexOf('./ingress.js') < markup.indexOf('./app.js'));
  const server = await readFile(new URL('../../internal/standalone/server.go', import.meta.url), 'utf8');
  for (const name of ['geodata', 'ingress']) assert.ok(server.includes('/admin/' + name + '.js'));
});
