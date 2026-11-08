// Synthetic, non-runnable image data shared by unit and opt-in real-tool tests.
import { createHash } from 'node:crypto';
import { mkdirSync, writeFileSync } from 'node:fs';
import { join } from 'node:path';
import { gzipSync } from 'node:zlib';

const digest = bytes => `sha256:${createHash('sha256').update(bytes).digest('hex')}`;
const indexType = 'application/vnd.oci.image.index.v1+json';
const manifestType = 'application/vnd.oci.image.manifest.v1+json';
const configType = 'application/vnd.oci.image.config.v1+json';

export function fixtureTar(entries) {
  const parts = [];
  for (const { name, data: value } of entries) {
    if (Buffer.byteLength(name) > 100) throw new Error('fixture entry name too long');
    const data = Buffer.from(value);
    const header = Buffer.alloc(512);
    header.write(name, 0);
    for (const offset of [100, 108, 116]) header.write((offset === 100 ? '0000644' : '0000000') + '\0', offset);
    header.write(data.length.toString(8).padStart(11, '0') + '\0', 124);
    header.write('00000000000\0', 136);
    header.fill(32, 148, 156);
    header.write('0', 156); header.write('ustar\0', 257); header.write('00', 263);
    header.write([...header].reduce((sum, byte) => sum + byte, 0).toString(8).padStart(6, '0') + '\0 ', 148);
    parts.push(header, data, Buffer.alloc((512 - data.length % 512) % 512));
  }
  return Buffer.concat([...parts, Buffer.alloc(1024)]);
}

export function createOCIFixture(root, options = {}) {
  const version = options.version || '0.5.3';
  const layout = join(root, 'layout');
  const blobs = join(layout, 'blobs', 'sha256');
  mkdirSync(blobs, { recursive: true });
  writeFileSync(join(layout, 'oci-layout'), JSON.stringify({ imageLayoutVersion: '1.0.0' }));
  const put = (data, mediaType, extra = {}) => {
    const bytes = Buffer.isBuffer(data) ? data : Buffer.from(JSON.stringify(data));
    const hash = digest(bytes);
    writeFileSync(join(blobs, hash.slice(7)), bytes);
    return { mediaType, digest: hash, size: bytes.length, ...extra };
  };
  const manifests = [];
  const platforms = {};
  for (const arch of ['amd64', 'arm64']) {
    const uncompressed = fixtureTar([{ name: 'synthetic-release-fixture.txt', data: `Synthetic ${arch} release fixture; no executable or credential.\n` }]);
    const layer = put(gzipSync(uncompressed), 'application/vnd.oci.image.layer.v1.tar+gzip');
    const config = put({ architecture: arch, os: 'linux', config: {
      User: options.rootUser ? '0' : '65532:65532', Entrypoint: ['/salcara-hub-launcher'],
      Labels: { 'org.opencontainers.image.version': options.wrongVersion ? '0.5.2' : version, 'salcara.test': 'release-oci' },
    }, rootfs: { type: 'layers', diff_ids: [digest(uncompressed)] } }, configType);
    const runtime = put({ schemaVersion: 2, mediaType: manifestType, config, layers: [layer] }, manifestType, {
      platform: { os: 'linux', architecture: options.duplicateArch ? 'amd64' : arch },
    });
    manifests.push(runtime);
    platforms[arch] = { manifest_digest: runtime.digest, config_digest: config.digest };
    const types = ['https://slsa.dev/provenance/v0.2', 'https://spdx.dev/Document']
      .filter(type => !(options.noSBOM && type.includes('spdx')) && !(options.noProvenance && type.includes('slsa')));
    const layers = types.map(type => put({ _type: 'https://in-toto.io/Statement/v0.1', predicateType: type, predicate: {},
      subject: options.noSubject ? [] : [{ name: '_', digest: { sha256: options.wrongSubject ? 'c'.repeat(64) : runtime.digest.slice(7) } }],
    },
      'application/vnd.in-toto+json', { annotations: { 'in-toto.io/predicate-type': options.badPredicate ? 'wrong' : type } }));
    manifests.push(put({ schemaVersion: 2, mediaType: manifestType,
      config: put({ architecture: 'unknown', os: 'unknown', rootfs: { type: 'layers', diff_ids: [] } }, configType), layers,
    }, manifestType, { platform: { architecture: 'unknown', os: 'unknown' }, annotations: {
      'vnd.docker.reference.type': 'attestation-manifest',
      'vnd.docker.reference.digest': options.unboundAttestation ? `sha256:${'b'.repeat(64)}` : runtime.digest,
    } }));
  }
  const image = put({ schemaVersion: 2, mediaType: indexType, manifests }, indexType, {
    annotations: { 'org.opencontainers.image.ref.name': 'release' },
  });
  writeFileSync(join(layout, 'index.json'), JSON.stringify({ schemaVersion: 2, manifests: [image] }));
  const archive = join(root, 'image.tar');
  // Unit tests use fake archive bytes; the real-tool test replaces them via skopeo.
  if (!options.realTools) writeFileSync(archive, 'Synthetic archive bytes for receipt tests; never passed to a tool.');
  return { root, layout, blobs, archive, indexDigest: image.digest, platforms };
}
