package hub

import (
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func botV2Body(p BotBindingPackage, phone string) map[string]any {
	return map[string]any{"binding": p, "phoneId": phone, "attemptId": "11111111-1111-4111-8111-111111111111", "phoneToken": hashText(p.GroupID + p.Invite + phone)}
}

func botResetInput(t *testing.T, h *Hub, groupID string) BotPhoneReset {
	t.Helper()
	for _, g := range h.BotGroupSummaries() {
		if g["groupId"] == groupID {
			id, err := randomHex(16)
			if err != nil {
				t.Fatal(err)
			}
			return BotPhoneReset{GroupID: groupID, ExpectedRevision: g["phoneRevision"].(string), RequestID: id[:8] + "-" + id[8:12] + "-4" + id[13:16] + "-8" + id[17:20] + "-" + id[20:], Confirm: true, Reason: "replace synthetic phone"}
		}
	}
	t.Fatal("missing group")
	return BotPhoneReset{}
}

func resetBotPhonePackage(t *testing.T, h *Hub, id string) BotBindingPackage {
	t.Helper()
	result, err := h.ResetBotPhone(botResetInput(t, h, id))
	if err != nil {
		t.Fatal(err)
	}
	return result.Binding
}

func requireBotCode(t *testing.T, w *httptest.ResponseRecorder, status int, code string) {
	t.Helper()
	if w.Code != status || code != "" && !strings.Contains(w.Body.String(), `"code":"`+code+`"`) {
		t.Fatalf("got %d %s; want %d %s", w.Code, w.Body, status, code)
	}
}

func TestBotPhoneV2EmptyRecoveryAndStrictProof(t *testing.T) {
	h, dir, url := botFixture(t)
	defer h.Close()
	p, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	body := botV2Body(p, strings.Repeat("a", 64))
	token := body["phoneToken"].(string)
	headers := map[string]string{"X-Salcara-Bot-Token": token}
	revision := h.BotGroupSummaries()[0]["phoneRevision"]
	legacy := botRequest(t, h, "POST", "/v1/bot/phone/bind", map[string]any{"binding": p, "phoneId": body["phoneId"]}, nil)
	requireBotCode(t, legacy, 409, "bot_group_empty")
	if h.BotGroupSummaries()[0]["phoneBound"] != false || h.BotGroupSummaries()[0]["phoneRevision"] != revision {
		t.Fatal("legacy empty consumed slot")
	}
	first := botRequest(t, h, "POST", "/v1/bot/phone/bind", body, nil)
	requireBotCode(t, first, 200, "")
	if !strings.Contains(first.Body.String(), `"devices":[]`) || !strings.Contains(first.Body.String(), token) {
		t.Fatal(first.Body)
	}
	newRevision := h.BotGroupSummaries()[0]["phoneRevision"]
	for key, replacement := range map[string]string{"phoneId": strings.Repeat("b", 64), "phoneToken": strings.Repeat("c", 64), "attemptId": "22222222-2222-4222-8222-222222222222"} {
		copyBody := botV2Body(p, strings.Repeat("a", 64))
		copyBody[key] = replacement
		requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", copyBody, nil), 409, "bot_phone_already_bound")
	}
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", body, nil), 200, "")
	if h.BotGroupSummaries()[0]["phoneRevision"] != newRevision {
		t.Fatal("retry changed revision")
	}
	h.Close()
	next, err := New(Config{DeploymentMode: "bot", DataDir: dir, PublicURL: url})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	requireBotCode(t, botRequest(t, next, "POST", "/v1/bot/phone/bind", body, nil), 200, "")
	requireBotCode(t, botRequest(t, next, "GET", "/v1/app/devices", nil, headers), 200, "")
	d := botDevice("00000000-0000-4000-8000-000000000001", "added later")
	requireBotCode(t, botRegisterRequest(t, next, p, d, strings.Repeat("d", 64)), 200, "")
	devices := botRequest(t, next, "GET", "/v1/app/devices", nil, headers)
	if !strings.Contains(devices.Body.String(), d.DeviceID) {
		t.Fatal("empty group did not discover new computer")
	}
	data, err := os.ReadFile(filepath.Join(dir, botGroupFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), token) || strings.Contains(string(data), body["phoneId"].(string)) || strings.Contains(string(data), p.Invite) {
		t.Fatal("raw credential persisted")
	}
}

func TestBotPhoneV2ParserRejectsAlternateForms(t *testing.T) {
	h, _, _ := botFixture(t)
	defer h.Close()
	p, _ := h.CreateBotBindingPackage()
	raw, _ := json.Marshal(botV2Body(p, strings.Repeat("a", 64)))
	text := string(raw)
	cases := []string{
		strings.Replace(text, `"attemptId":`, `"AttemptId":`, 1),
		strings.Replace(text, `"attemptId":"11111111-1111-4111-8111-111111111111"`, `"attemptId":null`, 1),
		strings.Replace(text, `"attemptId":"11111111-1111-4111-8111-111111111111",`, ``, 1),
		strings.Replace(text, `"phoneToken":`, `"phoneToken":"`+strings.Repeat("b", 64)+`","phoneToken":`, 1),
		strings.Replace(text, `"version":1`, `"version":1,"version":1`, 1),
		strings.Replace(text, `"groupId":`, `"GroupId":`, 1),
		strings.Replace(text, `"phoneToken":"`+botV2Body(p, strings.Repeat("a", 64))["phoneToken"].(string)+`"`, `"phoneToken":"`+strings.Repeat("A", 64)+`"`, 1),
		strings.Replace(text, `"attemptId":"11111111-1111-4111-8111-111111111111"`, `"attemptId":"11111111-1111-5111-8111-111111111111"`, 1),
		text + `{}`, `null`,
	}
	for i, raw := range cases {
		r := httptest.NewRequest("POST", "https://bot.example/v1/bot/phone/bind", strings.NewReader(raw))
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 400 {
			t.Fatalf("case %d accepted: %d %s", i, w.Code, w.Body)
		}
	}
	for _, headers := range []map[string]string{{"Authorization": "Bearer fake"}, {"X-Salcara-Bot-Token": ""}, {"X-Salcara-Pair-Token": ""}} {
		requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", botV2Body(p, strings.Repeat("a", 64)), headers), 400, "ambiguous_credentials")
	}
	if h.BotGroupSummaries()[0]["phoneBound"] != false {
		t.Fatal("invalid body consumed slot")
	}
}

func TestBotPhoneV2NoAuthorityBeforeDurableBindAndConcurrency(t *testing.T) {
	h, _, _ := botFixture(t)
	defer h.Close()
	p, _ := h.CreateBotBindingPackage()
	body := botV2Body(p, strings.Repeat("a", 64))
	headers := map[string]string{"X-Salcara-Bot-Token": body["phoneToken"].(string)}
	h.saveMu.Lock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- botRequest(t, h, "POST", "/v1/bot/phone/bind", body, nil) }()
	// A known client token cannot authenticate while the commit is waiting.
	for i := 0; i < 10; i++ {
		time.Sleep(time.Millisecond)
		requireBotCode(t, botRequest(t, h, "GET", "/v1/app/devices", nil, headers), 403, "bot_credentials_invalid")
	}
	h.saveMu.Unlock()
	requireBotCode(t, <-done, 200, "")
	requireBotCode(t, botRequest(t, h, "GET", "/v1/app/devices", nil, headers), 200, "")
	p2, _ := h.CreateBotBindingPackage()
	var wg sync.WaitGroup
	results := make(chan int, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results <- botRequest(t, h, "POST", "/v1/bot/phone/bind", botV2Body(p2, fmt.Sprintf("%064x", i+1)), nil).Code
		}(i)
	}
	wg.Wait()
	close(results)
	winners := 0
	for status := range results {
		if status == 200 {
			winners++
		} else if status != 409 {
			t.Fatalf("unexpected bind status %d", status)
		}
	}
	if winners != 1 {
		t.Fatalf("winners %d", winners)
	}
}

func TestBotPhoneV2CollisionAndRetiredTokenCannotResurrect(t *testing.T) {
	h, _, _ := botFixture(t)
	defer h.Close()
	p, _ := h.CreateBotBindingPackage()
	p2, _ := h.CreateBotBindingPackage()
	body := botV2Body(p, strings.Repeat("a", 64))
	token := body["phoneToken"].(string)
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", body, nil), 200, "")
	other := botV2Body(p2, strings.Repeat("b", 64))
	other["phoneToken"] = token
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", other, nil), 409, "bot_phone_already_bound")
	headers := map[string]string{"X-Salcara-Bot-Token": token}
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/revoke", nil, headers), 200, "")
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", body, nil), 403, "bot_credentials_invalid")
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", map[string]any{"binding": p, "phoneId": strings.Repeat("c", 64)}, nil), 403, "bot_credentials_invalid")
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", other, nil), 409, "bot_phone_already_bound")
	fresh := resetBotPhonePackage(t, h, p.GroupID)
	body["binding"] = fresh
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", body, nil), 409, "bot_phone_already_bound")
	newBody := botV2Body(fresh, strings.Repeat("c", 64))
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", newBody, nil), 200, "")
	// A completed old revoke is harmless even after a replacement phone binds.
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/revoke", nil, headers), 200, "")
	requireBotCode(t, botRequest(t, h, "GET", "/v1/app/devices", nil, headers), 403, "bot_credentials_invalid")
	requireBotCode(t, botRequest(t, h, "GET", "/v1/app/devices", nil, map[string]string{"X-Salcara-Bot-Token": newBody["phoneToken"].(string)}), 200, "")
}

func TestBotPhoneRevokeIntentSurvivesMainSaveFailureAndRestart(t *testing.T) {
	h, dir, url := botFixture(t)
	p, _ := h.CreateBotBindingPackage()
	body := botV2Body(p, strings.Repeat("a", 64))
	token := body["phoneToken"].(string)
	headers := map[string]string{"X-Salcara-Bot-Token": token}
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", body, nil), 200, "")
	unblock := blockBotRegistrationSave(t, h, botGroupFile)
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/revoke", nil, headers), 503, "bot_phone_revoke_pending")
	requireBotCode(t, botRequest(t, h, "GET", "/v1/app/devices", nil, headers), 503, "bot_phone_revoke_pending")
	intent, err := os.ReadFile(filepath.Join(dir, botPhoneIntentFile))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(intent), token) || strings.Contains(string(intent), p.Invite) {
		t.Fatal("intent contains raw secret")
	}
	// Close while replacement is still blocked: no graceful-shutdown repair.
	h.Close()
	unblock()
	next, err := New(Config{DeploymentMode: "bot", DataDir: dir, PublicURL: url})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	requireBotCode(t, botRequest(t, next, "GET", "/v1/app/devices", nil, headers), 503, "bot_phone_revoke_pending")
	requireBotCode(t, botRequest(t, next, "POST", "/v1/bot/phone/bind", body, nil), 403, "bot_credentials_invalid")
	requireBotCode(t, botRequest(t, next, "POST", "/v1/bot/phone/revoke", nil, headers), 200, "")
	requireBotCode(t, botRequest(t, next, "POST", "/v1/bot/phone/revoke", nil, headers), 200, "")
	requireBotCode(t, botRequest(t, next, "GET", "/v1/app/devices", nil, headers), 403, "bot_credentials_invalid")
}

func TestBotPhoneWholeVolumeFailureNeverAcknowledgesRevocation(t *testing.T) {
	h, dir, url := botFixture(t)
	p, _ := h.CreateBotBindingPackage()
	body := botV2Body(p, strings.Repeat("a", 64))
	headers := map[string]string{"X-Salcara-Bot-Token": body["phoneToken"].(string)}
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", body, nil), 200, "")
	unblockMain := blockBotRegistrationSave(t, h, botGroupFile)
	unblockJournal := blockBotRegistrationSave(t, h, botPhoneIntentFile)
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/revoke", nil, headers), 503, "bot_phone_revoke_pending")
	requireBotCode(t, botRequest(t, h, "GET", "/v1/app/devices", nil, headers), 503, "bot_phone_revoke_pending")
	h.Close()
	unblockJournal()
	unblockMain()
	next, err := New(Config{DeploymentMode: "bot", DataDir: dir, PublicURL: url})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	// No medium was writable, so no durable revocation can be claimed. The
	// caller must retain its pending token and explicitly retry to complete.
	requireBotCode(t, botRequest(t, next, "POST", "/v1/bot/phone/revoke", nil, headers), 200, "")
	requireBotCode(t, botRequest(t, next, "GET", "/v1/app/devices", nil, headers), 403, "bot_credentials_invalid")
}

func TestBotPhoneResetCASReplayRestartAndMembership(t *testing.T) {
	h, dir, url := botFixture(t)
	defer h.Close()
	p, _ := h.CreateBotBindingPackage()
	body := botV2Body(p, strings.Repeat("a", 64))
	headers := map[string]string{"X-Salcara-Bot-Token": body["phoneToken"].(string)}
	d := botDevice("00000000-0000-4000-8000-000000000001", "keep computer")
	requireBotCode(t, botRegisterRequest(t, h, p, d, strings.Repeat("d", 64)), 200, "")
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", body, nil), 200, "")
	in := botResetInput(t, h, p.GroupID)
	result, err := h.ResetBotPhone(in)
	if err != nil {
		t.Fatal(err)
	}
	if result.Binding.GroupID != p.GroupID || result.Binding.Invite == p.Invite || result.PhoneRevision == in.ExpectedRevision {
		t.Fatal("reset did not rotate in original group")
	}
	for i := 0; i < 2; i++ {
		again, err := h.ResetBotPhone(in)
		if err != nil || again != result {
			t.Fatalf("reset replay changed: %v", err)
		}
	}
	changed := in
	changed.Reason = "different payload"
	if _, err = h.ResetBotPhone(changed); err != ErrBotPhoneConflict {
		t.Fatalf("payload reuse: %v", err)
	}
	stale := in
	stale.RequestID = "22222222-2222-4222-8222-222222222222"
	if _, err = h.ResetBotPhone(stale); err != ErrBotPhoneConflict {
		t.Fatalf("stale revision: %v", err)
	}
	requireBotCode(t, botRequest(t, h, "GET", "/v1/app/devices", nil, headers), 403, "bot_credentials_invalid")
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", body, nil), 403, "bot_credentials_invalid")
	h.Close()
	next, err := New(Config{DeploymentMode: "bot", DataDir: dir, PublicURL: url})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	again, err := next.ResetBotPhone(in)
	if err != nil || again != result {
		t.Fatalf("restart replay changed %v", err)
	}
	newBody := botV2Body(result.Binding, strings.Repeat("b", 64))
	requireBotCode(t, botRequest(t, next, "POST", "/v1/bot/phone/bind", newBody, nil), 200, "")
	list := botRequest(t, next, "GET", "/v1/app/devices", nil, map[string]string{"X-Salcara-Bot-Token": newBody["phoneToken"].(string)})
	if list.Code != 200 || !strings.Contains(list.Body.String(), d.DeviceID) {
		t.Fatal("reset lost computer")
	}
	if _, err = next.ResetBotPhone(in); err != ErrBotPhoneConflict {
		t.Fatalf("old admin replay overwrote newly bound phone: %v", err)
	}
}

func TestBotPhoneResetPendingRetryAndRestart(t *testing.T) {
	h, dir, url := botFixture(t)
	p, _ := h.CreateBotBindingPackage()
	body := botV2Body(p, strings.Repeat("a", 64))
	headers := map[string]string{"X-Salcara-Bot-Token": body["phoneToken"].(string)}
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", body, nil), 200, "")
	in := botResetInput(t, h, p.GroupID)
	unblock := blockBotRegistrationSave(t, h, botGroupFile)
	if _, err := h.ResetBotPhone(in); err != ErrBotPhoneUnavailable {
		t.Fatalf("reset failure: %v", err)
	}
	requireBotCode(t, botRequest(t, h, "GET", "/v1/app/devices", nil, headers), 503, "bot_phone_revoke_pending")
	h.Close()
	unblock()
	next, err := New(Config{DeploymentMode: "bot", DataDir: dir, PublicURL: url})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	requireBotCode(t, botRequest(t, next, "GET", "/v1/app/devices", nil, headers), 503, "bot_phone_revoke_pending")
	result, err := next.ResetBotPhone(in)
	if err != nil {
		t.Fatal(err)
	}
	requireBotCode(t, botRequest(t, next, "POST", "/v1/bot/phone/bind", body, nil), 403, "bot_credentials_invalid")
	requireBotCode(t, botRequest(t, next, "POST", "/v1/bot/phone/bind", botV2Body(result.Binding, strings.Repeat("b", 64)), nil), 200, "")
}

func TestBotPhoneInvalidJournalFailsClosed(t *testing.T) {
	h, dir, url := botFixture(t)
	_, _ = h.CreateBotBindingPackage()
	h.Close()
	if err := os.WriteFile(filepath.Join(dir, botPhoneIntentFile), []byte(`{"version":1,"intents":[{}]}`), 0600); err != nil {
		t.Fatal(err)
	}
	if next, err := New(Config{DeploymentMode: "bot", DataDir: dir, PublicURL: url}); err == nil {
		next.Close()
		t.Fatal("invalid intent ignored")
	}
}

func TestBotPhoneLateBindCannotEscapeRevoke(t *testing.T) {
	h, _, _ := botFixture(t)
	defer h.Close()
	p, _ := h.CreateBotBindingPackage()
	body := botV2Body(p, strings.Repeat("a", 64))
	headers := map[string]string{"X-Salcara-Bot-Token": body["phoneToken"].(string)}
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/revoke", nil, headers), 403, "bot_credentials_invalid")
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", body, nil), 200, "")
	if err := h.RevokeBotGroup(p.GroupID); err != nil {
		t.Fatal(err)
	}
	// Invitation-only revoke does not prove the pending phone is unbound.
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", body, nil), 403, "bot_credentials_invalid")
	requireBotCode(t, botRequest(t, h, "GET", "/v1/app/devices", nil, headers), 200, "")
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/revoke", nil, headers), 200, "")
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", body, nil), 403, "bot_credentials_invalid")
}

func TestBotPhoneExactRecoveryPreservesQueuesAndScope(t *testing.T) {
	h, _, _ := botFixture(t)
	defer h.Close()
	p, _ := h.CreateBotBindingPackage()
	d := botDevice("same-attempt-computer", "same attempt")
	requireBotCode(t, botRegisterRequest(t, h, p, d, strings.Repeat("d", 64)), 200, "")
	body := botV2Body(p, strings.Repeat("a", 64))
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", body, nil), 200, "")
	h.mu.Lock()
	a := h.accounts[deviceAccount(d.DeviceID)]
	_, err := a.appendEventLocked(d.DeviceID, map[string]json.RawMessage{"type": json.RawMessage(`"message"`), "sessionKey": json.RawMessage(`"same-session"`), "text": json.RawMessage(`"keep legitimate queue"`)})
	if err != nil {
		h.mu.Unlock()
		t.Fatal(err)
	}
	conn := &bridgeConn{closed: make(chan struct{})}
	sub := &appSub{closed: make(chan struct{})}
	a.devices[d.DeviceID].conn = conn
	a.apps[sub] = struct{}{}
	a.eventBatches["keep"] = eventBatchReceipt{accepted: 1}
	scope, cache := h.durableScopeLocked(a.id, a.devices[d.DeviceID]), a.cacheBytes
	h.mu.Unlock()
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", body, nil), 200, "")
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.durableScopeLocked(a.id, a.devices[d.DeviceID]) != scope || a.events.n != 1 || len(a.sessions) != 1 || len(a.eventBatches) != 1 || a.cacheBytes != cache {
		t.Fatal("same attempt cleared legitimate generation state")
	}
	for _, closed := range []chan struct{}{sub.closed, conn.closed} {
		select {
		case <-closed:
			t.Fatal("recovery closed existing stream")
		default:
		}
	}
}

func TestBotPhoneStaleResetRequestCannotBlockPendingRecovery(t *testing.T) {
	h, _, _ := botFixture(t)
	defer h.Close()
	p, _ := h.CreateBotBindingPackage()
	first := botResetInput(t, h, p.GroupID)
	second := first
	second.RequestID = "22222222-2222-4222-8222-222222222222"
	unblock := blockBotRegistrationSave(t, h, botGroupFile)
	if _, err := h.ResetBotPhone(first); err != ErrBotPhoneUnavailable {
		t.Fatal(err)
	}
	if _, err := h.ResetBotPhone(second); err != ErrBotPhoneConflict {
		t.Fatalf("stale competing tab hid recoverable request: %v", err)
	}
	unblock()
	if _, err := h.ResetBotPhone(first); err != nil {
		t.Fatal(err)
	}
	latest := botResetInput(t, h, p.GroupID)
	if _, err := h.ResetBotPhone(latest); err != nil {
		t.Fatal(err)
	}
	reused := botResetInput(t, h, p.GroupID)
	reused.RequestID = first.RequestID
	if _, err := h.ResetBotPhone(reused); err != ErrBotPhoneConflict {
		t.Fatalf("old request id accepted with new payload: %v", err)
	}
}

func TestBotPhoneStaleIntentDoesNotOverwriteLaterBinding(t *testing.T) {
	h, dir, url := botFixture(t)
	p, _ := h.CreateBotBindingPackage()
	body := botV2Body(p, strings.Repeat("a", 64))
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", body, nil), 200, "")
	in := botResetInput(t, h, p.GroupID)
	unblock := blockBotRegistrationSave(t, h, botGroupFile)
	if _, err := h.ResetBotPhone(in); err != ErrBotPhoneUnavailable {
		t.Fatal(err)
	}
	stale, err := os.ReadFile(filepath.Join(dir, botPhoneIntentFile))
	if err != nil {
		t.Fatal(err)
	}
	unblock()
	result, err := h.ResetBotPhone(in)
	if err != nil {
		t.Fatal(err)
	}
	requireBotCode(t, botRequest(t, h, "POST", "/v1/bot/phone/bind", botV2Body(result.Binding, strings.Repeat("b", 64)), nil), 200, "")
	h.Close()
	if err := os.WriteFile(filepath.Join(dir, botPhoneIntentFile), stale, 0600); err != nil {
		t.Fatal(err)
	}
	if next, err := New(Config{DeploymentMode: "bot", DataDir: dir, PublicURL: url}); err == nil {
		next.Close()
		t.Fatal("stale intent overwrote later credential")
	}
}
