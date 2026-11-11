// One-use QA transport only. No rebuilding, signing, publishing, feed or image writes.
import assert from 'node:assert/strict';
import { execFileSync, spawn } from 'node:child_process';
import { createHash } from 'node:crypto';
import { createWriteStream, mkdtempSync, renameSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { basename, isAbsolute, join, resolve } from 'node:path';
import { pipeline } from 'node:stream/promises';
import { fileURLToPath } from 'node:url';

export const POLICY = Object.freeze({
  repo: 'dkdjndbfj-wq/salcara-hub-plugin', repoID: 1398411975,
  runID: 42995566424, runAttempt: 1, artifactID: 13845380631,
  sourceSHA: '7d4e99ea1b3ae70442060f478f739b514ea03a91',
  branch: 'codex/hub-release-0.5.5-20261111',
  artifactName: 'standalone-release-0.5.5', size: 137155702,
  digest: 'sha256:143738c211d04858237a6f4ce763fb82bc3fa29d79fa5aae15d9384335e94986',
  tag: 'standalone-v0.5.5',
  assetName: 'standalone-release-0.5.5-actions-original.zip',
});
export const repoPath = 'repos/' + POLICY.repo;
const positiveID = value => Number.isSafeInteger(value) && value > 0;

export function validateRun(run) {
  assert.equal(run.id, POLICY.runID, 'wrong run');
  assert.equal(run.run_attempt, POLICY.runAttempt, 'unexpected rerun');
  assert.equal(run.status, 'completed', 'run is incomplete');
  assert.equal(run.conclusion, 'success', 'run failed');
  assert.equal(run.event, 'workflow_dispatch', 'unexpected event');
  assert.equal(run.path, '.github/workflows/standalone-release.yml', 'wrong source workflow');
  assert.equal(run.head_sha, POLICY.sourceSHA, 'wrong source SHA');
  assert.equal(run.head_branch, POLICY.branch, 'wrong source branch');
  for (const repository of [run.repository, run.head_repository]) {
    assert.equal(repository?.id, POLICY.repoID, 'wrong repository');
    assert.equal(repository?.full_name, POLICY.repo, 'wrong repository name');
  }
}
export function validateArtifact(artifact) {
  assert.equal(artifact.id, POLICY.artifactID, 'wrong artifact');
  assert.equal(artifact.name, POLICY.artifactName, 'wrong artifact name');
  assert.equal(artifact.size_in_bytes, POLICY.size, 'wrong artifact size');
  assert.equal(artifact.digest, POLICY.digest, 'wrong artifact digest');
  assert.equal(artifact.expired, false, 'artifact expired');
  assert.equal(artifact.archive_download_url, 'https://api.github.com/' + repoPath + '/actions/artifacts/' + POLICY.artifactID + '/zip', 'wrong download endpoint');
  const run = artifact.workflow_run;
  assert.equal(run?.id, POLICY.runID, 'artifact belongs to another run');
  assert.equal(run.head_sha, POLICY.sourceSHA, 'artifact source mismatch');
  assert.equal(run.head_branch, POLICY.branch, 'artifact branch mismatch');
  assert.equal(run.repository_id, POLICY.repoID, 'artifact repository mismatch');
  assert.equal(run.head_repository_id, POLICY.repoID, 'artifact head repository mismatch');
}
export function validateTag(ref) {
  assert.equal(ref.ref, 'refs/tags/' + POLICY.tag, 'wrong tag ref');
  assert.equal(ref.object?.type, 'commit', 'owner must pre-create the exact lightweight tag');
  assert.equal(ref.object.sha, POLICY.sourceSHA, 'tag must point to the frozen source');
}
export function validateDownload(file) {
  assert.ok(isAbsolute(file.path), 'download path must be absolute');
  assert.equal(basename(file.path), POLICY.assetName, 'wrong staged filename');
  assert.equal(file.size, POLICY.size, 'download size mismatch');
  assert.equal(file.digest, POLICY.digest, 'download hash mismatch');
}
export function validateRuntime(env) {
  assert.equal(env.GITHUB_ACTIONS, 'true', 'reviewed Actions runtime required');
  assert.equal(env.RUNNER_OS, 'Linux', 'Linux runner required');
  assert.equal(env.GITHUB_REPOSITORY, POLICY.repo, 'wrong execution repository');
  assert.equal(env.GITHUB_EVENT_NAME, 'workflow_dispatch', 'manual dispatch required');
  assert.equal(env.GITHUB_REF, 'refs/heads/main', 'reviewed default-branch helper required');
  assert.equal(env.STAGE_ORIGINAL_ZIP, 'true', 'explicit staging confirmation required');
  assert.ok(env.GH_TOKEN, 'ephemeral Actions token required');
  assert.ok(!env.GH_DEBUG && !env.DEBUG, 'debug credential logging is forbidden');
}

// Authenticated listing includes drafts. A pagination failure is never absence.
export async function scanTargetReleases(api) {
  const matches = [], seen = new Set();
  for (let page = 1; page <= 100; page++) {
    const rows = await api(repoPath + '/releases?per_page=100&page=' + page);
    assert.ok(Array.isArray(rows) && rows.length <= 100, 'invalid release page');
    for (const row of rows) {
      assert.ok(positiveID(row.id) && typeof row.tag_name === 'string' && typeof row.draft === 'boolean', 'invalid release listing');
      assert.ok(!seen.has(row.id), 'release pagination changed or repeated');
      seen.add(row.id);
      if (row.tag_name === POLICY.tag) matches.push(row);
    }
    if (rows.length < 100) return { matches, pages: page };
  }
  throw new Error('release listing exceeded the bounded complete-pagination limit');
}
export function createArgs(path) {
  return ['release', 'create', POLICY.tag, path, '--repo', POLICY.repo,
    '--draft', '--verify-tag', '--latest=false', '--title', 'Salcara Hub 0.5.5 — QA staging draft',
    '--notes', 'QA transport of original Actions run 42995566424 artifact 13845380631. Not a public release. Local ZIP/OCI/binary/signature verification and coordinated publication approval remain required. Do not publish or delete this QA asset without coordinator approval.'];
}
export function validateAsset(asset, id) {
  assert.ok(positiveID(id));
  assert.equal(asset.id, id, 'asset ID mismatch');
  assert.equal(asset.name, POLICY.assetName, 'asset name mismatch');
  assert.equal(asset.state, 'uploaded', 'asset upload incomplete');
  assert.equal(asset.size, POLICY.size, 'remote asset size mismatch');
  assert.equal(asset.digest, POLICY.digest, 'remote asset digest mismatch');
}
export function validateDraft(release, id) {
  assert.equal(release.id, id, 'release ID mismatch');
  assert.equal(release.tag_name, POLICY.tag, 'release tag mismatch');
  assert.equal(release.draft, true, 'release must remain draft');
  assert.equal(release.prerelease, false, 'unexpected prerelease state');
  assert.equal(release.published_at, null, 'draft must explicitly have no publication time');
}
export async function listReleaseAssets(api, releaseID) {
  assert.ok(positiveID(releaseID), 'invalid release ID');
  const assets = [], seen = new Set();
  for (let page = 1; page <= 100; page++) {
    const rows = await api(repoPath + '/releases/' + releaseID + '/assets?per_page=100&page=' + page);
    assert.ok(Array.isArray(rows) && rows.length <= 100, 'invalid asset page');
    for (const row of rows) {
      assert.ok(positiveID(row.id) && !seen.has(row.id), 'invalid or repeated asset ID');
      seen.add(row.id);
      assets.push(row);
    }
    if (rows.length < 100) return assets;
  }
  throw new Error('asset listing exceeded the complete-pagination limit');
}
export class StageFailure extends Error {
  constructor(createAttempted, observation) {
    super(createAttempted
      ? 'Draft creation/upload was attempted; a draft or asset may remain. Do not retry or delete; inspect the recorded state manually.'
      : 'QA staging preflight or byte verification failed; no draft creation was attempted.');
    this.createAttempted = createAttempted;
    this.requiresManualInspection = createAttempted;
    this.observation = observation;
  }
}

export async function stageOriginalZip({ api, download, create }) {
  let createAttempted = false;
  try {
    validateRun(await api(repoPath + '/actions/runs/' + POLICY.runID));
    validateArtifact(await api(repoPath + '/actions/artifacts/' + POLICY.artifactID));
    validateTag(await api(repoPath + '/git/ref/tags/' + POLICY.tag));
    assert.equal((await scanTargetReleases(api)).matches.length, 0, 'target Release or draft already exists');
    const file = await download();
    validateDownload(file);
    // Recheck immediately before the only mutation. No tag creation is permitted here.
    validateRun(await api(repoPath + '/actions/runs/' + POLICY.runID));
    validateArtifact(await api(repoPath + '/actions/artifacts/' + POLICY.artifactID));
    validateTag(await api(repoPath + '/git/ref/tags/' + POLICY.tag));
    assert.equal((await scanTargetReleases(api)).matches.length, 0, 'target appeared while downloading');
    createAttempted = true;
    await create(createArgs(file.path));
    const observed = (await scanTargetReleases(api)).matches;
    assert.equal(observed.length, 1, 'cannot uniquely identify created draft');
    const releaseID = observed[0].id;
    const release = await api(repoPath + '/releases/' + releaseID);
    validateDraft(release, releaseID);
    const assets = await listReleaseAssets(api, releaseID);
    assert.equal(assets.length, 1, 'exactly one QA asset is allowed');
    const assetID = assets[0].id;
    validateAsset(assets[0], assetID);
    validateAsset(await api(repoPath + '/releases/assets/' + assetID), assetID);
    // --target is deliberately absent: the verified immutable tag, not
    // target_commitish's default-branch display value, identifies the source.
    validateTag(await api(repoPath + '/git/ref/tags/' + POLICY.tag));
    validateDraft(await api(repoPath + '/releases/' + releaseID), releaseID);
    return { verified: true, draft: true, releaseID, assetID, sourceSHA: POLICY.sourceSHA,
      originalRunID: POLICY.runID, originalArtifactID: POLICY.artifactID,
      assetName: POLICY.assetName, size: POLICY.size, digest: POLICY.digest,
      signed: false, published: false, feedChanged: false };
  } catch {
    let observation = { status: 'not-attempted' };
    if (createAttempted) {
      try {
        const found = await scanTargetReleases(api);
        observation = { status: 'read-only-listing', releases: found.matches.map(row => ({ id: row.id, draft: row.draft })) };
      } catch { observation = { status: 'unknown-api-unavailable' }; }
    }
    throw new StageFailure(createAttempted, observation);
  }
}

function apiJSON(route) {
  try {
    return JSON.parse(execFileSync('gh', ['api', route], {
      encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'], timeout: 30000, maxBuffer: 16 << 20,
    }));
  } catch { throw new Error('GitHub API request failed; raw errors and signed URLs are not logged'); }
}
async function downloadOriginalZip() {
  const directory = mkdtempSync(join(tmpdir(), 'salcara-hub-qa-original-'));
  const path = join(directory, POLICY.assetName), partial = path + '.partial';
  const hash = createHash('sha256');
  let size = 0, timedOut = false;
  const child = spawn('gh', ['api', repoPath + '/actions/artifacts/' + POLICY.artifactID + '/zip'],
    { stdio: ['ignore', 'pipe', 'pipe'] });
  child.stderr.resume(); // Signed redirect URLs must not reach the Actions log.
  const exited = new Promise((resolveExit, reject) => {
    child.once('error', () => reject(new Error('Download process unavailable')));
    child.once('close', resolveExit);
  });
  child.stdout.on('data', chunk => { size += chunk.length; hash.update(chunk); if (size > POLICY.size) child.kill(); });
  const timer = setTimeout(() => { timedOut = true; child.kill(); }, 10 * 60 * 1000);
  try {
    const [code] = await Promise.all([exited, pipeline(child.stdout, createWriteStream(partial, { flags: 'wx', mode: 0o600 }))]);
    assert.ok(!timedOut && code === 0, 'original ZIP transfer did not complete');
    const file = { path, size, digest: 'sha256:' + hash.digest('hex') };
    validateDownload(file);
    renameSync(partial, path); // Both paths are inside this freshly-created private directory.
    return file;
  } finally { clearTimeout(timer); child.kill(); }
}

if (process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url)) {
  try {
    validateRuntime(process.env);
    assert.equal(process.argv.length, 2, 'no variable release inputs are accepted');
    const result = await stageOriginalZip({
      api: apiJSON, download: downloadOriginalZip,
      create: args => {
        try { execFileSync('gh', args, { stdio: ['ignore', 'pipe', 'pipe'], timeout: 10 * 60 * 1000, maxBuffer: 1 << 20 }); }
        catch { throw new Error('Draft creation/upload did not finish conclusively'); }
      },
    });
    console.log(JSON.stringify(result));
  } catch (error) {
    console.error(JSON.stringify(error instanceof StageFailure
      ? { verified: false, message: error.message, createAttempted: error.createAttempted, requiresManualInspection: error.requiresManualInspection, observation: error.observation }
      : { verified: false, message: 'QA staging runtime checks failed; no remote operation was authorized.' }));
    process.exitCode = 1;
  }
}
