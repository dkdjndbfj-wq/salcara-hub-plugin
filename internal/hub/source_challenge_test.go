package hub

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func issueSourceChallengeFixture(t *testing.T, h *Hub, pair *http.Request, req commandReq) SourceChallenge {
	t.Helper()
	h.mu.Lock()
	h.accounts["receipt-owner"].devices["pc"].info.Capabilities = []string{"source.guard.v1", "source.grant.acquire.v1", "station.guard.v1"}
	h.mu.Unlock()
	req.SourceGrant = nil
	body, _ := json.Marshal(req)
	r := httptest.NewRequest("POST", "/app/source/challenge", bytes.NewReader(body))
	r.Header = pair.Header.Clone()
	w := httptest.NewRecorder()
	h.handleSourceChallenge(w, r, "receipt-owner")
	if w.Code != 200 {
		t.Fatalf("challenge status=%d body=%s", w.Code, w.Body.String())
	}
	var out struct {
		ChallengeID string `json:"challengeId"`
	}
	if json.Unmarshal(w.Body.Bytes(), &out) != nil || out.ChallengeID == "" {
		t.Fatal(w.Body.String())
	}
	h.mu.Lock()
	challenge := h.sourceChallenges[out.ChallengeID].SourceChallenge
	h.mu.Unlock()
	return challenge
}

func TestSourceChallengeBindsOriginalCommandAndGrant(t *testing.T) {
	h, pair, req, snapshot, private := sourceContractFixture(t)
	challenge := issueSourceChallengeFixture(t, h, pair, req)
	if challenge.CommandHash != snapshot.CommandHash || challenge.RequestID != req.RequestID || challenge.BindingID != snapshot.BindingID || !bytes.Equal(challenge.Command, req.Command) {
		t.Fatalf("challenge lost frozen command: %#v", challenge)
	}
	snapshot.ChallengeID = challenge.ChallengeID
	signSourceFixture(t, &req, snapshot, private)
	if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code != "" {
		t.Fatal(code)
	}
	h.mu.Lock()
	h.sourceChallenges[challenge.ChallengeID].BindingID = "another-binding"
	h.mu.Unlock()
	if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code != "source_challenge_expired" {
		t.Fatalf("rebinding accepted: %s", code)
	}
}

func TestSourceChallengeAcquisitionValidatesSignedNonceAndClearsCommand(t *testing.T) {
	h, pair, req, snapshot, private := sourceContractFixture(t)
	challenge := issueSourceChallengeFixture(t, h, pair, req)
	snapshot.ChallengeID = challenge.ChallengeID
	signSourceFixture(t, &req, snapshot, private)
	grant, _ := json.Marshal(req.SourceGrant)
	body, _ := json.Marshal(map[string]string{"challengeId": challenge.ChallengeID})
	r := httptest.NewRequest("POST", "/app/source/grant", bytes.NewReader(body))
	r.Header = pair.Header.Clone()
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() { h.handleSourceGrant(w, r, "receipt-owner"); close(done) }()
	h.mu.Lock()
	conn := h.accounts["receipt-owner"].devices["pc"].conn
	h.mu.Unlock()
	var env commandEnvelope
	select {
	case raw := <-conn.cmds:
		if json.Unmarshal(raw, &env) != nil {
			t.Fatal("bad acquisition envelope")
		}
	case <-time.After(time.Second):
		t.Fatal("no acquisition")
	}
	if string(env.Command) != `{"challengeId":"`+challenge.ChallengeID+`","type":"source.grant.acquire"}` || env.BindingID != snapshot.BindingID {
		t.Fatalf("acquisition not narrow: %s", env.Command)
	}
	h.mu.Lock()
	pending := h.pending[env.CommandID]
	h.mu.Unlock()
	pending.ch <- replyBody{OK: true, Result: grant}
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("acquisition stalled")
	}
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"sourceGrant"`)) {
		t.Fatalf("status=%d %s", w.Code, w.Body.String())
	}
	h.mu.Lock()
	retained := len(h.sourceChallenges[challenge.ChallengeID].Command)
	h.mu.Unlock()
	if retained != 0 {
		t.Fatal("private command retained after acquisition")
	}
}

func TestSourceChallengeBridgeRequiresIndependentDeviceSecretAndActiveAcquisition(t *testing.T) {
	h, pair, req, _, _ := sourceContractFixture(t)
	challenge := issueSourceChallengeFixture(t, h, pair, req)
	const secret = "0000000000000000000000000000000000000000000000000000000000000000"
	h.mu.Lock()
	dev := h.accounts["receipt-owner"].devices["pc"]
	dev.hasSecret = true
	dev.secretHash = sha256.Sum256([]byte(secret))
	h.sourceChallenges[challenge.ChallengeID].acquiring = true
	h.mu.Unlock()
	request := httptest.NewRequest("GET", "/bridge/source/challenge?challengeId="+challenge.ChallengeID, nil)
	request.Header.Set("X-Salcara-Device-Id", "pc")
	w := httptest.NewRecorder()
	h.handleBridgeSourceChallenge(w, request, "receipt-owner")
	if w.Code != 403 {
		t.Fatal("missing device secret accepted")
	}
	request.Header.Set("X-Salcara-Device-Secret", secret)
	w = httptest.NewRecorder()
	h.handleBridgeSourceChallenge(w, request, "receipt-owner")
	if w.Code != 200 {
		t.Fatalf("valid proof refused: %d %s", w.Code, w.Body.String())
	}
	h.mu.Lock()
	h.sourceChallenges[challenge.ChallengeID].acquiring = false
	h.mu.Unlock()
	w = httptest.NewRecorder()
	h.handleBridgeSourceChallenge(w, request, "receipt-owner")
	if w.Code != 403 {
		t.Fatal("inactive acquisition accepted")
	}
}

func TestSourceChallengeCannotRunThroughOrdinaryCommandRoute(t *testing.T) {
	h, pair, req, _, _ := sourceContractFixture(t)
	req.Command = json.RawMessage(`{"type":"source.grant.acquire","challengeId":"caller-nonce","issuerURL":"https://attacker.test"}`)
	req.SourceGrant = nil
	body, _ := json.Marshal(req)
	r := httptest.NewRequest("POST", "/app/commands", bytes.NewReader(body))
	r.Header = pair.Header.Clone()
	w := httptest.NewRecorder()
	h.handleAppCommands(w, r, "receipt-owner")
	if w.Code != 403 || !bytes.Contains(w.Body.Bytes(), []byte(`source_command_unsupported`)) {
		t.Fatalf("ordinary grant bypass: %d %s", w.Code, w.Body.String())
	}
}

func TestSourceGrantRouteRejectsCallerIssuerAndAmbiguousChallenge(t *testing.T) {
	h, pair, req, _, _ := sourceContractFixture(t)
	challenge := issueSourceChallengeFixture(t, h, pair, req)
	for _, body := range []string{
		`{"challengeId":"` + challenge.ChallengeID + `","issuerURL":"https://attacker.test"}`,
		`{"challengeId":"` + challenge.ChallengeID + `","challengeId":"other"}`,
		`{"ChallengeId":"` + challenge.ChallengeID + `"}`,
		`{"challengeId":null}`,
	} {
		r := httptest.NewRequest("POST", "/app/source/grant", bytes.NewBufferString(body))
		r.Header = pair.Header.Clone()
		w := httptest.NewRecorder()
		h.handleSourceGrant(w, r, "receipt-owner")
		if w.Code != 400 {
			t.Fatalf("ambiguous grant input accepted: %d %s", w.Code, body)
		}
	}
}

func TestSourceChallengeInvalidatedByPolicyAndSweptAfterExpiry(t *testing.T) {
	h, pair, req, _, _ := sourceContractFixture(t)
	challenge := issueSourceChallengeFixture(t, h, pair, req)
	h.mu.Lock()
	dev := h.accounts["receipt-owner"].devices["pc"]
	h.cfg.RemoteAccess.Revision = "r3"
	staleAccepted := h.sourceChallengeCurrentLocked(challenge.ChallengeID, "receipt-owner", dev) != nil
	h.sweepSourceChallengesLocked(challenge.ExpiresAt)
	retained := len(h.sourceChallenges)
	h.mu.Unlock()
	if staleAccepted {
		t.Fatal("old policy challenge accepted")
	}
	if retained != 0 {
		t.Fatal("expired private command retained")
	}
}

func TestCompletedChallengeRetiresWhileDurableReceiptStillPreventsReplay(t *testing.T) {
	h, pair, req, snapshot, private := sourceContractFixture(t)
	challenge := issueSourceChallengeFixture(t, h, pair, req)
	snapshot.ChallengeID = challenge.ChallengeID
	signSourceFixture(t, &req, snapshot, private)
	body, _ := json.Marshal(req)
	post := func() *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "/app/commands", bytes.NewReader(body))
		r.Header = pair.Header.Clone()
		w := httptest.NewRecorder()
		h.handleAppCommands(w, r, "receipt-owner")
		return w
	}
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- post() }()
	h.mu.Lock()
	conn := h.accounts["receipt-owner"].devices["pc"].conn
	h.mu.Unlock()
	var env commandEnvelope
	select {
	case raw := <-conn.cmds:
		if json.Unmarshal(raw, &env) != nil {
			t.Fatal("bad execution envelope")
		}
	case <-time.After(time.Second):
		t.Fatal("missing execution")
	}
	h.mu.Lock()
	pending := h.pending[env.CommandID]
	h.mu.Unlock()
	pending.ch <- replyBody{OK: true, Result: json.RawMessage(`{"sent":true}`)}
	select {
	case w := <-done:
		if w.Code != 200 {
			t.Fatalf("execution rejected: %s", w.Body.String())
		}
	case <-time.After(time.Second):
		t.Fatal("execution stalled")
	}
	h.mu.Lock()
	_, retained := h.sourceChallenges[challenge.ChallengeID]
	h.mu.Unlock()
	if retained {
		t.Fatal("completed challenge kept consuming acquisition capacity")
	}
	w := post()
	if w.Code != 200 || !bytes.Contains(w.Body.Bytes(), []byte(`"sent":true`)) {
		t.Fatalf("retired challenge receipt not recoverable: %d %s", w.Code, w.Body.String())
	}
	select {
	case <-conn.cmds:
		t.Fatal("retired challenge request executed twice")
	default:
	}
}
