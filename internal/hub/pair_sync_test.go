package hub

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestPairSyncCannotCompleteAfterConcurrentRevoke(t *testing.T) {
	h := newDeviceHub(t, Config{DataDir: t.TempDir()})
	id, secret, token := "sync-race-pc", strings.Repeat("a", 64), strings.Repeat("b", 64)
	enrollDevice(t, h, id, secret)
	hash := sha256.Sum256([]byte(token))
	body := map[string]string{"deviceId": id, "computerId": "physical-pc", "bindingId": strings.Repeat("c", 64), "phoneHash": strings.Repeat("d", 64), "pairHash": hex.EncodeToString(hash[:])}
	h.saveMu.Lock()
	unblock := sync.OnceFunc(h.saveMu.Unlock)
	defer unblock()
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() { done <- deviceRequest(t, h, "/bridge/pair/sync", id, secret, "", body, "") }()
	waitFor := func(check func(*device) bool) {
		t.Helper()
		for end := time.Now().Add(2 * time.Second); time.Now().Before(end); {
			h.mu.Lock()
			ready := check(h.accounts[deviceAccount(id)].devices[id])
			h.mu.Unlock()
			if ready {
				return
			}
			time.Sleep(time.Millisecond)
		}
		t.Fatal("pair transition did not arrive")
	}
	waitFor(func(dev *device) bool { return dev.pairPending })
	revoked := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		revoked <- deviceRequest(t, h, "/bridge/pair/revoke", id, secret, "", map[string]string{"deviceId": id, "bindingId": body["bindingId"], "phoneHash": body["phoneHash"]}, "")
	}()
	waitFor(func(dev *device) bool { return !dev.hasPair })
	unblock()
	if response := awaitPairClaim(t, done); response.Code != 409 {
		t.Fatal("sync acknowledged a concurrently revoked binding")
	}
	if response := awaitPairClaim(t, revoked); response.Code != 200 {
		t.Fatal("revoke failed")
	}
	if deviceRequest(t, h, "/app/devices", "", "", token, nil, "").Code != 403 {
		t.Fatal("revoked token accepted")
	}
	if deviceRequest(t, h, "/bridge/pair/sync", id, secret, "", body, "").Code != 409 {
		t.Fatal("late sync resurrected binding")
	}
}

func TestQRAndConnectionSyncKeepTheSameGenerationToken(t *testing.T) {
	h := newDeviceHub(t, Config{DataDir: t.TempDir()})
	id, secret, seed, binding, phoneID := "seeded-qr-pc", strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64)
	enrollDevice(t, h, id, secret)
	onlineDevice(t, h, id)
	start := deviceRequest(t, h, "/bridge/pair/start", id, secret, "", map[string]string{"deviceId": id, "computerId": "physical-pc", "bindingId": binding, "pairSeed": seed}, "")
	if start.Code != 200 {
		t.Fatal("QR start failed")
	}
	var attempt struct {
		Ticket string `json:"ticket"`
	}
	_ = json.Unmarshal(start.Body.Bytes(), &attempt)
	claim := deviceRequest(t, h, "/app/pair/qr", "", "", "", map[string]string{"deviceId": id, "ticket": attempt.Ticket, "phoneId": phoneID}, "")
	if claim.Code != 200 {
		t.Fatal("QR claim failed")
	}
	var pair struct {
		Token string `json:"pair_token"`
	}
	_ = json.Unmarshal(claim.Body.Bytes(), &pair)
	phone := sha256.Sum256([]byte(phoneID))
	phoneHash := hex.EncodeToString(phone[:])
	mac := hmac.New(sha256.New, []byte(seed))
	_, _ = mac.Write([]byte(phoneHash))
	expected := hex.EncodeToString(mac.Sum(nil))
	if pair.Token != expected {
		t.Fatal("QR did not use the generation-scoped phone token")
	}
	tokenHash := sha256.Sum256([]byte(pair.Token))
	body := map[string]string{"deviceId": id, "computerId": "physical-pc", "bindingId": binding, "phoneHash": phoneHash, "pairHash": hex.EncodeToString(tokenHash[:])}
	if result := deviceRequest(t, h, "/bridge/pair/sync", id, secret, "", body, ""); result.Code != 200 || !strings.Contains(result.Body.String(), `"retained":false`) {
		t.Fatal("same token sync failed")
	}
	// Also preserve a legacy/random token or an earlier derivation: a sync
	// must not silently replace a working credential merely because it differs.
	body["pairHash"] = strings.Repeat("e", 64)
	if result := deviceRequest(t, h, "/bridge/pair/sync", id, secret, "", body, ""); result.Code != 200 || !strings.Contains(result.Body.String(), `"retained":true`) {
		t.Fatal("existing token not retained")
	}
	if deviceRequest(t, h, "/app/devices", "", "", pair.Token, nil, "").Code != 200 {
		t.Fatal("sync invalidated the original QR token")
	}
}

func TestConnectionSyncCannotPersistCredentialReceipt(t *testing.T) {
	h := newDeviceHub(t, Config{DataDir: t.TempDir()})
	for _, receiptOnly := range []bool{false, true} {
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "/app/commands", nil)
		handled := h.handleReceiptCommand(w, r, "fixture", commandReq{DeviceID: "pc", RequestID: receiptRequestID, ReceiptOnly: receiptOnly, Command: json.RawMessage(`{"type":"remote.connections.sync"}`)}, nil)
		if !handled || w.Code != 400 || !strings.Contains(w.Body.String(), "connection_receipt_unsupported") {
			t.Fatal("credential receipt accepted")
		}
	}
}

func TestPairSyncPersistsWithoutOnlineQRAndIsIdempotent(t *testing.T) {
	dir := t.TempDir()
	h := newDeviceHub(t, Config{DataDir: dir})
	id, secret, token := "offline-saved-pc", strings.Repeat("a", 64), strings.Repeat("b", 64)
	enrollDevice(t, h, id, secret)
	hash := sha256.Sum256([]byte(token))
	body := map[string]string{"deviceId": id, "computerId": "physical-pc", "bindingId": strings.Repeat("c", 64), "phoneHash": strings.Repeat("d", 64), "pairHash": hex.EncodeToString(hash[:])}
	for attempt := 0; attempt < 2; attempt++ {
		r := deviceRequest(t, h, "/bridge/pair/sync", id, secret, "", body, "")
		if r.Code != 200 {
			t.Fatalf("sync attempt %d status %d", attempt, r.Code)
		}
		var result map[string]any
		_ = json.Unmarshal(r.Body.Bytes(), &result)
		if result["computerId"] != "physical-pc" || result["bindingId"] != body["bindingId"] {
			t.Fatal("identity not confirmed")
		}
		if strings.Contains(r.Body.String(), token) || strings.Contains(r.Body.String(), secret) {
			t.Fatal("raw credential leaked")
		}
		if deviceRequest(t, h, "/app/devices", "", "", token, nil, "").Code != 200 {
			t.Fatal("issued phone token unusable")
		}
	}
	h = newDeviceHub(t, Config{DataDir: dir})
	if deviceRequest(t, h, "/app/devices", "", "", token, nil, "").Code != 200 {
		t.Fatal("binding lost on restart")
	}
	if deviceRequest(t, h, "/bridge/pair/sync", id, secret, "", body, "").Code != 200 {
		t.Fatal("retry after restart failed")
	}
}

func TestPairSyncRejectsWrongDevicePhoneAndRevokedGeneration(t *testing.T) {
	h := newDeviceHub(t, Config{})
	id, secret, token := "sync-pc", strings.Repeat("a", 64), strings.Repeat("b", 64)
	enrollDevice(t, h, id, secret)
	hash := sha256.Sum256([]byte(token))
	body := map[string]string{"deviceId": id, "computerId": "physical-pc", "bindingId": strings.Repeat("c", 64), "phoneHash": strings.Repeat("d", 64), "pairHash": hex.EncodeToString(hash[:])}
	if deviceRequest(t, h, "/bridge/pair/sync", id, strings.Repeat("f", 64), "", body, "").Code != 403 {
		t.Fatal("wrong device credential accepted")
	}
	if deviceRequest(t, h, "/bridge/pair/sync", id, secret, "", body, "").Code != 200 {
		t.Fatal("first sync failed")
	}
	if deviceRequest(t, h, "/bridge/pair/sync", "", "", token, body, "").Code != 403 {
		t.Fatal("phone can mint bindings")
	}
	body["phoneHash"] = strings.Repeat("e", 64)
	if deviceRequest(t, h, "/bridge/pair/sync", id, secret, "", body, "").Code != 409 {
		t.Fatal("other phone replaced binding")
	}
	body["phoneHash"] = strings.Repeat("d", 64)
	if deviceRequest(t, h, "/app/pair/revoke", "", "", token, map[string]any{}, "").Code != 200 {
		t.Fatal("revoke failed")
	}
	if deviceRequest(t, h, "/bridge/pair/sync", id, secret, "", body, "").Code != 409 {
		t.Fatal("revoked generation resurrected")
	}
	body["bindingId"] = strings.Repeat("f", 64)
	if deviceRequest(t, h, "/bridge/pair/sync", id, secret, "", body, "").Code != 200 {
		t.Fatal("fresh locally authorized generation rejected")
	}
}
