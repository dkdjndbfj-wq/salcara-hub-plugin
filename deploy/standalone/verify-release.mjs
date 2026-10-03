// Offline, read-only verification of public release artifacts. No private key input.
import assert from 'node:assert/strict';
import { createHash, createPublicKey, verify } from 'node:crypto';
import { lstatSync, readFileSync } from 'node:fs';
import { resolve, join } from 'node:path';

const [artifactDirectory, expectedVersion, feedFile, identityFile] = process.argv.slice(2);
assert.ok(artifactDirectory && expectedVersion && feedFile && identityFile, 'artifact directory, version, public feed and public identity are required');
assert.match(expectedVersion, /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/);
function boundedFile(path, limit) {
  const info = lstatSync(path);
  assert.ok(info.isFile() && !info.isSymbolicLink() && info.size > 0 && info.size <= limit);
  return readFileSync(path);
}
const identity = JSON.parse(boundedFile(identityFile, 4096));
assert.equal(identity.algorithm, 'ed25519');
assert.equal(identity.key_id, 'salcara-local-20260930');
assert.equal(identity.public_key, '+JTC35ZD2PEJ5TO5ZOQtQTm5BoKDKMH59D6beZbn+gc=');
const envelope = JSON.parse(boundedFile(feedFile, 64 * 1024));
assert.equal(envelope.schema_version, 1);
assert.equal(envelope.publisher_key_id, identity.key_id);
const payload = Buffer.from(envelope.payload, 'base64');
const signature = Buffer.from(envelope.signature, 'base64');
assert.equal(payload.toString('base64'), envelope.payload);
assert.equal(signature.toString('base64'), envelope.signature);
assert.equal(signature.length, 64);
const publicKey = createPublicKey({
  key: Buffer.concat([Buffer.from('302a300506032b6570032100', 'hex'), Buffer.from(identity.public_key, 'base64')]),
  format: 'der', type: 'spki',
});
assert.ok(verify(null, payload, publicKey, signature), 'release signature must match the built-in publisher');
const feed = JSON.parse(payload);
assert.equal(feed.product, 'salcara-hub-standalone');
assert.equal(feed.version, expectedVersion);
assert.equal(feed.launcher_protocol, 1);
assert.equal(feed.data_schema, 1);
assert.deepEqual(Object.keys(feed.binaries).sort(), ['linux/amd64', 'linux/arm64']);
for (const architecture of ['amd64', 'arm64']) {
  const name = `salcara-hub_${expectedVersion}_linux_${architecture}`;
  const binary = boundedFile(join(resolve(artifactDirectory), name), 64 * 1024 * 1024);
  const entry = feed.binaries[`linux/${architecture}`];
  assert.equal(entry.url, `https://github.com/dkdjndbfj-wq/salcara-hub-plugin/releases/download/standalone-v${expectedVersion}/${name}`);
  assert.equal(entry.size_bytes, binary.length);
  assert.equal(entry.sha256, createHash('sha256').update(binary).digest('hex'));
}
console.log(`Verified standalone-v${expectedVersion}: publisher, signature, product, version and both exact release binaries.`);
