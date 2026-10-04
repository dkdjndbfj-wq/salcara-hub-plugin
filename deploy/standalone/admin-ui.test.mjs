import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const root = new URL('../../plugins/salcara-hub/internal/standalone/web/', import.meta.url);
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
const state = (extra = {}) => ({ stats: {}, devices: [], audit: [], total: 0, standalone_version: '0.5.0', update_configured: true, resource_mode: 'economy', resource_modes: profiles, ...extra });
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
  const requests = [], replies = session ? [response(state())] : [], authRequests = [], authReplies = [session ? response(session) : response({}, 401)], timers = new Set(), events = new Map();
  const context = vm.createContext({
    document: { getElementById: id => { assert.ok(ids.has(id), `unknown HTML id ${id}`); return ids.get(id); }, createElement: tag => new Element(tag) },
    fetch: async (path, options) => { const auth = String(path).includes('/auth/'); (auth ? authRequests : requests).push({ path: String(path), ...options }); const queue = auth ? authReplies : replies; assert.ok(queue.length, 'unexpected request / background polling'); return await queue.shift(); },
    AbortController, TextEncoder, URLSearchParams, Date,
    setTimeout: callback => { const handle = { callback }; timers.add(handle); return handle; }, clearTimeout: handle => timers.delete(handle),
    addEventListener: (type, handler) => events.set(type, handler),
  });
  vm.runInContext(source, context, { filename: 'app.js' });
  return { ids, requests, replies, authRequests, authReplies, timers, events, async login(value = state()) { await settle(); authReplies.push(response({ csrf_token: testCSRF })); replies.push(response(value)); ids.get('admin-password').value = testPassword; await ids.get('login-form').emit('submit'); } };
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
  assert.match(b.ids.get('notice').textContent, /密钥不正确/);
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
  assert.deepEqual(JSON.parse(change.body), { current_password: testPassword, new_password: 'new-password-test-only-12345' });
  for (const id of ['current-password', 'new-password', 'confirm-password']) assert.equal(b.ids.get(id).value, '');
  assert.equal(b.ids.get('workspace').hidden, true);
  assert.equal(b.ids.get('password-dialog').open, false);
  assert.match(b.ids.get('notice').textContent, /密钥已更换/);
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
  assert.match(b.ids.get('update-status').textContent, /<img onerror/);
  assert.equal(b.ids.get('update-status').children.length, 0);
  await b.ids.get('install-update').emit('click'); assert.equal(b.ids.get('update-dialog').open, true);
  b.replies.push(response({ status: 'updating', current_version: '0.4.0', latest_version: '0.4.1' }, 202));
  await b.ids.get('update-form').emit('submit');
  assert.deepEqual(JSON.parse(b.requests[2].body), { version: '0.4.1', sha256: 'a'.repeat(64), confirm: true });
  assert.match(b.ids.get('notice').textContent, /不等于更新完成/);
  assert.equal(b.ids.get('install-update').disabled, true);
  assert.equal(b.requests.length, 3); assert.equal(b.timers.size, 0);
});

test('device controls preserve device_ref and escape display-only content', async () => {
  const b = browser(); await b.login(state({ devices: [{ name: '<script>not HTML</script>', ref: 'test-device-ref', online: true }], total: 1 }));
  const row = b.ids.get('devices').children[0];
  assert.equal(row.children[0].children[0].textContent, '<script>not HTML</script>');
  assert.equal(row.children[0].children[0].children.length, 0);
  await row.children[4].children[3].emit('click'); assert.equal(b.ids.get('operation').open, true);
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
