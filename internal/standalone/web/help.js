(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const sources = globalThis.SalcaraPublicSources;
  const address = new URL('./', location.href);
  address.username = ''; address.password = ''; address.search = ''; address.hash = '';
  $('hub-url').textContent = address.href;
  let active = null, disposed = false, pending = true;
  let latestSources = null;
  const eligible = () => !disposed && !document.hidden && navigator.onLine !== false;
  function show(title, detail, state = '') {
    $('health').textContent = title;
    $('health').className = `pill${state ? ` ${state}` : ''}`;
    $('status').textContent = detail;
  }
  function controls() {
    $('check').disabled = !eligible() || active !== null;
    $('check').textContent = active ? '检测中…' : '重新检测';
  }
  async function request(signal) {
    try {
      const response = await fetch('./v1/ping', { credentials: 'omit', cache: 'no-store', redirect: 'error', signal, referrerPolicy: 'no-referrer' });
      const data = await response.json();
      if (!response.ok || !data || data.service !== 'salcara-hub' || data.protocol !== 'salcara-remote' || data.protocolVersion !== 1) {
        return { feedback: ['响应异常', '本站未返回兼容的连接服务。', 'bad'] };
      }
      const version = data.standalone_version || data.version;
      const label = typeof version === 'string' && /^[0-9A-Za-z][0-9A-Za-z.+-]{0,63}$/.test(version) ? ` v${version}` : '';
      return { feedback: ['Hub 可连接', `本站 Hub${label} 响应正常。`, 'ok'], sources: sources.read(data.source_status), version: label.trim() };
    } catch {
      return { feedback: ['暂未连通', '当前无法确认 Hub 状态，请重新检测。', 'bad'] };
    }
  }
  async function check() {
    if (!eligible() || active) return;
    const current = { controller: new AbortController(), timer: null };
    active = current;
    show('正在检测', '正在检查本站的设备连接入口。');
    sources.render(null, 'pending');
    $('hub-version').textContent = '—';
    controls();
    const deadline = new Promise(resolve => {
      current.timer = setTimeout(() => {
        current.controller.abort();
        resolve({ feedback: ['检测超时', '10 秒内未完成检测，请稍后重试。', 'bad'] });
      }, 10000);
    });
    const result = await Promise.race([request(current.controller.signal), deadline]);
    clearTimeout(current.timer);
    if (active !== current || !eligible()) return;
    active = null;
    pending = false;
    show(...result.feedback);
    latestSources = result.sources || null;
    sources.render(result.sources, 'stale');
    $('hub-version').textContent = result.version || '—';
    const checked = new Date();
    $('home-checked-at').textContent = checked.toLocaleTimeString();
    $('home-checked-at').dateTime = checked.toISOString();
    controls();
  }
  function pause() {
    if (active) {
      clearTimeout(active.timer);
      active.controller.abort();
      active = null;
      pending = true;
      show('已暂停', '页面当前不可见或网络已断开，检测已暂停。');
    }
    sources.render(null, 'stale');
    controls();
  }
  function resume() {
    controls();
    if (eligible() && pending) void check();
    else if (navigator.onLine === false) { show('浏览器离线', '恢复联网后自动检测。'); sources.render(null, 'stale'); }
    else if (eligible() && !active) sources.render(latestSources, 'stale', true);
  }
  $('check').addEventListener('click', () => { void check(); });
  $('copy-hub-url').addEventListener('click', async () => {
    $('copy-hub-url').disabled = true;
    try {
      await navigator.clipboard.writeText(address.href);
      $('copy-status').textContent = '已复制。在手机和电脑的 Hub 地址处粘贴即可。';
    } catch {
      $('copy-status').textContent = '浏览器未允许自动复制，请选中上方地址手动复制。';
    } finally { $('copy-hub-url').disabled = false; }
  });
  document.addEventListener('visibilitychange', () => { if (document.hidden) pause(); else resume(); });
  addEventListener('offline', () => { pending = true; pause(); resume(); });
  addEventListener('online', resume);
  addEventListener('pagehide', () => { disposed = true; pending = true; pause(); });
  addEventListener('pageshow', () => {
    if (!disposed) return;
    disposed = false;
    resume();
  });
  resume();
})();
