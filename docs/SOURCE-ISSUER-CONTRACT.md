# Remote source issuer contract

Device enrollment has a separate opt-in v1 protocol in the next-source
implementation; see [Station source issuer service](SOURCE-ISSUER-SERVICE-20261015.md#opt-in-device-enrollment-v1).
The command-bound `SourceSnapshot` below cannot be exchanged for a new-device
identity or QR phone authorization. An existing command issuer is not
implicitly trusted to enroll devices, and its request/response formats remain
unchanged.

The Hub verifies short-lived, command-bound Ed25519 grants. This repository
does not implement a Sub2API account verifier or certify a local account by
its editable name, `SourceType`, `ProviderID`, a successful `/models` request,
or the presence of an official-account login file.

## Operator trust and public discovery

`RemoteSourceDiscovery()` returns an isolated public `RemoteAccessPolicy`
snapshot: mode, current revision, audience, and configured issuer public
keys/scopes/authorization endpoints. A transport should expose that snapshot
on the authenticated, exact paired Hub connection. An endpoint from phone
command JSON, a redirect, or a different Hub must never replace the configured
issuer endpoint.

The acquisition transport is implemented by `source.grant.acquire.v1`. The
phone posts `{deviceId, command, requestId}` to `/app/source/challenge` using
its paired connection. The challenge expires after two minutes; the Hub's
minute janitor removes expired challenges. The Hub returns only
`{challengeId, expiresAt}`. The phone then
posts `{challengeId}` to `/app/source/grant`. The Hub dispatches only the
internal `source.grant.acquire` command; callers cannot run that command via
the ordinary `/app/commands` route. Bridge retrieves the stored challenge and
current public policy from `/bridge/source/challenge?challengeId=...` using
both its device ID and independent device secret. Retrieval is permitted only
while that exact paired acquisition is active. The stored command is cleared
after a valid grant is obtained. Challenge state is bounded and is never
persisted; a restart requires new acquisition. The signed snapshot includes
the exact `challengeId` nonce alongside the existing binding fields. Final
execution uses durable request receipts, so the same authorized command and
request ID cannot execute twice. A completed, cached receipt retires its
challenge immediately; authenticated retries recover that receipt even after
the challenge or grant has expired. Legacy version-one grants without a challenge
remain accepted by the pre-existing direct submission integration, while all
grants obtained by these new acquisition routes must carry the nonce.

An issuer may register:

```json
{
  "id": "station-a",
  "publicKey": "BASE64URL_ED25519_PUBLIC_KEY",
  "sourceTypes": ["site_api", "verified_proxy"],
  "agentIds": ["codex", "claude"],
  "origins": ["https://api.example.test"],
  "apiBases": ["https://api.example.test/tenant-a"],
  "authorizationUrl": "https://api.example.test/source-grants"
}
```

These example hosts and paths describe the integration contract, not existing
Sub2API endpoints. `authorizationUrl` is optional and must be canonical HTTPS;
its presence is discovery metadata, not evidence of an implemented verifier.
Missing `origins`/`apiBases` retain existing operator trust for compatibility.
Deployments that require registered station isolation should configure both.

The generic Bridge acquisition adapter requires **exactly one** issuer with
a canonical HTTPS authorization URL, `site_api` source type, and explicit
matching `origins` and `apiBases` scopes. Ambiguous/missing scopes or endpoints
fail closed. The authorization URL's origin must also equal the selected
account's actual normalized origin. A Hub-controlled scope list cannot
authorize sending another provider's key to a foreign verifier. Cross-origin
verification requires a separately trusted local connector/consent mechanism,
which this generic adapter does not provide. The API handle resolves against the current Bridge vault;
`remote.station.switch` resolves against its already-saved target station.
The adapter only supports a vault `api_key` credential. Official-account,
OAuth, reverse-proxy and native Desktop source proofs remain unsupported
until a real connector is implemented and verified. No native auth files are
read by acquisition.

Origins contain only scheme/authority, lowercase host, and an optional
nondefault port. HTTP origins are limited to loopback. API bases retain exact
path prefixes and omit trailing slash and `/v1`; matching is exact, never a
URL-prefix check. Encoded/dot path variants, URL credentials, query strings,
fragments, and duplicate scopes are rejected. A grant for a scoped API base
must include exactly that `apiBase`, with the same `origin`.

## Per-command authorization flow

1. The phone freezes the complete command and a standard lowercase request
   UUID. A newly acquired grant is bound to that command and UUID, not merely
   to the API picker selection.
2. An authenticated acquisition adapter resolves the handle against the
   current Bridge vault snapshot. The Bridge derives `origin`, `apiBase`, and
   an opaque configuration revision from the actual selected account. The
   revision must not expose an unsalted digest of an API key.
3. The Bridge contacts the operator-configured HTTPS verifier/issuer directly.
   The API credential or official-account authentication proof stays on that
   direct connection; it must not cross Hub/mobile messages, receipts, events,
   logs, or source snapshots. Redirects and caller-chosen verifier endpoints
   are rejected. Bounds, timeouts, and HTTPS certificate checks are required.
4. The real verifier authenticates the credential against its authoritative
   account store or connector and establishes the actual registered execution
   API base, source category, and allowed audience. It must not accept those
   claims solely because the Bridge supplied them. A valid station-A key is
   insufficient to authorize execution against station B or a privileged
   localhost proxy. A local proxy needs its own registered connector whose
   proof binds the fixed upstream account and actual execution route.
5. Only after that proof succeeds may the issuer sign the complete
   `SourceSnapshot`: version, issuer, audience, device/binding, handle,
   agent/control surface, source type, origin/API base, opaque config revision,
   policy revision, command hash, request UUID, optional model, and timestamps.
   The Hub checks the issuer's registered scope and the Bridge rechecks that
   the envelope matches the actual vault account and execution snapshot.

The existing command hash is SHA-256 of the Hub's canonical command JSON
(`canonicalCommand`, using Go JSON numbers). Acquisition should compute it in
the same Go implementation; a separate mobile serializer is not a substitute
for the complete signed command. Signature input is the exact UTF-8 snapshot
payload. Payload/signature use unpadded base64url. `ParseSignedSourceGrant` and
`ParseSourceSnapshot` reject duplicate, case-confusable, unknown, null, and
trailing fields; issuance adapters should use these parsers too.

Private signing keys belong to the trusted issuer runtime, never this public
policy file or phone/Bridge distribution. A configurable HTTPS verifier is a
reusable adapter boundary; its real account API, authentication semantics,
revocation behavior, and proof format must be supplied and tested by the
provider. No production account verifier can be inferred from this contract.

### Generic HTTPS adapter wire contract

Bridge sends one direct `POST authorizationUrl`, without any phone token,
Hub API key, or device secret in the headers or body:

```json
{
  "version": 1,
  "snapshot": {
    "version": 1,
    "issuer": "station-a",
    "audience": "hub-a",
    "deviceId": "paired-device",
    "bindingId": "current-binding",
    "credentialHandle": "opaque-handle",
    "agent": "codex",
    "controlSurface": "cli",
    "sourceType": "site_api",
    "origin": "https://api.example.test",
    "apiBase": "https://api.example.test/tenant-a",
    "configRevision": "opaque-config-revision",
    "policyRevision": "current-policy-revision",
    "commandHash": "SHA256_OF_FROZEN_CANONICAL_COMMAND",
    "requestId": "10000000-0000-4000-8000-000000000001",
    "challengeId": "challenge-HUB_NONCE",
    "issuedAt": 0,
    "expiresAt": 0
  },
  "credential": { "type": "api_key", "value": "PRIVATE_API_KEY" }
}
```

This is an integration specification, not a known Sub2API API. A provider
must implement that endpoint and authenticate the supplied credential against
its actual authoritative account state, derive/verify the requested registered
route, source class and audience, and enforce revocation. The request snapshot
contains requested public claims and is not evidence that those claims are
true. Only then may it return HTTP 200 with the exact
`{"payload":"BASE64URL_SNAPSHOT","signature":"BASE64URL_ED25519_SIGNATURE"}`
envelope. The signed snapshot must match every requested field, with fresh
`issuedAt`/`expiresAt` values (at most five minutes). The generic adapter rejects
redirects, invalid TLS certificates, responses over 24 KiB, ambiguous JSON,
invalid signatures and any changed snapshot field. Its direct request has a
12-second deadline and acquisition performs no model request. Issuer response
bodies and transport errors are never reflected into phone messages or logs.

## Lifecycle and compatibility

Grants last at most five minutes and require the current policy revision.
Every runtime mode change gets a new random epoch, even if a caller supplies
an old revision. Standalone commits mode/revision/baseRevision atomically in
`remote-access-mode`, restores the epoch on restart, and accepts older
single-line mode files. Deployment revision changes take precedence over a
saved epoch. Environment mode overrides remain operator-controlled and locked
against admin changes; deployments changing those overrides must rotate the
deployment policy revision too.

Grant TTL and Hub policy apply at admission/enqueue. Already delivered tasks
continue, while the Bridge still validates current account configuration at
execution. Completed authenticated receipts can be recovered after a grant
expires without executing again. Restricted execution requires durable
request receipts. Read-only `session.describe` joins the existing read/safety
commands and remains usable when remote execution is disabled.

Open mode skips grant eligibility. Existing managed-account/handle consistency
checks remain Bridge responsibilities. Restricted native Desktop execution
remains unavailable until the actual provider/route can be proven. A named
official account is not an exemption from the verifier contract.

## Persistence boundary

The Hub data directory also contains the numeric `event-sequence` high-water
reservation. Backups and migrations must retain it together with `devices.json`
and command receipts, and the same data directory must not be opened by two Hub
instances at once. It contains no conversation content or credentials.

## 2026-10-14 connector implementation update

The unsupported-source paragraph above records the original generic adapter
baseline. The current Bridge implements two explicit managed account paths:

- Protected OpenAI official accounts use the authenticated challenge reply,
  without disclosing provider tokens. If the station explicitly enables
  `allowOfficialAccounts`, the Hub signs a short-lived `station-official`
  snapshot after checking the current pairing, nonce, command, policy, account
  handle and execution route. The built-in connector supports only the existing
  managed `codex` and `claude` CLI routes. Claude Messages are translated into
  Responses by the same account-bound gateway executor. The Hub restart
  invalidates these grants because its signing key is process-local.
- API-key-authenticated station proxies may acquire `verified_proxy` grants.
  The unique registered issuer must explicitly permit `verified_proxy`, the
  agent, the exact origin and full API base. The authorization endpoint must
  remain canonical HTTPS on that same origin. The request credential retains
  `type: api_key`; the requested snapshot uses `sourceType: verified_proxy`.
  A local source label is only a requested category. The real issuer must
  authenticate and verify it before signing, and the Hub's independent station
  scope still applies even if that issuer has wider API scopes.

Normal OAuth and Token/JSON imports create protected official-account vault
references with an identity epoch; an arbitrary `oauth` label, web session or
native auth file is not accepted by these connectors. Acquisition rechecks
the account, pairing and station after network waits, and execution rechecks
the account revision and process-bound gateway authority. Native Desktop
provider attestation and arbitrary local proxies without a registered verifier
remain unsupported. A deployment must still implement its real external
issuer endpoint; this document does not invent a Sub2API verification API.

The official-account mode authorizes a managed computer worker through the
selected station. Provider inference traffic is sent by that computer's fixed
protected executor, not relayed through this Hub. No provider token is stored
or forwarded by the Hub. Synthetic connector and station-boundary tests are
included in both Bridge and Hub; real provider and deployed mobile-to-computer
acceptance remain separate.
