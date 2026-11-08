import assert from 'node:assert/strict';
import { test } from 'node:test';
import { assertPrivateArchive, assertPrivateArchiveListing } from './private-archive.mjs';

const filename = 'admin-initial-login.txt';
for (const listing of [
  '-rw------- 65532/65532         44 2026-10-14 12:00 admin-initial-login.txt\n',
  '-rw-------  0 65532  65532  44 Oct 14 12:00 admin-initial-login.txt\r\n',
]) {
  test('accepts a single private credential file in GNU/BSD metadata', () => assertPrivateArchiveListing(listing, filename));
}
for (const listing of [
  '-rw-r--r-- 65532/65532 44 2026-10-14 12:00 admin-initial-login.txt',
  '-rw------- 65532/65532 44 2026-10-14 12:00 other.txt',
  'lrw------- 65532/65532 44 2026-10-14 12:00 admin-initial-login.txt -> other.txt',
  'drw------- 65532/65532 0 2026-10-14 12:00 admin-initial-login.txt',
  '-rw------- 65532/65532 44 2026-10-14 12:00 admin-initial-login.txt\n-rw------- 65532/65532 0 2026-10-14 12:00 extra.txt',
  '',
]) {
  test(`rejects unsafe fixture metadata ${JSON.stringify(listing)}`, () => assert.throws(() => assertPrivateArchiveListing(listing, filename)));
}

function fixtureTar(mode) {
  const header = Buffer.alloc(512);
  const field = (offset, size, value) => header.write(value.padStart(size - 1, '0') + '\0', offset, size, 'ascii');
  header.write(filename, 0, 100, 'ascii');
  field(100, 8, mode.toString(8)); field(108, 8, '0'); field(116, 8, '0');
  field(124, 12, '0'); field(136, 12, '0');
  header.fill(32, 148, 156); header[156] = 48;
  header.write('ustar\0', 257, 'ascii'); header.write('00', 263, 'ascii');
  const checksum = header.reduce((sum, value) => sum + value, 0);
  header.write(checksum.toString(8).padStart(6, '0') + '\0 ', 148, 8, 'ascii');
  return Buffer.concat([header, Buffer.alloc(1024)]);
}

test('actual system tar validates source permissions without relying on host filesystem modes', () => {
  assertPrivateArchive(fixtureTar(0o600), filename);
  assert.throws(() => assertPrivateArchive(fixtureTar(0o644), filename), /0600/);
});

test('malformed and oversized archives are rejected without content in diagnostics', () => {
  assert.throws(() => assertPrivateArchive(Buffer.alloc(65537), filename), /bounded/);
  assert.throws(() => assertPrivateArchive(Buffer.from('synthetic-private-content'), filename), error =>
    error.message === 'Unable to inspect isolated credential archive metadata');
});
