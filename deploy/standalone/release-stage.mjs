// Public metadata only. CI never receives a publisher private key.
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { lstatSync, readFileSync, writeFileSync } from 'node:fs';
import { resolve, join } from 'node:path';

const [artifactDirectory, version, repository = 'dkdjndbfj-wq/salcara-hub-plugin'] = process.argv.slice(2);
assert.ok(artifactDirectory, 'artifact directory is required');
assert.match(version || '', /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/);
assert.ok(version.split('.').every(part => BigInt(part) <= 0xffffffffn), 'version component exceeds the launcher limit');
assert.equal(repository, 'dkdjndbfj-wq/salcara-hub-plugin', 'standalone publishing repository is fixed');
const directory = resolve(artifactDirectory);
const binaries = {};
const checksums = [];
for (const architecture of ['amd64', 'arm64']) {
  for (const program of ['salcara-hub', 'salcara-hub-launcher']) {
    const path = join(directory, `linux-${architecture}`, program);
    const stat = lstatSync(path);
    assert.ok(stat.isFile() && !stat.isSymbolicLink() && stat.size > 0 && stat.size <= 64 * 1024 * 1024);
    const data = readFileSync(path);
    assert.equal(data.subarray(0, 4).toString('hex'), '7f454c46', 'release binary must be Linux ELF');
    assert.ok(data.length >= 20 && data[4] === 2 && data[5] === 1, 'release binary must be ELF64 little-endian');
    assert.equal(data.readUInt16LE(18), architecture === 'amd64' ? 62 : 183, 'binary architecture must match the signed platform');
    const name = `${program}_${version}_linux_${architecture}`;
    writeFileSync(join(directory, name), data, { flag: 'wx', mode: 0o755 });
    const digest = createHash('sha256').update(data).digest('hex');
    checksums.push(`${digest}  ${name}`);
    if (program === 'salcara-hub') {
      binaries[`linux/${architecture}`] = {
        url: `https://github.com/${repository}/releases/download/standalone-v${version}/${name}`,
        sha256: digest,
        size_bytes: data.length,
      };
    }
  }
}
writeFileSync(join(directory, 'SHA256SUMS'), `${checksums.join('\n')}\n`, { flag: 'wx' });
writeFileSync(join(directory, 'standalone-payload.json'), `${JSON.stringify({
  product: 'salcara-hub-standalone', version, launcher_protocol: 1, data_schema: 1,
  release_notes: `Standalone Hub ${version}: independent Docker deployment and signed application updates.`, binaries,
}, null, 2)}\n`, { flag: 'wx' });
console.log(`Prepared public standalone-v${version} binaries, hashes and unsigned payload; no private key used.`);
