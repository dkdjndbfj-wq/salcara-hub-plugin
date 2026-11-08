package hub

import (
	"bytes"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func officialStationFixture(t *testing.T) (*Hub, *http.Request, commandReq, SourceChallenge, SourceSnapshot) {
	t.Helper()
	h, pair, req, snapshot, _ := sourceContractFixture(t)
	p := h.CurrentRemoteAccessPolicy()
	p.Issuers, p.Audience = nil, ""
	p.Station = &StationPolicy{Version: 1, APIBases: []string{}, AllowOfficialAccounts: true}
	if err := h.ApplyRemoteAccessPolicy(p, nil); err != nil {
		t.Fatal(err)
	}
	challenge := issueSourceChallengeFixture(t, h, pair, req)
	snapshot.Issuer, snapshot.SourceType = stationOfficialIssuer, "official_account_proxy"
	snapshot.Origin, snapshot.APIBase = officialAccountOrigin, officialAccountAPIBase
	snapshot.Audience, snapshot.PolicyRevision = challenge.Audience, challenge.PolicyRevision
	snapshot.ChallengeID = challenge.ChallengeID
	snapshot.IssuedAt, snapshot.ExpiresAt = 0, 0
	req.SourceGrant = nil
	return h, pair, req, challenge, snapshot
}

func TestStationOfficialOnlyAcquisitionRequiresAuthenticatedExactBridgeReply(t *testing.T) {
	h, pair, req, challenge, snapshot := officialStationFixture(t)
	if challenge.Audience != "station" || len(h.CurrentRemoteAccessPolicy().Issuers) != 0 {
		t.Fatal("official-only challenge required external issuer")
	}
	secret := strings.Repeat("a", 64)
	h.mu.Lock()
	dev := h.accounts["receipt-owner"].devices["pc"]
	dev.hasSecret, dev.secretHash = true, sha256.Sum256([]byte(secret))
	conn := dev.conn
	h.mu.Unlock()
	raw, _ := json.Marshal(map[string]string{"challengeId": challenge.ChallengeID})
	r := httptest.NewRequest("POST", "/app/source/grant", bytes.NewReader(raw))
	r.Header = pair.Header.Clone()
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.handleSourceGrant(w, r, "receipt-owner"); close(done) }()
	var env commandEnvelope
	select {
	case raw := <-conn.cmds:
		if err := json.Unmarshal(raw, &env); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("no acquisition command")
	}
	if env.Source != nil || !bytes.Contains(env.Command, []byte(`"type":"source.grant.acquire"`)) || env.BindingID != challenge.BindingID {
		t.Fatalf("wrong acquisition envelope: %#v", env)
	}
	assertion, _ := json.Marshal(map[string]any{"officialAccount": snapshot})
	reply, _ := json.Marshal(replyBody{DeviceID: "pc", CommandID: env.CommandID, OK: true, Result: assertion})
	// A phone knows the nonce and selected handle, but cannot make the assertion.
	phoneReply := httptest.NewRequest("POST", "/bridge/reply", bytes.NewReader(reply))
	phoneReply.Header = pair.Header.Clone()
	denied := httptest.NewRecorder()
	h.handleBridgeReply(denied, phoneReply, "receipt-owner")
	if denied.Code != 404 {
		t.Fatalf("unsigned phone assertion accepted: %d", denied.Code)
	}
	bridgeReply := httptest.NewRequest("POST", "/bridge/reply", bytes.NewReader(reply))
	bridgeReply.Header.Set("X-Salcara-Device-Id", "pc")
	bridgeReply.Header.Set("X-Salcara-Device-Secret", secret)
	ack := httptest.NewRecorder()
	h.handleBridgeReply(ack, bridgeReply, "receipt-owner")
	if ack.Code != 200 {
		t.Fatalf("authenticated reply rejected: %d", ack.Code)
	}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("official acquisition stalled")
	}
	var out struct {
		SourceGrant SignedSourceGrant `json:"sourceGrant"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &out) != nil {
		t.Fatalf("official grant failed: %d %s", w.Code, w.Body.String())
	}
	req.SourceGrant = &out.SourceGrant
	if code, source := h.executionSourceFor("receipt-owner", pair, req); code != "" || source == nil || source.Issuer != stationOfficialIssuer || source.Station == nil || !source.Station.AllowOfficialAccounts {
		t.Fatalf("official execution denied: %s %#v", code, source)
	}
	issued, err := sourceGrantSnapshot(out.SourceGrant)
	if err != nil || issued.IssuedAt < time.Now().Unix()-5 || issued.ExpiresAt > challenge.ExpiresAt || issued.ExpiresAt-issued.IssuedAt > 120 || issued.ExpiresAt <= issued.IssuedAt {
		t.Fatalf("invalid issued TTL: %#v %v", issued, err)
	}
	h.mu.Lock()
	retained := len(h.sourceChallenges[challenge.ChallengeID].Command)
	h.mu.Unlock()
	if retained != 0 {
		t.Fatal("acquisition retained private command")
	}
	if strings.Contains(w.Body.String(), secret) {
		t.Fatal("device secret leaked into grant")
	}
	// New process key invalidates every previously issued official assertion.
	other := newDeviceHub(t, Config{})
	payload, _ := base64.RawURLEncoding.DecodeString(out.SourceGrant.Payload)
	signature, _ := base64.RawURLEncoding.DecodeString(out.SourceGrant.Signature)
	if ed25519.Verify(other.officialPublicKey, payload, signature) {
		t.Fatal("official signing key persisted across Hub instances")
	}
	// The ordinary app route accepts only the signed envelope.
	req.SourceGrant = &SignedSourceGrant{Payload: base64.RawURLEncoding.EncodeToString(assertion)}
	if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code == "" {
		t.Fatal("unsigned app assertion executed")
	}
}

func TestStationOfficialAssertionBindsEveryExecutionField(t *testing.T) {
	for _, tc := range []struct {
		name   string
		mutate func(*SourceSnapshot)
	}{
		{"version", func(s *SourceSnapshot) { s.Version = 2 }},
		{"issuer", func(s *SourceSnapshot) { s.Issuer = "external" }},
		{"source-type", func(s *SourceSnapshot) { s.SourceType = "site_api" }},
		{"origin", func(s *SourceSnapshot) { s.Origin = "https://evil.example" }},
		{"base", func(s *SourceSnapshot) { s.APIBase = "https://chatgpt.com/other" }},
		{"surface", func(s *SourceSnapshot) { s.ControlSurface = "desktop" }},
		{"challenge", func(s *SourceSnapshot) { s.ChallengeID = "other-challenge" }},
		{"audience", func(s *SourceSnapshot) { s.Audience = "other-station" }},
		{"revision", func(s *SourceSnapshot) { s.PolicyRevision = "old-revision" }},
		{"device", func(s *SourceSnapshot) { s.DeviceID = "other-device" }},
		{"binding", func(s *SourceSnapshot) { s.BindingID = "other-phone" }},
		{"request", func(s *SourceSnapshot) { s.RequestID = "other-request" }},
		{"hash", func(s *SourceSnapshot) { s.CommandHash = strings.Repeat("0", 64) }},
		{"handle", func(s *SourceSnapshot) { s.CredentialHandle = "different-handle" }},
		{"config", func(s *SourceSnapshot) { s.ConfigRevision = "" }},
		{"agent", func(s *SourceSnapshot) { s.Agent = "custom-executable" }},
		{"builtin-agent-mismatch", func(s *SourceSnapshot) { s.Agent = "claude" }},
		{"model", func(s *SourceSnapshot) { s.Model = "other-model" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, pair, req, challenge, snapshot := officialStationFixture(t)
			tc.mutate(&snapshot)
			raw, _ := json.Marshal(map[string]any{"officialAccount": snapshot})
			grant, err := h.officialSourceGrant(raw, challenge)
			if err != nil {
				return
			}
			req.SourceGrant = &grant
			if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code == "" {
				t.Fatal("invalid official assertion authorized")
			}
		})
	}
}

func TestStationOfficialFlagExpiryAndExtraSecretsFailClosed(t *testing.T) {
	h, pair, req, challenge, snapshot := officialStationFixture(t)
	raw, _ := json.Marshal(map[string]any{"officialAccount": snapshot})
	grant, err := h.officialSourceGrant(raw, challenge)
	if err != nil {
		t.Fatal(err)
	}
	req.SourceGrant = &grant
	if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code != "" {
		t.Fatal(code)
	}
	for _, malformed := range [][]byte{
		append(append([]byte(nil), raw[:len(raw)-1]...), []byte(`,"accessToken":"synthetic-token"}`)...),
		[]byte(`{"officialAccount":null}`),
		[]byte(strings.Replace(string(raw), `"officialAccount":`, `"OfficialAccount":`, 1)),
	} {
		if _, err := h.officialSourceGrant(malformed, challenge); err == nil {
			t.Fatal("extra/case-confused official assertion accepted")
		}
	}
	h.mu.Lock()
	h.sourceChallenges[challenge.ChallengeID].ExpiresAt = time.Now().Unix() - 1
	h.mu.Unlock()
	if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code != "source_challenge_expired" {
		t.Fatalf("expired challenge executed: %s", code)
	}
	p := h.CurrentRemoteAccessPolicy()
	p.Station.AllowOfficialAccounts = false
	if err := h.ApplyRemoteAccessPolicy(p, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := h.officialSourceGrant(raw, challenge); err == nil {
		t.Fatal("official disabled still signs")
	}
	if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code == "" {
		t.Fatal("official disabled still executes")
	}
}
