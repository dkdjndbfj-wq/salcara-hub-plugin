// Local browser QA only: refuses every server except the dedicated test port
// authenticated with the repository's NON-PRODUCTION preview fixture token.
import assert from 'node:assert/strict';
import { randomBytes } from 'node:crypto';
const base = 'http://127.0.0.1:19877/salcara-hub';
const admin = 'salcara-preview-only-not-for-real-deployment-0000000000000000000000';
const headers = { Authorization: `Bearer ${admin}` };
const before = await fetch(`${base}/_admin/v1/state`, { headers });
assert.equal(before.status, 200, 'refusing to seed any non-preview Hub');
const abort = new AbortController();
for (const [id, name, os] of [['salcara-preview-pc', '演示 · 办公室电脑', 'Windows'], ['salcara-preview-mac', '演示 · 移动工作站', 'macOS']]) {
  const secret = randomBytes(32).toString('hex');
  const deviceHeaders = { 'X-Salcara-Device-Id': id, 'X-Salcara-Device-Secret': secret, 'Content-Type': 'application/json' };
  const response = await fetch(`${base}/v1/device/register`, { method:'POST', headers:deviceHeaders, body:JSON.stringify({ deviceId:id, name, os, version:'QA fixture', projects:[], tools:[{id:'codex',name:'Codex · 演示上报',available:true,version:'测试数据'}] }) });
  assert.equal(response.status, 200, 'use a fresh preview data directory; do not change a real device secret');
  const stream = await fetch(`${base}/v1/bridge/stream?deviceId=${id}`, { headers:deviceHeaders, signal:abort.signal });
  assert.equal(stream.status,200);
  (async () => { try { for await (const chunk of stream.body) void chunk; } catch { /* local fixture closed */ } })();
}
const snapshot = await (await fetch(`${base}/_admin/v1/state`, { headers })).json();
assert.equal(snapshot.stats.online_devices, 2);
console.log('Local non-production browser fixture ready: two simulated desktop connections, no models called.');
const stop = () => { abort.abort(); process.exit(0); };
process.on('SIGINT',stop); process.on('SIGTERM',stop); setTimeout(stop,15*60*1000);
