(() => {
  'use strict';
  const token = new URLSearchParams(location.hash.slice(1)).get('bridge_token');
  const pending = new Map();
  let seq = 0;
  let selected = new Set();
  let groups = [];
  let deviceOffset = 0;
  let deviceTotal = 0;
  let pendingAction = null;
  const deviceLimit = 50;
  const $ = (id) => document.getElementById(id);

  function request(type, payload = {}) {
    if (!token) return Promise.reject(new Error('插件会话无效，请重新打开配置页'));
    const requestId = `hub-${Date.now()}-${++seq}`;
    return new Promise((resolve, reject) => {
      const timer = setTimeout(() => {
        pending.delete(requestId);
        reject(new Error('操作超时，请重试'));
      }, 30000);
      pending.set(requestId, { resolve, reject, timer });
      parent.postMessage({ source: 'sub2api-plugin-ui', bridge_token: token,
        type, request_id: requestId, ...payload }, '*');
    });
  }

  window.addEventListener('message', (event) => {
    const data = event.data;
    if (event.source !== parent || !data || data.source !== 'sub2api-plugin-host' ||
        data.bridge_token !== token || typeof data.request_id !== 'string') return;
    const entry = pending.get(data.request_id);
    if (!entry) return;
    clearTimeout(entry.timer);
    pending.delete(data.request_id);
    if (data.ok) entry.resolve(data);
    else entry.reject(new Error(data.error || '操作失败'));
  });

  function note(message, isError = false) {
    $('notice').textContent = message;
    $('notice').style.color = isError ? '#b91c1c' : '#64748b';
  }

  function renderGroups() {
    const container = $('groups');
    container.replaceChildren();
    if (!groups.length) {
      const text = document.createElement('p');
      text.className = 'muted';
      text.textContent = '没有可用分组，请先在 Sub2API 创建分组。';
      container.append(text);
      return;
    }
    for (const group of groups) {
      const label = document.createElement('label');
      label.className = 'group';
      const input = document.createElement('input');
      input.type = 'checkbox';
      input.value = String(group.id);
      input.checked = selected.has(group.id);
      input.addEventListener('change', () => {
        if (input.checked) selected.add(group.id);
        else selected.delete(group.id);
      });
      const name = document.createElement('span');
      name.className = 'name';
      name.textContent = group.name;
      const platform = document.createElement('span');
      platform.className = 'platform';
      platform.textContent = group.platform;
      label.append(input, name, platform);
      container.append(label);
    }
  }

  function renderConfig(config) {
    selected = new Set(Array.isArray(config.allowed_group_ids) ? config.allowed_group_ids : []);
    $('sub2api-url').value = config.sub2api_url || 'http://127.0.0.1:8080';
    $('timeout').value = String(config.command_timeout_seconds || 45);
    $('trust-proxy').checked = false;
    renderGroups();
  }

  function currentConfig() {
    return {
      sub2api_url: $('sub2api-url').value.trim(),
      command_timeout_seconds: Number($('timeout').value),
      trust_proxy: $('trust-proxy').checked,
      allowed_group_ids: [...selected].sort((a, b) => a - b),
    };
  }

  async function status() {
    try {
      const reply = await request('plugin.status');
      const value = reply.result || {};
      const state = value.status_json ? JSON.parse(value.status_json) : {};
      $('health').textContent = value.healthy && state.configured ? '运行中' : '未启用';
      $('health').className = `health ${value.healthy && state.configured ? 'ok' : 'bad'}`;
      $('status-message').textContent = value.message || '插件未运行';
      $('version').textContent = state.version ? `Hub v${state.version}` : '—';
      const stats = state.stats || {};
      $('online-devices').textContent = stats.online_devices ?? '—';
      $('devices').textContent = stats.devices ?? '—';
      $('app-streams').textContent = stats.app_streams ?? '—';
      $('pending').textContent = stats.pending_commands ?? '—';
      $('paired-devices').textContent = stats.paired_devices ?? '—';
      $('running-sessions').textContent = stats.running_sessions ?? '—';
      $('waiting-approvals').textContent = stats.waiting_approvals ?? '—';
      $('request-errors').textContent = stats.request_errors ?? '—';
      $('latency-summary').textContent = stats.latency_samples > 0
        ? `Hub ↔ 电脑实际指令往返：均值 ${Number(stats.latency_mean_ms).toFixed(1)} ms，${stats.latency_samples} 次回复样本；成功 ${stats.command_success}，失败回复 ${stats.command_failures}，超时 ${stats.command_timeouts}（本次进程）。不是手机完整网络延迟。`
        : 'Hub ↔ 电脑实际指令往返：未测量；可在在线设备上点击“测量”。不是最近在线时间。';
    } catch (error) {
      $('health').textContent = '状态不可用';
      $('health').className = 'health bad';
      $('status-message').textContent = error.message;
    }
  }

  const actionNames = { ping: '测量', revoke: '撤销绑定', disconnect: '临时断开', ban: '封禁', unban: '解封' };
  function textNode(tag, value, cls = '') {
    const node = document.createElement(tag); node.textContent = String(value); if (cls) node.className = cls; return node;
  }
  function moment(ms) { return ms ? new Date(ms).toLocaleString() : '从未在线'; }
  async function devices() {
    try {
      const reply = await request('hub.admin.snapshot', { query: { query: $('device-search').value.trim(), filter: $('device-filter').value, offset: deviceOffset, limit: deviceLimit } });
      const value = reply.result || {};
      const rows = $('device-rows'); rows.replaceChildren();
      deviceTotal = Number(value.total || 0); deviceOffset = Number(value.offset || 0);
      for (const device of value.devices || []) {
        const tr = document.createElement('tr');
        const identity = document.createElement('td');
        identity.append(textNode('strong', device.name || '未命名电脑'), textNode('small', `${device.os || '未知系统'} · Bridge ${device.version || '未知'}`));
        identity.append(textNode('small', (device.tools || []).map(tool => `${tool.name || tool.id}: ${tool.available ? '已安装' : '未安装'}`).join('；') || '尚未上报工具'));
        identity.append(textNode('small', `匿名标识 ${device.ref.slice(0, 12)}…`));
        const connection = document.createElement('td');
        connection.append(textNode('span', device.banned ? '已封禁' : device.online ? '在线' : '离线'), textNode('small', device.paired ? '已绑定手机' : '未绑定手机'), textNode('small', `最近上报 ${moment(device.last_seen)}`));
        if (device.banned) connection.append(textNode('small', device.ban_reason || '管理员封禁'));
        const sessions = textNode('td', `运行 ${device.running_sessions} / 待审批 ${device.waiting_approvals}`);
        const latency = document.createElement('td');
        if (device.latency.samples) {
          latency.append(textNode('span', `${Number(device.latency.last_ms).toFixed(1)} ms · ${device.latency.outcome === 'ok' ? '成功回复' : '失败回复'}`), textNode('small', `测量于 ${moment(device.latency.measured_at)}`));
        } else latency.append(textNode('span', '未测量'));
        const actions = document.createElement('td'); actions.className = 'device-actions';
        for (const action of ['ping', 'revoke', 'disconnect', device.banned ? 'unban' : 'ban']) {
          const button = textNode('button', actionNames[action], 'secondary'); button.type = 'button';
          button.disabled = (action === 'ping' && (!device.online || device.banned)) || (action === 'disconnect' && !device.online);
          button.addEventListener('click', () => beginAction(action, device.ref)); actions.append(button);
        }
        tr.append(identity, connection, sessions, latency, actions); rows.append(tr);
      }
      if (!rows.children.length) { const tr = document.createElement('tr'); const td = textNode('td', '没有匹配设备'); td.colSpan = 5; tr.append(td); rows.append(tr); }
      $('device-page').textContent = `${deviceTotal} 台匹配设备；${deviceTotal ? deviceOffset + 1 : 0}–${Math.min(deviceOffset + deviceLimit, deviceTotal)}`;
      $('device-prev').disabled = deviceOffset <= 0; $('device-next').disabled = deviceOffset + deviceLimit >= deviceTotal;
      const audit = $('audit-rows'); audit.replaceChildren();
      for (const item of [...(value.audit || [])].reverse()) {
        const actor = item.actor?.startsWith('host-admin:') ? `宿主管理员 ID ${item.actor.slice(11)}` : '宿主管理员';
        audit.append(textNode('p', `${moment(item.at)} · ${actor} · ${actionNames[item.action] || item.action} · ${item.device_ref.slice(0, 12)}… · ${item.reason || '测量连接'}`));
      }
      if (!audit.children.length) audit.append(textNode('p', '暂无管理员操作'));
    } catch (error) { $('device-page').textContent = `设备管理暂不可用：${error.message}`; }
  }
  function beginAction(action, ref) {
    if (action === 'ping') { performAction({ action, ref, reason: '' }); return; }
    pendingAction = { action, ref };
    $('operation-title').textContent = `确认${actionNames[action]}`; $('operation-reason').value = '';
    $('operation-dialog').showModal();
  }
  async function performAction(action) {
    note('等待宿主管理员确认 / 二次验证…');
    try { await request('hub.admin.action', { action }); note('管理操作完成'); await Promise.all([status(), devices()]); }
    catch (error) { note(error.message, true); }
  }

  async function save() {
    $('save').disabled = true;
    note('正在保存…');
    try {
      const result = await request('config.save', { config: currentConfig() });
      renderConfig(result.config || currentConfig());
      note('已保存');
      await status();
    } catch (error) { note(error.message, true); }
    finally { $('save').disabled = false; }
  }

  async function test() {
    $('test').disabled = true;
    note('正在测试…');
    try {
      const result = await request('config.test');
      note(result.result?.message || '连接正常');
    } catch (error) { note(error.message, true); }
    finally { $('test').disabled = false; }
  }

  $('save').addEventListener('click', save);
  $('test').addEventListener('click', test);
  $('refresh-devices').addEventListener('click', () => { status(); devices(); });
  $('device-filter').addEventListener('change', () => { deviceOffset = 0; devices(); });
  let searchTimer;
  $('device-search').addEventListener('input', () => { clearTimeout(searchTimer); searchTimer = setTimeout(() => { deviceOffset = 0; devices(); }, 350); });
  $('device-prev').addEventListener('click', () => { deviceOffset = Math.max(0, deviceOffset - deviceLimit); devices(); });
  $('device-next').addEventListener('click', () => { deviceOffset += deviceLimit; devices(); });
  $('operation-cancel').addEventListener('click', () => { pendingAction = null; $('operation-dialog').close(); });
  $('operation-form').addEventListener('submit', event => { event.preventDefault(); if (!pendingAction) return; const action = { ...pendingAction, reason: $('operation-reason').value.trim() }; pendingAction = null; $('operation-dialog').close(); performAction(action); });
  addEventListener('pagehide', () => {
    for (const entry of pending.values()) { clearTimeout(entry.timer); entry.reject(new Error('配置页已关闭')); }
    pending.clear();
  });
  parent.postMessage({ source: 'sub2api-plugin-ui', bridge_token: token,
    type: 'sub2api.plugin.ready', request_id: 'ready' }, '*');
  request('config.load').then((config) => {
    renderConfig(config.config || {});
    note('仅显示运行汇总，不显示用户密钥或会话内容');
  }).catch((error) => note(error.message, true));
  let groupsLoaded = false;
  $('legacy-config').addEventListener('toggle', async () => {
    if (!$('legacy-config').open || groupsLoaded) return;
    try {
      const list = await request('groups.list');
      groups = Array.isArray(list.groups) ? list.groups : [];
      groupsLoaded = true;
      renderGroups();
    } catch (error) {
      $('groups').textContent = `旧版分组不可用：${error.message}。设备扫码模式不受影响。`;
    }
  });
  status();
  devices();
  setInterval(() => { status(); devices(); }, 10000);
  parent.postMessage({ source: 'sub2api-plugin-ui', bridge_token: token,
    type: 'ui.resize', request_id: 'resize', height: 760 }, '*');
})();
