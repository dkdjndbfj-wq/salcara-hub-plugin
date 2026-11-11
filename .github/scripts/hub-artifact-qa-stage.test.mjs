// Synthetic tests only: no subprocess, network, token, publisher key or real ZIP.
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { join } from 'node:path';
import { tmpdir } from 'node:os';
import {
  POLICY as P, repoPath, StageFailure, stageOriginalZip, scanTargetReleases,
  listReleaseAssets, validateRun, validateArtifact, validateTag, validateDownload,
  validateAsset, validateRuntime, createArgs,
} from './hub-artifact-qa-stage.mjs';

const copy = value => structuredClone(value);
const goodRun = () => ({
  id: P.runID, run_attempt: 1, status: 'completed', conclusion: 'success',
  event: 'workflow_dispatch', path: '.github/workflows/standalone-release.yml',
  head_sha: P.sourceSHA, head_branch: P.branch,
  repository: { id: P.repoID, full_name: P.repo },
  head_repository: { id: P.repoID, full_name: P.repo },
});
const goodArtifact = () => ({
  id: P.artifactID, name: P.artifactName, size_in_bytes: P.size, digest: P.digest,
  expired: false, archive_download_url: 'https://api.github.com/' + repoPath + '/actions/artifacts/' + P.artifactID + '/zip',
  workflow_run: { id: P.runID, head_sha: P.sourceSHA, head_branch: P.branch, repository_id: P.repoID, head_repository_id: P.repoID },
});
const goodTag = () => ({ ref: 'refs/tags/' + P.tag, object: { type: 'commit', sha: P.sourceSHA } });
const goodFile = () => ({ path: join(tmpdir(), P.assetName), size: P.size, digest: P.digest });
const goodAsset = () => ({ id: 901, name: P.assetName, size: P.size, digest: P.digest, state: 'uploaded' });
const goodRelease = () => ({ id: 801, tag_name: P.tag, draft: true, prerelease: false, published_at: null, target_commitish: 'main', assets: [goodAsset()] });
const goodRuntime = () => ({ GITHUB_ACTIONS: 'true', RUNNER_OS: 'Linux', GITHUB_REPOSITORY: P.repo,
  GITHUB_EVENT_NAME: 'workflow_dispatch', GITHUB_REF: 'refs/heads/main', STAGE_ORIGINAL_ZIP: 'true', GH_TOKEN: 'synthetic-not-a-real-token' });
function fixture() {
  const state = { run: goodRun(), artifact: goodArtifact(), tag: goodTag(), file: goodFile(),
    release: goodRelease(), asset: goodAsset(), created: false, createCalls: [], apiCalls: [], downloads: 0 };
  const dependencies = {
    api: async route => {
      state.apiCalls.push(route);
      if (state.override) {
        const result = await state.override(route, state);
        if (result !== undefined) return copy(result);
      }
      if (route === repoPath + '/actions/runs/' + P.runID) return copy(state.run);
      if (route === repoPath + '/actions/artifacts/' + P.artifactID) return copy(state.artifact);
      if (route === repoPath + '/git/ref/tags/' + P.tag) return copy(state.tag);
      if (route.startsWith(repoPath + '/releases?')) return state.created ? [copy(state.release)] : [];
      if (route === repoPath + '/releases/801') return copy(state.release);
      if (route.startsWith(repoPath + '/releases/801/assets?')) return [copy(state.asset)];
      if (route === repoPath + '/releases/assets/901') return copy(state.asset);
      throw new Error('Unexpected synthetic API route');
    },
    download: async () => { state.downloads++; if (state.downloadHook) await state.downloadHook(); return copy(state.file); },
    create: async args => { state.createCalls.push(args); state.created = true; if (state.createHook) await state.createHook(); },
  };
  return { state, dependencies };
}
async function blockedBeforeCreate(modify) {
  const f = fixture(); modify(f.state);
  await assert.rejects(stageOriginalZip(f.dependencies), error => error instanceof StageFailure && !error.createAttempted);
  assert.equal(f.state.createCalls.length, 0);
}
const otherRows = start => Array.from({ length: 100 }, (_, i) => ({ id: start + i, tag_name: 'older-' + (start + i), draft: false }));

test('successful original ZIP staging is draft-only and verifies by immutable IDs', async () => {
  const f = fixture();
  const result = await stageOriginalZip(f.dependencies);
  assert.equal(result.verified, true); assert.equal(result.draft, true);
  assert.equal(result.signed, false); assert.equal(result.published, false); assert.equal(result.feedChanged, false);
  assert.equal(result.digest, P.digest); assert.equal(result.sourceSHA, P.sourceSHA);
  assert.equal(f.state.createCalls.length, 1);
  assert.ok(f.state.apiCalls.includes(repoPath + '/releases/801'));
  assert.ok(f.state.apiCalls.includes(repoPath + '/releases/801/assets?per_page=100&page=1'));
  assert.ok(f.state.apiCalls.includes(repoPath + '/releases/assets/901'));
  assert.equal(f.state.apiCalls.filter(route => route === repoPath + '/releases/801').length, 2);
  assert.ok(!f.state.apiCalls.some(route => route.includes('/releases/tags/') || route.includes('/latest')));
});
test('create CLI cannot implicitly create tags, overwrite, target another SHA, or publish', () => {
  const args = createArgs(goodFile().path);
  for (const flag of ['--draft', '--verify-tag', '--latest=false']) assert.ok(args.includes(flag));
  for (const flag of ['--target', '--clobber', '--prerelease']) assert.ok(!args.includes(flag));
  assert.equal(args[2], P.tag); assert.equal(args[3], goodFile().path);
});

for (const [label, change] of [
  ['run ID', value => value.id++], ['run attempt', value => value.run_attempt++],
  ['pending run', value => value.status = 'in_progress'], ['failed run', value => value.conclusion = 'failure'],
  ['event', value => value.event = 'push'], ['workflow', value => value.path = '.github/workflows/other.yml'],
  ['source SHA', value => value.head_sha = 'f'.repeat(40)], ['branch', value => value.head_branch = 'main'],
  ['repository ID', value => value.repository.id++], ['repository name', value => value.repository.full_name = 'other/repo'],
  ['head repository ID', value => value.head_repository.id++], ['head repository name', value => value.head_repository.full_name = 'other/repo'],
]) test('rejects wrong ' + label + ' before creating a draft', async () => {
  const value = goodRun(); change(value); assert.throws(() => validateRun(value));
  await blockedBeforeCreate(state => state.run = value);
});
for (const [label, change] of [
  ['artifact ID', value => value.id++], ['name', value => value.name = 'another'],
  ['metadata size', value => value.size_in_bytes++], ['metadata digest', value => value.digest = 'sha256:' + '0'.repeat(64)],
  ['expired artifact', value => value.expired = true], ['download endpoint', value => value.archive_download_url = 'https://example.invalid/file.zip'],
  ['artifact run', value => value.workflow_run.id++], ['artifact source', value => value.workflow_run.head_sha = 'f'.repeat(40)],
  ['artifact branch', value => value.workflow_run.head_branch = 'main'],
  ['artifact repository', value => value.workflow_run.repository_id++],
  ['artifact head repository', value => value.workflow_run.head_repository_id++],
]) test('rejects wrong ' + label + ' before creating a draft', async () => {
  const value = goodArtifact(); change(value); assert.throws(() => validateArtifact(value));
  await blockedBeforeCreate(state => state.artifact = value);
});
for (const [label, change] of [
  ['tag ref', value => value.ref = 'refs/tags/another'],
  ['tag SHA', value => value.object.sha = '0'.repeat(40)],
  ['annotated tag', value => value.object.type = 'tag'],
]) test('rejects ' + label, async () => {
  const value = goodTag(); change(value); assert.throws(() => validateTag(value));
  await blockedBeforeCreate(state => state.tag = value);
});
test('missing tag / API error is not permission to create one', async () => {
  await blockedBeforeCreate(state => state.override = route => { if (route.includes('/git/ref/')) throw new Error('HTTP 404'); });
});
for (const [label, change] of [
  ['short download', value => value.size--], ['oversized download', value => value.size++],
  ['same-length wrong hash', value => value.digest = 'sha256:' + 'f'.repeat(64)],
  ['relative path', value => value.path = P.assetName], ['different filename', value => value.path = join(tmpdir(), 'other.zip')],
]) test('rejects ' + label + ' without remote writes', async () => {
  const value = goodFile(); change(value); assert.throws(() => validateDownload(value));
  await blockedBeforeCreate(state => state.file = value);
});
test('target draft on second page is found and rejected', async () => {
  await blockedBeforeCreate(state => state.override = route => {
    if (route.includes('/releases?') && route.endsWith('page=1')) return otherRows(1000);
    if (route.includes('/releases?') && route.endsWith('page=2')) return [goodRelease()];
  });
});
test('target public release on later page is rejected', async () => {
  await blockedBeforeCreate(state => state.override = route => {
    if (route.includes('/releases?') && route.endsWith('page=1')) return otherRows(1000);
    if (route.includes('/releases?') && route.endsWith('page=2')) return [{ ...goodRelease(), draft: false }];
  });
});
test('pagination failure cannot be treated as no existing draft', async () => {
  await blockedBeforeCreate(state => state.override = route => {
    if (route.includes('/releases?') && route.endsWith('page=1')) return otherRows(1000);
    if (route.includes('/releases?') && route.endsWith('page=2')) throw new Error('HTTP 429');
  });
});
test('exactly full first page requires reading the next empty page', async () => {
  const calls = [];
  const result = await scanTargetReleases(async route => { calls.push(route); return route.endsWith('page=1') ? otherRows(1000) : []; });
  assert.equal(result.pages, 2); assert.equal(calls.length, 2);
});
test('duplicate release page or malformed API shape fails closed', async () => {
  await assert.rejects(scanTargetReleases(async () => otherRows(1000)));
  await assert.rejects(scanTargetReleases(async () => ({ status: '404' })));
});
test('existing draft appearing during transfer is rejected before create', async () => {
  await blockedBeforeCreate(state => state.downloadHook = () => { state.created = true; });
});
test('tag moving during transfer is rejected before create', async () => {
  await blockedBeforeCreate(state => state.downloadHook = () => { state.tag.object.sha = 'f'.repeat(40); });
});
test('artifact changing during transfer is rejected before create', async () => {
  await blockedBeforeCreate(state => state.downloadHook = () => { state.artifact.digest = 'sha256:' + 'f'.repeat(64); });
});
for (const label of ['create response lost', 'upload response lost']) test(label + ' reports possible residual draft and never retries', async () => {
  const f = fixture(); f.state.createHook = () => { throw new Error(label); };
  await assert.rejects(stageOriginalZip(f.dependencies), error =>
    error instanceof StageFailure && error.createAttempted && error.requiresManualInspection
    && error.observation.releases[0].id === 801 && /may remain/.test(error.message));
  assert.equal(f.state.createCalls.length, 1);
});
test('failed create with unavailable inspection still reports uncertainty', async () => {
  const f = fixture();
  f.state.createHook = () => {
    f.state.override = route => { if (route.includes('/releases?')) throw new Error('network unavailable'); };
    throw new Error('lost response');
  };
  await assert.rejects(stageOriginalZip(f.dependencies), error => error.createAttempted && error.observation.status === 'unknown-api-unavailable');
  assert.equal(f.state.createCalls.length, 1);
});
for (const [label, change] of [
  ['asset ID', value => value.id++], ['asset name', value => value.name = 'other.zip'],
  ['asset size', value => value.size++], ['asset digest', value => value.digest = 'sha256:' + '0'.repeat(64)],
  ['missing asset digest', value => delete value.digest], ['unfinished asset', value => value.state = 'starter'],
]) test('post-upload ' + label + ' mismatch stops without retry or deletion', async () => {
  const f = fixture();
  f.state.override = route => { if (route === repoPath + '/releases/assets/901') { const value = goodAsset(); change(value); return value; } };
  await assert.rejects(stageOriginalZip(f.dependencies), error => error.createAttempted && error.requiresManualInspection);
  assert.equal(f.state.createCalls.length, 1);
});
for (const [label, change] of [
  ['public release', value => value.draft = false], ['wrong release tag', value => value.tag_name = 'other'],
  ['wrong release ID', value => value.id++], ['unexpected prerelease', value => value.prerelease = true],
  ['missing publication time', value => delete value.published_at],
  ['previous publication time', value => value.published_at = '2026-11-11T10:00:00Z'],
]) test('post-upload ' + label + ' fails by-ID verification', async () => {
  const f = fixture();
  f.state.override = route => { if (route === repoPath + '/releases/801') { const value = goodRelease(); change(value); return value; } };
  await assert.rejects(stageOriginalZip(f.dependencies), error => error.createAttempted);
  assert.equal(f.state.createCalls.length, 1);
});
test('publication during asset inspection is caught by final by-ID check', async () => {
  const f = fixture();
  f.state.override = route => { if (route === repoPath + '/releases/assets/901') f.state.release.draft = false; };
  await assert.rejects(stageOriginalZip(f.dependencies), error => error.createAttempted);
  assert.equal(f.state.createCalls.length, 1);
});
test('publication timestamp added during asset inspection fails the final by-ID check', async () => {
  const f = fixture();
  f.state.override = route => { if (route === repoPath + '/releases/assets/901') f.state.release.published_at = '2026-11-11T10:00:00Z'; };
  await assert.rejects(stageOriginalZip(f.dependencies), error => error.createAttempted && error.requiresManualInspection);
  assert.equal(f.state.createCalls.length, 1);
});
test('asset pagination is complete and duplicate pages fail', async () => {
  const calls = [];
  const assets = await listReleaseAssets(async route => {
    calls.push(route); return route.endsWith('page=1') ? Array.from({ length: 100 }, (_, i) => ({ id: i + 1 })) : [{ id: 901 }];
  }, 801);
  assert.equal(assets.length, 101); assert.equal(calls.length, 2);
  await assert.rejects(listReleaseAssets(async () => Array.from({ length: 100 }, (_, i) => ({ id: i + 1 })), 801));
});
test('extra remote assets are not silently accepted', async () => {
  const f = fixture();
  f.state.override = route => route.includes('/releases/801/assets?') ? [goodAsset(), { ...goodAsset(), id: 902 }] : undefined;
  await assert.rejects(stageOriginalZip(f.dependencies), error => error.createAttempted);
});
test('runtime cannot run locally or from unreviewed events/refs/repositories', () => {
  validateRuntime(goodRuntime());
  for (const field of ['GITHUB_ACTIONS', 'RUNNER_OS', 'GITHUB_REPOSITORY', 'GITHUB_EVENT_NAME', 'GITHUB_REF', 'STAGE_ORIGINAL_ZIP', 'GH_TOKEN']) {
    const env = goodRuntime(); delete env[field]; assert.throws(() => validateRuntime(env));
  }
  for (const field of ['GH_DEBUG', 'DEBUG']) assert.throws(() => validateRuntime({ ...goodRuntime(), [field]: 'api' }));
});
test('workflow has manual opt-in and only Actions-read / contents-write permissions', () => {
  const file = readFileSync(new URL('../workflows/hub-artifact-qa-stage.yml', import.meta.url), 'utf8');
  assert.match(file, /workflow_dispatch:/); assert.match(file, /default: false/);
  assert.match(file, /permissions: \{\}/); assert.match(file, /actions: read/); assert.match(file, /contents: write/);
  assert.match(file, /cancel-in-progress: false/); assert.match(file, /persist-credentials: false/);
  assert.match(file, /node --test/);
  assert.doesNotMatch(file, /packages:|publisher|private.key|standalone-feed|release edit|clobber|push:|pull_request:|schedule:/i);
  assert.deepEqual([...file.matchAll(/secrets\.([A-Za-z0-9_]+)/g)].map(item => item[1]), ['GITHUB_TOKEN']);
});
test('policy pins exactly the reviewed origin and original ZIP, with no user-selectable release', () => {
  assert.ok(Object.isFrozen(P));
  assert.equal(P.runID, 42995566424); assert.equal(P.artifactID, 13845380631);
  assert.equal(P.sourceSHA, '7d4e99ea1b3ae70442060f478f739b514ea03a91');
  assert.equal(P.size, 137155702);
  assert.equal(P.digest, 'sha256:143738c211d04858237a6f4ce763fb82bc3fa29d79fa5aae15d9384335e94986');
  validateAsset(goodAsset(), 901);
});
