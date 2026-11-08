// Release-only OCI verification. No operator data, signing key, or model API.
import assert from 'node:assert/strict';
import { createHash, randomBytes } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { createReadStream, lstatSync, mkdirSync, mkdtempSync, readFileSync, writeFileSync, appendFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { resolve, join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

export const imageRepository = 'ghcr.io/dkdjndbfj-wq/salcara-hub-standalone';
export const architectures = ['amd64', 'arm64'];
const indexType = 'application/vnd.oci.image.index.v1+json';
const manifestType = 'application/vnd.oci.image.manifest.v1+json';
const configType = 'application/vnd.oci.image.config.v1+json';
const digestPattern = /^sha256:[a-f0-9]{64}$/;
const provenanceTypes = new Set(['https://slsa.dev/provenance/v0.2', 'https://slsa.dev/provenance/v1']);
const sbomType = 'https://spdx.dev/Document';

export function validateIdentity(version, sourceSHA) {
  assert.match(version || '', /^(0|[1-9]\d*)\.(0|[1-9]\d*)\.(0|[1-9]\d*)$/);
  assert.ok(version.split('.').every(part => BigInt(part) <= 0xffffffffn), 'version exceeds launcher limits');
  assert.match(sourceSHA || '', /^[a-f0-9]{40}$/);
}

function regularFile(path, maxBytes) {
  const stat = lstatSync(path);
  assert.ok(stat.isFile() && !stat.isSymbolicLink() && stat.size > 0 && stat.size <= maxBytes, 'invalid release file');
  return stat;
}
function jsonFile(path) {
  regularFile(path, 32 << 20);
  return JSON.parse(readFileSync(path, 'utf8'));
}
export async function fileDigest(path, maxBytes = 1024 << 20) {
  regularFile(path, maxBytes);
  const hash = createHash('sha256');
  let bytes = 0;
  for await (const chunk of createReadStream(path)) {
    bytes += chunk.length;
    assert.ok(bytes <= maxBytes, 'release file grew beyond its limit');
    hash.update(chunk);
  }
  return { digest: `sha256:${hash.digest('hex')}`, size: bytes };
}

export async function verifyLayout(directory, version, expectedIndexDigest) {
  assert.match(expectedIndexDigest || '', digestPattern);
  const root = resolve(directory);
  for (const path of [root, join(root, 'blobs'), join(root, 'blobs', 'sha256')]) {
    const stat = lstatSync(path);
    assert.ok(stat.isDirectory() && !stat.isSymbolicLink(), 'OCI directories cannot be links');
  }
  assert.equal(jsonFile(join(root, 'oci-layout')).imageLayoutVersion, '1.0.0');
  const wrapper = jsonFile(join(root, 'index.json'));
  assert.equal(wrapper.schemaVersion, 2);
  assert.equal(wrapper.manifests?.length, 1, 'one immutable multiarch image is required');
  const image = wrapper.manifests[0];
  assert.equal(image.mediaType, indexType);
  assert.equal(image.digest, expectedIndexDigest, 'OCI image must match the build output digest');
  const verified = new Map();
  let totalBytes = 0;
  async function blob(descriptor, json = false) {
    assert.match(descriptor?.digest || '', digestPattern);
    assert.ok(Number.isSafeInteger(descriptor.size) && descriptor.size > 0 && descriptor.size <= 256 << 20, 'invalid OCI blob size');
    const path = join(root, 'blobs', 'sha256', descriptor.digest.slice(7));
    if (verified.has(descriptor.digest)) assert.equal(verified.get(descriptor.digest), descriptor.size);
    else {
      assert.ok(verified.size < 256, 'too many OCI blobs');
      const actual = await fileDigest(path, 256 << 20);
      assert.equal(actual.digest, descriptor.digest, 'OCI blob digest mismatch');
      assert.equal(actual.size, descriptor.size, 'OCI blob size mismatch');
      verified.set(descriptor.digest, descriptor.size);
      totalBytes += descriptor.size;
      assert.ok(totalBytes <= 1024 << 20, 'OCI image exceeds the release limit');
    }
    return json ? jsonFile(path) : undefined;
  }
  const index = await blob(image, true);
  assert.equal(index.schemaVersion, 2);
  assert.equal(index.mediaType, indexType);
  assert.ok(Array.isArray(index.manifests) && index.manifests.length <= 16);
  const platforms = {};
  const attestations = [];
  for (const descriptor of index.manifests) {
    assert.equal(descriptor.mediaType, manifestType);
    const manifest = await blob(descriptor, true);
    assert.equal(manifest.schemaVersion, 2);
    assert.equal(manifest.mediaType, manifestType);
    assert.equal(manifest.config?.mediaType, configType);
    const config = await blob(manifest.config, true);
    assert.ok(Array.isArray(manifest.layers) && manifest.layers.length <= 64);
    if (descriptor.annotations?.['vnd.docker.reference.type'] === 'attestation-manifest') {
      const target = descriptor.annotations['vnd.docker.reference.digest'];
      assert.match(target || '', digestPattern);
      assert.equal(descriptor.platform?.os, 'unknown');
      assert.equal(descriptor.platform?.architecture, 'unknown');
      const types = new Set();
      for (const layer of manifest.layers) {
        assert.equal(layer.mediaType, 'application/vnd.in-toto+json');
        const statement = await blob(layer, true);
        const type = layer.annotations?.['in-toto.io/predicate-type'];
        assert.ok(['https://in-toto.io/Statement/v0.1', 'https://in-toto.io/Statement/v1'].includes(statement._type), 'invalid attestation statement type');
        assert.equal(statement.predicateType, type, 'attestation predicate must match its descriptor');
        assert.ok(typeof type === 'string' && type.length > 0);
        assert.ok(Array.isArray(statement.subject) && statement.subject.some(subject => subject?.digest?.sha256 === target.slice(7)),
          'attestation statement subject must bind its target runtime manifest');
        types.add(type);
      }
      attestations.push({ target, types });
    } else {
      const arch = descriptor.platform?.architecture;
      assert.ok(architectures.includes(arch), 'unexpected runtime architecture');
      assert.equal(descriptor.platform.os, 'linux');
      assert.equal(config.os, 'linux');
      assert.equal(config.architecture, arch);
      assert.equal(config.config?.User, '65532:65532');
      assert.deepEqual(config.config?.Entrypoint, ['/salcara-hub-launcher']);
      assert.equal(config.config?.Labels?.['org.opencontainers.image.version'], version);
      assert.equal(config.rootfs?.type, 'layers');
      assert.equal(config.rootfs.diff_ids?.length, manifest.layers.length);
      assert.ok(config.rootfs.diff_ids.every(value => digestPattern.test(value)));
      assert.equal(platforms[arch], undefined, 'duplicate runtime architecture');
      for (const layer of manifest.layers) await blob(layer);
      platforms[arch] = { manifest_digest: descriptor.digest, config_digest: manifest.config.digest };
    }
  }
  assert.deepEqual(Object.keys(platforms).sort(), architectures);
  const runtimeDigests = new Set(Object.values(platforms).map(item => item.manifest_digest));
  assert.ok(attestations.every(item => runtimeDigests.has(item.target)), 'unbound OCI attestation');
  for (const platform of Object.values(platforms)) {
    const predicates = new Set(attestations.filter(item => item.target === platform.manifest_digest).flatMap(item => [...item.types]));
    assert.ok([...provenanceTypes].some(type => predicates.has(type)), 'each architecture needs build provenance');
    assert.ok(predicates.has(sbomType), 'each architecture needs its SBOM');
  }
  return { index_digest: expectedIndexDigest, platforms };
}

// Modern containerd-backed Docker can report a manifest digest in inspect.Id;
// older image stores report the config digest. Hash the config bytes from the
// exact imported image's Docker save archive instead of assuming either shape.
export function readDockerArchiveConfig(archive) {
  regularFile(archive, 1024 << 20);
  const tar = process.platform === 'win32' ? join(process.env.SystemRoot || 'C:\\Windows', 'System32', 'tar.exe') : 'tar';
  const entry = name => execFileSync(tar, ['-xOf', resolve(archive), name], {
    stdio: ['ignore', 'pipe', 'pipe'], timeout: 10000, maxBuffer: 32 << 20,
  });
  const manifest = JSON.parse(entry('manifest.json').toString('utf8'));
  assert.ok(Array.isArray(manifest) && manifest.length === 1, 'one imported Docker image is required');
  const name = manifest[0].Config;
  assert.match(name || '', /^(?:[a-f0-9]{64}\.json|blobs\/sha256\/[a-f0-9]{64})$/, 'unexpected Docker config path');
  const config = entry(name);
  assert.ok(config.length > 0 && config.length <= 32 << 20, 'invalid Docker config size');
  const expected = name.endsWith('.json') ? name.slice(0, -5) : name.slice('blobs/sha256/'.length);
  assert.equal(createHash('sha256').update(config).digest('hex'), expected, 'Docker config filename does not match its bytes');
  return config;
}

export function verifyImportedImage(inspection, receipt, arch, savedConfig) {
  assert.ok(architectures.includes(arch));
  assert.equal(inspection.length, 1);
  if (savedConfig !== undefined) {
    assert.ok(Buffer.isBuffer(savedConfig));
    assert.equal(`sha256:${createHash('sha256').update(savedConfig).digest('hex')}`, receipt.platforms[arch].config_digest,
      'Docker import changed the checked OCI config');
  } else {
    assert.equal(inspection[0].Id, receipt.platforms[arch].config_digest, 'Docker import changed the checked OCI config');
  }
  assert.equal(inspection[0].Os, 'linux');
  assert.equal(inspection[0].Architecture, arch);
  assert.equal(inspection[0].Config?.Labels?.['org.opencontainers.image.version'], receipt.version);
}

export async function verifyImageArtifact(layout, archive, receiptPath, version, sourceSHA, indexDigest, archiveDigest) {
  validateIdentity(version, sourceSHA);
  assert.match(archiveDigest || '', digestPattern);
  const receipt = jsonFile(receiptPath);
  const graph = await verifyLayout(layout, version, indexDigest);
  const actual = await fileDigest(archive);
  assert.equal(actual.digest, archiveDigest, 'OCI archive differs from the successful build job');
  assert.deepEqual(receipt, {
    schema: 1, product: 'salcara-hub-standalone', version, source_sha: sourceSHA,
    archive_digest: actual.digest, archive_size: actual.size, ...graph,
  }, 'release receipt identity mismatch');
  return receipt;
}

async function seal([layout, archive, receiptPath, version, sourceSHA, indexDigest]) {
  validateIdentity(version, sourceSHA);
  const graph = await verifyLayout(layout, version, indexDigest);
  const actual = await fileDigest(archive);
  const receipt = { schema: 1, product: 'salcara-hub-standalone', version, source_sha: sourceSHA,
    archive_digest: actual.digest, archive_size: actual.size, ...graph };
  writeFileSync(receiptPath, `${JSON.stringify(receipt, null, 2)}\n`, { flag: 'wx', mode: 0o600 });
  if (process.env.GITHUB_OUTPUT) appendFileSync(process.env.GITHUB_OUTPUT, `archive_digest=${actual.digest}\n`);
}

function exportExactBinaries([archive, receiptPath, outputDirectory]) {
  const receipt = jsonFile(receiptPath);
  validateIdentity(receipt.version, receipt.source_sha);
  const run = (command, args) => execFileSync(command, args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], timeout: 120000 });
  for (const arch of architectures) {
    const image = `salcara-hub-standalone-ci:${arch}`;
    run('skopeo', ['--override-os', 'linux', '--override-arch', arch, 'copy', `oci-archive:${resolve(archive)}`, `docker-daemon:${image}`]);
    const inspection = JSON.parse(run('docker', ['image', 'inspect', image]));
    assert.match(inspection[0]?.Id || '', digestPattern);
    const temporary = mkdtempSync(join(tmpdir(), 'salcara-release-import-'));
    try {
      const saved = join(temporary, 'imported.tar');
      run('docker', ['image', 'save', '--output', saved, inspection[0].Id]);
      verifyImportedImage(inspection, receipt, arch, readDockerArchiveConfig(saved));
    } finally {
      assert.equal(dirname(temporary), resolve(tmpdir()));
      assert.ok(!lstatSync(temporary).isSymbolicLink());
      rmSync(temporary, { recursive: true, force: true });
    }
    const output = join(resolve(outputDirectory), `linux-${arch}`);
    mkdirSync(output, { mode: 0o700 });
    const name = `salcara-hub-release-export-${randomBytes(10).toString('hex')}`;
    // create/cp never starts the foreign-architecture binary or accesses a real volume.
    run('docker', ['create', '--platform', `linux/${arch}`, '--name', name, '--label', 'salcara.test=release-export', inspection[0].Id]);
    try {
      for (const binary of ['salcara-hub', 'salcara-hub-launcher']) run('docker', ['cp', `${name}:/${binary}`, join(output, binary)]);
    } finally {
      const owned = JSON.parse(run('docker', ['inspect', name]))[0];
      assert.equal(owned.Config.Labels['salcara.test'], 'release-export');
      assert.equal(owned.State.Running, false);
      run('docker', ['rm', name]);
    }
  }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  const [command, ...args] = process.argv.slice(2);
  try {
    if (command === 'seal') await seal(args);
    else if (command === 'verify') await verifyImageArtifact(...args);
    else if (command === 'export') exportExactBinaries(args);
    else if (command === 'check-archive') {
      assert.match(args[1] || '', digestPattern);
      assert.equal((await fileDigest(args[0])).digest, args[1], 'OCI archive differs from the successful build job');
    }
    else throw new Error('unknown release image operation');
    console.log(`Release image ${command} verification passed; no production service was started.`);
  } catch (error) {
    // Never echo a child process' raw stderr or environment into CI logs.
    console.error(`Release image rejected: ${error instanceof assert.AssertionError ? error.message : 'local verification or tool failed'}`);
    process.exitCode = 1;
  }
}
