package hub

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func botScopePhone(t *testing.T, h *Hub, id string) (BotBindingPackage, string) {
	t.Helper()
	p, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	if w := botRegisterRequest(t, h, p, botDevice(id, "Scope fixture"), strings.Repeat("d", 64)); w.Code != http.StatusOK {
		t.Fatalf("register computer: %d %s", w.Code, w.Body)
	}
	return p, bindBotSourcePhone(t, h, p)
}

func TestBotSharedAuthPendingAllAppRoutes(t *testing.T) {
	h, _, _ := botFixture(t)
	t.Cleanup(h.Close)
	_, token := botScopePhone(t, h, "scope-pending-computer")
	headers := map[string]string{"X-Salcara-Bot-Token": token}
	if w := botRequest(t, h, http.MethodGet, "/v1/app/devices", nil, headers); w.Code != http.StatusOK {
		t.Fatalf("legitimate phone: %d %s", w.Code, w.Body)
	}
	unblock := blockBotRegistrationSave(t, h, botGroupFile)
	if w := botRequest(t, h, http.MethodPost, "/v1/bot/phone/revoke", nil, headers); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("revoke must remain pending: %d %s", w.Code, w.Body)
	}
	for path, route := range h.routes {
		if !route.auth || !strings.HasPrefix(path, "/app/") {
			continue
		}
		for _, method := range strings.Split(route.method, ", ") {
			t.Run(method+path, func(t *testing.T) {
				w := botRequest(t, h, method, "/v1"+path, nil, headers)
				var result struct {
					Code      string `json:"code"`
					Retryable bool   `json:"retryable"`
				}
				if w.Code != http.StatusServiceUnavailable || json.Unmarshal(w.Body.Bytes(), &result) != nil || result.Code != "bot_phone_revoke_pending" || !result.Retryable {
					t.Fatalf("pending credential lost through shared auth: %d %s", w.Code, w.Body)
				}
			})
		}
	}
	unblock()
	if w := botRequest(t, h, http.MethodPost, "/v1/bot/phone/revoke", nil, headers); w.Code != http.StatusOK {
		t.Fatalf("same token could not finish revoke: %d %s", w.Code, w.Body)
	}
	if w := botRequest(t, h, http.MethodGet, "/v1/app/devices", nil, headers); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "bot_credentials_invalid") {
		t.Fatalf("retired token retained authority: %d %s", w.Code, w.Body)
	}
}

func TestBotSharedAuthRejectsAmbiguousModes(t *testing.T) {
	h, _, _ := botFixture(t)
	t.Cleanup(h.Close)
	_, token := botScopePhone(t, h, "scope-headers-computer")
	for _, name := range []string{"Authorization", "X-Salcara-Pair-Token", "X-Salcara-Device-Secret", "X-Salcara-Device-Id", "X-Salcara-Bot-Invite", "X-Salcara-Enrollment-Claim"} {
		t.Run(name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodGet, "/v1/app/devices", nil)
			r.Header.Set("X-Salcara-Bot-Token", token)
			r.Header.Set(name, strings.Repeat("a", 64))
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "ambiguous_credentials") {
				t.Fatalf("mixed credentials accepted: %d %s", w.Code, w.Body)
			}
		})
	}
	for _, tokens := range [][]string{{token, token}, {token, strings.Repeat("a", 64)}, {""}} {
		r := httptest.NewRequest(http.MethodGet, "/v1/app/devices", nil)
		for _, value := range tokens {
			r.Header.Add("X-Salcara-Bot-Token", value)
		}
		if len(tokens) == 1 {
			r.Header.Set("Authorization", "Bearer other-mode")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "ambiguous_credentials") {
			t.Fatalf("duplicate/empty Bot header selected another auth mode: %d %s", w.Code, w.Body)
		}
	}
}

func TestBotScopeRetiresRuntimeCachesAndSubscriptions(t *testing.T) {
	h, _, _ := botFixture(t)
	t.Cleanup(h.Close)
	first, _ := botScopePhone(t, h, "scope-first-computer")
	_, _ = botScopePhone(t, h, "scope-other-computer")
	h.mu.Lock()
	defer h.mu.Unlock()
	a, other := h.accounts[deviceAccount("scope-first-computer")], h.accounts[deviceAccount("scope-other-computer")]
	for _, acct := range []*account{a, other} {
		for id := range acct.devices {
			if _, err := acct.appendEventLocked(id, map[string]json.RawMessage{"type": json.RawMessage(`"message"`), "sessionKey": json.RawMessage(`"scope-session"`), "text": json.RawMessage(`"old-generation-only"`)}); err != nil {
				t.Fatal(err)
			}
			acct.eventBatches["old-batch"] = eventBatchReceipt{digest: "old", accepted: 1, lastSeq: acct.seq}
			acct.eventBatchOrder = []string{"old-batch"}
			acct.devices[id].conn = &bridgeConn{closed: make(chan struct{})}
			acct.devices[id].standby = &bridgeConn{closed: make(chan struct{})}
		}
	}
	sub, otherSub := &appSub{closed: make(chan struct{})}, &appSub{closed: make(chan struct{})}
	a.apps[sub], other.apps[otherSub] = struct{}{}, struct{}{}
	wake, otherWake := a.wakeChanLocked(), other.wakeChanLocked()
	bridge, standby := a.devices["scope-first-computer"].conn, a.devices["scope-first-computer"].standby
	otherBytes := other.cacheBytes
	h.notifyBotScopeChangedLocked(h.botGroups[first.GroupID])
	for name, closed := range map[string]<-chan struct{}{"app": sub.closed, "bridge": bridge.closed, "standby": standby.closed, "wait": wake} {
		select {
		case <-closed:
		default:
			t.Fatalf("%s retained the old phone generation", name)
		}
	}
	if len(a.apps) != 0 || a.events.n != 0 || a.events.bytes != 0 || len(a.sessions) != 0 || len(a.eventBatches) != 0 || len(a.eventBatchOrder) != 0 || a.cacheBytes != 0 || a.cacheEntry != nil || a.sessionEvictedThrough != a.seq || a.events.evictedThrough != a.seq {
		t.Fatal("old generation still has RAM replay or stale cache accounting")
	}
	if h.eventCacheBytes != otherBytes || other.cacheBytes != otherBytes || other.events.n != 1 || len(other.sessions) != 1 || len(other.eventBatches) != 1 || len(other.apps) != 1 {
		t.Fatal("unrelated computer cache or subscription was changed")
	}
	for _, open := range []<-chan struct{}{otherSub.closed, otherWake, other.devices["scope-other-computer"].conn.closed} {
		select {
		case <-open:
			t.Fatal("unrelated computer subscription was closed")
		default:
		}
	}
	if len(a.devices) != 1 || len(h.botGroups[first.GroupID].Computers) != 1 {
		t.Fatal("phone generation change removed the computer")
	}
}

func TestBotScopeRetiresCapturedAppReplay(t *testing.T) {
	h, _, _ := botFixture(t)
	t.Cleanup(h.Close)
	_, token := botScopePhone(t, h, "scope-sse-computer")
	h.mu.Lock()
	a := h.accounts[deviceAccount("scope-sse-computer")]
	_, err := a.appendEventLocked("scope-sse-computer", map[string]json.RawMessage{"type": json.RawMessage(`"message"`), "text": json.RawMessage(`"private-old-generation-replay"`)})
	h.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/v1/app/stream?deviceId=scope-sse-computer", nil)
	r.Header.Set("X-Salcara-Bot-Token", token)
	w, _, done := holdBlockedSSERequest(t, h, r, false)
	if response := botRequest(t, h, http.MethodPost, "/v1/bot/phone/revoke", nil, map[string]string{"X-Salcara-Bot-Token": token}); response.Code != http.StatusOK {
		t.Fatalf("phone revoke: %d %s", response.Code, response.Body)
	}
	w.released.Do(func() { close(w.release) })
	awaitSSESignal(t, done, "revoked App stream waited for its ping interval")
	if strings.Contains(w.Body.String(), "private-old-generation-replay") {
		t.Fatal("replay captured before revoke escaped after stream retirement")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.appStreams != 0 || a.appStreams != 0 || len(a.apps) != 0 {
		t.Fatal("retired stream leaked its admission budget")
	}
}

func TestBotSharedAuthPendingWakesExistingEventWait(t *testing.T) {
	h, _, _ := botFixture(t)
	t.Cleanup(h.Close)
	_, token := botScopePhone(t, h, "scope-wait-computer")
	r := httptest.NewRequest(http.MethodGet, "/v1/app/events?deviceId=scope-wait-computer&scope=device&wait=25", nil)
	r.Header.Set("X-Salcara-Bot-Token", token)
	read := holdHTTPRead(t, h, r)
	awaitReadState(t, h, func() bool { return h.eventWaiters == 1 })
	unblock := blockBotRegistrationSave(t, h, botGroupFile)
	if w := botRequest(t, h, http.MethodPost, "/v1/bot/phone/revoke", nil, map[string]string{"X-Salcara-Bot-Token": token}); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("revoke did not remain pending: %d %s", w.Code, w.Body)
	}
	w := awaitPhoneRead(t, read)
	if w.Code != http.StatusServiceUnavailable || !strings.Contains(w.Body.String(), "bot_phone_revoke_pending") || !strings.Contains(w.Body.String(), `"retryable":true`) {
		t.Fatalf("in-flight wait turned pending into invalid credentials: %d %s", w.Code, w.Body)
	}
	unblock()
}

func TestBotScopeOperationReadRechecksAfterStorageWait(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "retired"
		if pending {
			name = "pending"
		}
		t.Run(name, func(t *testing.T) {
			h, _, _ := botFixture(t)
			t.Cleanup(h.Close)
			h.durable.volumeProbe = healthyMailboxVolume
			_, token := botScopePhone(t, h, "scope-operation-computer")
			id := "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
			auth := httptest.NewRequest(http.MethodGet, "/", nil)
			auth.Header.Set("X-Salcara-Bot-Token", token)
			owner := shaHex(deviceAccount("scope-operation-computer") + "\x00scope-operation-computer\x00" + h.receiptCredential(deviceAccount("scope-operation-computer"), auth))
			key := shaHex(owner + "\x00" + id)
			ticket := h.durable.signTicket(executionTicket{Version: 2, OperationID: id, Owner: owner, Hash: shaHex("operation"), Deadline: nowMs() + 60000, Epoch: h.durable.epoch})
			if err := h.durable.saveReply(key, owner, replyBody{OK: true, Result: json.RawMessage(`{"secret":"retired-operation-result"}`)}, http.StatusOK, true); err != nil {
				t.Fatal(err)
			}
			path := "/v1/app/operations?deviceId=scope-operation-computer&operationId=" + id + "&ticket=" + url.QueryEscape(ticket)
			if w := botRequest(t, h, http.MethodGet, path, nil, map[string]string{"X-Salcara-Bot-Token": token}); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "retired-operation-result") {
				t.Fatalf("legitimate receipt read: %d %s", w.Code, w.Body)
			}
			// The store has one SQL connection. Holding it produces a real
			// storage wait after operationAccess, without a production hook.
			conn, err := h.durable.db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = conn.Close() })
			waits := h.durable.db.Stats().WaitCount
			r := httptest.NewRequest(http.MethodGet, path, nil)
			r.Header.Set("X-Salcara-Bot-Token", token)
			read := holdHTTPRead(t, h, r)
			deadline := time.Now().Add(3 * time.Second)
			for h.durable.db.Stats().WaitCount <= waits {
				if time.Now().After(deadline) {
					t.Fatal("receipt read did not wait after authorization")
				}
				time.Sleep(time.Millisecond)
			}
			var unblock func()
			status, code := http.StatusForbidden, "bot_credentials_invalid"
			if pending {
				unblock = blockBotRegistrationSave(t, h, botGroupFile)
				status, code = http.StatusServiceUnavailable, "bot_phone_revoke_pending"
			}
			revoked := botRequest(t, h, http.MethodPost, "/v1/bot/phone/revoke", nil, map[string]string{"X-Salcara-Bot-Token": token})
			if pending && revoked.Code != http.StatusServiceUnavailable || !pending && revoked.Code != http.StatusOK {
				t.Fatalf("revoke: %d %s", revoked.Code, revoked.Body)
			}
			_ = conn.Close()
			w := awaitPhoneRead(t, read)
			if w.Code != status || !strings.Contains(w.Body.String(), code) || strings.Contains(w.Body.String(), "retired-operation-result") {
				t.Fatalf("delayed storage read escaped revocation: %d %s", w.Code, w.Body)
			}
			if unblock != nil {
				unblock()
			}
		})
	}
}

func TestBotScopeOperationAckSerializesWithPhoneRevocation(t *testing.T) {
	h, _, _ := botFixture(t)
	t.Cleanup(h.Close)
	h.durable.volumeProbe = healthyMailboxVolume
	_, token := botScopePhone(t, h, "scope-ack-computer")
	id := "bbbbbbbb-bbbb-4bbb-8bbb-bbbbbbbbbbbb"
	auth := httptest.NewRequest(http.MethodGet, "/", nil)
	auth.Header.Set("X-Salcara-Bot-Token", token)
	owner := shaHex(deviceAccount("scope-ack-computer") + "\x00scope-ack-computer\x00" + h.receiptCredential(deviceAccount("scope-ack-computer"), auth))
	key := shaHex(owner + "\x00" + id)
	ticket := h.durable.signTicket(executionTicket{Version: 2, OperationID: id, Owner: owner, Hash: shaHex("ack"), Deadline: nowMs() + 60000, Epoch: h.durable.epoch})
	if err := h.durable.saveReply(key, owner, replyBody{OK: true}, http.StatusOK, true); err != nil {
		t.Fatal(err)
	}
	body := map[string]string{"deviceId": "scope-ack-computer", "operationId": id, "ticket": ticket}
	headers := map[string]string{"X-Salcara-Bot-Token": token}
	if w := botRequest(t, h, http.MethodPost, "/v1/app/operations/ack", body, headers); w.Code != http.StatusOK {
		t.Fatalf("legitimate ack: %d %s", w.Code, w.Body)
	}
	conn, err := h.durable.db.Conn(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	waits := h.durable.db.Stats().WaitCount
	raw, _ := json.Marshal(body)
	r := httptest.NewRequest(http.MethodPost, "/v1/app/operations/ack", strings.NewReader(string(raw)))
	r.Header.Set("X-Salcara-Bot-Token", token)
	ack := holdHTTPRead(t, h, r)
	deadline := time.Now().Add(3 * time.Second)
	for h.durable.db.Stats().WaitCount <= waits {
		if time.Now().After(deadline) {
			t.Fatal("ack did not reach the authorized mutation boundary")
		}
		time.Sleep(time.Millisecond)
	}
	revokeRequest := httptest.NewRequest(http.MethodPost, "/v1/bot/phone/revoke", nil)
	revokeRequest.Header.Set("X-Salcara-Bot-Token", token)
	revoke := holdHTTPRead(t, h, revokeRequest)
	_ = conn.Close()
	if w := awaitPhoneRead(t, ack); w.Code != http.StatusOK {
		t.Fatalf("ack admitted before revoke was not completed: %d %s", w.Code, w.Body)
	}
	if w := awaitPhoneRead(t, revoke); w.Code != http.StatusOK {
		t.Fatalf("revoke failed to proceed after ack: %d %s", w.Code, w.Body)
	}
	if w := botRequest(t, h, http.MethodPost, "/v1/app/operations/ack", body, headers); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "bot_credentials_invalid") {
		t.Fatalf("old phone could ack after completed revocation: %d %s", w.Code, w.Body)
	}
}
