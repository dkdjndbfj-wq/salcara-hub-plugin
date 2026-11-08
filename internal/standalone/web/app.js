(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const apiBase = '../_admin/v1/';
  let authenticated = false, csrf = '', epoch = 0, offset = 0, total = 0, pending = null, busy = false, updateCandidate = null, updaterConfigured = false, resourceMode = '', modes = [], modeCandidate = null, cleanupCandidate = null, refreshSeq = 0;
  let remoteAccessRevision = '', remoteAccessLocked = true, remoteAccessDirty = false;
  const remoteScopes = ['api', 'external', 'proxy'];
  let adminRole = 'owner', adminName = '主管理员', storageDirty = false, storageEdit = 0;
  let botReadSeq = 0;
  let updateInfo = null, updateTimer = null, updateReading = false, updateSeq = 0, pendingUpdate = null, updateTracking = false;
  const controllers = new Set();
  const names = { ping: '测量', revoke: '撤销绑定', disconnect: '临时断开', ban: '封禁', unban: '解封', 'resource-mode': '切换运行模式', 'remote-access': '修改远程来源', 'device-cleanup': '修改自动清理', 'device-cleanup-run': '自动清理' };
  const limit = 50;
  const pages = {
    overview: ['概览', '连接状态与运行情况，一眼掌握。'],
    users: ['用户', '管理用户归属、使用状态与单独授权。'],
    devices: ['设备与配对', '查看用户的电脑连接、手机配对与远程会话。'],
    servers: ['服务器', '接入服务器，部署和管理入口。'],
    network: ['线路与代理池', '统一管理代理节点与回源线路。'],
    access: ['访问策略', '管理本站允许使用的远程任务来源。'],
    audit: ['操作记录', '查看本站设备和管理设置的最近变更。'],
    system: ['系统', '管理运行资源与维护策略。'],
    updates: ['版本更新', '自动检测新版本，按需一键更新 Hub。'],
    operations: ['运维中心', '检查服务健康、管理协作权限与备份恢复。'],
  };
  let activePage = 'overview';
  const network = typeof window.SalcaraNetwork === 'function' ? window.SalcaraNetwork({
    request, getEpoch: () => epoch, isAuthenticated: () => authenticated, isBusy: () => busy,
  }) : null;
  const servers = typeof window.SalcaraServers === 'function' ? window.SalcaraServers({
    request, getEpoch: () => epoch, isAuthenticated: () => authenticated, isBusy: () => busy,
  }) : null;
  const ingress = window.SalcaraIngress?.({ request, getEpoch: () => epoch, isAuthenticated: () => authenticated, isBusy: () => busy });
  const operations = window.SalcaraOperations?.({ request, getEpoch: () => epoch, isAuthenticated: () => authenticated, getRole: () => adminRole });
  const users = window.SalcaraUsers?.({ request, getEpoch: () => epoch, isAuthenticated: () => authenticated, getRole: () => adminRole, isBusy: () => busy, refreshDevices: () => refresh(), showDevices: id => { $('device-user-filter').value = id; offset = 0; showPage('devices'); void refresh().catch(() => {}); } });
  function showPage(id, moveFocus = false) {
    if (!Object.hasOwn(pages, id)) return;
    if (adminRole !== 'owner' && ['servers','network','access','system','updates'].includes(id)) id = 'overview';
    if (activePage !== id) note('');
    activePage = id;
    for (const key of Object.keys(pages)) {
      $('page-' + key).hidden = key !== id;
      $('nav-' + key).className = 'nav-item' + (key === id ? ' active' : '');
      $('nav-' + key).setAttribute('aria-current', key === id ? 'page' : 'false');
    }
    $('page-title').textContent = pages[id][0];
    $('page-description').textContent = pages[id][1];
    $('last-refreshed').hidden = id === 'network' || id === 'servers' || id === 'updates';
    $('update-banner').hidden = id === 'updates' || adminRole !== 'owner' || updateInfo?.status !== 'available';
    network?.setVisible(authenticated && id === 'network');
    servers?.setVisible(authenticated && id === 'servers');
    ingress?.setVisible(authenticated && id === 'servers');
    operations?.setVisible(authenticated ? id : '');
    users?.setVisible(authenticated ? id : '');
    if (authenticated && id === 'updates' && updaterConfigured) void readUpdateStatus();
    if (moveFocus) $('page-title').focus?.({ preventScroll: true });
  }
  for (const id of Object.keys(pages)) $('nav-' + id).addEventListener('click', () => { if (authenticated) showPage(id, true); });
  function note(message, error = false) { $('notice').textContent = message; $('notice').className = `notice global-notice${error ? ' error' : ''}`; }
  function text(tag, value, cls = '') { const node = document.createElement(tag); node.textContent = String(value ?? '—'); if (cls) node.className = cls; return node; }
  function moment(ms) { return Number(ms) > 0 ? new Date(Number(ms)).toLocaleString() : '尚无记录'; }
  function clearData() { for (const id of ['online','registered','paired','running','waiting','pending','banned','errors']) $(id).textContent = '—'; $('devices').replaceChildren(); $('audit').replaceChildren(); }
  function clearSession(message = '已退出管理。') {
    clearTimeout(updateTimer); updateTimer = null; updateInfo = null; updateReading = false; updateSeq++; pendingUpdate = null; updateTracking = false;
    $('update-banner').hidden = true; $('update-badge').hidden = true; $('update-error').textContent = ''; $('update-notes').textContent = ''; $('update-notes-wrap').hidden = true;
    ingress?.reset();
    operations?.reset(); users?.reset(); storageDirty = false;
    authenticated = false; csrf = ''; epoch++; network?.reset(); servers?.reset(); showPage('overview'); $('last-refreshed').textContent = '尚未读取运行状态'; for (const c of controllers) c.abort(); controllers.clear();
    offset = 0; total = 0; $('search').value = ''; $('filter').value = ''; $('device-user-filter').value = '';
    pending = null; busy = false; updateCandidate = null; updaterConfigured = false; resourceMode = ''; modes = []; modeCandidate = null; if ($('operation').open) $('operation').close(); if ($('update-dialog').open) $('update-dialog').close(); if ($('mode-dialog').open) $('mode-dialog').close(); cleanupCandidate = null; if ($('cleanup-dialog').open) $('cleanup-dialog').close(); $('cleanup-current').textContent = '未认证'; $('cleanup-details').textContent = '';
    $('enrollment-result').hidden = true; $('enrollment-value').value = ''; $('enrollment-expiry').textContent = ''; $('hub-address').textContent = '正在读取 Hub 地址';
    botReadSeq++; $('bot-binding-value').value = ''; $('bot-binding-result').hidden = true; $('bot-panel').hidden = true; $('bot-groups').replaceChildren();
    for (const id of ['storage-days','storage-global','storage-binding']) $(id).value = '';
    $('storage-queue').checked = false; $('storage-current').textContent = '未认证'; $('storage-details').textContent = '';
    $('mode-options').replaceChildren(); $('mode-current').textContent = '未认证'; $('mode-details').textContent = '';
    remoteAccessRevision = ''; remoteAccessLocked = true; remoteAccessDirty = false; $('remote-access-mode').value = 'disabled'; for (const scope of remoteScopes) { $(`remote-${scope}-enabled`).checked = false; $(`remote-${scope}-bases`).value = ''; $(`remote-${scope}-status`).textContent = ''; } $('remote-official-enabled').checked = false; $('remote-official-status').textContent = ''; $('remote-access-current').textContent = '未认证'; $('remote-access-details').textContent = '';
    $('admin-password').value = ''; clearPasswordForm(); if ($('password-dialog').open) $('password-dialog').close(); $('login').disabled = false; $('workspace').hidden = true; $('login-panel').hidden = false; clearData(); syncUpdateButtons();
    $('health').textContent = '尚未认证'; $('health').className = 'pill'; note(message);
    $('login-error').textContent = message; $('admin-password').type = 'password'; $('toggle-login-password').textContent = '显示'; $('toggle-login-password').setAttribute('aria-pressed','false');
  }
  async function request(path, method = 'GET', body, anonymous = false, externalSignal, format = 'json') {
    if (!authenticated && !anonymous) throw new Error('请先登录。');
    const generation = epoch, controller = new AbortController(); controllers.add(controller);
    const abortExternal = () => controller.abort();
    if (externalSignal?.aborted) controller.abort();
    externalSignal?.addEventListener('abort', abortExternal, { once: true });
    const timeout = setTimeout(() => controller.abort(), path === 'update/check' ? 30000 : 20000);
    try {
      const response = await fetch(apiBase + path, { method, headers: { ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}), ...(method !== 'GET' && !anonymous ? { 'X-Salcara-CSRF': csrf } : {}) }, body: body !== undefined ? JSON.stringify(body) : undefined, credentials: 'same-origin', cache: 'no-store', redirect: 'error', signal: controller.signal, referrerPolicy: 'no-referrer' });
      if (generation !== epoch) throw new Error('管理员会话已关闭。');
      if (response.status === 401) { if (!anonymous) clearSession('登录已过期，请重新进入。'); throw new Error(path === 'auth/login' ? '账号或密码不正确，请检查管理员账号与管理密钥。' : '请重新进入。'); }
      let data; try { data = response.ok && format === 'text' ? await response.text() : await response.json(); } catch { throw new Error('返回的不是 Hub 接口，请检查反向代理路径。'); }
      if (generation !== epoch) throw new Error('管理员会话已关闭。');
      if (!response.ok) throw new Error(data.error || data.message || `请求失败（${response.status}）`);
      return data;
    } catch (error) { if (error.name === 'AbortError') throw new Error('请求中断或超时；操作可能已送达，请刷新状态，不要直接重复操作。'); throw error; }
    finally { clearTimeout(timeout); controllers.delete(controller); externalSignal?.removeEventListener('abort', abortExternal); }
  }
  function activateSession(value) {
    if (typeof value.csrf_token !== 'string' || value.csrf_token.length < 32 || value.csrf_token.length > 128 || /[^A-Za-z0-9_-]/.test(value.csrf_token)) throw new Error('登录响应无效，请检查 Hub 地址。');
    csrf = value.csrf_token; authenticated = true;
    adminRole = value.role || 'owner'; adminName = value.name || '主管理员';
    $('new-admin-username').value = value.username || 'admin'; $('login-error').textContent = '';
    for (const id of ['servers','network','access','system','updates']) $('nav-' + id).hidden = adminRole !== 'owner';
    if ($('admin-identity')) $('admin-identity').textContent = adminRole === 'owner' ? '主管理员' : `${adminName} · ${{operator:'运维',viewer:'只读'}[adminRole] || '受限'}`;
    const enrollmentPanel = $('create-enrollment').closest?.('section'); if (enrollmentPanel) enrollmentPanel.hidden = adminRole !== 'owner';
    $('create-enrollment').hidden = adminRole !== 'owner';
    if (adminRole !== 'owner') { $('enrollment-result').hidden = true; $('bot-panel').hidden = true; }
  }
  function clearPasswordForm() { for (const id of ['current-password', 'new-password', 'confirm-password']) $(id).value = ''; $('password-notice').textContent = ''; }
  function showWorkspace() { $('workspace').hidden = false; $('login-panel').hidden = true; showPage(activePage); }
  function render(value, updateRevision = updateSeq) {
    if (!authenticated) return;
    $('hub-address').textContent = typeof value.public_url === 'string' && value.public_url
      ? value.public_url
      : `${window.location.origin}/salcara-hub`;
    $('last-refreshed').textContent = '最近读取：' + new Date().toLocaleString() + ' · 按需刷新';
    const stats = value.stats || {};
    for (const [id, field] of Object.entries({ online:'online_devices', registered:'devices', paired:'paired_devices', running:'running_sessions', waiting:'waiting_approvals', pending:'pending_commands', banned:'banned_devices', errors:'request_errors' })) $(id).textContent = stats[field] ?? '—';
    $('health').textContent = 'Hub 已连接'; $('health').className = 'pill ok';
    $('version').textContent = value.standalone_version ? `v${value.standalone_version}` : '版本信息';
    renderModes(value);
    renderCleanup(value);
    renderStorage(value.durable_storage);
    renderRemoteAccess(value);
    users?.setStation(value.remote_access_station);
    $('latency').textContent = stats.latency_samples > 0 ? `Hub ↔ 电脑指令往返均值 ${Number(stats.latency_mean_ms).toFixed(1)} ms，${stats.latency_samples} 次回复样本。不是手机全链路延迟。` : '尚未测量 Hub ↔ 电脑指令往返；可对在线设备点击“测量”。';
    total = Number(value.total || 0); offset = Number(value.offset || 0);
    const rows = $('devices'); rows.replaceChildren();
    for (const d of value.devices || []) {
      const tr = document.createElement('tr'), identity = document.createElement('td');
      identity.append(text('strong', d.name || '未命名电脑'), text('small', `${d.os || '未知系统'} · Bridge ${d.version || '未知'}`), text('small', (d.tools || []).map(t => `${t.name || t.id}: ${t.available ? '已安装' : '未安装'}`).join('；') || '尚未上报工具'), text('small', `匿名标识 ${String(d.ref || '').slice(0,12)}…`));
      identity.append(text('small', `${value.deployment_mode === 'bot' ? 'Bot 组授权' : d.user_name || '未分配用户'}${d.policy_override ? ' · 单独授权' : ''}`, 'device-user-tag'));
      const connection = document.createElement('td'); connection.dataset.label = '连接'; connection.append(text('span', d.banned ? '已封禁' : d.online ? '在线' : '离线', d.banned ? 'pill bad' : d.online ? 'pill ok' : 'pill'), text('small', d.paired ? '已绑定手机' : '未绑定手机'), text('small', `最近上报 ${moment(d.last_seen)}`)); if (d.banned) connection.append(text('small', d.ban_reason));
      if (d.user_disabled) connection.append(text('small','所属用户已禁用','text-danger'));
      if (d.pair_state === 'pending') connection.append(text('small','配对正在保存'));
      const latency = document.createElement('td'); latency.dataset.label = 'Hub ↔ 电脑'; if (d.latency?.samples) latency.append(text('span', `${Number(d.latency.last_ms).toFixed(1)} ms`), text('small', moment(d.latency.measured_at))); else latency.append(text('span', '未测量'));
      const actions = document.createElement('td'); actions.className = 'device-actions';
      const more = document.createElement('details'); more.className = 'row-more';
      const summary = text('summary', '更多'); summary.setAttribute('aria-label', '更多设备操作');
      const moreActions = document.createElement('div'); moreActions.className = 'more-actions';
      more.append(summary, moreActions);
      for (const action of ['ping','revoke','disconnect', d.banned ? 'unban' : 'ban']) {
        const button = text('button', names[action], action === 'ban' || action === 'revoke' ? 'secondary text-danger' : 'secondary'); button.type = 'button';
        button.disabled = adminRole === 'viewer' || (action === 'ping' && (!d.online || d.banned || d.user_disabled)) || (action === 'disconnect' && !d.online) || (action === 'revoke' && !d.paired && d.pair_state !== 'pending');
        button.addEventListener('click', () => { more.open = false; begin(action, d.ref, d); });
        (action === 'ping' ? actions : moreActions).append(button);
      }
      actions.append(more);
      if (adminRole === 'owner' && users && value.deployment_mode !== 'bot') { const assign = text('button',d.user_id ? '变更用户' : '关联用户','secondary'); assign.type='button'; assign.addEventListener('click',()=>{more.open=false;void users.assign(d);}); moreActions.append(assign); }
      const sessions = text('td', `运行 ${d.running_sessions ?? 0} / 待电脑确认 ${d.waiting_approvals ?? 0}`); sessions.dataset.label = '会话';
      tr.append(identity, connection, sessions, latency, actions); rows.append(tr);
    }
    if (!rows.children.length) { const tr = document.createElement('tr'), td = text('td', $('search').value || $('filter').value || $('device-user-filter').value ? '没有匹配设备，可重置筛选查看其他电脑。' : '还没有电脑接入。为用户生成登记码，在电脑端连接此 Hub 后即可显示。'); td.colSpan = 5; td.className = 'empty-cell'; tr.append(td); rows.append(tr); }
    $('page').textContent = `${total} 台匹配设备 · ${total ? offset+1 : 0}–${Math.min(offset+limit,total)}`; $('prev').disabled = offset <= 0; $('next').disabled = offset+limit >= total;
    const audit = $('audit'); audit.replaceChildren();
    for (const a of [...(value.audit || [])].reverse()) { const subject = a.device_ref ? `${String(a.device_ref).slice(0,12)}…` : '服务策略'; const reason = a.action === 'resource-mode' ? modes.find(p => p.id === a.reason)?.label || a.reason : a.reason || '测量连接'; audit.append(text('p', `${moment(a.at)} · ${a.actor === 'standalone-admin' ? '独立 Hub 管理员' : a.actor === 'hub' ? '自动任务' : '宿主管理员'} · ${names[a.action] || a.action} · ${subject} · ${reason}`)); }
    if (!audit.children.length) audit.append(text('p', '暂无管理操作。', 'muted'));
    updaterConfigured = Boolean(value.update_configured);
    const botPanel = $('bot-panel');
    if (botPanel) botPanel.hidden = adminRole !== 'owner' || value.deployment_mode !== 'bot';
    const enrollmentPanel = $('create-enrollment').closest?.('section'); if (enrollmentPanel) enrollmentPanel.hidden = adminRole !== 'owner' || value.deployment_mode === 'bot';
    if (adminRole === 'owner' && value.update_status && updateRevision === updateSeq) renderUpdate(value.update_status);
    else if (!updateInfo || !updaterConfigured) {
      $('update-current').textContent = value.standalone_version || '—';
      $('update-status').textContent = updaterConfigured ? '已连接更新组件，进入版本更新页查看检测结果。' : '未连接更新组件。请使用配套 Docker 镜像启动 Hub；已有数据应按部署说明升级并保留数据卷。';
      $('update-label').textContent = updaterConfigured ? '等待查询' : '未配置';
      $('update-schedule').textContent = updaterConfigured ? '正在确认自动检查能力。' : '自动检查尚未启用。';
      if (!updaterConfigured) { updateInfo = null; updateCandidate = null; clearTimeout(updateTimer); updateTimer = null; $('update-banner').hidden = true; $('update-badge').hidden = true; }
    }
    syncUpdateButtons();
  }
  async function refreshBotGroups() {
    const box = $('bot-groups'); if (!box || $('bot-panel').hidden) return;
    const generation = epoch, seq = ++botReadSeq;
    try {
      const value = await request('bot/invites');
      if (generation !== epoch || seq !== botReadSeq) return;
      box.replaceChildren();
      const groups = Array.isArray(value.groups) ? value.groups : [];
      if (!groups.length) { box.append(text('p', '尚未生成 Bot 绑定包。')); return; }
      for (const group of groups) {
        const row = text('p', `组 ${String(group.groupId).slice(0, 12)}… · ${Number(group.computers) || 0}/8 台电脑 · ${group.phoneBound ? '已绑定手机' : '未绑定手机'}${group.revoked ? ' · 已撤销' : ''}`);
        if (!group.revoked) { const button = text('button', '撤销', 'secondary'); button.type = 'button'; button.addEventListener('click', async () => {
          if (busy || !authenticated || adminRole !== 'owner' || !confirm('撤销这个 Bot 组的新增绑定权限？已有设备需分别撤销。')) return;
          const generation = epoch; busy = true; button.disabled = true; syncUpdateButtons();
          try { await request('bot/invites/revoke', 'POST', { groupId: group.groupId, confirm: true }); await refreshBotGroups(); if (generation === epoch) note('已停止该组新增绑定。已有设备需在设备页分别撤销。'); }
          catch (error) { if (generation === epoch) note(error.message, true); }
          finally { if (generation === epoch) { busy = false; button.disabled = false; syncUpdateButtons(); } }
        }); row.append(' ', button); }
        box.append(row);
      }
    } catch (error) { if (generation === epoch && seq === botReadSeq) box.textContent = error.message; }
  }
  async function refresh(reloadRemoteAccess = false) {
    const generation = epoch, requestSeq = ++refreshSeq, updateRevision = updateSeq;
    const query = new URLSearchParams({ query: $('search').value.trim(), filter: $('filter').value, offset: String(offset), limit: String(limit) });
    if ($('device-user-filter').value) query.set('user_id',$('device-user-filter').value);
    try {
      const data = await request(`state?${query}`);
      // Paging/filter refreshes may overlap. Only the newest response can
      // repaint the table or change the health indicator.
      if (generation !== epoch || requestSeq !== refreshSeq) return data;
      if (reloadRemoteAccess) remoteAccessDirty = false;
      render(data, updateRevision); note('运行状态已刷新。');
      if (adminRole === 'owner') void refreshBotGroups();
      return data;
    }
    catch (error) { if (generation === epoch && requestSeq === refreshSeq) { if (authenticated) { $('health').textContent = '连接不可用'; $('health').className = 'pill bad'; } note(error.message, true); } throw error; }
  }
  function begin(action, ref, device = {}) { if (busy) return; if (action === 'ping') { perform({ action, device_ref: ref, reason: '', confirm: true }); return; } pending = { action, device_ref: ref }; $('operation-title').textContent = `确认${names[action]}`; $('operation-target').textContent = `${device.name || '设备'}${device.user_name ? ' · '+device.user_name : ''} · ${ref.slice(0,12)}`; $('operation-impact').textContent = {ban:'将断开电脑、撤销手机配对，并阻止此设备再次连接。电脑本地任务保留。',unban:'允许电脑重新连接；原手机配对不会恢复，需要重新扫码。用户禁用仍然有效。',revoke:'手机将失去这台电脑的访问权限，需要重新扫码。电脑本地任务保留。',disconnect:'临时断开远程连接，电脑可能自动重连。如需持续阻止访问，请禁用用户或封禁设备。'}[action] || ''; $('operation-error').textContent=''; $('reason').value = ''; $('operation').showModal(); }
  async function perform(value) { if (busy) return; const generation = epoch; busy = true; syncUpdateButtons(); note('正在执行；不会自动重试。'); try { await request('action', 'POST', value); await refresh(); if (generation === epoch) note('管理操作完成。'); } catch (error) { if (generation === epoch) note(error.message, true); } finally { if (generation === epoch) { busy = false; syncUpdateButtons(); } } }
  $('login-form').addEventListener('submit', async event => {
    event.preventDefault(); if (busy) return;
    const password = $('admin-password').value;
    if (password.length < 16 || new TextEncoder().encode(password).length > 1024) { note('请填写管理密钥。', true); return; }
    const generation = ++epoch; busy = true; $('admin-password').value = ''; $('login').disabled = true;
    try {
      const username = $('admin-username').value.trim();
      const session = await request('auth/login', 'POST', { password, ...(username ? {username} : {}) }, true);
      activateSession(session);
      if (generation === epoch) showWorkspace();
      await refresh();
    }
    catch (error) {
      if (generation === epoch) {
        // A valid login is enough to show the workspace shell. If the first
        // state request fails, keep the page visible and report the error.
        if (authenticated) note(error.message, true);
        else clearSession(error.message);
      }
    }
    finally { if (generation === epoch) { busy = false; $('login').disabled = false; syncUpdateButtons(); } }
  });
  $('create-bot-binding')?.addEventListener('click', async () => {
    if (busy || !authenticated || adminRole !== 'owner') return; const generation = epoch; busy = true; syncUpdateButtons();
    $('bot-binding-value').value = ''; $('bot-binding-result').hidden = true;
    try { const value = await request('bot/invites', 'POST', { confirm: true }); if (generation !== epoch) return; $('bot-binding-value').value = JSON.stringify(value, null, 2); $('bot-binding-result').hidden = false; await refreshBotGroups(); if (generation === epoch) note('Bot 绑定包已生成，请通过可信渠道交给电脑和手机。'); }
    catch (error) { if (generation === epoch) note(error.message, true); }
    finally { if (generation === epoch) { busy = false; syncUpdateButtons(); } }
  });
  $('copy-bot-binding')?.addEventListener('click', async () => { const value = $('bot-binding-value').value; try { await navigator.clipboard.writeText(value); note('绑定包已复制。'); } catch { $('bot-binding-value').focus(); $('bot-binding-value').select(); note('无法访问剪贴板，绑定包已选中。', true); } });
  $('logout').addEventListener('click', async () => {
    if (busy || !authenticated) return; busy = true; syncUpdateButtons(); const generation = epoch;
    try { await request('auth/logout', 'POST', {}); if (generation === epoch) clearSession(); }
    catch (error) { if (generation === epoch) note(`退出未确认：${error.message}`, true); }
    finally { if (generation === epoch) { busy = false; syncUpdateButtons(); } }
  });
  $('refresh').addEventListener('click', () => { if (activePage === 'users' && users) void users.refresh(); else if (activePage === 'updates') void readUpdateStatus(); else if ((activePage === 'operations' || activePage === 'audit') && operations) operations.refresh(); else if (activePage === 'network' && network) network.refresh(); else if (activePage === 'servers' && servers) servers.refresh(); else refresh().catch(() => {}); });
  $('reset-device-filter').addEventListener('click',()=>{ $('search').value=''; $('filter').value=''; $('device-user-filter').value=''; offset=0; void refresh().catch(()=>{}); });
  $('toggle-login-password').addEventListener('click',()=>{ const show=$('admin-password').type==='password'; $('admin-password').type=show?'text':'password'; $('toggle-login-password').textContent=show?'隐藏':'显示'; $('toggle-login-password').setAttribute('aria-pressed',String(show)); $('toggle-login-password').setAttribute('aria-label',show?'隐藏密码':'显示密码'); });
  $('create-enrollment').addEventListener('click', async () => {
    if (busy || !authenticated) return;
    const generation = epoch; busy = true; syncUpdateButtons(); $('enrollment-result').hidden = true; $('enrollment-value').value = '';
    note('正在生成并保存一次性登记码。');
    try {
      const user_id = $('enrollment-user').value;
      const value = await request('enrollment-claims', 'POST', { confirm: true, ...(user_id ? { user_id } : {}) });
      if (generation === epoch) {
        $('enrollment-value').value = value.claim;
        $('enrollment-expiry').textContent = `有效期至 ${new Date(value.expires_at).toLocaleString()}。该码只会显示这一次。`;
        $('enrollment-result').hidden = false;
        void users?.refreshEnrollments();
        note('登记码已安全保存，请通过可信渠道交给使用者。');
      }
    } catch (error) { if (generation === epoch) note(`${error.message} 如未收到登记码，请重新生成。`, true); }
    finally { if (generation === epoch) { busy = false; syncUpdateButtons(); } }
  });
  $('copy-enrollment').addEventListener('click', async () => {
    const value = $('enrollment-value').value;
    if (!value || busy) return;
    try { await navigator.clipboard.writeText(value); note('登记码已复制。'); }
    catch { $('enrollment-value').focus(); $('enrollment-value').select(); note('无法访问剪贴板，登记码已选中，可手动复制。', true); }
  });
  $('copy-hub-address').addEventListener('click', async () => {
    const value = $('hub-address').textContent.trim();
    if (!value || busy) return;
    try { await navigator.clipboard.writeText(value); note('Hub 地址已复制。'); }
    catch { note('无法访问剪贴板，请手动复制上方 Hub 地址。', true); }
  });
  $('change-password').addEventListener('click', () => { if (busy) return; clearPasswordForm(); $('password-dialog').showModal(); });
  $('cancel-password').addEventListener('click', () => { if (busy) return; clearPasswordForm(); $('password-dialog').close(); });
  $('password-dialog').addEventListener('close', clearPasswordForm);
  $('password-form').addEventListener('submit', async event => {
    event.preventDefault(); if (busy || !authenticated) return;
    const current_password = $('current-password').value, new_password = $('new-password').value;
    if (new_password !== $('confirm-password').value) { $('password-notice').textContent = '两次新密钥不一致。'; return; }
    if (new_password.length < 16 || new TextEncoder().encode(new_password).length > 1024) { $('password-notice').textContent = '新密钥至少 16 个字符，最长 1024 字节。'; return; }
    const generation = epoch; busy = true; syncUpdateButtons();
    // Clear sensitive inputs before the request; failures never auto-retry a password change.
    clearPasswordForm();
    try { const username = $('new-admin-username').value.trim(); const value = await request('auth/password', 'POST', { current_password, new_password, ...(username ? {username} : {}) }); if (generation === epoch) { $('admin-username').value=username; clearSession(value.durability_warning ? '账号和密码已保存，但同步未完全确认；请重新登录并检查服务器。' : '账号和密码已更新，请重新登录。'); } }
    catch (error) { if (generation === epoch) { $('password-dialog').close(); note(`${error.message} 若发送超时，请先尝试新密钥进入，不要重复提交。`, true); } }
    finally { if (generation === epoch) { busy = false; syncUpdateButtons(); } }
  });
  $('filter-form').addEventListener('submit', event => { event.preventDefault(); offset = 0; refresh().catch(() => {}); });
  $('prev').addEventListener('click', () => { offset = Math.max(0,offset-limit); refresh().catch(() => {}); }); $('next').addEventListener('click', () => { offset += limit; refresh().catch(() => {}); });
  $('cancel-operation').addEventListener('click', () => { pending = null; $('operation').close(); });
  $('operation-form').addEventListener('submit', event => { event.preventDefault(); if (!pending) return; const reason = $('reason').value.trim(); if (!reason || new TextEncoder().encode(reason).length > 256) { note('操作原因需要 1–256 字节。', true); return; } const action = { ...pending, reason, confirm: true }; pending = null; $('operation').close(); perform(action); });
  function renderUpdate(value) {
    if (!authenticated || adminRole !== 'owner') return;
    updateInfo = value; updateSeq++; updateTracking = value.status === 'updating';
    updateCandidate = value.configured !== false && !value.checking && value.status === 'available' && /^\d+\.\d+\.\d+(?:[-+][A-Za-z0-9.-]+)?$/.test(value.latest_version || '') && /^[a-f0-9]{64}$/.test(value.sha256 || '') ? { version: value.latest_version, sha256: value.sha256 } : null;
    const labels = { current: '已是最新版本', available: '发现新版本', unavailable: '暂时无法检查更新', unconfigured: '未连接更新组件', updating: '正在更新', failed: '更新未完成' };
    $('update-label').textContent = value.checking ? '正在检查' : value.status === 'current' && !value.latest_version ? '等待检查' : labels[value.status] || '状态待确认';
    $('update-label').className = 'pill' + (value.status === 'available' || value.status === 'current' && value.latest_version ? ' ok' : ['failed','unavailable'].includes(value.status) ? ' bad' : '');
    $('update-current').textContent = value.current_version || '—';
    if (value.current_version) $('version').textContent = `v${value.current_version}`;
    $('update-latest').textContent = value.latest_version || '尚未取得';
    $('update-checked').textContent = updateTime(value.checked_at, '尚无记录');
    $('update-next').textContent = updateTime(value.next_check_at, value.auto_check ? '等待安排' : '未开启');
    $('update-schedule').textContent = value.auto_check ? `后台自动检查已开启 · 每 ${Number(value.check_interval_seconds) >= 3600 ? `${Number(value.check_interval_seconds) / 3600} 小时` : `${Number(value.check_interval_seconds) / 60} 分钟`}检查一次。安装需由管理员确认。` : value.configured === false ? '此部署未启用应用更新。' : '当前启动器未启用自动检查，可手动检查；升级 Docker 镜像以启用后台检测。';
    $('update-status').textContent = value.message || labels[value.status] || '更新状态待确认';
    $('update-notes').textContent = value.release_notes || '';
    $('update-notes-wrap').hidden = !value.release_notes;
    $('update-error').textContent = '';
    $('update-badge').hidden = value.status !== 'available';
    $('update-banner').hidden = activePage === 'updates' || value.status !== 'available';
    $('update-banner-text').textContent = value.status === 'available' ? `Hub ${value.latest_version} 已发布，可前往版本更新安装。` : '';
    syncUpdateButtons(); scheduleUpdateStatus();
  }
  function updateTime(value, fallback) { const ms = Date.parse(value); return Number.isFinite(ms) ? new Date(ms).toLocaleString() : fallback; }
  function scheduleUpdateStatus() {
    clearTimeout(updateTimer); updateTimer = null;
    if (!authenticated || adminRole !== 'owner' || !updaterConfigured || (!updateInfo?.auto_check && !updateInfo?.checking && !updateTracking)) return;
    updateTimer = setTimeout(() => { updateTimer = null; void readUpdateStatus(); }, updateInfo?.checking || updateTracking ? 2000 : 60000);
  }
  async function readUpdateStatus() {
    if (!authenticated || adminRole !== 'owner' || !updaterConfigured || updateReading) return;
    if (busy) { scheduleUpdateStatus(); return; }
    const generation = epoch, seq = ++updateSeq; updateReading = true;
    try {
      const value = await request('update/status');
      if (generation === epoch && seq === updateSeq) renderUpdate(value);
    } catch (error) {
      if (generation === epoch && seq === updateSeq) { updateCandidate = null; $('update-error').textContent = `${error.message} 仅重试读取状态，不会重复安装。`; syncUpdateButtons(); }
    } finally { if (generation === epoch) { updateReading = false; scheduleUpdateStatus(); } }
  }
  $('view-update').addEventListener('click', () => { if (authenticated) showPage('updates', true); });
  function syncVersionShortcut() {
    const available = authenticated && adminRole === 'owner' && updaterConfigured && updateInfo?.configured !== false && updateInfo?.status === 'available';
    $('version').className = 'brand-version' + (available ? ' available' : '');
    $('version').disabled = !authenticated || adminRole !== 'owner' || busy;
    const hint = available ? `发现新版本 v${updateInfo.latest_version}，点击更新` : updateTracking ? '正在更新，点击查看进度' : '查看版本更新';
    $('version').title = hint;
    $('version').setAttribute('aria-label', `${$('version').textContent}，${hint}`);
  }
  $('version').addEventListener('click', () => {
    if (!authenticated || adminRole !== 'owner' || busy) return;
    if (updateCandidate) beginUpdate();
    else showPage('updates', true);
  });
  function syncUpdateButtons() { syncVersionShortcut(); network?.sync(); servers?.sync(); $('check-update').disabled = !authenticated || adminRole !== 'owner' || !updaterConfigured || updateInfo?.configured === false || busy || updateInfo?.checking || updateInfo?.status === 'updating'; $('update-result').disabled = !authenticated || adminRole !== 'owner' || !updaterConfigured || busy; $('install-update').disabled = !authenticated || adminRole !== 'owner' || !updateCandidate || busy; $('change-password').disabled = busy; $('save-password').disabled = busy; $('cancel-password').disabled = busy; $('logout').disabled = busy; $('create-enrollment').disabled = busy || !authenticated; $('copy-enrollment').disabled = busy || !authenticated || !$('enrollment-value').value; $('copy-hub-address').disabled = busy || !authenticated || !$('hub-address').textContent || $('hub-address').textContent === '正在读取 Hub 地址'; for (const button of $('mode-options').children) button.disabled = busy || button.dataset.mode === resourceMode; for (const id of ['cleanup-save','cleanup-run','cleanup-unpaired','cleanup-paired']) $(id).disabled = busy || !authenticated; for (const id of ['remote-access-mode','remote-official-enabled','remote-access-save']) $(id).disabled = busy || !authenticated || remoteAccessLocked; $('remote-access-reload').disabled = busy || !authenticated || !remoteAccessDirty; for (const scope of remoteScopes) { $(`remote-${scope}-enabled`).disabled = busy || !authenticated || remoteAccessLocked; $(`remote-${scope}-bases`).disabled = busy || !authenticated || remoteAccessLocked || !$(`remote-${scope}-enabled`).checked; } }
  function renderRemoteAccess(value) {
    const station = value.remote_access_station;
    remoteAccessLocked = value.remote_access_locked !== false || !station || ![1, 2].includes(station.version);
    const labels = { open: '按来源名单开放', restricted: '来源签名验证', disabled: '已暂停' };
    $('remote-access-current').textContent = labels[value.remote_access_mode] || '设置不可用';
    $('remote-access-current').className = value.remote_access_ready ? 'pill ok' : 'pill';
    if (!remoteAccessDirty) {
      remoteAccessRevision = typeof value.remote_access_revision === 'string' ? value.remote_access_revision : '';
      $('remote-access-mode').value = value.remote_access_mode || 'disabled';
      const lists = [station?.apiBases, station?.externalApiBases, station?.version === 1 ? station?.apiBases : station?.reverseProxyApiBases];
      remoteScopes.forEach((scope, index) => {
        const bases = Array.isArray(lists[index]) ? lists[index] : [];
        $(`remote-${scope}-enabled`).checked = bases.length > 0;
        $(`remote-${scope}-bases`).value = bases.join('\n');
      });
      $('remote-official-enabled').checked = station?.allowOfficialAccounts === true;
    }
    const statuses = Array.isArray(value.remote_access_sources) ? value.remote_access_sources : [];
    for (const [scope, id] of [['api', 'api'], ['external', 'external_api'], ['proxy', 'reverse_proxy'], ['official', 'official_account']]) {
      const source = statuses.find(item => item?.id === id);
      const messages = { disabled: '已关闭', paused: '全站远程执行已暂停', allowlist: '按地址名单允许，电脑端复核实际来源', computer_verification: '由电脑本地官方账号连接器验证', issuer_configured: '来源签名按 Agent 范围配置齐全；仍需验证上游账号和实际连通性', issuer_missing: `签名配置缺失或范围重叠：${Number(source?.issuerCoveredCount) || 0}/${Number(source?.addressCount) || 0} 个地址配置就绪` };
      if (scope === 'official') messages.allowlist = '允许受支持的官方账号，电脑端复核本地账号';
      $(`remote-${scope}-status`).textContent = source ? `当前生效：${messages[source.status] || '来源状态待确认'}` : '来源状态由当前服务器版本提供';
    }
    $('remote-access-details').textContent = remoteAccessLocked ? '远程来源由部署设置锁定。' : station.version === 1 ? '当前旧规则中 API 与反代共用名单。此处保留原范围，保存后升级为独立分类授权；旧电脑端需升级。' : value.remote_access_mode === 'restricted' ? '签名模式需要在服务器策略文件配置来源发行方、公钥及授权地址；开关和名单不会代替来源证明。保存后立即生效，已有任务继续运行。' : '保存后立即生效，服务重启后保留。新的请求使用新授权，已有任务继续运行。';
    if (remoteAccessDirty) $('remote-access-details').textContent = `有未保存的更改；来源状态显示已保存配置。${$('remote-access-details').textContent}`;
    syncUpdateButtons();
  }
  $('remote-access-form').addEventListener('input', () => { remoteAccessDirty = true; $('remote-access-details').textContent = '有未保存的更改；开关与地址保存后生效，来源状态显示已保存配置。'; syncUpdateButtons(); });
  $('remote-access-reload').addEventListener('click', async () => {
    if (busy || !authenticated || !remoteAccessDirty) return;
    const generation = epoch; busy = true; syncUpdateButtons();
    try { await refresh(true); }
    catch (error) { if (generation === epoch) note(error.message, true); }
    finally { if (generation === epoch) { busy = false; syncUpdateButtons(); } }
  });
  $('remote-access-form').addEventListener('submit', async event => {
    event.preventDefault(); if (busy || !authenticated || remoteAccessLocked || !remoteAccessRevision) return;
    const lists = remoteScopes.map(scope => $(`remote-${scope}-enabled`).checked ? $(`remote-${scope}-bases`).value.split(/\r?\n/).map(v => v.trim()).filter(Boolean) : []);
    if (new Set(lists.flat()).size > 32 || lists.some(list => list.length > 32) || lists.flat().some(v => v.length > 4096) || remoteScopes.some((scope, index) => $(`remote-${scope}-enabled`).checked && lists[index].length === 0)) { note('每个启用的来源类别都需要填写完整地址，三份名单合计最多 32 个不同地址。', true); return; }
    const [apiBases, externalApiBases, reverseProxyApiBases] = lists;
    const target = { mode: $('remote-access-mode').value, confirm: true, expectedRevision: remoteAccessRevision, station: { version: 2, apiBases, externalApiBases, reverseProxyApiBases, allowOfficialAccounts: $('remote-official-enabled').checked } };
    const generation = epoch; busy = true; syncUpdateButtons(); note('正在保存远程来源。');
    try { const value = await request('remote-access', 'POST', target); if (generation !== epoch) return; remoteAccessDirty = false; remoteAccessRevision = value.remote_access_revision; await refresh(); if (generation === epoch) note(value.message || '远程来源已保存。', Boolean(value.durability_warning)); }
    catch (error) { if (generation === epoch) note(`${error.message} 当前输入已保留；重新载入页面可读取实际设置。`, true); }
    finally { if (generation === epoch) { busy = false; syncUpdateButtons(); } }
  });
  function renderModes(value) {
    resourceMode = typeof value.resource_mode === 'string' ? value.resource_mode : '';
    modes = Array.isArray(value.resource_modes) ? value.resource_modes.filter(p => p && ['economy','balanced','performance'].includes(p.id)) : [];
    const current = modes.find(p => p.id === resourceMode);
    $('mode-current').textContent = current ? `当前：${current.label}` : '模式配置不可用';
    $('mode-options').replaceChildren();
    for (const mode of modes) {
      const button = text('button', '', `mode-card${mode.id === resourceMode ? ' selected' : ''}`); button.type = 'button'; button.dataset.mode = mode.id; button.setAttribute('aria-pressed', String(mode.id === resourceMode));
      button.append(text('strong', `${mode.label}${mode.id === 'economy' ? ' · 默认推荐' : ''}`), text('span', mode.description || '使用服务器预设资源策略'), text('small', `Go 内存软目标 ${Number(mode.memory_limit_mib)} MiB`));
      button.disabled = busy || mode.id === resourceMode;
      button.addEventListener('click', () => { if (busy || mode.id === resourceMode) return; modeCandidate = mode.id; $('mode-target').textContent = `即将切换为“${mode.label}”。只调整 Hub 服务，不修改 Docker 硬上限或 Sub2API。`; $('mode-dialog').showModal(); });
      $('mode-options').append(button);
    }
    $('mode-details').textContent = current ? `当前策略：每个设备账户最多缓存 ${Number(current.account_events)} 条事件 / 每个会话 ${Number(current.session_events)} 条${current.global_event_cache_bytes ? `；全站事件缓存预算 ${Number(current.global_event_cache_bytes) / 1048576} MiB` : ''}；手机流连接上限 ${Number(current.max_app_streams)} 个 / 账户${current.max_global_app_streams ? `、${Number(current.max_global_app_streams)} 个 / 全站` : ''}；Hub 待处理指令上限 ${Number(current.max_pending_commands)} 条。实际容量还取决于消息大小与服务器条件。` : '请确认已运行支持三种资源模式的独立 Hub 版本。';
  }
  function cleanupDays(days, label) { return Number(days) > 0 ? `${label}离线超过 ${Number(days)} 天清理` : `${label}不清理`; }
  function renderStorage(s) {
    if (!$('storage-form')) return;
    $('storage-current').textContent = !s?.enabled ? '未启用' : s.lowDisk ? '磁盘预留空间不足' : s.warning ? '容量接近上限' : '已启用';
    if (!s?.enabled) return;
    const p = s.policy;
    if (!storageDirty) {
      for (const [id,value] of [['storage-days',p.retentionDays],['storage-global',Math.floor(p.globalBytes/1048576)],['storage-binding',Math.floor(p.bindingBytes/1048576)]]) $(id).value=String(value);
      $('storage-queue').checked=Boolean(p.offlineQueueEnabled);
    }
    $('storage-details').textContent=`磁盘占用 ${(s.diskBytes/1048576).toFixed(1)} MiB（包含数据库、WAL、未完成上传和缩略图）；保留 ${s.eventCount} 条事件、${s.blobCount} 张图片。上次清理：${moment(s.lastCleanupAt)}。${s.lowDisk?'磁盘空间低于安全预留，新的正文和图片暂缓接收。':''}`;
  }
  async function saveStorage(runNow) {
    if(busy) return;
    const generation=epoch, edit=storageEdit, policy={retentionDays:Number($('storage-days').value),globalBytes:Number($('storage-global').value)*1048576,bindingBytes:Number($('storage-binding').value)*1048576,offlineQueueEnabled:$('storage-queue').checked};
    busy=true; $('storage-save').disabled=true; $('storage-run').disabled=true;
    try { const s=await request('storage','POST',runNow?{runNow:true}:{policy});if(generation===epoch){refreshSeq++;if(!runNow&&edit===storageEdit)storageDirty=false;renderStorage(s);note(runNow?'已完成一轮到期清理，仍在使用的图片会保留。':edit===storageEdit?'存储设置已保存；新内容按新保留期限处理。':'已保存提交时的设置；之后编辑的内容尚未保存。');} }
    catch(error){if(generation===epoch)note(error.message,true);}
    finally{if(generation===epoch){busy=false;$('storage-save').disabled=false;$('storage-run').disabled=false;}}
  }
  $('storage-form')?.addEventListener('submit',event=>{event.preventDefault();saveStorage(false);});
  $('storage-form')?.addEventListener('input',()=>{storageDirty=true;storageEdit++;});
  $('storage-form')?.addEventListener('change',()=>{storageDirty=true;storageEdit++;});
  $('storage-run')?.addEventListener('click',()=>saveStorage(true));
  function renderCleanup(value) {
    const c = value.device_cleanup;
    if (!c || typeof c !== 'object') { $('cleanup-current').textContent = '设置不可用'; return; }
    const on = Number(c.unpaired_days) > 0 || Number(c.paired_days) > 0;
    $('cleanup-current').textContent = on ? '已开启' : '已关闭'; $('cleanup-current').className = on ? 'pill ok' : 'pill';
    if (document.activeElement !== $('cleanup-unpaired')) $('cleanup-unpaired').value = String(Number(c.unpaired_days) || 0);
    if (document.activeElement !== $('cleanup-paired')) $('cleanup-paired').value = String(Number(c.paired_days) || 0);
    $('cleanup-details').textContent = `${cleanupDays(c.unpaired_days, '未配对电脑')}；${cleanupDays(c.paired_days, '已配对电脑')}。上次检查：${moment(c.last_run_at)}${Number(c.last_run_at) > 0 ? `，清理 ${Number(c.last_removed) || 0} 台` : ''}。`;
    syncUpdateButtons();
  }
  function readDays(id) { const raw = $(id).value.trim(); const n = Number(raw); return raw !== '' && Number.isInteger(n) && n >= 0 && n <= 3650 ? n : null; }
  $('cleanup-form').addEventListener('submit', event => {
    event.preventDefault(); if (busy) return;
    const unpaired = readDays('cleanup-unpaired'), paired = readDays('cleanup-paired');
    if (unpaired === null || paired === null) { note('清理天数需为 0 到 3650 之间的整数，0 表示不清理。', true); return; }
    cleanupCandidate = { unpaired_days: unpaired, paired_days: paired, confirm: true };
    $('cleanup-dialog-title').textContent = '保存自动清理设置';
    $('cleanup-target').textContent = `${cleanupDays(unpaired, '未配对电脑')}；${cleanupDays(paired, '已配对电脑')}。保存后会立即按新规则检查一次${paired > 0 ? '；被清理的已配对电脑需要在手机上重新扫码' : ''}。`;
    $('cleanup-dialog').showModal();
  });
  $('cleanup-run').addEventListener('click', () => {
    if (busy) return; cleanupCandidate = { run_now: true, confirm: true };
    $('cleanup-dialog-title').textContent = '立即清理一次';
    $('cleanup-target').textContent = '按当前已保存的规则立即检查并清理不活跃电脑。';
    $('cleanup-dialog').showModal();
  });
  $('cancel-cleanup').addEventListener('click', () => { cleanupCandidate = null; $('cleanup-dialog').close(); });
  $('cleanup-confirm-form').addEventListener('submit', async event => {
    event.preventDefault(); if (!cleanupCandidate || busy) return;
    const generation = epoch, target = cleanupCandidate; cleanupCandidate = null; busy = true; $('cleanup-dialog').close(); syncUpdateButtons(); note('正在处理清理设置；不会自动重试。');
    try { const value = await request('device-cleanup', 'POST', target); await refresh(); if (generation === epoch) note(`${target.run_now ? '清理完成' : '清理设置已保存并生效'}，本次清理 ${Number(value.removed) || 0} 台电脑。${value.durability_warning ? '配置同步未完成，断电后请重新确认设置。' : ''}`, Boolean(value.durability_warning)); }
    catch (error) { if (generation === epoch) note(`${error.message} 请刷新确认实际设置，不要直接重复提交。`, true); }
    finally { if (generation === epoch) { busy = false; syncUpdateButtons(); } }
  });
  $('cancel-mode').addEventListener('click', () => { modeCandidate = null; $('mode-dialog').close(); });
  $('mode-form').addEventListener('submit', async event => {
    event.preventDefault(); if (!modeCandidate || busy || !modes.some(p => p.id === modeCandidate)) return;
    const generation = epoch, target = { mode: modeCandidate, confirm: true }; modeCandidate = null; busy = true; $('mode-dialog').close(); syncUpdateButtons(); note('正在保存运行模式；不会自动重试。');
    try { const value = await request('resource-mode', 'POST', target); await refresh(); if (generation === epoch) note(value.durability_warning ? '模式已生效，但配置同步未完成；断电后请重新确认模式。' : '运行模式已保存并生效。设备绑定与电脑原会话不受影响。', Boolean(value.durability_warning)); }
    catch (error) { if (generation === epoch) note(`${error.message} 请刷新确认实际模式，不要直接重复提交。`, true); }
    finally { if (generation === epoch) { busy = false; syncUpdateButtons(); } }
  });
  async function checkUpdate(check = false) {
    if (!authenticated || adminRole !== 'owner' || !updaterConfigured || busy || check && (updateInfo?.configured === false || updateInfo?.checking || updateInfo?.status === 'updating')) return; const generation = epoch; updateSeq++; busy = true; syncUpdateButtons(); note(check ? '正在核对可信发布版本…' : '正在读取本地更新状态…');
    try { const value = await request(check ? 'update/check' : 'update/status', check ? 'POST' : 'GET', check ? {} : undefined); if (generation === epoch) { busy = false; renderUpdate(value); note('更新状态已读取。'); } }
    catch (error) { if (generation === epoch) { updateCandidate = null; note(error.message, true); } }
    finally { if (generation === epoch) { busy = false; syncUpdateButtons(); } }
  }
  $('check-update').addEventListener('click', () => checkUpdate(true)); $('update-result').addEventListener('click', () => checkUpdate(false));
  function beginUpdate() { if (!authenticated || adminRole !== 'owner' || !updateCandidate || busy) return; pendingUpdate = { ...updateCandidate }; $('update-target').textContent = `即将更新至 ${pendingUpdate.version}。设备绑定与配置保留，建议先在运维中心导出备份。`; $('update-dialog').showModal(); }
  $('install-update').addEventListener('click', beginUpdate);
  $('cancel-update').addEventListener('click', () => { pendingUpdate = null; $('update-dialog').close(); });
  $('update-dialog').addEventListener('close', () => { pendingUpdate = null; });
  $('update-form').addEventListener('submit', async event => {
    event.preventDefault(); if (!authenticated || adminRole !== 'owner' || !pendingUpdate || busy) return; const generation = epoch, target = { ...pendingUpdate, confirm: true }; pendingUpdate = null; updateCandidate = null; updateSeq++; updateTracking = true; busy = true; $('update-dialog').close(); syncUpdateButtons();
    showPage('updates', true);
    note('提交更新任务；连接可能暂时中断。不会自动重试。');
    try { const value = await request('update/apply', 'POST', target); if (generation === epoch) { busy = false; renderUpdate(value); note('更新任务已受理。这不等于更新完成，页面会自动读取进度；服务重启后可能需要重新登录。'); } }
    catch (error) { if (generation === epoch) { $('update-error').textContent = `${error.message} 请刷新状态确认结果，不要重复提交。`; note($('update-error').textContent, true); } }
    finally { if (generation === epoch) { busy = false; syncUpdateButtons(); scheduleUpdateStatus(); } }
  });
  async function restoreSession() {
    const generation = epoch;
    try {
      const session = await request('auth/session', 'GET', undefined, true);
      activateSession(session);
    }
    catch { if (generation === epoch) clearSession(''); return; }
    if (generation !== epoch) return;
    showWorkspace();
    void refresh().catch(() => undefined);
  }
  addEventListener('pagehide', () => clearSession('管理页面已关闭。'));
  addEventListener('pageshow', event => { if (event.persisted) restoreSession(); });
  restoreSession();
})();
