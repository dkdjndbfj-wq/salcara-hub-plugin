(() => {
  'use strict';
  window.SalcaraIngress = ({ request, getEpoch, isAuthenticated, isBusy }) => {
    const $ = id => document.getElementById(id);
    const text = (tag, value, cls) => { const el = document.createElement(tag); el.textContent = value; if (cls) el.className = cls; return el; };
    const names = { unverified: '待验证', verifying: '正在验证', verified: '已验证同一 Hub', failed: '验证失败', interrupted: '验证中断' };
    const errors = { revision_conflict: '配置已更新，请重新读取后修改。', ingress_busy: '入口验证正在运行，请等待完成。', invalid_ingress_url: '请填写公网 HTTPS 入口地址，不含账号、查询参数或片段。', invalid_ingress_configuration: '请检查入口名称、重复地址和优先级（1–1000）。', ingress_storage_failed: '配置写入失败，原配置保留；请检查 Hub 磁盘。', ingress_not_configured: '请先在部署配置中设置公网 HTTPS Hub 地址。' };
    let visible = false, state = null, rows = [], dirty = false, loading = false, writing = false, sequence = 0, controller = null, timer = null, budget = 0;
    const active = () => visible && isAuthenticated() && document.visibilityState !== 'hidden';
    const note = message => { $('ingress-notice').textContent = message; };
    function stop() { sequence++; controller?.abort(); controller = null; loading = false; clearTimeout(timer); timer = null; }
    function sync() {
      const blocked = !active() || !state?.configured || loading || writing || state.verification?.running || isBusy();
      for (const id of ['ingress-enabled', 'ingress-default-priority']) $(id).disabled = blocked;
      $('ingress-save').disabled = blocked || !dirty;
      $('ingress-add').disabled = blocked || rows.length >= 7;
      $('ingress-verify').disabled = blocked || dirty || !(state?.endpoints || []).some(e => e.enabled);
      $('ingress-refresh').disabled = !active() || loading || writing;
      $('ingress-refresh').textContent = dirty ? '放弃草稿并重新读取' : '读取状态';
      for (const row of rows) for (const el of row.controls) el.disabled = blocked;
      $('ingress-draft').textContent = dirty ? '有未保存修改；保存后需重新验证新增或改址的入口。' : '只有启用且通过同一 Hub 身份验证的入口会下发给客户端。';
    }
    function markDirty() { dirty = true; clearTimeout(timer); timer = null; sync(); }
    function addRow(endpoint = {}) {
      const wrap = text('div', '', 'ingress-entry'), header = text('div', '', 'section-title'), fields = text('div', '', 'ingress-fields');
      const controls = [], row = { endpoint, wrap, controls };
      const status = text('span', names[endpoint.status] || '保存后验证', 'pill');
      const remove = text('button', '移除', 'secondary'); remove.type = 'button'; controls.push(remove);
      header.append(status, remove); wrap.append(header);
      for (const [key, label, type, initial] of [['name', '入口名称', 'text', endpoint.name || ''], ['url', 'HTTPS 地址', 'url', endpoint.url || ''], ['priority', '优先级', 'number', endpoint.priority || 200]]) {
        const field = text('label', '', 'field'), input = document.createElement('input');
        input.type = type; input.value = String(initial); input.required = true; input.autocomplete = 'off';
        if (key === 'name') input.maxLength = 80;
        if (key === 'url') { input.maxLength = 512; input.placeholder = 'https://edge.example.com/salcara-hub'; input.spellcheck = false; }
        if (key === 'priority') { input.min = '1'; input.max = '1000'; input.step = '1'; }
        input.addEventListener('input', () => { if (key === 'url') status.textContent = '保存后重新验证'; markDirty(); });
        field.append(text('span', label), input); fields.append(field); row[key] = input; controls.push(input);
      }
      const label = text('label', '', 'check-field'), enabled = document.createElement('input'); enabled.type = 'checkbox'; enabled.checked = endpoint.enabled !== false; row.enabled = enabled; controls.push(enabled);
      enabled.addEventListener('change', markDirty); label.append(enabled, text('span', '允许客户端自动使用'));
      wrap.append(fields, label);
      if (endpoint.verified_at) wrap.append(text('p', '最近验证：' + new Date(endpoint.verified_at).toLocaleString(), 'muted small'));
      if (endpoint.error) wrap.append(text('p', '未通过身份或网络检查，请核对 HTTPS 证书、回源地址和所属 Hub。', 'muted small'));
      remove.addEventListener('click', () => { rows = rows.filter(item => item !== row); $('ingress-list').replaceChildren(...rows.map(item => item.wrap)); markDirty(); });
      rows.push(row); $('ingress-list').append(wrap);
    }
    function render(value) {
      state = value; dirty = false; rows = []; $('ingress-list').replaceChildren();
      $('ingress-enabled').checked = value.enabled === true; $('ingress-default-priority').value = String(value.default_priority || 100);
      $('ingress-canonical').textContent = value.canonical_url || '未配置公网 HTTPS Hub 地址';
      const verified = (value.endpoints || []).filter(e => e.enabled && e.status === 'verified').length;
      $('ingress-status').textContent = !value.configured ? '需要 HTTPS Hub 地址' : value.verification?.running ? '正在验证' : !value.enabled ? '单入口模式' : verified ? `自动切换已就绪 · ${verified} 个备用` : '已配置，等待可用入口';
      for (const endpoint of value.endpoints || []) addRow(endpoint);
      const job = value.verification;
      $('ingress-progress').textContent = job ? `${job.running ? '验证进行中' : '最近验证'} · ${job.completed} / ${job.total}，通过 ${job.succeeded}，未通过 ${job.failed}` : '保存备用入口后，点击验证全部入口。';
      if (value.last_error) note(errors[value.last_error] || '入口配置需要检查，请读取状态或重新验证。');
      sync();
    }
    function schedule() { clearTimeout(timer); timer = null; if (active() && !dirty && state?.verification?.running && budget-- > 0) timer = setTimeout(() => { timer = null; void refresh(); }, 2000); }
    async function refresh() {
      if (!active() || loading || writing) return;
      stop(); const seq = sequence, epoch = getEpoch(); controller = new AbortController(); loading = true; sync();
      try {
        const value = await request('ingress/state', 'GET', undefined, false, controller.signal);
        if (seq !== sequence || epoch !== getEpoch() || !active()) return;
        if (!Number.isSafeInteger(value.revision)) throw new Error('invalid state');
        note(''); render(value);
      } catch { if (seq === sequence && epoch === getEpoch() && active()) { state = null; note('状态读取失败，操作已锁定，请重新读取。'); } }
      finally { if (seq === sequence) { loading = false; controller = null; sync(); schedule(); } }
    }
    async function mutate(operation, extra = {}) {
      if (!active() || !state?.configured || loading || writing || state.verification?.running || isBusy()) return;
      const revision = state.revision, epoch = getEpoch(); stop(); const seq = sequence; writing = true; sync();
      try {
        const value = await request('ingress/' + operation, 'POST', { expected_revision: revision, ...extra });
        if (seq !== sequence || epoch !== getEpoch() || !active()) return;
        render(value); budget = 90; note(operation === 'save' ? '入口配置已保存；新增或改址的入口需要通过验证后才可用。' : '正在逐一验证入口连接的 Hub 身份。');
      } catch (error) { if (seq === sequence && epoch === getEpoch() && active()) { state = null; note((errors[error.message] || '未确认操作结果。') + ' 请重新读取状态，不要直接重复提交。'); } }
      finally { if (seq === sequence) { writing = false; sync(); schedule(); } }
    }
    $('ingress-form').addEventListener('submit', event => {
      event.preventDefault(); if (!dirty) return;
      const priority = Number($('ingress-default-priority').value);
      const endpoints = rows.map(row => ({ ...(row.endpoint.id ? { id: row.endpoint.id } : {}), name: row.name.value.trim(), url: row.url.value.trim(), priority: Number(row.priority.value), enabled: row.enabled.checked }));
      if (!Number.isInteger(priority) || priority < 1 || priority > 1000 || endpoints.some(e => !e.name || new TextEncoder().encode(e.name).length > 80 || !Number.isInteger(e.priority) || e.priority < 1 || e.priority > 1000)) { note('名称最多 80 字节，优先级需为 1–1000 的整数。'); return; }
      void mutate('save', { enabled: $('ingress-enabled').checked, default_priority: priority, endpoints });
    });
    $('ingress-add').addEventListener('click', () => { if ($('ingress-add').disabled) return; addRow(); markDirty(); });
    $('ingress-enabled').addEventListener('change', markDirty); $('ingress-default-priority').addEventListener('input', markDirty);
    $('ingress-verify').addEventListener('click', () => { if (!dirty) void mutate('verify'); });
    $('ingress-refresh').addEventListener('click', () => { budget = 90; void refresh(); });
    function setVisible(show) {
      const changed = visible !== show; visible = show;
      if (!show) { stop(); writing = false; state = null; dirty = false; rows = []; $('ingress-list').replaceChildren(); }
      if (show && changed && active()) { budget = 90; void refresh(); } sync();
    }
    document.addEventListener('visibilitychange', () => { if (!active()) { stop(); writing = false; } else if (!dirty) void refresh(); else { state = null; note('页面恢复后请重新读取状态，再继续修改。'); sync(); } });
    return { setVisible, refresh, sync, reset: () => setVisible(false) };
  };
})();
