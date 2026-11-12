import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const root = new URL('../../internal/standalone/web/', import.meta.url);
const source = await readFile(new URL('app.js', root), 'utf8');
const markup = await readFile(new URL('index.html', root), 'utf8');
// These values exist only in the isolated in-memory browser fixture.
const testPassword = 'salcara-ui-test-password-only-1234';
const testCSRF = 'c'.repeat(43);
const profiles = [
  { id: 'economy', label: '省资源', description: '较小缓存', memory_limit_mib: 96, account_events: 256, session_events: 128, max_app_streams: 4, max_pending_commands: 64 },
  { id: 'balanced', label: '均衡', description: '适中缓存', memory_limit_mib: 192, account_events: 1000, session_events: 256, max_app_streams: 8, max_pending_commands: 256 },
  { id: 'performance', label: '高负载', description: '较大缓存', memory_limit_mib: 320, account_events: 2000, session_events: 500, max_app_streams: 32, max_pending_commands: 512 },
];
const state = (extra = {}) => ({ stats: {}, devices: [], audit: [], total: 0, standalone_version: '0.5.0', public_url: 'https://hub.example.com/salcara-hub', update_configured: true, resource_mode: 'economy', resource_modes: profiles, ...extra });
const stationState = (extra = {}) => state({ remote_access_mode: 'open', remote_access_revision: 'station-r1', remote_access_locked: false, remote_access_ready: true, remote_access_station: { version: 1, apiBases: ['https://a.example/team'], allowOfficialAccounts: false }, ...extra });
const storageState = days => ({ enabled:true,policy:{retentionDays:days,globalBytes:256*1048576,bindingBytes:64*1048576,offlineQueueEnabled:false},diskBytes:1048576,eventCount:0,blobCount:0 });
const response = (value, status = 200) => ({ status, ok: status >= 200 && status < 300, json: async () => value });
function deferred() { let resolve; const promise = new Promise(r => { resolve = r; }); return { promise, resolve }; }
async function settle() { for (let i = 0; i < 5; i++) await new Promise(resolve => setImmediate(resolve)); }
class Element {
  constructor(tag = 'div') { this.tagName = tag.toUpperCase(); this.value = ''; this.disabled = false; this.hidden = false; this.open = false; this.children = []; this.dataset = {}; this.handlers = new Map(); this.attributes = {}; this.content = ''; }
  set textContent(value) { this.content = String(value); this.children = []; }
  get textContent() { return this.content + this.children.map(child => child.textContent).join(''); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.children = children; this.content = ''; }
  setAttribute(key, value) { this.attributes[key] = value; }
  addEventListener(type, handler) { const list = this.handlers.get(type) || []; list.push(handler); this.handlers.set(type, list); }
  showModal() { this.open = true; }
  close() { this.open = false; }
  async emit(type) { if (this.disabled && type === 'click') return; for (const handler of this.handlers.get(type) || []) await handler({ preventDefault() {} }); await settle(); }
}
function browser(session = null) {
  const ids = new Map([...markup.matchAll(/id="([^"]+)"/g)].map(match => [match[1], new Element()]));
  ids.get('workspace').hidden = true;
  const requests = [], replies = session ? [response(state())] : [], authRequests = [], authReplies = [session ? response(session) : response({}, 401)], timers = new Set(), events = new Map(), clipboardWrites = [];
  const context = vm.createContext({
    document: { getElementById: id => { assert.ok(ids.has(id), `unknown HTML id ${id}`); return ids.get(id); }, createElement: tag => new Element(tag) },
    window: { location: { origin: 'https://hub.example.com' } },
    navigator: { clipboard: { writeText: async value => clipboardWrites.push(value) } },
    fetch: async (path, options) => { const auth = String(path).includes('/auth/'); (auth ? authRequests : requests).push({ path: String(path), ...options }); const queue = auth ? authReplies : replies; assert.ok(queue.length, 'unexpected request / background polling'); return await queue.shift(); },
    AbortController, TextEncoder, URLSearchParams, Date,
    setTimeout: (callback, delay) => { const handle = { callback, delay }; timers.add(handle); return handle; }, clearTimeout: handle => timers.delete(handle),
    addEventListener: (type, handler) => events.set(type, handler),
  });
  vm.runInContext(source, context, { filename: 'app.js' });
  return { ids, requests, replies, authRequests, authReplies, timers, events, clipboardWrites, async login(value = state()) { await settle(); authReplies.push(response({ csrf_token: testCSRF })); replies.push(response(value)); ids.get('admin-password').value = testPassword; await ids.get('login-form').emit('submit'); } };
}

test('login keeps credentials out of the form, URLs and storage; no idle requests', async () => {
  const b = browser(); await b.login();
  assert.equal(b.ids.get('workspace').hidden, false);
  assert.equal(b.ids.get('admin-password').value, '');
  assert.equal(b.requests.length, 1);
  assert.equal(b.requests[0].headers.Authorization, undefined);
  assert.equal(b.requests[0].credentials, 'same-origin');
  assert.equal(b.requests[0].referrerPolicy, 'no-referrer');
  assert.ok(!b.requests[0].path.includes(testPassword));
  assert.equal(b.authRequests.length, 2);
  assert.deepEqual(JSON.parse(b.authRequests[1].body), { password: testPassword });
  assert.equal(b.timers.size, 0);
  assert.doesNotMatch(source, /localStorage|sessionStorage|postMessage|setInterval/);
  await settle(); assert.equal(b.requests.length, 1);
});

test('storage drafts survive refresh, and edits made during save stay unsaved', async () => {
  const b=browser();await b.login(state({durable_storage:storageState(7)}));
  b.ids.get('storage-days').value='12';await b.ids.get('storage-form').emit('input');
  b.replies.push(response(state({durable_storage:storageState(7)})));await b.ids.get('refresh').emit('click');assert.equal(b.ids.get('storage-days').value,'12');
  const save=deferred();b.replies.push(save.promise);const sent=b.ids.get('storage-form').emit('submit');await settle();
  b.ids.get('storage-days').value='14';await b.ids.get('storage-form').emit('input');save.resolve(response(storageState(12)));await sent;await settle();
  assert.equal(b.ids.get('storage-days').value,'14');assert.match(b.ids.get('notice').textContent,/尚未保存/);
  b.replies.push(response(storageState(14)));await b.ids.get('storage-form').emit('submit');assert.match(b.ids.get('notice').textContent,/存储设置已保存/);
});

test('storage capacity warnings explain identity retention and incomplete reads', async () => {
  const b = browser();
  await b.login(state({ durable_storage: { ...storageState(7), capacityKnown: true, warning: true,
    capacityMessage: '清理到期内容不会删除这些安全记录。',
    recordCapacity: [{ kind: 'owner_operations', label: '单个连接最高任务身份', used: 4096, limit: 4096, warning: true, full: true }] } }));
  assert.equal(b.ids.get('storage-current').textContent, '安全记录已满');
  assert.match(b.ids.get('storage-details').textContent, /单个连接最高任务身份/);
  assert.match(b.ids.get('storage-details').textContent, /不会删除/);
  b.replies.push(response(state({ durable_storage: { ...storageState(7), capacityKnown: false, warning: true, capacityMessage: '请刷新重试' } })));
  await b.ids.get('refresh').emit('click');
  assert.equal(b.ids.get('storage-current').textContent, '容量读取失败');
  assert.match(b.ids.get('storage-details').textContent, /请刷新重试/);
});

test('partial device cleanup displays the persisted outcome and recovery direction', async () => {
  const b = browser();
  await b.login(state({ device_cleanup: { unpaired_days: 7, paired_days: 0, last_run_at: 1, last_removed: 1, last_error: '电脑已清理，绑定组记录尚未同步；请再次清理。' } }));
  assert.equal(b.ids.get('cleanup-current').textContent, '清理未完成');
  assert.match(b.ids.get('cleanup-details').textContent, /Bot 手机绑定/);
  assert.match(b.ids.get('cleanup-details').textContent, /绑定组记录尚未同步/);
});

test('leaving the page clears sensitive Bot binding and storage state', async () => {
  const b=browser();await b.login(state({durable_storage:storageState(7)}));
  b.ids.get('bot-binding-value').value='private-fixture-binding';b.ids.get('bot-binding-result').hidden=false;b.ids.get('bot-groups').append(new Element());
  b.events.get('pagehide')();
  assert.equal(b.ids.get('bot-binding-value').value,'');assert.equal(b.ids.get('bot-binding-result').hidden,true);assert.equal(b.ids.get('bot-groups').children.length,0);
  assert.equal(b.ids.get('storage-days').value,'');assert.equal(b.ids.get('storage-details').textContent,'');
});

test('global notices retain their floating placement through empty, success and error updates', async () => {
  const b = browser(); await settle();
  const notice = b.ids.get('notice');
  assert.equal(notice.textContent, '', 'an unauthenticated session must not leave an empty toast message');
  assert.match(notice.className, /\bglobal-notice\b/);
  await b.login();
  assert.match(notice.textContent, /状态已刷新/);
  assert.match(notice.className, /\bglobal-notice\b/);
  assert.doesNotMatch(notice.className, /\berror\b/);
  b.replies.push(response({ error: '测试连接暂不可用' }, 503));
  await b.ids.get('refresh').emit('click');
  assert.match(notice.textContent, /测试连接暂不可用/);
  assert.match(notice.className, /\bglobal-notice\b/);
  assert.match(notice.className, /\berror\b/);
});

test('self-hosted Hub shows a copyable direct address without relay credentials', async () => {
  const b = browser(); await b.login();
  assert.equal(b.ids.get('hub-address').textContent, 'https://hub.example.com/salcara-hub');
  assert.match(markup, /无需 Sub2API API Key/);
  assert.match(markup, /一次性设备登记码/);
  assert.match(markup, /手机二维码由已登记电脑生成/);
  await b.ids.get('copy-hub-address').emit('click');
  assert.deepEqual(b.clipboardWrites, ['https://hub.example.com/salcara-hub']);
});

test('workspace shell appears while the first state request is still loading', async () => {
  const b = browser(); await settle();
  const initialState = deferred();
  b.authReplies.push(response({ csrf_token: testCSRF }));
  b.replies.push(initialState.promise);
  b.ids.get('admin-password').value = testPassword;
  const login = b.ids.get('login-form').emit('submit');
  await settle();
  assert.equal(b.ids.get('workspace').hidden, false);
  assert.equal(b.ids.get('health').textContent, '尚未认证');
  initialState.resolve(response(state()));
  await login;
  assert.equal(b.ids.get('health').textContent, 'Hub 已连接');
});

test('restored session shows the page before background state loading finishes', async () => {
  const b = browser({ csrf_token: testCSRF });
  const initialState = deferred();
  b.replies[0] = initialState.promise;
  await settle();
  assert.equal(b.ids.get('workspace').hidden, false);
  assert.equal(b.requests.length, 1);
  initialState.resolve(response(state()));
  await settle();
  assert.equal(b.ids.get('health').textContent, 'Hub 已连接');
});

test('station controls submit the current revision and selected API/official scope', async () => {
  const b = browser(); await b.login(stationState());
  assert.equal(b.ids.get('remote-api-bases').value, 'https://a.example/team');
  assert.equal(b.ids.get('remote-api-bases').disabled, false);
  b.ids.get('remote-access-mode').value = 'restricted';
  b.ids.get('remote-api-enabled').checked = false;
  b.ids.get('remote-official-enabled').checked = true;
  await b.ids.get('remote-access-form').emit('input');
  assert.equal(b.ids.get('remote-api-bases').disabled, true);
  b.replies.push(response({ remote_access_revision: 'station-r2', message: '已保存' }), response(stationState({ remote_access_mode: 'restricted', remote_access_revision: 'station-r2', remote_access_station: { version: 1, apiBases: [], allowOfficialAccounts: true } })));
  await b.ids.get('remote-access-form').emit('submit');
  assert.equal(b.requests[1].path, '../_admin/v1/remote-access');
  assert.equal(b.requests[1].headers['X-Salcara-CSRF'], testCSRF);
  assert.deepEqual(JSON.parse(b.requests[1].body), { mode: 'restricted', confirm: true, expectedRevision: 'station-r1', station: { version: 2, apiBases: [], externalApiBases: [], reverseProxyApiBases: ['https://a.example/team'], allowOfficialAccounts: true } });
  assert.equal(b.ids.get('remote-api-bases').value, '');
  assert.equal(b.ids.get('remote-official-enabled').checked, true);
  assert.match(b.ids.get('remote-access-current').textContent, /来源签名验证/);
});

test('v2 external API and reverse-proxy authorizations stay independent and clear on logout', async () => {
  const b = browser();
  const station = { version: 2, apiBases: ['https://own.example'], externalApiBases: ['https://external.example/team'], reverseProxyApiBases: ['https://proxy.example/team'], allowOfficialAccounts: false };
  await b.login(stationState({ remote_access_station: station, remote_access_sources: [
    { id: 'external_api', enabled: true, status: 'issuer_configured', addressCount: 1, issuerCoveredCount: 1 },
    { id: 'reverse_proxy', enabled: true, status: 'issuer_missing', addressCount: 1, issuerCoveredCount: 0 },
  ] }));
  assert.equal(b.ids.get('remote-external-bases').value, 'https://external.example/team');
  assert.equal(b.ids.get('remote-proxy-bases').value, 'https://proxy.example/team');
  assert.match(b.ids.get('remote-proxy-status').textContent, /0\/1/);
  assert.match(b.ids.get('remote-external-status').textContent, /仍需验证/);
  b.ids.get('remote-external-enabled').checked = false;
  await b.ids.get('remote-access-form').emit('input');
  assert.equal(b.ids.get('remote-external-bases').disabled, true);
  assert.equal(b.ids.get('remote-proxy-bases').disabled, false);
  b.replies.push(response({ remote_access_revision: 'r2' }), response(stationState({ remote_access_revision: 'r2', remote_access_station: { ...station, externalApiBases: [] } })));
  await b.ids.get('remote-access-form').emit('submit');
  const submitted = JSON.parse(b.requests[1].body).station;
  assert.deepEqual(submitted, { ...station, externalApiBases: [] });
  b.events.get('pagehide')();
  assert.equal(b.ids.get('remote-external-bases').value, '');
  assert.equal(b.ids.get('remote-proxy-bases').value, '');
  assert.equal(b.ids.get('remote-proxy-enabled').checked, false);
});

test('enabled external/proxy categories require an explicit address and never mean unrestricted access', async () => {
  const b = browser(); await b.login(stationState({ remote_access_station: { version: 2, apiBases: [], allowOfficialAccounts: false } }));
  b.ids.get('remote-external-enabled').checked = true;
  await b.ids.get('remote-access-form').emit('input');
  await b.ids.get('remote-access-form').emit('submit');
  assert.equal(b.requests.length, 1);
  assert.match(b.ids.get('notice').textContent, /每个启用/);
  b.ids.get('remote-external-bases').value = 'https://external.example';
  b.ids.get('remote-proxy-enabled').checked = true;
  await b.ids.get('remote-access-form').emit('input');
  await b.ids.get('remote-access-form').emit('submit');
  assert.equal(b.requests.length, 1);
});

test('a stale station draft keeps its revision until an explicit reload replaces it', async () => {
  const b = browser(); await b.login(stationState());
  b.ids.get('remote-api-bases').value = 'https://draft.example/team';
  await b.ids.get('remote-access-form').emit('input');
  b.replies.push(response(stationState({ remote_access_revision: 'concurrent-r2', remote_access_station: { version: 1, apiBases: [], allowOfficialAccounts: false } })));
  await b.ids.get('refresh').emit('click');
  assert.equal(b.ids.get('remote-api-bases').value, 'https://draft.example/team');
  b.replies.push(response({ error: '设置已变化' }, 409));
  await b.ids.get('remote-access-form').emit('submit');
  assert.equal(JSON.parse(b.requests.at(-1).body).expectedRevision, 'station-r1');
  assert.equal(b.ids.get('remote-api-bases').value, 'https://draft.example/team');
  assert.match(b.ids.get('notice').textContent, /设置已变化/);
  assert.equal(b.requests.length, 3);
  assert.equal(b.ids.get('remote-access-reload').disabled, false);
  b.replies.push(response(stationState({ remote_access_revision: 'concurrent-r2', remote_access_station: { version: 1, apiBases: ['https://current.example/team'], allowOfficialAccounts: false } })));
  await b.ids.get('remote-access-reload').emit('click');
  assert.equal(b.ids.get('remote-api-bases').value, 'https://current.example/team');
  assert.equal(b.ids.get('remote-access-reload').disabled, true);
  b.ids.get('remote-official-enabled').checked = true;
  await b.ids.get('remote-access-form').emit('input');
  b.replies.push(response({ remote_access_revision: 'station-r3' }), response(stationState({ remote_access_revision: 'station-r3', remote_access_station: { version: 1, apiBases: ['https://current.example/team'], allowOfficialAccounts: true } })));
  await b.ids.get('remote-access-form').emit('submit');
  const submitted = JSON.parse(b.requests.at(-2).body);
  assert.equal(submitted.expectedRevision, 'concurrent-r2');
  assert.deepEqual(submitted.station.apiBases, ['https://current.example/team']);
  assert.equal(b.ids.get('remote-official-enabled').checked, true);
});

test('a failed explicit station reload preserves the draft and its original revision', async () => {
  const b = browser(); await b.login(stationState());
  b.ids.get('remote-api-bases').value = 'https://draft.example/team';
  await b.ids.get('remote-access-form').emit('input');
  b.replies.push(response({ error: '暂时不可用' }, 503));
  await b.ids.get('remote-access-reload').emit('click');
  assert.equal(b.ids.get('remote-api-bases').value, 'https://draft.example/team');
  assert.equal(b.ids.get('remote-access-reload').disabled, false);
  b.replies.push(response(stationState({ remote_access_revision: 'concurrent-r2' })));
  await b.ids.get('refresh').emit('click');
  b.replies.push(response({ error: '设置已变化' }, 409));
  await b.ids.get('remote-access-form').emit('submit');
  assert.equal(JSON.parse(b.requests.at(-1).body).expectedRevision, 'station-r1');
  assert.equal(b.ids.get('remote-api-bases').value, 'https://draft.example/team');
});

test('deployment locks station controls and session closure clears the draft', async () => {
  const b = browser(); await b.login(stationState({ remote_access_locked: true }));
  for (const id of ['remote-access-mode', 'remote-api-bases', 'remote-api-enabled', 'remote-official-enabled', 'remote-access-save']) assert.equal(b.ids.get(id).disabled, true);
  await b.ids.get('remote-access-form').emit('submit');
  assert.equal(b.requests.length, 1);
  b.events.get('pagehide')();
  assert.equal(b.ids.get('remote-api-bases').value, '');
  assert.equal(b.ids.get('remote-official-enabled').checked, false);
  assert.equal(b.ids.get('remote-access-save').disabled, true);
});

test('resource mode requires confirmation and sends only a named preset', async () => {
  const b = browser(); await b.login();
  const buttons = b.ids.get('mode-options').children;
  assert.equal(buttons.length, 3); assert.equal(buttons[0].disabled, true);
  assert.match(b.ids.get('mode-details').textContent, /256/);
  await buttons[1].emit('click');
  assert.equal(b.ids.get('mode-dialog').open, true);
  assert.equal(b.requests.length, 1);
  b.replies.push(response({ resource_mode: 'balanced' }), response(state({ resource_mode: 'balanced' })));
  await b.ids.get('mode-form').emit('submit');
  assert.equal(b.requests[1].path, '../_admin/v1/resource-mode');
  assert.equal(b.requests[1].method, 'POST');
  assert.equal(b.requests[1].headers['X-Salcara-CSRF'], testCSRF);
  assert.deepEqual(JSON.parse(b.requests[1].body), { mode: 'balanced', confirm: true });
  assert.match(b.ids.get('mode-current').textContent, /均衡/);
  assert.equal(b.ids.get('mode-options').children[1].disabled, true);
  assert.equal(b.ids.get('mode-options').children[0].disabled, false);
  assert.equal(b.timers.size, 0);
});

test('device enrollment claim is minted only in the authenticated admin session and cleared on logout', async () => {
  const b = browser(); await b.login();
  const claim = 'a'.repeat(64), expires = '2026-10-26T00:00:00Z';
  b.replies.push(response({ claim, expires_at: expires }));
  await b.ids.get('create-enrollment').emit('click');
  assert.equal(b.requests[1].path, '../_admin/v1/enrollment-claims');
  assert.equal(b.requests[1].headers['X-Salcara-CSRF'], testCSRF);
  assert.deepEqual(JSON.parse(b.requests[1].body), { confirm: true });
  assert.equal(b.ids.get('enrollment-value').value, claim);
  assert.equal(b.ids.get('enrollment-result').hidden, false);
  assert.match(b.ids.get('enrollment-expiry').textContent, /有效期至/);
  b.authReplies.push(response({ ok: true }));
  await b.ids.get('logout').emit('click');
  assert.equal(b.ids.get('enrollment-value').value, '');
  assert.equal(b.ids.get('enrollment-result').hidden, true);
});

test('canceling a preset never changes the server and unknown presets are not rendered', async () => {
  const b = browser(); await b.login(state({ resource_modes: [...profiles, { id: 'custom', label: '<script>bad</script>' }] }));
  assert.equal(b.ids.get('mode-options').children.length, 3);
  await b.ids.get('mode-options').children[2].emit('click'); await b.ids.get('cancel-mode').emit('click');
  await b.ids.get('mode-form').emit('submit');
  assert.equal(b.requests.length, 1); assert.equal(b.ids.get('mode-dialog').open, false);
});

test('mode durability warning is not reported as an unqualified saved success', async () => {
  const b = browser(); await b.login(); await b.ids.get('mode-options').children[1].emit('click');
  b.replies.push(response({ resource_mode: 'balanced', durability_warning: true }), response(state({ resource_mode: 'balanced', audit: [{ action: 'resource-mode', reason: 'balanced', actor: 'standalone-admin' }] })));
  await b.ids.get('mode-form').emit('submit');
  assert.match(b.ids.get('notice').textContent, /配置同步未完成/);
  assert.match(b.ids.get('audit').textContent, /切换运行模式 · 服务策略 · 均衡/);
});

test('pagehide during asynchronous JSON decoding cannot repopulate a new login', async () => {
  const b = browser(), parsed = deferred(); await b.login();
  b.replies.push({ status: 200, ok: true, json: () => parsed.promise });
  const oldRefresh = b.ids.get('refresh').emit('click'); await settle();
  b.events.get('pagehide')();
  await b.login(state({ resource_mode: 'balanced', standalone_version: 'new-session' }));
  parsed.resolve(state({ resource_mode: 'performance', standalone_version: 'stale-session', devices: [{ name: 'must not display' }] }));
  await oldRefresh;
  assert.match(b.ids.get('version').textContent, /new-session/);
  assert.doesNotMatch(b.ids.get('devices').textContent, /must not display/);
  assert.match(b.ids.get('mode-current').textContent, /均衡/);
  assert.equal(b.ids.get('workspace').hidden, false); assert.equal(b.ids.get('login').disabled, false);
});

test('reload restores the opaque cookie session without retaining a password', async () => {
  const b = browser({ csrf_token: testCSRF }); await settle();
  assert.equal(b.authRequests.length, 1);
  assert.equal(b.authRequests[0].path, '../_admin/v1/auth/session');
  assert.equal(b.requests.length, 1);
  assert.equal(b.ids.get('workspace').hidden, false);
  assert.equal(b.ids.get('admin-password').value, '');
  assert.equal(b.timers.size, 0);
});

test('back-forward cache return rechecks the server session before showing data', async () => {
  const b = browser(); await b.login(); b.events.get('pagehide')();
  assert.equal(b.ids.get('workspace').hidden, true);
  b.authReplies.push(response({ csrf_token: testCSRF })); b.replies.push(response(state()));
  b.events.get('pageshow')({ persisted: true }); await settle();
  assert.equal(b.ids.get('workspace').hidden, false);
  assert.equal(b.authRequests.at(-1).path, '../_admin/v1/auth/session');
  assert.equal(b.ids.get('admin-password').value, '');
});

test('failed password login never displays privileged data or retains password', async () => {
  const b = browser(); await settle();
  b.authReplies.push(response({}, 401));
  b.ids.get('admin-password').value = testPassword;
  await b.ids.get('login-form').emit('submit');
  assert.equal(b.ids.get('workspace').hidden, true);
  assert.equal(b.ids.get('admin-password').value, '');
  assert.equal(b.requests.length, 0);
  assert.match(b.ids.get('notice').textContent, /账号或密码不正确/);
  assert.match(b.ids.get('notice').className, /\bglobal-notice\b/);
  assert.equal(b.ids.get('login').disabled, false);
});

test('logout revokes the server cookie session and includes CSRF', async () => {
  const b = browser(); await b.login(); b.authReplies.push(response({ ok: true }));
  await b.ids.get('logout').emit('click');
  assert.equal(b.authRequests.at(-1).path, '../_admin/v1/auth/logout');
  assert.equal(b.authRequests.at(-1).headers['X-Salcara-CSRF'], testCSRF);
  assert.equal(b.ids.get('workspace').hidden, true);
});

test('password change validates confirmation, clears fields and returns to login', async () => {
  const b = browser(); await b.login(); await b.ids.get('change-password').emit('click');
  b.ids.get('current-password').value = testPassword;
  b.ids.get('new-password').value = 'new-password-test-only-12345';
  b.ids.get('confirm-password').value = 'mismatch';
  await b.ids.get('password-form').emit('submit');
  assert.match(b.ids.get('password-notice').textContent, /不一致/);
  assert.equal(b.authRequests.length, 2);
  b.ids.get('confirm-password').value = b.ids.get('new-password').value;
  b.authReplies.push(response({ ok: true, durability_warning: false }));
  await b.ids.get('password-form').emit('submit');
  const change = b.authRequests.at(-1);
  assert.equal(change.path, '../_admin/v1/auth/password');
  assert.equal(change.headers['X-Salcara-CSRF'], testCSRF);
  assert.deepEqual(JSON.parse(change.body), { current_password: testPassword, new_password: 'new-password-test-only-12345', username: 'admin' });
  for (const id of ['current-password', 'new-password', 'confirm-password']) assert.equal(b.ids.get(id).value, '');
  assert.equal(b.ids.get('workspace').hidden, true);
  assert.equal(b.ids.get('password-dialog').open, false);
  assert.match(b.ids.get('notice').textContent, /账号和密码已更新/);
});

test('session expiry hides privileged data and does not auto retry a mutation', async () => {
  const b = browser(); await b.login();
  b.replies.push(response({}, 401)); await b.ids.get('refresh').emit('click');
  assert.equal(b.ids.get('workspace').hidden, true);
  assert.equal(b.ids.get('devices').children.length, 0);
  assert.match(b.ids.get('notice').textContent, /登录已过期/);
  assert.equal(b.requests.length, 2);
});

test('signed update target requires explicit confirmation; 202 does not mean finished', async () => {
  const b = browser(); await b.login();
  b.replies.push(response({ status: 'available', latest_version: '0.4.1', sha256: 'a'.repeat(64), release_notes: '<img onerror=alert(1)>' }));
  await b.ids.get('check-update').emit('click');
  assert.deepEqual(JSON.parse(b.requests[1].body), {});
  assert.match(b.ids.get('update-notes').textContent, /<img onerror/);
  assert.equal(b.ids.get('update-notes').children.length, 0);
  await b.ids.get('install-update').emit('click'); assert.equal(b.ids.get('update-dialog').open, true);
  b.replies.push(response({ status: 'updating', current_version: '0.4.0', latest_version: '0.4.1' }, 202));
  await b.ids.get('update-form').emit('submit');
  assert.deepEqual(JSON.parse(b.requests[2].body), { version: '0.4.1', sha256: 'a'.repeat(64), confirm: true });
  assert.match(b.ids.get('notice').textContent, /不等于更新完成/);
  assert.equal(b.ids.get('install-update').disabled, true);
  assert.equal(b.requests.length, 3); assert.equal(b.timers.size, 1);
});

const updateState = (extra = {}) => ({ configured: true, auto_check: true, check_interval_seconds: 21600, checked_at: '2026-11-08T01:00:00Z', next_check_at: '2026-11-08T07:00:00Z', current_version: '0.5.2', latest_version: '0.5.3', sha256: 'a'.repeat(64), status: 'available', message: '发现签名新版', ...extra });

test('cached automatic detection appears on login and polls only local status', async () => {
  const b = browser(); await b.login(state({ update_status: updateState() }));
  assert.equal(b.ids.get('update-banner').hidden, false);
  assert.equal(b.ids.get('update-badge').hidden, false);
  assert.match(b.ids.get('update-schedule').textContent, /6 小时/);
  assert.equal(b.ids.get('update-current').textContent, '0.5.2');
  assert.equal(b.ids.get('version').textContent, 'v0.5.2');
  assert.match(b.ids.get('version').className, /available/);
  assert.match(b.ids.get('version').attributes['aria-label'], /发现新版本 v0.5.3/);
  assert.equal(b.ids.get('install-update').disabled, false);
  assert.equal(b.requests.length, 1);
  const timer = [...b.timers][0]; assert.equal(timer.delay, 60000);
  b.replies.push(response(updateState({ status: 'current', current_version: '0.5.3' })));
  b.timers.delete(timer); timer.callback(); await settle();
  assert.equal(b.requests[1].path, '../_admin/v1/update/status');
  assert.equal(b.requests[1].method, 'GET');
  assert.equal(b.ids.get('update-banner').hidden, true);
  assert.equal(b.ids.get('install-update').disabled, true);
  assert.equal(b.ids.get('version').textContent, 'v0.5.3');
  assert.doesNotMatch(b.ids.get('version').className, /available/);
  b.events.get('pagehide')(); assert.equal(b.timers.size, 0);
});

test('update entry loads state; a dialog keeps the exact version the owner saw', async () => {
  const b = browser(); await b.login(state({ update_status: updateState() }));
  b.replies.push(response(updateState())); await b.ids.get('view-update').emit('click');
  assert.equal(b.ids.get('page-updates').hidden, false);
  assert.equal(b.ids.get('page-system').hidden, true);
  assert.equal(b.ids.get('nav-updates').attributes['aria-current'], 'page');
  await b.ids.get('install-update').emit('click');
  b.replies.push(response(updateState({ latest_version: '0.5.4', sha256: 'b'.repeat(64) })));
  await b.ids.get('update-result').emit('click');
  assert.match(b.ids.get('update-target').textContent, /0.5.3/);
  b.replies.push(response({ error: '版本已变化，请重新检查' }, 409));
  await b.ids.get('update-form').emit('submit');
  assert.deepEqual(JSON.parse(b.requests.at(-1).body), { version: '0.5.3', sha256: 'a'.repeat(64), confirm: true });
  assert.equal(b.ids.get('install-update').disabled, true);
  assert.match(b.ids.get('update-error').textContent, /重新检查/);
  assert.equal(b.requests.filter(r => r.path.endsWith('update/apply')).length, 1);
});

test('checks in progress and disabled deployments never offer install', async () => {
  const b = browser(); await b.login(state({ update_status: updateState({ checking: true }) }));
  assert.equal(b.ids.get('install-update').disabled, true);
  assert.equal(b.ids.get('check-update').disabled, true);
  assert.equal(b.ids.get('update-label').textContent, '正在检查');
  b.replies.push(response(updateState({ auto_check: false, configured: false, status: 'unavailable', latest_version: '', sha256: '', next_check_at: '' })));
  await b.ids.get('update-result').emit('click');
  assert.equal(b.ids.get('check-update').disabled, true);
  assert.equal(b.ids.get('install-update').disabled, true);
  assert.equal(b.ids.get('update-banner').hidden, true);
  assert.equal(b.timers.size, 0);
});

test('background update response after logout cannot restore candidates or polling', async () => {
  const b = browser(); await b.login(state({ update_status: updateState() }));
  const waiting = deferred(); b.replies.push(waiting.promise);
  const timer = [...b.timers][0]; b.timers.delete(timer); timer.callback(); await settle();
  b.events.get('pagehide')(); waiting.resolve(response(updateState())); await settle();
  assert.equal(b.ids.get('update-banner').hidden, true);
  assert.equal(b.ids.get('install-update').disabled, true);
  assert.equal(b.timers.size, 0);
});

test('an older overview response cannot undo an accepted update', async () => {
  const b = browser(); await b.login(state({ update_status: updateState() }));
  const waiting = deferred(); b.replies.push(waiting.promise);
  const refresh = b.ids.get('refresh').emit('click'); await settle();
  await b.ids.get('install-update').emit('click');
  b.replies.push(response(updateState({ status: 'updating' }), 202));
  await b.ids.get('update-form').emit('submit');
  waiting.resolve(response(state({ update_status: updateState() }))); await refresh; await settle();
  assert.equal(b.ids.get('update-label').textContent, '正在更新');
  assert.equal(b.ids.get('install-update').disabled, true);
  assert.equal([...b.timers][0].delay, 2000);
});

test('an uncertain apply retries status reads without resubmitting even on an older launcher', async () => {
  const b = browser(); await b.login(state({ update_status: updateState({ auto_check: false }) }));
  await b.ids.get('install-update').emit('click');
  b.replies.push(response({ error: '连接暂时中断' }, 502));
  await b.ids.get('update-form').emit('submit');
  assert.equal(b.ids.get('install-update').disabled, true);
  const timer = [...b.timers][0]; assert.equal(timer.delay, 2000);
  b.replies.push(response(updateState({ auto_check: false, status: 'current', current_version: '0.5.3' })));
  b.timers.delete(timer); timer.callback(); await settle();
  assert.equal(b.requests.at(-1).path, '../_admin/v1/update/status');
  assert.equal(b.requests.filter(r => r.path.endsWith('update/apply')).length, 1);
  assert.equal(b.ids.get('update-current').textContent, '0.5.3');
  assert.equal(b.timers.size, 0);
});

test('cancelled update confirmation never submits an installation', async () => {
  const b = browser(); await b.login(state({ update_status: updateState() }));
  await b.ids.get('install-update').emit('click');
  await b.ids.get('cancel-update').emit('click');
  await b.ids.get('update-form').emit('submit');
  assert.equal(b.ids.get('update-dialog').open, false);
  assert.equal(b.requests.length, 1);
});

test('highlighted brand version opens the exact update confirmation directly', async () => {
  const b = browser(); await b.login(state({ update_status: updateState() }));
  await b.ids.get('version').emit('click');
  assert.equal(b.ids.get('update-dialog').open, true);
  assert.match(b.ids.get('update-target').textContent, /0.5.3/);
  assert.equal(b.requests.length, 1, 'shortcut should not check the feed or install before confirmation');
  b.replies.push(response(updateState({ status: 'updating' }), 202));
  await b.ids.get('update-form').emit('submit');
  assert.equal(b.ids.get('page-updates').hidden, false);
  assert.equal(b.requests.at(-1).path, '../_admin/v1/update/apply');
  assert.deepEqual(JSON.parse(b.requests.at(-1).body), { version: '0.5.3', sha256: 'a'.repeat(64), confirm: true });
});

test('restricted administrators cannot see or poll the update controls', async () => {
  const b = browser({ csrf_token: testCSRF, role: 'viewer' }); await settle();
  assert.equal(b.ids.get('nav-updates').hidden, true);
  await b.ids.get('nav-updates').emit('click');
  assert.equal(b.ids.get('page-updates').hidden, true);
  assert.equal(b.ids.get('check-update').disabled, true);
  assert.equal(b.ids.get('version').disabled, true);
  assert.equal(b.timers.size, 0);
});

test('device controls preserve device_ref and escape display-only content', async () => {
  const b = browser(); await b.login(state({ devices: [{ name: '<script>not HTML</script>', ref: 'test-device-ref', online: true }], total: 1 }));
  const row = b.ids.get('devices').children[0];
  assert.equal(row.children[0].children[0].textContent, '<script>not HTML</script>');
  assert.equal(row.children[0].children[0].children.length, 0);
  const buttons = node => [node, ...node.children.flatMap(buttons)].filter(child => child.tagName === 'BUTTON');
  await buttons(row.children[4]).find(button => button.textContent === '封禁').emit('click'); assert.equal(b.ids.get('operation').open, true);
  b.ids.get('reason').value = '演示封禁';
  b.replies.push(response({ ok: true }), response(state()));
  await b.ids.get('operation-form').emit('submit'); await settle();
  assert.deepEqual(JSON.parse(b.requests[1].body), { action: 'ban', device_ref: 'test-device-ref', reason: '演示封禁', confirm: true });
});

test('unconfigured updater stays disabled and logout/pagehide abort active requests', async () => {
  const b = browser(); await b.login(state({ update_configured: false }));
  assert.equal(b.ids.get('check-update').disabled, true);
  assert.equal(b.ids.get('install-update').disabled, true);
  const waiting = deferred(); b.replies.push(waiting.promise);
  const pending = b.ids.get('refresh').emit('click'); await settle();
  const request = b.requests[1]; b.events.get('pagehide')();
  assert.equal(request.signal.aborted, true);
  waiting.resolve(response(state())); await pending;
  assert.equal(b.ids.get('workspace').hidden, true);
  assert.equal(b.ids.get('mode-options').children.length, 0);
});
