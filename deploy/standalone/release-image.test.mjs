import assert from 'node:assert/strict';
import test from 'node:test';
import { createHash } from 'node:crypto';
import { mkdtempSync, readFileSync, rmSync, writeFileSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join } from 'node:path';
import { fileDigest, validateIdentity, verifyLayout, verifyImageArtifact, verifyImportedImage, readDockerArchiveConfig } from './release-image.mjs';
import { registryClient, requireAbsent, publishExact } from './publish-image.mjs';
import { createOCIFixture, fixtureTar } from './release-image-fixture.mjs';
import { cleanupToolsFixtures } from './release-image-tools-smoke.mjs';

const sha = 'a'.repeat(40);
const version = '0.5.3';
const digest = bytes => `sha256:${createHash('sha256').update(bytes).digest('hex')}`;
const jsonResponse = (data, status = 200, headers = {}) => new Response(JSON.stringify(data), { status, headers });
const absent = () => jsonResponse({ errors: [{ code: 'MANIFEST_UNKNOWN' }] }, 404);

// A small synthetic OCI graph; no Docker, real credential, network, or executable.
function fixture(t, options = {}) {
  const root = mkdtempSync(join(tmpdir(), 'salcara-oci-release-test-'));
  t.after(() => rmSync(root, { recursive: true, force: true }));
  return createOCIFixture(root, options);
}

test('OCI verifier follows and hashes the actual graph, requiring both runtime architectures and their attestations', async t => {
  const f = fixture(t);
  assert.deepEqual(await verifyLayout(f.layout, version, f.indexDigest), { index_digest: f.indexDigest, platforms: f.platforms });
});

test('OCI 1.1 attestations accept only the canonical empty config while keeping runtime configs and subjects bound', async t => {
  for (const emptyConfigDescriptor of [undefined, { data: 'e30=' }]) {
    const f = fixture(t, { emptyAttestationConfig: true, emptyConfigDescriptor });
    assert.deepEqual(await verifyLayout(f.layout, version, f.indexDigest), { index_digest: f.indexDigest, platforms: f.platforms });
  }
});

for (const option of ['rootUser', 'wrongVersion', 'duplicateArch', 'noSBOM', 'noProvenance', 'badPredicate', 'unboundAttestation', 'noSubject', 'wrongSubject']) {
  test(`OCI graph rejects ${option} without invoking a container`, async t => {
    const f = fixture(t, { [option]: true });
    await assert.rejects(verifyLayout(f.layout, version, f.indexDigest));
  });
  test(`OCI empty-config attestations still reject ${option}`, async t => {
    const f = fixture(t, { emptyAttestationConfig: true, [option]: true });
    await assert.rejects(verifyLayout(f.layout, version, f.indexDigest));
  });
}

test('OCI empty-config compatibility cannot substitute config content, inline data, size or runtime media type', async t => {
  for (const options of [
    { emptyConfigBytes: '[]' },
    { emptyConfigBytes: 'null' },
    { emptyConfigBytes: '{"os":"linux"}' },
    { emptyConfigBytes: '{}\n' },
    { emptyConfigDescriptor: { size: 3 } },
    { emptyConfigDescriptor: { data: 'W10=' } },
    { runtimeConfigMediaType: 'application/vnd.oci.empty.v1+json' },
  ]) {
    const f = fixture(t, { emptyAttestationConfig: true, ...options });
    await assert.rejects(verifyLayout(f.layout, version, f.indexDigest));
  }
  const f = fixture(t, { emptyAttestationConfig: true });
  const emptyDigest = digest(Buffer.from('{}'));
  writeFileSync(join(f.blobs, emptyDigest.slice(7)), '[]');
  await assert.rejects(verifyLayout(f.layout, version, f.indexDigest), /digest mismatch/);
});

test('OCI attestation platform and artifact subject cannot be redirected or relabelled', async t => {
  for (const options of [
    { attestationPlatform: { os: 'linux', architecture: 'unknown' } },
    { attestationPlatform: { os: 'unknown', architecture: 'amd64' } },
    { manifestSubject: { digest: `sha256:${'c'.repeat(64)}` } },
    { manifestSubject: { size: 1 } },
    { manifestSubject: { mediaType: 'application/vnd.oci.image.config.v1+json' } },
  ]) {
    const f = fixture(t, { emptyAttestationConfig: true, ...options });
    await assert.rejects(verifyLayout(f.layout, version, f.indexDigest));
  }
  for (const legacyAttestationConfig of [
    { architecture: 'amd64', os: 'linux', rootfs: { type: 'layers', diff_ids: [] } },
    { architecture: 'unknown', os: 'unknown', rootfs: { type: 'layers', diff_ids: [`sha256:${'d'.repeat(64)}`] } },
  ]) {
    const f = fixture(t, { legacyAttestationConfig });
    await assert.rejects(verifyLayout(f.layout, version, f.indexDigest));
  }
});

test('OCI verifier rejects a changed blob and a different top-level build digest', async t => {
  const f = fixture(t);
  await assert.rejects(verifyLayout(f.layout, version, `sha256:${'c'.repeat(64)}`));
  writeFileSync(join(f.blobs, f.platforms.amd64.config_digest.slice(7)), '{}');
  await assert.rejects(verifyLayout(f.layout, version, f.indexDigest), /digest mismatch/);
});

test('archive receipt binds hash, byte length, graph, source SHA and exact version', async t => {
  const f = fixture(t);
  const archive = await fileDigest(f.archive);
  const receiptPath = join(f.root, 'receipt.json');
  const receipt = { schema: 1, product: 'salcara-hub-standalone', version, source_sha: sha,
    archive_digest: archive.digest, archive_size: archive.size, index_digest: f.indexDigest, platforms: f.platforms };
  writeFileSync(receiptPath, JSON.stringify(receipt));
  assert.deepEqual(await verifyImageArtifact(f.layout, f.archive, receiptPath, version, sha, f.indexDigest, archive.digest), receipt);
  await assert.rejects(verifyImageArtifact(f.layout, f.archive, receiptPath, version, 'b'.repeat(40), f.indexDigest, archive.digest));
  writeFileSync(f.archive, 'substituted archive');
  await assert.rejects(verifyImageArtifact(f.layout, f.archive, receiptPath, version, sha, f.indexDigest, archive.digest), /successful build/);
});

test('imported Docker image is checked against the OCI config ID, architecture and version', t => {
  const f = fixture(t);
  const receipt = { version, platforms: f.platforms };
  const image = { Id: f.platforms.amd64.config_digest, Os: 'linux', Architecture: 'amd64',
    Config: { Labels: { 'org.opencontainers.image.version': version } } };
  assert.doesNotThrow(() => verifyImportedImage([image], receipt, 'amd64'));
  for (const changed of [{ ...image, Id: `sha256:${'f'.repeat(64)}` }, { ...image, Architecture: 'arm64' }, { ...image, Os: 'windows' }]) {
    assert.throws(() => verifyImportedImage([changed], receipt, 'amd64'));
  }
});

test('containerd image IDs require exact saved config bytes rather than treating a manifest as a config', t => {
  const f = fixture(t);
  const receipt = { version, platforms: f.platforms };
  const image = { Id: `sha256:${'f'.repeat(64)}`, Os: 'linux', Architecture: 'amd64',
    Config: { Labels: { 'org.opencontainers.image.version': version } } };
  assert.throws(() => verifyImportedImage([image], receipt, 'amd64'));
  const saved = readFileSync(join(f.blobs, f.platforms.amd64.config_digest.slice(7)));
  assert.doesNotThrow(() => verifyImportedImage([image], receipt, 'amd64', saved));
  assert.throws(() => verifyImportedImage([image], receipt, 'amd64', Buffer.from('{}')));
});

test('Docker save config reader verifies classic and containerd config names against actual bytes', t => {
  const f = fixture(t);
  const config = readFileSync(join(f.blobs, f.platforms.amd64.config_digest.slice(7)));
  const hex = digest(config).slice(7);
  for (const name of [`${hex}.json`, `blobs/sha256/${hex}`]) {
    writeFileSync(f.archive, fixtureTar([{ name: 'manifest.json', data: JSON.stringify([{ Config: name }]) }, { name, data: config }]));
    assert.deepEqual(readDockerArchiveConfig(f.archive), config);
  }
});

test('Docker save config reader rejects substituted bytes, traversal, ambiguity and duplicated config entries', t => {
  const f = fixture(t);
  const config = Buffer.from('{"synthetic":true}');
  const name = `${digest(config).slice(7)}.json`;
  const manifest = entries => ({ name: 'manifest.json', data: JSON.stringify(entries) });
  for (const entries of [
    [manifest([{ Config: name }]), { name, data: '{}' }],
    [manifest([{ Config: '../secret.json' }])],
    [manifest([{ Config: name }, { Config: name }]), { name, data: config }],
    [manifest([{ Config: name }]), { name, data: config }, { name, data: config }],
  ]) {
    writeFileSync(f.archive, fixtureTar(entries));
    assert.throws(() => readDockerArchiveConfig(f.archive));
  }
});

test('real-tool failure cleanup uses observed IDs, refuses changed ownership and still cleans other fixtures', () => {
  const first = { name: 'salcara-oci-tools-smoke-0123456789abcdef:amd64', digest: `sha256:${'a'.repeat(64)}` };
  const second = { name: 'salcara-oci-tools-smoke-0123456789abcdef:arm64', digest: `sha256:${'b'.repeat(64)}` };
  const removed = [];
  const run = args => {
    const name = args[2];
    if (args[1] === 'ls') return 'known-image';
    if (args[1] === 'inspect') return JSON.stringify([{ Id: name === first.name ? `sha256:${'f'.repeat(64)}` : second.digest,
      Config: { Labels: { 'salcara.test': 'release-oci' } }, RepoTags: [name] }]);
    if (args[1] === 'rm') removed.push(name);
    return '';
  };
  const failed = cleanupToolsFixtures([first, second], '/synthetic-fixture-only', run, path => removed.push(path));
  assert.deepEqual(failed, [`image ${first.name}`]);
  assert.deepEqual(removed, [second.name, '/synthetic-fixture-only']);
});

test('real-tool failure cleanup handles a partial import and never selects a foreign image', () => {
  const partial = { name: 'salcara-oci-tools-smoke-0123456789abcdef:amd64', digest: null };
  let removedRoot = false;
  assert.deepEqual(cleanupToolsFixtures([partial], '/synthetic-fixture-only', args => {
    assert.equal(args[1], 'ls'); return '';
  }, () => { removedRoot = true; }), []);
  assert.equal(removedRoot, true);
  const failed = cleanupToolsFixtures([{ name: 'user-image:latest' }], '/synthetic-fixture-only', () => { throw new Error('must not touch Docker'); }, () => {});
  assert.deepEqual(failed, ['image user-image:latest']);
});

test('release identity rejects noncanonical versions, path injection and excessive version components', () => {
  for (const invalid of ['v0.5.3', '00.5.3', '0.5.3-dev', '4294967296.0.0', '0.5.3/other']) assert.throws(() => validateIdentity(invalid, sha));
  assert.throws(() => validateIdentity(version, 'branch-name'));
});

test('registry authentication and manifest reads use fixed HTTPS targets, no redirects and bounded requests', async () => {
  const calls = [];
  const get = await registryClient({ username: 'synthetic-user', password: 'synthetic-token', fetchImpl: async (url, options) => {
    calls.push({ url, options });
    return calls.length === 1 ? jsonResponse({ token: 'synthetic-registry-token' }) : absent();
  } });
  await requireAbsent(get, version);
  assert.match(calls[0].url, /^https:\/\/ghcr\.io\/token\?service=ghcr.io&scope=repository:dkdjndbfj-wq\/salcara-hub-standalone:pull$/);
  assert.equal(calls[1].url, 'https://ghcr.io/v2/dkdjndbfj-wq/salcara-hub-standalone/manifests/0.5.3');
  for (const call of calls) {
    assert.equal(call.options.redirect, 'error');
    assert.equal(call.options.cache, 'no-store');
    assert.ok(call.options.signal instanceof AbortSignal);
  }
  assert.equal(calls[1].options.headers.Authorization, 'Bearer synthetic-registry-token');
});

test('only authenticated, structured manifest/name absence permits first publication', async () => {
  await requireAbsent(async () => absent(), version);
  await requireAbsent(async () => jsonResponse({ errors: [{ code: 'NAME_UNKNOWN' }] }, 404), version);
  for (const status of [200, 301, 401, 403, 429, 500, 503]) {
    await assert.rejects(requireAbsent(async () => jsonResponse({}, status), version));
  }
  for (const errors of [[], [{ code: 'UNAUTHORIZED' }], [{ code: 'MANIFEST_UNKNOWN' }, { code: 'DENIED' }]]) {
    await assert.rejects(requireAbsent(async () => jsonResponse({ errors }, 404), version));
  }
  await assert.rejects(requireAbsent(async () => new Response('<html>not found</html>', { status: 404 }), version));
  await assert.rejects(requireAbsent(async () => { throw new Error('synthetic network timeout'); }, version));
  await assert.rejects(registryClient({ username: 'synthetic', password: 'synthetic', fetchImpl: async () => jsonResponse({}, 403) }));
  await assert.rejects(registryClient({ username: 'synthetic', password: 'synthetic', fetchImpl: async () => jsonResponse({}) }));
});

test('publication checks artifact then absence, copies once, and verifies the returned index bytes and digest', async () => {
  const bytes = Buffer.from('{"schemaVersion":2,"synthetic":"multiarch"}');
  const expectedDigest = digest(bytes);
  const events = [];
  const getManifest = async () => {
    events.push('query');
    return events.length === 2 ? absent() : new Response(bytes, { headers: { 'docker-content-digest': expectedDigest } });
  };
  await publishExact({ version, expectedDigest, getManifest,
    verifyArtifact: async () => { events.push('verify'); }, copyArtifact: async () => { events.push('copy'); } });
  assert.deepEqual(events, ['verify', 'query', 'copy', 'query']);
});

test('invalid artifacts, existing tags and unknown availability never reach the copy operation', async () => {
  let copied = 0;
  const options = { version, expectedDigest: `sha256:${'f'.repeat(64)}`, copyArtifact: async () => { copied++; } };
  await assert.rejects(publishExact({ ...options, verifyArtifact: async () => { throw new Error('bad artifact'); }, getManifest: async () => { throw new Error('must not query'); } }));
  for (const status of [200, 401, 403, 429, 503]) {
    await assert.rejects(publishExact({ ...options, verifyArtifact: async () => {}, getManifest: async () => jsonResponse({}, status) }));
  }
  assert.equal(copied, 0);
});

test('an uncertain copy is never automatically retried, and a mismatched published digest is not success', async () => {
  let copies = 0;
  const base = { version, expectedDigest: `sha256:${'f'.repeat(64)}`, verifyArtifact: async () => {} };
  await assert.rejects(publishExact({ ...base, getManifest: async () => absent(), copyArtifact: async () => { copies++; throw new Error('connection ended'); } }));
  assert.equal(copies, 1);
  let queries = 0;
  await assert.rejects(publishExact({ ...base, getManifest: async () => ++queries === 1 ? absent() : jsonResponse({ different: 'image' }, 200, { 'docker-content-digest': base.expectedDigest }),
    copyArtifact: async () => { copies++; } }), /differs from the tested artifact/);
  assert.equal(copies, 2);
});

test('workflow serializes by version across refs and publishes the checked artifact ID without rebuilding', () => {
  const workflow = readFileSync(new URL('../../.github/workflows/standalone-release.yml', import.meta.url), 'utf8');
  const publish = workflow.slice(workflow.indexOf('  publish-image:'));
  assert.match(workflow, /group: standalone-release-\$\{\{ github.repository \}\}-\$\{\{ inputs.version \}\}/);
  assert.match(workflow, /cancel-in-progress: false/);
  assert.equal((workflow.match(/uses: docker\/build-push-action/g) || []).length, 1);
  assert.match(workflow, /outputs: type=oci,dest=/);
  assert.match(workflow, /tags: ghcr\.io\/dkdjndbfj-wq\/salcara-hub-standalone:\$\{\{ steps.version.outputs.value \}\}/);
  assert.match(workflow, /provenance: true/);
  assert.match(workflow, /sbom: true/);
  assert.match(publish, /artifact-ids: \$\{\{ needs.build.outputs.artifact_id \}\}/);
  assert.match(publish, /ARCHIVE_DIGEST: \$\{\{ needs.build.outputs.archive_digest \}\}/);
  assert.match(publish, /IMAGE_DIGEST: \$\{\{ needs.build.outputs.image_digest \}\}/);
  assert.doesNotMatch(publish, /build-push-action|docker build|setup-buildx|push: true|--tls-verify=false/);
  assert.ok(publish.indexOf('release-image.mjs check-archive') < publish.indexOf('skopeo copy'));
  assert.ok(publish.indexOf('release-image.mjs verify') < publish.indexOf('skopeo login'));
  assert.match(publish, /publish-image.mjs/);
  assert.ok(workflow.indexOf('container-smoke.mjs salcara-hub-standalone-ci:amd64') < workflow.indexOf('uses: actions/upload-artifact@v4'));
});
