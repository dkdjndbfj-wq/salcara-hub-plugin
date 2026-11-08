(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const sources = globalThis.SalcaraPublicSources;
  const interval = 30000, timeout = 8000, historyLimit = 12;
  let active = null, nextTimer = null, generation = 0, disposed = false;
  let visible = !document.hidden, online = navigator.onLine !== false;
  let latest = null;
  const records = [];

  function clearNext() {
    if (nextTimer !== null) clearTimeout(nextTimer);
    nextTimer = null;
  }
  function cancelActive() {
    generation++;
    if (!active) return;
    clearTimeout(active.timer);
    active.controller.abort();
    active = null;
  }
  function eligible() { return !disposed && visible && online; }
  function durationSince(start) {
    const value = performance.now() - start;
    return Number.isFinite(value) && value >= 0 ? value : null;
  }
  function durationText(value) { return value === null ? '—' : `${Math.round(value)} ms`; }
  function stamp(element, at) {
    element.textContent = at.toLocaleString();
    element.dateTime = at.toISOString();
  }
  function description(outcome) {
    switch (outcome.kind) {
      case 'ok': return { title: '可用', detail: 'Hub 状态接口响应正常。', cls: 'pill ok' };
      case 'service': return { title: '服务异常', detail: outcome.status === 503 ? '本站返回服务暂不可用（HTTP 503），请稍后重试。' : `本站状态接口返回异常${outcome.status ? `（HTTP ${outcome.status}）` : ''}，请稍后重试。`, cls: 'pill bad' };
      case 'timeout': return { title: '检测超时', detail: '8 秒内未完成检测，当前无法确认 Hub 状态。请检查网络或稍后重试。', cls: 'pill' };
      case 'invalid': return { title: '响应异常', detail: '本站未返回有效的状态结果，当前无法确认 Hub 状态。', cls: 'pill' };
      default: return { title: '无法确认', detail: '当前浏览器无法完成请求，可能是网络或入口连接问题；不能据此判断 Hub 已停止。', cls: 'pill' };
    }
  }
  function showOutcome(outcome, stale = false) {
    const result = description(outcome);
    $('health').textContent = result.title;
    $('health').className = result.cls;
    $('status-visual').className = `status-visual ${outcome.kind === 'ok' ? 'is-ok' : 'is-warning'}`;
    $('summary').textContent = result.detail;
    $('request-duration').textContent = durationText(outcome.duration);
    sources.render(stale ? null : outcome.sources, 'stale');
  }
  function updateControls() {
    $('check').disabled = !eligible() || active !== null;
    $('check').textContent = active ? '检测中…' : '立即检测';
    if (disposed || !visible) $('refresh-state').textContent = '页面在后台，检测已暂停。';
    else if (!online) $('refresh-state').textContent = '浏览器离线，检测已暂停；恢复联网后可继续。';
    else if (!$('auto-check').checked) $('refresh-state').textContent = '自动检测已关闭，可随时手动检测。';
    else $('refresh-state').textContent = active ? '正在检测，完成后约 30 秒再次检查。' : '每 30 秒自动检测；页面在后台或浏览器离线时暂停。';
  }
  function showPaused() {
    $('health').textContent = online ? '已暂停' : '浏览器离线';
    $('health').className = 'pill';
    $('status-visual').className = 'status-visual';
    sources.render(null, 'stale');
    $('summary').textContent = online ? '页面当前不可见，连接检测已暂停。' : '浏览器报告当前离线，连接检测已暂停，无法确认服务状态。';
    $('request-duration').textContent = '—';
    updateControls();
  }
  function renderHistory() {
    const rows = records.map(outcome => {
      const row = document.createElement('li');
      row.className = `status-record status-record--${outcome.kind === 'ok' ? 'ok' : 'bad'}`;
      const at = document.createElement('time'); at.className = 'status-record-time'; stamp(at, outcome.at);
      const result = document.createElement('strong'); result.className = 'status-record-result'; result.textContent = description(outcome).title;
      const duration = document.createElement('span'); duration.className = 'status-record-duration'; duration.textContent = durationText(outcome.duration);
      row.append(at, result, duration);
      return row;
    });
    $('status-history').replaceChildren(...rows);
    renderTrend();
  }
  function renderTrend() {
    const scale = Math.max(1, ...records.map(outcome => outcome.duration ?? 0));
    const samples = records.slice().reverse().map(outcome => {
      const sample = document.createElement('li');
      sample.className = `chart-sample--${outcome.kind === 'ok' ? 'ok' : 'bad'}`;
      const label = `${outcome.at.toLocaleTimeString()} · ${description(outcome).title} · ${durationText(outcome.duration)}`;
      sample.title = label;
      const bar = document.createElement('span');
      bar.className = 'chart-bar';
      bar.style.height = `${Math.max(2, (outcome.duration ?? 0) / scale * 100)}%`;
      const text = document.createElement('span');
      text.className = 'sr-only'; text.textContent = label;
      sample.append(bar, text);
      return sample;
    });
    $('request-chart').replaceChildren(...samples);
    if (samples.length) samples[0].style.gridColumnStart = String(historyLimit - samples.length + 1);
    $('chart-empty').hidden = records.length > 0;
    $('sample-count').textContent = `${records.length} / ${historyLimit} 次`;
    $('chart-scale').textContent = `刻度上限 ${durationText(scale)}`;
    const successful = records.filter(outcome => outcome.kind === 'ok').length;
    $('chart-summary').textContent = `成功 ${successful} 次 · 未成功 ${records.length - successful} 次`;
  }
  function schedule() {
    clearNext();
    if (!eligible() || !$('auto-check').checked || active) return;
    nextTimer = setTimeout(() => { nextTimer = null; void check(); }, interval);
  }
  async function request(controller, start) {
    try {
      const response = await fetch('./statusz', { credentials: 'omit', cache: 'no-store', redirect: 'error', signal: controller.signal, referrerPolicy: 'no-referrer' });
      const body = await response.text();
      const duration = durationSince(start);
      if (!response.ok) return { kind: 'service', status: response.status, duration };
      let data;
      try { data = JSON.parse(body); } catch { return { kind: 'invalid', duration }; }
      if (data && data.status === 'available') return { kind: 'ok', duration, sources: sources.read(data.sources) };
      if (data && data.status === 'unavailable') return { kind: 'service', duration };
      return { kind: 'invalid', duration };
    } catch {
      return { kind: 'network', duration: durationSince(start) };
    }
  }
  async function check() {
    if (!eligible() || active) return;
    clearNext();
    const current = { controller: new AbortController(), timer: null, generation: ++generation };
    const start = performance.now();
    active = current;
    $('health').textContent = '正在检测'; $('health').className = 'pill';
    $('status-visual').className = 'status-visual';
    sources.render(null, 'pending');
    $('summary').textContent = '正在检查当前浏览器到本站公开状态接口的连接。';
    $('request-duration').textContent = '检测中';
    updateControls();
    const deadline = new Promise(resolve => {
      current.timer = setTimeout(() => {
        current.controller.abort();
        resolve({ kind: 'timeout', duration: durationSince(start) });
      }, timeout);
    });
    const outcome = await Promise.race([request(current.controller, start), deadline]);
    clearTimeout(current.timer);
    if (active !== current || current.generation !== generation || !eligible()) return;
    active = null;
    outcome.at = new Date();
    latest = outcome;
    showOutcome(outcome);
    stamp($('checked-at'), outcome.at);
    if (outcome.kind === 'ok') stamp($('last-success'), outcome.at);
    // Keep only timing/outcome history; source authorization retains one snapshot.
    records.unshift({ kind: outcome.kind, status: outcome.status, duration: outcome.duration, at: outcome.at });
    records.length = Math.min(records.length, historyLimit);
    renderHistory();
    updateControls();
    schedule();
  }
  function resume() {
    if (!eligible()) { showPaused(); return; }
    if (active) { updateControls(); return; }
    if ($('auto-check').checked) { void check(); return; }
    if (latest) showOutcome(latest, true);
    else {
      $('health').textContent = '等待检测'; $('health').className = 'pill';
      $('status-visual').className = 'status-visual';
      $('summary').textContent = '自动检测已关闭，可点击“立即检测”检查连接。';
      $('request-duration').textContent = '—';
    }
    updateControls();
  }
  function pause() { clearNext(); cancelActive(); showPaused(); }
  $('check').addEventListener('click', () => { void check(); });
  $('auto-check').addEventListener('change', () => {
    clearNext();
    if ($('auto-check').checked) resume();
    else updateControls();
  });
  document.addEventListener('visibilitychange', () => {
    const next = !document.hidden;
    if (next === visible) return;
    visible = next;
    if (visible) resume(); else pause();
  });
  addEventListener('offline', () => {
    if (!online) return;
    online = false;
    pause();
  });
  addEventListener('online', () => {
    if (online) return;
    online = true;
    resume();
  });
  addEventListener('pagehide', () => { disposed = true; pause(); });
  addEventListener('pageshow', () => {
    if (!disposed) return;
    disposed = false;
    visible = !document.hidden;
    online = navigator.onLine !== false;
    resume();
  });
  resume();
})();
