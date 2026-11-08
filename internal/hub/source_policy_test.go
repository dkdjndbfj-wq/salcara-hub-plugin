package hub

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestRemoteAccessPolicyDefaultsOpenAndRejectsInvalidIssuer(t *testing.T) {
	p, err := ParseRemoteAccessPolicy([]byte(`{"mode":""}`))
	if err != nil || p.Mode != "open" || p.Revision != "1" {
		t.Fatalf("defaults: %#v %v", p, err)
	}
	if _, err := ParseRemoteAccessPolicy([]byte(`{"mode":"restricted","audience":"hub","issuers":[{"id":"issuer","publicKey":"bad","sourceTypes":["site_api"]}]}`)); err == nil {
		t.Fatal("invalid public key accepted")
	}
}

func sourceContractFixture(t *testing.T) (*Hub, *http.Request, commandReq, SourceSnapshot, ed25519.PrivateKey) {
	t.Helper()
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := receiptFixture(t, time.Second)
	policy := RemoteAccessPolicy{Mode: "restricted", Revision: "r2", Audience: "hub-prod", Station: &StationPolicy{Version: 1, APIBases: []string{"https://example.com"}}, Issuers: []SourceIssuer{{ID: "site", PublicKey: base64.RawURLEncoding.EncodeToString(public), SourceTypes: []string{"site_api"}}}}
	if err := policy.normalize(); err != nil {
		t.Fatal(err)
	}
	h.cfg.RemoteAccess = policy
	const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	h.mu.Lock()
	dev := h.accounts["receipt-owner"].devices["pc"]
	dev.bindingID = "binding-1"
	h.setPairLocked("receipt-owner", dev, sha256.Sum256([]byte(token)), true)
	h.mu.Unlock()
	command := json.RawMessage(`{"type":"session.send","accountId":"credential-1","agent":"codex","controlSurface":"cli","sessionKey":"codex:s","model":"gpt"}`)
	canonical, err := canonicalCommand(command)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	snapshot := SourceSnapshot{Version: 1, Issuer: "site", Audience: "hub-prod", DeviceID: "pc", BindingID: "binding-1", CredentialHandle: "credential-1", Agent: "codex", ControlSurface: "cli", SourceType: "site_api", Origin: "https://example.com", APIBase: "https://example.com", ConfigRevision: "cfg-1", PolicyRevision: "r2", CommandHash: shaHex(canonical), RequestID: receiptRequestID, Model: "gpt", IssuedAt: now, ExpiresAt: now + 60}
	req := commandReq{DeviceID: "pc", Command: command, RequestID: receiptRequestID}
	signSourceFixture(t, &req, snapshot, private)
	r := httptest.NewRequest("POST", "/api/app/commands", nil)
	r.Header.Set("X-Salcara-Pair-Token", token)
	return h, r, req, snapshot, private
}

func signSourceFixture(t *testing.T, req *commandReq, snapshot SourceSnapshot, private ed25519.PrivateKey) {
	t.Helper()
	payload, err := json.Marshal(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	req.SourceGrant = &SignedSourceGrant{Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, payload))}
	req.sourceGrantRaw = nil
}

func TestSignedSourceGrantExactFields(t *testing.T) {
	if _, err := ParseSignedSourceGrant([]byte(`{"payload":"a","signature":"b"}`)); err != nil {
		t.Fatal(err)
	}
	for _, raw := range []string{
		`{"payload":"a","payload":"b","signature":"c"}`,
		`{"payload":"a","Payload":"b","signature":"c"}`,
		`{"payload":"a","signature":"b","Signature":"c"}`,
		`{"payload":"a","signature":"b","unknown":1}`,
		`{"payload":null,"signature":"b"}`,
		`{"payload":1,"signature":"b"}`,
		`{"payload":"a"}`,
		`{"payload":"a","signature":"b"}{}`,
	} {
		if _, err := ParseSignedSourceGrant([]byte(raw)); err == nil {
			t.Fatalf("ambiguous grant accepted: %s", raw)
		}
	}
}

func TestSourceSnapshotExactFields(t *testing.T) {
	_, _, _, snapshot, _ := sourceContractFixture(t)
	snapshot.APIBase = "https://example.com/prefix"
	raw, _ := json.Marshal(snapshot)
	fields, err := strictObjectFields(raw, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSourceSnapshot(raw); err != nil {
		t.Fatal(err)
	}
	for name, value := range fields {
		for _, variant := range []string{name, strings.ToUpper(name[:1]) + name[1:]} {
			key, _ := json.Marshal(variant)
			ambiguous := append(append(append(append([]byte(nil), raw[:len(raw)-1]...), ','), key...), ':')
			ambiguous = append(append(ambiguous, value...), '}')
			if _, err := ParseSourceSnapshot(ambiguous); err == nil {
				t.Fatalf("ambiguous snapshot field accepted: %s", variant)
			}
		}
	}
	for _, suffix := range []string{`,"extra":1}`, `,"model":null}`} {
		invalid := append(append([]byte(nil), raw[:len(raw)-1]...), suffix...)
		if _, err := ParseSourceSnapshot(invalid); err == nil {
			t.Fatalf("invalid snapshot accepted: %s", suffix)
		}
	}
	if _, err := ParseSourceSnapshot(append(raw, []byte(` {}`)...)); err == nil {
		t.Fatal("trailing snapshot JSON accepted")
	}
}

func TestHTTPGrantNestedConfusionRejectedBeforeEnvelope(t *testing.T) {
	h, r, req, _, _ := sourceContractFixture(t)
	grant, _ := json.Marshal(req.SourceGrant)
	grant = append(append([]byte(nil), grant[:len(grant)-1]...), []byte(`,"Payload":"different"}`)...)
	request := map[string]any{"deviceId": req.DeviceID, "command": req.Command, "requestId": req.RequestID, "sourceGrant": json.RawMessage(grant)}
	raw, _ := json.Marshal(request)
	r.Body = io.NopCloser(bytes.NewReader(raw))
	w := httptest.NewRecorder()
	var decoded commandReq
	if !decodeCommandRequest(w, r, &decoded) {
		t.Fatalf("outer valid JSON failed before grant policy: %d", w.Code)
	}
	if code, source := h.executionSourceFor("receipt-owner", r, decoded); code != "source_grant_invalid" || source != nil {
		t.Fatalf("nested confused grant forwarded: %s %#v", code, source)
	}
}

func TestIssuerScopesBindRegisteredExecutionBase(t *testing.T) {
	h, r, req, snapshot, private := sourceContractFixture(t)
	policy := h.CurrentRemoteAccessPolicy()
	policy.Issuers[0].Origins = []string{"https://example.com"}
	policy.Issuers[0].APIBases = []string{"https://example.com/tenant-a"}
	policy.Station.APIBases = []string{"https://example.com/tenant-a"}
	if err := h.ApplyRemoteAccessPolicy(policy, nil); err != nil {
		t.Fatal(err)
	}
	snapshot.APIBase = "https://example.com/tenant-a"
	snapshot.PolicyRevision = h.CurrentRemoteAccessPolicy().Revision
	signSourceFixture(t, &req, snapshot, private)
	if code, source := h.executionSourceFor("receipt-owner", r, req); code != "" || source == nil || source.APIBase != snapshot.APIBase {
		t.Fatalf("registered API base rejected/lost: %s %#v", code, source)
	}
	for _, tc := range []struct{ name, origin, base string }{
		{"missing-base", "https://example.com", ""},
		{"different-path", "https://example.com", "https://example.com/tenant-b"},
		{"prefix-confusion", "https://example.com", "https://example.com/tenant-a.evil"},
		{"different-origin", "https://other.example", "https://other.example/tenant-a"},
		{"localhost-proxy", "http://127.0.0.1:9000", "http://127.0.0.1:9000"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			changed := snapshot
			changed.Origin, changed.APIBase = tc.origin, tc.base
			signSourceFixture(t, &req, changed, private)
			if code := h.evaluateSourcePolicy("receipt-owner", r, req); code != "source_scope_denied" {
				t.Fatalf("wrong scope accepted: %s", code)
			}
		})
	}
}

func TestIssuerScopeConfigurationAndDiscovery(t *testing.T) {
	public, _, _ := ed25519.GenerateKey(nil)
	issuer := SourceIssuer{ID: "site", PublicKey: base64.RawURLEncoding.EncodeToString(public), SourceTypes: []string{"site_api"}, Origins: []string{"https://example.com"}, APIBases: []string{"https://example.com/prefix"}, AuthorizationURL: "https://identity.example.com/source-grants"}
	policy := RemoteAccessPolicy{Mode: "restricted", Revision: "r1", Audience: "hub", Issuers: []SourceIssuer{issuer}}
	if err := policy.normalize(); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name   string
		mutate func(*SourceIssuer)
	}{
		{"origin-path", func(i *SourceIssuer) { i.Origins = []string{"https://example.com/path"} }},
		{"origin-query", func(i *SourceIssuer) { i.Origins = []string{"https://example.com?"} }},
		{"origin-case", func(i *SourceIssuer) { i.Origins = []string{"https://EXAMPLE.com"} }},
		{"origin-http", func(i *SourceIssuer) { i.Origins = []string{"http://example.com"} }},
		{"origin-port", func(i *SourceIssuer) { i.Origins = []string{"https://example.com:443"} }},
		{"origin-port-range", func(i *SourceIssuer) { i.Origins = []string{"https://example.com:65536"} }},
		{"origin-empty-port", func(i *SourceIssuer) { i.Origins = []string{"https://example.com:"} }},
		{"origin-trailing-dot-port", func(i *SourceIssuer) { i.Origins = []string{"https://example.com.:9000"} }},
		{"origin-duplicate", func(i *SourceIssuer) { i.Origins = []string{"https://example.com", "https://example.com"} }},
		{"base-origin", func(i *SourceIssuer) { i.APIBases = []string{"https://other.example/prefix"} }},
		{"base-v1", func(i *SourceIssuer) { i.APIBases = []string{"https://example.com/prefix/v1"} }},
		{"base-slash", func(i *SourceIssuer) { i.APIBases = []string{"https://example.com/prefix/"} }},
		{"base-dot", func(i *SourceIssuer) { i.APIBases = []string{"https://example.com/a/../prefix"} }},
		{"base-query", func(i *SourceIssuer) { i.APIBases = []string{"https://example.com/prefix?"} }},
		{"base-encoded", func(i *SourceIssuer) { i.APIBases = []string{"https://example.com/a%2fb"} }},
		{"base-duplicate", func(i *SourceIssuer) {
			i.APIBases = []string{"https://example.com/prefix", "https://example.com/prefix"}
		}},
		{"authorization-http", func(i *SourceIssuer) { i.AuthorizationURL = "http://identity.example.com/source-grants" }},
		{"authorization-user", func(i *SourceIssuer) { i.AuthorizationURL = "https://user@identity.example.com/source-grants" }},
		{"authorization-query", func(i *SourceIssuer) { i.AuthorizationURL = "https://identity.example.com/source-grants?" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := cloneRemoteAccessPolicy(policy)
			tc.mutate(&candidate.Issuers[0])
			raw, _ := json.Marshal(candidate)
			if _, err := ParseRemoteAccessPolicy(raw); err == nil {
				t.Fatal("malformed issuer scope accepted")
			}
		})
	}
	h, _, _, _, _ := sourceContractFixture(t)
	if err := h.ApplyRemoteAccessPolicy(policy, nil); err != nil {
		t.Fatal(err)
	}
	discovery := h.RemoteSourceDiscovery()
	discovery.Issuers[0].Origins[0], discovery.Issuers[0].APIBases[0] = "https://changed.test", "https://changed.test"
	if got := h.CurrentRemoteAccessPolicy(); got.Issuers[0].Origins[0] != issuer.Origins[0] || got.Issuers[0].APIBases[0] != issuer.APIBases[0] || got.Issuers[0].AuthorizationURL != issuer.AuthorizationURL {
		t.Fatal("public discovery mutated runtime issuer scope")
	}
}

func TestSourceDescribeReadAndModeEpoch(t *testing.T) {
	h, r, req, _, _ := sourceContractFixture(t)
	oldRevision := h.CurrentRemoteAccessPolicy().Revision
	policy := h.CurrentRemoteAccessPolicy()
	policy.Mode = "disabled"
	if err := h.ApplyRemoteAccessPolicy(policy, nil); err != nil {
		t.Fatal(err)
	}
	if h.CurrentRemoteAccessPolicy().Revision == oldRevision {
		t.Fatal("mode switch retained grant epoch")
	}
	read := req
	read.Command, read.SourceGrant = json.RawMessage(`{"type":"session.describe","sessionKey":"claude:s","controlSurface":"read-only"}`), nil
	if code := h.evaluateSourcePolicy("receipt-owner", r, read); code != "" {
		t.Fatalf("safe describe rejected while execution disabled: %s", code)
	}
	policy = h.CurrentRemoteAccessPolicy()
	policy.Mode = "restricted"
	policy.Revision = oldRevision // A caller must not resurrect a previous epoch.
	if err := h.ApplyRemoteAccessPolicy(policy, nil); err != nil {
		t.Fatal(err)
	}
	if code := h.evaluateSourcePolicy("receipt-owner", r, req); code != "source_policy_stale" {
		t.Fatalf("old grant revived after mode cycle: %s", code)
	}
}

func TestRemoteAccessPolicyNormalizesTrustKeyWithoutRetainingPrivateKey(t *testing.T) {
	public, _, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(RemoteAccessPolicy{Mode: "restricted", Revision: "r2", Audience: "deployment", Issuers: []SourceIssuer{{ID: "site", PublicKey: base64.RawURLEncoding.EncodeToString(public), SourceTypes: []string{"site_api", "verified_proxy"}}}})
	p, err := ParseRemoteAccessPolicy(raw)
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Issuers) != 1 || len(p.Issuers[0].key) != ed25519.PublicKeySize || p.Issuers[0].ID != "site" {
		t.Fatalf("normalized trust anchor: %#v", p)
	}
}

func TestSourcePolicyClassifiesExecutionSeparatelyFromReads(t *testing.T) {
	if !sourceReadOrSafetyCommand("sessions.list") || !sourceReadOrSafetyCommand("session.interrupt") {
		t.Fatal("safe read/safety command not classified")
	}
	if !sourceExecutionCommand("session.send") || !sourceExecutionCommand("approval.respond") || !sourceExecutionCommand("features.list") {
		t.Fatal("execution command not classified")
	}
	if sourceExecutionCommand("sessions.list") || sourceReadOrSafetyCommand("features.list") || sourceReadOrSafetyCommand("unknown.future.command") {
		t.Fatal("unknown command was implicitly allowed")
	}
}

func TestCommandMetaKeepsBusinessFieldsAndRejectsTypeConfusion(t *testing.T) {
	meta, err := commandMeta(json.RawMessage(`{"type":"session.start","cwd":"C:\\work","prompt":"hello","model":"gpt"}`))
	if err != nil || commandString(meta, "type") != "session.start" || commandString(meta, "prompt") != "hello" {
		t.Fatalf("business fields were rejected: %#v %v", meta, err)
	}
	if _, err := commandMeta(json.RawMessage(`{"type":"session.send","Type":"session.open","sessionKey":"claude:x"}`)); err == nil {
		t.Fatal("case-confusable type accepted")
	}
	for _, raw := range []string{
		`{"type":"session.send","model":"signed","Model":"executed"}`,
		`{"type":"session.send","sessionKey":"codex:one","SessionKey":"claude:two"}`,
		`{"type":"session.send","type":"session.open"}`,
		`{"type":"session.send"}{"type":"session.open"}`,
		`{"type":"session.send"`,
	} {
		if _, err := commandMeta(json.RawMessage(raw)); err == nil {
			t.Fatalf("ambiguous or truncated command accepted: %s", raw)
		}
	}
}

func TestEvaluateSourcePolicyAcceptsBoundSignedGrantAndRejectsTampering(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	h, _ := receiptFixture(t, time.Second)
	h.cfg.RemoteAccess = RemoteAccessPolicy{Mode: "restricted", Revision: "r2", Audience: "hub-prod", Station: &StationPolicy{Version: 1, APIBases: []string{"https://example.com"}}, Issuers: []SourceIssuer{{
		ID: "site", PublicKey: base64.RawURLEncoding.EncodeToString(public), SourceTypes: []string{"site_api"}, key: public,
	}}}
	const token = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	h.mu.Lock()
	dev := h.accounts["receipt-owner"].devices["pc"]
	dev.bindingID = "binding-1"
	h.setPairLocked("receipt-owner", dev, sha256.Sum256([]byte(token)), true)
	h.mu.Unlock()

	command := json.RawMessage(`{"type":"desktop.session.send","accountId":"credential-1","agent":"codex","controlSurface":"desktop","sessionKey":"codex:s","model":"gpt"}`)
	requestID := receiptRequestID
	canonical, err := canonicalCommand(command)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().Unix()
	snapshot := SourceSnapshot{Version: 1, Issuer: "site", Audience: "hub-prod", DeviceID: "pc", BindingID: "binding-1", CredentialHandle: "credential-1", Agent: "codex", ControlSurface: "desktop", SourceType: "site_api", Origin: "https://example.com", APIBase: "https://example.com", ConfigRevision: "cfg-1", PolicyRevision: "r2", CommandHash: shaHex(canonical), RequestID: requestID, Model: "gpt", IssuedAt: now, ExpiresAt: now + 60}
	payload, _ := json.Marshal(snapshot)
	grant := &SignedSourceGrant{Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(private, payload))}
	req := commandReq{DeviceID: "pc", Command: command, RequestID: requestID, SourceGrant: grant}
	httpReq := httptest.NewRequest("POST", "/api/app/commands", nil)
	httpReq.Header.Set("X-Salcara-Pair-Token", token)
	if code := h.evaluateSourcePolicy("receipt-owner", httpReq, req); code != "" {
		t.Fatalf("valid grant rejected: %s", code)
	}

	tampered := req
	tampered.Command = json.RawMessage(`{"type":"desktop.session.send","accountId":"credential-1","agent":"codex","controlSurface":"desktop","sessionKey":"codex:s","model":"other"}`)
	if code := h.evaluateSourcePolicy("receipt-owner", httpReq, tampered); code != "source_command_mismatch" {
		t.Fatalf("tampered command: %s", code)
	}
	tampered = req
	tampered.RequestID = "20000000-0000-4000-8000-000000000002"
	if code := h.evaluateSourcePolicy("receipt-owner", httpReq, tampered); code != "source_policy_stale" {
		t.Fatalf("replayed request id: %s", code)
	}
}
