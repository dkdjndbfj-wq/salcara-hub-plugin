import assert from 'node:assert/strict';
import test from 'node:test';
import { createHash } from 'node:crypto';
import { spawnSync } from 'node:child_process';
import { mkdtempSync, mkdirSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { fileURLToPath } from 'node:url';
import { join } from 'node:path';

const stage = fileURLToPath(new URL('./release-stage.mjs', import.meta.url));
function fixture(t, wrongArch = false) {
  const dir = mkdtempSync(join(tmpdir(), 'salcara-standalone-release-'));
  t.after(() => rmSync(dir, { recursive: true, force: true }));
  for (const arch of ['amd64', 'arm64']) {
    mkdirSync(join(dir, `linux-${arch}`));
    for (const name of ['salcara-hub', 'salcara-hub-launcher']) {
      const data = Buffer.alloc(64);
      Buffer.from('7f454c46', 'hex').copy(data);
      data[4] = 2; data[5] = 1;
      data.writeUInt16LE(arch === 'amd64' || wrongArch ? 62 : 183, 18);
      writeFileSync(join(dir, `linux-${arch}`, name), data);
    }
  }
  return dir;
}
function run(dir, version = '0.4.0', repo) {
  return spawnSync(process.execPath, [stage, dir, version, ...(repo ? [repo] : [])], { encoding: 'utf8' });
}
test('release staging binds exact public binaries to standalone tag and product', t => {
  const dir = fixture(t);
  assert.equal(run(dir).status, 0);
  const feed = JSON.parse(readFileSync(join(dir, 'standalone-payload.json')));
  assert.equal(feed.product, 'salcara-hub-standalone');
  assert.equal(feed.version, '0.4.0');
  assert.deepEqual(Object.keys(feed.binaries).sort(), ['linux/amd64', 'linux/arm64']);
  for (const arch of ['amd64', 'arm64']) {
    const data = readFileSync(join(dir, `salcara-hub_0.4.0_linux_${arch}`));
    assert.equal(feed.binaries[`linux/${arch}`].sha256, createHash('sha256').update(data).digest('hex'));
    assert.equal(feed.binaries[`linux/${arch}`].size_bytes, data.length);
    assert.match(feed.binaries[`linux/${arch}`].url, /\/standalone-v0\.4\.0\/salcara-hub_0\.4\.0_linux_/);
  }
  assert.notEqual(run(dir).status, 0, 'does not overwrite existing release outputs');
});
test('release staging rejects cross-labelled binaries', t => {
  assert.notEqual(run(fixture(t, true)).status, 0);
});
test('release staging rejects invalid identities and versions before writing', t => {
  for (const version of ['v0.4.0', '0.4.0/../../bad', '01.4.0', '4294967296.4.0']) {
    assert.notEqual(run(fixture(t), version).status, 0);
  }
  assert.notEqual(run(fixture(t), '0.4.0', 'dkdjndbfj-wq/salcara-personal-hub').status, 0);
});
test('release configuration preserves the Hub product and public update identity', () => {
  const workflow = readFileSync(new URL('../../.github/workflows/standalone-release.yml', import.meta.url), 'utf8');
  assert.ok(workflow.includes('ghcr.io/dkdjndbfj-wq/salcara-hub-standalone:'));
  assert.ok(!workflow.includes('publisher.private'));
  assert.ok(!workflow.includes('secrets.SALCARA'));
  assert.ok(!workflow.includes('gh release create'));
  const source = readFileSync(new URL('../../internal/launcher/config.go', import.meta.url), 'utf8');
  assert.ok(source.includes('main/updates/standalone-account.json'));
  assert.ok(source.includes('Product          = "salcara-hub-standalone"'));
});

test('public verifier fails closed on mismatched publisher without a private key', t => {
  const dir = fixture(t);
  const identityPath = join(dir, 'public.json');
  const feedPath = join(dir, 'feed.json');
  writeFileSync(identityPath, JSON.stringify({ algorithm: 'ed25519', key_id: 'another-publisher', public_key: Buffer.alloc(32).toString('base64') }));
  writeFileSync(feedPath, '{}');
  const result = spawnSync(process.execPath, [fileURLToPath(new URL('./verify-release.mjs', import.meta.url)), dir, '0.4.0', feedPath, identityPath], { encoding: 'utf8' });
  assert.notEqual(result.status, 0);
});

test('public verifier refuses an unsigned envelope for the real public identity', t => {
  const dir = fixture(t);
  const identityPath = join(dir, 'public.json');
  const feedPath = join(dir, 'feed.json');
  writeFileSync(identityPath, readFileSync(new URL('../../publisher/public.json', import.meta.url)));
  writeFileSync(feedPath, JSON.stringify({ schema_version: 1, publisher_key_id: 'salcara-local-20260930', payload: Buffer.from('{}').toString('base64'), signature: Buffer.alloc(64).toString('base64') }));
  const result = spawnSync(process.execPath, [fileURLToPath(new URL('./verify-release.mjs', import.meta.url)), dir, '0.4.0', feedPath, identityPath], { encoding: 'utf8' });
  assert.notEqual(result.status, 0);
});
