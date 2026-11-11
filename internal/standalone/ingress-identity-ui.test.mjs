import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

// Isolated DOM/state checks, not a browser-rendering or visual QA claim.
const source = await readFile(new URL('./web/ingress.js', import.meta.url), 'utf8');
const markup = await readFile(new URL('./web/index.html', import.meta.url), 'utf8');
const instanceID = '0123456789abcdef0123456789abcdef';
const publicKey = Buffer.from(new Uint8Array(32).fill(255)).toString('base64');
const state = extra => ({ configured: true, revision: 4, enabled: true, default_priority: 100, canonical_url: 'https://example.test/salcara-hub', hub_instance_id: instanceID, identity_public_key: publicKey, endpoints: [{ id: 'edge-a', name: 'fixture edge', url: 'https://edge.example.test/salcara-hub', enabled: true, priority: 200, status: 'verified' }], ...extra });
const settle = async () => { for (let i = 0; i < 5; i++) await new Promise(resolve => setImmediate(resolve)); };
function deferred() { let resolve; const promise = new Promise(done => { resolve = done; }); return { promise, resolve }; }
class Element {
  constructor(tag = 'div') { this.tagName = tag.toUpperCase(); this.children = []; this.handlers = new Map(); this.value = ''; this.content = ''; this.open = false; this.hidden = false; this.disabled = false; }
  set textContent(value) { this.content = String(value); this.children = []; }
  get textContent() { return this.content + this.children.map(child => child.textContent).join(''); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.content = ''; this.children = children; }
  addEventListener(type, handler) { this.handlers.set(type, handler); }
  async emit(type) { if (!this.disabled) this.handlers.get(type)?.({ preventDefault() {} }); await settle(); }
}
function fixture() {
  const ids = new Map([...markup.matchAll(/id="([^"]+)"/g)].map(m => [m[1], new Element()]));
  const replies = [], calls = [], events = new Map(), timers = new Set();
  let epoch = 1, authenticated = true;
  const document = { visibilityState: 'visible', getElementById: id => { assert.ok(ids.has(id), id); return ids.get(id); }, createElement: tag => new Element(tag), addEventListener: (type, handler) => events.set(type, handler) };
  const window = {};
  vm.runInContext(source, vm.createContext({ window, document, AbortController, TextEncoder, Date, setTimeout: fn => { timers.add(fn); return fn; }, clearTimeout: fn => timers.delete(fn) }), { filename: 'ingress.js' });
  const api = window.SalcaraIngress({
    request: async (...args) => { calls.push(args); assert.ok(replies.length, 'unexpected request'); const value = await replies.shift(); if (value instanceof Error) throw value; return value; },
    getEpoch: () => epoch, isAuthenticated: () => authenticated, isBusy: () => false,
  });
  const details = ids.get('ingress-form').children.find(el => el.tagName === 'DETAILS');
  assert.ok(details, 'identity must stay inside the existing ingress surface');
  const fields = details.children[1], instance = fields.children[0].children[1], key = fields.children[1].children[1], note = details.children[2];
  return { api, ids, replies, calls, timers, details, fields, instance, key, note,
    async open(value = state()) { replies.push(value); api.setVisible(true); await settle(); },
    logout() { epoch++; authenticated = false; api.reset(); },
    async hide() { document.visibilityState = 'hidden'; events.get('visibilitychange')(); await settle(); },
  };
}

test('identity is collapsed by default and shows complete, unchanged read-only server values', async () => {
  const b = fixture(); assert.equal(b.details.open, false); assert.equal(b.instance.value, ''); assert.equal(b.key.value, '');
  await b.open(state({ signing_seed: 'fixture-private-seed-do-not-render', private_key: 'fixture-private-key-do-not-render' }));
  assert.equal(b.details.children[0].textContent, '核对手机连接身份');
  assert.equal(b.details.open, false); assert.equal(b.fields.hidden, false);
  assert.equal(b.instance.value, instanceID); assert.equal(b.key.value, publicKey);
  assert.equal(b.instance.readOnly, true); assert.equal(b.key.readOnly, true);
  assert.equal(b.instance.tagName, 'TEXTAREA'); assert.equal(b.key.tagName, 'TEXTAREA');
  assert.match(b.note.textContent, /可信渠道.*逐字核对/);
  assert.doesNotMatch(b.details.textContent + b.instance.value + b.key.value, /fixture-private|…/);
  assert.equal(b.calls.length, 1); assert.equal(b.calls[0][0], 'ingress/state'); assert.equal(b.timers.size, 0);
  assert.doesNotMatch(source, /crypto\.subtle|createHash|innerHTML/);
});

test('unconfigured, incomplete or non-string identity never leaves old values or creates placeholders', async () => {
  for (const invalid of [{ configured: false }, { hub_instance_id: '' }, { identity_public_key: undefined }, { identity_public_key: 123 }, { hub_instance_id: null }]) {
    const b = fixture(); await b.open(); b.details.open = true;
    b.replies.push(state(invalid)); await b.api.refresh();
    assert.equal(b.details.open, false); assert.equal(b.fields.hidden, true);
    assert.equal(b.instance.value, ''); assert.equal(b.key.value, ''); assert.match(b.note.textContent, /未配置或不可用/);
  }
});

test('reset, logout and hidden-page transitions collapse and clear the identity fields', async () => {
  for (const exit of [b => b.api.reset(), b => b.logout(), b => b.api.setVisible(false), b => b.hide()]) {
    const b = fixture(); await b.open(); b.details.open = true; await exit(b);
    assert.equal(b.details.open, false); assert.equal(b.fields.hidden, true);
    assert.equal(b.instance.value, ''); assert.equal(b.key.value, '');
  }
});

test('late state and mutation replies after logout cannot reveal identity again', async () => {
  for (const mutate of [false, true]) {
    const b = fixture(); await b.open(); const pending = deferred(); b.replies.push(pending.promise);
    const operation = mutate ? b.ids.get('ingress-verify').emit('click') : b.api.refresh();
    await settle(); b.logout(); pending.resolve(state()); await operation; await settle();
    assert.equal(b.instance.value, ''); assert.equal(b.key.value, ''); assert.equal(b.fields.hidden, true);
  }
});

test('a late reply from a previous visit cannot replace the current full identity', async () => {
  const b = fixture(), old = deferred(); b.replies.push(old.promise); b.api.setVisible(true); await settle();
  b.api.setVisible(false);
  const nextID = 'a'.repeat(32), nextKey = Buffer.from(new Uint8Array(32).fill(17)).toString('base64');
  await b.open(state({ hub_instance_id: nextID, identity_public_key: nextKey }));
  old.resolve(state()); await settle();
  assert.equal(b.instance.value, nextID); assert.equal(b.key.value, nextKey); assert.equal(b.details.open, false);
});

test('a failed or malformed state clears the old identity instead of leaving it available to compare', async () => {
  for (const response of [new Error('read failed'), state({ revision: undefined })]) {
    const b = fixture(); await b.open(); b.details.open = true; b.replies.push(response); await b.api.refresh();
    assert.equal(b.instance.value, ''); assert.equal(b.key.value, ''); assert.equal(b.details.open, false);
    assert.match(b.note.textContent, /读取失败/);
  }
});
