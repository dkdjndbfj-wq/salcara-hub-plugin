package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

const diagnosticTestNonce = "diagnostic-fixture-123456789"

func diagnosticFixture(t *testing.T) (*Hub, *device, string, string) {
	t.Helper()
	h := newDeviceHub(t, Config{CommandTimeout: 100 * time.Millisecond})
	secret := strings.Repeat("a", 64)
	enrollDevice(t, h, "diagnostic-pc", secret)
	dev := onlineDevice(t, h, "diagnostic-pc")
	token := claimQR(t, h, "diagnostic-pc", issueQR(t, h, "diagnostic-pc", secret))
	return h, dev, token, secret
}
func diagnosticCall(t *testing.T, h *Hub, token, target string) *httptest.ResponseRecorder {
	t.Helper()
	return deviceRequest(t, h, "/app/diagnostics", "", "", token, map[string]string{"deviceId": "diagnostic-pc", "nonce": diagnosticTestNonce, "target": target}, "")
}
func diagnosticReply(t *testing.T, h *Hub, dev *device, secret string, result func(string) any) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		select {
		case raw := <-dev.conn.cmds:
			var env struct {
				CommandID string `json:"commandId"`
				Command   struct {
					Type  string `json:"type"`
					Nonce string `json:"nonce"`
				} `json:"command"`
			}
			if json.Unmarshal(raw, &env) != nil || env.Command.Type != "device.ping" || env.Command.Nonce != diagnosticTestNonce {
				t.Error("unexpected diagnostic command")
				return
			}
			time.Sleep(2 * time.Millisecond)
			deviceRequest(t, h, "/bridge/reply", "diagnostic-pc", secret, "", map[string]any{"deviceId": "diagnostic-pc", "commandId": env.CommandID, "ok": true, "result": result(env.Command.Nonce)}, "")
		case <-time.After(time.Second):
			t.Error("diagnostic was not dispatched")
		}
	}()
	return done
}
func TestAppDiagnosticsHubMeasuresOnlyOwnedMinimalExchange(t *testing.T) {
	h, dev, token, _ := diagnosticFixture(t)
	w := diagnosticCall(t, h, token, "hub")
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body)
	}
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	if len(out) != 7 || out["nonce"] != diagnosticTestNonce || out["deviceOnline"] != true || out["target"] != "hub" {
		t.Fatalf("unexpected public fields: %v", out)
	}
	if len(dev.conn.cmds) != 0 || h.Stats().CommandSuccess != 0 {
		t.Fatal("Hub target dispatched a command")
	}
	for _, secret := range []string{token, "secret", "projects", "subscription", "device_ref"} {
		if strings.Contains(w.Body.String(), secret) {
			t.Fatal("diagnostic leaked metadata")
		}
	}
	if w = diagnosticCall(t, h, token, "hub"); w.Code != 429 || w.Header().Get("Retry-After") != "5" {
		t.Fatal("Hub probe not rate limited")
	}
}
func TestAppDiagnosticsRequiresPairAndExactDevice(t *testing.T) {
	h, dev, token, _ := diagnosticFixture(t)
	for _, credential := range []string{"", strings.Repeat("b", 64)} {
		if w := diagnosticCall(t, h, credential, "device"); w.Code == 200 {
			t.Fatal("unauthorized diagnostic")
		}
	}
	other := deviceRequest(t, h, "/app/diagnostics", "", "", token, map[string]string{"deviceId": "another-pc", "nonce": diagnosticTestNonce, "target": "device"}, "")
	if other.Code != 403 || len(dev.conn.cmds) != 0 {
		t.Fatalf("cross-device admitted: %d", other.Code)
	}
}
func TestAppDiagnosticsRejectsAmbiguousPayloadAndQueries(t *testing.T) {
	h, _, token, _ := diagnosticFixture(t)
	for _, body := range []string{
		`{"deviceId":"diagnostic-pc","nonce":"short","target":"hub"}`,
		`{"deviceId":"diagnostic-pc","nonce":"diagnostic-fixture-123456789","target":"hub","type":"session.start"}`,
		`{"deviceId":"diagnostic-pc","nonce":"diagnostic-fixture-123456789","target":"hub","target":"device"}`,
		`{"deviceId":"diagnostic-pc","nonce":"diagnostic-fixture-123456789","target":"shell"}`,
	} {
		r := httptest.NewRequest("POST", "/salcara-hub/v1/app/diagnostics", strings.NewReader(body))
		r.Header.Set("X-Salcara-Pair-Token", token)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("accepted invalid body: %d", w.Code)
		}
	}
	r := httptest.NewRequest("POST", "/salcara-hub/v1/app/diagnostics?token=private", bytes.NewBufferString(`{}`))
	r.Header.Set("X-Salcara-Pair-Token", token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != 400 || strings.Contains(w.Body.String(), "private") {
		t.Fatal("query was not rejected")
	}
}
func TestAppDiagnosticsEchoValidatesNonceAndReturnsOnlyMeasuredDuration(t *testing.T) {
	h, dev, token, secret := diagnosticFixture(t)
	done := diagnosticReply(t, h, dev, secret, func(nonce string) any { return map[string]any{"nonce": nonce, "receivedAt": 1} })
	w := diagnosticCall(t, h, token, "device")
	<-done
	var out map[string]any
	json.Unmarshal(w.Body.Bytes(), &out)
	if w.Code != 200 || len(out) != 6 || out["nonce"] != diagnosticTestNonce {
		t.Fatalf("unexpected reply %d %s", w.Code, w.Body)
	}
	ms, ok := out["hubDeviceMs"].(float64)
	if !ok || ms < 1 || ms > 1000 {
		t.Fatalf("not locally measured: %v", out)
	}
	if strings.Contains(w.Body.String(), "receivedAt") {
		t.Fatal("computer wall clock exposed instead of measured RTT")
	}
	if w = diagnosticCall(t, h, token, "device"); w.Code != 429 {
		t.Fatal("repeated echo not limited")
	}
}
func TestAppDiagnosticsMalformedEchoNeverReleasesUntrustedContent(t *testing.T) {
	for _, result := range []func(string) any{
		func(string) any { return map[string]any{"nonce": "wrong", "receivedAt": 1} },
		func(n string) any { return map[string]any{"nonce": n} },
		func(n string) any { return map[string]any{"nonce": n, "receivedAt": -1} },
		func(n string) any { return map[string]any{"nonce": n, "receivedAt": 1, "secret": "private-leak"} },
	} {
		h, dev, token, secret := diagnosticFixture(t)
		done := diagnosticReply(t, h, dev, secret, result)
		w := diagnosticCall(t, h, token, "device")
		<-done
		if w.Code != 502 || strings.Contains(w.Body.String(), "private-leak") || strings.Contains(w.Body.String(), "hubDeviceMs") {
			t.Fatalf("bad echo accepted %d %s", w.Code, w.Body)
		}
	}
}
func TestAppDiagnosticsOfflineTimeoutAndGlobalAdmission(t *testing.T) {
	t.Run("offline", func(t *testing.T) {
		h, dev, token, _ := diagnosticFixture(t)
		dev.conn = nil
		w := diagnosticCall(t, h, token, "device")
		if w.Code != 409 || !strings.Contains(w.Body.String(), "diagnostic_device_offline") {
			t.Fatal(w.Body.String())
		}
	})
	t.Run("timeout", func(t *testing.T) {
		h, _, token, _ := diagnosticFixture(t)
		w := diagnosticCall(t, h, token, "device")
		if w.Code != 504 || h.diagnostics.active != 0 || len(h.pending) != 0 {
			t.Fatalf("timeout leaked active work %d %s", w.Code, w.Body)
		}
	})
	t.Run("global", func(t *testing.T) {
		h, dev, token, _ := diagnosticFixture(t)
		h.diagnostics.active = diagnosticMaxActive
		w := diagnosticCall(t, h, token, "device")
		if w.Code != 503 || len(dev.conn.cmds) != 0 {
			t.Fatal("global limit exceeded")
		}
	})
}
func TestAppDiagnosticsRevocationAndCancellationFenceInflightReplies(t *testing.T) {
	for _, cancelOnly := range []bool{false, true} {
		h, dev, token, secret := diagnosticFixture(t)
		body, _ := json.Marshal(map[string]string{"deviceId": "diagnostic-pc", "nonce": diagnosticTestNonce, "target": "device"})
		ctx, cancel := context.WithCancel(context.Background())
		r := httptest.NewRequest("POST", "/salcara-hub/v1/app/diagnostics", bytes.NewReader(body)).WithContext(ctx)
		r.Header.Set("X-Salcara-Pair-Token", token)
		w := httptest.NewRecorder()
		done := make(chan struct{})
		go func() { h.ServeHTTP(w, r); close(done) }()
		raw := <-dev.conn.cmds
		var env struct {
			CommandID string `json:"commandId"`
		}
		json.Unmarshal(raw, &env)
		if cancelOnly {
			cancel()
		} else {
			h.mu.Lock()
			h.revokePairLocked(deviceAccount("diagnostic-pc"), dev)
			h.mu.Unlock()
		}
		deviceRequest(t, h, "/bridge/reply", "diagnostic-pc", secret, "", map[string]any{"deviceId": "diagnostic-pc", "commandId": env.CommandID, "ok": true, "result": map[string]any{"nonce": diagnosticTestNonce, "receivedAt": 1}}, "")
		<-done
		cancel()
		if !cancelOnly && w.Code != 403 {
			t.Fatalf("revoked success escaped: %d %s", w.Code, w.Body)
		}
		if strings.Contains(w.Body.String(), "hubDeviceMs") || h.diagnostics.active != 0 || len(h.pending) != 0 {
			t.Fatal("revocation/cancel left a result or reservation")
		}
	}
}

func TestAppDiagnosticsBotRequiresExactGroupOwnership(t *testing.T) {
	h, _, _ := botFixture(t)
	defer h.Close()
	one, _ := h.CreateBotBindingPackage()
	two, _ := h.CreateBotBindingPackage()
	dev := botDevice("33333333-3333-4333-8333-333333333333", "电脑")
	if w := botRegisterRequest(t, h, one, dev, strings.Repeat("d", 64)); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	for i, group := range []BotBindingPackage{one, two} {
		bound := botRequest(t, h, "POST", "/v1/bot/phone/bind", botV2Body(group, strings.Repeat(string(rune('a'+i)), 64)), nil)
		var phone struct {
			BotToken string `json:"botToken"`
		}
		if bound.Code != 200 || json.Unmarshal(bound.Body.Bytes(), &phone) != nil {
			t.Fatal(bound.Body.String())
		}
		w := botRequest(t, h, "POST", "/v1/app/diagnostics", map[string]string{"deviceId": dev.DeviceID, "nonce": diagnosticTestNonce, "target": "hub"}, map[string]string{"X-Salcara-Bot-Token": phone.BotToken})
		want := 200
		if i == 1 {
			want = 403
		}
		if w.Code != want {
			t.Fatalf("group %d: %d %s", i, w.Code, w.Body)
		}
	}
}

func TestAppDiagnosticsOwnerAdmissionIsBoundedAndPrunesOnlyIdleSlots(t *testing.T) {
	h, _, token, _ := diagnosticFixture(t)
	h.diagnostics.owners = make(map[*device]*diagnosticSlot)
	for i := 0; i < diagnosticMaxOwners; i++ {
		h.diagnostics.owners[&device{}] = &diagnosticSlot{touched: time.Now()}
	}
	if w := diagnosticCall(t, h, token, "hub"); w.Code != 503 {
		t.Fatalf("admission cap ignored: %d", w.Code)
	}
	for _, slot := range h.diagnostics.owners {
		slot.touched = time.Now().Add(-11 * time.Minute)
	}
	active := &device{}
	h.diagnostics.owners[active] = &diagnosticSlot{active: true, touched: time.Now().Add(-11 * time.Minute)}
	if w := diagnosticCall(t, h, token, "hub"); w.Code != 200 {
		t.Fatal(w.Body.String())
	}
	if len(h.diagnostics.owners) != 2 || h.diagnostics.owners[active] == nil {
		t.Fatal("pruning removed active owner or retained expired owners")
	}
}

func TestAppDiagnosticsConcurrentRequestsAdmitOnlyOneEcho(t *testing.T) {
	h, dev, token, _ := diagnosticFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	body, _ := json.Marshal(map[string]string{"deviceId": "diagnostic-pc", "nonce": diagnosticTestNonce, "target": "device"})
	r := httptest.NewRequest("POST", "/salcara-hub/v1/app/diagnostics", bytes.NewReader(body)).WithContext(ctx)
	r.Header.Set("X-Salcara-Pair-Token", token)
	done := make(chan struct{})
	go func() { h.ServeHTTP(httptest.NewRecorder(), r); close(done) }()
	<-dev.conn.cmds
	if w := diagnosticCall(t, h, token, "device"); w.Code != 429 || len(dev.conn.cmds) != 0 {
		t.Fatalf("concurrent echo admitted: %d", w.Code)
	}
	cancel()
	<-done
	if h.diagnostics.active != 0 {
		t.Fatal("reservation leaked")
	}
}
