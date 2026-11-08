(() => {
  'use strict';
  // All requests retain the main app's cookie, CSRF, timeout and session guards.
  window.SalcaraNetwork = ({ request, getEpoch, isAuthenticated, isBusy }) => {
    const $ = id => document.getElementById(id);
    const modeNames = { manual: '手动固定', failover: '主备切换', latency: '延迟优选', balance: '负载均衡' };
    const healthFields = {
      interval_ms: ['network-health-interval', 30000, 5000, 3600000, 1000], timeout_ms: ['network-health-timeout', 6000, 100, 30000, 1000],
      concurrency: ['network-health-concurrency', 4, 1, 16, 1], failure_threshold: ['network-health-failures', 3, 1, 10, 1],
      recovery_successes: ['network-health-successes', 2, 1, 10, 1], recovery_hold_ms: ['network-health-recovery', 5000, 0, 600000, 1000],
      stale_after_ms: ['network-health-stale', 120000, 5000, 86400000, 1000],
    };
    const selectionFields = {
      switch_cooldown_ms: ['network-selection-cooldown', 120000, 0, 3600000, 1000],
      min_improvement_ms: ['network-selection-gain', 30, 0, 60000, 1],
      min_improvement_percent: ['network-selection-percent', 20, 0, 100, 1],
    };
    const limitFields = { max_requests: ['network-limit-requests', 256, 1, 4096], max_streams: ['network-limit-streams', 128, 1, 4096], queue_limit: ['network-limit-queue', 64, 0, 4096], queue_timeout_ms: ['network-limit-timeout', 2000, 1, 30000] };
    const healthNames = { unknown: '未测量', pending: '待检测', probing: '检测中', healthy: '可用', available: '可用', ok: '可用', degraded: '降级', failed: '不可达', unhealthy: '不可达', unavailable: '不可达', disabled: '已禁用', stale: '结果过期' };
    let visible = false, state = null, available = false, loading = false, mutating = false;
    let sequence = 0, readController = null, timer = null, pollBudget = 0, policyDirty = false, policyRevision = null, policyEdits = 0;
    let nodeDraft = null, deleteDraft = null, actionButtons = [];
    let inspecting = false, inspectionSequence = 0, inspectionController = null, subscriptionPreview = null;
    let subscriptionChoices = [], subscriptionSelected = new Set();
    let liveRefresh = false;
    let policyController = null;
    let releaseButtons = [];
    let observationController = null, observationSequence = 0, observing = false, observationOpenedFor = null;
    const sources = window.SalcaraSources?.({ request, getEpoch, isAuthenticated, isBusy, getPool: () => state, refreshPool: () => refresh({ polling: true }) });
    const geodata = window.SalcaraGeodata?.({ request, getEpoch, isAuthenticated, isBusy, refreshPool: () => refresh({ polling: true }) });
    const controllerID = value => typeof value?.controller_id === 'string' && value.controller_id ? value.controller_id : null;
    const advanced = value => Boolean(value?.health_policy && value?.selection_policy);
    const validRegion = value => new TextEncoder().encode(value).length <= 32 && !/[\p{Cc}\p{Cf}]/u.test(value);
    const readSettings = fields => {
      const result = {};
      for (const [key, [id, , min, max, scale]] of Object.entries(fields)) {
        const raw = $(id).value.trim(), parsed = Number(raw) * scale;
        if (!raw || !Number.isSafeInteger(parsed) || parsed < min || parsed > max) return null;
        result[key] = parsed;
      }
      return result;
    };
    const targetBody = (body, target) => target ? { ...body, expected_controller_id: target } : body;
    const controllerChangedMessage = '线路管理入口已变化，旧入口的草稿和订阅预览已清除。请重新打开节点或检测订阅后操作。';
    const text = (tag, value, cls) => { const el = document.createElement(tag); el.textContent = String(value ?? '—'); if (cls) el.className = cls; return el; };
    const clean = value => String(value ?? '').replace(/\b(?:https?|socks5h?):\/\/[^\s<>]+/gi, value => {
      try { const u = new URL(value); return `${u.protocol}//${u.host}${u.pathname && u.pathname !== '/' ? '/…' : ''}${u.username || u.password ? '（凭据已隐藏）' : ''}`; } catch { return '[地址已隐藏]'; }
    }).slice(0, 1200);
    const time = value => Number(value) > 0 ? new Date(Number(value)).toLocaleString() : '尚未检测';
    const metric = value => Number.isSafeInteger(value) && value >= 0 ? String(value) : '未采集';
    const occupancy = (value, maximum) => `${metric(value)} / ${Number.isSafeInteger(maximum) && maximum > 0 ? maximum : '未配置'}`;
    const integer = (id, min, max) => { const value = $(id).value.trim(); const parsed = Number(value); return value && Number.isSafeInteger(parsed) && parsed >= min && parsed <= max ? parsed : null; };
    const active = () => visible && isAuthenticated() && document.visibilityState !== 'hidden';
    const writable = () => available && state?.configured === true && !state?.release?.trial && !mutating && !inspecting && !isBusy();
    const note = (message, error = false) => { $('network-notice').textContent = clean(message); $('network-notice').className = 'notice inline-notice' + (error ? ' error' : ''); };
    const nodeNote = (message, error = false) => { $('network-node-notice').textContent = clean(message); $('network-node-notice').className = 'notice inline-notice' + (error ? ' error' : ''); };
    const subscriptionNote = (message, error = false) => { $('network-subscription-notice').textContent = clean(message); $('network-subscription-notice').className = 'notice inline-notice' + (error ? ' error' : ''); };
    const subscriptionCapacity = () => available && state?.configured === true && Array.isArray(state.nodes) ? Math.max(0, 32 - state.nodes.length) : 0;
    function syncSubscriptionSelection() {
      const capacity = subscriptionCapacity(), selected = subscriptionSelected.size;
      const blocked = !writable() || !subscriptionPreview || subscriptionPreview.expiresAt <= Date.now();
      for (const choice of subscriptionChoices) {
        choice.input.checked = subscriptionSelected.has(choice.index);
        choice.input.disabled = blocked || !choice.input.checked && selected >= capacity;
      }
      $('network-subscription-selection').textContent = subscriptionPreview
        ? `协议兼容 ${subscriptionPreview.supported} 个 / 已选 ${selected} 个 / 可新增 ${capacity} 个${selected > capacity ? ' · 容量已变化，请减少勾选后导入' : ''}` : '';
      $('network-subscription-import').disabled = blocked || selected < 1 || selected > capacity;
      $('network-subscription-import').textContent = selected ? `导入勾选的 ${selected} 个节点` : '导入勾选节点';
    }
    function clearSubscriptionPreview() {
      subscriptionPreview = null;
      subscriptionChoices = []; subscriptionSelected = new Set();
      $('network-subscription-preview').hidden = true;
      $('network-subscription-summary').textContent = ''; $('network-subscription-expiry').textContent = '';
      $('network-subscription-selection').textContent = '';
      $('network-subscription-nodes').replaceChildren(); $('network-subscription-warnings').replaceChildren();
    }
    function clearSubscription() {
      inspectionSequence++;
      inspectionController?.abort(); inspectionController = null; inspecting = false;
      $('network-subscription-url').value = ''; clearSubscriptionPreview(); subscriptionNote('');
    }
    function stopRead() {
      if (timer !== null) clearTimeout(timer);
      timer = null;
      if (readController) readController.abort();
      readController = null; sequence++; loading = false;
    }
    function clearNodeForm() {
      nodeDraft = null;
      $('network-node-name').value = ''; $('network-node-url').value = '';
      $('network-node-region').value = '';
      $('network-node-kind').value = 'proxy'; $('network-node-enabled').checked = true;
      $('network-node-weight').value = '1'; $('network-node-requests').value = '64'; $('network-node-streams').value = '16'; $('network-node-draining').checked = false;
      nodeNote('');
    }
    function closeNode() { if ($('network-node-dialog').open) $('network-node-dialog').close(); clearNodeForm(); }
    function syncNodeKind() {
      const direct = $('network-node-kind').value === 'direct';
      $('network-node-url-field').hidden = direct;
      $('network-node-url').required = !direct && !nodeDraft?.id;
      if (direct) $('network-node-url').value = '';
    }
    function policyHint() {
      const mode = $('network-mode').value;
      const labels = {
        manual: '固定使用选中的服务端节点；变更影响新连接，已有连接可能继续使用原线路。',
        failover: '优先使用指定节点，在探测失败时由入口选择可用备用节点。',
        latency: '入口根据到 Hub 的探测结果选择线路；较低探测延迟不等于用户端到端已加速。',
        balance: '将新流量分配给有空余容量的可用节点，结合权重与实际占用分流。已有流连接继续使用原节点。',
      };
      $('network-policy-hint').textContent = available ? labels[mode] || '请选择选路方式。' : '只有入口完成配置并可访问后，才能管理服务端选路。';
    }
    function sync() {
      const blocked = !writable();
      $('network-refresh').disabled = loading || mutating || !isAuthenticated();
      $('network-add').disabled = blocked;
      $('network-probe').disabled = blocked || Boolean(state?.probe_running);
      $('network-probe').textContent = state?.probe_running ? '检测进行中…' : '检测线路';
      $('network-probe-all').disabled = blocked || !advanced(state) || Boolean(state?.probe_running);
      $('network-probe-cancel').hidden = typeof state?.probe_run?.cancelled !== 'boolean' || !state?.probe_run?.running;
      $('network-probe-cancel').disabled = blocked || !state?.probe_run?.running || state?.probe_run?.cancelled === true;
      $('network-probe-cancel').textContent = state?.probe_run?.cancelled ? '正在停止检测…' : '停止本轮检测';
      for (const id of ['network-mode','network-policy-save']) $(id).disabled = blocked || !(state?.nodes || []).length;
      $('network-selected').disabled = blocked || ['latency','balance'].includes($('network-mode').value) || !(state?.nodes || []).length;
      for (const [id] of Object.values(limitFields)) $(id).disabled = blocked;
      for (const [id] of [...Object.values(healthFields), ...Object.values(selectionFields)]) $(id).disabled = blocked || !advanced(state);
      for (const id of ['network-health-enabled','network-region-mode','network-preferred-regions','network-allow-unknown']) $(id).disabled = blocked || !advanced(state);
      $('network-use-observed').disabled = blocked || !advanced(state) || !state?.geo_available;
      $('network-node-region').disabled = mutating || isBusy() || !advanced(state);
      $('network-live').checked = liveRefresh; $('network-live').disabled = !isAuthenticated();
      $('network-live-status').textContent = liveRefresh ? '此页可见时每 5 秒读取状态；切换页面或后台时暂停，不触发线路探测。' : '自动刷新未开启；刷新只读取状态，不触发线路探测。';
      $('network-policy-reload').disabled = loading || mutating || !isAuthenticated() || !policyDirty;
      $('network-subscription-url').disabled = !isAuthenticated() || inspecting || mutating || isBusy();
      $('network-subscription-inspect').disabled = !isAuthenticated() || inspecting || mutating || isBusy();
      $('network-subscription-inspect').textContent = inspecting ? '正在检测…' : '检测订阅';
      syncSubscriptionSelection();
      $('network-subscription-clear').disabled = inspecting || mutating;
      for (const button of actionButtons) button.disabled = blocked;
      for (const id of ['network-node-name','network-node-kind','network-node-url','network-node-enabled','network-node-weight','network-node-requests','network-node-streams','network-node-draining','network-node-save','network-node-cancel','network-delete-confirm','network-delete-cancel']) $(id).disabled = mutating || isBusy();
      for (const button of releaseButtons) button.disabled = loading || mutating || isBusy() || !active() || button.releaseBlocked;
      $('network-policy-save').textContent = state?.release ? '检查并准备应用' : '保存策略';
      $('network-node-save').textContent = state?.release ? '检查并准备应用' : '保存节点';
      $('network-delete-confirm').textContent = state?.release ? '检查并准备移除' : '移除节点';
      $('network-legacy-subscription').hidden = Boolean(state?.release);
      sources?.sync();
      $('network-observability-refresh').disabled = !active() || !available || observing || isBusy();
      $('network-observability-refresh').textContent = observing ? '读取中…' : '读取观测';
      $('network-observability-node').disabled = observing || !available;
      $('network-observability-window').disabled = observing || !available;
      policyHint();
    }
    const decisionReasons = { manual_selection: '按管理员指定的节点转发。', manual_unavailable: '指定节点不可用，当前没有入口线路。', weighted_capacity: '按节点权重和当前占用分配新请求，普通请求与长连接分别计算容量。', failover_precedence: '健康和地区筛选后优先指定节点，失败时按配置顺序切换。', lowest_latency: '从满足条件的节点中优先选择已验证且平滑延迟较低的线路。', switch_cooldown: '更快线路已达到改善门槛，正在等待切换冷却。', improvement_below_threshold: '其他线路的延迟改善尚未达到全部门槛。', no_eligible_route: '没有节点同时满足启停、健康、排空和地区条件。', disabled: '节点已停用', draining: '节点正在排空，只保留已有连接', manual_unselected: '手动模式下未选中此节点', unhealthy: '最近健康检查表明节点不可用', stale_disallowed: '健康样本过期，策略不接受未验证节点', unknown_disallowed: '节点尚未验证，策略不接受未验证节点', region_excluded: '节点不符合当前地区条件', capacity_full: '请求与长连接容量均已用尽', limited_trial: '仅允许一条恢复试探连接', selected: '当前入口正在使用此节点', eligible: '满足当前选路条件' };
    function stopObservation(clear = false) {
      observationSequence++; observationController?.abort(); observationController = null; observing = false;
      if (!clear) return;
      for (const id of ['completed', 'failed', 'rejected']) $('network-observability-' + id).textContent = '未读取';
      $('network-observability-note').textContent = '点击读取后显示实测数据；缺失时段不会补成零。';
      $('network-observability-decision').textContent = '选路原因尚未读取。'; $('network-observability-candidates').replaceChildren(); $('network-observability-samples').replaceChildren();
    }
    Object.assign(decisionReasons, { source_capacity_full: '该订阅的共享请求和长连接额度已满', source_request_capacity_full: '该订阅的共享请求额度已满', source_stream_capacity_full: '该订阅的共享长连接额度已满' });
    function observationOptions(value, changed) {
      $('network-observability-panel').hidden = !value?.release;
      const select = $('network-observability-node'), selected = changed ? '' : select.value;
      select.replaceChildren(); const all = text('option', '整个节点池'); all.value = ''; select.append(all);
      for (const node of value.nodes || []) { const option = text('option', node.name || '未命名节点'); option.value = node.id; select.append(option); }
      select.value = (value.nodes || []).some(node => node.id === selected) ? selected : '';
      if (changed || selected && !select.value) stopObservation(true);
    }
    async function refreshObservation() {
      if (!active() || !available || observing || !state?.release || isBusy()) return;
      stopObservation(true); observing = true;
      const epoch = getEpoch(), seq = observationSequence, target = controllerID(state), controller = new AbortController(); observationController = controller;
      observationOpenedFor = target;
      const nodeId = $('network-observability-node').value, windowMS = Number($('network-observability-window').value) || 86400000;
      sync();
      try {
        const value = await request(nodeId || windowMS !== 86400000 ? 'network/observability/query' : 'network/observability', nodeId || windowMS !== 86400000 ? 'POST' : 'GET', nodeId || windowMS !== 86400000 ? targetBody({ ...(nodeId ? { node_id: nodeId } : {}), window_ms: windowMS }, target) : undefined, false, controller.signal);
        if (seq !== observationSequence || epoch !== getEpoch() || target !== controllerID(state) || !active()) return;
        if (value.controller_id !== target || value.counter_semantics !== 'bucket_deltas_and_instantaneous_gauges') throw new Error('invalid telemetry scope');
        const samples = (Array.isArray(value.samples) ? value.samples : []).flatMap(sample => { const runtime = nodeId ? sample.nodes?.find(node => node.id === nodeId)?.runtime : sample.runtime; return runtime && Number.isSafeInteger(sample.at) && sample.at > 0 ? [{ at: sample.at, runtime }] : []; });
        for (const [id, field] of [['completed', 'completed_requests'], ['failed', 'failed_requests'], ['rejected', 'rejected_requests']]) {
          const counts = samples.map(sample => sample.runtime[field]);
          const total = counts.reduce((sum, count) => sum + (Number.isSafeInteger(count) && count >= 0 ? count : 0), 0);
          $('network-observability-' + id).textContent = samples.length && counts.every(count => Number.isSafeInteger(count) && count >= 0) && Number.isSafeInteger(total) ? String(total) : '未采集';
        }
        $('network-observability-note').textContent = `${samples.length} 个采样 · 每 ${Number(value.sample_interval_ms) / 60000 || 1} 分钟记录 · 缺失时段未补零${value.last_error ? ' · 部分历史数据保存或载入失败' : ''} · 读取于 ${new Date().toLocaleTimeString()}`;
        const decision = value.decision || {};
        $('network-observability-decision').textContent = `当前调度：${decisionReasons[decision.reason] || '暂未提供可解释的选路结果。'}${decision.request_capacity === false ? ' 池内普通请求容量已满。' : ''}${decision.stream_capacity === false ? ' 池内长连接容量已满。' : ''}`;
        const candidates = $('network-observability-candidates'); candidates.replaceChildren();
        for (const candidate of decision.candidates || []) {
          if (nodeId && candidate.id !== nodeId) continue;
          const name = state.nodes?.find(node => node.id === candidate.id)?.name || '历史节点';
          const row = text('div', '', 'observation-candidate'); row.append(text('strong', name), text('span', decisionReasons[candidate.reason] || '选路资格待确认', 'muted small')); candidates.append(row);
        }
        const rows = $('network-observability-samples'); rows.replaceChildren();
        for (const sample of samples.slice(-60).reverse()) { const row = document.createElement('tr'); row.append(text('td', time(sample.at)), ...['completed_requests', 'failed_requests', 'rejected_requests'].map(field => text('td', metric(sample.runtime[field])))); rows.append(row); }
        if (!samples.length) { const row = document.createElement('tr'), empty = text('td', '此窗口尚无采样数据。', 'empty-cell'); empty.colSpan = 4; row.append(empty); rows.append(row); }
      } catch { if (seq === observationSequence && epoch === getEpoch() && active() && !controller.signal.aborted) $('network-observability-note').textContent = '运行观测暂不可用，请刷新后再试；当前线路不受影响。'; }
      finally { if (seq === observationSequence && epoch === getEpoch()) { observing = false; observationController = null; sync(); } }
    }
    $('network-observability-refresh').addEventListener('click', () => { void refreshObservation(); });
    for (const id of ['network-observability-node', 'network-observability-window']) $(id).addEventListener('change', () => { stopObservation(true); sync(); });
    const releaseReasons = { preview_expired: '检查结果已过期，请重新准备。', no_verified_route: '没有已验证可用的线路，未应用变更。', preflight_cancelled: '检查已中断，请重新准备。', changed_node_unreachable: '变更的节点无法连接目标 Hub，未应用变更。', rollback_preserved_direct_block: '已恢复历史配置，并保留当前禁止直连的设置。', restart_rollback: '重启后自动恢复', deadline_rollback: '试用超时自动恢复', manual_rollback: '手动回滚', confirmed: '已确认', trial_confirmed: '试用已确认' };
    function releaseAction(parent, label, path, extra, blocked = false, cls = 'secondary') {
      const button = text('button', label, cls); button.type = 'button'; button.releaseBlocked = blocked;
      const revision = state.revision, target = controllerID(state), epoch = getEpoch();
      button.addEventListener('click', async () => {
        if (!active() || epoch !== getEpoch() || target !== controllerID(state) || revision !== state.revision || loading || mutating || isBusy() || button.releaseBlocked) return;
        if (path === 'start' && !state.release?.previews?.some(item => item.id === extra.preview_id && item.status === 'ready' && item.expires_at > Date.now())) { note('检查结果已过期，请重新准备。', true); return; }
        stopRead(); mutating = true; sync();
        try {
          await request(`network/releases/${path}`, 'POST', targetBody({ expected_revision: revision, ...extra }, target));
          if (epoch !== getEpoch() || target !== controllerID(state) || !isAuthenticated()) return;
          if (active()) { const loaded = await refresh(); if (epoch === getEpoch() && target === controllerID(state)) note(loaded ? path === 'start' ? '已开始限时试用。请检查连接效果，再确认保留。' : path === 'confirm' ? '试用配置已确认保留。' : '已请求回滚，请查看当前线路与发布状态。' : '操作已提交，状态暂未确认，请刷新核对。', !loaded); }
        } catch { if (epoch === getEpoch() && target === controllerID(state) && active()) note('发布操作未完成，配置可能已变化。请刷新核对后操作。', true); }
        finally { if (epoch === getEpoch() && target === controllerID(state)) { mutating = false; sync(); schedule(); } }
      });
      releaseButtons.push(button); parent.append(button);
    }
    function renderRelease(value) {
      const release = value.release, content = $('network-release-content'), history = $('network-release-history');
      $('network-release-panel').hidden = !release; content.replaceChildren(); history.replaceChildren(); releaseButtons = [];
      if (!release) return;
      $('network-release-detail').textContent = release.last_error ? releaseReasons[release.last_error] || '最近一次发布未完成，请核对当前配置。' : '先验证线路，再进入 2 分钟试用；未确认或服务重启时自动恢复上一份配置。';
      const trial = release.trial;
      $('network-release-status').textContent = trial ? '限时试用中' : release.previews?.some(item => item.status === 'checking') ? '正在检查' : '当前配置稳定';
      if (trial) {
        const row = text('div', '', 'release-trial'), remaining = Math.max(0, Math.ceil((trial.expires_at - Date.now()) / 1000));
        row.append(text('strong', remaining ? `剩余 ${remaining} 秒确认` : '试用已到期，正在等待回滚状态'), text('p', '新连接正在使用试用配置。确认后保留；未确认会自动回滚。', 'muted small'));
        const actions = text('div', '', 'actions');
        releaseAction(actions, '确认保留配置', 'confirm', { trial_id: trial.id }, remaining === 0, 'primary');
        releaseAction(actions, '立即回滚', 'rollback', { trial_id: trial.id }); row.append(actions); content.append(row);
      }
      for (const preview of release.previews || []) {
        const row = text('div', '', 'release-preview'), ready = preview.status === 'ready' && preview.expires_at > Date.now();
        row.append(text('strong', preview.status === 'checking' ? `正在检查 ${preview.completed} / ${preview.total} 个节点` : ready ? `检查通过 · ${preview.healthy} 个可用节点` : '本次配置未应用'));
        row.append(text('p', preview.reason ? releaseReasons[preview.reason] || '检查未通过，请重新核对变更。' : ready ? `检查结果有效至 ${time(preview.expires_at)}，试用后需要再次确认。` : preview.status === 'checking' ? '检查不会改变当前线路。' : '检查结果已过期，请重新准备。', 'muted small'));
        if (ready) releaseAction(row, '开始 2 分钟试用', 'start', { preview_id: preview.id }, Boolean(trial) || preview.base_revision !== value.revision, 'primary');
        content.append(row);
      }
      if (!content.children.length) content.append(text('p', '暂无待发布变更。编辑节点或策略后，点击“检查并准备应用”。', 'muted small'));
      for (const version of release.history || []) {
        const row = text('div', '', 'release-history-row'); row.append(text('span', `版本 ${version.revision} · ${time(version.created_at)} · ${releaseReasons[version.reason] || '历史配置'}`));
        releaseAction(row, '恢复此版本', 'rollback', { version_id: version.id }, !version.revertible || Boolean(trial)); history.append(row);
      }
      if (!history.children.length) history.append(text('p', '首次确认发布后会保留可恢复的历史版本。', 'muted small'));
    }
    function renderPolicy(value) {
      const nodes = Array.isArray(value.nodes) ? value.nodes : [];
      $('network-source-group-details').hidden = !Array.isArray(value.source_groups) || !value.source_groups.length;
      const groupList = $('network-source-group-list'); groupList.replaceChildren();
      for (const group of value.source_groups || []) {
        const row = text('div', '', 'network-info-row');
        const names = (group.node_ids || []).map(id => nodes.find(node => node.id === id)?.name).filter(Boolean);
        row.append(text('strong', names.length ? names.join('、') : group.retired ? '已移除节点的连接仍在排空' : '已配置额度的订阅'), text('span', `请求 ${group.runtime?.active_requests ?? 0}${group.limited ? ' / ' + group.max_requests : ''} · 长连接 ${group.runtime?.active_streams ?? 0}${group.limited ? ' / ' + group.max_streams : ''}${group.limited ? '' : ' · 来源未单独限额'}`, 'muted small'));
        groupList.append(row);
      }
      const selected = policyDirty ? $('network-selected').value : String(value.selected_id || '');
      const mode = policyDirty ? $('network-mode').value : value.mode || 'manual';
      const options = $('network-selected'); options.replaceChildren();
      const empty = text('option', nodes.length ? '请选择节点' : '尚无节点'); empty.value = ''; options.append(empty);
      for (const node of nodes) {
        const option = text('option', `${node.name || '未命名节点'}${node.enabled === false ? ' · 已禁用' : node.draining ? ' · 排空中' : ''}`);
        option.value = String(node.id); option.disabled = node.enabled === false || node.draining === true; options.append(option);
      }
      if (selected && !nodes.some(node => String(node.id) === selected)) {
        const stale = text('option', '草稿中的节点已不在当前池内'); stale.value = selected; options.append(stale);
      }
      options.value = selected;
      $('network-mode').value = Object.hasOwn(modeNames, mode) ? mode : 'manual';
      if (!policyDirty) {
        policyRevision = value.revision;
        policyController = controllerID(value);
        for (const [key, [id, fallback]] of Object.entries(limitFields)) $(id).value = String(value.limits?.[key] ?? fallback);
        for (const [fields, settings] of [[healthFields, value.health_policy], [selectionFields, value.selection_policy]]) {
          for (const [key, [id, fallback, , , scale]] of Object.entries(fields)) $(id).value = String((settings?.[key] ?? fallback) / scale);
        }
        $('network-health-enabled').checked = value.health_policy?.enabled !== false;
        $('network-region-mode').value = value.selection_policy?.region_mode || 'off';
        $('network-preferred-regions').value = (value.selection_policy?.preferred_regions || []).join('，');
        $('network-use-observed').checked = value.selection_policy?.use_observed_region === true;
        $('network-allow-unknown').checked = value.selection_policy?.allow_unknown !== false;
      }
      $('network-policy-current').textContent = value.configured ? `当前：${modeNames[value.mode] || '尚未设置'}${policyDirty ? ' · 有未保存草稿' : ''}` : '尚未配置';
      $('network-advanced-support').textContent = advanced(value) ? '地区由管理员标注，仅用于入口之后的选路；地理位置不等于线路质量。手动固定模式保持指定节点。' : '当前入口未提供高级检测策略；升级 Edge 后可配置。';
    }
    function renderProbe(value) {
      const policy = value.health_policy, run = value.probe_run;
      $('network-probe-policy').textContent = !policy ? '检测周期尚未读取' : policy.enabled
        ? `自动检测：每轮结束后 ${policy.interval_ms / 1000} 秒再启动 · 单节点超时 ${policy.timeout_ms / 1000} 秒 · 最多 ${Math.min(4, policy.concurrency)} 路并发（多个任务共享总上限 4）`
        : '周期检测已暂停；手动检测仍可使用，历史样本会继续过期。';
      $('network-probe-next').textContent = Number(value.next_probe_at) > 0 ? `下次计划 ${time(value.next_probe_at)}` : value.probe_running ? '本轮完成后安排下次检测' : '暂无下次计划';
      $('network-probe-progress').textContent = run
        ? `${run.cancelled ? run.running ? '正在停止本轮检测' : '本轮已取消' : run.running ? '检测中' : '最近一轮'}：${run.completed} / ${run.total} · 通过 ${run.succeeded} · 失败 ${run.failed} · 跳过 ${run.skipped}${run.pending ? ' · 后续检测已排队' : ''}`
        : '尚无检测记录。检测全部包括停用节点，但不会启用它们。';
    }
    function render(value) {
      const changedController = state !== null && controllerID(state) !== controllerID(value);
      if (changedController) {
        closeNode(); deleteDraft = null; if ($('network-delete-dialog').open) $('network-delete-dialog').close();
        policyDirty = false; policyRevision = null; policyController = null; policyEdits++; clearSubscription();
      }
      state = value; available = value.configured === true;
      const nodes = Array.isArray(value.nodes) ? value.nodes : [];
      const current = nodes.find(node => String(node.id) === String(value.active_id));
      const status = $('network-status');
      const statusNames = { ready: '入口已配置', active: '入口已配置', unknown: '线路待验证', unverified: '线路待验证', degraded: '线路降级', unavailable: '线路不可用', unhealthy: '线路不可用', stopped: '入口已停止', idle: '等待线路', unconfigured: '入口未配置' };
      status.textContent = available ? statusNames[value.status] || '入口已配置' : '入口未配置';
      status.className = 'pill' + (['unavailable','unhealthy','stopped'].includes(value.status) ? ' bad' : '');
      const durabilityWarning = value.durability === 'uncertain' ? '配置已应用，但磁盘持久化未确认，请检查存储。' : '';
      $('network-reason').textContent = durabilityWarning || clean(value.reason || (available ? '代理节点由服务端入口使用。请结合节点健康状态与实际业务连接确认线路效果。' : '此 Hub 尚未配置服务端线路入口。节点管理会在入口接通后可用。'));
      $('network-reason').className = durabilityWarning ? 'notice error' : 'muted';
      $('network-origin').textContent = available ? clean(value.origin || '未返回目标地址') : '—';
      $('network-active').textContent = available ? value.mode === 'balance' ? '按请求分配 · 负载均衡' : current?.name || (value.active_id ? '当前节点不在列表中' : '尚未选定') : '—';
      $('network-updated').textContent = new Date().toLocaleString();
      renderProbe(value);
      $('network-count').textContent = available ? `${nodes.length} 个节点 · ${nodes.filter(node => node.enabled !== false && !node.draining).length} 个允许新流量 · ${nodes.filter(node => node.draining).length} 个排空中` : '配置入口后管理节点';
      const runtime = value.runtime || {};
      $('network-load-requests').textContent = occupancy(runtime.active_requests, value.limits?.max_requests);
      $('network-load-streams').textContent = occupancy(runtime.active_streams, value.limits?.max_streams);
      $('network-load-queued').textContent = `${metric(runtime.queued_requests)} / ${metric(runtime.queued_streams)}`;
      for (const [id, key] of [['completed','completed_requests'],['failed','failed_requests'],['rejected','rejected_requests']]) $('network-load-' + id).textContent = metric(runtime[key]);
      const rows = $('network-nodes'); rows.replaceChildren(); actionButtons = [];
      for (const node of nodes) {
        const tr = document.createElement('tr');
        const identity = document.createElement('td'); identity.append(text('strong', node.name || '未命名节点'));
        if (value.mode !== 'balance' && String(node.id) === String(value.active_id)) identity.append(text('span', '当前线路', 'active-node'));
        identity.append(text('small', node.proxy_url ? clean(node.proxy_url) : '直连 Hub'));
        if (node.region) identity.append(text('small', `地区：${node.region} · 管理员标注`));
        if (node.network && node.network.status !== 'not_observed') {
          const info = node.network, details = text('details', '', 'node-network-details');
          const statuses = { expired: '观察已过期', dns_failed: '地址解析失败', unknown: '归属未知', mixed_networks: '多个地址归属不一致', direct_or_external: '直连或本地地址', lookup_failed: '查询失败', interrupted: '观察中断' };
          details.append(text('summary', info.status === 'observed' ? `${info.country || '地区未知'} · ${info.asn ? `AS${info.asn}` : 'ASN 未知'} · 服务器地址` : statuses[info.status] || '尚未确认归属'));
          details.append(text('p', `${info.organization || '网络组织未知'} · ${info.source || '未知来源'}。有效至 ${time(info.expires_at)}。服务器地址归属不代表出口或用户所在地。`, 'muted small'));
          if (info.addresses?.length) details.append(text('p', info.addresses.join(' · '), 'muted small'));
          identity.append(details);
        }
        if (node.has_credentials) identity.append(text('small', '连接凭据已保存，界面不回显'));
        const health = node.health || {}, kind = health.stale ? 'stale' : health.status || health.state || 'unknown';
        const healthCell = document.createElement('td'); healthCell.dataset.label = '健康状态';
        const healthy = ['healthy','available','ok'].includes(kind), failed = ['failed','unhealthy','unavailable'].includes(kind);
        if (node.enabled === false) healthCell.append(text('span', '已禁用', 'pill'));
        healthCell.append(text('span', (node.enabled === false ? '诊断：' : '') + (healthNames[kind] || '状态待确认'), 'pill' + (healthy ? ' ok' : failed ? ' bad' : '')));
        if (node.draining) healthCell.append(text('small', '排空中 · 不接收新流量', 'draining-state'));
        if (node.recovering) healthCell.append(text('small', '恢复试探中 · 限制新流量'));
        if (health.stale) healthCell.append(text('small', '检测结果已过期', 'draining-state'));
        if (Number(health.consecutive_failures) > 0) healthCell.append(text('small', `连续失败 ${health.consecutive_failures} 次`));
        const healthErrors = { probe_failed: '线路探测失败', connection_failed: '上游连接失败', probe_retry_pending: '探测未通过，等待重试' };
        if (health.error) healthCell.append(text('small', healthErrors[health.error] || clean(health.error)));
        if (Number(health.last_probe_at) > 0) healthCell.append(text('small', `最近探测 ${time(health.last_probe_at)}`));
        const latency = document.createElement('td'); latency.dataset.label = '入口 ↔ Hub';
        const measured = health.latency_ms !== null && health.latency_ms !== undefined && Number(health.checked_at) > 0 && Number.isFinite(Number(health.latency_ms)) && healthy;
        latency.append(text('span', measured ? `${Number(health.latency_ms).toFixed(1)} ms` : '—'), text('small', Number(health.checked_at) > 0 ? `上次成功 ${time(health.checked_at)}` : '尚无成功测量'));
        if (Number.isFinite(health.latency_ewma_ms)) latency.append(text('small', `平滑延迟 ${Number(health.latency_ewma_ms).toFixed(1)} ms${health.stale ? ' · 已过期' : ''}`));
        const load = document.createElement('td'); load.dataset.label = '占用与容量'; load.className = 'node-load';
        load.append(text('span', `请求 ${occupancy(node.runtime?.active_requests, node.max_requests)}`), text('small', `流连接 ${occupancy(node.runtime?.active_streams, node.max_streams)}`), text('small', `权重 ${Number.isSafeInteger(node.weight) ? node.weight : '未配置'} · 完成 ${metric(node.runtime?.completed_requests)} · 失败 ${metric(node.runtime?.failed_requests)}`));
        const actions = document.createElement('td');
        if (advanced(value)) {
          const probe = text('button', '测通', 'secondary'); probe.type = 'button';
          probe.addEventListener('click', () => probeNodes({ scope: 'all', node_ids: [node.id] }, controllerID(value))); actions.append(probe); actionButtons.push(probe);
        }
        for (const [label, handler] of [['编辑', () => openNode(node)], ['移除', () => openDelete(node)]]) {
          const button = text('button', label, label === '移除' ? 'secondary text-danger' : 'secondary'); button.type = 'button'; button.addEventListener('click', handler); actions.append(button); actionButtons.push(button);
        }
        tr.append(identity, healthCell, latency, load, actions); rows.append(tr);
      }
      if (!nodes.length) { const tr = document.createElement('tr'), td = text('td', available ? '尚无节点。添加代理入口或直连线路后开始检测。' : '服务端入口未配置，暂时没有节点可管理。', 'empty-cell'); td.colSpan = 5; tr.append(td); rows.append(tr); }
      const events = $('network-events'); events.replaceChildren();
      const entries = Array.isArray(value.events) ? value.events : [];
      const eventNames = { switch: '线路切换', route_changed: '线路切换', select: '选择线路', probe: '线路检测', health_changed: '健康状态变化', request_failed: '请求连接失败', configuration_changed: '配置变更', durability_warning: '保存状态提醒', policy: '更新策略', 'node-add': '添加节点', 'node-update': '更新节点', 'node-delete': '移除节点' };
      const eventMessages = { started: '入口已启动', probe_completed: '线路检测完成', connection_failed: '上游连接失败', upstream_connection_failed: '上游连接失败', configuration_changed: '配置已变更', configuration_saved: '配置已保存', configuration_saved_directory_sync_uncertain: '配置已应用，但磁盘持久化未确认', healthy: '线路可用', unhealthy: '线路不可达', degraded: '线路降级', unknown: '尚未验证' };
      for (const event of [...entries].reverse()) {
        const action = event.action || event.type || '线路事件';
        const subject = nodes.find(node => String(node.id) === String(event.node_id || event.to))?.name || '';
        const message = event.reason || event.message || '';
        events.append(text('p', [time(event.at), eventNames[action] || clean(action), subject, eventMessages[message] || clean(message)].filter(Boolean).join(' · ')));
      }
      if (!entries.length) events.append(text('p', '暂无线路记录。', 'muted'));
      renderPolicy(value); observationOptions(value, changedController); renderRelease(value); sources?.setContext(value, visible); geodata?.setContext(value, visible); sync();
      if (active() && value.release && observationOpenedFor !== controllerID(value)) void refreshObservation();
      return changedController;
    }
    function schedule() {
      if (!active() || loading || mutating || timer !== null) return;
      const releasePending = state?.release?.trial || state?.release?.previews?.some(item => item.status === 'checking' || item.status === 'ready' && item.expires_at > Date.now());
      if (!liveRefresh && !state?.probe_running && !releasePending) return;
      if (!liveRefresh && !releasePending && pollBudget <= 0) { note('检测仍在进行。已暂停自动查询，可点击“刷新状态”继续查看。'); return; }
      if (!liveRefresh && !releasePending) pollBudget--;
      timer = setTimeout(() => { timer = null; void refresh({ polling: true }); }, releasePending ? 3000 : liveRefresh ? 5000 : 1500);
    }
    async function refresh({ polling = false, discardPolicy = false } = {}) {
      if (!active()) return false;
      stopRead();
      const current = ++sequence, generation = getEpoch(), controller = new AbortController(); readController = controller;
      const editsAtStart = policyEdits;
      loading = true; if (!polling) pollBudget = 40; sync();
      try {
        const value = await request('network/state', 'GET', undefined, false, controller.signal);
        if (generation !== getEpoch() || current !== sequence || !active()) return false;
        // Discard only the draft that existed when reload began, never subsequent edits.
        if (discardPolicy && policyEdits === editsAtStart) { policyDirty = false; policyRevision = null; }
        const changedController = render(value);
        if (changedController) note(controllerChangedMessage, true);
        else if (!polling) note(value.probe_running ? '检测进行中，仅在此页可见时查询进度。' : '已读取服务端线路状态。');
        return true;
      } catch (error) {
        if (generation !== getEpoch() || current !== sequence || !active() || controller.signal.aborted) return false;
        available = false;
        sources?.setContext(null, visible);
        geodata?.setContext(null, visible);
        liveRefresh = false;
        $('network-status').textContent = '入口未接通'; $('network-status').className = 'pill bad';
        $('network-reason').textContent = '暂时无法读取服务端入口。下面可能保留上次状态，恢复连接并刷新后才能修改。';
        note(error.message || '无法读取服务端线路状态。', true);
        return false;
      } finally {
        if (generation === getEpoch() && current === sequence) { loading = false; readController = null; sync(); if (available) schedule(); }
      }
    }
    function openNode(node) {
      if (!writable()) return;
      clearNodeForm(); nodeDraft = { id: node?.id, revision: state.revision, controller: controllerID(state), direct: node ? !node.proxy_url : false };
      $('network-node-title').textContent = node ? '编辑服务端节点' : '添加服务端节点';
      $('network-node-name').value = node?.name || ''; $('network-node-kind').value = nodeDraft.direct ? 'direct' : 'proxy';
      $('network-node-region').value = node?.region || '';
      $('network-node-enabled').checked = node?.enabled !== false;
      $('network-node-weight').value = String(node?.weight ?? 1); $('network-node-requests').value = String(node?.max_requests ?? 64); $('network-node-streams').value = String(node?.max_streams ?? 16); $('network-node-draining').checked = node?.draining === true;
      $('network-node-url-hint').textContent = node ? '代理地址留空保留原地址和凭据；填写新地址将整体替换。' : '可含连接凭据；地址仅在本次提交时发送。';
      syncNodeKind(); sync(); $('network-node-dialog').showModal(); $('network-node-name').focus?.();
    }
    function openDelete(node) {
      if (!writable()) return;
      deleteDraft = { id: node.id, revision: state.revision, controller: controllerID(state) };
      $('network-delete-target').textContent = `确定移除“${node.name || '未命名节点'}”？`;
      $('network-delete-dialog').showModal();
    }
    async function mutate(path, body, onSuccess, successText, failureTarget) {
      if (!writable()) return;
      if (body.expected_controller_id && body.expected_controller_id !== controllerID(state)) { note(controllerChangedMessage, true); return; }
      stopRead(); mutating = true; const generation = getEpoch(); sync();
      const preparing = Boolean(state?.release) && ['network/nodes','network/nodes/delete','network/policy'].includes(path);
      const submittedPath = preparing ? 'network/releases/prepare' : path;
      let submittedBody = body;
      if (preparing) {
        const { expected_revision, expected_controller_id, ...payload } = body;
        submittedBody = targetBody({ expected_revision, action: path.slice('network/'.length), payload }, expected_controller_id);
      }
      try {
        const result = await request(submittedPath, 'POST', submittedBody);
        if (generation !== getEpoch() || !isAuthenticated()) return;
        onSuccess?.();
        if (active()) {
          const loaded = await refresh();
          if (generation === getEpoch()) {
            const uncertain = result?.durability === 'uncertain' || loaded && state?.durability === 'uncertain';
            if (uncertain) {
              $('network-reason').textContent = '配置已应用，但磁盘持久化未确认，请检查存储。' + (loaded ? '' : '入口状态暂时无法读取，请稍后刷新。');
              $('network-reason').className = 'notice error';
            }
            const changedController = loaded && body.expected_controller_id && body.expected_controller_id !== controllerID(state);
            note(changedController ? controllerChangedMessage : preparing ? loaded ? '配置检查已受理，当前线路尚未改变。请在配置发布区查看检查结果。' : '检查已提交，状态尚未确认，请刷新查看。' : uncertain ? '配置已应用，但磁盘持久化未确认，请检查存储。' : loaded ? successText : '操作已提交，但状态尚未确认。请刷新后检查实际结果。', !loaded || uncertain || changedController);
          }
        }
      } catch (error) {
        if (generation !== getEpoch() || !isAuthenticated()) return;
        const message = `${error.message || '操作未完成。'} 请刷新核对；不会自动重试。`;
        if (failureTarget) failureTarget(message, true);
        else if ($('network-node-dialog').open) nodeNote(message, true); else note(message, true);
      } finally {
        if (generation === getEpoch()) { mutating = false; sync(); if (available) schedule(); }
      }
    }
    $('network-refresh').addEventListener('click', () => { void refresh(); });
    $('network-live').addEventListener('change', () => { liveRefresh = isAuthenticated() && $('network-live').checked; if (timer !== null) clearTimeout(timer); timer = null; sync(); schedule(); });
    $('network-add').addEventListener('click', () => openNode(null));
    $('network-node-kind').addEventListener('change', syncNodeKind);
    $('network-node-cancel').addEventListener('click', closeNode);
    $('network-node-dialog').addEventListener('close', clearNodeForm);
    $('network-node-dialog').addEventListener('cancel', event => { if (mutating) event.preventDefault(); });
    $('network-node-form').addEventListener('submit', event => {
      event.preventDefault(); if (!writable() || !nodeDraft) return;
      const name = $('network-node-name').value.trim(), direct = $('network-node-kind').value === 'direct', address = $('network-node-url').value.trim();
      if (!name) { nodeNote('请填写节点名称。', true); return; }
      if (!direct && !address && (!nodeDraft.id || nodeDraft.direct)) { nodeNote('请填写 HTTP / SOCKS5 代理地址。', true); return; }
      if (address) {
        try { const url = new URL(address); if (!['http:','https:','socks5:','socks5h:'].includes(url.protocol) || !url.hostname) throw new Error(); }
        catch { nodeNote('请填写完整的 HTTP、HTTPS 或 SOCKS5 代理地址。', true); return; }
      }
      const weight = integer('network-node-weight', 1, 100), max_requests = integer('network-node-requests', 1, 4096), max_streams = integer('network-node-streams', 1, 4096);
      if (weight === null || max_requests === null || max_streams === null) { nodeNote('权重需为 1–100 的整数，请求与流连接上限需为 1–4096 的整数。', true); return; }
      const node = { name, enabled: $('network-node-enabled').checked, weight, max_requests, max_streams, draining: $('network-node-draining').checked };
      if (advanced(state)) {
        const region = $('network-node-region').value.trim();
        if (!validRegion(region)) { nodeNote('地区标签最多 32 字节，不能包含控制字符。', true); return; }
        node.region = region;
      }
      if (nodeDraft.id) node.id = nodeDraft.id;
      if (direct || address) node.proxy_url = direct ? '' : address;
      const body = targetBody({ expected_revision: nodeDraft.revision, node }, nodeDraft.controller);
      // Credentials never survive submission, an error, page exit or logout.
      $('network-node-url').value = ''; nodeNote(state?.release ? '正在准备检查节点…' : '正在保存节点…');
      void mutate('network/nodes', body, closeNode, '节点已保存，已读取最新状态。');
    });
    $('network-delete-cancel').addEventListener('click', () => { deleteDraft = null; $('network-delete-dialog').close(); });
    $('network-delete-dialog').addEventListener('cancel', event => { if (mutating) event.preventDefault(); });
    $('network-delete-dialog').addEventListener('close', () => { deleteDraft = null; });
    $('network-delete-form').addEventListener('submit', event => {
      event.preventDefault(); if (!writable() || !deleteDraft) return;
      const body = targetBody({ expected_revision: deleteDraft.revision, id: deleteDraft.id }, deleteDraft.controller);
      void mutate('network/nodes/delete', body, () => { deleteDraft = null; $('network-delete-dialog').close(); }, '节点已移除，已读取最新状态。');
    });
    const markPolicyDirty = () => { policyEdits++; policyDirty = true; sync(); };
    $('network-policy-form').addEventListener('input', markPolicyDirty);
    $('network-mode').addEventListener('change', markPolicyDirty);
    $('network-selected').addEventListener('change', markPolicyDirty);
    $('network-policy-reload').addEventListener('click', () => { void refresh({ discardPolicy: true }); });
    $('network-policy-form').addEventListener('submit', event => {
      event.preventDefault(); if (!writable() || policyRevision === null) return;
      const mode = $('network-mode').value, selected_id = $('network-selected').value;
      if (!Object.hasOwn(modeNames, mode)) { note('请选择有效的运行方式。', true); return; }
      if (!['latency','balance'].includes(mode) && !(state.nodes || []).some(node => String(node.id) === selected_id && node.enabled !== false && !node.draining)) { note('请为当前策略选择一个允许使用的节点。', true); return; }
      const limits = {};
      for (const [key, [id, , min, max]] of Object.entries(limitFields)) { limits[key] = integer(id, min, max); if (limits[key] === null) { note('请按字段范围填写容量与等待时间的整数值。', true); return; } }
      const body = { expected_revision: policyRevision, mode, selected_id: mode === 'balance' ? '' : selected_id, limits };
      if (advanced(state)) {
        const health_policy = readSettings(healthFields), selection_policy = readSettings(selectionFields);
        if (!health_policy || !selection_policy) { note('请按字段范围填写检测与选路参数。', true); return; }
        if (health_policy.timeout_ms > health_policy.interval_ms || health_policy.stale_after_ms < health_policy.interval_ms + health_policy.timeout_ms) { note('检测超时不能超过间隔；结果有效期至少覆盖间隔与单节点超时之和。', true); return; }
        const preferred_regions = [...new Map($('network-preferred-regions').value.split(/[,，\n]/).map(s => s.trim()).filter(Boolean).map(s => [s.toLocaleLowerCase(), s])).values()];
        const region_mode = $('network-region-mode').value;
        if (!['off','prefer','only'].includes(region_mode) || preferred_regions.length > 32 || preferred_regions.some(s => !validRegion(s)) || region_mode !== 'off' && !preferred_regions.length) { note('请填写有效地区标签：最多 32 项，每项最多 32 字节；启用地区规则时至少填写一项。', true); return; }
        body.health_policy = { ...health_policy, enabled: $('network-health-enabled').checked };
        body.selection_policy = { ...selection_policy, preferred_regions, region_mode, allow_unknown: $('network-allow-unknown').checked, use_observed_region: $('network-use-observed').checked };
      }
      void mutate('network/policy', targetBody(body, policyController), () => { policyDirty = false; }, '检测与选路策略已保存。已有连接保持原线路，新连接按策略调度。');
    });
    function probeNodes(options, target = controllerID(state)) { void mutate('network/probe', targetBody(advanced(state) ? options : {}, target), () => { pollBudget = 40; }, '检测请求已受理；请查看本轮进度与节点检测时间。'); }
    $('network-probe').addEventListener('click', () => probeNodes({ scope: 'enabled' }));
    $('network-probe-all').addEventListener('click', () => { if (advanced(state)) probeNodes({ scope: 'all' }); });
    $('network-probe-cancel').addEventListener('click', () => { const run = state?.probe_run; if (run?.running && !run.cancelled && typeof run.cancelled === 'boolean') void mutate('network/probe/cancel', targetBody({ run_id: run.id }, controllerID(state)), null, '停止请求已受理；本轮未完成项将记为跳过，周期检测仍按策略运行。'); });
    $('network-subscription-form').addEventListener('submit', async event => {
      event.preventDefault(); if (!isAuthenticated() || inspecting || mutating || isBusy()) return;
      const url = $('network-subscription-url').value.trim();
      $('network-subscription-url').value = '';
      // A new attempt always invalidates the previous preview, even if its URL is invalid.
      clearSubscriptionPreview(); sync();
      try { const parsed = new URL(url); if (parsed.protocol !== 'https:' || !parsed.hostname || parsed.username || parsed.password || parsed.hash || url.length > 8192) throw new Error(); }
      catch { subscriptionNote('请填写不含用户名、密码或片段的公网 HTTPS 订阅地址。', true); return; }
      inspecting = true;
      const generation = getEpoch(), current = ++inspectionSequence, controller = new AbortController(); inspectionController = controller;
      const target = controllerID(state);
      subscriptionNote('正在检查订阅格式与节点协议，尚未导入或切换线路。'); sync();
      try {
        const result = await request('network/subscriptions/inspect', 'POST', targetBody({ url }, target), false, controller.signal);
        if (generation !== getEpoch() || current !== inspectionSequence || !active()) return;
        const count = value => Number.isFinite(Number(value)) ? Math.max(0, Math.floor(Number(value))) : 0;
        const total = count(result.total), supported = count(result.supported), unsupported = count(result.unsupported);
        const formatNames = { 'clash': 'Clash YAML', 'clash_yaml': 'Clash YAML', 'clash_json': 'Clash JSON', 'clash-yaml': 'Clash YAML', 'mihomo': 'Mihomo / Clash YAML', 'uri_list': '节点链接列表', 'base64_uri_list': 'Base64 节点列表', 'uri-list': '节点链接列表', 'base64': 'Base64 订阅', 'json': 'JSON' };
        const reasonNames = { native_proxy: '当前转发器支持；导入后检测连通性', requires_mihomo: '需要 Mihomo 内核；请接入其 mixed / SOCKS5 监听地址', unsupported_protocol: '当前未支持此协议', invalid_node: '节点配置不完整或无效', insecure_tls: '此节点要求跳过证书验证，不能导入', unsafe_proxy_address: '订阅节点指向内网、本机或其他受限地址', duplicate_node: '重复节点，已排除' };
        const expiry = Number(result.expires_at) > 0 ? Number(result.expires_at) : Date.parse(result.expires_at);
        if (typeof result.preview_id === 'string' && result.preview_id && Number.isFinite(expiry)) subscriptionPreview = { id: result.preview_id, expiresAt: expiry, supported, controller: target };
        $('network-subscription-summary').textContent = `${formatNames[result.format] || clean(result.format) || '未知格式'} · 共 ${total} 个 · 不支持 ${unsupported} 个`;
        $('network-subscription-expiry').textContent = subscriptionPreview ? `本次预览有效至 ${new Date(expiry).toLocaleString()}` : '本次结果没有可导入的有效预览';
        const rows = $('network-subscription-nodes'); rows.replaceChildren();
        for (const node of Array.isArray(result.nodes) ? result.nodes : []) {
          const tr = document.createElement('tr'), name = document.createElement('td'), protocol = text('td', clean(node.protocol) || '未知协议'), status = document.createElement('td');
          const hasIndex = Number.isSafeInteger(node.index) && node.index >= 0 && !subscriptionChoices.some(choice => choice.index === node.index);
          if (node.supported === true && hasIndex) {
            const label = document.createElement('label'); label.className = 'subscription-node-choice';
            const input = document.createElement('input'); input.type = 'checkbox'; input.dataset.index = String(node.index);
            label.append(input, text('span', clean(node.name) || '未命名节点')); name.append(label);
            subscriptionChoices.push({ index: node.index, input });
            if (subscriptionPreview && subscriptionSelected.size < subscriptionCapacity()) subscriptionSelected.add(node.index);
            input.addEventListener('change', () => {
              if (input.checked) {
                if (!writable() || !subscriptionPreview || subscriptionPreview.expiresAt <= Date.now() || subscriptionSelected.size >= subscriptionCapacity()) {
                  input.checked = false;
                  subscriptionNote(`当前最多可新增 ${subscriptionCapacity()} 个节点。请先取消其他勾选或释放节点池容量。`, true);
                } else subscriptionSelected.add(node.index);
              } else subscriptionSelected.delete(node.index);
              sync();
            });
          } else name.append(text('span', clean(node.name) || '未命名节点'));
          protocol.dataset.label = '协议'; status.dataset.label = '检测结果';
          status.append(text('span', node.supported === true ? '协议支持' : '暂不支持', node.supported === true ? 'pill ok' : 'pill'));
          if (node.reason) status.append(text('small', reasonNames[node.reason] || clean(node.reason)));
          if (node.supported === true && !hasIndex) status.append(text('small', '缺少有效导入索引，请重新检测。'));
          tr.append(name, protocol, status); rows.append(tr);
        }
        if (!rows.children.length) { const tr = document.createElement('tr'), td = text('td', '订阅中没有可显示的节点。', 'empty-cell'); td.colSpan = 3; tr.append(td); rows.append(tr); }
        const warnings = $('network-subscription-warnings'); warnings.replaceChildren();
        for (const warning of Array.isArray(result.warnings) ? result.warnings : []) warnings.append(text('p', clean(warning)));
        $('network-subscription-preview').hidden = false;
        subscriptionNote(supported > 0 ? available ? '检测完成。已按剩余容量默认勾选前面的兼容节点；只导入实际勾选项，导入后仍需检测连通性。' : '检测完成，可查看协议兼容性。入口尚未配置，接通后请重新检测并勾选导入。' : '检测完成，当前没有协议兼容节点。请查看不支持原因，并核对服务端内核能力。', supported === 0);
      } catch (error) {
        if (generation === getEpoch() && current === inspectionSequence && active() && !controller.signal.aborted) subscriptionNote(error.message || '订阅检测未完成，请核对地址后重新检测。', true);
      } finally {
        if (generation === getEpoch() && current === inspectionSequence) { inspecting = false; inspectionController = null; sync(); }
      }
    });
    $('network-subscription-clear').addEventListener('click', () => { clearSubscription(); sync(); });
    $('network-subscription-import').addEventListener('click', () => {
      if (!writable() || !subscriptionPreview || subscriptionSelected.size < 1) return;
      if (subscriptionPreview.expiresAt <= Date.now()) { subscriptionNote('检测预览已过期，请重新检测订阅。', true); sync(); return; }
      if (subscriptionSelected.size > subscriptionCapacity()) { subscriptionNote('节点池剩余容量已变化。请减少勾选数量，系统不会自动截断导入列表。', true); sync(); return; }
      const node_indexes = [...subscriptionSelected].sort((a, b) => a - b);
      void mutate('network/subscriptions/import', targetBody({ preview_id: subscriptionPreview.id, expected_revision: state.revision, node_indexes }, subscriptionPreview.controller), () => {
        clearSubscriptionPreview(); subscriptionNote('勾选的节点已导入。请检测线路，再决定是否调整选路策略。');
      }, '订阅节点已导入。请检测线路，确认入口到 Hub 的实际连通性。', subscriptionNote);
    });
    document.addEventListener?.('visibilitychange', () => { if (!active()) { stopRead(); stopObservation(); clearSubscription(); } else if (visible) void refresh(); });
    function setVisible(next) {
      const changed = visible !== next; visible = next;
      sources?.setContext(state, next);
      geodata?.setContext(state, next);
      if (!next) { stopRead(); stopObservation(); observationOpenedFor = null; closeNode(); clearSubscription(); if ($('network-delete-dialog').open) $('network-delete-dialog').close(); return; }
      if (changed && active()) void refresh();
    }
    function reset() {
      visible = false; stopRead(); state = null; available = false; mutating = false; policyDirty = false; policyRevision = null; pollBudget = 0;
      liveRefresh = false;
      sources?.reset(); releaseButtons = []; $('network-release-panel').hidden = true; $('network-release-content').replaceChildren(); $('network-release-history').replaceChildren();
      geodata?.reset();
      $('network-source-group-details').hidden = true; $('network-source-group-list').replaceChildren();
      stopObservation(true); observationOpenedFor = null; $('network-observability-panel').hidden = true;
      policyController = null;
      closeNode(); clearSubscription(); deleteDraft = null; if ($('network-delete-dialog').open) $('network-delete-dialog').close();
      $('network-status').textContent = '尚未读取'; $('network-status').className = 'pill';
      $('network-reason').textContent = '进入此页后读取入口配置。'; $('network-reason').className = 'muted';
      for (const id of ['network-origin','network-active','network-updated']) $(id).textContent = '—';
      $('network-count').textContent = '尚未读取节点'; $('network-nodes').replaceChildren(); $('network-events').replaceChildren(); actionButtons = [];
      renderProbe({}); $('network-preferred-regions').value = ''; $('network-advanced-support').textContent = '尚未读取高级策略';
      for (const id of ['requests','streams','queued','completed','failed','rejected']) $('network-load-' + id).textContent = '未采集';
      $('network-mode').value = 'manual'; $('network-selected').replaceChildren(); $('network-policy-current').textContent = '尚未读取'; note(''); sync();
    }
    return { setVisible, refresh, reset, sync };
  };
})();
