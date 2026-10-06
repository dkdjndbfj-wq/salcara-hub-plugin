package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"
	"testing"
)

func modernPair(t *testing.T, h *Hub, id, secret, binding, allowed, phone string) (string, string) {
	t.Helper()
	start := deviceRequest(t, h, "/bridge/pair/start", id, secret, "", map[string]string{"deviceId": id, "bindingId": binding, "phoneHash": allowed, "computerId": "physical-fixture"}, "")
	if start.Code != 200 {
		t.Fatal(start.Code, start.Body)
	}
	var ticket struct {
		Ticket string `json:"ticket"`
	}
	_ = json.Unmarshal(start.Body.Bytes(), &ticket)
	claimed := deviceRequest(t, h, "/app/pair/qr", "", "", "", map[string]string{"deviceId": id, "ticket": ticket.Ticket, "phoneId": phone}, "")
	if claimed.Code != 200 {
		t.Fatal(claimed.Code, claimed.Body)
	}
	var result struct {
		Token      string `json:"pair_token"`
		ComputerID string `json:"computerId"`
	}
	_ = json.Unmarshal(claimed.Body.Bytes(), &result)
	if result.ComputerID != "physical-fixture" {
		t.Fatal("missing attested physical computer ID")
	}
	return result.Token, ticket.Ticket
}
func TestModernQRRestrictsOtherPhonesAndSurvivesPersistence(t *testing.T) {
	dir := t.TempDir()
	h := newDeviceHub(t, Config{DataDir: dir})
	id, secret, binding, phone := "pc-modern", strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	enrollDevice(t, h, id, secret)
	dev := onlineDevice(t, h, id)
	dev.conn.pairChanged = make(chan struct{}, 1)
	hash := sha256.Sum256([]byte(phone))
	allowed := hex.EncodeToString(hash[:])
	attemptID := strings.Repeat("f", 64)
	start := deviceRequest(t, h, "/bridge/pair/start", id, secret, "", map[string]string{"deviceId": id, "bindingId": binding, "phoneHash": allowed, "computerId": "physical-fixture", "attemptId": attemptID}, "")
	var info struct {
		Ticket string `json:"ticket"`
		Code   string `json:"code"`
	}
	_ = json.Unmarshal(start.Body.Bytes(), &info)
	missingPhone := deviceRequest(t, h, "/app/pair/qr", "", "", "", map[string]string{"deviceId": id, "ticket": info.Ticket}, "")
	if missingPhone.Code != 403 || dev.hasPair {
		t.Fatal("modern QR accepted without a phone identity")
	}
	bad := deviceRequest(t, h, "/app/pair/qr", "", "", "", map[string]string{"deviceId": id, "ticket": info.Ticket, "phoneId": strings.Repeat("d", 64)}, "")
	if bad.Code != 403 || dev.hasPair {
		t.Fatal("another phone accepted")
	}
	// Failed claim must not burn a legitimate phone's QR.
	good := deviceRequest(t, h, "/app/pair/qr", "", "", "", map[string]string{"deviceId": id, "ticket": info.Ticket, "phoneId": phone}, "")
	if good.Code != 200 || dev.bindingID != binding || dev.phoneHash != allowed {
		t.Fatal(good.Code, good.Body)
	}
	if len(dev.conn.pairChanged) == 0 {
		t.Fatal("no desktop pairing feedback")
	}
	status := deviceRequest(t, h, "/bridge/pair/status", id, secret, "", nil, "")
	if status.Code != 200 || !strings.Contains(status.Body.String(), `"paired":true`) {
		t.Fatal(status.Code, status.Body)
	}
	for _, raw := range []string{secret, phone, info.Ticket, `"pair_token"`} {
		if strings.Contains(status.Body.String(), raw) {
			t.Fatal("pair status leaked a raw credential")
		}
	}
	h.saveNow()
	reopened := newDeviceHub(t, Config{DataDir: dir})
	restored := reopened.accounts[deviceAccount(id)].devices[id]
	if restored.phoneHash != allowed || restored.bindingID != binding || !restored.hasPair || restored.pairAttemptID != attemptID {
		t.Fatal("phone identity not persisted")
	}
	revoke := deviceRequest(t, h, "/bridge/pair/revoke", id, secret, "", map[string]string{"deviceId": id, "bindingId": strings.Repeat("e", 64), "phoneHash": allowed}, "")
	if revoke.Code != 409 || !dev.hasPair {
		t.Fatal("stale conditional revoke deleted current pair")
	}
	revoke = deviceRequest(t, h, "/bridge/pair/revoke", id, secret, "", map[string]string{"deviceId": id}, "")
	if revoke.Code != 200 || dev.hasPair || dev.phoneHash != allowed || dev.bindingID != binding {
		t.Fatal("revocation cannot identify the global binding")
	}
}

func TestPairStartAllowsPostRevokeRebindButNotAnActiveEmptyHash(t *testing.T) {
	h := newDeviceHub(t, Config{DataDir: t.TempDir()})
	id, secret := "pc-rebind", strings.Repeat("a", 64)
	enrollDevice(t, h, id, secret)
	onlineDevice(t, h, id)
	firstBinding := strings.Repeat("b", 64)
	phone := strings.Repeat("c", 64)
	phoneHashBytes := sha256.Sum256([]byte(phone))
	phoneHash := hex.EncodeToString(phoneHashBytes[:])

	start := deviceRequest(t, h, "/bridge/pair/start", id, secret, "", map[string]string{
		"deviceId": id, "bindingId": firstBinding, "computerId": "physical-rebind",
	}, "")
	if start.Code != http.StatusOK {
		t.Fatalf("initial rebind window: %d %s", start.Code, start.Body)
	}
	var info struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(start.Body.Bytes(), &info); err != nil || info.Ticket == "" {
		t.Fatalf("initial ticket: %v %s", err, start.Body)
	}
	claimed := deviceRequest(t, h, "/app/pair/qr", "", "", "", map[string]string{
		"deviceId": id, "ticket": info.Ticket, "phoneId": phone,
	}, "")
	if claimed.Code != http.StatusOK {
		t.Fatalf("initial claim: %d %s", claimed.Code, claimed.Body)
	}

	// An active pair cannot be replaced by a desktop that presents a fresh
	// binding generation without a phone hash. It must revoke first.
	activeStart := deviceRequest(t, h, "/bridge/pair/start", id, secret, "", map[string]string{
		"deviceId": id, "bindingId": strings.Repeat("d", 64), "computerId": "physical-rebind",
	}, "")
	if activeStart.Code != http.StatusConflict {
		t.Fatalf("active empty-hash start accepted: %d %s", activeStart.Code, activeStart.Body)
	}

	revoke := deviceRequest(t, h, "/bridge/pair/revoke", id, secret, "", map[string]string{
		"deviceId": id, "bindingId": firstBinding, "phoneHash": phoneHash,
	}, "")
	if revoke.Code != http.StatusOK {
		t.Fatalf("revoke: %d %s", revoke.Code, revoke.Body)
	}

	secondBinding := strings.Repeat("e", 64)
	rebindStart := deviceRequest(t, h, "/bridge/pair/start", id, secret, "", map[string]string{
		"deviceId": id, "bindingId": secondBinding, "computerId": "physical-rebind",
	}, "")
	if rebindStart.Code != http.StatusOK {
		t.Fatalf("post-revoke start: %d %s", rebindStart.Code, rebindStart.Body)
	}
	var rebind struct {
		Ticket string `json:"ticket"`
	}
	if err := json.Unmarshal(rebindStart.Body.Bytes(), &rebind); err != nil || rebind.Ticket == "" {
		t.Fatalf("post-revoke ticket: %v %s", err, rebindStart.Body)
	}
	claimed = deviceRequest(t, h, "/app/pair/qr", "", "", "", map[string]string{
		"deviceId": id, "ticket": rebind.Ticket, "phoneId": strings.Repeat("f", 64),
	}, "")
	if claimed.Code != http.StatusOK {
		t.Fatalf("post-revoke claim: %d %s", claimed.Code, claimed.Body)
	}
}

func TestCommandMetadataIsStampedByHubNotByPhone(t *testing.T) {
	h := newDeviceHub(t, Config{})
	id, secret := "pc-command", strings.Repeat("a", 64)
	enrollDevice(t, h, id, secret)
	dev := onlineDevice(t, h, id)
	modernPair(t, h, id, secret, strings.Repeat("b", 64), "", strings.Repeat("c", 64))
	done := make(chan int, 1)
	go func() {
		_, status := h.executeCommand(context.Background(), deviceAccount(id), id, json.RawMessage(`{"type":"projects.list","phoneHash":"attacker"}`), func(d *device) bool { return d == dev }, "")
		done <- status
	}()
	var envelope commandEnvelope
	_ = json.Unmarshal(<-dev.conn.cmds, &envelope)
	if !envelope.Phone || envelope.BindingID != dev.bindingID || envelope.PhoneHash != dev.phoneHash {
		t.Fatal("phone spoofed binding metadata")
	}
	rep := deviceRequest(t, h, "/bridge/reply", id, secret, "", replyBody{DeviceID: id, CommandID: envelope.CommandID, OK: true, Result: json.RawMessage(`{}`)}, "")
	if rep.Code != 200 || <-done != 200 {
		t.Fatal("command reply failed")
	}
}
