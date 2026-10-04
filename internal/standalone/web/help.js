(() => {
  'use strict';
  const $ = id => document.getElementById(id);
  $('hub-url').textContent = new URL('./', location.href).href;
  $('check').addEventListener('click', async () => {
    $('check').disabled = true;
    const controller = new AbortController(), timer = setTimeout(() => controller.abort(), 10000);
    try {
      const response = await fetch('./v1/ping', { credentials: 'omit', cache: 'no-store', redirect: 'error', signal: controller.signal, referrerPolicy: 'no-referrer' });
      const data = await response.json();
      if (!response.ok || data.service !== 'salcara-hub' || data.protocol !== 'salcara-remote' || data.protocolVersion !== 1) throw new Error('地址没有返回兼容的远程通道。');
      $('health').textContent = 'Hub 可连接'; $('health').className = 'pill ok';
      $('status').textContent = `本站 Hub v${data.version} 可连接。电脑在线与桌面授权情况请在手机配对后查看。`;
    } catch (error) { $('health').textContent = '连接失败'; $('health').className = 'pill bad'; $('status').textContent = error.name === 'AbortError' ? '请求超时，请检查网络或稍后重试。' : error.message; }
    finally { clearTimeout(timer); $('check').disabled = false; }
  });
})();
