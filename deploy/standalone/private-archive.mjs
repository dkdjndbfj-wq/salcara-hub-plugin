// Verify source permissions from docker cp's TAR, not Windows' synthesized
// file mode after extraction. This inspects metadata only, never prints content.
import assert from 'node:assert/strict';
import { execFileSync } from 'node:child_process';
import { join } from 'node:path';

export function assertPrivateArchiveListing(listing, filename) {
  assert.match(filename, /^[a-z][a-z0-9-]*\.txt$/, 'expected a fixed credential fixture name');
  const lines = listing.trim().split(/\r?\n/).filter(Boolean);
  assert.equal(lines.length, 1, 'credential copy must contain exactly one file');
  assert.match(lines[0], /^-rw-------\s/, 'container credential must be a regular 0600 file');
  assert.ok(lines[0].endsWith(` ${filename}`), 'credential archive filename must match');
}

export function assertPrivateArchive(archive, filename) {
  assert.ok(Buffer.isBuffer(archive) && archive.length <= 64 * 1024, 'credential fixture archive must be bounded');
  const tar = process.platform === 'win32'
    ? join(process.env.SystemRoot || 'C:\\Windows', 'System32', 'tar.exe') : 'tar';
  let listing;
  try {
    listing = execFileSync(tar, ['-tvf', '-'], {
      input: archive, encoding: 'utf8', stdio: ['pipe', 'pipe', 'pipe'],
      timeout: 10000, maxBuffer: 64 * 1024,
    });
  } catch {
    throw new Error('Unable to inspect isolated credential archive metadata');
  }
  assertPrivateArchiveListing(listing, filename);
}
