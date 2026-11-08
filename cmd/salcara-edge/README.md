# Salcara Edge

`salcara-edge` is an optional, independently deployed HTTP ingress for one fixed
Salcara Hub. It forwards `/salcara-hub` over a server-owned HTTP(S) or SOCKS5
proxy, including an existing Mihomo mixed/SOCKS listener. Client apps receive no
subscription or proxy credentials.

The forwarding path is **client → edge → selected proxy → fixed Hub origin**.
Deploy it on an ingress whose network path is useful for the intended clients.
Installing it beside the Hub on the same VPS proves functionality, but does not
by itself optimize return packets on already accepted Hub connections. A normal
airport subscription does not create a public ingress or a source-preserving
VPN route. No acceleration percentage is implied by node health checks.

## Run

Build with `go build ./cmd/salcara-edge`. Create a private control-token file
containing exactly 64 hexadecimal characters (32 cryptographically random bytes),
with mode `0600` on Unix, then run:

```sh
salcara-edge \
  -origin https://hub-origin.example.com \
  -data-dir /var/lib/salcara-edge \
  -control-token-file /run/secrets/salcara-edge-control \
  -listen 127.0.0.1:8790 \
  -control-listen 127.0.0.1:8791 \
  -trusted-proxy-cidrs 127.0.0.1/32,::1/128
```

The origin may be a private HTTP address on a trusted network or a normally
verified HTTPS origin. It must be the backend address, **not a public hostname
whose DNS points back to this edge**. The origin is fixed at startup and cannot
be overridden by request URLs or management requests.

HTTP origins are permitted only while every configured node is direct. Adding
any HTTP/SOCKS proxy requires an HTTPS origin, including when that proxy node is
disabled, so cookies, device authorization and bodies cannot traverse an airport
in cleartext. Configuration rejects this combination with `https_origin_required`.
TLS certificate verification has no production bypass.

Publish ingress through an HTTPS reverse proxy with SSE buffering disabled and
appropriate streaming timeouts. Keep the control listener private. It only
accepts numeric loopback listeners; a remote Hub control client needs a separate
private HTTPS reverse proxy or secured tunnel to this listener. Do not publish
the management endpoint on the public user ingress.

`-trusted-proxy-cidrs` is optional. With no configured CIDRs, forwarded headers
are ignored. If a frontend is trusted, it must append the actual connecting IP
to `X-Forwarded-For` and set `X-Forwarded-Proto` itself. Edge walks the trusted
chain from the right and sends a single client IP to Hub, whose existing trusted
proxy mode reads the rightmost address. Never trust arbitrary Internet CIDRs.

All flags also have `SALCARA_EDGE_*` environment equivalents: `ORIGIN`,
`DATA_DIR`, `CONTROL_TOKEN_FILE`, `LISTEN`, `CONTROL_LISTEN`, and
`TRUSTED_PROXY_CIDRS`. Secrets are read from the file, not logged.

## Management contract

Every endpoint requires `Authorization: Bearer <control token>`. Query tokens,
redirected control endpoints, and arbitrary fetch destinations are not features
of this service. Successful changes return complete redacted state.

| Method and path | JSON request |
| --- | --- |
| `GET /v1/state` | none |
| `POST /v1/nodes` | `{"expected_revision":1,"node":{"name":"Mihomo","proxy_url":"socks5://127.0.0.1:7891","enabled":true}}` |
| `POST /v1/nodes/delete` | `{"expected_revision":2,"id":"node-id"}` |
| `POST /v1/policy` | `{"expected_revision":3,"mode":"manual","selected_id":"node-id"}` |
| `POST /v1/probe` | `{}` |
| `POST /v1/subscriptions/inspect` | `{"url":"https://provider.example/subscription"}` |
| `POST /v1/subscriptions/import` | `{"preview_id":"opaque-id","expected_revision":4,"node_indexes":[0,2]}` |

An omitted node ID creates a node; a supplied ID edits an existing node. On
edit, omitting `proxy_url` retains the saved credential. Explicit `""` means
direct. State always strips the entire URL userinfo and includes
`has_credentials`. At most 32 nodes are saved. The initial configuration is a
manual, enabled `direct` node. A disabled selected node is not accepted by the
policy API. Disable/delete selects the next enabled node or leaves no route.

Policies are `manual`, `failover` (selected first, then creation order),
`latency`, and `balance`. The first three retain a single selected route;
`balance` selects an eligible node independently for each new request, and
reports an empty `active_id`. Latency changes need at least 30 ms and 20% improvement, with a
two-minute cooldown; failure bypasses that cooldown. Probes run every 30 seconds
by default with at most four concurrent requests and six seconds per request.
They request only the fixed origin's `/salcara-hub/statusz` and require
`{"status":"available"}`. Three consecutive probe failures mark a node unhealthy;
an actual transport or response-body read failure does so immediately. Each
completed probe updates selection and wakes queued traffic without waiting for
the slowest node in the probe pass. This is edge-to-Hub latency,
not client-to-Hub or complete phone-to-computer latency.

Health values are `unknown`, `healthy`, `degraded`, and `unhealthy`.
`checked_at` is the time of the last successful latency sample; failed probes
never relabel an old RTT as newly measured. `last_probe_at` records the latest
probe attempt. These and event `at` use Unix milliseconds. Pending failures
display `degraded` after a prior success, or `unknown` before any success, while
remaining eligible until the third consecutive failure. Top-level status is
`unknown`, `ready`, `degraded`, `unavailable`, or `stopped`. Unknown health is not
proof of connectivity. A revision conflict returns HTTP 409 and
`revision_conflict`; callers must reload rather than overwrite newer changes.

SSE flushes immediately and has no overall body deadline. The upstream response
header deadline is 130 seconds, covering Hub's supported 120-second command wait;
probes retain their separate six-second budget. Existing connections
keep their transport when selection changes. New requests use the new route;
broken streams reconnect through the application's existing cursor protocol.
POST and other mutating requests use a fresh HTTP/1.1 connection and are not
retried on another node. A transport failure returns a redacted 502 while later
requests can use a fallback. The app must reconcile command receipts itself.

## Concurrent scheduling and capacity

Every node accepts `weight` (1–100, default 1), `max_requests` (1–4096,
default 64), `max_streams` (1–4096, default 16), and `draining` (default false).
Omitted fields on an edit keep their saved value. Policy updates optionally
include a `limits` object; omitted fields retain their value:

```json
{
  "expected_revision": 4,
  "mode": "balance",
  "selected_id": "",
  "limits": {
    "max_requests": 256,
    "max_streams": 128,
    "queue_limit": 64,
    "queue_timeout_ms": 2000
  }
}
```

The displayed values are defaults. Global request and stream limits each allow
1–4096; the shared queue allows 0–4096 entries (0 disables waiting), and the
queue timeout allows 1–30000 ms. Old saved configurations migrate to these
defaults. Reducing a limit does not terminate already admitted work; new work
waits or is rejected until occupancy falls below the new limit.

Balance chooses the lowest `(current class occupancy + 1) / weight` score among
nodes with available capacity, rotating equal-score ties. Selection and slot
reservation are atomic. This is weighted least occupancy, not round-robin,
measured throughput adaptation, or guaranteed traffic percentages. A faster
node can finish and accept more work; multiple node entries pointing at the
same proxy do not create independent physical capacity. Other modes queue on
their chosen node when full; they do not silently turn into load balancing.

A GET whose Accept header includes `text/event-stream`, or whose path is the
Hub `app/stream` or `bridge/stream` endpoint, occupies a stream slot. Other
requests, including long polling, occupy request slots. These classes have
independent global and node admission budgets, with a shared total queue bound.
Waiting is FIFO within each class and cancellable; it is not per-user fairness.
Excess arrivals receive `429 queue_full`; queue expiry receives
`503 queue_timeout`; no usable route receives `503 no_available_route`. These
responses include `Retry-After`, and the edge never replays the caller's command.

Slots remain reserved until the upstream body ends, is closed, fails, or the
caller cancels. Every path releases once. An upstream SSE truncation now marks
the route failed; caller cancellation does not. A clean EOF is completion, not
evidence of a broken node. Draining, disabling, deleting or changing a node
does not terminate its existing streams. Deleted routes remain counted in the
global runtime until their in-flight work completes; replacement transports
share the same node occupancy so edits cannot reset occupied slots.

Unknown nodes initially admit at most one business operation in total. After a
failure, a node is excluded from automatic modes until a successful probe;
it then remains `recovering: true`, with a single combined request/stream slot,
until a subsequent successful probe at least five seconds after that recovery
sample. This bounds recovery trials instead of flooding a recently restored
proxy. Manual mode may explicitly use an unhealthy node, still with the single
trial limit. An active long stream can occupy that trial slot until a later
successful probe restores the full limits.

State includes global and per-node `runtime`: `active_requests`,
`active_streams`, `queued_requests`, `queued_streams`, `completed_requests`,
`failed_requests`, `rejected_requests`, and `cancelled_requests`. Cumulative
counters restart with the process and include streams as business requests;
health probes are excluded. Completed counts normal body completion. Failed
counts transport/read errors and completed HTTP 429/5xx responses; origin
HTTP errors alone do not mark a proxy unhealthy. Cancelled counts caller
cancellation or early body close. Queueing and rejection happen before node
assignment, so per-node queued/rejected counts remain zero and the global
values carry that information. These admission limits do not replace reverse
proxy connection/header limits, bandwidth controls, per-user quotas or capacity
benchmarks on the actual deployment.

## Subscription inspection

The inspector supports Clash/provider YAML, equivalent JSON, URI lists, and
base64 URI lists. It only extracts nodes; rules, DNS, scripts, TUN, and nested
provider URLs are never executed or fetched. HTTP(S) and SOCKS5 nodes with
compatible options can be imported. SS/SSR, Trojan, VMess, VLESS, Hysteria,
Hysteria2, TUIC and similar protocols are reported as `requires_mihomo` and are
not imported as if they were native implementations. An operator can separately
connect a configured Mihomo HTTP/SOCKS listener.

Compatibility is distinct from connectivity. Inspection does not connect to
nodes. It returns counts, per-node support reasons, and a ten-minute preview.
Import consumes a preview only after successful atomic persistence and starts
health probes. Conflicts and failed imports preserve the preview and old pool.
Every preview row has its original zero-based `index`. Optional `node_indexes`
imports exactly the selected compatible rows; duplicate, unknown and unsupported
indexes are rejected. Omitting this field imports all compatible rows. A full
pool or oversized selection fails without silently truncating the subscription.
Imports are additive; duplicated native URLs are skipped or reported, never
used to overwrite credentials silently. The preview cache holds at most eight
entries and inspection admits at most two concurrent operations.

Subscription fetches use public HTTPS only, a five-second timeout, three
redirects, and a 1 MiB decoded-body limit. Each connection validates all DNS
answers, rejects private/loopback/metadata/reserved addresses, then dials a
validated IP directly. No environment proxy, cookie jar, or authorization
headers are used. Source URLs and node credentials never appear in previews.
Aliases, duplicate YAML keys, excessive nesting, and excessive node counts are
rejected. Imported node addresses have the same public-address restriction,
rechecked at each dial to prevent DNS rebinding. Explicit administrator-created
nodes can use private addresses, as required for a local Mihomo listener.

## Storage and limits

Saved proxy credentials are plaintext in a private configuration file, not
encrypted at rest. Use a private persistent volume and backups. Unix directories
and files with group/other access are rejected; Windows permission bits do not
verify ACLs, so deployments must provision appropriate Windows ACLs themselves.
Symlink state files and directories, nonregular files and oversized files are
rejected. An OS file lock admits one writer per data directory and releases on
process exit, including crashes. Do not remove its lock file to bypass it.

Changes fsync a new 0600 temporary file, atomically replace configuration, then
sync the directory where supported. State's `durability` is `confirmed` when
directory sync succeeds, `platform_atomic` on Windows, and `uncertain` if the
rename committed but directory sync could not be confirmed. The last case is a
successful visible change with a warning, not a falsely reported rollback.

The only added Go dependency is `go.yaml.in/yaml/v3`; include both `go.mod` and
`go.sum` in builds. Validate with:

```sh
go test ./internal/edgepool/... ./cmd/salcara-edge/...
go test -race ./internal/edgepool/...
go vet ./internal/edgepool/... ./cmd/salcara-edge/...
```
