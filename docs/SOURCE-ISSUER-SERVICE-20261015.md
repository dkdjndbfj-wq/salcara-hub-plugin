# Station source issuer service

`internal/sourceissuer` is the deployable reference issuer for a station that
uses the existing Sub2API identity endpoint. It closes the previously missing
deployment boundary for `verified_proxy` and `site_api` grants without putting
the station key in the Hub or the phone.

The service accepts the Hub's command-bound snapshot and an API key over the
station's own HTTPS origin. It then calls the configured `/v1/salcara-hub/key`
endpoint, obtains the authoritative `group_id`, checks the configured exact
API-base/source/Hub-audience/Agent/model scope, and signs a two-minute Ed25519
grant. It never trusts the caller's source label or API-base claim by itself.
Redirects, non-HTTPS public endpoints, foreign verifier hosts, duplicate JSON
members, extra fields, stale challenges, invalid request IDs, and oversized or
slow bodies are rejected. Keys are held only for the direct verifier request;
the service stores only a bounded SHA-256 rate-limit map.

The standalone binary is `cmd/salcara-source-issuer`:

```text
salcara-source-issuer.exe -config issuer.json -key issuer-seed.b64url -check
salcara-source-issuer.exe -config issuer.json -key issuer-seed.b64url -public-config
salcara-source-issuer.exe -config issuer.json -key issuer-seed.b64url -listen 127.0.0.1:9418
```

The listener is loopback-only so a station HTTPS reverse proxy must explicitly
forward the configured authorization path to it. The public proxy must use the
same origin as the configured API base and must not expose the loopback port.
The signing key file contains one unpadded base64url Ed25519 seed, with file
permissions restricted to the service account. `-public-config` prints the
issuer trust entry for the Hub policy; it never prints the private seed or a
verifier URL.

Example configuration (synthetic values):

```json
{
  "id": "station-a",
  "authorizationUrl": "https://api.example.test/source-grants",
  "routes": [{
    "apiBase": "https://api.example.test/tenant-a",
    "verifierUrl": "https://api.example.test/v1/salcara-hub/key",
    "groups": [{
      "id": 7,
      "sourceType": "site_api",
      "audiences": ["hub-a"],
      "agents": ["codex", "claude"]
    }]
  }]
}
```

The service is an adapter boundary, not a universal provider integration. A
station must make its real authoritative identity endpoint return the exact
`{"group_id": number}` object and configure its own group policy. It does not
implement OAuth, Token/JSON import, or a native desktop provider attestation;
those remain the existing account-vault and managed-executor boundaries.

Validation performed for this source addition:

| Check | Result |
| --- | --- |
| `go test ./internal/sourceissuer ./cmd/salcara-source-issuer -count=1` | passed |
| `go vet ./internal/sourceissuer ./cmd/salcara-source-issuer` | pending with整树 run |
| synthetic issuer → Sub2API key-group → signed Hub grant | passed |
| wrong group, source/route/audience/agent claim, revocation | passed |
| redirect, duplicate/unknown JSON, body/credential bounds, cancellation, concurrent requests | passed |

This service does not imply that a third-party station has deployed it. The
station operator still needs to configure and run the issuer and publish its
public key in the Hub policy.

## Opt-in device enrollment v1

The next-source implementation also serves a separate, fixed same-origin
`enrollmentUrl`. This is not a command source grant and does not authorize a
phone: normal QR pairing is still required. Existing configurations keep
device enrollment disabled. No endpoint is discovered from a redirect, and no
API key is sent to a Hub, phone, or arbitrary verifier.

Add these optional fields to the existing configuration (synthetic example):

```json
{
  "enrollmentUrl": "https://api.example.test/device-enrollment",
  "enrollmentHubs": [{
    "audience": "hub-a",
    "hubInstanceId": "11111111111111111111111111111111",
    "identityPublicKey": "<Hub ingress Ed25519 public key, standard padded base64>"
  }]
}
```

Each existing route that permits enrollment needs a fixed
`enrollmentVerifierUrl`, ending in `/v1/salcara-hub/enrollment-identity`. It must
belong to the station's exact public host or an explicitly configured
loopback backend. Each allowed group must separately set
`allowDeviceEnrollment: true`; its existing `audiences` must include the
approved Hub audience. Merely configuring URLs, command issuers, source
categories or a successful `/models` response does not enable enrollment.

The new verifier must authenticate the real API key and return exactly:

```json
{"version":1,"subject":"sub2api-user:42","groupId":7}
```

The station adapter obtains the positive user ID and group from its actual
authentication context and checks enabled, expired and quota state. Request
fields cannot supply this identity. The original `/v1/salcara-hub/key` remains
unchanged because command authorization still expects the exact older
`{"group_id":7}` object. A station without the new adapter does not support
automatic enrollment; do not substitute a model endpoint or the older schema.

The enrollment POST accepts exactly
`{version:1,challenge:{payload,signature},credential:{type:"api_key",value}}`.
The challenge is purpose-separated Ed25519 proof from a preconfigured exact
`(audience,hubInstanceId,identityPublicKey)` tuple, not a key supplied by the
caller. It binds issuer, complete API base, device ID, secret hash, policy
revision, challenge and request IDs, and a maximum 120-second lifetime.
Envelope fields use unpadded base64url; only the configured Hub identity public
key uses standard padded base64, matching ingress identity.

After verification, the service obtains the authoritative subject/group,
checks group opt-in and audience, and signs the frozen enrollment grant. The
grant keeps every immutable challenge field, uses the
`salcara-device-enrollment-v1` signature domain, and expires no later than the
original challenge or 120 seconds. The Hub independently checks current
policy, first-device admission, ownership, bans, capacity and durable challenge
consumption; the issuer does not bypass these checks or consume the challenge.
Successful desktop enrollment does not bind a phone.

### Explicit recovery after an uncertain enrollment response

The desktop must keep the original Hub identity pin, device ID and device
secret. It first reads `/discovery/policy` using those device credentials to
confirm whether the original submission completed. The existing 403
`device_credentials_invalid` means either a missing device or invalid/blocked
credentials; it is not evidence that enrollment is safe to replay. A missing
credential set returns 401 `device_credentials_required`, not "device absent".

Only an explicit user retry may request a new anonymous
`/device/enrollment/challenge`, using the same device ID/secret hash and a new
request ID. The Hub waits for in-flight enrollment persistence, checks all
device namespaces, and refuses an existing ID with 409
`enrollment_request_conflict` before reusing any cached challenge. Known user
disablement remains 423 `user_disabled`; banned targets are rejected with 403
`device_credentials_invalid`. Neither case issues a new challenge. No API key
or device secret is sent to this anonymous endpoint.

Only a fresh challenge verified against the same trusted Hub pin and exact
pending context permits requesting a new grant from the fixed same-origin
issuer. A challenge is not a device reservation: final registration rechecks
existence and ownership atomically, so concurrent attempts cannot replace the
winning device or its subject. Network errors, 5xx and generic 401/403 never
authorize automatic replay, a new device ID, or a new phone binding. No extra
status endpoint is introduced.

The command-source and enrollment issuer endpoints share the bounded
32-request admission and 120-per-minute
credential-hash limiter (at most 4096 hashes). Enrollment bodies are limited to
48 KiB, identity responses to 4 KiB, verifier requests to five seconds; slow
body reads retain the eight-second deadline. No credential/verifier verdict
is cached, so revocation is checked on each request. All redirect statuses are
refused without forwarding credentials to their target. Malformed or unknown
identity responses and transient failures return 503
`api_verifier_unavailable`; verifier 401/403 maps to 401
`api_credentials_invalid`, and a valid but disallowed group receives 403
`api_enrollment_denied`. Hub trust and challenge expiry have distinct 403
codes. Upstream error bodies and raw API credentials are never reflected.

`-check` validates both sets of configuration offline. `-public-config`
includes `deviceEnrollment: true` and `enrollmentUrl` only when explicit
enrollment group opt-in and its trust/verifier configuration are present; it
omits verifier URLs, pinned Hub configuration and private signing material.
Enabling this source does not deploy a station adapter or update an existing
installation automatically.
