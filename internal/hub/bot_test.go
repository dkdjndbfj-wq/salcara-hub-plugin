package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func botFixture(t *testing.T) (*Hub, string, string) {
	t.Helper()
	dir := t.TempDir()
	h, err := New(Config{DeploymentMode: "bot", DataDir: dir, PublicURL: "https://bot.example/salcara-hub"})
	if err != nil {
		t.Fatal(err)
	}
	return h, dir, "https://bot.example/salcara-hub"
}

func botRequest(t *testing.T, h *Hub, method, path string, body any, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	r := httptest.NewRequest(method, "https://bot.example"+path, strings.NewReader(string(raw)))
	for k, v := range headers {
		r.Header.Set(k, v)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestBotBindingPackageAndPhoneLimits(t *testing.T) {
	h, dir, hubURL := botFixture(t)
	p, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	if p.HubURL != hubURL || !validBotPackage(p) {
		t.Fatalf("invalid binding package: %#v", p)
	}
	if _, err := os.Stat(filepath.Join(dir, botGroupFile)); err != nil {
		t.Fatal(err)
	}
	phoneID := strings.Repeat("a", 64)
	first := botRequest(t, h, http.MethodPost, "/v1/bot/phone/bind", map[string]any{"binding": p, "phoneId": phoneID}, nil)
	if first.Code != http.StatusOK {
		t.Fatalf("phone bind: %d %s", first.Code, first.Body.String())
	}
	second := botRequest(t, h, http.MethodPost, "/v1/bot/phone/bind", map[string]any{"binding": p, "phoneId": strings.Repeat("b", 64)}, nil)
	if second.Code != http.StatusConflict {
		t.Fatalf("second phone accepted: %d", second.Code)
	}
	var got struct {
		BotToken string `json:"botToken"`
	}
	if json.Unmarshal(first.Body.Bytes(), &got) != nil || !validHex(got.BotToken, 32) {
		t.Fatalf("missing bot token: %s", first.Body.String())
	}
	list := botRequest(t, h, http.MethodGet, "/v1/app/devices", nil, map[string]string{"X-Salcara-Bot-Token": got.BotToken})
	if list.Code != http.StatusOK {
		t.Fatalf("bot device list: %d %s", list.Code, list.Body.String())
	}
	// Restart restores the group record but never exposes its invite or phone token.
	h.Close()
	restarted, err := New(Config{DeploymentMode: "bot", DataDir: dir, PublicURL: hubURL})
	if err != nil {
		t.Fatal(err)
	}
	defer restarted.Close()
	if len(restarted.BotGroupSummaries()) != 1 || !restarted.BotGroupSummaries()[0]["phoneBound"].(bool) {
		t.Fatal("bot group did not survive restart")
	}
	restarted.mu.Lock()
	verifiedAfterRestart := restarted.botGroupForPackageLocked(p) != nil
	restarted.mu.Unlock()
	if !verifiedAfterRestart {
		t.Fatal("persisted Bot signing key did not verify the original package after restart")
	}
	if err := restarted.RevokeBotGroup(p.GroupID); err != nil {
		t.Fatal(err)
	}
	still := botRequest(t, restarted, http.MethodGet, "/v1/app/devices", nil, map[string]string{"X-Salcara-Bot-Token": got.BotToken})
	if still.Code != http.StatusOK {
		t.Fatalf("invite revoke unexpectedly revoked existing phone: %d", still.Code)
	}
	newPhone := botRequest(t, restarted, http.MethodPost, "/v1/bot/phone/bind", map[string]any{"binding": p, "phoneId": strings.Repeat("c", 64)}, nil)
	if newPhone.Code != http.StatusForbidden {
		t.Fatalf("revoked invite accepted new phone: %d", newPhone.Code)
	}
}

func TestBotBindingPackageRejectsWrongHubAndUnknownFields(t *testing.T) {
	h, _, _ := botFixture(t)
	defer h.Close()
	p, _ := h.CreateBotBindingPackage()
	p.HubURL = "https://other.example/salcara-hub"
	w := botRequest(t, h, http.MethodPost, "/v1/bot/phone/bind", map[string]any{"binding": p, "phoneId": strings.Repeat("a", 64)}, nil)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("wrong hub accepted: %d", w.Code)
	}
	raw := `{"binding":{"type":"salcara-hub-bot-binding","version":1,"hubUrl":"https://bot.example/salcara-hub","groupId":"x","invite":"` + strings.Repeat("a", 64) + `","signature":"` + strings.Repeat("b", 64) + `","extra":true},"phoneId":"` + strings.Repeat("a", 64) + `"}`
	req := httptest.NewRequest(http.MethodPost, "https://bot.example/v1/bot/phone/bind", strings.NewReader(raw))
	out := httptest.NewRecorder()
	h.ServeHTTP(out, req)
	if out.Code != http.StatusBadRequest {
		t.Fatalf("unknown field accepted: %d", out.Code)
	}
}

func botDevice(id, name string) Device {
	return Device{DeviceID: id, Name: name, OS: "linux", Version: "e2e", Tools: json.RawMessage(`[]`), Projects: json.RawMessage(`[]`), Capabilities: []string{"stream"}}
}

func botRegisterRequest(t *testing.T, h *Hub, p BotBindingPackage, d Device, secret string) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(d)
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "https://bot.example/v1/device/register", strings.NewReader(string(raw)))
	r.Header.Set("X-Salcara-Device-Id", d.DeviceID)
	r.Header.Set("X-Salcara-Device-Secret", secret)
	r.Header.Set("X-Salcara-Bot-Group", p.GroupID)
	r.Header.Set("X-Salcara-Bot-Invite", p.Invite)
	r.Header.Set("X-Salcara-Bot-Signature", p.Signature)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func TestBotSignatureTamperAndConcurrentComputerLimit(t *testing.T) {
	h, _, _ := botFixture(t)
	defer h.Close()
	p, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	// The package signature covers the Hub URL and invite metadata; changing
	// either field must not turn the JSON into a valid binding.
	tampered := p
	tampered.Signature = strings.Repeat("0", 64)
	if w := botRequest(t, h, http.MethodPost, "/v1/bot/phone/bind", map[string]any{"binding": tampered, "phoneId": strings.Repeat("a", 64)}, nil); w.Code != http.StatusForbidden {
		t.Fatalf("tampered package accepted: %d %s", w.Code, w.Body.String())
	}

	var wg sync.WaitGroup
	codes := make(chan int, 9)
	for i := 0; i < 9; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			id := "00000000-0000-4000-8000-" + fmt.Sprintf("%012d", i+1)
			w := botRegisterRequest(t, h, p, botDevice(id, fmt.Sprintf("电脑 %d", i+1)), fmt.Sprintf("%064x", i+1))
			codes <- w.Code
		}()
	}
	wg.Wait()
	close(codes)
	accepted := 0
	for code := range codes {
		if code == http.StatusOK {
			accepted++
		}
	}
	if accepted != maxBotComputers {
		t.Fatalf("concurrent Bot registration accepted %d computers, want %d", accepted, maxBotComputers)
	}
	if got := len(h.BotGroupSummaries()); got != 1 || h.BotGroupSummaries()[0]["computers"].(int) != maxBotComputers {
		t.Fatalf("unexpected Bot group summary: %#v", h.BotGroupSummaries())
	}
}

func TestBotGroupsCannotCrossReadOrCommand(t *testing.T) {
	h, _, _ := botFixture(t)
	defer h.Close()
	one, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	two, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	device := botDevice("33333333-3333-4333-8333-333333333333", "组一电脑")
	if w := botRegisterRequest(t, h, one, device, strings.Repeat("d", 64)); w.Code != http.StatusOK {
		t.Fatalf("computer registration: %d %s", w.Code, w.Body.String())
	}
	bindOne := botRequest(t, h, http.MethodPost, "/v1/bot/phone/bind", map[string]any{"binding": one, "phoneId": strings.Repeat("e", 64)}, nil)
	if bindOne.Code != http.StatusOK {
		t.Fatalf("group one phone registration: %d %s", bindOne.Code, bindOne.Body.String())
	}
	var onePhone struct {
		BotToken string `json:"botToken"`
	}
	if err := json.Unmarshal(bindOne.Body.Bytes(), &onePhone); err != nil {
		t.Fatal(err)
	}
	bindTwo := botRequest(t, h, http.MethodPost, "/v1/bot/phone/bind", map[string]any{"binding": two, "phoneId": strings.Repeat("f", 64)}, nil)
	if bindTwo.Code != http.StatusOK {
		t.Fatalf("group two phone registration: %d %s", bindTwo.Code, bindTwo.Body.String())
	}
	var twoPhone struct {
		BotToken string `json:"botToken"`
	}
	if err := json.Unmarshal(bindTwo.Body.Bytes(), &twoPhone); err != nil {
		t.Fatal(err)
	}
	other := botRequest(t, h, http.MethodGet, "/v1/app/devices", nil, map[string]string{"X-Salcara-Bot-Token": twoPhone.BotToken})
	if other.Code != http.StatusOK || strings.Contains(other.Body.String(), device.DeviceID) {
		t.Fatalf("cross-group device list leaked device: %d %s", other.Code, other.Body.String())
	}
	command := map[string]any{"deviceId": device.DeviceID, "requestId": "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", "command": map[string]any{"type": "session.ping"}}
	denied := botRequest(t, h, http.MethodPost, "/v1/app/commands", command, map[string]string{"X-Salcara-Bot-Token": twoPhone.BotToken})
	if denied.Code != http.StatusForbidden {
		t.Fatalf("cross-group command was not denied: %d %s", denied.Code, denied.Body.String())
	}
	allowed := botRequest(t, h, http.MethodPost, "/v1/app/commands", command, map[string]string{"X-Salcara-Bot-Token": onePhone.BotToken})
	if allowed.Code != http.StatusConflict || !strings.Contains(allowed.Body.String(), "computer_offline") {
		t.Fatalf("Bot command did not reach the device delivery boundary: %d %s", allowed.Code, allowed.Body.String())
	}
}

func TestBotPhoneRevocationLeavesComputerCredentialIndependent(t *testing.T) {
	h, _, _ := botFixture(t)
	defer h.Close()
	p, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	device := botDevice("44444444-4444-4444-8444-444444444444", "可独立撤销电脑")
	secret := strings.Repeat("1", 64)
	if w := botRegisterRequest(t, h, p, device, secret); w.Code != http.StatusOK {
		t.Fatalf("computer registration: %d %s", w.Code, w.Body.String())
	}
	bound := botRequest(t, h, http.MethodPost, "/v1/bot/phone/bind", map[string]any{"binding": p, "phoneId": strings.Repeat("2", 64)}, nil)
	if bound.Code != http.StatusOK {
		t.Fatalf("phone registration: %d %s", bound.Code, bound.Body.String())
	}
	var phone struct {
		BotToken string `json:"botToken"`
	}
	if err := json.Unmarshal(bound.Body.Bytes(), &phone); err != nil {
		t.Fatal(err)
	}
	revoked := botRequest(t, h, http.MethodPost, "/v1/bot/phone/revoke", nil, map[string]string{"X-Salcara-Bot-Token": phone.BotToken})
	if revoked.Code != http.StatusOK {
		t.Fatalf("phone revoke: %d %s", revoked.Code, revoked.Body.String())
	}
	if list := botRequest(t, h, http.MethodGet, "/v1/app/devices", nil, map[string]string{"X-Salcara-Bot-Token": phone.BotToken}); list.Code != http.StatusForbidden {
		t.Fatalf("revoked Bot phone still authorized: %d %s", list.Code, list.Body.String())
	}
	// The computer's own identity remains valid and can reconnect without the
	// one-time Bot invitation or the phone token.
	raw, _ := json.Marshal(device)
	r := httptest.NewRequest(http.MethodPost, "https://bot.example/v1/device/register", strings.NewReader(string(raw)))
	r.Header.Set("X-Salcara-Device-Id", device.DeviceID)
	r.Header.Set("X-Salcara-Device-Secret", secret)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("computer credential was revoked with phone: %d %s", w.Code, w.Body.String())
	}
}

func TestBotPhoneBindRollsBackWhenStateCannotBePersisted(t *testing.T) {
	h, dir, _ := botFixture(t)
	defer h.Close()
	p, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	// Point persistence at a missing parent after the invitation itself has
	// been committed. The failed bind must not occupy the one-phone slot.
	badDir := filepath.Join(dir, "not-a-directory")
	if err := os.WriteFile(badDir, []byte("block"), 0o600); err != nil {
		t.Fatal(err)
	}
	h.cfg.DataDir = badDir
	phoneID := strings.Repeat("a", 64)
	failed := botRequest(t, h, http.MethodPost, "/v1/bot/phone/bind", map[string]any{"binding": p, "phoneId": phoneID}, nil)
	if failed.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed bind status: %d %s", failed.Code, failed.Body.String())
	}
	h.cfg.DataDir = dir
	retried := botRequest(t, h, http.MethodPost, "/v1/bot/phone/bind", map[string]any{"binding": p, "phoneId": phoneID}, nil)
	if retried.Code != http.StatusOK {
		t.Fatalf("retry after failed persistence: %d %s", retried.Code, retried.Body.String())
	}
}
