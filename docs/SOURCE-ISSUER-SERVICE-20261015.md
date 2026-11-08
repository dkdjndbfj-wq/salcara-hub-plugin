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
