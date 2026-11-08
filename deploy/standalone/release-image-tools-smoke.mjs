// Explicit opt-in real skopeo/Docker transport check, using ONLY a synthetic image.
// No registry push/login, daemon socket mount, executable payload, or user data.
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { randomBytes } from 'node:crypto';
import { mkdtempSync, mkdirSync, lstatSync, readFileSync, writeFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname, basename, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { createOCIFixture } from './release-image-fixture.mjs';
import { verifyLayout, verifyImportedImage, readDockerArchiveConfig } from './release-image.mjs';

export function cleanupToolsFixtures(images, root, run, removeRoot = directory => {
  assert.equal(dirname(resolve(directory)), resolve(tmpdir()));
  assert.match(basename(directory), /^salcara-oci-tools-smoke-[a-zA-Z0-9]{6}$/);
  assert.ok(!lstatSync(directory).isSymbolicLink());
  rmSync(directory, { recursive: true, force: true });
}) {
  const failures = [];
  for (const image of images) {
    try {
      assert.match(image.name, /^salcara-oci-tools-smoke-[a-f0-9]{16}:(amd64|arm64)$/);
      if (!run(['image', 'ls', '--filter', `reference=${image.name}`, '--format', '{{.ID}}']).trim()) continue;
      const current = JSON.parse(run(['image', 'inspect', image.name]))[0];
      if (image.digest) assert.equal(current.Id, image.digest);
      assert.equal(current.Config.Labels['salcara.test'], 'release-oci');
      assert.ok(current.RepoTags.includes(image.name));
      run(['image', 'rm', image.name]);
    } catch { failures.push(`image ${image.name}`); }
  }
  // A failed image check must not mask the original validation error or prevent
  // cleanup of another known fixture and the isolated temporary directory.
  try { removeRoot(root); } catch { failures.push('synthetic temporary directory'); }
  return failures;
}

export async function runToolsSmoke(tool, buildkitFixture = false) {
assert.match(tool || '', /^quay\.io\/skopeo\/stable(?::[a-zA-Z0-9._-]+|@sha256:[a-f0-9]{64})$/, 'pass the already-pulled official tool image');
const run = (args, timeout = 45000) => execFileSync('docker', args, { encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], timeout });
const root = mkdtempSync(join(tmpdir(), 'salcara-oci-tools-smoke-'));
const suffix = randomBytes(8).toString('hex');
const images = [];
let toolOrdinal = 0;
let failure;
let cleanupFailures = [];
const skopeo = args => {
  const name = `salcara-oci-tools-${suffix}-${++toolOrdinal}`;
  try {
    return run(['run', '--rm', '--pull=never', '--name', name, '--label', 'salcara.test=oci-tool', '--network=none', '--read-only', '--cap-drop=ALL',
      '--security-opt=no-new-privileges', '--pids-limit=64', '--memory=256m',
      '--tmpfs', '/tmp:rw,noexec,nosuid,nodev,size=32m', '--tmpfs', '/var/tmp:rw,noexec,nosuid,nodev,size=32m',
      '--mount', `type=bind,src=${root},dst=/work`, tool, ...args]);
  } finally {
    let remaining;
    try { remaining = JSON.parse(run(['inspect', name]))[0]; } catch { /* --rm already removed the exact fixture */ }
    if (remaining) {
      assert.equal(remaining.Config.Labels['salcara.test'], 'oci-tool');
      if (remaining.State.Running) run(['stop', '--time', '5', name]);
      // --rm may have removed it after stop; no other container is selected.
      try { run(['rm', name]); } catch { /* already removed */ }
    }
  }
};
try {
  const fixture = createOCIFixture(root, { realTools: true, emptyAttestationConfig: true });
  console.log(skopeo(['--version']).trim());
  const help = skopeo(['copy', '--help']);
  assert.match(help, /--all/); assert.match(help, /--preserve-digests/); assert.match(help, /--authfile/);
  skopeo(['copy', '--all', '--preserve-digests', 'oci:/work/layout:release', 'oci-archive:/work/image.tar:release']);
  skopeo(['copy', '--all', '--preserve-digests', 'oci-archive:/work/image.tar', 'oci:/work/roundtrip:release']);
  await verifyLayout(join(root, 'roundtrip'), '0.5.3', fixture.indexDigest);
  for (const arch of ['amd64', 'arm64']) {
    const name = `salcara-oci-tools-smoke-${suffix}:${arch}`;
    skopeo(['--override-os', 'linux', '--override-arch', arch, 'copy', 'oci-archive:/work/image.tar', `docker-archive:/work/${arch}.tar:${name}`]);
    assert.equal(run(['image', 'ls', '--filter', `reference=${name}`, '--format', '{{.ID}}']).trim(), '', 'fixture image name already exists');
    const ownedImage = { name, digest: null };
    images.push(ownedImage);
    run(['load', '--input', join(root, `${arch}.tar`)]);
    const inspection = JSON.parse(run(['image', 'inspect', name]));
    ownedImage.digest = inspection[0].Id;
    const saved = join(root, `${arch}-imported.tar`);
    run(['image', 'save', '--output', saved, inspection[0].Id]);
    verifyImportedImage(inspection, { version: '0.5.3', platforms: fixture.platforms }, arch, readDockerArchiveConfig(saved));
  }
  if (buildkitFixture) {
    // A scratch-only local context: no RUN, real Hub source or executable.
    // This explicit option may fetch BuildKit's standard SBOM scanner image;
    // it never starts the fixture image or publishes an artifact.
    const context = join(root, 'buildkit-context');
    mkdirSync(context);
    writeFileSync(join(context, 'Dockerfile'), 'FROM scratch\nCOPY fixture.txt /fixture.txt\nUSER 65532:65532\nENTRYPOINT ["/salcara-hub-launcher"]\nLABEL org.opencontainers.image.version="0.5.3" salcara.test="release-oci"\n');
    writeFileSync(join(context, 'fixture.txt'), 'Synthetic BuildKit provenance fixture.\n');
    run(['buildx', 'build', '--platform', 'linux/amd64,linux/arm64', '--tag', `salcara-oci-buildkit-fixture-${suffix}:test`, '--provenance=true', '--sbom=true',
      '--metadata-file', join(root, 'buildkit-metadata.json'),
      '--output', `type=oci,oci-mediatypes=true,oci-artifact=true,dest=${join(root, 'buildkit.tar')}`, context], 300000);
    skopeo(['copy', '--all', '--preserve-digests', 'oci-archive:/work/buildkit.tar', 'oci:/work/buildkit-layout:release']);
    const layout = join(root, 'buildkit-layout');
    const blob = descriptor => {
      assert.match(descriptor.digest, /^sha256:[a-f0-9]{64}$/);
      return JSON.parse(readFileSync(join(layout, 'blobs', 'sha256', descriptor.digest.slice(7)), 'utf8'));
    };
    const wrapper = JSON.parse(readFileSync(join(layout, 'index.json'), 'utf8'));
    assert.equal(wrapper.manifests.length, 1);
    const metadata = JSON.parse(readFileSync(join(root, 'buildkit-metadata.json'), 'utf8'));
    await verifyLayout(layout, '0.5.3', metadata['containerimage.digest']);
    const index = blob(wrapper.manifests[0]);
    const attestations = index.manifests.filter(item => item.annotations?.['vnd.docker.reference.type'] === 'attestation-manifest');
    assert.equal(attestations.length, 2, 'real BuildKit must attest both runtime architectures');
    for (const attestation of attestations) {
      assert.equal(blob(attestation).config.mediaType, 'application/vnd.oci.empty.v1+json', 'exercise the real BuildKit OCI empty-config format');
    }
    console.log('Real scratch BuildKit OCI artifact verification passed: both platforms, canonical empty configs, all blob hashes, provenance, SBOM and manifest/statement subjects verified by the release validator.');
  }
} catch (error) {
  failure = error;
} finally {
  cleanupFailures = cleanupToolsFixtures(images, root, run);
}
if (cleanupFailures.length) console.error(`Synthetic fixture cleanup needs attention: ${cleanupFailures.join(', ')}`);
if (failure) throw failure;
assert.equal(cleanupFailures.length, 0, 'synthetic fixture cleanup was incomplete');
console.log('Offline real-tool smoke passed: OCI archive/index/attestations survived unchanged; both Docker imports kept the exact config bytes. No fixture image was started.');
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  await runToolsSmoke(process.argv[2], process.argv.includes('--buildkit-fixture'));
}
