import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';
const web = new URL('../../internal/standalone/web/', import.meta.url);
const source = await readFile(new URL('sources.js', web), 'utf8'), markup = await readFile(new URL('index.html', web), 'utf8');
class Element {
  constructor(tag = 'div') { this.tagName = tag.toUpperCase(); this.value = ''; this.disabled = false; this.hidden = false; this.open = false; this.children = []; this.handlers = new Map(); this.content = ''; }
  set textContent(value) { this.content = String(value); this.children = []; }
  get textContent() { return this.content + this.children.map(item => item.textContent || '').join(''); }
  append(...children) { this.children.push(...children); } replaceChildren(...children) { this.children = children; this.content = ''; }
  addEventListener(type, handler) { this.handlers.set(type, [...(this.handlers.get(type) || []), handler]); }
  setAttribute(key, value) { this[key] = String(value); }
  showModal() { this.open = true; } close() { this.open = false; for (const fn of this.handlers.get('close') || []) fn({ preventDefault() {} }); } focus() {}
  async emit(type) { if (this.disabled && type === 'click') return; for (const fn of this.handlers.get(type) || []) await fn({ preventDefault() {} }); await settle(); }
}
const settle = async () => { for (let i = 0; i < 8; i++) await new Promise(resolve => setImmediate(resolve)); };
const find = (root, tag, value) => root.tagName === tag && (value === undefined || root.textContent === value) ? root : root.children.map(item => find(item, tag, value)).find(Boolean);
const inputs = root => [...(root.tagName === 'INPUT' && root.type === 'checkbox' ? [root] : []), ...root.children.flatMap(inputs)];
const all = (root, predicate) => [...(predicate(root) ? [root] : []), ...root.children.flatMap(child => all(child, predicate))];
const field = (root, name) => all(root, e => e.tagName === 'LABEL' && e.children[0]?.textContent === name)[0]?.children[1];
const shown = root => all(root, e => e.className === 'source-node' && !e.hidden);
const fixture = (extra = {}) => ({ configured: true, controller_id: 'controller-a', source_revision: 2, busy: false, kernel: { installed: false, status: 'idle', version: 'fixture', generations: 0 }, sources: [], ...extra });
const saved = (extra = {}) => ({ id: 'source-a', name: '回国线路', enabled: true, auto_apply: false, interval_ms: 21600000, has_url: true, status: 'idle', nodes: [], ...extra });
function browser() {
  const ids = new Map([...markup.matchAll(/id="([^"]+)"/g)].map(match => [match[1], new Element()])), calls = [], replies = [], timers = new Set(), events = new Map();
  let epoch = 1, auth = true, pool = { configured: true, controller_id: 'controller-a', revision: 7, nodes: [], release: { trial: null } }, refreshes = 0;
  const window = {}, document = { visibilityState: 'visible', getElementById: id => { assert.ok(ids.has(id), id); return ids.get(id); }, createElement: tag => new Element(tag), addEventListener: (event, fn) => events.set(event, fn) };
  vm.runInContext(source, vm.createContext({ window, document, Date, URL, AbortController, setTimeout: (fn, delay) => { const value = { fn, delay }; timers.add(value); return value; }, clearTimeout: value => timers.delete(value) }));
  const controller = window.SalcaraSources({ request: async (path, method, body, anonymous, signal) => { calls.push({ path, method, body, signal }); assert.ok(replies.length, path); const result = await replies.shift(); if (result instanceof Error) throw result; return result; }, getEpoch: () => epoch, isAuthenticated: () => auth, isBusy: () => false, getPool: () => pool, refreshPool: async () => { refreshes++; } });
  return { calls, replies, timers, ids, controller, document, $: id => ids.get(id), async open(value = fixture()) { replies.push(value); controller.setContext(pool, true); await settle(); },
    async refresh(value) { replies.push(value); await controller.refresh(); }, async click(id) { await ids.get(id).emit('click'); }, async submit(id) { await ids.get(id).emit('submit'); },
    async tick(value) { replies.push(value); const timer = [...timers][0]; assert.ok(timer); timers.delete(timer); timer.fn(); await settle(); },
    hide() { controller.setContext(pool, false); }, async change(value) { pool = value; controller.setContext(pool, true); await settle(); }, logout() { epoch++; auth = false; controller.reset(); },
    async background() { document.visibilityState = 'hidden'; events.get('visibilitychange')(); await settle(); }, get refreshes() { return refreshes; } };
}

test('new source saves only configuration, clears URL immediately and binds revisions/controller', async () => {
  const b = browser(); assert.equal(b.calls.length, 0); await b.open(); await b.click('sources-add');
  b.$('source-name').value = '回国订阅'; b.$('source-url').value = 'https://fixture.example/sub?token=private-fixture';
  let finish; b.replies.push(new Promise(resolve => { finish = resolve; })); const pending = b.submit('source-form'); await settle();
  assert.equal(b.$('source-url').value, ''); const call = b.calls.at(-1); assert.equal(call.path, 'network/sources/save'); assert.equal(call.body.expected_source_revision, 2); assert.equal(call.body.expected_controller_id, 'controller-a'); assert.equal(call.body.auto_apply, false); assert.equal(call.body.interval_ms, 21600000);
  finish(fixture({ source_revision: 3, sources: [saved()] })); await pending; assert.equal(b.$('source-dialog').open, false); assert.match(b.$('sources-notice').textContent, /来源已保存/); assert.equal(b.calls.length, 2);
  assert.equal(b.$('sources-list').textContent.includes('private-fixture'), false);
});

test('edit leaves secret URL blank and preserves current source revision', async () => {
  const b = browser(); await b.open(fixture({ sources: [saved()] })); await find(b.$('sources-list'), 'BUTTON', '编辑').emit('click'); assert.equal(b.$('source-url').value, '');
  b.$('source-name').value = '已改名'; b.replies.push(fixture({ source_revision: 3, sources: [saved({ name: '已改名' })] })); await b.submit('source-form');
  assert.equal(b.calls.at(-1).body.url, ''); assert.equal(b.calls.at(-1).body.id, 'source-a'); assert.equal(b.calls.at(-1).body.expected_source_revision, 2);
});

test('candidate selection preserves existing choices, disables unsupported nodes and applies only selected keys', async () => {
  const record = saved({ status: 'review', candidate_id: 'candidate-a', nodes: [{ key: 'selected', name: '已选', protocol: 'ss', supported: true, selected: true }, { key: 'new', name: '新增', protocol: 'socks5', supported: true }, { key: 'unsupported', name: '未知', supported: false }] });
  const b = browser(); await b.open(fixture({ sources: [record] })); const rows = inputs(b.$('sources-list')); assert.equal(rows[0].checked, true); assert.equal(rows[1].checked, false); assert.equal(rows[2].disabled, true);
  rows[1].checked = true; await rows[1].emit('change'); b.replies.push(fixture({ busy: true, sources: [saved({ ...record, status: 'applying' })] })); await find(b.$('sources-list'), 'BUTTON', '验证并应用勾选节点').emit('click');
  const body = b.calls.at(-1).body; assert.deepEqual([...body.node_keys], ['selected', 'new']); assert.equal(body.candidate_id, 'candidate-a'); assert.equal(body.expected_revision, 7); assert.equal(body.expected_source_revision, 2);
  assert.match(b.$('sources-notice').textContent, /任务已受理/); assert.equal([...b.timers][0].delay, 3000);
  await b.tick(fixture({ sources: [saved({ status: 'applied' })] })); assert.equal(b.timers.size, 0); assert.equal(b.refreshes, 1); assert.match(b.$('sources-list').textContent, /已应用/);
});

test('busy task polls only visible page and late response after logout cannot render', async () => {
  const b = browser(); await b.open(fixture({ busy: true })); assert.equal(b.timers.size, 1); await b.background(); assert.equal(b.timers.size, 0);
  const c = browser(); let finish; c.replies.push(new Promise(resolve => { finish = resolve; })); const pending = c.open(); await settle(); c.logout(); finish(fixture({ sources: [saved({ name: 'late-private' })] })); await pending;
  assert.equal(c.$('sources-list').textContent.includes('late-private'), false); assert.equal(c.timers.size, 0);
});

test('controller switch clears secret draft, old candidate callbacks and selections', async () => {
  const b = browser(); await b.open(fixture({ sources: [saved({ candidate_id: 'candidate-a', nodes: [{ key: 'one', name: 'node', supported: true, selected: true }] })] }));
  const stale = find(b.$('sources-list'), 'BUTTON', '验证并应用勾选节点'); await b.click('sources-add'); b.$('source-url').value = 'private';
  b.replies.push(fixture({ controller_id: 'controller-b' })); await b.change({ configured: true, controller_id: 'controller-b', revision: 7, release: {} });
  assert.equal(b.$('source-url').value, ''); assert.equal(b.$('source-dialog').open, false); const count = b.calls.length; await stale.emit('click'); assert.equal(b.calls.length, count);
});

test('unknown source and kernel errors render fixed text, never raw URLs or response bodies', async () => {
  const b = browser(); await b.open(fixture({ kernel: { error: 'https://private.example/secret' }, sources: [saved({ status: 'failed', error: 'private-token' })] }));
  assert.equal(b.$('sources-kernel-detail').textContent.includes('private'), false); assert.equal(b.$('sources-list').textContent.includes('private-token'), false);
  b.replies.push(new Error('https://private.example/secret')); await b.click('sources-kernel-install'); assert.equal(b.$('sources-notice').textContent.includes('private'), false);
});

test('legacy controllers hide subscription manager without issuing unsupported requests', async () => {
  const b = browser(); await b.change({ configured: true, controller_id: 'old', revision: 1 }); assert.equal(b.calls.length, 0); assert.equal(b.$('network-sources-panel').hidden, true);
});

test('known translated API conflict explains history references without exposing arbitrary error text', async () => {
  const b=browser();await b.open(fixture({sources:[saved()]}));await find(b.$('sources-list'),'BUTTON','删除订阅').emit('click');b.replies.push(new Error('活动配置或保留的历史版本仍引用此订阅，暂不能删除'));await b.submit('source-delete-form');assert.match(b.$('sources-notice').textContent,/历史版本仍引用/);
});

test('late mutation cannot overwrite fresh state after switching away and back to the same controller', async () => {
  const b=browser();await b.open();await b.click('sources-add');b.$('source-name').value='old-operation';b.$('source-url').value='https://fixture.example/sub';let finish;b.replies.push(new Promise(resolve=>{finish=resolve}));const pending=b.submit('source-form');await settle();
  b.replies.push(fixture({controller_id:'controller-b'}));await b.change({configured:true,controller_id:'controller-b',revision:7,release:{}});
  b.replies.push(fixture({sources:[saved({name:'fresh-state'})]}));await b.change({configured:true,controller_id:'controller-a',revision:7,release:{}});
  finish(fixture({sources:[saved({name:'old-operation'})]}));await pending;assert.match(b.$('sources-list').textContent,/fresh-state/);assert.doesNotMatch(b.$('sources-list').textContent,/old-operation/);
});

test('source deletion confirmation names both active configuration and retained history references', async () => {
  const b=browser();await b.open(fixture({sources:[saved()]}));await find(b.$('sources-list'),'BUTTON','删除订阅').emit('click');assert.match(b.$('source-delete-name').textContent,/活动配置或保留的历史版本仍引用此订阅/);assert.equal(b.$('source-delete-dialog').open,true);
});
test('sources static module loads before network and has an embedded JavaScript route', async () => {
  const scripts=[...markup.matchAll(/<script src="([^\"]+)" defer><\/script>/g)].map(match=>match[1]);assert.ok(scripts.includes('./sources.js'));assert.ok(scripts.indexOf('./sources.js')<scripts.indexOf('./network.js'));
  const server=await readFile(new URL('../../internal/standalone/server.go',import.meta.url),'utf8');assert.match(server,/\/\/go:embed web\/\*/);assert.match(server,/case Prefix \+ "\/admin\/sources\.js":\s*file, kind = "sources\.js", "text\/javascript; charset=utf-8"/);
});

test('source final-policy and registry-cap failures have fixed actionable translations', async () => {
  const b=browser();await b.open(fixture({sources:[saved({status:'failed',error:'no_eligible_route'}),saved({id:'source-b',status:'failed',error:'source_registry_limit'})]}));assert.match(b.$('sources-list').textContent,/当前策略下没有可用线路，已保留原配置，请检查所选线路或地区限制/);assert.match(b.$('sources-list').textContent,/订阅数据量超过上限/);assert.doesNotMatch(b.$('sources-list').textContent,/no_eligible_route|source_registry_limit/);
});

const quotaPool = extra => ({ configured: true, controller_id: 'controller-a', revision: 7, mode: 'balance', selected_id: 'direct', nodes: [], release: { trial: null }, source_limits: [{ source_id: 'other-source', max_requests: 10, max_streams: 5 }], source_groups: [], ...extra });
async function quotaBrowser(extra = {}) {
  const b = browser(); b.replies.push(fixture({ sources: [saved()] })); await b.change(quotaPool(extra));
  await find(b.$('sources-list'), 'BUTTON', '共享额度').emit('click'); return b;
}
test('subscription shared quotas prepare a safe release and preserve other source limits', async () => {
  const b = await quotaBrowser(); assert.equal(b.$('source-quota-dialog').open, true);
  b.$('source-quota-enabled').checked = true; b.$('source-quota-requests').value = '20'; b.$('source-quota-streams').value = '12';
  b.replies.push({ revision: 7, release: { previews: [] } }); await b.submit('source-quota-form');
  const call = b.calls.at(-1); assert.equal(call.path, 'network/releases/prepare'); assert.equal(call.body.expected_revision, 7); assert.equal(call.body.action, 'policy'); assert.equal(call.body.expected_controller_id, 'controller-a');
  assert.deepEqual(JSON.parse(JSON.stringify(call.body.payload.source_limits)), [{ source_id: 'other-source', max_requests: 10, max_streams: 5 }, { source_id: 'source-a', max_requests: 20, max_streams: 12 }]);
  assert.equal(b.refreshes, 1); assert.equal(b.$('source-quota-dialog').open, false); assert.match(b.$('sources-notice').textContent, /尚未生效/);
});
test('quota draft retains original CAS through pool refresh and removal clears only that source', async () => {
  const b = await quotaBrowser({ source_limits: [{ source_id: 'source-a', max_requests: 8, max_streams: 4 }] });
  await b.change(quotaPool({ revision: 8 })); b.$('source-quota-enabled').checked = false;
  b.replies.push(new Error('revision conflict')); await b.submit('source-quota-form');
  assert.equal(b.calls.at(-1).body.expected_revision, 7); assert.deepEqual([...b.calls.at(-1).body.payload.source_limits], []);
  assert.equal(b.$('source-quota-dialog').open, false); assert.match(b.$('sources-notice').textContent, /不会自动重试/);
});
test('quota input rejects invalid counts and controller changes invalidate the draft', async () => {
  const b = await quotaBrowser(); b.$('source-quota-enabled').checked = true; b.$('source-quota-requests').value = '0';
  const count = b.calls.length; await b.submit('source-quota-form'); assert.equal(b.calls.length, count); assert.match(b.$('source-quota-notice').textContent, /1–4096/);
  b.replies.push(fixture({ controller_id: 'controller-b' })); await b.change(quotaPool({ controller_id: 'controller-b' }));
  assert.equal(b.$('source-quota-dialog').open, false); const after = b.calls.length; await b.submit('source-quota-form'); assert.equal(b.calls.length, after);
});
test('source card reflects shared occupied capacity including retired connections', async () => {
  const b = await quotaBrowser({ source_groups: [{ source_id: 'source-a', limited: true, max_requests: 2, max_streams: 3, runtime: { active_requests: 1, active_streams: 3 }, retired: true }] });
  assert.match(b.$('sources-list').textContent, /请求 1 \/ 2 · 长连接 3 \/ 3/); assert.match(b.$('sources-list').textContent, /旧连接排空/);
});

const directory = nodes => saved({ status: 'review', candidate_id: 'directory-a', nodes });
const candidate = (key, extra = {}) => ({ key, name: key, protocol: 'ss', supported: true, change: 'unchanged', ...extra });
async function filter(b, name, value) { const input = field(b.$('sources-list'), name); input.value = value; await input.emit(input.type === 'search' ? 'input' : 'change'); }
async function bulk(b, label) { await find(b.$('sources-list'), 'BUTTON', label).emit('click'); }

test('directory combines Unicode search, protocol and actual updated status without losing hidden selections', async () => {
  const record = directory([candidate('sh', { name: '上海 Ａ', selected: true }), candidate('sz', { name: '深圳 a', protocol: 'http', change: 'updated' }), candidate('bad', { name: '深圳 a', protocol: 'http', supported: false, change: 'added' })]);
  const b = browser(); await b.open(fixture({ sources: [record] }));
  await filter(b, '搜索节点', 'A'); assert.equal(shown(b.$('sources-list')).length, 3);
  await filter(b, '协议', 'http'); await filter(b, '目录状态', 'updated'); assert.equal(shown(b.$('sources-list')).length, 1); assert.match(shown(b.$('sources-list'))[0].textContent, /深圳 a.*已变更/);
  await bulk(b, '全选筛选内支持项'); assert.match(b.$('sources-list').textContent, /已选 2 个（筛选外 1 个）/);
  await filter(b, '搜索节点', '没有结果'); assert.equal(shown(b.$('sources-list')).length, 0); assert.match(b.$('sources-list').textContent, /没有匹配/);
  b.replies.push(fixture({ sources: [record] })); await bulk(b, '验证并应用勾选节点'); assert.deepEqual([...b.calls.at(-1).body.node_keys], ['sh', 'sz']);
});

test('changed-only bulk selection replaces visible choices, preserves hidden choices and excludes unsupported nodes', async () => {
  const b = browser(); await b.open(fixture({ sources: [directory([candidate('hidden', { protocol: 'http', selected: true }), candidate('same', { selected: true }), candidate('added', { change: 'added' }), candidate('updated', { change: 'updated' }), candidate('legacy', { change: 'changed' }), candidate('unsupported', { change: 'added', supported: false })])] }));
  await filter(b, '协议', 'ss'); await bulk(b, '仅选筛选内变更项'); assert.deepEqual(inputs(b.$('sources-list')).map(i => i.checked), [true, false, true, true, true, false]);
  await bulk(b, '清空筛选内选择'); assert.deepEqual(inputs(b.$('sources-list')).map(i => i.checked), [true, false, false, false, false, false]);
});

test('a 256-node directory is complete and oversized bulk selection never silently truncates', async () => {
  const b = browser(); await b.open(fixture({ sources: [directory(Array.from({ length: 256 }, (_, i) => candidate(`node-${i}`, { selected: i === 255 })))] }));
  assert.equal(inputs(b.$('sources-list')).length, 256); await bulk(b, '全选筛选内支持项'); assert.equal(inputs(b.$('sources-list')).filter(i => i.checked).length, 1); assert.match(b.$('sources-list').textContent, /本次选择共 256 个.*已选节点未改变/);
  await filter(b, '搜索节点', 'node-255'); assert.equal(shown(b.$('sources-list')).length, 1); assert.equal(shown(b.$('sources-list'))[0].children[0].checked, true);
});

test('capacity reserves other sources and manual nodes while replacing all old nodes of this source', async () => {
  const b = browser(); await b.open(fixture({ sources: [directory([candidate('a'), candidate('b'), candidate('c')])] }));
  await b.change({ configured: true, controller_id: 'controller-a', revision: 8, release: {}, nodes: [...Array.from({ length: 29 }, () => ({ source_id: 'other' })), {}, { source_id: 'source-a' }, { source_id: 'source-a' }] });
  assert.match(b.$('sources-list').textContent, /当前来源可用 2 \/ 32/); await bulk(b, '全选筛选内支持项'); assert.equal(inputs(b.$('sources-list')).filter(i => i.checked).length, 0);
  const checks = inputs(b.$('sources-list')); checks[0].checked = true; await checks[0].emit('change'); checks[1].checked = true; await checks[1].emit('change'); assert.equal(checks[2].disabled, true);
  await b.change({ configured: true, controller_id: 'controller-a', revision: 9, release: {}, nodes: Array.from({ length: 31 }, () => ({})) });
  assert.match(b.$('sources-list').textContent, /已超出容量/); assert.equal(find(b.$('sources-list'), 'BUTTON', '验证并应用勾选节点').disabled, true); assert.equal(checks[0].disabled, false);
});

test('same-candidate refresh keeps filters, expansion and choices while new candidates reject stale callbacks', async () => {
  const record = directory([candidate('one'), candidate('two')]), b = browser(); await b.open(fixture({ sources: [record] }));
  await filter(b, '搜索节点', 'two'); await bulk(b, '全选筛选内支持项'); find(b.$('sources-list'), 'DETAILS').open = false;
  const oldBulk = find(b.$('sources-list'), 'BUTTON', '全选筛选内支持项'), oldCheckbox = inputs(b.$('sources-list'))[0];
  await b.refresh(fixture({ sources: [record] })); assert.equal(field(b.$('sources-list'), '搜索节点').value, 'two'); assert.equal(find(b.$('sources-list'), 'DETAILS').open, false); assert.deepEqual(inputs(b.$('sources-list')).map(i => i.checked), [false, true]);
  await b.refresh(fixture({ sources: [{ ...record, candidate_id: 'directory-b' }] })); assert.equal(field(b.$('sources-list'), '搜索节点').value, ''); await oldBulk.emit('click'); oldCheckbox.checked = true; await oldCheckbox.emit('change'); assert.deepEqual(inputs(b.$('sources-list')).map(i => i.checked), [false, false]);
});

test('oversized catalogs and ambiguous keys cannot submit a partial or malformed selection', async () => {
  const b = browser(); await b.open(fixture({ sources: [directory([candidate('same'), candidate('same'), candidate(''), candidate(undefined)])] }));
  await bulk(b, '全选筛选内支持项'); assert.equal(inputs(b.$('sources-list')).every(i => i.disabled), true); assert.equal(find(b.$('sources-list'), 'BUTTON', '验证并应用勾选节点').disabled, true);
  await b.refresh(fixture({ sources: [directory(Array.from({ length: 257 }, (_, i) => candidate(`n${i}`)))] })); assert.equal(inputs(b.$('sources-list')).length, 0); assert.match(b.$('sources-list').textContent, /超过每来源 256 个/); assert.equal(find(b.$('sources-list'), 'BUTTON', '全选筛选内支持项').disabled, true);
});
