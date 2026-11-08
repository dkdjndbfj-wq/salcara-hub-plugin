(() => {
  'use strict';
  window.SalcaraSources = ({ request, getEpoch, isAuthenticated, isBusy, getPool, refreshPool }) => {
    const $ = id => document.getElementById(id);
    const text = (tag, value, cls) => { const e = document.createElement(tag); e.textContent = String(value ?? ''); if (cls) e.className = cls; return e; };
    const statuses = { idle: '等待刷新', refreshing: '正在获取订阅', review: '等待核对节点', applying: '正在验证并应用', applied: '已应用', failed: '本次更新失败', interrupted: '任务已中断', storage_error: '保存失败' };
    const reasons = { managed_kernel: '由 Mihomo 内核转发', native_proxy: '原生代理', unsupported_protocol: '暂不支持此协议', invalid_node: '节点配置无效', insecure_tls: '证书验证不符合要求', unsafe_proxy_address: '地址不符合要求', duplicate_node: '重复节点' };
    const errors = {
      source_busy: '后台已有任务，请等待完成。', revision_conflict: '配置已变化，请刷新后重新核对。', source_not_found: '订阅已不存在，请刷新。', invalid_source: '请核对订阅名称和更新间隔。',
      subscription_url_invalid: '请填写有效的公网 HTTPS 订阅地址。', source_limit: '最多保存 16 个订阅。', subscription_no_supported_nodes: '订阅没有可用节点，当前线路保持原配置。',
      source_identity_ambiguous: '订阅中存在无法区分的节点，请联系服务商修正。', source_in_use: '活动配置或保留的历史版本仍引用此订阅，暂时无法删除。',
      source_candidate_changed: '候选节点已变化，请刷新并重新勾选。', source_selection_invalid: '请重新核对勾选的节点。', kernel_missing: '此协议需要先安装 Mihomo 内核。',
      trial_in_progress: '线路正在试用，请先确认或回滚。', restore_failed: '内核恢复失败，请检查服务器并重新安装。',
      kernel_not_installed: '此协议需要先安装 Mihomo 内核。', kernel_platform_unsupported: '当前服务器平台尚不支持自动安装内核。', kernel_hash_mismatch: '内核文件校验失败，已停止使用；请重新安装。',
      kernel_download_failed: '内核下载失败，请检查服务器网络后重试。', kernel_storage_failed: '无法保存内核，请检查服务器磁盘空间和权限。', kernel_process_failed: '内核进程已停止，请检查服务器并重新安装。',
      kernel_configuration_rejected: '内核未接受节点配置，请核对订阅。', kernel_start_failed: '内核未能启动，请检查服务器后重试。', kernel_draining: '旧连接尚未结束，请稍后再应用。',
      subscription_fetch_failed: '无法获取订阅，请检查地址、有效期和服务器网络；当前线路保持原配置。', subscription_format_unsupported: '无法识别订阅格式，请使用服务商提供的 Clash 配置或节点链接列表。',
      source_candidate_unreachable: '候选节点未通过到 Hub 的连通性验证，当前线路保持原配置。', source_candidate_expired: '候选版本已过期，请重新刷新订阅。', invalid_source_selection: '请核对节点选择，活动池最多 32 个节点。', source_storage_failed: '订阅保存失败，请检查服务器磁盘后刷新实际状态。',
      no_eligible_route: '当前策略下没有可用线路，已保留原配置，请检查所选线路或地区限制。', source_registry_limit: '订阅数据量超过上限，请减少订阅内容后重试。',
    };
    // Older admin adapters retain only translated messages, so accept these
    // exact fixed strings as aliases instead of displaying arbitrary errors.
    const failureAliases = { '活动配置或保留的历史版本仍引用此订阅，暂不能删除': 'source_in_use', '订阅或内核任务正在运行，请等待任务完成': 'source_busy', '候选节点未通过固定 Hub 回源验证，原配置继续保留': 'source_candidate_unreachable', '候选版本已过期，请重新刷新订阅': 'source_candidate_expired', '请选择有效节点，活动池最多 32 条线路': 'invalid_source_selection', '请先安装受管 Mihomo 内核，再应用复杂协议节点': 'kernel_not_installed', '上一版内核仍在承载连接，请排空后再发布下一版': 'kernel_draining', '配置正在试用，请先确认或回退，再进行其他修改': 'trial_in_progress', '发布后没有符合当前策略且可达的线路，原配置已保留': 'no_eligible_route' };
    const failure = error => errors[error?.code] || errors[error?.error] || errors[error?.message] || errors[failureAliases[error?.message]] || errors[typeof error === 'string' ? error : ''] || '操作未完成，请刷新核对后重试。';
    const time = value => Number(value) > 0 ? new Date(Number(value)).toLocaleString() : '暂无';
    let visible = false, enabled = false, state = null, target = null, loading = false, writing = false, sequence = 0, writeSequence = 0, reader = null, timer = null;
    let draft = null, removeDraft = null, choices = new Map(), buttons = [], quotaDraft = null, quotaLabels = new Map();
    const active = () => visible && enabled && isAuthenticated() && document.visibilityState !== 'hidden';
    const writable = () => active() && state?.configured === true && state.controller_id === target && !loading && !writing && !state.busy && !isBusy() && !getPool()?.release?.trial;
    const note = (message, error = false) => { $('sources-notice').textContent = message; $('sources-notice').className = 'notice inline-notice' + (error ? ' error' : ''); };
    const formNote = (message, error = false) => { $('source-form-notice').textContent = message; $('source-form-notice').className = 'notice inline-notice' + (error ? ' error' : ''); };
    function clearDraft() { draft = null; $('source-name').value = ''; $('source-url').value = ''; $('source-interval').value = '360'; $('source-enabled').checked = true; $('source-auto').checked = false; formNote(''); }
    function closeDraft() { if ($('source-dialog').open) $('source-dialog').close(); clearDraft(); }
    function closeQuota() { quotaDraft = null; if ($('source-quota-dialog').open) $('source-quota-dialog').close(); $('source-quota-notice').textContent = ''; }
    function syncQuotaLabels() {
      const pool = getPool();
      for (const [id, label] of quotaLabels) {
        const group = pool?.source_groups?.find(item => item.source_id === id);
        label.textContent = group ? `共享占用 · 请求 ${group.runtime?.active_requests ?? 0}${group.limited ? ' / ' + group.max_requests : ''} · 长连接 ${group.runtime?.active_streams ?? 0}${group.limited ? ' / ' + group.max_streams : ''}${group.limited ? '' : ' · 未设置来源总额度'}${group.retired ? ' · 等待旧连接排空' : ''}` : '尚无活动节点或来源共享额度';
        label.hidden = !Array.isArray(pool?.source_groups);
      }
    }
    function stop() { sequence++; reader?.abort(); reader = null; loading = false; if (timer !== null) clearTimeout(timer); timer = null; }
    function sync() {
      const blocked = !writable();
      $('sources-add').disabled = blocked || (state?.sources || []).length >= 16;
      $('sources-refresh').disabled = !active() || loading || writing;
      $('sources-kernel-install').disabled = blocked || state?.kernel?.status === 'installing';
      for (const button of buttons) button.disabled = blocked || Boolean(button.sourceBlocked);
      for (const choice of choices.values()) choice.update(blocked);
      for (const id of ['source-name', 'source-url', 'source-interval', 'source-enabled', 'source-auto', 'source-save', 'source-cancel', 'source-delete-confirm', 'source-delete-cancel']) $(id).disabled = writing || isBusy();
      for (const id of ['source-quota-enabled', 'source-quota-save', 'source-quota-cancel']) $(id).disabled = writing || isBusy();
      for (const id of ['source-quota-requests', 'source-quota-streams']) $(id).disabled = writing || isBusy() || !$('source-quota-enabled').checked;
      syncQuotaLabels();
    }
    function action(parent, label, handler, blocked = false, cls = 'secondary') {
      const button = text('button', label, cls); button.type = 'button'; button.sourceBlocked = blocked;
      const owner = target, epoch = getEpoch();
      button.addEventListener('click', () => { if (owner === target && epoch === getEpoch() && writable() && !button.sourceBlocked) handler(); });
      buttons.push(button); parent.append(button); return button;
    }
    function directory(source, previous) {
      const same = previous?.candidate === source.candidate_id, nodes = Array.isArray(source.nodes) ? source.nodes : [];
      const validSize = nodes.length <= 256, keys = new Map();
      for (const node of nodes) if (typeof node.key === 'string' && node.key) keys.set(node.key, (keys.get(node.key) || 0) + 1);
      const supported = node => validSize && node.supported === true && keys.get(node.key) === 1;
      const available = new Set(nodes.filter(supported).map(node => node.key));
      const selected = new Set([...(same ? previous.selected : nodes.filter(node => node.selected).map(node => node.key))].filter(key => available.has(key)));
      const choice = { candidate: source.candidate_id, selected, rows: [], query: same ? previous.query : '', protocol: same ? previous.protocol : '', status: same ? previous.status : '' };
      choices.set(source.id, choice);
      const current = () => choices.get(source.id) === choice;
      const capacity = () => Math.max(0, 32 - (getPool()?.nodes || []).filter(node => node.source_id !== source.id).length);
      const changed = node => ['added', 'updated', 'changed'].includes(node.change);
      const normalize = value => String(value || '').normalize('NFKC').toLocaleLowerCase();
      const matches = node => (!choice.query || normalize(`${node.name || ''} ${node.protocol || ''}`).includes(normalize(choice.query.trim()))) && (!choice.protocol || node.protocol === choice.protocol) &&
        (!choice.status || ({ supported: supported(node), unsupported: !supported(node), selected: selected.has(node.key), added: node.change === 'added', updated: ['updated', 'changed'].includes(node.change), unchanged: node.change === 'unchanged' })[choice.status]);
      const details = text('details', '', 'source-candidate'); details.open = same ? previous.details.open : source.status === 'review'; choice.details = details;
      details.append(text('summary', `核对候选节点 · ${nodes.length} / 256 个${source.removed ? ` · 已移除 ${source.removed} 个旧节点` : ''}`));
      const filters = text('div', '', 'source-directory-filters');
      const field = (name, control) => { const label = text('label', '', 'source-directory-field'); control.setAttribute('aria-label', name); label.append(text('span', name), control); filters.append(label); };
      const search = document.createElement('input'); search.type = 'search'; search.placeholder = '搜索节点名称或协议'; search.value = choice.query; field('搜索节点', search);
      const select = (name, options, value) => { const input = document.createElement('select'); for (const [key, label] of options) { const option = text('option', label); option.value = key; input.append(option); } input.value = value; field(name, input); return input; };
      const protocol = select('协议', [['', '全部协议'], ...[...new Set(nodes.map(node => node.protocol).filter(Boolean))].sort().map(value => [value, value])], choice.protocol);
      const status = select('目录状态', [['', '全部状态'], ['supported', '协议支持'], ['unsupported', '暂不支持'], ['selected', '已选'], ['added', '新增'], ['updated', '已变更'], ['unchanged', '未变更']], choice.status);
      details.append(filters);
      const result = text('p', '', 'source-directory-result muted small'), controls = text('div', '', 'source-directory-actions'), message = text('p', '', 'source-selection-message'); message.setAttribute('role', 'status');
      const changeSelection = mode => {
        if (!current()) return;
        const next = new Set(selected), matching = nodes.filter(matches);
        for (const node of matching) { if (mode !== 'all') next.delete(node.key); if (mode !== 'clear' && supported(node) && (mode === 'all' || changed(node))) next.add(node.key); }
        if (next.size > capacity()) { message.textContent = `本次选择共 ${next.size} 个，当前来源最多可放入 ${capacity()} 个。请缩小筛选范围；已选节点未改变。`; return; }
        selected.clear(); for (const key of next) selected.add(key); message.textContent = ''; sync();
      };
      action(controls, '全选筛选内支持项', () => changeSelection('all'), !validSize);
      action(controls, '仅选筛选内变更项', () => changeSelection('changed'), !validSize);
      action(controls, '清空筛选内选择', () => changeSelection('clear'), !validSize);
      details.append(result, controls, text('p', '批量操作仅影响筛选范围；范围外已选节点保留。“变更项”包含新增和已变更节点。', 'source-directory-help muted small'));
      const rows = text('div', '', 'source-node-list');
      if (validSize) for (const node of nodes) {
        const row = text('label', '', 'source-node'), input = document.createElement('input'); input.type = 'checkbox';
        const description = text('span', '', 'source-node-description'); description.append(text('strong', node.name || '未命名节点'), text('small', `${node.protocol || '未知协议'} · ${keys.get(node.key) !== 1 ? '节点标识无效' : reasons[node.reason] || (node.supported ? '协议支持' : '暂不支持')} · ${{ added: '新增', updated: '已变更', changed: '已变更', unchanged: '未变更' }[node.change] || '待核对'}`));
        choice.rows.push({ input, row, node });
        input.addEventListener('change', () => {
          if (!current()) return;
          if (writable() && supported(node)) { if (!input.checked) selected.delete(node.key); else if (selected.size < capacity()) selected.add(node.key); }
          message.textContent = ''; sync();
        });
        row.append(input, description); rows.append(row);
      }
      const empty = text('p', '', 'source-directory-empty muted small'); rows.append(empty);
      const count = text('p', '', 'source-selection-summary'); count.setAttribute('aria-live', 'polite');
      details.append(rows, count, message);
      const apply = action(details, '验证并应用勾选节点', () => {
        if (!current() || !selected.size || selected.size > capacity() || !validSize) return;
        void mutate('sources/apply', { id: source.id, candidate_id: source.candidate_id, expected_source_revision: state.source_revision, expected_revision: getPool()?.revision, node_keys: [...selected] });
      }, false, 'primary');
      choice.update = blocked => {
        let shown = 0, selectedShown = 0;
        for (const item of choice.rows) { item.row.hidden = !matches(item.node); if (!item.row.hidden) { shown++; if (selected.has(item.node.key)) selectedShown++; } item.input.checked = selected.has(item.node.key); item.input.disabled = blocked || !supported(item.node) || !item.input.checked && selected.size >= capacity(); }
        result.textContent = `显示 ${shown} / ${nodes.length} 个候选 · 协议支持 ${available.size} 个`;
        empty.hidden = shown > 0; empty.textContent = validSize ? '没有匹配的候选节点，请调整搜索或筛选。' : '候选目录超过每来源 256 个的上限，请减少订阅内容后重新获取。';
        count.textContent = `已选 ${selected.size} 个${selected.size > selectedShown ? `（筛选外 ${selected.size - selectedShown} 个）` : ''} · 当前来源可用 ${capacity()} / 32 个活动名额`;
        if (selected.size > capacity()) count.textContent += ' · 已超出容量，请取消部分选择';
        apply.sourceBlocked = !validSize || !selected.size || selected.size > capacity(); apply.disabled = blocked || apply.sourceBlocked;
      };
      for (const [input, key, event] of [[search, 'query', 'input'], [protocol, 'protocol', 'change'], [status, 'status', 'change']]) input.addEventListener(event, () => { if (!current()) return; choice[key] = input.value; message.textContent = ''; sync(); });
      return details;
    }
    function render(value) {
      if (value.controller_id !== target) { stop(); closeDraft(); state = null; choices.clear(); note('线路管理入口已变化，请刷新后重新操作。', true); return; }
      state = value; buttons = []; quotaLabels = new Map();
      const kernel = value.kernel || {};
      $('sources-kernel-status').textContent = kernel.status === 'installing' ? '正在下载并校验内核…' : kernel.installed ? `Mihomo ${kernel.version || ''} · 已安装` : 'Mihomo 尚未安装';
      $('sources-kernel-detail').textContent = kernel.error ? failure(kernel.error) : kernel.installed ? Number(kernel.generations) > 0 ? `保留 ${Number(kernel.generations)} 代运行配置，供现有连接结束。` : '内核已就绪，勾选并应用订阅节点后开始转发。' : 'SS、VMess、VLESS、Trojan 等协议由服务器内核接入。';
      $('sources-kernel-install').textContent = kernel.installed ? '校验 / 重新安装' : '安装 Mihomo';
      $('sources-count').textContent = `${value.sources?.length || 0} / 16 个订阅${value.busy ? ' · 后台任务进行中' : ''}`;
      const list = $('sources-list'), previous = choices; list.replaceChildren(); choices = new Map();
      for (const source of Array.isArray(value.sources) ? value.sources : []) {
        const card = text('article', '', 'source-card'), head = text('div', '', 'section-title');
        const identity = text('div', ''), label = text('h3', source.name || '未命名订阅'); identity.append(label, text('p', source.enabled ? `每 ${source.interval_ms / 60000} 分钟检查 · ${source.auto_apply ? '自动更新已选节点' : '手动核对后应用'}` : '已暂停定时更新', 'muted small'));
        head.append(identity, text('span', statuses[source.status] || '状态待确认', 'pill' + (source.status === 'failed' || source.status === 'storage_error' ? ' bad' : source.status === 'applied' ? ' ok' : ''))); card.append(head);
        card.append(text('p', `最近获取 ${time(source.last_success)} · 最近应用 ${time(source.last_applied)} · 下次检查 ${source.enabled ? time(source.next_refresh) : '已暂停'}`, 'muted small'));
        if (source.error) card.append(text('p', failure(source.error), 'notice error'));
        const actions = text('div', '', 'actions source-actions');
        action(actions, '刷新订阅', () => { void mutate('sources/refresh', { id: source.id }); });
        action(actions, '编辑', () => open(source));
        action(actions, '共享额度', () => openQuota(source), !Array.isArray(getPool()?.source_limits));
        action(actions, '删除订阅', () => { removeDraft = { id: source.id, revision: state.source_revision, target }; $('source-delete-name').textContent = `删除“${source.name}”的订阅来源？活动配置或保留的历史版本仍引用此订阅时，会阻止删除。`; $('source-delete-dialog').showModal(); }, false, 'danger subtle');
        card.append(actions);
        const quota = text('p', '', 'muted small'); quotaLabels.set(source.id, quota); card.append(quota);
        if (source.candidate_id) card.append(directory(source, previous.get(source.id)));
        list.append(card);
      }
      if (!list.children.length) list.append(text('p', '添加服务商的订阅地址，在这里集中更新和选择节点。', 'empty-cell'));
      sync();
    }
    function schedule() { if (!active() || loading || writing || timer !== null || !(state?.busy || state?.kernel?.status === 'installing' || state?.sources?.some(item => ['refreshing', 'applying'].includes(item.status)))) return; timer = setTimeout(() => { timer = null; void refresh(true); }, 3000); }
    async function refresh(polling = false) {
      if (!active()) return; stop(); const seq = sequence, epoch = getEpoch(), owner = target, controller = new AbortController(); reader = controller; loading = true; sync();
      try { const result = await request('network/sources/state', 'GET', undefined, false, controller.signal); if (seq !== sequence || epoch !== getEpoch() || owner !== target || !active()) return; if (result.controller_id !== owner) { state = null; note('线路管理入口已变化，请刷新后重新操作。', true); return; } const wasBusy = state?.busy; render(result); if (!polling) note('已读取订阅状态。'); if (wasBusy && !result.busy) { note('后台任务已结束，请查看各订阅和内核的实际状态。'); void refreshPool(); } }
      catch (error) { if (seq === sequence && epoch === getEpoch() && active() && !controller.signal.aborted) { state = null; note(failure(error), true); } }
      finally { if (seq === sequence && epoch === getEpoch()) { loading = false; reader = null; sync(); schedule(); } }
    }
    async function mutate(path, body, onSuccess) {
      if (!writable()) return; stop(); writing = true; const epoch = getEpoch(), owner = target, operation = ++writeSequence; sync(); note('操作已提交，正在等待服务器响应。');
      try { const result = await request(`network/${path}`, 'POST', { ...body, expected_controller_id: owner }); if (operation !== writeSequence || epoch !== getEpoch() || owner !== target || !isAuthenticated()) return; onSuccess?.(); if (active()) { render(result); note(path === 'sources/save' ? '订阅来源已保存。刷新后核对候选节点。' : path === 'sources/delete' ? '订阅来源已删除。' : '后台任务已受理，请查看状态变化。'); } }
      catch (error) { if (operation === writeSequence && epoch === getEpoch() && owner === target && active()) { note(failure(error), true); if ($('source-dialog').open) formNote(failure(error), true); } }
      finally { if (operation === writeSequence && epoch === getEpoch() && owner === target) { writing = false; sync(); schedule(); } }
    }
    function open(source) {
      if (!writable()) return; clearDraft(); draft = { id: source?.id || '', revision: state.source_revision, target };
      $('source-dialog-title').textContent = source ? '编辑订阅' : '添加订阅'; $('source-name').value = source?.name || ''; $('source-interval').value = String((source?.interval_ms || 21600000) / 60000);
      $('source-enabled').checked = source?.enabled !== false; $('source-auto').checked = source?.auto_apply === true;
      $('source-url').required = !source; $('source-url-hint').textContent = source ? '留空保留已保存的地址；填写后整体替换，提交即清空。' : '地址只保存到服务器，提交后立即清空。';
      sync(); $('source-dialog').showModal(); $('source-name').focus?.();
    }
    function openQuota(source) {
      const pool = getPool();
      if (!writable() || !Array.isArray(pool?.source_limits) || !pool?.release) return;
      const limits = pool.source_limits.map(item => ({ source_id: item.source_id, max_requests: item.max_requests, max_streams: item.max_streams }));
      const limit = limits.find(item => item.source_id === source.id);
      quotaDraft = { sourceID: source.id, revision: pool.revision, target, limits, mode: pool.mode, selected: pool.selected_id };
      $('source-quota-name').textContent = source.name || '订阅来源'; $('source-quota-enabled').checked = Boolean(limit);
      $('source-quota-requests').value = String(limit?.max_requests ?? 64); $('source-quota-streams').value = String(limit?.max_streams ?? 32);
      $('source-quota-notice').textContent = ''; sync(); $('source-quota-dialog').showModal();
    }
    $('source-quota-enabled').addEventListener('change', sync);
    $('source-quota-cancel').addEventListener('click', closeQuota);
    $('source-quota-dialog').addEventListener('close', () => { quotaDraft = null; });
    $('source-quota-dialog').addEventListener('cancel', event => { if (writing) event.preventDefault(); });
    $('source-quota-form').addEventListener('submit', async event => {
      event.preventDefault(); if (!writable() || !quotaDraft || quotaDraft.target !== target) return;
      const draft = quotaDraft, limited = $('source-quota-enabled').checked;
      const requests = Number($('source-quota-requests').value), streams = Number($('source-quota-streams').value);
      if (limited && (![requests, streams].every(value => Number.isInteger(value) && value >= 1 && value <= 4096))) { $('source-quota-notice').textContent = '请求与长连接额度需为 1–4096 的整数。'; return; }
      const limits = draft.limits.filter(item => item.source_id !== draft.sourceID);
      if (limited) limits.push({ source_id: draft.sourceID, max_requests: requests, max_streams: streams });
      const epoch = getEpoch(), owner = target, operation = ++writeSequence; stop(); writing = true; sync();
      try {
        await request('network/releases/prepare', 'POST', { expected_controller_id: owner, expected_revision: draft.revision, action: 'policy', payload: { mode: draft.mode, selected_id: draft.selected, source_limits: limits } });
        if (operation !== writeSequence || epoch !== getEpoch() || owner !== target || !isAuthenticated()) return;
        closeQuota(); if (active()) { await refreshPool(); if (operation === writeSequence && epoch === getEpoch() && owner === target && active()) note('共享额度检查已受理，尚未生效。请到配置发布区开始试用并确认保留。'); }
      } catch {
        if (operation === writeSequence && epoch === getEpoch() && owner === target && active()) {
          closeQuota(); note('共享额度未确认完成，请刷新线路状态后重新打开设置；不会自动重试提交。', true);
        }
      } finally { if (operation === writeSequence && epoch === getEpoch() && owner === target) { writing = false; sync(); schedule(); } }
    });
    $('sources-add').addEventListener('click', () => open(null)); $('sources-refresh').addEventListener('click', () => { void refresh(); });
    $('sources-kernel-install').addEventListener('click', () => { void mutate('kernel/install', {}); });
    $('source-cancel').addEventListener('click', closeDraft); $('source-dialog').addEventListener('close', clearDraft); $('source-dialog').addEventListener('cancel', event => { if (writing) event.preventDefault(); });
    $('source-form').addEventListener('submit', event => {
      event.preventDefault(); const url = $('source-url').value.trim(); $('source-url').value = '';
      if (!writable() || !draft || draft.target !== target) return;
      const name = $('source-name').value.trim(), interval = Number($('source-interval').value);
      if (!name || !Number.isFinite(interval) || interval < 5 || interval > 10080 || !Number.isSafeInteger(interval * 60000)) { formNote('请填写名称，更新间隔为 5–10080 分钟。', true); return; }
      if (url) { try { const u = new URL(url); if (u.protocol !== 'https:' || !u.hostname || u.username || u.password || u.hash || url.length > 8192) throw new Error(); } catch { formNote('请填写不含用户名、密码或片段的公网 HTTPS 订阅地址。', true); return; } }
      if (!url && !draft.id) { formNote('请填写订阅地址。', true); return; }
      void mutate('sources/save', { id: draft.id, expected_source_revision: draft.revision, name, url, enabled: $('source-enabled').checked, auto_apply: $('source-auto').checked, interval_ms: interval * 60000 }, closeDraft);
    });
    $('source-delete-cancel').addEventListener('click', () => { removeDraft = null; $('source-delete-dialog').close(); });
    $('source-delete-dialog').addEventListener('close', () => { removeDraft = null; }); $('source-delete-dialog').addEventListener('cancel', event => { if (writing) event.preventDefault(); });
    $('source-delete-form').addEventListener('submit', event => { event.preventDefault(); if (!removeDraft || removeDraft.target !== target) return; void mutate('sources/delete', { id: removeDraft.id, expected_source_revision: removeDraft.revision }, () => { removeDraft = null; $('source-delete-dialog').close(); }); });
    function setContext(pool, shown) {
      const nextTarget = pool?.controller_id || null, supported = Boolean(pool?.release) && pool?.configured === true, changed = target !== nextTarget;
      if (changed) { writeSequence++; stop(); closeDraft(); closeQuota(); if ($('source-delete-dialog').open) $('source-delete-dialog').close(); removeDraft = null; state = null; choices.clear(); writing = false; }
      const wasActive = active(); target = nextTarget; enabled = supported; visible = shown; $('network-sources-panel').hidden = !supported;
      if (!active()) { stop(); closeDraft(); closeQuota(); if ($('source-delete-dialog').open) $('source-delete-dialog').close(); }
      else if (changed || !wasActive || !state && !loading) void refresh();
      sync();
    }
    function reset() { writeSequence++; stop(); visible = false; enabled = false; state = null; target = null; writing = false; closeDraft(); closeQuota(); choices.clear(); buttons = []; quotaLabels.clear(); if ($('source-delete-dialog').open) $('source-delete-dialog').close(); $('sources-list').replaceChildren(); $('network-sources-panel').hidden = true; note(''); }
    document.addEventListener('visibilitychange', () => { if (!active()) { stop(); closeDraft(); closeQuota(); } else void refresh(); });
    return { setContext, reset, sync, refresh };
  };
})();
