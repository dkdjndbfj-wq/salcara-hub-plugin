package hub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestBotCanonicalMigrationPreservesEnrolledIdentity(t *testing.T) {
	for _, publicURL := range []string{"https://moved.example/salcara-hub", "https://bot.example:9443/salcara-hub"} {
		t.Run(publicURL, func(t *testing.T) {
			h, dir, _ := botFixture(t)
			pack, err := h.CreateBotBindingPackage()
			if err != nil {
				t.Fatal(err)
			}
			device := botDevice("migration-pc", "Migration PC")
			secret := strings.Repeat("a", 64)
			if w := botRegisterRequest(t, h, pack, device, secret); w.Code != http.StatusOK {
				t.Fatalf("initial registration: %d %s", w.Code, w.Body)
			}
			token := bindBotSourcePhone(t, h, pack)
			before := h.IngressState()
			scope := h.bridgeEventScope(deviceAccount(device.DeviceID), device.DeviceID)
			h.Close()

			next, err := New(Config{DeploymentMode: "bot", DataDir: dir, PublicURL: publicURL})
			if err != nil {
				t.Fatal(err)
			}
			defer next.Close()
			after := next.IngressState()
			if after.HubInstanceID != before.HubInstanceID || after.IdentityPublicKey != before.IdentityPublicKey || !after.Configured {
				t.Fatal("canonical migration replaced the persisted Hub identity")
			}
			if after.CanonicalURL != publicURL+"/v1" || after.Enabled || after.Revision <= before.Revision {
				t.Fatal("canonical migration did not invalidate the prior ingress configuration")
			}
			manifest, _, _ := ingressSignedManifest(t, next, httptest.NewRequest(http.MethodGet, "/v1/ingress/manifest", nil), before.IdentityPublicKey)
			if manifest.HubInstanceID != before.HubInstanceID || manifest.CanonicalURL != publicURL+"/v1" || len(manifest.Endpoints) != 1 || manifest.Endpoints[0].URL != publicURL+"/v1" {
				t.Fatal("migrated endpoint was not published under the original signed identity")
			}
			if scope == "" || next.bridgeEventScope(deviceAccount(device.DeviceID), device.DeviceID) != scope {
				t.Fatal("address-only migration rotated the existing event generation")
			}
			headers := map[string]string{"X-Salcara-Device-Id": device.DeviceID, "X-Salcara-Device-Secret": secret}
			if w := botRequest(t, next, http.MethodPost, "/v1/device/register", device, headers); w.Code != http.StatusOK {
				t.Fatalf("enrolled computer could not reconnect: %d %s", w.Code, w.Body)
			}
			if w := botRequest(t, next, http.MethodGet, "/v1/app/devices", nil, map[string]string{"X-Salcara-Bot-Token": token}); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), device.DeviceID) {
				t.Fatalf("enrolled phone could not recover its computer: %d %s", w.Code, w.Body)
			}
			if w := botRegisterRequest(t, next, pack, botDevice("migration-new-pc", "New PC"), strings.Repeat("b", 64)); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "bot_credentials_invalid") {
				t.Fatalf("old URL-bound invite authorized a new computer: %d %s", w.Code, w.Body)
			}
			if w := botRequest(t, next, http.MethodPost, "/v1/bot/phone/bind", map[string]any{"binding": pack, "phoneId": strings.Repeat("c", 64)}, nil); w.Code != http.StatusBadRequest {
				t.Fatalf("old URL-bound package accepted at migrated canonical: %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestBotDataLossReplacesIdentityAndRejectsRetiredCredentials(t *testing.T) {
	h, _, publicURL := botFixture(t)
	pack, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	device := botDevice("retired-pc", "Retired PC")
	secret := strings.Repeat("d", 64)
	if w := botRegisterRequest(t, h, pack, device, secret); w.Code != http.StatusOK {
		t.Fatalf("initial registration: %d %s", w.Code, w.Body)
	}
	token := bindBotSourcePhone(t, h, pack)
	before := h.IngressState()
	h.Close()

	next, err := New(Config{DeploymentMode: "bot", DataDir: t.TempDir(), PublicURL: publicURL})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	after := next.IngressState()
	if !after.Configured || before.HubInstanceID == after.HubInstanceID || before.IdentityPublicKey == after.IdentityPublicKey {
		t.Fatal("new empty data directory reused the retired Hub identity")
	}
	headers := map[string]string{"X-Salcara-Device-Id": device.DeviceID, "X-Salcara-Device-Secret": secret}
	if w := botRequest(t, next, http.MethodPost, "/v1/device/register", device, headers); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "bot_credentials_invalid") {
		t.Fatalf("retired computer silently re-enrolled: %d %s", w.Code, w.Body)
	}
	if w := botRequest(t, next, http.MethodGet, "/v1/app/devices", nil, map[string]string{"X-Salcara-Bot-Token": token}); w.Code != http.StatusForbidden || !strings.Contains(w.Body.String(), "bot_credentials_invalid") {
		t.Fatalf("retired phone accepted by replacement instance: %d %s", w.Code, w.Body)
	}
	if w := botRegisterRequest(t, next, pack, botDevice("replacement-new-pc", "New PC"), strings.Repeat("e", 64)); w.Code != http.StatusForbidden {
		t.Fatalf("retired invite accepted by replacement instance: %d %s", w.Code, w.Body)
	}
}

func TestBotGroupComputersKeepMetadataCommandsAndEventsIsolated(t *testing.T) {
	h, _, _ := botFixture(t)
	defer h.Close()
	h.durable.volumeProbe = healthyMailboxVolume
	pack, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	other, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	devices := []Device{botDevice("group-pc-a", "PC A"), botDevice("group-pc-b", "PC B")}
	devices[0].Tools = json.RawMessage(`[{"id":"codex","accountId":"api-a","apiBase":"https://api-a.example/v1"}]`)
	devices[1].Tools = json.RawMessage(`[{"id":"claude","accountId":"api-b","apiBase":"https://api-b.example/v1"}]`)
	devices[0].Projects, devices[1].Projects = json.RawMessage(`[{"id":"project-a"}]`), json.RawMessage(`[{"id":"project-b"}]`)
	secrets := []string{strings.Repeat("a", 64), strings.Repeat("b", 64)}
	for i, device := range devices {
		if w := botRegisterRequest(t, h, pack, device, secrets[i]); w.Code != http.StatusOK {
			t.Fatalf("register %s: %d %s", device.DeviceID, w.Code, w.Body)
		}
	}
	outsider := botDevice("outside-pc", "Outside PC")
	if w := botRegisterRequest(t, h, other, outsider, strings.Repeat("c", 64)); w.Code != http.StatusOK {
		t.Fatalf("register outsider: %d %s", w.Code, w.Body)
	}
	token, otherToken := bindBotSourcePhone(t, h, pack), bindBotSourcePhone(t, h, other)
	phone := map[string]string{"X-Salcara-Bot-Token": token}
	foreignPhone := map[string]string{"X-Salcara-Bot-Token": otherToken}
	w := botRequest(t, h, http.MethodGet, "/v1/app/devices", nil, phone)
	var listed struct {
		Devices []DeviceStatus `json:"devices"`
	}
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &listed) != nil || len(listed.Devices) != 2 {
		t.Fatalf("group list: %d %s", w.Code, w.Body)
	}
	byID := map[string]DeviceStatus{}
	for _, device := range listed.Devices {
		byID[device.DeviceID] = device
	}
	for _, expected := range devices {
		actual, ok := byID[expected.DeviceID]
		if !ok || !bytes.Equal(actual.Tools, expected.Tools) || !bytes.Equal(actual.Projects, expected.Projects) {
			t.Fatalf("computer metadata was merged or replaced: %s", expected.DeviceID)
		}
	}
	if _, ok := byID[outsider.DeviceID]; ok {
		t.Fatal("group list included another group's computer")
	}
	conns := []*bridgeConn{{cmds: make(chan []byte, 2), closed: make(chan struct{})}, {cmds: make(chan []byte, 2), closed: make(chan struct{})}}
	h.mu.Lock()
	for i, device := range devices {
		h.accounts[deviceAccount(device.DeviceID)].devices[device.DeviceID].conn = conns[i]
	}
	h.mu.Unlock()
	for i, device := range devices {
		command := map[string]any{"type": "session.ping", "agent": []string{"codex", "claude"}[i], "accountId": []string{"api-a", "api-b"}[i]}
		body := map[string]any{"deviceId": device.DeviceID, "command": command}
		if denied := botRequest(t, h, http.MethodPost, "/v1/app/commands", body, foreignPhone); denied.Code != http.StatusForbidden {
			t.Fatalf("foreign group dispatched to %s: %d %s", device.DeviceID, denied.Code, denied.Body)
		}
		completed := make(chan *httptest.ResponseRecorder, 1)
		go func() { completed <- botRequest(t, h, http.MethodPost, "/v1/app/commands", body, phone) }()
		var raw []byte
		select {
		case raw = <-conns[i].cmds:
		case <-time.After(3 * time.Second):
			t.Fatalf("no command for %s", device.DeviceID)
		}
		var envelope commandEnvelope
		var forwarded map[string]any
		if json.Unmarshal(raw, &envelope) != nil || json.Unmarshal(envelope.Command, &forwarded) != nil || envelope.DeviceID != device.DeviceID || forwarded["agent"] != command["agent"] || forwarded["accountId"] != command["accountId"] || len(conns[1-i].cmds) != 0 {
			t.Fatalf("command crossed computer/Agent/API target: %s", raw)
		}
		h.mu.Lock()
		pending := h.pending[envelope.CommandID]
		h.mu.Unlock()
		if pending == nil {
			t.Fatal("dispatched command has no pending reply")
		}
		pending.ch <- replyBody{OK: true, Result: json.RawMessage(`{"target":"` + device.DeviceID + `"}`)}
		select {
		case reply := <-completed:
			if reply.Code != http.StatusOK || !strings.Contains(reply.Body.String(), device.DeviceID) {
				t.Fatalf("wrong computer reply: %d %s", reply.Code, reply.Body)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("command reply did not complete")
		}
	}
	scopes := []string{h.bridgeEventScope(deviceAccount(devices[0].DeviceID), devices[0].DeviceID), h.bridgeEventScope(deviceAccount(devices[1].DeviceID), devices[1].DeviceID)}
	if scopes[0] == "" || scopes[0] == scopes[1] {
		t.Fatal("different computers shared an event generation")
	}
	for i, device := range devices {
		headers := map[string]string{"X-Salcara-Device-Id": device.DeviceID, "X-Salcara-Device-Secret": secrets[i]}
		body := map[string]any{"deviceId": device.DeviceID, "eventScope": scopes[1-i], "events": []any{map[string]any{"type": "message", "sessionKey": "same-session", "deviceId": devices[1-i].DeviceID, "text": device.DeviceID}}}
		if rejected := botRequest(t, h, http.MethodPost, "/v1/bridge/events", body, headers); rejected.Code != http.StatusConflict || !strings.Contains(rejected.Body.String(), "event_scope_changed") {
			t.Fatalf("another computer's event scope accepted: %d %s", rejected.Code, rejected.Body)
		}
		body["eventScope"] = scopes[i]
		if uploaded := botRequest(t, h, http.MethodPost, "/v1/bridge/events", body, headers); uploaded.Code != http.StatusOK {
			t.Fatalf("upload %s: %d %s", device.DeviceID, uploaded.Code, uploaded.Body)
		}
		path := "/v1/app/events?deviceId=" + device.DeviceID + "&sessionKey=same-session"
		if denied := botRequest(t, h, http.MethodGet, path, nil, foreignPhone); denied.Code != http.StatusForbidden {
			t.Fatalf("foreign group read events: %d %s", denied.Code, denied.Body)
		}
	}
	for _, device := range devices {
		read := botRequest(t, h, http.MethodGet, "/v1/app/events?deviceId="+device.DeviceID+"&sessionKey=same-session", nil, phone)
		var page eventPage
		if read.Code != http.StatusOK || json.Unmarshal(read.Body.Bytes(), &page) != nil || len(page.Events) != 1 {
			t.Fatalf("read %s: %d %s", device.DeviceID, read.Code, read.Body)
		}
		var event struct{ DeviceID, Text string }
		if json.Unmarshal(page.Events[0], &event) != nil || event.DeviceID != device.DeviceID || event.Text != device.DeviceID {
			t.Fatalf("event crossed computer target or trusted inner deviceId: %s", page.Events[0])
		}
	}
}
