import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

// In-memory DOM checks only: this does not claim rendered/browser validation.
const source = await readFile(new URL('./web/app.js', import.meta.url), 'utf8');
const storeSource = await readFile(new URL('./web/bot-reset-store.js', import.meta.url), 'utf8');
const markup = await readFile(new URL('./web/index.html', import.meta.url), 'utf8');
const groupId = 'bot-fixture-group';
const revision = 'a'.repeat(32);
const reply = (data, status = 200) => ({ ok: status >= 200 && status < 300, status, json: async () => data });
const result = () => ({ ok: true, groupId, phoneRevision: 'b'.repeat(32), binding: { type: 'salcara-bot-binding', version: 1, hubUrl: 'https://example.test/salcara-hub', groupId, invite: 'fixture-private-invite', signature: 'fixture-private-signature' } });
const settle = async () => { for (let i = 0; i < 5; i++) await new Promise(resolve => setImmediate(resolve)); };
function deferred() { let resolve; const promise = new Promise(r => { resolve = r; }); return { promise, resolve }; }
class Element {
  constructor(tag = 'div') { this.tagName = tag.toUpperCase(); this.children = []; this.handlers = new Map(); this.dataset = {}; this.value = ''; this.content = ''; this.disabled = false; this.open = false; this.hidden = false; this.checked = false; }
  set textContent(value) { this.content = String(value); this.children = []; }
  get textContent() { return this.content + this.children.map(c => typeof c === 'string' ? c : c.textContent).join(''); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.content = ''; this.children = children; }
  setAttribute() {}
  addEventListener(type, fn) { this.handlers.set(type, [...(this.handlers.get(type) || []), fn]); }
  showModal() { this.open = true; }
  close() { this.open = false; for (const fn of this.handlers.get('close') || []) fn({}); }
  async emit(type) { if (type === 'click' && this.disabled) return; for (const fn of this.handlers.get(type) || []) await fn({ preventDefault() {} }); await settle(); }
}
class MemoryStorage {
  constructor() { this.values = new Map(); this.reads = 0; this.failRead = false; this.failWrite = false; this.failRemove = false; this.dropWrites = false; }
  get length() { if (this.failRead) throw new Error('storage unavailable'); this.reads++; return this.values.size; }
  key(i) { this.reads++; return [...this.values.keys()][i] ?? null; }
  getItem(key) { if (this.failRead) throw new Error('storage unavailable'); this.reads++; return this.values.get(key) ?? null; }
  setItem(key, value) { if (this.failWrite) throw new Error('storage quota exceeded'); if (!this.dropWrites) this.values.set(key, value); }
  removeItem(key) { if (this.failRemove) throw new Error('storage remove unavailable'); this.values.delete(key); }
}
async function fixture({ role = 'owner', actor = 'owner', bot = true, storage = new MemoryStorage(), idOffset = 0 } = {}) {
  const ids = new Map([...markup.matchAll(/id="([^"]+)"/g)].map(m => [m[1], new Element()]));
  const b = { ids, storage, role, actor, groups: [{ groupId, phoneRevision: revision, phoneBound: true, computers: 2 }], requests: [], confirmations: [], events: new Map(), resets: 0, response: async () => reply(result()) };
  const state = { stats: {}, devices: [{ ref: 'd'.repeat(64), name: 'fixture computer', paired: true, online: true }], audit: [], total: 1, deployment_mode: bot ? 'bot' : 'standalone', resource_modes: [], update_configured: false };
  let serial = idOffset;
  const context = vm.createContext({
    document: { getElementById: id => { assert.ok(ids.has(id), id); return ids.get(id); }, createElement: tag => new Element(tag) },
    window: { location: { origin: 'https://example.test' }, localStorage: storage }, navigator: { clipboard: { writeText: async () => {} } },
    crypto: { randomUUID: () => `00000000-0000-4000-8000-${String(++serial).padStart(12, '0')}` },
    fetch: async (path, options) => {
      b.requests.push({ path: String(path), ...options });
      if (path.endsWith('/auth/session') || path.endsWith('/auth/login')) return reply({ csrf_token: 'c'.repeat(43), role: b.role, actor: b.actor });
      if (path.endsWith('/auth/logout')) return reply({ ok: true });
      if (path.includes('/state?')) return reply(state);
      if (path.endsWith('/bot/invites') && options.method === 'GET') return reply({ groups: b.groups });
      if (path.endsWith('/bot/phone/reset')) { b.resets++; return b.response(); }
      if (path.endsWith('/bot/invites/revoke')) { b.groups[0].revoked = true; return reply({ ok: true }); }
      throw new Error(`unexpected request ${path}`);
    },
    confirm: message => { b.confirmations.push(message); return true; },
    AbortController, TextEncoder, URLSearchParams, Date,
    setTimeout: () => ({}), clearTimeout() {}, addEventListener: (event, fn) => b.events.set(event, fn),
  });
  vm.runInContext(storeSource, context, { filename: 'bot-reset-store.js' });
  vm.runInContext(source, context, { filename: 'app.js' });
  await settle();
  b.button = label => {
    const visit = node => { if (node.tagName === 'BUTTON' && node.textContent === label) return node; for (const child of node.children || []) { const found = visit(child); if (found) return found; } };
    return visit(ids.get('bot-groups'));
  };
  b.open = async () => { const button = b.button('重置手机并更新绑定包') || b.button('重试本次手机重置'); assert.ok(button); await button.emit('click'); };
  b.confirm = async () => { ids.get('bot-reset-reason').value = 'replace lost fixture phone'; ids.get('bot-reset-confirm').checked = true; await ids.get('bot-reset-form').emit('submit'); };
  b.resetRequests = () => b.requests.filter(r => r.path.endsWith('/bot/phone/reset'));
  return b;
}

test('reset requires reason and explicit confirmation, submits exact group revision, displays only replacement package', async () => {
  const b = await fixture(); await b.open();
  assert.equal(b.ids.get('bot-reset-target').textContent, `原组：${groupId}`);
  await b.ids.get('bot-reset-form').emit('submit'); assert.equal(b.resets, 0);
  b.ids.get('bot-reset-reason').value = 'reason'; await b.ids.get('bot-reset-form').emit('submit'); assert.equal(b.resets, 0);
  await b.confirm();
  const request = b.resetRequests()[0], body = JSON.parse(request.body);
  assert.deepEqual(body, { groupId, expectedRevision: revision, requestId: '00000000-0000-4000-8000-000000000001', confirm: true, reason: 'replace lost fixture phone' });
  assert.equal(request.headers['X-Salcara-CSRF'], 'c'.repeat(43));
  assert.deepEqual(JSON.parse(b.ids.get('bot-binding-value').value), result().binding);
  assert.equal(b.ids.get('bot-reset-dialog').open, false);
  assert.equal(b.storage.values.size, 0, 'confirmed success must remove recovery metadata');
  assert.match(b.ids.get('notice').textContent, /电脑及历史保留/);
  assert.doesNotMatch(source, /localStorage|sessionStorage|setInterval/);
});

test('503 and unknown responses retain the same immutable request through refresh, closing and reopening', async () => {
  for (const response of [() => reply({ error: 'bot_phone_reset_pending' }, 503), () => { throw new Error('lost response'); }, () => reply({ ok: true })]) {
    const b = await fixture(); b.response = response; await b.open(); await b.confirm();
    const first = b.resetRequests()[0].body;
    assert.equal(b.ids.get('bot-reset-dialog').open, true);
    assert.equal(b.ids.get('bot-reset-reason').disabled, true);
    assert.match(b.ids.get('bot-reset-notice').textContent, /同一请求/);
    await b.ids.get('bot-reset-cancel').emit('click');
    b.groups[0].phoneRevision = 'c'.repeat(32); b.groups[0].phonePending = true;
    await b.ids.get('refresh').emit('click'); await b.open();
    assert.equal(b.ids.get('bot-reset-confirm').disabled, true);
    b.response = async () => reply(result()); await b.ids.get('bot-reset-form').emit('submit');
    assert.equal(b.resetRequests()[1].body, first);
    assert.equal(b.ids.get('bot-binding-result').hidden, false);
  }
});

test('definitive revision conflict ends the old attempt, a new one requires explicit confirmation against fresh state', async () => {
  const b = await fixture(); b.response = () => { b.groups[0].phoneRevision = 'c'.repeat(32); return reply({ error: 'bot_phone_revision_conflict' }, 409); };
  await b.open(); await b.confirm();
  assert.equal(b.ids.get('bot-reset-dialog').open, false); assert.equal(b.resets, 1);
  await b.open(); assert.equal(b.ids.get('bot-reset-confirm').checked, false);
  b.response = () => reply(result()); await b.confirm();
  const [first, second] = b.resetRequests().map(r => JSON.parse(r.body));
  assert.notEqual(first.requestId, second.requestId); assert.equal(second.expectedRevision, 'c'.repeat(32));
});

test('leaving during an in-flight reset clears all secrets and late responses cannot repaint', async () => {
  const b = await fixture(), pending = deferred(); b.response = () => pending.promise;
  await b.open(); const submitted = b.confirm(); await settle();
  b.events.get('pagehide')(); pending.resolve(reply(result())); await submitted;
  assert.equal(b.ids.get('bot-binding-value').value, ''); assert.equal(b.ids.get('bot-binding-result').hidden, true);
  assert.equal(b.ids.get('bot-reset-reason').value, ''); assert.equal(b.ids.get('bot-reset-dialog').open, false);
  assert.equal(b.ids.get('bot-groups').children.length, 0);
  assert.equal(b.storage.values.size, 1, 'pagehide and late responses must not erase recovery metadata');
});

test('invite-only revoke preserves existing permission wording; ordinary QR revoke is not available in Bot mode', async () => {
  const b = await fixture();
  assert.doesNotMatch(b.ids.get('devices').textContent, /撤销绑定/);
  await b.button('停止新增绑定').emit('click');
  assert.match(b.confirmations[0], /已有电脑和手机权限保留/);
  assert.match(b.ids.get('notice').textContent, /已有电脑和手机权限保留/);
  const normal = await fixture({ bot: false }); assert.match(normal.ids.get('devices').textContent, /撤销绑定/);
});

test('viewer cannot read groups or trigger reset, and pending groups do not create new reset requests', async () => {
  const viewer = await fixture({ role: 'viewer' }); await viewer.ids.get('bot-reset-form').emit('submit');
  assert.equal(viewer.resets, 0); assert.equal(viewer.ids.get('bot-panel').hidden, true);
  assert.equal(viewer.requests.some(r => r.path.endsWith('/bot/invites')), false);
  const b = await fixture(); b.groups[0].phonePending = true; await b.ids.get('refresh').emit('click');
  assert.equal(b.button('重置手机并更新绑定包').disabled, true);
});

test('reload and owner reauthentication recover the exact persisted request without storing binding secrets', async () => {
  const storage = new MemoryStorage(), first = await fixture({ storage });
  first.response = () => reply({ error: 'bot_phone_reset_pending' }, 503);
  await first.open(); await first.confirm();
  const sent = first.resetRequests()[0].body;
  assert.equal(storage.values.size, 1);
  for (const raw of storage.values.values()) {
    assert.deepEqual(Object.keys(JSON.parse(raw)).sort(), ['confirm','expectedRevision','groupId','reason','requestId'].sort());
    assert.doesNotMatch(raw, /invite|signature|botToken|phoneToken|csrf/);
  }
  // Logout clears the screen, not the durable recovery metadata.
  await first.ids.get('logout').emit('click'); assert.equal(storage.values.size, 1);
  assert.equal(first.ids.get('bot-reset-reason').value, '');
  const reads = storage.reads, viewer = await fixture({ storage, role: 'viewer', actor: 'viewer-fixture' });
  assert.equal(storage.reads, reads, 'viewer must not read owner recovery metadata');
  assert.equal(viewer.ids.get('bot-panel').hidden, true);
  const recovered = await fixture({ storage }); recovered.groups[0].phonePending = true; recovered.groups[0].phoneRevision = 'c'.repeat(32);
  await recovered.ids.get('refresh').emit('click');
  assert.ok(recovered.button('重试本次手机重置')); await recovered.open();
  assert.equal(recovered.ids.get('bot-reset-reason').value, 'replace lost fixture phone');
  await recovered.ids.get('bot-reset-form').emit('submit');
  assert.equal(recovered.resetRequests()[0].body, sent); assert.equal(storage.values.size, 0);
});

test('storage read, write and readback failures prevent the first reset POST', async () => {
  for (const property of ['failRead', 'failWrite', 'dropWrites']) {
    const storage = new MemoryStorage(); storage[property] = true;
    const b = await fixture({ storage }); await b.open(); await b.confirm();
    assert.equal(b.resets, 0, property); assert.match(b.ids.get('bot-reset-notice').textContent, /本次请求尚未发送/);
    storage[property] = false; await b.confirm(); assert.equal(b.resets, 1, 'retry may start only after persistence works');
  }
});

test('multiple request keys survive independently and definitive conflicts remove only the selected request', async () => {
  const storage = new MemoryStorage(), first = await fixture({ storage });
  first.response = () => reply({ error: 'bot_phone_reset_pending' }, 503); await first.open(); await first.confirm();
  const [originalKey, originalRaw] = [...storage.values][0], another = JSON.parse(originalRaw);
  another.requestId = '00000000-0000-4000-8000-000000000002';
  const anotherKey = originalKey.replace('00000000-0000-4000-8000-000000000001', another.requestId);
  storage.setItem(anotherKey, JSON.stringify(another));
  const recovered = await fixture({ storage }); recovered.response = () => reply({ error: 'bot_phone_revision_conflict' }, 409);
  await recovered.open(); await recovered.ids.get('bot-reset-form').emit('submit');
  assert.equal(storage.values.has(originalKey), false); assert.equal(storage.values.has(anotherKey), true);
  await recovered.open(); recovered.response = () => reply(result()); await recovered.ids.get('bot-reset-form').emit('submit');
  assert.equal(JSON.parse(recovered.resetRequests()[1].body).requestId, another.requestId); assert.equal(storage.values.size, 0);
});

test('a different owner scope cannot display or overwrite another owner recovery request', async () => {
  const storage = new MemoryStorage(), first = await fixture({ storage }); first.response = () => reply({}, 503);
  await first.open(); await first.confirm(); const original = [...storage.values];
  const other = await fixture({ storage, actor: 'other-owner', idOffset: 10 });
  assert.ok(other.button('重置手机并更新绑定包')); assert.equal(other.button('重试本次手机重置'), undefined);
  await other.open(); await other.confirm(); assert.deepEqual([...storage.values], original);
});

test('failed cleanup keeps a recoverable request but never persists the successful replacement package', async () => {
  const storage = new MemoryStorage(), b = await fixture({ storage }); storage.failRemove = true;
  await b.open(); await b.confirm();
  assert.equal(b.ids.get('bot-binding-result').hidden, false); assert.equal(storage.values.size, 1);
  assert.match(b.ids.get('notice').textContent, /暂未清除恢复记录/);
  for (const raw of storage.values.values()) assert.doesNotMatch(raw, /fixture-private|invite|signature/);
  storage.failRemove = false; const recovered = await fixture({ storage }); await recovered.open(); await recovered.ids.get('bot-reset-form').emit('submit');
  assert.equal(storage.values.size, 0);
});

test('a saved request whose group is absent can still receive a definitive 404 and clear only its metadata', async () => {
  const storage = new MemoryStorage(), first = await fixture({ storage }); first.response = () => reply({}, 503);
  await first.open(); await first.confirm();
  const recovered = await fixture({ storage }); recovered.groups = []; await recovered.ids.get('refresh').emit('click');
  assert.match(recovered.ids.get('bot-groups').textContent, /最新组状态未找到/);
  recovered.response = () => reply({ error: 'bot_group_not_found' }, 404);
  await recovered.open(); await recovered.ids.get('bot-reset-form').emit('submit');
  assert.equal(storage.values.size, 0); assert.equal(recovered.resets, 1);
});
