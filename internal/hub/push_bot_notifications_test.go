package hub

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"
)

func botPushHub(t *testing.T) (*Hub, *fakeGoogle) {
	t.Helper()
	creds, pub := serviceAccountFixture(t)
	google := &fakeGoogle{t: t, key: pub}
	h := newDeviceHub(t, Config{DeploymentMode: "bot", DataDir: t.TempDir(), PublicURL: "https://bot.example/salcara-hub", FCMCredentials: creds, HTTPClient: &http.Client{Transport: google}})
	h.push.sender.tokenURL = "https://oauth.test/token"
	h.push.sender.endpoint = "https://fcm.test"
	return h, google
}

func botPushGroup(t *testing.T, h *Hub, ids ...string) (BotBindingPackage, string) {
	t.Helper()
	pack, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range ids {
		if w := botRegisterRequest(t, h, pack, botDevice(id, "Notification test"), strings.Repeat("d", 64)); w.Code != http.StatusOK {
			t.Fatalf("computer bind: %d %s", w.Code, w.Body)
		}
	}
	return pack, bindBotSourcePhone(t, h, pack)
}

func registerBotPush(t *testing.T, h *Hub, id, phone, token, scope string) {
	t.Helper()
	w := botRequest(t, h, http.MethodPost, "/v1/app/push/register", map[string]string{
		"deviceId": id, "token": token, "platform": "android", "notificationScope": scope,
	}, map[string]string{"X-Salcara-Bot-Token": phone})
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"push":"fcm"`) {
		t.Fatalf("Bot push register: %d %s", w.Code, w.Body)
	}
}

func postBotPushTurn(t *testing.T, h *Hub, id, turn string) {
	t.Helper()
	w := botRequest(t, h, http.MethodPost, "/v1/bridge/events", map[string]any{
		"deviceId": id, "eventScope": h.bridgeEventScope(deviceAccount(id), id),
		"events": []map[string]any{{"type": "turn", "sessionKey": "codex:shared", "tool": "codex", "status": "completed", "turnId": turn, "ts": time.Now().UnixMilli()}},
	}, map[string]string{"X-Salcara-Device-Id": id, "X-Salcara-Device-Secret": strings.Repeat("d", 64)})
	if w.Code != http.StatusOK {
		t.Fatalf("Bot event: %d %s", w.Code, w.Body)
	}
}

func TestBotPushTwoComputersAndGroupsStaySeparate(t *testing.T) {
	h, google := botPushHub(t)
	pack, phone := botPushGroup(t, h, "push-bot-one", "push-bot-two")
	_, otherPhone := botPushGroup(t, h, "push-bot-one")
	registerBotPush(t, h, "push-bot-one", phone, fcmToken, notificationScopeA)
	registerBotPush(t, h, "push-bot-two", phone, fcmToken, notificationScopeB)
	otherScope := "ns1_" + strings.Repeat("c", 64)
	registerBotPush(t, h, "push-bot-one", otherPhone, fcmToken+"other", otherScope)
	// Revoking an invitation does not revoke an already bound phone.
	if err := h.RevokeBotGroup(pack.GroupID); err != nil {
		t.Fatal(err)
	}
	postBotPushTurn(t, h, "push-bot-one", "same-turn")
	postBotPushTurn(t, h, "push-bot-two", "same-turn")
	messages := waitMessages(t, google, 3)
	if len(messages) != 3 {
		t.Fatalf("expected exactly three independent targets, got %d", len(messages))
	}
	got := map[string]string{}
	for _, raw := range messages {
		msg := raw["message"].(map[string]any)
		data := msg["data"].(map[string]any)
		got[data["notificationScope"].(string)] = data["deviceId"].(string)
	}
	if len(got) != 3 || got[notificationScopeA] != "push-bot-one" || got[notificationScopeB] != "push-bot-two" || got[otherScope] != "push-bot-one" {
		t.Fatalf("Bot push targets overlapped: %v", got)
	}
	// Unregistering one device must not delete the other device's same FCM token.
	registerBotPush(t, h, "push-bot-one", phone, "", notificationScopeA)
	postBotPushTurn(t, h, "push-bot-two", "next-turn")
	if messages := waitMessages(t, google, 4); len(messages) != 4 {
		t.Fatalf("unregistering one computer affected another: %d", len(messages))
	}
}

func TestBotPushRechecksAuthorizationAfterOAuthWait(t *testing.T) {
	for _, change := range []string{"phone-revoke", "membership-remove", "phone-rebind", "unregister", "pending-registration", "banned", "disabled", "device-secret", "device-remove"} {
		t.Run(change, func(t *testing.T) {
			h, google := botPushHub(t)
			transport := &oauthWaitingPushTransport{base: google, started: make(chan struct{}), release: make(chan struct{})}
			h.push.sender.client = &http.Client{Transport: transport}
			unblock := sync.OnceFunc(func() { close(transport.release) })
			defer unblock()
			pack, phone := botPushGroup(t, h, "push-bot-stale")
			_, sentinelPhone := botPushGroup(t, h, "push-bot-sentinel")
			registerBotPush(t, h, "push-bot-stale", phone, fcmToken, notificationScopeA)
			registerBotPush(t, h, "push-bot-sentinel", sentinelPhone, fcmToken+"sentinel", notificationScopeB)
			postBotPushTurn(t, h, "push-bot-stale", "old-turn")
			awaitSSESignal(t, transport.started, "Bot push did not reach OAuth wait")
			switch change {
			case "phone-revoke", "phone-rebind":
				w := botRequest(t, h, http.MethodPost, "/v1/bot/phone/revoke", nil, map[string]string{"X-Salcara-Bot-Token": phone})
				if w.Code != http.StatusOK {
					t.Fatalf("phone revoke: %d %s", w.Code, w.Body)
				}
				if change == "phone-rebind" {
					oldPhone := phone
					phone = bindBotSourcePhone(t, h, pack)
					registerBotPush(t, h, "push-bot-stale", phone, fcmToken, notificationScopeA)
					late := botRequest(t, h, http.MethodPost, "/v1/app/push/register", map[string]string{"deviceId": "push-bot-stale", "token": ""}, map[string]string{"X-Salcara-Bot-Token": oldPhone})
					if late.Code != http.StatusForbidden {
						t.Fatalf("old phone registration still accepted after rebind: %d", late.Code)
					}
					postBotPushTurn(t, h, "push-bot-stale", "new-turn")
				}
			case "unregister":
				registerBotPush(t, h, "push-bot-stale", phone, "", notificationScopeA)
			default:
				h.mu.Lock()
				dev := h.accounts[deviceAccount("push-bot-stale")].devices["push-bot-stale"]
				switch change {
				case "membership-remove":
					delete(h.botGroups[pack.GroupID].Computers, "push-bot-stale")
				case "pending-registration":
					dev.registrationEpoch++
				case "banned":
					dev.banned = true
				case "disabled":
					h.managedUsers.Devices[dev.info.DeviceID] = managedAssignment{UserID: "disabled-notification-owner"}
				case "device-secret":
					dev.secretHash[0] ^= 1
				case "device-remove":
					delete(h.accounts, deviceAccount(dev.info.DeviceID))
				}
				h.mu.Unlock()
			}
			postBotPushTurn(t, h, "push-bot-sentinel", "sentinel-turn")
			unblock()
			want := 1
			if change == "phone-rebind" {
				want++
			}
			messages := waitMessages(t, google, want)
			if len(messages) != want {
				t.Fatalf("stale Bot push escaped send-time authorization: %d", len(messages))
			}
			for _, raw := range messages {
				data := raw["message"].(map[string]any)["data"].(map[string]any)
				blob, _ := json.Marshal(data)
				if strings.Contains(string(blob), "old-turn") || change != "phone-rebind" && data["deviceId"] != "push-bot-sentinel" {
					t.Fatalf("stale Bot job reached FCM: %v", data)
				}
			}
		})
	}
}

func TestPushBindingRejectsNonCurrentRelayDevice(t *testing.T) {
	h := newDeviceHub(t, Config{DataDir: t.TempDir()})
	const id = "push-relay-binding"
	pairPhone(t, h, id, strings.Repeat("a", 64))
	h.mu.Lock()
	defer h.mu.Unlock()
	acct := deviceAccount(id)
	dev := h.accounts[acct].devices[id]
	stable := h.pushBindingLocked(acct, dev)
	if stable == "" {
		t.Fatal("confirmed relay phone has no binding")
	}
	copy := *dev
	if h.pushBindingLocked(acct, &copy) != "" || h.pushBindingLocked("unrelated-account", dev) != "" {
		t.Fatal("non-current device or account obtained a push binding")
	}
	for _, state := range []string{"unpaired", "pair-pending", "registration-pending", "banned", "retired-pair", "disabled"} {
		saved := *dev
		savedAssignment := h.managedUsers.Devices[id]
		switch state {
		case "unpaired":
			dev.hasPair = false
		case "pair-pending":
			dev.pairPending = true
		case "registration-pending":
			dev.registrationEpoch++
		case "banned":
			dev.banned = true
		case "retired-pair":
			h.managedUsers.Devices[id] = managedAssignment{RevokedPair: stable}
		case "disabled":
			h.managedUsers.Devices[id] = managedAssignment{UserID: "missing-disabled-owner"}
		}
		if h.pushBindingLocked(acct, dev) != "" || len(h.pushAccountsLocked(acct, dev)) != 0 {
			t.Errorf("%s relay retained notification authority", state)
		}
		*dev = saved
		h.managedUsers.Devices[id] = savedAssignment
	}
	if h.pushBindingLocked(acct, dev) != stable {
		t.Fatal("restoring original relay changed its stable binding")
	}
}

func TestBotPushBindingRequiresExactGroupPhoneAndDevice(t *testing.T) {
	h, _, _ := botFixture(t)
	t.Cleanup(h.Close)
	pack, _ := botPushGroup(t, h, "push-bot-binding")
	h.mu.Lock()
	defer h.mu.Unlock()
	acct := "bot:" + pack.GroupID
	dev := h.accounts[deviceAccount("push-bot-binding")].devices["push-bot-binding"]
	group := h.botGroups[pack.GroupID]
	stable := h.pushBindingLocked(acct, dev)
	if stable == "" || dev.hasPair || h.pushDeviceLocked(acct, dev.info.DeviceID) != dev {
		t.Fatal("Bot binding incorrectly depends on QR pairing")
	}
	copy := *dev
	if h.pushBindingLocked(acct, &copy) != "" || h.pushDeviceLocked("bot:unknown-group", dev.info.DeviceID) != nil {
		t.Fatal("wrong Bot group or non-current device obtained notification authority")
	}
	for _, state := range []string{"no-phone-token", "no-phone-id", "no-secret", "wrong-group-id"} {
		savedDevice, savedGroup := *dev, *group
		switch state {
		case "no-phone-token":
			group.PhoneTokenHash = ""
		case "no-phone-id":
			group.PhoneIDHash = ""
		case "no-secret":
			dev.hasSecret = false
		case "wrong-group-id":
			group.ID = "different-group"
		}
		if h.pushBindingLocked(acct, dev) != "" {
			t.Errorf("%s Bot retained notification authority", state)
		}
		*dev, *group = savedDevice, savedGroup
	}
	// A read-only alternate configuration avoids changing a running Hub's cfg.
	nonBot := &Hub{accounts: h.accounts, botGroups: h.botGroups}
	if nonBot.pushBindingLocked(acct, dev) != "" {
		t.Fatal("non-Bot deployment accepted a Bot notification account")
	}
	group.Revoked = true
	if h.pushBindingLocked(acct, dev) != stable {
		t.Fatal("invitation revocation changed a live phone's authority")
	}
}
