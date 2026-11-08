(() => {
  'use strict';
  const ids = ['api', 'external_api', 'reverse_proxy', 'official_account'];
  const states = {
    disabled: ['未开放', '本站未开放此类来源', 'muted'],
    paused: ['已暂停', '远程执行已暂停', 'warning'],
    allowlist: ['按名单开放', '电脑端复核来源', 'ok'],
    computer_verification: ['电脑验证', '本地官方账号授权', 'info'],
    issuer_configured: ['授权就绪', '来源签名配置齐全', 'ok'],
    issuer_missing: ['待完善授权', '来源签名尚未就绪', 'warning'],
    unknown: ['暂未确认', '当前未提供此项状态', 'muted'],
    pending: ['确认中', '正在读取本站设置', 'muted'],
    stale: ['待重新确认', '等待下次成功检测', 'muted'],
  };
  function read(value) {
    const input = Array.isArray(value) && value.length <= ids.length ? value : [];
    return ids.map(id => {
      const matches = input.filter(item => item && item.id === id);
      const state = matches.length === 1 && typeof matches[0].status === 'string' ? matches[0].status : 'unknown';
      return Object.hasOwn(states, state) && !['pending', 'stale'].includes(state) ? state : 'unknown';
    });
  }
  function render(snapshot, fallback = 'unknown', cached = false) {
    let open = 0, known = 0;
    ids.forEach((id, index) => {
      const state = snapshot?.[index] || fallback;
      const [label, detail, color] = states[state] || states.unknown;
      const badge = document.getElementById(`source-${id}-status`);
      badge.textContent = label; badge.className = `source-badge ${color}`;
      document.getElementById(`source-${id}-detail`).textContent = detail;
      if (['allowlist', 'computer_verification', 'issuer_configured', 'issuer_missing'].includes(state)) open++;
      if (!['unknown', 'pending', 'stale'].includes(state)) known++;
    });
    const count = document.getElementById('source-enabled-count');
    if (count) count.textContent = known === ids.length ? `${open} / ${ids.length}` : '—';
    document.getElementById('sources-refresh').textContent = snapshot && known ? (cached ? '上次检测授权' : '本站当前授权') : fallback === 'pending' ? '同步中' : '待同步';
  }
  globalThis.SalcaraPublicSources = Object.freeze({ read, render });
})();
