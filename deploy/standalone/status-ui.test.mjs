import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const root = new URL('../../internal/standalone/web/', import.meta.url);
const source = await readFile(new URL('status.js', root), 'utf8');
const sourcesModule = await readFile(new URL('public-sources.js', root), 'utf8');
const markup = await readFile(new URL('status.html', root), 'utf8');
const response = (data = { status: 'available' }, status = 200) => ({ ok: status >= 200 && status < 300, status, text: async () => JSON.stringify(data) });
function deferred() { let resolve, reject; const promise = new Promise((yes, no) => { resolve = yes; reject = no; }); return { promise, resolve, reject }; }
async function settle() { for (let i = 0; i < 5; i++) await new Promise(resolve => setImmediate(resolve)); }
class Element {
  constructor() { this.text = ''; this.className = ''; this.children = []; this.disabled = false; this.checked = false; this.hidden = false; this.style = {}; this.handlers = new Map(); }
  set textContent(value) { this.text = String(value); this.children = []; }
  get textContent() { return this.text + this.children.map(child => child.textContent).join(''); }
  append(...children) { this.children.push(...children); }
  replaceChildren(...children) { this.text = ''; this.children = children; }
  addEventListener(type, handler) { this.handlers.set(type, handler); }
  async emit(type) { if (type === 'click' && this.disabled) return; this.handlers.get(type)?.(); await settle(); }
}
function browser({ online = true, hidden = false, initial } = {}) {
  const ids = new Map([...markup.matchAll(/id="([^"]+)"/g)].map(match => [match[1], new Element()]));
  ids.get('auto-check').checked = true;
  const clock = { now: 0 }, timers = new Set(), requests = [], replies = initial === undefined ? [] : [initial];
  const events = new Map(), documentEvents = new Map();
  class FixtureDate extends Date { constructor(...args) { super(...(args.length ? args : [1700000000000 + clock.now])); } static now() { return 1700000000000 + clock.now; } }
  const document = { hidden, getElementById: id => { assert.ok(ids.has(id), `unknown HTML id: ${id}`); return ids.get(id); }, createElement: () => new Element(), addEventListener: (name, fn) => documentEvents.set(name, fn) };
  const navigator = { onLine: online };
  const context = vm.createContext({
    document, navigator, performance: { now: () => clock.now }, Date: FixtureDate, AbortController,
    fetch: (path, options) => {
      requests.push({ path, ...options });
      assert.ok(replies.length, 'unexpected request');
      return Promise.resolve(replies.shift());
    },
    setTimeout: (callback, delay) => { const timer = { callback, delay }; timers.add(timer); return timer; },
    clearTimeout: timer => timers.delete(timer),
    addEventListener: (name, fn) => events.set(name, fn),
  });
  vm.runInContext(sourcesModule, context, { filename: 'public-sources.js' });
  vm.runInContext(source, context, { filename: 'status.js' });
  return {
    ids, requests, replies, timers, clock, document, navigator,
    $: id => ids.get(id),
    async fireTimer(delay) { const timer = [...timers].find(value => value.delay === delay); assert.ok(timer, `missing ${delay}ms timer`); timers.delete(timer); clock.now += delay; timer.callback(); await settle(); },
    async emit(name) { events.get(name)?.(); await settle(); },
    async visibility(hidden) { document.hidden = hidden; documentEvents.get('visibilitychange')?.(); await settle(); },
    async online(value) { navigator.onLine = value; events.get(value ? 'online' : 'offline')?.(); await settle(); },
  };
}

function history(b) { return b.$('status-history').children; }
function timerCount(b, delay) { return [...b.timers].filter(timer => timer.delay === delay).length; }

test('public status measures the complete GET response with the monotonic clock and omits credentials', async () => {
  const body = deferred();
  const b = browser({ initial: { ...response(), text: () => body.promise } });
  await settle();
  assert.equal(b.$('health').textContent, '正在检测');
  assert.equal(b.$('check').disabled, true);
  b.clock.now = 123.6;
  body.resolve('{"status":"available"}'); await settle();
  assert.equal(b.$('request-duration').textContent, '124 ms');
  assert.equal(b.$('health').textContent, '可用');
  assert.equal(b.$('check').disabled, false);
  assert.equal(b.$('last-success').dateTime, b.$('checked-at').dateTime);
  assert.equal(history(b).length, 1);
  assert.equal(history(b)[0].children[2].textContent, '124 ms');
  assert.equal(b.requests.length, 1);
  assert.equal(b.requests[0].path, './statusz');
  assert.equal(b.requests[0].credentials, 'omit');
  assert.equal(b.requests[0].cache, 'no-store');
  assert.equal(b.requests[0].redirect, 'error');
  assert.equal(b.requests[0].referrerPolicy, 'no-referrer');
  assert.equal(b.requests[0].headers, undefined);
  assert.equal(timerCount(b, 8000), 0);
  assert.equal(timerCount(b, 30000), 1);
  assert.doesNotMatch(source, /localStorage|sessionStorage|innerHTML|_admin|\/healthz|\/v1\/ping|setInterval/);
  assert.match(markup, /此状态只表示 Hub 接口当前可响应/);
  assert.match(markup, /不是手机到电脑的业务全链路延迟/);
  assert.match(markup, /不是服务器历史/);
});

test('HTTP 503 is a service result while arbitrary response content is never displayed', async () => {
  const b = browser({ initial: response({ error: '<img src=x onerror=alert(1)> private-node.example' }, 503) });
  b.clock.now = 51; await settle();
  assert.equal(b.$('health').textContent, '服务异常');
  assert.match(b.$('summary').textContent, /HTTP 503/);
  assert.equal(b.$('request-duration').textContent, '51 ms');
  assert.equal(history(b).length, 1);
  assert.doesNotMatch([...b.ids.values()].map(node => node.textContent).join(' '), /private-node|onerror/);
  assert.equal(b.$('last-success').dateTime, undefined);
});

test('invalid JSON, unknown status and explicit unavailability cannot be presented as available', async () => {
  for (const initial of [response({ wrong: 'available' }), { ...response(), text: async () => '<html>gateway</html>' }, response(null)]) {
    const b = browser({ initial }); await settle();
    assert.equal(b.$('health').textContent, '响应异常');
    assert.match(b.$('summary').textContent, /无法确认/);
  }
  const b = browser({ initial: response({ status: 'unavailable' }) }); await settle();
  assert.equal(b.$('health').textContent, '服务异常');
  assert.doesNotMatch(b.$('summary').textContent, /HTTP 200/);
});

test('a network failure is uncertainty and preserves only the separately labeled last success', async () => {
  const b = browser({ initial: response() }); await settle();
  const successfulAt = b.$('last-success').dateTime;
  const pending = deferred(); b.replies.push(pending.promise);
  await b.$('check').emit('click');
  b.clock.now = 87;
  pending.reject(new Error('private upstream address')); await settle();
  assert.equal(b.$('health').textContent, '无法确认');
  assert.match(b.$('summary').textContent, /不能据此判断 Hub 已停止/);
  assert.equal(b.$('request-duration').textContent, '87 ms');
  assert.equal(b.$('last-success').dateTime, successfulAt);
  assert.notEqual(b.$('checked-at').dateTime, successfulAt);
  assert.equal(history(b).length, 2);
  assert.doesNotMatch(b.$('summary').textContent, /private upstream/);
});

test('the eight second deadline bounds a request even if it ignores abort and late results are discarded', async () => {
  const pending = deferred();
  const b = browser({ initial: pending.promise });
  await b.fireTimer(8000);
  assert.equal(b.requests[0].signal.aborted, true);
  assert.equal(b.$('health').textContent, '检测超时');
  assert.equal(b.$('request-duration').textContent, '8000 ms');
  assert.equal(b.$('check').disabled, false);
  assert.equal(history(b).length, 1);
  assert.equal(timerCount(b, 30000), 1);
  pending.resolve(response()); await settle();
  assert.equal(b.$('health').textContent, '检测超时');
  assert.equal(history(b).length, 1);
  assert.equal(b.$('last-success').dateTime, undefined);
});

test('automatic detection schedules once after completion and never overlaps a request', async () => {
  const pending = deferred();
  const b = browser({ initial: pending.promise }); await settle();
  assert.equal(timerCount(b, 30000), 0);
  await b.$('check').emit('click');
  await b.emit('pageshow');
  await b.emit('online');
  await b.visibility(false);
  assert.equal(b.requests.length, 1);
  assert.equal(b.requests[0].signal.aborted, false);
  pending.resolve(response()); await settle();
  assert.equal(timerCount(b, 30000), 1);
  b.replies.push(response()); await b.fireTimer(30000);
  assert.equal(b.requests.length, 2);
  assert.equal(timerCount(b, 30000), 1);
  assert.equal(history(b).length, 2);
});

test('turning automatic detection off clears its timer while manual detection remains available', async () => {
  const b = browser({ initial: response() }); await settle();
  b.$('auto-check').checked = false; await b.$('auto-check').emit('change');
  assert.equal(b.timers.size, 0);
  assert.match(b.$('refresh-state').textContent, /已关闭/);
  b.replies.push(response()); await b.$('check').emit('click');
  assert.equal(b.requests.length, 2);
  assert.equal(history(b).length, 2);
  assert.equal(b.timers.size, 0);
  b.replies.push(response()); b.$('auto-check').checked = true; await b.$('auto-check').emit('change');
  assert.equal(b.requests.length, 3);
  assert.equal(timerCount(b, 30000), 1);
});

test('turning automatic detection off during a request prevents rescheduling without losing that result', async () => {
  const pending = deferred(); const b = browser({ initial: pending.promise });
  b.$('auto-check').checked = false; await b.$('auto-check').emit('change');
  pending.resolve(response()); await settle();
  assert.equal(b.$('health').textContent, '可用');
  assert.equal(history(b).length, 1);
  assert.equal(b.timers.size, 0);
});

test('a hidden page cancels its request, ignores its late body, and resumes once when visible', async () => {
  const body = deferred();
  const b = browser({ initial: { ...response(), text: () => body.promise } }); await settle();
  await b.visibility(true);
  assert.equal(b.requests[0].signal.aborted, true);
  assert.equal(b.timers.size, 0);
  assert.equal(b.$('check').disabled, true);
  body.resolve('{"status":"available"}'); await settle();
  assert.equal(history(b).length, 0);
  assert.equal(b.$('health').textContent, '已暂停');
  b.replies.push(response()); await b.visibility(false);
  await b.visibility(false); await b.emit('online');
  assert.equal(b.requests.length, 2);
  assert.equal(history(b).length, 1);
  assert.equal(timerCount(b, 30000), 1);
});

test('an initially hidden page makes no request until it becomes visible', async () => {
  const b = browser({ hidden: true }); await settle();
  assert.equal(b.requests.length, 0); assert.equal(b.timers.size, 0);
  assert.equal(b.$('request-chart').children.length, 0, 'no invented samples before the first check');
  assert.equal(b.$('chart-empty').hidden, false);
  b.replies.push(response()); await b.visibility(false);
  assert.equal(b.requests.length, 1); assert.equal(history(b).length, 1);
});

test('offline is distinct from service failure and online/visible events jointly resume exactly once', async () => {
  const b = browser({ online: false }); await settle();
  assert.equal(b.requests.length, 0);
  assert.equal(b.$('health').textContent, '浏览器离线');
  assert.equal(b.$('check').disabled, true);
  assert.equal(history(b).length, 0);
  await b.visibility(true);
  await b.online(true);
  assert.equal(b.requests.length, 0, 'online event must not probe a hidden page');
  b.replies.push(response()); await b.visibility(false);
  await b.online(true); await b.visibility(false);
  assert.equal(b.requests.length, 1);
  assert.equal(history(b).length, 1);
  assert.equal(timerCount(b, 30000), 1);
});

test('going offline cancels in-flight work and does not add a false failed record', async () => {
  const pending = deferred(); const b = browser({ initial: pending.promise });
  await b.online(false);
  assert.equal(b.requests[0].signal.aborted, true);
  assert.equal(b.timers.size, 0);
  pending.resolve(response()); await settle();
  assert.equal(history(b).length, 0);
  assert.equal(b.$('health').textContent, '浏览器离线');
  b.replies.push(response()); await b.online(true);
  assert.equal(b.requests.length, 2);
  assert.equal(history(b).length, 1);
});

test('manual mode stays paused across background and offline recovery', async () => {
  const b = browser({ initial: response() }); await settle();
  b.$('auto-check').checked = false; await b.$('auto-check').emit('change');
  await b.visibility(true); await b.online(false); await b.online(true); await b.visibility(false);
  assert.equal(b.requests.length, 1); assert.equal(b.timers.size, 0);
  assert.equal(b.$('check').disabled, false);
  assert.equal(b.$('health').textContent, '可用');
  assert.match(b.$('refresh-state').textContent, /已关闭/);
});

test('pagehide disposes polling and results; a back-forward restoration starts only one fresh check', async () => {
  const pending = deferred(); const b = browser({ initial: pending.promise });
  await b.emit('pagehide');
  assert.equal(b.requests[0].signal.aborted, true);
  assert.equal(b.timers.size, 0);
  pending.resolve(response()); await settle();
  assert.equal(history(b).length, 0);
  b.replies.push(response()); await b.emit('pageshow'); await b.emit('pageshow');
  assert.equal(b.requests.length, 2);
  assert.equal(history(b).length, 1);
  assert.equal(timerCount(b, 30000), 1);
});

test('history is bounded to twelve actual checks, newest first, with both successes and failures', async () => {
  const b = browser({ initial: response() }); await settle();
  b.$('auto-check').checked = false; await b.$('auto-check').emit('change');
  for (let i = 1; i <= 14; i++) {
    const pending = deferred(); b.replies.push(pending.promise);
    await b.$('check').emit('click'); b.clock.now += i;
    pending.resolve(response({}, 503)); await settle();
  }
  assert.equal(b.requests.length, 15);
  assert.equal(history(b).length, 12);
  assert.equal(history(b)[0].children[2].textContent, '14 ms');
  assert.equal(history(b)[11].children[2].textContent, '3 ms');
  assert.equal(history(b)[0].children[1].textContent, '服务异常');
  const samples = b.$('request-chart').children;
  assert.equal(samples.length, 12);
  assert.match(samples[0].title, /3 ms$/);
  assert.match(samples[11].title, /14 ms$/);
  assert.equal(samples[11].className, 'chart-sample--bad');
  assert.equal(samples[11].children[0].style.height, '100%');
  assert.equal(b.$('chart-empty').hidden, true);
  assert.equal(b.$('sample-count').textContent, '12 / 12 次');
  assert.match(b.$('chart-summary').textContent, /成功 0 次 · 未成功 12 次/);
  assert.equal(b.timers.size, 0);
});

test('trend uses measured samples, distinguishes outcomes and never adds timers', async () => {
  const pending = deferred();
  const b = browser({ initial: pending.promise });
  b.clock.now = 64; pending.resolve(response()); await settle();
  assert.equal(b.$('status-visual').className, 'status-visual is-ok');
  const failed = deferred(); b.replies.push(failed.promise);
  await b.$('check').emit('click');
  b.clock.now += 128; failed.reject(new Error('network')); await settle();
  const samples = b.$('request-chart').children;
  assert.equal(samples.length, 2);
  assert.equal(samples[0].className, 'chart-sample--ok');
  assert.equal(samples[0].children[0].style.height, '50%');
  assert.equal(samples[1].className, 'chart-sample--bad');
  assert.equal(samples[1].children[0].style.height, '100%');
  assert.equal(b.$('chart-scale').textContent, '刻度上限 128 ms');
  assert.match(b.$('chart-summary').textContent, /成功 1 次 · 未成功 1 次/);
  assert.equal(b.$('status-visual').className, 'status-visual is-warning');
  assert.equal(b.timers.size, 1);
  await b.visibility(true);
  assert.equal(b.timers.size, 0);
  assert.equal(b.$('request-chart').children.length, 2);
});

test('public source authorization refreshes with the same check and is cleared on failure or pause', async () => {
  const data = status => ({ status: 'available', sources: ['api', 'external_api', 'reverse_proxy', 'official_account'].map(id => ({ id, status })) });
  const b = browser({ initial: response(data('allowlist')) }); await settle();
  assert.equal(b.$('source-external_api-status').textContent, '按名单开放');
  assert.equal(b.$('source-enabled-count').textContent, '4 / 4');
  assert.equal(b.requests.length, 1, 'source status must not create a separate poll');
  b.replies.push(response(data('disabled'))); await b.$('check').emit('click');
  assert.equal(b.$('source-external_api-status').textContent, '未开放');
  assert.equal(b.$('source-enabled-count').textContent, '0 / 4');
  b.replies.push(response({}, 503)); await b.$('check').emit('click');
  assert.equal(b.$('source-api-status').textContent, '待重新确认');
  assert.equal(b.$('source-enabled-count').textContent, '—');
  b.replies.push(response(data('issuer_configured'))); await b.$('check').emit('click');
  assert.equal(b.$('source-api-status').textContent, '授权就绪');
  b.$('auto-check').checked = false; await b.$('auto-check').emit('change');
  await b.visibility(true); await b.visibility(false);
  assert.equal(b.$('source-api-status').textContent, '待重新确认');
  assert.equal(b.requests.length, 4);
  assert.equal(b.timers.size, 0);
});
