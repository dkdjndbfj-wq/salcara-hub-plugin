import assert from 'node:assert/strict';
import { readFile } from 'node:fs/promises';
import test from 'node:test';
import vm from 'node:vm';

const root = new URL('../../internal/standalone/web/', import.meta.url);
const source = await readFile(new URL('public-sources.js', root), 'utf8');
const ids = ['api', 'external_api', 'reverse_proxy', 'official_account'];
function browser() {
  const elements = new Map(['source-enabled-count', 'sources-refresh', ...ids.flatMap(id => [`source-${id}-status`, `source-${id}-detail`])].map(id => [id, { textContent: '', className: '' }]));
  const context = vm.createContext({ document: { getElementById: id => elements.get(id) || null } });
  vm.runInContext(source, context);
  return { ...context.SalcaraPublicSources, $: id => elements.get(id), all: () => [...elements.values()].map(el => el.textContent).join(' ') };
}

test('public source panel separates authorization readiness, missing configuration and disabled categories', () => {
  const b = browser();
  b.render(b.read(ids.map((id, i) => ({ id, status: ['disabled', 'allowlist', 'issuer_missing', 'computer_verification'][i] }))));
  assert.equal(b.$('source-api-status').textContent, '未开放');
  assert.equal(b.$('source-external_api-status').textContent, '按名单开放');
  assert.equal(b.$('source-reverse_proxy-status').textContent, '待完善授权');
  assert.equal(b.$('source-official_account-status').textContent, '电脑验证');
  assert.equal(b.$('source-enabled-count').textContent, '3 / 4');
  assert.doesNotMatch(b.all(), /在线|已连通|额度充足/);
  b.render(b.read(ids.map(id => ({ id, status: 'issuer_configured' }))));
  assert.equal(b.$('source-external_api-status').textContent, '授权就绪');
  assert.equal(b.$('source-enabled-count').textContent, '4 / 4');
  b.render(b.read(ids.map(id => ({ id, status: 'paused' }))));
  assert.equal(b.$('source-enabled-count').textContent, '0 / 4');
  assert.equal(b.$('source-reverse_proxy-status').textContent, '已暂停');
});

test('missing, duplicate, oversized and unknown source responses never imply permission or display response content', () => {
  for (const value of [null, {}, [], [{ id: 'external_api', status: '<img onerror=private>' }], [{ id: 'api', status: 'allowlist' }, { id: 'api', status: 'disabled' }], Array(5).fill({ id: 'api', status: 'allowlist' }), [{ id: 'api', status: 'toString' }]]) {
    const b = browser(); b.render(b.read(value));
    assert.equal(b.$('source-enabled-count').textContent, '—');
    assert.equal(b.$('source-api-status').textContent, '暂未确认');
    assert.doesNotMatch(b.all(), /onerror|private|toString/);
  }
});

test('pending, failed and cached checks have distinct freshness and retain no false current access', () => {
  const b = browser(), snapshot = b.read(ids.map(id => ({ id, status: 'allowlist' })));
  b.render(snapshot);
  assert.equal(b.$('sources-refresh').textContent, '本站当前授权');
  b.render(null, 'pending');
  assert.equal(b.$('source-api-status').textContent, '确认中');
  assert.equal(b.$('source-enabled-count').textContent, '—');
  b.render(null, 'stale');
  assert.equal(b.$('source-api-status').textContent, '待重新确认');
  b.render(snapshot, 'stale', true);
  assert.equal(b.$('sources-refresh').textContent, '上次检测授权');
});
