import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const web = new URL('../../internal/standalone/web/', import.meta.url);
const source = await readFile(new URL('network.js', web), 'utf8');
const markup = await readFile(new URL('index.html', web), 'utf8');
// All URLs and credentials in this file are synthetic, in-memory fixture data.
const syntheticProxy = 'http://fixture-user:fixture-secret@proxy.example:8080';
const syntheticSubscription = 'https://subscription.example/private-fixture?token=fixture-token';

class Element {
  constructor(tag = 'div') {
    this.tagName = tag.toUpperCase(); this.value = ''; this.disabled = false; this.hidden = false; this.open = false;
    this.children = []; this.dataset = {}; this.attributes = {}; this.handlers = new Map(); this.content = '';
  }
  set textContent(value) { this.content = String(value); this.children = []; }
  get textContent() { return this.content + this.children.map(child => child.textContent || '').join(''); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.children = children; this.content = ''; }
  setAttribute(name, value) { this.attributes[name] = String(value); }
  addEventListener(type, handler) { const handlers = this.handlers.get(type) || []; handlers.push(handler); this.handlers.set(type, handlers); }
  showModal() { this.open = true; }
  close() { this.open = false; for (const handler of this.handlers.get('close') || []) handler({ preventDefault() {} }); }
  focus() {}
  async emit(type) {
    if (this.disabled && type === 'click') return;
    for (const handler of this.handlers.get(type) || []) await handler({ preventDefault() {} });
    await settle();
  }
}
async function settle() { for (let i = 0; i < 8; i++) await new Promise(resolve => setImmediate(resolve)); }
function deferred() { let resolve; const promise = new Promise(done => { resolve = done; }); return { promise, resolve }; }
function findButton(element, label) {
  if (element.tagName === 'BUTTON' && element.textContent === label) return element;
  for (const child of element.children || []) { const found = findButton(child, label); if (found) return found; }
  return null;
}
function findInputs(element) {
  return [...(element.tagName === 'INPUT' ? [element] : []), ...(element.children || []).flatMap(findInputs)];
}
const node = (extra = {}) => ({
  id: 'node-a', name: 'Fixture node A', proxy_url: 'socks5://redacted@proxy.example:1080', has_credentials: true,
  enabled: true, health: { status: 'healthy', latency_ms: 21.5, checked_at: Date.now() }, ...extra,
});
const state = (extra = {}) => ({
  configured: true, controller_id: 'fixture-controller-a', revision: 4, origin: 'https://hub.example/salcara-hub', mode: 'manual', selected_id: 'node-a',
  active_id: 'node-a', status: 'ready', durability: 'confirmed', probe_running: false, nodes: [node()], events: [], ...extra,
});
const releaseState = (extra = {}) => state({ release: { trial: null, previews: [], history: [], last_error: '', ...extra } });
const preview = (extra = {}) => ({
  format: 'clash', total: 2, supported: 1, unsupported: 1,
  nodes: [{ index: 0, name: 'Supported HTTP', protocol: 'http', supported: true }, { index: 1, name: 'Unsupported SS', protocol: 'ss', supported: false, reason: '需要配置支持此协议的内核' }],
  warnings: [], preview_id: 'fixture-preview-1', expires_at: Date.now() + 60000, ...extra,
});

function browser() {
  const ids = new Map([...markup.matchAll(/id="([^"]+)"/g)].map(match => [match[1], new Element()]));
  const replies = [], calls = [], timers = new Set(), events = new Map();
  let epoch = 1, authenticated = true;
  const window = {};
  const document = {
    visibilityState: 'visible',
    getElementById: id => { assert.ok(ids.has(id), `unknown HTML id ${id}`); return ids.get(id); },
    createElement: tag => new Element(tag),
    addEventListener: (type, handler) => events.set(type, handler),
  };
  const context = vm.createContext({
    window, document, URL, Date, AbortController, TextEncoder,
    setTimeout: (callback, delay) => { const timer = { callback, delay }; timers.add(timer); return timer; },
    clearTimeout: timer => timers.delete(timer),
  });
  vm.runInContext(source, context, { filename: 'network.js' });
  const controller = window.SalcaraNetwork({
    request: async (path, method = 'GET', body, anonymous = false, signal) => {
      calls.push({ path, method, body, anonymous, signal });
      if (path === 'network/observability' && !replies.length) return observations({ samples: [] });
      assert.ok(replies.length, `unexpected request: ${path}`);
      // Deliberately allow an aborted reply to resolve: session/view guards must also reject stale data.
      const reply = await replies.shift();
      if (reply instanceof Error) throw reply;
      return reply;
    },
    getEpoch: () => epoch,
    isAuthenticated: () => authenticated,
    isBusy: () => false,
  });
  return {
    ids, calls, replies, timers, controller, document,
    $: id => ids.get(id),
    async open(value = state()) { replies.push(value); controller.setVisible(true); await settle(); },
    async click(id) { await ids.get(id).emit('click'); },
    async submit(id) { await ids.get(id).emit('submit'); },
    async refresh(value = state()) { replies.push(value); await controller.refresh(); await settle(); },
    async fireTimer() { const timer = timers.values().next().value; assert.ok(timer, 'expected active timer'); timers.delete(timer); timer.callback(); await settle(); },
    async visibility(hidden) { document.visibilityState = hidden ? 'hidden' : 'visible'; events.get('visibilitychange')?.(); await settle(); },
    logout() { epoch++; authenticated = false; controller.reset(); },
    login() { epoch++; authenticated = true; },
  };
}

test('network remains idle until opened; missing entry configuration does not enable mutations', async () => {
  const b = browser(); await settle();
  assert.equal(b.calls.length, 0); assert.equal(b.timers.size, 0);
  await b.open({ configured: false, status: 'unconfigured', reason: '入口尚未配置' });
  assert.match(b.$('network-status').textContent, /未配置/);
  for (const id of ['network-add', 'network-probe', 'network-policy-save', 'network-subscription-import']) assert.equal(b.$(id).disabled, true);
  assert.equal(b.$('network-subscription-inspect').disabled, false);
  await b.click('network-add'); await b.click('network-probe');
  assert.equal(b.calls.length, 1); assert.equal(b.timers.size, 0);
});

test('an unreachable entry marks retained state stale and disables writes', async () => {
  const b = browser(); await b.open();
  assert.match(b.$('network-nodes').textContent, /21.5 ms/);
  await b.refresh(new Error('入口连接失败'));
  assert.match(b.$('network-status').textContent, /未接通/);
  assert.match(b.$('network-reason').textContent, /上次状态/);
  assert.equal(b.$('network-add').disabled, true);
  assert.equal(b.$('network-policy-save').disabled, true);
  assert.equal(b.timers.size, 0);
});

test('new node credentials clear before the request completes and are not echoed on error', async () => {
  const b = browser(); await b.open(); await b.click('network-add');
  b.$('network-node-name').value = 'New fixture node'; b.$('network-node-url').value = syntheticProxy;
  const pending = deferred(); b.replies.push(pending.promise);
  const submit = b.submit('network-node-form'); await settle();
  assert.equal(b.$('network-node-url').value, '');
  assert.equal(b.calls.at(-1).path, 'network/nodes');
  assert.equal(b.calls.at(-1).body.node.proxy_url, syntheticProxy);
  assert.equal(b.calls.at(-1).body.expected_revision, 4);
  pending.resolve(new Error(`connection failed: ${syntheticProxy}`)); await submit;
  assert.equal(b.$('network-node-url').value, '');
  assert.doesNotMatch(b.$('network-node-notice').textContent, /fixture-user|fixture-secret/);
  assert.equal(b.calls.length, 2, 'failed writes must not auto-retry');
});

test('editing without a URL preserves credentials; selecting direct sends an explicit empty URL', async () => {
  const b = browser(); await b.open();
  await findButton(b.$('network-nodes'), '编辑').emit('click');
  b.$('network-node-name').value = 'Renamed fixture';
  b.replies.push({}, state({ revision: 5 })); await b.submit('network-node-form');
  const edit = b.calls.find(call => call.path === 'network/nodes');
  assert.equal(edit.body.node.id, 'node-a'); assert.equal(Object.hasOwn(edit.body.node, 'proxy_url'), false);
  await findButton(b.$('network-nodes'), '编辑').emit('click');
  b.$('network-node-kind').value = 'direct'; await b.$('network-node-kind').emit('change');
  b.replies.push({}, state({ revision: 6, nodes: [node({ proxy_url: '', has_credentials: false })] }));
  await b.submit('network-node-form');
  const direct = b.calls.filter(call => call.path === 'network/nodes').at(-1);
  assert.equal(direct.body.node.proxy_url, ''); assert.equal(direct.body.expected_revision, 5);
});

test('policy conflicts keep the draft and the revision it was based on', async () => {
  const b = browser(); await b.open();
  b.$('network-mode').value = 'failover'; await b.$('network-mode').emit('change');
  await b.refresh(state({ revision: 5, mode: 'latency' }));
  b.replies.push(new Error('设置已变化，请重新载入'));
  await b.submit('network-policy-form');
  assert.equal(b.calls.at(-1).body.expected_revision, 4);
  assert.equal(b.calls.at(-1).body.mode, 'failover');
  assert.equal(b.$('network-mode').value, 'failover');
  assert.equal(b.$('network-policy-reload').disabled, false);
  assert.equal(b.calls.length, 3, 'conflicting writes must not refresh or retry automatically');
  b.replies.push(state({ revision: 5, mode: 'latency' })); await b.click('network-policy-reload');
  assert.equal(b.$('network-mode').value, 'latency');
  assert.equal(b.$('network-policy-reload').disabled, true);
});

test('a policy reload preserves edits made while its response is pending', async () => {
  for (const edit of [
    { id: 'network-mode', event: 'change', mode: 'latency', selected: 'node-a' },
    { id: 'network-selected', event: 'change', mode: 'failover', selected: 'node-b' },
    { id: 'network-policy-form', event: 'input', mode: 'latency', selected: 'node-a' },
  ]) {
    const b = browser(), nodes = [node(), node({ id: 'node-b', name: 'Fixture node B' })];
    await b.open(state({ nodes }));
    b.$('network-mode').value = 'failover'; await b.$('network-mode').emit('change');
    const pending = deferred(); b.replies.push(pending.promise);
    await b.click('network-policy-reload');
    assert.equal(b.$(edit.id).disabled, false, 'editing remains available while state reloads');
    b.$('network-mode').value = edit.mode; b.$('network-selected').value = edit.selected;
    await b.$(edit.id).emit(edit.event);
    pending.resolve(state({ revision: 5, mode: 'manual', nodes })); await settle();
    assert.equal(b.$('network-mode').value, edit.mode);
    assert.equal(b.$('network-selected').value, edit.selected);
    assert.match(b.$('network-policy-current').textContent, /有未保存草稿/);
    assert.equal(b.$('network-policy-reload').disabled, false);
    b.replies.push(new Error('设置已变化，请重新载入')); await b.submit('network-policy-form');
    assert.equal(b.calls.at(-1).body.expected_revision, 4, 'new edits remain based on the original configuration');
    assert.equal(b.calls.at(-1).body.mode, edit.mode);
    assert.equal(b.calls.at(-1).body.selected_id, edit.selected);
  }
});

test('subscription inspection clears the URL immediately and does not import or switch nodes', async () => {
  const b = browser(); await b.open();
  b.$('network-subscription-url').value = syntheticSubscription;
  const pending = deferred(); b.replies.push(pending.promise);
  const inspect = b.submit('network-subscription-form'); await settle();
  assert.equal(b.$('network-subscription-url').value, '');
  assert.equal(b.calls.at(-1).path, 'network/subscriptions/inspect');
  assert.equal(b.calls.at(-1).body.url, syntheticSubscription);
  pending.resolve(preview()); await inspect;
  assert.match(b.$('network-subscription-summary').textContent, /不支持 1 个/);
  assert.match(b.$('network-subscription-selection').textContent, /协议兼容 1 个 \/ 已选 1 个 \/ 可新增 31 个/);
  assert.match(b.$('network-subscription-nodes').textContent, /需要配置支持此协议的内核/);
  assert.doesNotMatch(b.$('network-subscription-preview').textContent, /fixture-token|private-fixture/);
  assert.equal(b.$('network-subscription-import').disabled, false);
  assert.deepEqual(b.calls.map(call => call.path), ['network/state', 'network/subscriptions/inspect']);
});

test('a new invalid subscription attempt removes the old preview and rejects unsupported URL forms locally', async () => {
  const b = browser(); await b.open();
  b.$('network-subscription-url').value = syntheticSubscription;
  b.replies.push(preview()); await b.submit('network-subscription-form');
  assert.equal(b.$('network-subscription-import').disabled, false);
  for (const invalid of [
    'not a URL',
    'http://subscription.example/list',
    'https://fixture-user:fixture-secret@subscription.example/list',
    'https://subscription.example/list#fixture-fragment',
  ]) {
    b.$('network-subscription-url').value = invalid;
    await b.submit('network-subscription-form');
    assert.equal(b.$('network-subscription-url').value, '');
    assert.equal(b.$('network-subscription-preview').hidden, true);
    assert.equal(b.$('network-subscription-import').disabled, true);
    assert.match(b.$('network-subscription-notice').textContent, /公网 HTTPS/);
    await b.click('network-subscription-import');
    assert.equal(b.calls.length, 2, 'invalid URLs and stale previews must not trigger server requests');
  }
});

test('unsupported-only and expired subscription previews cannot be imported', async () => {
  const b = browser(); await b.open();
  b.$('network-subscription-url').value = syntheticSubscription;
  b.replies.push(preview({ total: 1, supported: 0, unsupported: 1, nodes: [{ name: 'Unsupported', protocol: 'ss', supported: false, reason: '需要 Mihomo' }] }));
  await b.submit('network-subscription-form');
  assert.equal(b.$('network-subscription-import').disabled, true);
  await b.click('network-subscription-import');
  assert.equal(b.calls.length, 2);
  assert.match(b.$('network-subscription-nodes').textContent, /需要 Mihomo/);
  b.$('network-subscription-url').value = syntheticSubscription;
  b.replies.push(preview({ expires_at: Date.now() - 1000 })); await b.submit('network-subscription-form');
  assert.equal(b.$('network-subscription-import').disabled, true);
  await b.click('network-subscription-import'); assert.equal(b.calls.length, 3);
});

test('subscription import uses only a preview ID and current revision, then clears the preview', async () => {
  const b = browser(); await b.open();
  b.$('network-subscription-url').value = syntheticSubscription;
  b.replies.push(preview()); await b.submit('network-subscription-form');
  b.replies.push({}, state({ revision: 5 })); await b.click('network-subscription-import');
  const imported = b.calls.find(call => call.path === 'network/subscriptions/import');
  assert.deepEqual(JSON.parse(JSON.stringify(imported.body)), { preview_id: 'fixture-preview-1', expected_revision: 4, expected_controller_id: 'fixture-controller-a', node_indexes: [0] });
  assert.equal(b.$('network-subscription-preview').hidden, true);
  assert.equal(b.$('network-subscription-url').value, '');
  assert.match(b.$('network-subscription-notice').textContent, /仍?需|请检测/);
});

test('subscription defaults fit capacity and import preserves the explicitly selected original indexes', async () => {
  const b = browser();
  const existing = Array.from({ length: 30 }, (_, index) => node({ id: index ? `existing-${index}` : 'node-a' }));
  await b.open(state({ nodes: existing }));
  b.$('network-subscription-url').value = syntheticSubscription;
  b.replies.push(preview({ total: 5, supported: 4, unsupported: 1, nodes: [
    { index: 0, name: 'First HTTP', protocol: 'http', supported: true },
    { index: 1, name: 'Unsupported SS', protocol: 'ss', supported: false, reason: 'requires_mihomo' },
    { index: 2, name: 'Second HTTP', protocol: 'http', supported: true },
    { index: 3, name: 'Third HTTP', protocol: 'http', supported: true },
    { index: 4, name: 'Fourth HTTP', protocol: 'http', supported: true },
  ] }));
  await b.submit('network-subscription-form');
  const choices = findInputs(b.$('network-subscription-nodes'));
  assert.deepEqual(choices.filter(input => input.checked).map(input => Number(input.dataset.index)), [0, 2]);
  assert.equal(choices.length, 4, 'unsupported nodes must not offer a checkbox');
  assert.match(b.$('network-subscription-selection').textContent, /协议兼容 4 个 \/ 已选 2 个 \/ 可新增 2 个/);
  const first = choices.find(input => input.dataset.index === '0'), fourth = choices.find(input => input.dataset.index === '4');
  assert.equal(fourth.disabled, true);
  fourth.checked = true; await fourth.emit('change');
  assert.equal(fourth.checked, false, 'a dispatched change must not bypass the capacity limit');
  first.checked = false; await first.emit('change');
  assert.equal(fourth.disabled, false);
  fourth.checked = true; await fourth.emit('change');
  b.replies.push({}, state({ revision: 5, nodes: [...existing, node({ id: 'new-2' }), node({ id: 'new-4' })] }));
  await b.click('network-subscription-import');
  const imported = b.calls.find(call => call.path === 'network/subscriptions/import');
  assert.deepEqual(JSON.parse(JSON.stringify(imported.body.node_indexes)), [2, 4]);
  assert.equal(imported.body.expected_revision, 4);
  assert.equal(b.$('network-subscription-selection').textContent, '');
  assert.equal(findInputs(b.$('network-subscription-nodes')).length, 0);
});

test('a capacity reduction never silently truncates the selected subscription nodes', async () => {
  const b = browser();
  const existing = count => Array.from({ length: count }, (_, index) => node({ id: index ? `existing-${index}` : 'node-a' }));
  await b.open(state({ nodes: existing(29) }));
  b.$('network-subscription-url').value = syntheticSubscription;
  b.replies.push(preview({ total: 4, supported: 4, unsupported: 0, nodes: Array.from({ length: 4 }, (_, index) => ({ index, name: `New ${index}`, protocol: 'http', supported: true })) }));
  await b.submit('network-subscription-form');
  const choices = findInputs(b.$('network-subscription-nodes'));
  assert.equal(choices.filter(input => input.checked).length, 3);
  await b.refresh(state({ revision: 5, nodes: existing(31) }));
  assert.equal(choices.filter(input => input.checked).length, 3);
  assert.equal(b.$('network-subscription-import').disabled, true);
  assert.match(b.$('network-subscription-selection').textContent, /已选 3 个 \/ 可新增 1 个.*容量已变化/);
  choices[0].checked = false; await choices[0].emit('change');
  choices[1].checked = false; await choices[1].emit('change');
  assert.equal(b.$('network-subscription-import').disabled, false);
  b.replies.push({}, state({ revision: 6, nodes: existing(32) })); await b.click('network-subscription-import');
  const imported = b.calls.find(call => call.path === 'network/subscriptions/import');
  assert.deepEqual(JSON.parse(JSON.stringify(imported.body.node_indexes)), [2]);
  assert.equal(imported.body.expected_revision, 5);
});

test('full or unconfigured pools can inspect compatibility without selecting import nodes', async () => {
  for (const value of [
    state({ nodes: Array.from({ length: 32 }, (_, index) => node({ id: `existing-${index}` })) }),
    { configured: false, status: 'unconfigured', reason: '入口尚未配置' },
  ]) {
    const b = browser(); await b.open(value);
    b.$('network-subscription-url').value = syntheticSubscription;
    b.replies.push(preview()); await b.submit('network-subscription-form');
    assert.match(b.$('network-subscription-selection').textContent, /协议兼容 1 个 \/ 已选 0 个 \/ 可新增 0 个/);
    assert.equal(b.$('network-subscription-import').disabled, true);
    assert.ok(findInputs(b.$('network-subscription-nodes')).every(input => input.disabled && !input.checked));
    assert.match(b.$('network-subscription-nodes').textContent, /Unsupported SS/);
    await b.click('network-subscription-clear');
    assert.equal(b.$('network-subscription-selection').textContent, '');
    assert.equal(findInputs(b.$('network-subscription-nodes')).length, 0);
    assert.equal(b.calls.length, 2);
  }
});

test('missing import indexes cannot turn compatibility results into an unintended import', async () => {
  const b = browser(); await b.open(); b.$('network-subscription-url').value = syntheticSubscription;
  b.replies.push(preview({ total: 1, supported: 1, unsupported: 0, nodes: [{ name: 'Missing index', protocol: 'http', supported: true }] }));
  await b.submit('network-subscription-form');
  assert.equal(b.$('network-subscription-import').disabled, true);
  assert.equal(findInputs(b.$('network-subscription-nodes')).length, 0);
  assert.match(b.$('network-subscription-nodes').textContent, /缺少有效导入索引/);
});

test('health metrics do not coerce null latency into zero and operational codes are readable', async () => {
  const b = browser(); await b.open(state({ status: 'unknown', nodes: [node({ health: { status: 'healthy', latency_ms: null, checked_at: Date.now(), last_probe_at: Date.now(), error: 'probe_retry_pending' } })], events: [{ at: Date.now(), type: 'route_changed', message: 'probe_completed' }] }));
  assert.equal(b.$('network-status').textContent, '线路待验证');
  assert.doesNotMatch(b.$('network-nodes').textContent, /0\.0 ms|probe_retry_pending/);
  assert.match(b.$('network-nodes').textContent, /等待重试/);
  assert.match(b.$('network-nodes').textContent, /最近探测/);
  assert.match(b.$('network-nodes').textContent, /上次成功/);
  assert.match(b.$('network-events').textContent, /线路切换.*线路检测完成/);
  assert.doesNotMatch(b.$('network-events').textContent, /route_changed|probe_completed/);
});

test('a state response arriving after logout cannot restore private node information', async () => {
  const b = browser(); await b.open();
  const late = deferred(); b.replies.push(late.promise); const request = b.controller.refresh(); await settle();
  const signal = b.calls.at(-1).signal;
  b.logout(); assert.equal(signal.aborted, true);
  late.resolve(state({ nodes: [node({ name: 'late private node' })] })); await request;
  assert.doesNotMatch(b.$('network-nodes').textContent, /late private node/);
  assert.equal(b.$('network-status').textContent, '尚未读取');
  assert.equal(b.timers.size, 0);
});

test('a subscription response from an old session cannot populate a new session', async () => {
  const b = browser(); await b.open();
  b.$('network-subscription-url').value = syntheticSubscription;
  const late = deferred(); b.replies.push(late.promise); const inspection = b.submit('network-subscription-form'); await settle();
  const signal = b.calls.at(-1).signal;
  b.logout(); assert.equal(signal.aborted, true);
  b.login(); await b.open(state({ revision: 8, nodes: [] }));
  late.resolve(preview({ nodes: [{ name: 'old private subscription', protocol: 'http', supported: true }] })); await inspection;
  assert.equal(b.$('network-subscription-preview').hidden, true);
  assert.doesNotMatch(b.$('network-subscription-nodes').textContent, /old private subscription/);
  assert.equal(b.$('network-subscription-url').value, '');
});

test('probe polling runs only on the visible network page and stops when probing completes', async () => {
  const b = browser(); await b.open(state({ probe_running: true }));
  assert.equal(b.timers.size, 1);
  b.controller.setVisible(false); assert.equal(b.timers.size, 0);
  assert.equal(b.calls.length, 1);
  await b.open(state({ probe_running: true })); assert.equal(b.timers.size, 1);
  await b.visibility(true); assert.equal(b.timers.size, 0);
  const before = b.calls.length; await settle(); assert.equal(b.calls.length, before);
  b.replies.push(state({ probe_running: true })); await b.visibility(false); assert.equal(b.timers.size, 1);
  b.replies.push(state({ probe_running: false })); await b.fireTimer();
  assert.equal(b.timers.size, 0); assert.equal(b.$('network-probe').disabled, false);
});

test('probe progress polling is bounded even when the server never finishes', async () => {
  const b = browser(); await b.open(state({ probe_running: true }));
  let polls = 0;
  while (b.timers.size && polls < 100) { b.replies.push(state({ probe_running: true })); await b.fireTimer(); polls++; }
  assert.ok(polls > 0 && polls < 100, 'progress checks must stop after a bounded number of polls');
  assert.equal(b.timers.size, 0);
  assert.match(b.$('network-notice').textContent, /暂停自动查询/);
});

test('leaving the page clears subscription input, aborts inspection, and ignores its late preview', async () => {
  const b = browser(); await b.open();
  b.$('network-subscription-url').value = syntheticSubscription;
  const late = deferred(); b.replies.push(late.promise); const inspection = b.submit('network-subscription-form'); await settle();
  const signal = b.calls.at(-1).signal;
  b.controller.setVisible(false); assert.equal(signal.aborted, true);
  late.resolve(preview()); await inspection;
  assert.equal(b.$('network-subscription-url').value, '');
  assert.equal(b.$('network-subscription-preview').hidden, true);
  assert.equal(b.timers.size, 0);
});

test('uncertain persistence remains visible after a successful mutation', async () => {
  const b = browser(); await b.open();
  b.$('network-mode').value = 'failover'; await b.$('network-mode').emit('change');
  b.replies.push({}, state({ revision: 5, mode: 'failover', durability: 'uncertain' })); await b.submit('network-policy-form');
  assert.match(b.$('network-reason').textContent, /配置已应用，但磁盘持久化未确认/);
  assert.match(b.$('network-notice').textContent, /磁盘持久化未确认/);
  assert.doesNotMatch(b.$('network-notice').textContent, /策略已保存/);
  await b.refresh(state({ revision: 5, mode: 'failover', durability: 'platform_atomic' }));
  assert.doesNotMatch(b.$('network-reason').textContent, /未确认|失败/);
});

test('a mutation persistence warning survives a failed follow-up state read', async () => {
  const b = browser(); await b.open();
  b.$('network-mode').value = 'failover'; await b.$('network-mode').emit('change');
  b.replies.push({ durability: 'uncertain' }, new Error('入口状态暂时不可达'));
  await b.submit('network-policy-form');
  assert.match(b.$('network-status').textContent, /未接通/);
  assert.match(b.$('network-reason').textContent, /磁盘持久化未确认/);
  assert.match(b.$('network-reason').textContent, /状态暂时无法读取/);
  assert.match(b.$('network-notice').textContent, /磁盘持久化未确认/);
  assert.doesNotMatch(b.$('network-notice').textContent, /策略已保存/);
  assert.equal(b.calls.length, 3);
});

test('balance shows actual occupancy and saves global limits without requiring a preferred node', async () => {
  const b = browser();
  const limits = { max_requests: 200, max_streams: 80, queue_limit: 12, queue_timeout_ms: 1500 };
  const runtime = { active_requests: 9, active_streams: 3, queued_requests: 2, queued_streams: 1, completed_requests: 31, failed_requests: 4, rejected_requests: 6 };
  await b.open(state({ mode: 'balance', active_id: '', selected_id: '', limits, runtime, nodes: [node({ weight: 7, max_requests: 20, max_streams: 8, draining: true, runtime })] }));
  assert.equal(b.$('network-active').textContent, '按请求分配 · 负载均衡');
  assert.equal(b.$('network-load-requests').textContent, '9 / 200');
  assert.equal(b.$('network-load-streams').textContent, '3 / 80');
  assert.equal(b.$('network-load-queued').textContent, '2 / 1');
  assert.equal(b.$('network-load-rejected').textContent, '6');
  assert.match(b.$('network-nodes').textContent, /排空中.*请求 9 \/ 20.*流连接 3 \/ 8.*权重 7/);
  assert.equal(b.$('network-selected').disabled, true);
  b.$('network-limit-queue').value = '0'; await b.$('network-policy-form').emit('input');
  b.replies.push({}, state({ revision: 5, mode: 'balance' })); await b.submit('network-policy-form');
  const saved = b.calls.find(call => call.path === 'network/policy');
  assert.deepEqual(JSON.parse(JSON.stringify(saved.body)), { expected_revision: 4, expected_controller_id: 'fixture-controller-a', mode: 'balance', selected_id: '', limits: { ...limits, queue_limit: 0 } });
});

test('missing runtime stays uncollected; invalid limits never submit or replace the draft', async () => {
  const b = browser(); await b.open();
  assert.match(b.$('network-load-requests').textContent, /未采集/);
  assert.equal(b.$('network-load-completed').textContent, '未采集');
  for (const invalid of ['', '-1', '4097', '1.5']) {
    b.$('network-limit-queue').value = invalid; await b.$('network-policy-form').emit('input');
    await b.submit('network-policy-form');
    assert.equal(b.calls.length, 1);
    assert.match(b.$('network-notice').textContent, /整数/);
  }
  b.$('network-limit-requests').value = '123'; await b.$('network-policy-form').emit('input');
  await b.refresh(state({ revision: 7, limits: { max_requests: 300 } }));
  assert.equal(b.$('network-limit-requests').value, '123');
});

test('node editing submits weight capacity and drain status while preserving credentials and original revision', async () => {
  const b = browser(); await b.open(state({ nodes: [node({ weight: 3, max_requests: 40, max_streams: 12 })] }));
  await findButton(b.$('network-nodes'), '编辑').emit('click');
  assert.equal(b.$('network-node-weight').value, '3');
  b.$('network-node-weight').value = '8'; b.$('network-node-requests').value = '50'; b.$('network-node-streams').value = '10'; b.$('network-node-draining').checked = true;
  await b.refresh(state({ revision: 5 }));
  b.replies.push(new Error('设置已变化，请重新载入')); await b.submit('network-node-form');
  const saved = b.calls.at(-1);
  assert.equal(saved.body.expected_revision, 4);
  assert.equal(saved.body.node.weight, 8); assert.equal(saved.body.node.max_requests, 50); assert.equal(saved.body.node.max_streams, 10); assert.equal(saved.body.node.draining, true);
  assert.equal(Object.hasOwn(saved.body.node, 'proxy_url'), false);
});

test('optional live refresh reads only state every five seconds and preserves drafts across polling', async () => {
  const b = browser(); await b.open();
  assert.equal(b.timers.size, 0);
  b.$('network-live').checked = true; await b.$('network-live').emit('change');
  assert.equal(b.timers.size, 1); assert.equal(b.timers.values().next().value.delay, 5000);
  b.$('network-mode').value = 'balance'; await b.$('network-mode').emit('change');
  b.$('network-limit-requests').value = '99'; await b.$('network-policy-form').emit('input');
  b.replies.push(state({ revision: 5, mode: 'manual', runtime: { active_requests: 8 }, limits: { max_requests: 100 } })); await b.fireTimer();
  assert.equal(b.$('network-mode').value, 'balance'); assert.equal(b.$('network-limit-requests').value, '99');
  assert.equal(b.$('network-load-requests').textContent, '8 / 100');
  assert.ok(b.calls.every(call => call.path === 'network/state' && call.method === 'GET'));
  await b.visibility(true); assert.equal(b.timers.size, 0);
  b.replies.push(state()); await b.visibility(false); assert.equal(b.timers.size, 1);
  b.controller.setVisible(false); assert.equal(b.timers.size, 0);
  await b.open(); assert.equal(b.timers.size, 1);
  b.logout(); assert.equal(b.timers.size, 0); assert.equal(b.$('network-live').checked, false);
  assert.equal(b.$('network-load-requests').textContent, '未采集');
});

test('live polling errors stop automatic requests and never treat retained metrics as current', async () => {
  const b = browser(); await b.open();
  b.$('network-live').checked = true; await b.$('network-live').emit('change');
  b.replies.push(new Error('线路控制入口不可用')); await b.fireTimer();
  assert.equal(b.timers.size, 0); assert.equal(b.$('network-live').checked, false);
  assert.match(b.$('network-reason').textContent, /可能保留上次状态/);
  assert.equal(b.$('network-policy-save').disabled, true);
});

test('controller changes invalidate node and policy drafts plus subscription previews even at the same revision', async () => {
  const b = browser(); await b.open();
  b.$('network-mode').value = 'balance'; await b.$('network-mode').emit('change');
  b.$('network-limit-requests').value = '77'; await b.$('network-policy-form').emit('input');
  b.$('network-subscription-url').value = syntheticSubscription; b.replies.push(preview()); await b.submit('network-subscription-form');
  assert.equal(b.calls.at(-1).body.expected_controller_id, 'fixture-controller-a');
  await findButton(b.$('network-nodes'), '编辑').emit('click'); b.$('network-node-url').value = syntheticProxy;
  await b.refresh(state({ controller_id: 'fixture-controller-b', revision: 4, mode: 'latency', limits: { max_requests: 300 } }));
  assert.equal(b.$('network-node-dialog').open, false); assert.equal(b.$('network-node-url').value, '');
  assert.equal(b.$('network-mode').value, 'latency'); assert.equal(b.$('network-limit-requests').value, '300');
  assert.equal(b.$('network-subscription-preview').hidden, true); assert.equal(b.$('network-policy-reload').disabled, true);
  assert.match(b.$('network-notice').textContent, /入口已变化.*旧入口/);
  const before = b.calls.length; await b.submit('network-node-form'); await b.click('network-subscription-import'); assert.equal(b.calls.length, before);
  b.$('network-mode').value = 'balance'; await b.$('network-mode').emit('change');
  b.replies.push({}, state({ controller_id: 'fixture-controller-b', revision: 5, mode: 'balance' })); await b.submit('network-policy-form');
  assert.equal(b.calls.find(call => call.path === 'network/policy').body.expected_controller_id, 'fixture-controller-b');
});

test('delete drafts and in-flight subscription inspections cannot cross controller identities', async () => {
  const b = browser(); await b.open();
  await findButton(b.$('network-nodes'), '移除').emit('click');
  await b.refresh(state({ controller_id: 'fixture-controller-b' }));
  assert.equal(b.$('network-delete-dialog').open, false);
  await b.submit('network-delete-form'); assert.equal(b.calls.length, 2);
  b.$('network-subscription-url').value = syntheticSubscription;
  const pending = deferred(); b.replies.push(pending.promise); const inspection = b.submit('network-subscription-form'); await settle();
  const signal = b.calls.at(-1).signal; assert.equal(b.calls.at(-1).body.expected_controller_id, 'fixture-controller-b');
  await b.refresh(state({ controller_id: 'fixture-controller-c' })); assert.equal(signal.aborted, true);
  pending.resolve(preview()); await inspection;
  assert.equal(b.$('network-subscription-preview').hidden, true); assert.equal(b.$('network-subscription-import').disabled, true);
});

test('node writes, delete and probe bind to their observed controller while legacy detection remains available', async () => {
  const b = browser(); await b.open();
  await findButton(b.$('network-nodes'), '编辑').emit('click'); b.replies.push(new Error('controller_changed')); await b.submit('network-node-form');
  assert.equal(b.calls.at(-1).body.expected_controller_id, 'fixture-controller-a');
  await b.click('network-node-cancel');
  await findButton(b.$('network-nodes'), '移除').emit('click'); b.replies.push(new Error('controller_changed')); await b.submit('network-delete-form');
  assert.equal(b.calls.at(-1).body.expected_controller_id, 'fixture-controller-a');
  await b.click('network-delete-cancel');
  b.replies.push({}, state()); await b.click('network-probe');
  assert.equal(b.calls.find(call => call.path === 'network/probe').body.expected_controller_id, 'fixture-controller-a');
  await b.refresh(state({ controller_id: undefined }));
  b.$('network-subscription-url').value = syntheticSubscription; b.replies.push(preview()); await b.submit('network-subscription-form');
  assert.equal(Object.hasOwn(b.calls.at(-1).body, 'expected_controller_id'), false);
  assert.match(b.$('network-subscription-summary').textContent, /共 2 个/);
});

const healthPolicy = { enabled: true, interval_ms: 30000, timeout_ms: 6000, concurrency: 4, failure_threshold: 3, recovery_successes: 2, recovery_hold_ms: 5000, stale_after_ms: 120000 };
const selectionPolicy = { switch_cooldown_ms: 120000, min_improvement_ms: 30, min_improvement_percent: 20, preferred_regions: [], region_mode: 'off', allow_unknown: true };
const advancedState = extra => state({ health_policy: healthPolicy, selection_policy: selectionPolicy, ...extra });

test('advanced policies save seconds as milliseconds and bind geography to the observed revision', async () => {
  const b = browser(); await b.open(advancedState());
  assert.equal(b.$('network-health-interval').value, '30');
  b.$('network-health-interval').value = '45'; b.$('network-health-timeout').value = '5.5';
  b.$('network-health-enabled').checked = false;
  b.$('network-region-mode').value = 'prefer'; b.$('network-preferred-regions').value = '香港，SG, sg';
  await b.$('network-policy-form').emit('input');
  b.replies.push({}, advancedState({ revision: 5 })); await b.submit('network-policy-form');
  const body = JSON.parse(JSON.stringify(b.calls.find(c => c.path === 'network/policy').body));
  assert.equal(body.expected_revision, 4); assert.equal(body.expected_controller_id, 'fixture-controller-a');
  assert.deepEqual(body.health_policy, { ...healthPolicy, enabled: false, interval_ms: 45000, timeout_ms: 5500 });
  assert.deepEqual(body.selection_policy.preferred_regions, ['香港', 'sg']);
  assert.equal(body.selection_policy.region_mode, 'prefer');
});

test('invalid timing and empty or overlong regional restrictions do not submit', async () => {
  const b = browser(); await b.open(advancedState()); const count = b.calls.length;
  b.$('network-health-stale').value = '31'; await b.submit('network-policy-form'); assert.equal(b.calls.length, count);
  assert.match(b.$('network-notice').textContent, /有效期/);
  b.$('network-health-stale').value = '120'; b.$('network-region-mode').value = 'only';
  await b.submit('network-policy-form'); assert.equal(b.calls.length, count);
  b.$('network-preferred-regions').value = '香'.repeat(11); await b.submit('network-policy-form'); assert.equal(b.calls.length, count);
  b.$('network-preferred-regions').value = '香港'; b.$('network-health-timeout').value = '31';
  await b.submit('network-policy-form'); assert.equal(b.calls.length, count);
});

test('automatic refresh preserves advanced drafts and switching controllers discards them', async () => {
  const b = browser(); await b.open(advancedState());
  b.$('network-health-interval').value = '60'; b.$('network-preferred-regions').value = '香港'; await b.$('network-policy-form').emit('input');
  await b.refresh(advancedState({ revision: 8, health_policy: { ...healthPolicy, interval_ms: 90000 } }));
  assert.equal(b.$('network-health-interval').value, '60'); assert.equal(b.$('network-preferred-regions').value, '香港');
  await b.refresh(advancedState({ controller_id: 'new-controller', health_policy: { ...healthPolicy, interval_ms: 90000 } }));
  assert.equal(b.$('network-health-interval').value, '90'); assert.equal(b.$('network-preferred-regions').value, '');
});

test('all-node and per-node diagnostics include scope without enabling a disabled route', async () => {
  const value = advancedState({ nodes: [node({ enabled: false, region: '香港' })] });
  const b = browser(); await b.open(value);
  assert.match(b.$('network-nodes').textContent, /已禁用.*诊断：可用/);
  const staleButton = findButton(b.$('network-nodes'), '测通');
  b.replies.push({}, value); await b.click('network-probe-all');
  assert.deepEqual(JSON.parse(JSON.stringify(b.calls.find(c => c.path === 'network/probe').body)), { scope: 'all', expected_controller_id: 'fixture-controller-a' });
  b.replies.push({}, value); await findButton(b.$('network-nodes'), '测通').emit('click');
  assert.deepEqual(JSON.parse(JSON.stringify(b.calls.filter(c => c.path === 'network/probe').at(-1).body)), { scope: 'all', node_ids: ['node-a'], expected_controller_id: 'fixture-controller-a' });
  assert.equal(b.calls.some(c => c.path === 'network/nodes'), false);
  await b.refresh(advancedState({ controller_id: 'controller-b' })); const count = b.calls.length;
  await staleButton.emit('click'); assert.equal(b.calls.length, count);
});

test('region editing preserves credentials and enforces the UTF-8 label bound', async () => {
  const b = browser(); await b.open(advancedState({ nodes: [node({ region: '香港' })] }));
  await findButton(b.$('network-nodes'), '编辑').emit('click');
  assert.equal(b.$('network-node-region').value, '香港');
  b.$('network-node-region').value = '香'.repeat(11); const count = b.calls.length;
  await b.submit('network-node-form'); assert.equal(b.calls.length, count);
  b.$('network-node-region').value = '新加坡'; b.replies.push({}, advancedState()); await b.submit('network-node-form');
  const write = b.calls.find(c => c.path === 'network/nodes'); assert.equal(write.body.node.region, '新加坡');
  assert.equal(Object.hasOwn(write.body.node, 'proxy_url'), false);
});

test('probe progress and stale measurements remain explicit while old controllers disable new actions', async () => {
  const b = browser(); await b.open(advancedState({ next_probe_at: Date.now() + 30000, probe_run: { total: 3, completed: 2, succeeded: 1, failed: 1, skipped: 0, running: true, pending: true }, nodes: [node({ health: { status: 'healthy', latency_ms: 20, latency_ewma_ms: 24, checked_at: Date.now(), stale: true } })] }));
  assert.match(b.$('network-probe-progress').textContent, /2 \/ 3.*通过 1.*失败 1.*排队/);
  assert.match(b.$('network-probe-policy').textContent, /30 秒.*6 秒.*4 路/);
  assert.match(b.$('network-nodes').textContent, /结果过期/);
  await b.refresh(state()); assert.equal(b.$('network-probe-all').disabled, true); assert.equal(b.$('network-health-interval').disabled, true);
});

test('safe node edits prepare a preview and never claim the configuration was applied', async () => {
  const b = browser(); await b.open(releaseState()); await b.click('network-add');
  b.$('network-node-name').value = 'Safe candidate'; b.$('network-node-url').value = syntheticProxy;
  const pending = releaseState({ previews: [{ id: 'preview-safe', status: 'checking', total: 2, completed: 0, healthy: 0, base_revision: 4 }] });
  b.replies.push(pending, pending); await b.submit('network-node-form');
  const call = b.calls.find(call => call.path === 'network/releases/prepare'); assert.ok(call);
  assert.equal(call.body.action, 'nodes'); assert.equal(call.body.expected_revision, 4); assert.equal(call.body.expected_controller_id, 'fixture-controller-a');
  assert.equal(call.body.payload.node.proxy_url, syntheticProxy); assert.equal(call.body.payload.expected_revision, undefined);
  assert.equal(b.$('network-node-url').value, ''); assert.match(b.$('network-notice').textContent, /尚未改变/);
  assert.equal(b.calls.some(call => call.path === 'network/nodes'), false); assert.equal([...b.timers][0].delay, 3000);
  b.controller.setVisible(false); assert.equal(b.timers.size, 0);
});

test('ready release starts a bounded trial and requires explicit confirmation', async () => {
  const ready = releaseState({ previews: [{ id: 'ready-one', status: 'ready', base_revision: 4, healthy: 1, expires_at: Date.now() + 30000 }] });
  const b = browser(); await b.open(ready); const start = findButton(b.$('network-release-content'), '开始 2 分钟试用'); assert.ok(start);
  const trial = { ...releaseState({ trial: { id: 'trial-one', revision: 5, started_at: Date.now(), expires_at: Date.now() + 120000 } }), revision: 5 };
  b.replies.push(trial, trial); await start.emit('click');
  assert.equal(b.calls.find(call => call.path === 'network/releases/start').body.preview_id, 'ready-one');
  assert.match(b.$('network-release-status').textContent, /试用/); assert.equal(b.$('network-add').disabled, true); assert.equal(b.$('network-policy-save').disabled, true);
  const confirm = findButton(b.$('network-release-content'), '确认保留配置'); b.replies.push({ ...releaseState(), revision: 5 }, { ...releaseState(), revision: 5 }); await confirm.emit('click');
  const call = b.calls.find(call => call.path === 'network/releases/confirm'); assert.equal(call.body.trial_id, 'trial-one'); assert.equal(call.body.expected_revision, 5);
  assert.equal(b.$('network-add').disabled, false); assert.match(b.$('network-notice').textContent, /确认保留/);
});

test('rejected and expired previews never offer trial, and foreign controller callbacks do not submit', async () => {
  const b = browser(); await b.open(releaseState({ previews: [{ id: 'rejected', status: 'rejected', reason: 'changed_node_unreachable' }, { id: 'expired', status: 'ready', expires_at: Date.now() - 1 }] }));
  assert.equal(findButton(b.$('network-release-content'), '开始 2 分钟试用'), null); assert.match(b.$('network-release-content').textContent, /无法连接/);
  await b.refresh(releaseState({ previews: [{ id: 'ready-one', status: 'ready', base_revision: 4, healthy: 1, expires_at: Date.now() + 30000 }] }));
  const stale = findButton(b.$('network-release-content'), '开始 2 分钟试用');
  await b.refresh({ ...releaseState(), controller_id: 'controller-b' }); const count = b.calls.length; await stale.emit('click'); assert.equal(b.calls.length, count);
});

test('trial rollback and historical restore bind their IDs to the current revision', async () => {
  const b = browser(); await b.open(releaseState({ trial: { id: 'trial-one', expires_at: Date.now() + 120000 } }));
  b.replies.push(releaseState(), releaseState({ history: [{ id: 'history-one', revision: 2, created_at: Date.now(), revertible: true }] }));
  await findButton(b.$('network-release-content'), '立即回滚').emit('click'); assert.equal(b.calls.find(call => call.path === 'network/releases/rollback').body.trial_id, 'trial-one');
  b.replies.push(releaseState(), releaseState()); await findButton(b.$('network-release-history'), '恢复此版本').emit('click');
  assert.equal(b.calls.filter(call => call.path === 'network/releases/rollback')[1].body.version_id, 'history-one');
});

const observations = (extra = {}) => ({ configured: true, controller_id: 'fixture-controller-a', counter_semantics: 'bucket_deltas_and_instantaneous_gauges', sample_interval_ms: 60000, retention_ms: 86400000, samples: [{ at: Date.now() - 60000, runtime: { completed_requests: 7, failed_requests: 2, rejected_requests: 1 }, nodes: [] }, { at: Date.now(), runtime: { completed_requests: 3, failed_requests: 0, rejected_requests: 0 }, nodes: [] }], decision: { reason: 'switch_cooldown', candidates: [{ id: 'node-a', reason: 'capacity_full', eligible: true }] }, last_error: '', ...extra });
test('observability sums actual bucket increments and explains selection without inventing percentiles', async () => {
  const b = browser(); await b.open(releaseState()); b.replies.push(observations()); await b.click('network-observability-refresh');
  assert.equal(b.calls.at(-1).path, 'network/observability'); assert.equal(b.$('network-observability-completed').textContent, '10'); assert.equal(b.$('network-observability-failed').textContent, '2'); assert.equal(b.$('network-observability-rejected').textContent, '1');
  assert.match(b.$('network-observability-decision').textContent, /切换冷却/); assert.match(b.$('network-observability-candidates').textContent, /容量均已用尽/); assert.equal(b.$('network-observability-samples').children.length, 2);
  assert.doesNotMatch(b.$('network-observability-panel').textContent, /p95|成功率/);
});
test('single-node window uses node deltas and leaves missing or empty measurements uncollected', async () => {
  const b = browser(); await b.open(releaseState()); b.$('network-observability-node').value = 'node-a'; b.$('network-observability-window').value = '3600000';
  b.replies.push(observations({ samples: [{ at: Date.now(), runtime: { completed_requests: 999, failed_requests: 999, rejected_requests: 999 }, nodes: [{ id: 'node-a', runtime: { completed_requests: 0, failed_requests: 0, rejected_requests: 0 } }] }] })); await b.click('network-observability-refresh');
  assert.equal(b.calls.at(-1).path, 'network/observability/query'); assert.deepEqual(JSON.parse(JSON.stringify(b.calls.at(-1).body)), { node_id: 'node-a', window_ms: 3600000, expected_controller_id: 'fixture-controller-a' }); assert.equal(b.$('network-observability-completed').textContent, '0');
  b.replies.push(observations({ samples: [] })); await b.click('network-observability-refresh'); assert.equal(b.$('network-observability-completed').textContent, '未采集'); assert.match(b.$('network-observability-samples').textContent, /尚无采样/);
});
test('late observation cannot cross controller ownership or survive leaving the page', async () => {
  const b = browser(); await b.open(releaseState()); const late = deferred(); b.replies.push(late.promise); await b.click('network-observability-refresh');
  b.controller.setVisible(false); assert.equal(b.calls.at(-1).signal.aborted, true); late.resolve(observations()); await settle(); assert.equal(b.$('network-observability-completed').textContent, '未读取');
  b.replies.push(releaseState()); b.controller.setVisible(true); await settle(); b.replies.push(observations({ controller_id: 'another-controller' })); await b.click('network-observability-refresh'); assert.equal(b.$('network-observability-completed').textContent, '未读取'); assert.match(b.$('network-observability-note').textContent, /暂不可用/);
});

test('probe cancellation binds to the displayed run and preserves completed evidence', async () => {
  const b = browser(); const run={id:'run-one',running:true,cancelled:false,total:3,completed:1,succeeded:1,failed:0,skipped:0}; await b.open(state({probe_running:true,probe_run:run})); assert.equal(b.$('network-probe-cancel').hidden,false);
  const stopped=state({probe_running:false,probe_run:{...run,running:false,cancelled:true,completed:3,skipped:2}});b.replies.push(stopped,stopped);await b.click('network-probe-cancel');
  const call=b.calls.find(call=>call.path==='network/probe/cancel');assert.equal(call.body.run_id,'run-one');assert.equal(call.body.expected_controller_id,'fixture-controller-a');assert.match(b.$('network-probe-progress').textContent,/本轮已取消.*通过 1.*跳过 2/);assert.equal(b.$('network-probe-cancel').hidden,true);
});
test('opening the supported network view reads observations once without high-frequency history polling', async () => {
  const b=browser();await b.open(releaseState());assert.equal(b.calls.filter(call=>call.path==='network/observability').length,1);await b.refresh(releaseState());assert.equal(b.calls.filter(call=>call.path==='network/observability').length,1);
});

test('probe summary uses the shared four-slot ceiling without inflating a smaller configured limit', async () => {
  const b=browser();await b.open(advancedState({health_policy:{...healthPolicy,concurrency:16}}));assert.match(b.$('network-probe-policy').textContent,/最多 4 路并发/);assert.doesNotMatch(b.$('network-probe-policy').textContent,/最多 16 路并发/);assert.equal(b.$('network-health-concurrency').value,'16');
  await b.refresh(advancedState({health_policy:{...healthPolicy,concurrency:2}}));assert.match(b.$('network-probe-policy').textContent,/最多 2 路并发/);assert.match(markup,/当前 Edge 总探测并发上限 4，多个任务共享/);
});
