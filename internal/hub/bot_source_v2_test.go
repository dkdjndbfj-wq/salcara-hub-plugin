package hub

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

type botSourceFixture struct {
	h             *Hub
	pack          BotBindingPackage
	token, secret string
	device        Device
	conn          *bridgeConn
	req           commandReq
	private       ed25519.PrivateKey
}

func newBotSourceFixture(t *testing.T) *botSourceFixture {
	t.Helper()
	h, _, _ := botFixture(t)
	t.Cleanup(h.Close)
	h.durable.volumeProbe = healthyMailboxVolume
	pack, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	f := &botSourceFixture{h: h, pack: pack, secret: strings.Repeat("d", 64), device: botDevice("33333333-3333-4333-8333-333333333333", "Source test")}
	f.device.Capabilities = []string{"source.guard.v1", "source.grant.acquire.v1", "station.guard.v1", "station.guard.v2"}
	if w := botRegisterRequest(t, h, pack, f.device, f.secret); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	f.token = bindBotSourcePhone(t, h, pack)
	pub, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	f.private = private
	p := RemoteAccessPolicy{Mode: "restricted", Audience: "bot-source", Station: &StationPolicy{Version: 2, APIBases: []string{}, ExternalAPIBases: []string{"https://source.test"}, ReverseProxyAPIBases: []string{"https://source.test"}}, Issuers: []SourceIssuer{{ID: "issuer", PublicKey: base64.RawURLEncoding.EncodeToString(pub), SourceTypes: []string{"site_api", "verified_proxy"}, Origins: []string{"https://source.test"}, APIBases: []string{"https://source.test"}}}}
	if err := h.ApplyRemoteAccessPolicy(p, nil); err != nil {
		t.Fatal(err)
	}
	f.conn = &bridgeConn{cmds: make(chan []byte, 10), closed: make(chan struct{})}
	h.mu.Lock()
	h.accounts[deviceAccount(f.device.DeviceID)].devices[f.device.DeviceID].conn = f.conn
	h.mu.Unlock()
	f.req = commandReq{DeviceID: f.device.DeviceID, RequestID: receiptRequestID, Command: json.RawMessage(`{"type":"session.send","accountId":"credential-1","agent":"codex","controlSurface":"cli","sessionKey":"codex:s","model":"gpt"}`)}
	return f
}

func bindBotSourcePhone(t *testing.T, h *Hub, pack BotBindingPackage) string {
	t.Helper()
	w := botRequest(t, h, http.MethodPost, "/v1/bot/phone/bind", botV2Body(pack, strings.Repeat("a", 64)), nil)
	var result struct {
		BotToken string `json:"botToken"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.BotToken == "" {
		t.Fatalf("phone bind: %d %s", w.Code, w.Body.String())
	}
	return result.BotToken
}

func (f *botSourceFixture) request(path string, body any) *http.Request {
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	r.Header.Set("X-Salcara-Bot-Token", f.token)
	return r
}

func (f *botSourceFixture) challenge(t *testing.T) SourceChallenge {
	t.Helper()
	w := httptest.NewRecorder()
	f.h.handleSourceChallenge(w, f.request("/app/source/challenge", f.req), "bot:"+f.pack.GroupID)
	var result struct {
		ChallengeID string `json:"challengeId"`
	}
	if w.Code != 200 || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.ChallengeID == "" {
		t.Fatalf("challenge: %d %s", w.Code, w.Body.String())
	}
	f.h.mu.Lock()
	defer f.h.mu.Unlock()
	state := f.h.sourceChallenges[result.ChallengeID]
	if state.account != deviceAccount(f.device.DeviceID) || state.authenticationAccount() != "bot:"+f.pack.GroupID {
		t.Fatal("caller and dispatch identities conflated")
	}
	return state.SourceChallenge
}

func (f *botSourceFixture) sign(t *testing.T, challenge SourceChallenge, kind string) commandReq {
	t.Helper()
	req := f.req
	now := time.Now().Unix()
	snapshot := SourceSnapshot{Version: 1, Issuer: "issuer", Audience: challenge.Audience, DeviceID: challenge.DeviceID, BindingID: challenge.BindingID, CredentialHandle: "credential-1", Agent: "codex", ControlSurface: "cli", SourceType: kind, Origin: "https://source.test", APIBase: "https://source.test", ConfigRevision: "cfg-1", PolicyRevision: challenge.PolicyRevision, CommandHash: challenge.CommandHash, RequestID: challenge.RequestID, ChallengeID: challenge.ChallengeID, Model: "gpt", IssuedAt: now, ExpiresAt: now + 60}
	signSourceFixture(t, &req, snapshot, f.private)
	return req
}

func TestBotSourceV2AcquiresAndExecutesThroughAuthenticatedDevice(t *testing.T) {
	f := newBotSourceFixture(t)
	challenge := f.challenge(t)
	req := f.sign(t, challenge, "site_api")
	grant, _ := json.Marshal(req.SourceGrant)
	w := httptest.NewRecorder()
	done := make(chan struct{})
	go func() {
		f.h.handleSourceGrant(w, f.request("/app/source/grant", map[string]string{"challengeId": challenge.ChallengeID}), "bot:"+f.pack.GroupID)
		close(done)
	}()
	var env commandEnvelope
	select {
	case raw := <-f.conn.cmds:
		if json.Unmarshal(raw, &env) != nil {
			t.Fatal("bad envelope")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Bot grant acquisition was not dispatched")
	}
	if env.EventScope == "" || challenge.BindingID != shaHex("salcara-bot-source-v1\x00"+f.pack.GroupID+"\x00"+env.EventScope) {
		t.Fatal("source binding not tied to authenticated Bot event generation")
	}
	bridge := httptest.NewRequest(http.MethodGet, "/bridge/source/challenge?challengeId="+challenge.ChallengeID, nil)
	bridge.Header.Set("X-Salcara-Device-Id", f.device.DeviceID)
	bridge.Header.Set("X-Salcara-Device-Secret", f.secret)
	bw := httptest.NewRecorder()
	f.h.handleBridgeSourceChallenge(bw, bridge, deviceAccount(f.device.DeviceID))
	if bw.Code != 200 || !strings.Contains(bw.Body.String(), challenge.BindingID) {
		t.Fatalf("device could not fetch its Bot challenge: %d %s", bw.Code, bw.Body)
	}
	f.h.mu.Lock()
	pending := f.h.pending[env.CommandID]
	f.h.mu.Unlock()
	pending.ch <- replyBody{OK: true, Result: grant}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Bot grant acquisition stalled")
	}
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"sourceGrant"`) {
		t.Fatalf("grant: %d %s", w.Code, w.Body)
	}
	for _, kind := range []string{"site_api", "verified_proxy"} {
		signed := f.sign(t, challenge, kind)
		if code, source := f.h.executionSourceFor("bot:"+f.pack.GroupID, f.request("/app/commands", signed), signed); code != "" || source == nil || source.SourceType != kind {
			t.Fatalf("valid %s failed final source check: %s %+v", kind, code, source)
		}
	}
	// Exercise the real app command path, including receipt ownership and the
	// final authorization callback, rather than only evaluating the grant.
	commandResult := httptest.NewRecorder()
	commandDone := make(chan struct{})
	go func() {
		f.h.handleAppCommands(commandResult, f.request("/app/commands", req), "bot:"+f.pack.GroupID)
		close(commandDone)
	}()
	select {
	case raw := <-f.conn.cmds:
		if json.Unmarshal(raw, &env) != nil || env.Source == nil || env.Source.Mode != "restricted" {
			t.Fatal("missing checked source envelope")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("authorized Bot task was not delivered")
	}
	f.h.mu.Lock()
	pending = f.h.pending[env.CommandID]
	f.h.mu.Unlock()
	pending.ch <- replyBody{OK: true, Result: json.RawMessage(`{"sent":true}`)}
	select {
	case <-commandDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Bot task reply stalled")
	}
	if commandResult.Code != 200 || !strings.Contains(commandResult.Body.String(), `"sent":true`) {
		t.Fatalf("command: %d %s", commandResult.Code, commandResult.Body)
	}
	// Completed receipts remain readable to this caller after nonce retirement,
	// without replaying execution or requiring an independent QR pair.
	replay := httptest.NewRecorder()
	f.h.handleAppCommands(replay, f.request("/app/commands", req), "bot:"+f.pack.GroupID)
	if replay.Code != 200 || !strings.Contains(replay.Body.String(), `"sent":true`) {
		t.Fatalf("Bot receipt replay: %d %s", replay.Code, replay.Body)
	}
	select {
	case <-f.conn.cmds:
		t.Fatal("receipt replay sent task twice")
	default:
	}

	// Model reads use transient authorization rather than durable receipts.
	f.req.Command = json.RawMessage(`{"type":"models.list","accountId":"credential-1","agent":"codex","controlSurface":"cli","model":"gpt"}`)
	f.req.RequestID = "20000000-0000-4000-8000-000000000002"
	transientChallenge := f.challenge(t)
	transientReq := f.sign(t, transientChallenge, "site_api")
	transientResult, transientDone := httptest.NewRecorder(), make(chan struct{})
	go func() {
		f.h.handleAppCommands(transientResult, f.request("/app/commands", transientReq), "bot:"+f.pack.GroupID)
		close(transientDone)
	}()
	select {
	case raw := <-f.conn.cmds:
		if json.Unmarshal(raw, &env) != nil || env.Source == nil {
			t.Fatal("missing transient source")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Bot source read still required QR authorization")
	}
	f.h.mu.Lock()
	pending = f.h.pending[env.CommandID]
	f.h.mu.Unlock()
	pending.ch <- replyBody{OK: true, Result: json.RawMessage(`{"models":["gpt"]}`)}
	select {
	case <-transientDone:
	case <-time.After(2 * time.Second):
		t.Fatal("Bot source read stalled")
	}
	if transientResult.Code != 200 || !strings.Contains(transientResult.Body.String(), `"models"`) {
		t.Fatalf("transient: %d %s", transientResult.Code, transientResult.Body)
	}
}

func TestBotSourceV2RevocationDuringAcquisitionCannotReleaseGrant(t *testing.T) {
	f := newBotSourceFixture(t)
	challenge := f.challenge(t)
	req := f.sign(t, challenge, "site_api")
	grant, _ := json.Marshal(req.SourceGrant)
	request := f.request("/app/source/grant", map[string]string{"challengeId": challenge.ChallengeID})
	w, done := httptest.NewRecorder(), make(chan struct{})
	go func() { f.h.handleSourceGrant(w, request, "bot:"+f.pack.GroupID); close(done) }()
	var env commandEnvelope
	select {
	case raw := <-f.conn.cmds:
		if json.Unmarshal(raw, &env) != nil {
			t.Fatal("bad acquisition")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("no acquisition")
	}
	f.h.mu.Lock()
	pending := f.h.pending[env.CommandID]
	f.h.mu.Unlock()
	revoked := botRequest(t, f.h, http.MethodPost, "/v1/bot/phone/revoke", nil, map[string]string{"X-Salcara-Bot-Token": f.token})
	if revoked.Code != 200 {
		t.Fatal(revoked.Body)
	}
	f.pack = resetBotPhonePackage(t, f.h, f.pack.GroupID)
	f.token = bindBotSourcePhone(t, f.h, f.pack)
	pending.ch <- replyBody{OK: true, Result: grant}
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("revoked acquisition stalled")
	}
	if w.Code != 403 || strings.Contains(w.Body.String(), `"sourceGrant"`) {
		t.Fatalf("revoked caller received grant: %d %s", w.Code, w.Body)
	}
	f.h.mu.Lock()
	current := f.h.sourceChallenges[challenge.ChallengeID]
	f.h.mu.Unlock()
	if current != nil {
		t.Fatal("failed acquisition retained private command or grant")
	}
}

func TestBotSourceV2RevocationGroupIsolationAndPolicyChanges(t *testing.T) {
	f := newBotSourceFixture(t)
	challenge := f.challenge(t)
	req := f.sign(t, challenge, "verified_proxy")
	if code := f.h.evaluateSourcePolicy("bot:"+f.pack.GroupID, f.request("/app/commands", req), req); code != "" {
		t.Fatal(code)
	}
	p := f.h.CurrentRemoteAccessPolicy()
	p.Station.ReverseProxyAPIBases = nil
	if err := f.h.ApplyRemoteAccessPolicy(p, nil); err != nil {
		t.Fatal(err)
	}
	if code := f.h.evaluateSourcePolicy("bot:"+f.pack.GroupID, f.request("/app/commands", req), req); code == "" {
		t.Fatal("old proxy grant survived class revocation")
	}
	challenge = f.challenge(t)
	req = f.sign(t, challenge, "verified_proxy")
	if code := f.h.evaluateSourcePolicy("bot:"+f.pack.GroupID, f.request("/app/commands", req), req); code != "source_station_denied" {
		t.Fatalf("proxy borrowed API grant: %s", code)
	}
	req = f.sign(t, challenge, "site_api")
	oldToken := f.token
	w := botRequest(t, f.h, http.MethodPost, "/v1/bot/phone/revoke", nil, map[string]string{"X-Salcara-Bot-Token": oldToken})
	if w.Code != 200 {
		t.Fatal(w.Body)
	}
	if code := f.h.evaluateSourcePolicy("bot:"+f.pack.GroupID, f.request("/app/commands", req), req); code != "device_authorization_expired" {
		t.Fatalf("revoked phone accepted: %s", code)
	}
	f.pack = resetBotPhonePackage(t, f.h, f.pack.GroupID)
	f.token = bindBotSourcePhone(t, f.h, f.pack)
	if f.token == oldToken {
		t.Fatal("rebind reused credential")
	}
	if code := f.h.evaluateSourcePolicy("bot:"+f.pack.GroupID, f.request("/app/commands", req), req); code != "source_binding_mismatch" {
		t.Fatalf("replacement phone inherited old source: %s", code)
	}
	grantResult := httptest.NewRecorder()
	f.h.handleSourceGrant(grantResult, f.request("/app/source/grant", map[string]string{"challengeId": challenge.ChallengeID}), "bot:"+f.pack.GroupID)
	if grantResult.Code != 403 {
		t.Fatal("replacement phone recovered old grant")
	}
	newChallenge := f.challenge(t)
	if newChallenge.BindingID == challenge.BindingID {
		t.Fatal("Bot token rotation did not rotate source binding")
	}
	req = f.sign(t, newChallenge, "site_api")
	if code := f.h.evaluateSourcePolicy("bot:"+f.pack.GroupID, f.request("/app/commands", req), req); code != "" {
		t.Fatalf("new phone denied: %s", code)
	}
	p = f.h.CurrentRemoteAccessPolicy()
	p.Mode = "disabled"
	if err := f.h.ApplyRemoteAccessPolicy(p, nil); err != nil {
		t.Fatal(err)
	}
	if code := f.h.evaluateSourcePolicy("bot:"+f.pack.GroupID, f.request("/app/commands", req), req); code != "remote_execution_disabled" {
		t.Fatal(code)
	}
}

func TestBotSourceV2BindingSurvivesRestartButNotCrossGroupReuse(t *testing.T) {
	f := newBotSourceFixture(t)
	other, err := f.h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	if w := botRegisterRequest(t, f.h, other, f.device, f.secret); w.Code != 200 {
		t.Fatal(w.Body)
	}
	otherToken := bindBotSourcePhone(t, f.h, other)
	challenge := f.challenge(t)
	req := f.sign(t, challenge, "site_api")
	request := f.request("/app/commands", req)
	request.Header.Set("X-Salcara-Bot-Token", otherToken)
	if code := f.h.evaluateSourcePolicy("bot:"+f.pack.GroupID, request, req); code != "device_authorization_expired" {
		t.Fatalf("token authenticated as wrong group: %s", code)
	}
	if code := f.h.evaluateSourcePolicy("bot:"+other.GroupID, request, req); code != "source_binding_mismatch" {
		t.Fatalf("grant crossed groups: %s", code)
	}
	settings := f.h.cfg
	f.h.Close()
	restarted, err := New(settings)
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	restarted.mu.Lock()
	_, dev, binding := restarted.sourceIdentityLocked("bot:"+f.pack.GroupID, f.request("/app/commands", req), f.device.DeviceID)
	restarted.mu.Unlock()
	if dev == nil || binding != challenge.BindingID {
		t.Fatal("Bot source binding changed on same-credential restart")
	}
	if code := restarted.evaluateSourcePolicy("bot:"+f.pack.GroupID, f.request("/app/commands", req), req); code != "source_challenge_expired" {
		t.Fatalf("ephemeral old challenge survived restart: %s", code)
	}
}
