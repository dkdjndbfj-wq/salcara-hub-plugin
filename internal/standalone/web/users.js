(() => {
  'use strict';
  window.SalcaraUsers = ({ request, getEpoch, isAuthenticated, getRole, isBusy, showDevices, refreshDevices }) => {
    const $ = id => document.getElementById(id);
    let users = [], revision = '', readSeq = 0, enrollmentSeq = 0, visible = '', busy = false, editing = null, assignment = null, station = null;
    const canEdit = () => isAuthenticated() && getRole() === 'owner';
    const node = (tag, value, cls) => { const n = document.createElement(tag); n.textContent = value; if (cls) n.className = cls; return n; };
    const button = (label, action, cls = 'secondary') => { const b = node('button', label, cls); b.type = 'button'; b.addEventListener('click', action); return b; };
    const notice = (id, message) => { $(id).textContent = message; };
    function selectUsers(id, first, activeOnly = false) {
      const select = $(id), current = select.value;
      select.replaceChildren();
      for (const [value, label] of first) { const opt = node('option', label); opt.value = value; select.append(opt); }
      for (const user of users) {
        if (activeOnly && user.disabled) continue;
        const opt = node('option', user.name + (user.disabled ? ' · 已禁用' : '')); opt.value = user.id; select.append(opt);
      }
      select.value = [...select.options].some(o => o.value === current) ? current : '';
    }
    function render() {
      $('users-count').textContent = users.length;
      $('users-active').textContent = users.filter(u => !u.disabled).length;
      $('users-disabled').textContent = users.filter(u => u.disabled).length;
      $('users-exceptions').textContent = users.filter(u => u.station).length;
      $('user-create').hidden = !canEdit();
      $('user-create').disabled = busy || !revision;
      const query = $('users-search').value.trim().toLocaleLowerCase(), filter = $('users-filter').value;
      const matches = users.filter(u => `${u.name} ${u.note || ''}`.toLocaleLowerCase().includes(query) && (!filter || filter === 'disabled' && u.disabled || filter === 'active' && !u.disabled || filter === 'custom' && u.station));
      const list = $('users-list'); list.replaceChildren();
      if (!matches.length) {
        const empty = node('div', '', 'users-empty');
        empty.append(node('strong', users.length ? '没有匹配的用户' : '先添加第一位用户'), node('p', users.length ? '试试其他名称或清除筛选条件。' : '创建用户后，为其生成登记码，或关联已有电脑。', 'muted'));
        if (users.length) empty.append(button('清除筛选', () => { $('users-search').value = ''; $('users-filter').value = ''; render(); }));
        list.append(empty);
      }
      for (const user of matches) {
        const row = node('article', '', 'user-row'), identity = node('div', '', 'user-identity'), title = node('div', '', 'user-title-line');
        const avatar = node('span', [...user.name].slice(0, 1).join(''), 'user-avatar'); avatar.setAttribute('aria-hidden', 'true');
        title.append(node('strong', user.name), node('span', user.disabled ? '已禁用' : '已启用', user.disabled ? 'pill bad' : 'pill ok'));
        const copy = node('div', '', 'user-copy'); copy.append(title, node('p', user.note || '暂无备注', 'muted small'));
        identity.append(avatar, copy);
        const activity = node('div', '', 'user-activity'); activity.append(node('span', `${user.devices} 台电脑 · ${user.online} 台在线`), node('small', `${user.paired} 台已配对 · ${user.station ? '单独授权' : '跟随全站'}`, 'muted'));
        const actions = node('div', '', 'actions'); actions.append(button('查看设备', () => showDevices(user.id)));
        if (canEdit()) actions.append(button('管理', () => edit(user), 'secondary'));
        row.append(identity, activity, actions); list.append(row);
      }
      selectUsers('device-user-filter', [['', '全部用户'], ['unassigned', '未分配用户']]);
      selectUsers('enrollment-user', [['', '暂不分配用户']], true);
      if (!assignment) selectUsers('device-user-select', [['', '未分配用户']]);
    }
    async function refresh() {
      if (!isAuthenticated()) return;
      const epoch = getEpoch(), seq = ++readSeq;
      notice('users-notice', '正在读取用户…');
      try {
        const value = await request('users');
        if (epoch !== getEpoch() || seq !== readSeq) return;
        users = Array.isArray(value.users) ? value.users : []; revision = value.revision || '';
        render(); notice('users-notice', getRole() === 'owner' ? '' : '当前为查看权限；用户和单独授权由主管理员管理。');
      } catch (error) { if (epoch === getEpoch() && seq === readSeq) { revision = ''; render(); notice('users-notice', error.message); } }
    }
    function fillPolicy(policy) {
      const p = policy || {};
      $('user-api-bases').value = (p.apiBases || []).join('\n');
      $('user-external-bases').value = (p.externalApiBases || []).join('\n');
      $('user-proxy-bases').value = (p.reverseProxyApiBases || (p.version === 1 ? p.apiBases : []) || []).join('\n');
      $('user-official').checked = !!p.allowOfficialAccounts;
    }
    function syncPolicy() {
      $('user-policy-fields').hidden = $('user-policy-mode').value !== 'custom';
      $('user-status').textContent = $('user-disabled').value === 'true' ? '已禁用' : '已启用';
      $('user-status').className = $('user-disabled').value === 'true' ? 'pill bad' : 'pill ok';
    }
    function edit(user) {
      if (!canEdit() || busy || isBusy() || !revision) return;
      editing = { id: user?.id || '', revision };
      $('user-title').textContent = user ? `管理 ${user.name}` : '添加用户';
      $('user-device-summary').textContent = user ? `${user.devices} 台电脑 · ${user.paired} 台已配对` : '创建后关联电脑，手机无需注册。';
      $('user-name').value = user?.name || ''; $('user-note').value = user?.note || '';
      $('user-disabled').value = user?.disabled ? 'true' : 'false';
      $('user-policy-mode').value = user?.station ? 'custom' : 'inherit';
      fillPolicy(user?.station || station); syncPolicy(); $('user-reason').value = ''; notice('user-form-notice', '');
      $('user-dialog').showModal();
    }
    function setBusy(value) {
      busy = value;
      for (const id of ['user-save','user-cancel','device-user-save','device-user-cancel','user-create']) $(id).disabled = value;
      for (const id of ['user-name','user-note','user-disabled','user-policy-mode','user-api-bases','user-external-bases','user-proxy-bases','user-official','user-reason','device-user-select','device-user-reason']) $(id).disabled = value;
    }
    async function save(incoming, dialog, noticeID) {
      if (!canEdit() || busy || isBusy()) return;
      const epoch = getEpoch(); setBusy(true); notice(noticeID, '正在保存…');
      try {
        const value = await request('users', 'POST', { ...incoming, confirm: true });
        if (epoch !== getEpoch()) return;
        readSeq++; users = value.users || []; revision = value.revision || ''; render();
        $(dialog).close(); notice('users-notice', '用户设置已保存。');
        await refreshDevices();
      } catch (error) { if (epoch === getEpoch()) notice(noticeID, error.message); }
      finally { if (epoch === getEpoch()) setBusy(false); }
    }
    $('user-create').addEventListener('click', () => edit(null));
    $('user-policy-mode').addEventListener('change', syncPolicy); $('user-disabled').addEventListener('change', syncPolicy);
    $('user-form').addEventListener('submit', event => {
      event.preventDefault(); if (!editing) return;
      const lines = id => $(id).value.split(/\r?\n/).map(v => v.trim()).filter(Boolean);
      const policy = $('user-policy-mode').value === 'custom' ? { version: 2, apiBases: lines('user-api-bases'), externalApiBases: lines('user-external-bases'), reverseProxyApiBases: lines('user-proxy-bases'), allowOfficialAccounts: $('user-official').checked } : null;
      void save({ action: 'save', expected_revision: editing.revision, id: editing.id, name: $('user-name').value.trim(), note: $('user-note').value.trim(), disabled: $('user-disabled').value === 'true', station: policy, reason: $('user-reason').value.trim() }, 'user-dialog', 'user-form-notice');
    });
    $('user-cancel').addEventListener('click', () => { if (!busy) $('user-dialog').close(); });
    $('user-dialog').addEventListener('close', () => { editing = null; });
    $('user-dialog').addEventListener('cancel', event => { if (busy) event.preventDefault(); });
    $('users-filter-form').addEventListener('submit', event => { event.preventDefault(); render(); });
    $('users-search').addEventListener('input', render); $('users-filter').addEventListener('change', render);
    async function assign(device) {
      if (!canEdit() || busy || isBusy()) return;
      const epoch = getEpoch(); await refresh(); if (epoch !== getEpoch() || !revision) return;
      selectUsers('device-user-select', [['', '未分配用户']]);
      assignment = { ref: device.ref, revision };
      $('device-user-target').textContent = `${device.name || '未命名电脑'} · ${device.user_name || '未分配用户'}`;
      $('device-user-select').value = device.user_id || ''; $('device-user-reason').value = ''; notice('device-user-notice', '');
      $('device-user-dialog').showModal();
    }
    $('device-user-form').addEventListener('submit', event => { event.preventDefault(); if (!assignment) return;
      void save({ action: 'assign', expected_revision: assignment.revision, id: $('device-user-select').value, device_ref: assignment.ref, reason: $('device-user-reason').value.trim() }, 'device-user-dialog', 'device-user-notice');
    });
    $('device-user-cancel').addEventListener('click', () => { if (!busy) $('device-user-dialog').close(); });
    $('device-user-dialog').addEventListener('close', () => { assignment = null; });
    $('device-user-dialog').addEventListener('cancel', event => { if (busy) event.preventDefault(); });
    async function refreshEnrollments() {
      if (!canEdit() || !$('enrollment-history').open) return;
      const epoch = getEpoch(), seq = ++enrollmentSeq; $('enrollment-list').textContent = '正在读取…';
      try {
        const value = await request('enrollment-claims');
        if (epoch !== getEpoch() || seq !== enrollmentSeq) return;
        const list = $('enrollment-list'); list.replaceChildren();
        for (const claim of value.claims || []) {
          const row = node('div', '', 'enrollment-row'), info = node('div', '');
          info.append(node('strong', claim.user_name), node('p', `${claim.usable ? '待使用' : '已到期或用户已禁用'} · ${new Date(claim.expires_at).toLocaleString()} 到期`, 'muted small'));
          const revoke = button('撤销', async () => {
            if (busy || !canEdit() || !confirm(`撤销“${claim.user_name}”的这条未使用登记码？`)) return;
            const generation = getEpoch(); revoke.disabled = true; setBusy(true);
            try { await request('enrollment-claims', 'POST', { revoke: claim.ref, confirm: true }); if (generation === getEpoch()) await refreshEnrollments(); }
            catch (error) { if (generation === getEpoch()) { let message = row.querySelector('[role="status"]'); if (!message) { message = node('p', '', 'notice'); message.setAttribute('role', 'status'); info.append(message); } message.textContent = error.message; } }
            finally { if (generation === getEpoch()) { setBusy(false); revoke.disabled = false; } }
          }, 'secondary text-danger');
          row.append(info, revoke); list.append(row);
        }
        if (!list.children.length) list.append(node('p', '没有尚未使用的登记码。', 'muted'));
      } catch (error) { if (epoch === getEpoch() && seq === enrollmentSeq) $('enrollment-list').textContent = error.message; }
    }
    $('enrollment-history').addEventListener('toggle', () => { void refreshEnrollments(); });
    return {
      refresh, assign, refreshEnrollments,
      setStation: value => { station = value; },
      setVisible(page) { if (page === visible) return; visible = page; if (page === 'users' || page === 'devices') void refresh(); if (page === 'devices') void refreshEnrollments(); },
      reset() { readSeq++; enrollmentSeq++; users = []; revision = ''; station = null; visible = ''; editing = null; assignment = null; setBusy(false);
        for (const id of ['user-dialog','device-user-dialog']) if ($(id).open) $(id).close();
        for (const id of ['user-name','user-note','user-reason','user-api-bases','user-external-bases','user-proxy-bases','device-user-reason','users-search']) $(id).value = '';
        for (const id of ['user-title','device-user-target','user-device-summary','user-form-notice','device-user-notice','users-notice']) $(id).textContent = '';
        $('user-official').checked = false; $('user-disabled').value = 'false'; $('user-policy-mode').value = 'inherit'; syncPolicy();
        $('users-filter').value = ''; $('enrollment-list').replaceChildren(); render();
      },
    };
  };
})();
