// Copy an already-verified OCI image. Never build here and never log credentials.
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { execFileSync } from 'node:child_process';
import { resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { imageRepository, validateIdentity, verifyImageArtifact } from './release-image.mjs';

const repositoryPath = imageRepository.slice('ghcr.io/'.length);
const accept = 'application/vnd.oci.image.index.v1+json, application/vnd.docker.distribution.manifest.list.v2+json, application/vnd.oci.image.manifest.v1+json';
const hash = bytes => `sha256:${createHash('sha256').update(bytes).digest('hex')}`;

async function responseBytes(response, limit) {
  assert.ok(response.body, 'registry returned an empty response');
  const declared = response.headers.get('content-length');
  if (declared) assert.ok(/^\d+$/.test(declared) && Number(declared) <= limit, 'registry response is too large');
  const reader = response.body.getReader();
  const chunks = [];
  let length = 0;
  try {
    while (true) {
      const { value, done } = await reader.read();
      if (done) break;
      length += value.length;
      assert.ok(length <= limit, 'registry response is too large');
      chunks.push(Buffer.from(value));
    }
  } finally { await reader.cancel().catch(() => {}); }
  return Buffer.concat(chunks);
}

export async function registryClient({ username, password, fetchImpl = fetch }) {
  assert.ok(typeof username === 'string' && /^[A-Za-z0-9_.[\]-]+$/.test(username), 'registry username is required');
  assert.ok(typeof password === 'string' && password.length > 0 && !/[\r\n]/.test(password), 'registry credential is required');
  const url = `https://ghcr.io/token?service=ghcr.io&scope=repository:${repositoryPath}:pull`;
  const response = await fetchImpl(url, {
    headers: { Authorization: `Basic ${Buffer.from(`${username}:${password}`).toString('base64')}` },
    redirect: 'error', cache: 'no-store', signal: AbortSignal.timeout(15000),
  });
  assert.equal(response.status, 200, 'registry authentication failed; version availability is unknown');
  const body = JSON.parse((await responseBytes(response, 64 << 10)).toString('utf8'));
  const token = body.token || body.access_token;
  assert.ok(typeof token === 'string' && token.length > 0 && token.length < 64 << 10 && !/[\r\n]/.test(token), 'registry authentication did not return a token');
  return async version => fetchImpl(`https://${imageRepository.split('/')[0]}/v2/${repositoryPath}/manifests/${version}`, {
    headers: { Authorization: `Bearer ${token}`, Accept: accept },
    redirect: 'error', cache: 'no-store', signal: AbortSignal.timeout(15000),
  });
}

export async function requireAbsent(getManifest, version) {
  validateIdentity(version, '0'.repeat(40));
  const response = await getManifest(version);
  if (response.status !== 404) {
    await response.body?.cancel().catch(() => {});
    throw new Error(response.status === 200 ? 'version already exists; refusing to overwrite' : 'registry availability is unknown; refusing to publish');
  }
  const body = JSON.parse((await responseBytes(response, 64 << 10)).toString('utf8'));
  assert.ok(Array.isArray(body.errors) && body.errors.length > 0
    && body.errors.every(item => ['MANIFEST_UNKNOWN', 'NAME_UNKNOWN'].includes(item?.code)),
  'only an authenticated missing-manifest response permits publication');
}

export async function requirePublishedDigest(getManifest, version, expectedDigest) {
  assert.match(expectedDigest, /^sha256:[a-f0-9]{64}$/);
  const response = await getManifest(version);
  assert.equal(response.status, 200, 'published tag cannot be verified');
  const bytes = await responseBytes(response, 4 << 20);
  assert.equal(hash(bytes), expectedDigest, 'published image index differs from the tested artifact');
  assert.equal(response.headers.get('docker-content-digest'), expectedDigest, 'registry digest is inconsistent');
}

export async function publishExact({ version, expectedDigest, getManifest, verifyArtifact, copyArtifact }) {
  validateIdentity(version, '0'.repeat(40));
  assert.match(expectedDigest || '', /^sha256:[a-f0-9]{64}$/);
  // Re-check immediately before writing; do not treat permission/network errors as absence.
  await verifyArtifact();
  await requireAbsent(getManifest, version);
  await copyArtifact();
  await requirePublishedDigest(getManifest, version, expectedDigest);
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    assert.equal(process.env.GITHUB_ACTIONS, 'true', 'publication is only available in the reviewed workflow');
    assert.equal(process.env.RUNNER_OS, 'Linux');
    const [layout, archive, receipt, version, sourceSHA, indexDigest, archiveDigest] = process.argv.slice(2);
    validateIdentity(version, sourceSHA);
    const getManifest = await registryClient({ username: process.env.GHCR_USERNAME, password: process.env.GHCR_TOKEN });
    await publishExact({ version, expectedDigest: indexDigest, getManifest,
      verifyArtifact: () => verifyImageArtifact(layout, archive, receipt, version, sourceSHA, indexDigest, archiveDigest),
      copyArtifact: () => execFileSync('skopeo', ['copy', '--all', '--preserve-digests', '--retry-times', '0', '--authfile', process.env.REGISTRY_AUTH_FILE,
        `oci-archive:${resolve(archive)}`, `docker://${imageRepository}:${version}`],
      { stdio: ['ignore', 'pipe', 'pipe'], timeout: 15 * 60 * 1000 }),
    });
    console.log(`Published verified ${imageRepository}:${version}@${indexDigest}.`);
  } catch {
    // Raw HTTP/child-process errors can contain an Authorization header or credential.
    console.error('Image publication refused or could not be verified. Do not retry with overwrite; inspect the exact version/digest and CI receipt.');
    process.exitCode = 1;
  }
}
