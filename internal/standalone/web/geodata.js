(() => {
  'use strict';
  window.SalcaraGeodata = ({ request, getEpoch, isAuthenticated, isBusy, refreshPool }) => {
    const $ = id => document.getElementById(id);
    let pool = null, visible = false, state = null, loading = false, writing = false, sequence = 0, controller = null, timer = null, budget = 0;
    const active = () => visible && isAuthenticated() && document.visibilityState !== 'hidden' && pool?.geo_available === true;
    const text = (tag, value, cls) => { const el = document.createElement(tag); el.textContent = value; if (cls) el.className = cls; return el; };
    const time = value => Number(value) > 0 ? new Date(value).toLocaleString() : '尚未安排';
    const errors = { geo_download_failed: '数据库下载失败，上一版本继续保留', geo_database_invalid: '数据库校验失败，上一版本继续保留', geo_database_expired: '数据库已过期，请更新', geo_storage_failed: '数据库未能写入，请检查入口磁盘', geo_state_invalid: '本地状态损坏，请重新安装数据库', geo_observation_interrupted: '本轮观察中断，保留上轮结果', geo_install_interrupted: '安装中断，保留上一版本' };
    function stop() { sequence++; controller?.abort(); controller = null; loading = false; clearTimeout(timer); timer = null; }
    function sync() {
      const blocked = !active() || !state || loading || writing || state.run?.running || isBusy();
      $('geo-refresh').disabled = !active() || loading || writing;
      $('geo-install').disabled = blocked;
      $('geo-observe').disabled = blocked || !state?.enabled || !state?.installed || state?.expired === true;
      $('geo-enabled').disabled = blocked || !state?.installed;
      $('geo-auto-update').disabled = blocked || !state?.installed;
      $('geo-install').textContent = state?.installed ? '更新本地数据库' : '安装并开启';
    }
    function render(value) {
      state = value;
      $('geo-status').textContent = value.run?.running ? '任务进行中' : value.expired ? '数据已过期' : value.enabled && value.installed ? '本地观察已开启' : value.installed ? '观察已暂停' : '尚未安装';
      $('geo-status').className = 'pill' + (value.enabled && value.installed && !value.expired ? ' ok' : '');
      $('geo-enabled').checked = value.enabled === true; $('geo-auto-update').checked = value.auto_update === true;
      $('geo-version').textContent = value.installed ? `${value.month} · 构建于 ${time(value.build_at)}` : '按需从 DB-IP 官方下载国家与 ASN 数据库';
      $('geo-schedule').textContent = `每 6 小时观察一次，结果最多保留 24 小时。下次观察：${value.enabled ? time(value.next_observe_at) : '已暂停'}。${value.auto_update && (value.installed || value.update_pending) ? `自动检查月度数据库更新；失败 1 小时后重试。下次检查：${time(value.next_update_at)}。` : ''}`;
      $('geo-progress').textContent = value.run?.running ? `${value.run.kind === 'install' ? '校验并安装数据库' : '解析节点服务器地址'} · ${value.run.completed} / ${value.run.total || '准备中'}` : value.last_error ? errors[value.last_error] || '任务未能完成，请重试或检查入口日志' : '地区归属来自本地数据，不向查询服务发送用户 IP。';
      const list = $('geo-groups'); list.replaceChildren();
      const names = new Map((pool?.nodes || []).map(n => [n.id, n.name]));
      for (const group of value.groups || []) {
        const row = text('div', '', 'network-info-row');
        row.append(text('strong', `AS${group.asn} · ${group.organization || '名称未知'}`), text('span', `${group.country || '跨地区 / 未确认'} · ${(group.node_ids || []).map(id => names.get(id) || '节点已移除').join('、')}`, 'muted small')); list.append(row);
      }
      if (!(value.groups || []).length) list.append(text('p', '观察完成后可在这里查看共享 ASN 的节点。', 'muted small'));
      sync();
    }
    function schedule() {
      clearTimeout(timer); timer = null;
      if (active() && state?.run?.running && budget-- > 0) timer = setTimeout(() => { timer = null; void refresh(); }, 2000);
    }
    async function refresh() {
      if (!active() || loading || writing) return;
      stop(); const seq = sequence, epoch = getEpoch(), target = pool.controller_id;
      loading = true; controller = new AbortController(); sync();
      try {
        const value = await request('network/geodata/state', 'GET', undefined, false, controller.signal);
        if (seq !== sequence || epoch !== getEpoch() || !active() || target !== pool?.controller_id) return;
        if (value.controller_id !== target || !Number.isSafeInteger(value.revision)) throw new Error('network scope mismatch');
        const completed = state?.run?.running && !value.run?.running;
        render(value);
        if (completed) void refreshPool();
      } catch {
        if (seq === sequence && epoch === getEpoch() && active()) { state = null; $('geo-progress').textContent = '状态读取失败，操作已锁定；请重新读取。'; }
      } finally { if (seq === sequence) { loading = false; controller = null; sync(); schedule(); } }
    }
    async function mutate(operation, extra = {}) {
      if (!active() || !state || loading || writing || state.run?.running || isBusy()) return;
      const revision = state.revision, target = pool.controller_id, epoch = getEpoch();
      stop(); const seq = sequence; writing = true; sync();
      try {
        const value = await request('network/geodata/' + operation, 'POST', { expected_revision: revision, expected_controller_id: target, ...extra });
        if (seq !== sequence || epoch !== getEpoch() || target !== pool?.controller_id || !active()) return;
        if (value.controller_id !== target) throw new Error('scope mismatch');
        render(value); budget = 150;
        if (!value.run?.running) void refreshPool();
      } catch (error) {
        if (seq === sequence && epoch === getEpoch() && active()) {
          const dependency = '当前严格地区规则依赖网络归属，请先在策略中取消使用观察地区，再暂停观察';
          state = null; $('geo-progress').textContent = error.message === dependency ? dependency + '。请重新读取状态后操作。' : '未确认操作结果，请重新读取状态后再操作。';
        }
      } finally { if (seq === sequence) { writing = false; sync(); schedule(); } }
    }
    $('geo-refresh').addEventListener('click', () => { budget = 150; void refresh(); });
    $('geo-install').addEventListener('click', () => void mutate('install'));
    $('geo-observe').addEventListener('click', () => void mutate('observe'));
    for (const id of ['geo-enabled', 'geo-auto-update']) $(id).addEventListener('change', () => void mutate('configure', { enabled: $('geo-enabled').checked, auto_update: $('geo-auto-update').checked }));
    function setContext(next, show) {
      const changed = pool?.controller_id !== next?.controller_id, opening = show && !visible;
      pool = next; visible = show; $('network-geodata-panel').hidden = next?.geo_available !== true;
      if (changed || !active()) {
        stop(); state = null; writing = false; $('geo-groups').replaceChildren();
        $('geo-status').textContent = '尚未读取'; $('geo-status').className = 'pill'; $('geo-version').textContent = ''; $('geo-schedule').textContent = '';
        $('geo-enabled').checked = false; $('geo-auto-update').checked = false; $('geo-progress').textContent = '等待读取当前入口的网络归属设置。';
      }
      if (active() && (changed || opening)) { budget = 150; void refresh(); }
      sync();
    }
    document.addEventListener('visibilitychange', () => { if (!active()) { stop(); writing = false; state = null; } else { budget = 150; void refresh(); } });
    return { setContext, reset: () => setContext(null, false) };
  };
})();
