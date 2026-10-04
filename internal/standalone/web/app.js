(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  const apiBase = '../_admin/v1/';
  let authenticated = false, csrf = '', epoch = 0, offset = 0, total = 0, pending = null, busy = false, updateCandidate = null, updaterConfigured = false, resourceMode = '', modes = [], modeCandidate = null, cleanupCandidate = null;
  const controllers = new Set();
  const names = { ping: '测量', revoke: '撤销绑定', disconnect: '临时断开', ban: '封禁', unban: '解封', 'resource-mode': '切换运行模式', 'device-cleanup': '修改自动清理', 'device-cleanup-run': '自动清理' };
  const limit = 50;
  function note(message, error = false) { $('notice').textContent = message; $('notice').className = `notice${error ? ' error' : ''}`; }
  function text(tag, value, cls = '') { const node = document.createElement(tag); node.textContent = String(value ?? '—'); if (cls) node.className = cls; return node; }
  function moment(ms) { return Number(ms) > 0 ? new Date(Number(ms)).toLocaleString() : '尚无记录'; }
  function clearData() { for (const id of ['online','registered','paired','running','waiting','pending','banned','errors']) $(id).textContent = '—'; $('devices').replaceChildren(); $('audit').replaceChildren(); }
  function clearSession(message = '已退出管理。') {
    authenticated = false; csrf = ''; epoch++; for (const c of controllers) c.abort(); controllers.clear();
    pending = null; busy = false; updateCandidate = null; updaterConfigured = false; resourceMode = ''; modes = []; modeCandidate = null; if ($('operation').open) $('operation').close(); if ($('update-dialog').open) $('update-dialog').close(); if ($('mode-dialog').open) $('mode-dialog').close(); cleanupCandidate = null; if ($('cleanup-dialog').open) $('cleanup-dialog').close(); $('cleanup-current').textContent = '未认证'; $('cleanup-details').textContent = '';
    $('mode-options').replaceChildren(); $('mode-current').textContent = '未认证'; $('mode-details').textContent = '';
    $('admin-password').value = ''; clearPasswordForm(); if ($('password-dialog').open) $('password-dialog').close(); $('login').disabled = false; $('workspace').hidden = true; $('login-panel').hidden = false; clearData(); syncUpdateButtons();
    $('health').textContent = '尚未认证'; $('health').className = 'pill'; note(message);
  }
  async function request(path, method = 'GET', body, anonymous = false) {
    if (!authenticated && !anonymous) throw new Error('请先登录。');
    const generation = epoch, controller = new AbortController(); controllers.add(controller);
    const timeout = setTimeout(() => controller.abort(), path === 'update/check' ? 30000 : 20000);
    try {
      const response = await fetch(apiBase + path, { method, headers: { ...(body !== undefined ? { 'Content-Type': 'application/json' } : {}), ...(method !== 'GET' && !anonymous ? { 'X-Salcara-CSRF': csrf } : {}) }, body: body !== undefined ? JSON.stringify(body) : undefined, credentials: 'same-origin', cache: 'no-store', redirect: 'error', signal: controller.signal, referrerPolicy: 'no-referrer' });
      if (generation !== epoch) throw new Error('管理员会话已关闭。');
      if (response.status === 401) { if (!anonymous) clearSession('登录已过期，请重新进入。'); throw new Error(path === 'auth/login' ? '管理密钥不正确。' : '请重新进入。'); }
      let data; try { data = await response.json(); } catch { throw new Error('返回的不是 Hub 接口，请检查反向代理路径。'); }
      if (generation !== epoch) throw new Error('管理员会话已关闭。');
      if (!response.ok) throw new Error(data.error || data.message || `请求失败（${response.status}）`);
      return data;
    } catch (error) { if (error.name === 'AbortError') throw new Error('请求中断或超时；操作可能已送达，请刷新状态，不要直接重复操作。'); throw error; }
    finally { clearTimeout(timeout); controllers.delete(controller); }
  }
  function activateSession(value) {
    if (typeof value.csrf_token !== 'string' || value.csrf_token.length < 32 || value.csrf_token.length > 128 || /[^A-Za-z0-9_-]/.test(value.csrf_token)) throw new Error('登录响应无效，请检查 Hub 地址。');
    csrf = value.csrf_token; authenticated = true;
  }
  function clearPasswordForm() { for (const id of ['current-password', 'new-password', 'confirm-password']) $(id).value = ''; $('password-notice').textContent = ''; }
  function showWorkspace() { $('workspace').hidden = false; $('login-panel').hidden = true; }
  function render(value) {
    if (!authenticated) return;
    const stats = value.stats || {};
    for (const [id, field] of Object.entries({ online:'online_devices', registered:'devices', paired:'paired_devices', running:'running_sessions', waiting:'waiting_approvals', pending:'pending_commands', banned:'banned_devices', errors:'request_errors' })) $(id).textContent = stats[field] ?? '—';
    $('health').textContent = 'Hub 已连接'; $('health').className = 'pill ok';
    $('version').textContent = value.standalone_version ? `服务 v${value.standalone_version}` : '独立 Docker 服务';
    renderModes(value);
    renderCleanup(value);
    $('latency').textContent = stats.latency_samples > 0 ? `Hub ↔ 电脑指令往返均值 ${Number(stats.latency_mean_ms).toFixed(1)} ms，${stats.latency_samples} 次回复样本。不是手机全链路延迟。` : '尚未测量 Hub ↔ 电脑指令往返；可对在线设备点击“测量”。';
    total = Number(value.total || 0); offset = Number(value.offset || 0);
    const rows = $('devices'); rows.replaceChildren();
    for (const d of value.devices || []) {
      const tr = document.createElement('tr'), identity = document.createElement('td');
      identity.append(text('strong', d.name || '未命名电脑'), text('small', `${d.os || '未知系统'} · Bridge ${d.version || '未知'}`), text('small', (d.tools || []).map(t => `${t.name || t.id}: ${t.available ? '已安装' : '未安装'}`).join('；') || '尚未上报工具'), text('small', `匿名标识 ${String(d.ref || '').slice(0,12)}…`));
      const connection = document.createElement('td'); connection.append(text('span', d.banned ? '已封禁' : d.online ? '在线' : '离线'), text('small', d.paired ? '已绑定手机' : '未绑定手机'), text('small', `最近上报 ${moment(d.last_seen)}`)); if (d.banned) connection.append(text('small', d.ban_reason));
      const latency = document.createElement('td'); if (d.latency?.samples) latency.append(text('span', `${Number(d.latency.last_ms).toFixed(1)} ms`), text('small', moment(d.latency.measured_at))); else latency.append(text('span', '未测量'));
      const actions = document.createElement('td'); actions.className = 'device-actions';
      for (const action of ['ping','revoke','disconnect', d.banned ? 'unban' : 'ban']) { const button = text('button', names[action], 'secondary'); button.type = 'button'; button.disabled = (action === 'ping' && (!d.online || d.banned)) || (action === 'disconnect' && !d.online); button.addEventListener('click', () => begin(action, d.ref)); actions.append(button); }
      tr.append(identity, connection, text('td', `运行 ${d.running_sessions ?? 0} / 待电脑确认 ${d.waiting_approvals ?? 0}`), latency, actions); rows.append(tr);
    }
    if (!rows.children.length) { const tr = document.createElement('tr'), td = text('td', '没有匹配设备'); td.colSpan = 5; tr.append(td); rows.append(tr); }
    $('page').textContent = `${total} 台匹配设备 · ${total ? offset+1 : 0}–${Math.min(offset+limit,total)}`; $('prev').disabled = offset <= 0; $('next').disabled = offset+limit >= total;
    const audit = $('audit'); audit.replaceChildren();
    for (const a of [...(value.audit || [])].reverse()) { const subject = a.device_ref ? `${String(a.device_ref).slice(0,12)}…` : '服务策略'; const reason = a.action === 'resource-mode' ? modes.find(p => p.id === a.reason)?.label || a.reason : a.reason || '测量连接'; audit.append(text('p', `${moment(a.at)} · ${a.actor === 'standalone-admin' ? '独立 Hub 管理员' : a.actor === 'hub' ? '自动任务' : '宿主管理员'} · ${names[a.action] || a.action} · ${subject} · ${reason}`)); }
    if (!audit.children.length) audit.append(text('p', '暂无管理操作。', 'muted'));
    updaterConfigured = Boolean(value.update_configured);
    if (!updateCandidate) $('update-status').textContent = updaterConfigured ? '已启用同容器签名程序更新；手动检查时才访问发布源。' : '当前由 Hub 单程序启动，没有连接签名更新组件。请使用配套 Docker 启动器，或按部署说明手动更新。';
    $('check-update').disabled = !updaterConfigured || busy; $('update-result').disabled = !updaterConfigured || busy; $('install-update').disabled = !updateCandidate || busy;
  }
  async function refresh() {
    const generation = epoch;
    const query = new URLSearchParams({ query: $('search').value.trim(), filter: $('filter').value, offset: String(offset), limit: String(limit) });
    try { const data = await request(`state?${query}`); render(data); note('状态已刷新；不会后台轮询。'); }
    catch (error) { if (generation === epoch) { if (authenticated) { $('health').textContent = '连接不可用'; $('health').className = 'pill bad'; } note(error.message, true); } throw error; }
  }
  function begin(action, ref) { if (busy) return; if (action === 'ping') { perform({ action, device_ref: ref, reason: '', confirm: true }); return; } pending = { action, device_ref: ref }; $('operation-title').textContent = `确认${names[action]}`; $('reason').value = ''; $('operation').showModal(); }
  async function perform(value) { if (busy) return; const generation = epoch; busy = true; syncUpdateButtons(); note('正在执行；不会自动重试。'); try { await request('action', 'POST', value); await refresh(); if (generation === epoch) note('管理操作完成。'); } catch (error) { if (generation === epoch) note(error.message, true); } finally { if (generation === epoch) { busy = false; syncUpdateButtons(); } } }
  $('login-form').addEventListener('submit', async event => {
    event.preventDefault(); if (busy) return;
    const password = $('admin-password').value;
    if (password.length < 16 || new TextEncoder().encode(password).length > 1024) { note('请填写管理密钥。', true); return; }
    const generation = ++epoch; busy = true; $('admin-password').value = ''; $('login').disabled = true;
    try { const session = await request('auth/login', 'POST', { password }, true); activateSession(session); await refresh(); if (generation === epoch) showWorkspace(); }
    catch (error) { if (generation === epoch) clearSession(error.message); }
    finally { if (generation === epoch) { busy = false; $('login').disabled = false; syncUpdateButtons(); } }
  });
  $('logout').addEventListener('click', async () => {
    if (busy || !authenticated) return; busy = true; syncUpdateButtons(); const generation = epoch;
    try { await request('auth/logout', 'POST', {}); if (generation === epoch) clearSession(); }
    catch (error) { if (generation === epoch) note(`退出未确认：${error.message}`, true); }
    finally { if (generation === epoch) { busy = false; syncUpdateButtons(); } }
  });
  $('refresh').addEventListener('click', () => refresh().catch(() => {}));
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
    try { const value = await request('auth/password', 'POST', { current_password, new_password }); if (generation === epoch) clearSession(value.durability_warning ? '密钥已更换，但保存或清理未完全确认；请用新密钥进入并检查服务器。' : '密钥已更换，请用新密钥进入。'); }
    catch (error) { if (generation === epoch) { $('password-dialog').close(); note(`${error.message} 若发送超时，请先尝试新密钥进入，不要重复提交。`, true); } }
    finally { if (generation === epoch) { busy = false; syncUpdateButtons(); } }
  });
  $('filter-form').addEventListener('submit', event => { event.preventDefault(); offset = 0; refresh().catch(() => {}); });
  $('prev').addEventListener('click', () => { offset = Math.max(0,offset-limit); refresh().catch(() => {}); }); $('next').addEventListener('click', () => { offset += limit; refresh().catch(() => {}); });
  $('cancel-operation').addEventListener('click', () => { pending = null; $('operation').close(); });
  $('operation-form').addEventListener('submit', event => { event.preventDefault(); if (!pending) return; const reason = $('reason').value.trim(); if (!reason || new TextEncoder().encode(reason).length > 256) { note('操作原因需要 1–256 字节。', true); return; } const action = { ...pending, reason, confirm: true }; pending = null; $('operation').close(); perform(action); });
  function renderUpdate(value) {
    if (!authenticated) return;
    updateCandidate = value.status === 'available' && /^\d+\.\d+\.\d+(?:-[A-Za-z0-9.-]+)?$/.test(value.latest_version || '') && /^[a-f0-9]{64}$/.test(value.sha256 || '') ? { version: value.latest_version, sha256: value.sha256 } : null;
    const labels = { current: '已经是最新版本', available: '发现可用更新', unavailable: '暂时无法取得可信更新', unconfigured: '没有连接更新组件', updating: '更新任务已受理，请稍后查询结果', failed: '更新未完成，请查看状态；不要自动重试', updated: '更新完成', rolled_back: '新程序验证失败，已回到旧程序' };
    $('update-status').textContent = `${labels[value.status] || '更新状态待确认'}${value.current_version ? ` · 当前 ${value.current_version}` : ''}${value.latest_version ? ` · 发布 ${value.latest_version}` : ''}${value.message ? `。${value.message}` : ''}${value.release_notes ? `\n${value.release_notes}` : ''}`;
    $('install-update').disabled = !updateCandidate || busy; $('check-update').disabled = !updaterConfigured || busy; $('update-result').disabled = !updaterConfigured || busy;
  }
  function syncUpdateButtons() { $('check-update').disabled = !updaterConfigured || busy; $('update-result').disabled = !updaterConfigured || busy; $('install-update').disabled = !updateCandidate || busy; $('change-password').disabled = busy; $('save-password').disabled = busy; $('cancel-password').disabled = busy; $('logout').disabled = busy; for (const button of $('mode-options').children) button.disabled = busy || button.dataset.mode === resourceMode; for (const id of ['cleanup-save','cleanup-run','cleanup-unpaired','cleanup-paired']) $(id).disabled = busy || !authenticated; }
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
    if (!updaterConfigured || busy) return; const generation = epoch; busy = true; syncUpdateButtons(); note(check ? '正在核对可信发布版本…' : '正在读取本地更新状态…');
    try { const value = await request(check ? 'update/check' : 'update/status', check ? 'POST' : 'GET', check ? {} : undefined); if (generation === epoch) { busy = false; renderUpdate(value); note('更新状态已读取，不会后台轮询。'); } }
    catch (error) { if (generation === epoch) { updateCandidate = null; note(error.message, true); } }
    finally { if (generation === epoch) { busy = false; syncUpdateButtons(); } }
  }
  $('check-update').addEventListener('click', () => checkUpdate(true)); $('update-result').addEventListener('click', () => checkUpdate(false));
  $('install-update').addEventListener('click', () => { if (!updateCandidate || busy) return; $('update-target').textContent = `即将更新至 ${updateCandidate.version}，只更新 Hub 程序，不升级 Sub2API。`; $('update-dialog').showModal(); });
  $('cancel-update').addEventListener('click', () => $('update-dialog').close());
  $('update-form').addEventListener('submit', async event => {
    event.preventDefault(); if (!updateCandidate || busy) return; const generation = epoch, target = { ...updateCandidate, confirm: true }; updateCandidate = null; busy = true; $('update-dialog').close(); syncUpdateButtons();
    note('提交更新任务；连接可能暂时中断。不会自动重试。');
    try { const value = await request('update/apply', 'POST', target); if (generation === epoch) { busy = false; renderUpdate(value); note('更新任务已受理。这不等于更新完成，请稍后点击“查询更新结果”。'); } }
    catch (error) { if (generation === epoch) note(`${error.message} 请稍后查询更新结果，不要重复提交。`, true); }
    finally { if (generation === epoch) { busy = false; syncUpdateButtons(); } }
  });
  async function restoreSession() {
    const generation = epoch;
    try { const session = await request('auth/session', 'GET', undefined, true); activateSession(session); await refresh(); if (generation === epoch) showWorkspace(); }
    catch { if (generation === epoch) clearSession(''); }
  }
  addEventListener('pagehide', () => clearSession('管理页面已关闭。'));
  addEventListener('pageshow', event => { if (event.persisted) restoreSession(); });
  restoreSession();
})();
