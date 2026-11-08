import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const root = new URL('../../internal/standalone/web/', import.meta.url);
const source = await readFile(new URL('help.js', root), 'utf8');
const sourcesModule = await readFile(new URL('public-sources.js', root), 'utf8');
const markup = await readFile(new URL('help.html', root), 'utf8');
const response = (data = { service: 'salcara-hub', protocol: 'salcara-remote', protocolVersion: 1, version: '0.1.0', standalone_version: '0.5.2' }) => ({ ok: true, json: async () => data });
function deferred() { let resolve; const promise = new Promise(yes => { resolve = yes; }); return { promise, resolve }; }
async function settle() { for (let i = 0; i < 5; i++) await new Promise(resolve => setImmediate(resolve)); }

function browser({ hidden = false, online = true, initial = response(), clipboard = true } = {}) {
  const elements = new Map([...markup.matchAll(/id="([^"]+)"/g)].map(match => [match[1], { textContent: '', disabled: false, handlers: new Map(), addEventListener(type, fn) { this.handlers.set(type, fn); } }]));
  const timers = new Set(), requests = [], replies = [initial], copies = [], events = new Map(), documentEvents = new Map();
  const document = { hidden, getElementById: id => { assert.ok(elements.has(id), `unknown HTML id: ${id}`); return elements.get(id); }, addEventListener: (name, fn) => documentEvents.set(name, fn) };
  const navigator = { onLine: online, clipboard: clipboard ? { writeText: async value => copies.push(value) } : undefined };
  const context = vm.createContext({
    document, navigator, URL, AbortController, location: { href: 'https://name:secret@example.test/salcara-hub/?auth_token=private#private' },
    fetch: (path, options) => { requests.push({ path, ...options }); assert.ok(replies.length, 'unexpected request'); return Promise.resolve(replies.shift()); },
    setTimeout: (callback, delay) => { const timer = { callback, delay }; timers.add(timer); return timer; },
    clearTimeout: timer => timers.delete(timer), addEventListener: (name, fn) => events.set(name, fn),
  });
  vm.runInContext(sourcesModule, context, { filename: 'public-sources.js' });
  vm.runInContext(source, context, { filename: 'help.js' });
  return {
    $: id => elements.get(id), requests, replies, copies, timers, document, navigator,
    async click(id) { const el = elements.get(id); if (!el.disabled) el.handlers.get('click')(); await settle(); },
    async emit(name) { events.get(name)?.(); await settle(); },
    async visibility(value) { document.hidden = value; documentEvents.get('visibilitychange')(); await settle(); },
    async deadline() { const timer = [...timers].find(value => value.delay === 10000); assert.ok(timer); timers.delete(timer); timer.callback(); await settle(); },
  };
}

test('homepage copies a clean local Hub URL and performs only one initial public check', async () => {
  const b = browser(); await settle();
  assert.equal(b.$('hub-url').textContent, 'https://example.test/salcara-hub/');
  await b.click('copy-hub-url');
  assert.deepEqual(b.copies, ['https://example.test/salcara-hub/']);
  assert.match(b.$('copy-status').textContent, /已复制/);
  assert.equal(b.$('health').textContent, 'Hub 可连接');
  assert.match(b.$('status').textContent, /v0\.5\.2/);
  assert.doesNotMatch(b.$('status').textContent, /v0\.1\.0/);
  assert.equal(b.requests[0].path, './v1/ping');
  assert.equal(b.requests[0].credentials, 'omit');
  assert.equal(b.requests[0].cache, 'no-store');
  assert.equal(b.requests[0].redirect, 'error');
  assert.equal(b.requests[0].referrerPolicy, 'no-referrer');
  assert.equal(b.requests.length, 1);
  assert.equal(b.timers.size, 0);
  await b.visibility(true); await b.visibility(false); await b.emit('pageshow');
  assert.equal(b.requests.length, 1, 'completed homepage checks must not poll');
});

test('clipboard denial offers manual copying without claiming success', async () => {
  const b = browser({ clipboard: false }); await settle(); await b.click('copy-hub-url');
  assert.match(b.$('copy-status').textContent, /手动复制/);
  assert.doesNotMatch(b.$('copy-status').textContent, /已复制/);
  assert.equal(b.$('copy-hub-url').disabled, false);
});

test('homepage validates the Hub protocol and never displays raw unexpected response content', async () => {
  for (const initial of [response(null), response({ service: 'private-upstream' }), { ok: true, json: async () => { throw new Error('private-upstream'); } }]) {
    const b = browser({ initial }); await settle();
    assert.notEqual(b.$('health').textContent, 'Hub 可连接');
    assert.doesNotMatch(b.$('status').textContent, /private-upstream/);
    assert.equal(b.$('check').disabled, false);
    assert.equal(b.timers.size, 0);
  }
});

test('homepage deadline covers the body, ignores late results and permits a fresh manual check', async () => {
  const body = deferred();
  const b = browser({ initial: { ok: true, json: () => body.promise } }); await settle();
  await b.click('check'); assert.equal(b.requests.length, 1);
  await b.deadline();
  assert.equal(b.$('health').textContent, '检测超时');
  assert.equal(b.requests[0].signal.aborted, true);
  assert.equal(b.$('check').disabled, false);
  body.resolve(await response().json()); await settle();
  assert.equal(b.$('health').textContent, '检测超时');
  b.replies.push(response()); await b.click('check');
  assert.equal(b.$('health').textContent, 'Hub 可连接');
  assert.equal(b.requests.length, 2);
  assert.equal(b.timers.size, 0);
});

test('background pages cancel unfinished checks and resume exactly once', async () => {
  const pending = deferred(); const b = browser({ initial: pending.promise }); await settle();
  await b.visibility(true);
  assert.equal(b.requests[0].signal.aborted, true);
  assert.equal(b.timers.size, 0);
  assert.equal(b.$('check').disabled, true);
  pending.resolve(response()); await settle();
  assert.equal(b.$('health').textContent, '已暂停');
  b.replies.push(response()); await b.visibility(false); await b.visibility(false);
  assert.equal(b.requests.length, 2);
  assert.equal(b.timers.size, 0);
});

test('hidden or offline startup waits for eligibility and back-forward navigation resumes safely', async () => {
  const b = browser({ hidden: true, online: false }); await settle();
  assert.equal(b.requests.length, 0);
  await b.visibility(false); assert.equal(b.requests.length, 0);
  b.navigator.onLine = true; await b.emit('online'); await b.emit('online');
  assert.equal(b.requests.length, 1);
  await b.emit('pagehide'); assert.equal(b.timers.size, 0);
  b.replies.push(response()); await b.emit('pageshow'); await b.emit('pageshow');
  assert.equal(b.requests.length, 2);
  assert.equal(b.timers.size, 0);
});

test('homepage uses discovery source status, marks restored information as cached and never polls extra endpoints', async () => {
  const payload = await response().json();
  payload.source_status = ['api', 'external_api', 'reverse_proxy', 'official_account'].map(id => ({ id, status: 'allowlist' }));
  const b = browser({ initial: response(payload) }); await settle();
  assert.equal(b.$('source-external_api-status').textContent, '按名单开放');
  assert.equal(b.$('sources-refresh').textContent, '本站当前授权');
  await b.visibility(true); await b.visibility(false);
  assert.equal(b.$('source-external_api-status').textContent, '按名单开放');
  assert.equal(b.$('sources-refresh').textContent, '上次检测授权');
  assert.equal(b.requests.length, 1);
  assert.equal(b.timers.size, 0);
  b.replies.push(response(null)); await b.click('check');
  assert.equal(b.$('source-external_api-status').textContent, '待重新确认');
});
