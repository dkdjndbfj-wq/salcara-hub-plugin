(() => {
  'use strict';
  window.SalcaraServers = ({ request, getEpoch, isAuthenticated, isBusy }) => {
    const $ = id => document.getElementById(id);
    const text = (tag, value, cls = '') => { const node = document.createElement(tag); node.textContent = String(value ?? '—'); node.className = cls; return node; };
    const time = value => Number(value) > 0 ? new Date(Number(value)).toLocaleString() : '尚无记录';
    const fields = ['servers-name','servers-host','servers-port','servers-username','servers-auth-type','servers-password','servers-private-key','servers-passphrase'];
    let visible = false, state = null, available = false, reading = false, working = false, timer = null;
    let readSequence = 0, operationSequence = 0, readController = null, operationController = null, actionButtons = [];
    let discovery = null, checked = null, planDraft = null, plan = null, deleteDraft = null;
    const active = () => visible && isAuthenticated() && document.visibilityState !== 'hidden';
    const writable = () => active() && available && !working && !isBusy();
    const validToken = value => typeof value === 'string' && value.length > 0 && value.length <= 256;
    const unexpired = value => value && Number(value.expires_at) > Date.now();
    const notice = (message, error = false, target = 'servers-notice') => { $(target).textContent = message; $(target).className = 'notice inline-notice' + (error ? ' error' : ''); };
    function errorMessage(error) {
      // Only return fixed backend diagnostics; arbitrary SSH output may contain secrets.
      const safeDiagnostics = new Set([
        '此 Hub 未配置相应架构的 Edge 部署文件', 'Edge 部署文件缺失或无效', '无法读取 Edge 部署文件', '部署文件已变化',
        '回源地址必须是独立的 HTTPS Hub 地址，不含凭据和参数', '回源主机无效', '服务器不存在或不满足自动部署条件',
        '服务器正在部署', '部署预览已达上限，请稍后重试', '部署方案已过期，请重新生成', '配置已变化，请重新生成部署方案',
        '服务器不存在或已有部署任务', '最多同时部署两台服务器，请稍后重试', '请刷新并选择已完成部署的服务器',
        '远程控制认证或固定回源验证失败', '配置版本已变化，请刷新后重试', '请输入有效主机和 SSH 端口', '主机必须是 IP 或域名',
        '不支持带区域标识的 IP', '请填写有效用户名并先核对主机指纹', '无法连接 SSH；请检查公网地址、端口与网络',
        'SSH 密码为空或过长', 'SSH 密钥过长', 'SSH 私钥或口令无效', '请选择密码或私钥认证',
        '主机指纹已变化，已停止认证；请重新核对', 'SSH 握手或认证失败，请检查用户名与凭据',
        '服务器环境检查失败', '服务器环境返回无效', '请输入不超过160字节的服务器名称', '检查结果已达上限或服务停止，请稍后重试',
        '检查结果已过期，请重新检查', '该服务器正在部署', '最多管理16台服务器', '请先取消活动入口或等待部署结束',
        '服务器不存在', '服务器管理已停止', '已有服务器操作进行中，请稍后重试', '服务器管理不可用',
      ]);
      if (safeDiagnostics.has(error?.message)) return error.message;
      const messages = {
        revision_conflict: '服务器配置已变化，请刷新后重新预检或生成方案。',
        check_expired: '预检已过期，请重新输入凭据并预检。', plan_expired: '部署方案已过期，请重新生成。',
        fingerprint_mismatch: '主机指纹不匹配，请通过可信渠道核对后重新发现。',
        ssh_auth_failed: 'SSH 认证失败，请检查用户名、密码或私钥。',
        artifact_unavailable: '此架构的 Edge 安装程序不可用，请管理员配置部署文件。',
        unsupported_platform: '此主机环境尚不支持部署，需要 Linux systemd 和 amd64 / arm64。',
        host_busy: '该主机已有运行中的部署任务，请等待结束。',
      };
      return messages[error?.message] || '操作未完成或状态尚未确认。请刷新状态核对；不会自动重试。';
    }
    function clearSecrets() { for (const id of ['servers-password','servers-private-key','servers-passphrase']) $(id).value = ''; }
    function clearCheck() {
      checked = null; $('servers-check-panel').hidden = true; $('servers-check-summary').textContent = '';
      $('servers-check-results').replaceChildren(); $('servers-check-warnings').replaceChildren(); $('servers-check-expiry').textContent = '';
    }
    function invalidateConnection() {
      discovery = null; clearCheck(); $('servers-fingerprint-panel').hidden = true; $('servers-fingerprint').textContent = ''; $('servers-trust').checked = false;
      sync();
    }
    function clearForm() {
      clearSecrets(); $('servers-name').value = ''; $('servers-host').value = ''; $('servers-port').value = '22'; $('servers-username').value = 'root'; $('servers-auth-type').value = 'password';
      invalidateConnection();
    }
    function clearPlan() {
      plan = null; $('servers-plan-preview').hidden = true;
      for (const id of ['servers-plan-origin','servers-plan-arch','servers-plan-sha','servers-plan-expiry']) $(id).textContent = '';
      $('servers-plan-changes').replaceChildren(); $('servers-plan-warnings').replaceChildren();
    }
    function stopRead() { if (timer !== null) clearTimeout(timer); timer = null; readController?.abort(); readController = null; readSequence++; reading = false; }
    function stopOperation() { operationController?.abort(); operationController = null; operationSequence++; working = false; }
    function sync() {
      const blocked = !writable(), editingBlocked = working || isBusy() || !isAuthenticated();
      for (const id of fields) $(id).disabled = editingBlocked;
      const key = $('servers-auth-type').value === 'key';
      $('servers-key-fields').hidden = !key; $('servers-password-field').hidden = key;
      $('servers-refresh').disabled = reading || working || !isAuthenticated();
      $('servers-discover').disabled = blocked;
      $('servers-trust').disabled = editingBlocked || !discovery;
      $('servers-check').disabled = blocked || !discovery || !$('servers-trust').checked || Boolean(checked);
      $('servers-save').disabled = blocked || !unexpired(checked);
      $('servers-clear').disabled = editingBlocked;
      $('servers-use-environment').disabled = blocked || !state?.active_id;
      $('servers-origin').disabled = editingBlocked;
      $('servers-plan-create').disabled = blocked || !planDraft;
      $('servers-deploy').disabled = blocked || !unexpired(plan);
      for (const id of ['servers-plan-cancel','servers-delete-cancel']) $(id).disabled = working;
      $('servers-delete-confirm').disabled = blocked || !deleteDraft;
      for (const { button, unavailable } of actionButtons) button.disabled = blocked || unavailable;
    }
    function render(value) {
      state = value; available = Number.isSafeInteger(value.revision) && value.revision >= 0;
      const hosts = Array.isArray(value.hosts) ? value.hosts : [], jobs = Array.isArray(value.jobs) ? value.jobs : [];
      const current = hosts.find(host => host.id === value.active_id);
      $('servers-summary').textContent = `${hosts.length} / 16 台服务器 · 线路入口：${current?.name || (value.active_id ? '记录待确认' : '环境配置')} · 最近读取 ${new Date().toLocaleString()}`;
      $('servers-artifacts').textContent = `Edge 部署文件：amd64 ${value.artifacts?.amd64 ? '已就绪' : '不可用'} · arm64 ${value.artifacts?.arm64 ? '已就绪' : '不可用'}`;
      const rows = $('servers-hosts'); rows.replaceChildren(); actionButtons = [];
      for (const host of hosts) {
        const row = document.createElement('tr'), identity = document.createElement('td'), status = document.createElement('td'), actions = document.createElement('td');
        identity.append(text('strong', host.name || host.host), text('small', `${host.username}@${host.host}:${host.port}`), text('small', host.fingerprint, 'fingerprint'));
        if (host.id === value.active_id) identity.append(text('span', '线路管理入口', 'pill ok'));
        status.dataset.label = '预检与部署'; status.append(text('span', host.deployed ? '已安装 Edge' : host.can_deploy ? '预检通过' : '暂不可部署', 'pill' + (host.deployed ? ' ok' : '')), text('small', `${host.platform || '未识别'} / ${host.arch || '未识别'} · ${host.auth_type === 'key' ? '私钥认证' : '密码认证'}`), text('small', `最近预检 ${time(host.last_checked_at)}`));
        if (host.origin) status.append(text('small', `回源 ${host.origin}`));
        const running = jobs.some(job => job.host_id === host.id && job.status === 'running');
        actions.className = 'server-actions';
        for (const [label, handler, unavailable] of [
          ['部署方案', () => openPlan(host), !host.can_deploy || running],
          ['设为线路入口', () => activate(host.id), !host.deployed || host.id === value.active_id || running],
          ['移除记录', () => openDelete(host), host.id === value.active_id || running],
        ]) {
          const button = text('button', label, 'secondary' + (label === '移除记录' ? ' text-danger' : '')); button.type = 'button'; button.addEventListener('click', handler); actions.append(button); actionButtons.push({ button, unavailable });
        }
        row.append(identity, status, actions); rows.append(row);
      }
      if (!hosts.length) { const row = document.createElement('tr'), cell = text('td', '尚未添加服务器。完成下方指纹核对和预检后保存。', 'empty-cell'); cell.colSpan = 3; row.append(cell); rows.append(row); }
      const jobBox = $('servers-jobs'); jobBox.replaceChildren();
      const statuses = { running: '部署中', succeeded: '部署完成', failed: '部署失败', interrupted: '任务中断，请先核对远端' };
      for (const job of [...jobs].reverse()) {
        const entry = document.createElement('details'); entry.className = 'server-job'; entry.open = job.status === 'running' || job.status === 'failed' || job.status === 'interrupted';
        const summary = document.createElement('summary'); summary.append(text('strong', hosts.find(host => host.id === job.host_id)?.name || '已移除的服务器'), text('span', statuses[job.status] || '状态待确认', 'pill' + (job.status === 'succeeded' ? ' ok' : ['failed','interrupted'].includes(job.status) ? ' bad' : '')), text('small', time(job.updated_at || job.created_at)));
        const log = document.createElement('ol'); log.className = 'server-job-log';
        for (const step of (Array.isArray(job.steps) ? job.steps : []).slice(-100)) log.append(text('li', `${time(step.at)} · ${String(step.message || '').slice(0, 2000)}`));
        entry.append(summary, log);
        if (job.error) entry.append(text('p', String(job.error).slice(0, 2000), 'notice error'));
        if (job.status === 'interrupted') entry.append(text('p', '管理服务重启中断了任务。请先核对远端程序与配置，再决定是否重新生成部署方案。', 'muted small'));
        jobBox.append(entry);
      }
      if (!jobs.length) jobBox.append(text('p', '尚无部署任务。', 'muted'));
      $('servers-job-status').textContent = jobs.some(job => job.status === 'running') ? '任务正在远端执行；此页可见时每 5 秒读取进度。离开页面不会取消已受理的部署。' : '没有运行中的任务。安装成功后仍需配置公网 HTTPS 入口并验证业务线路。';
      sync();
    }
    function schedule() {
      if (!active() || !available || reading || working || timer !== null || !(state?.jobs || []).some(job => job.status === 'running')) return;
      timer = setTimeout(() => { timer = null; void refresh(true); }, 5000);
    }
    async function refresh(polling = false) {
      if (!active() || working) return false;
      stopRead(); const sequence = ++readSequence, generation = getEpoch(), controller = new AbortController(); readController = controller; reading = true; sync();
      try {
        const value = await request('servers/state', 'GET', undefined, false, controller.signal);
        if (sequence !== readSequence || generation !== getEpoch() || !active()) return false;
        render(value); if (!polling) notice('服务器状态已读取。'); return true;
      } catch (error) {
        if (sequence !== readSequence || generation !== getEpoch() || !active() || controller.signal.aborted) return false;
        available = false; notice('服务器状态暂时无法读取，显示的是上次结果。请刷新后再操作。', true); return false;
      } finally {
        if (sequence === readSequence && generation === getEpoch()) { reading = false; readController = null; sync(); schedule(); }
      }
    }
    async function post(path, body, success, target = 'servers-notice') {
      if (!writable()) return;
      stopRead(); working = true; const sequence = ++operationSequence, generation = getEpoch(), controller = new AbortController(); operationController = controller; sync();
      try {
        const value = await request('servers/' + path, 'POST', body, false, controller.signal);
        if (sequence !== operationSequence || generation !== getEpoch() || !active()) return;
        success(value);
      } catch (error) {
        if (sequence !== operationSequence || generation !== getEpoch() || !active() || controller.signal.aborted) return;
        notice(errorMessage(error), true, target);
      } finally {
        if (sequence === operationSequence && generation === getEpoch()) { working = false; operationController = null; sync(); schedule(); }
      }
    }
    function connection() {
      const host = $('servers-host').value.trim(), port = Number($('servers-port').value);
      if (!host || /[\s/@?#]/.test(host) || !Number.isInteger(port) || port < 1 || port > 65535) { notice('请填写主机 IP / 域名和 1–65535 范围内的 SSH 端口。', true); return null; }
      return { host, port };
    }
    function openPlan(host) {
      if (!writable()) return;
      planDraft = { id: host.id, name: host.name || host.host, revision: state.revision }; clearPlan();
      $('servers-origin').value = host.origin || ''; $('servers-plan-target').textContent = `${host.name || host.host} · ${host.host} · ${host.arch}`; notice('', false, 'servers-plan-notice');
      $('servers-plan-dialog').showModal(); sync();
    }
    function openDelete(host) {
      if (!writable()) return;
      deleteDraft = { id: host.id, revision: state.revision }; $('servers-delete-target').textContent = `移除“${host.name || host.host}”的管理记录？`;
      $('servers-delete-dialog').showModal(); sync();
    }
    function activate(id) {
      if (!writable()) return;
      void post('activate', { id, expected_revision: state.revision }, value => { render(value); notice('线路管理入口已更新。进入“线路与代理池”检查实际状态。'); });
    }
    for (const id of fields) $(id).addEventListener(id === 'servers-auth-type' ? 'change' : 'input', () => { if (id === 'servers-auth-type') clearSecrets(); invalidateConnection(); });
    $('servers-trust').addEventListener('change', () => { clearCheck(); sync(); });
    $('servers-refresh').addEventListener('click', () => { void refresh(); });
    $('servers-use-environment').addEventListener('click', () => activate(''));
    $('servers-clear').addEventListener('click', () => { if (!working) { clearForm(); notice('表单已清空。'); } });
    $('servers-discover').addEventListener('click', () => {
      if (!writable()) return;
      const address = connection(); invalidateConnection(); if (!address) return;
      notice('正在发现主机指纹，未发送认证凭据…');
      void post('discover', address, value => {
        if (typeof value.fingerprint !== 'string' || !value.fingerprint.startsWith('SHA256:')) { notice('未取得有效主机指纹，请检查 SSH 地址。', true); return; }
        discovery = { ...address, fingerprint: value.fingerprint };
        $('servers-fingerprint').textContent = `${value.algorithm || 'SSH'} · ${value.fingerprint}`; $('servers-fingerprint-panel').hidden = false;
        notice('已取得指纹。请通过可信渠道核对并勾选确认，再提交认证预检。');
      });
    });
    $('servers-host-form').addEventListener('submit', event => {
      event.preventDefault(); if (!writable() || !discovery || !$('servers-trust').checked) return;
      const name = $('servers-name').value.trim(), username = $('servers-username').value.trim(), auth_type = $('servers-auth-type').value;
      if (!name || new TextEncoder().encode(name).length > 160 || !username || /\s/.test(username)) { notice('请填写不超过 160 字节的服务器名称与有效 SSH 用户名。', true); return; }
      const body = { name, ...discovery, username, auth_type }, revision = state.revision;
      if (auth_type === 'password') { body.password = $('servers-password').value; if (!body.password) { notice('请填写 SSH 密码。', true); return; } }
      else if (auth_type === 'key') { body.private_key = $('servers-private-key').value; body.passphrase = $('servers-passphrase').value; if (!body.private_key.trim()) { notice('请填写完整 SSH 私钥。', true); return; } }
      else return;
      clearSecrets(); clearCheck(); notice('正在认证并检查环境，凭据已从表单清空…');
      void post('check', body, value => {
        if (!validToken(value.check_id) || !unexpired(value)) { notice('预检结果已过期或无效，请重新预检。', true); return; }
        checked = { check_id: value.check_id, expires_at: value.expires_at, revision };
        $('servers-check-panel').hidden = false; $('servers-check-summary').textContent = `${value.platform || '未识别'} / ${value.arch || '未识别'} · ${value.can_deploy ? '环境支持部署' : '环境暂不支持部署'}`;
        for (const item of value.checks || []) $('servers-check-results').append(text('li', item));
        for (const item of value.warnings || []) $('servers-check-warnings').append(text('p', item));
        $('servers-check-expiry').textContent = `预检有效至 ${time(value.expires_at)}`; notice('预检已完成，请查看结果并保存服务器。');
      });
    });
    $('servers-save').addEventListener('click', () => {
      if (!writable() || !unexpired(checked)) { sync(); return; }
      const body = { check_id: checked.check_id, expected_revision: checked.revision };
      void post('save', body, value => { render(value); clearForm(); notice('服务器已保存；请在列表中生成并审阅部署方案。'); });
    });
    $('servers-origin').addEventListener('input', () => { clearPlan(); sync(); });
    $('servers-plan-form').addEventListener('submit', event => {
      event.preventDefault(); if (!writable() || !planDraft) return;
      const origin = $('servers-origin').value.trim();
      try { const url = new URL(origin); if (url.protocol !== 'https:' || url.username || url.password || url.hash || url.search) throw new Error(); }
      catch { notice('请填写不含凭据、查询参数和片段的 HTTPS Hub 回源地址。', true, 'servers-plan-notice'); return; }
      clearPlan(); notice('正在生成部署方案…', false, 'servers-plan-notice');
      planDraft.revision = state.revision;
      void post('plan', { id: planDraft.id, origin }, value => {
        if (!validToken(value.plan_id) || !unexpired(value) || value.host_id !== planDraft.id || !/^[a-f0-9]{64}$/i.test(value.artifact_sha256 || '')) { notice('部署方案无效或已过期，请重新生成。', true, 'servers-plan-notice'); return; }
        plan = { plan_id: value.plan_id, expires_at: value.expires_at, revision: planDraft.revision };
        $('servers-plan-preview').hidden = false; $('servers-plan-origin').textContent = value.origin; $('servers-plan-arch').textContent = value.arch; $('servers-plan-sha').textContent = value.artifact_sha256;
        for (const item of value.changes || []) $('servers-plan-changes').append(text('li', item));
        for (const item of value.warnings || []) $('servers-plan-warnings').append(text('p', item));
        $('servers-plan-expiry').textContent = `方案有效至 ${time(value.expires_at)}`; notice('请核对以上变更。点击“按此方案部署”才开始安装。', false, 'servers-plan-notice');
      }, 'servers-plan-notice');
    });
    $('servers-deploy').addEventListener('click', () => {
      if (!writable() || !unexpired(plan)) { sync(); return; }
      const body = { plan_id: plan.plan_id, expected_revision: plan.revision };
      // A failed or timed-out deployment submission cannot reuse this local approval token.
      clearPlan();
      void post('deploy', body, value => { render(value); $('servers-plan-dialog').close(); planDraft = null; notice('部署任务已受理，请查看任务日志；正在运行不代表安装完成。'); }, 'servers-plan-notice');
    });
    $('servers-plan-cancel').addEventListener('click', () => { if (!working) $('servers-plan-dialog').close(); });
    $('servers-plan-dialog').addEventListener('cancel', event => { if (working) event.preventDefault(); });
    $('servers-plan-dialog').addEventListener('close', () => { planDraft = null; clearPlan(); $('servers-origin').value = ''; notice('', false, 'servers-plan-notice'); });
    $('servers-delete-cancel').addEventListener('click', () => { if (!working) $('servers-delete-dialog').close(); });
    $('servers-delete-dialog').addEventListener('cancel', event => { if (working) event.preventDefault(); });
    $('servers-delete-dialog').addEventListener('close', () => { deleteDraft = null; });
    $('servers-delete-form').addEventListener('submit', event => {
      event.preventDefault(); if (!writable() || !deleteDraft) return;
      void post('delete', { id: deleteDraft.id, expected_revision: deleteDraft.revision }, value => { render(value); $('servers-delete-dialog').close(); deleteDraft = null; notice('服务器管理记录已移除，远端程序保持原状。'); });
    });
    function leave() {
      stopRead(); stopOperation(); clearForm();
      if ($('servers-plan-dialog').open) $('servers-plan-dialog').close(); if ($('servers-delete-dialog').open) $('servers-delete-dialog').close();
      planDraft = null; deleteDraft = null; clearPlan();
    }
    function setVisible(next) { const changed = visible !== next; visible = next; if (!next) { leave(); return; } if (changed && active()) void refresh(); }
    function reset() {
      visible = false; leave(); state = null; available = false; actionButtons = [];
      $('servers-hosts').replaceChildren(); $('servers-jobs').replaceChildren(); $('servers-summary').textContent = '进入此页后读取服务器。'; $('servers-artifacts').textContent = ''; $('servers-job-status').textContent = '仅有运行中的任务时，每 5 秒读取进度。'; notice(''); sync();
    }
    document.addEventListener?.('visibilitychange', () => { if (!active()) leave(); else if (visible) void refresh(); });
    clearForm();
    return { setVisible, refresh, reset, sync };
  };
})();
