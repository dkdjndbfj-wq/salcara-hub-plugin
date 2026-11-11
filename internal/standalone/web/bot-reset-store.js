(() => {
  'use strict';
  // Recovery metadata only. Binding packages, invitations, phone credentials,
  // session cookies and CSRF tokens never cross this whitelist.
  const fields = ['groupId', 'expectedRevision', 'requestId', 'confirm', 'reason'];
  const bytes = value => new TextEncoder().encode(value).length;
  function payload(value) {
    if (!value || typeof value !== 'object' || Object.keys(value).length !== fields.length || !fields.every(field => Object.hasOwn(value, field)) ||
      typeof value.groupId !== 'string' || !value.groupId || value.groupId.trim() !== value.groupId || bytes(value.groupId) > 128 || /[\u0000-\u0020\u007f]/.test(value.groupId) ||
      typeof value.expectedRevision !== 'string' || !/^[0-9a-f]{32}$/.test(value.expectedRevision) || typeof value.requestId !== 'string' || !/^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/.test(value.requestId) || value.confirm !== true ||
      typeof value.reason !== 'string' || !value.reason.trim() || bytes(value.reason) > 256) throw new Error('浏览器中的重置恢复记录无效；请保留记录并联系管理员，不要发起另一轮重置。');
    return Object.freeze(Object.fromEntries(fields.map(field => [field, value[field]])));
  }
  window.SalcaraBotResetStore = ({ isOwner, getOwner }) => {
    function scope() {
      const owner = getOwner();
      if (!isOwner() || typeof owner !== 'string' || !/^[A-Za-z0-9_-]{1,64}$/.test(owner)) throw new Error('请以主管理员身份恢复手机重置。');
      return `salcara-bot-phone-reset-pending-v1:${window.location.origin}/salcara-hub:${owner}:`;
    }
    function key(prefix, value) {
      return `${prefix}${encodeURIComponent(value.groupId)}:${value.requestId}`;
    }
    function read(prefix) {
      const storage = window.localStorage, keys = [];
      for (let i = 0; i < storage.length; i++) {
        const item = storage.key(i);
        if (item?.startsWith(prefix)) keys.push(item);
      }
      if (keys.length > 256) throw new Error('待恢复的重置请求过多，请先处理已有请求。');
      const records = [];
      for (const item of keys.sort()) {
        const raw = storage.getItem(item);
        if (raw === null) continue; // Another page may have resolved this request.
        if (raw.length > 2048) throw new Error('重置恢复记录超过安全上限。');
        const record = payload(JSON.parse(raw));
        if (key(prefix, record) !== item) throw new Error('重置恢复记录与组或请求标识不一致。');
        records.push(record);
      }
      return records;
    }
    return {
      load: () => read(scope()),
      async prepare(value) {
        const prefix = scope(), candidate = payload(value), records = read(prefix);
        const existing = records.find(record => record.groupId === candidate.groupId);
        if (existing) return existing;
        if (records.length >= 256) throw new Error('待恢复的重置请求过多，请先处理已有请求。');
        // Each request owns its own key. Concurrent pages can never replace
        // another request's recovery data, even before the Hub's revision CAS.
        const item = key(prefix, candidate), serialized = JSON.stringify(candidate);
        window.localStorage.setItem(item, serialized);
        if (window.localStorage.getItem(item) !== serialized) throw new Error('浏览器未能保存重置恢复记录，本次请求尚未发送。');
        return candidate;
      },
      async remove(value) {
        const prefix = scope(), candidate = payload(value), item = key(prefix, candidate);
        window.localStorage.removeItem(item);
        if (window.localStorage.getItem(item) !== null) throw new Error('浏览器未能清除已结束的恢复记录。');
      },
    };
  };
})();
