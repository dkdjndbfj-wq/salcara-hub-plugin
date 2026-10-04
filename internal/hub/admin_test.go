package hub

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestAdminMetadataMetricsAndPaginationNeverExposeConversationOrCredentials(t *testing.T) {
	h := newDeviceHub(t, Config{})
	secret := strings.Repeat("a", 64)
	enrollDevice(t, h, "metrics-pc", secret)
	dev := onlineDevice(t, h, "metrics-pc")
	claimQR(t, h, "metrics-pc", issueQR(t, h, "metrics-pc", secret))
	h.mu.Lock()
	dev.info.Tools = json.RawMessage(`[{"id":"codex","name":"Codex","available":true,"key":"sk-never-admin","extra":"PRIVATE-PAYLOAD"}]`)
	a := h.accounts[deviceAccount("metrics-pc")]
	for i, status := range []string{"running", "waiting_approval", "idle"} {
		fields := map[string]json.RawMessage{"type": json.RawMessage(`"session.updated"`), "sessionKey": json.RawMessage(fmt.Sprintf(`"session-%d"`, i)), "session": json.RawMessage(fmt.Sprintf(`{"status":%q,"title":"PRIVATE-CONVERSATION","cwd":"PRIVATE-PROJECT-PATH"}`, status))}
		a.appendEventLocked("metrics-pc", fields)
	}
	for i := 0; i < 2; i++ {
		sub := &appSub{ch: make(chan sseMsg, 1), closed: make(chan struct{}), deviceID: "metrics-pc"}
		a.apps[sub] = struct{}{}
	}
	h.mu.Unlock()
	stats := h.Stats()
	if stats.PairedDevices != 1 || stats.AppStreams != 2 || stats.RunningSessions != 1 || stats.WaitingApprovals != 1 || stats.LatencyMeanMS != nil {
		t.Fatalf("incorrect metrics %+v", stats)
	}
	body, _ := json.Marshal(h.AdminSnapshot())
	for _, private := range []string{secret, "sk-never-admin", "PRIVATE-PAYLOAD", "PRIVATE-CONVERSATION", "PRIVATE-PROJECT-PATH", "secretHash", "pairHash"} {
		if bytes.Contains(body, []byte(private)) {
			t.Fatalf("admin snapshot leaked %s", private)
		}
	}
	// Metadata-only search and a hard cap prevent rendering 10k rows at once.
	h.mu.Lock()
	for i := 0; i < 220; i++ {
		id := fmt.Sprintf("paged-%03d", i)
		h.registerDeviceLocked(deviceAccount(id), Device{DeviceID: id, Name: "Paged fixture"}, sha256.Sum256([]byte(secret)))
	}
	h.mu.Unlock()
	page := h.AdminSnapshotPage(AdminQuery{Query: "paged", Limit: 99999, Offset: 10}).(map[string]any)
	if page["total"].(int) != 220 || len(page["devices"].([]AdminDevice)) != 100 || page["offset"].(int) != 10 {
		t.Fatal("pagination cap/filter failed")
	}
	filtered := h.AdminSnapshotPage(AdminQuery{Filter: "paired"}).(map[string]any)
	if filtered["total"].(int) != 1 {
		t.Fatal("paired filter not scoped")
	}
}

func TestAdminBanPersistentlyBlocksSameIDAndInvalidatesPhonesAndTickets(t *testing.T) {
	dir := t.TempDir()
	h := newDeviceHub(t, Config{DataDir: dir})
	id, secret := "ban-pc", strings.Repeat("b", 64)
	enrollDevice(t, h, id, secret)
	dev := onlineDevice(t, h, id)
	token := claimQR(t, h, id, issueQR(t, h, id, secret))
	ticket := issueQR(t, h, id, secret)
	ref := adminRef(deviceAccount(id), id)
	if _, err := h.AdminActionAs(context.Background(), "ban", ref, "测试封禁", "host-admin:42"); err != nil {
		t.Fatal(err)
	}
	if dev.conn != nil || !dev.banned || dev.hasPair {
		t.Fatal("ban did not disconnect/revoke")
	}
	if deviceRequest(t, h, "/app/devices", "", "", token, nil, "").Code != 403 {
		t.Fatal("banned phone token valid")
	}
	if deviceRequest(t, h, "/app/pair/qr", "", "", "", map[string]string{"deviceId": id, "ticket": ticket}, "").Code != 403 {
		t.Fatal("banned QR valid")
	}
	for _, changedSecret := range []string{secret, strings.Repeat("c", 64)} {
		if deviceRequest(t, h, "/device/register", id, changedSecret, "", Device{DeviceID: id}, "").Code != 403 {
			t.Fatal("banned ID bypassed by changed secret")
		}
	}
	h.mu.Lock()
	_, status, _ := h.registerDeviceLocked("another-legacy-account", Device{DeviceID: id}, sha256.Sum256([]byte(secret)))
	h.mu.Unlock()
	if status != 403 {
		t.Fatal("same device ID bypassed ban via namespace")
	}
	h.Close()
	restarted := newDeviceHub(t, Config{DataDir: dir})
	if deviceRequest(t, restarted, "/device/register", id, secret, "", Device{DeviceID: id}, "").Code != 403 {
		t.Fatal("ban lost on restart")
	}
	audit := restarted.AdminSnapshot().(map[string]any)["audit"].([]AdminAudit)
	if len(audit) != 1 || audit[0].Actor != "host-admin:42" || audit[0].Reason != "测试封禁" {
		t.Fatal("admin audit not persisted accurately")
	}
	if _, err := restarted.AdminAction(context.Background(), "unban", ref, "测试解封"); err != nil {
		t.Fatal(err)
	}
	enrollDevice(t, restarted, id, secret)
	if restarted.accounts[deviceAccount(id)].devices[id].hasPair {
		t.Fatal("unban resurrected revoked phone")
	}
	// A fresh random ID is a new enrollment by design, not hardware identity.
	enrollDevice(t, restarted, "new-device-id", secret)
}

func TestAdminReasonsAndPublicPathsFailClosed(t *testing.T) {
	h := newDeviceHub(t, Config{})
	enrollDevice(t, h, "reason-pc", strings.Repeat("d", 64))
	ref := adminRef(deviceAccount("reason-pc"), "reason-pc")
	for _, reason := range []string{"", "\n", strings.Repeat("x", 257), "sk-accidentally-pasted-secret", strings.Repeat("e", 64)} {
		if _, err := h.AdminAction(context.Background(), "ban", ref, reason); err == nil {
			t.Fatal("invalid/credential-like reason accepted")
		}
	}
	for _, action := range []string{"delete", "shell", "api.switch"} {
		if _, err := h.AdminAction(context.Background(), action, ref, "reason"); err == nil {
			t.Fatal("unknown admin operation accepted")
		}
	}
	if h.accounts[deviceAccount("reason-pc")].devices["reason-pc"].banned {
		t.Fatal("invalid action mutated device")
	}
	w := requestHub(t, h, http.MethodPost, "/../_admin/action", "", "", "", map[string]string{"action": "ban", "ref": ref, "reason": "reason"})
	if w.Code == 200 {
		t.Fatal("public Hub routed administrator action")
	}
}

func TestAdminPingUsesActualRequestReplyRTTAndNonce(t *testing.T) {
	h := newDeviceHub(t, Config{CommandTimeout: time.Second})
	id, secret := "ping-pc", strings.Repeat("f", 64)
	enrollDevice(t, h, id, secret)
	dev := onlineDevice(t, h, id)
	respond := func(wrong bool) {
		go func() {
			raw := <-dev.conn.cmds
			var env struct {
				CommandID string `json:"commandId"`
				Command   struct {
					Nonce string `json:"nonce"`
				} `json:"command"`
			}
			json.Unmarshal(raw, &env)
			time.Sleep(15 * time.Millisecond)
			nonce := env.Command.Nonce
			if wrong {
				nonce = "not-the-request"
			}
			deviceRequest(t, h, "/bridge/reply", id, secret, "", map[string]any{"deviceId": id, "commandId": env.CommandID, "ok": true, "result": map[string]any{"nonce": nonce, "receivedAt": 1}}, "")
		}()
	}
	respond(false)
	ref := adminRef(deviceAccount(id), id)
	if _, err := h.AdminAction(context.Background(), "ping", ref, ""); err != nil {
		t.Fatal(err)
	}
	if dev.latency.Samples != 1 || dev.latency.LastMS < 10 || dev.latency.LastMS > 2000 || dev.latency.Kind != "device.ping" {
		t.Fatalf("not true measured RTT %+v", dev.latency)
	}
	respond(true)
	if _, err := h.AdminAction(context.Background(), "ping", ref, ""); err == nil {
		t.Fatal("mismatched ping nonce reported successful")
	}
	if h.Stats().CommandFailures != 1 {
		t.Fatal("failed reply not reflected in counters")
	}
}

func TestAdminTemporaryDisconnectIsNotBanAndDoesNotStopLocalTasks(t *testing.T) {
	h := newDeviceHub(t, Config{})
	id, secret := "disconnect-pc", strings.Repeat("a", 64)
	enrollDevice(t, h, id, secret)
	dev := onlineDevice(t, h, id)
	if _, err := h.AdminAction(context.Background(), "disconnect", adminRef(deviceAccount(id), id), "临时断开测试"); err != nil {
		t.Fatal(err)
	}
	if dev.conn != nil || dev.banned {
		t.Fatal("temporary disconnect became a ban")
	}
	enrollDevice(t, h, id, secret)
	if h.Stats().RunningSessions != 0 {
		t.Fatal("offline statuses counted as live tasks")
	}
}

func TestQueuedCommandReplyCannotOutliveBanRevokeOrConnection(t *testing.T) {
	for _, action := range []string{"ban", "revoke", "disconnect"} {
		t.Run(action, func(t *testing.T) {
			h := newDeviceHub(t, Config{CommandTimeout: time.Second})
			id := "queued-reply-pc"
			enrollDevice(t, h, id, strings.Repeat("a", 64))
			dev := onlineDevice(t, h, id)
			conn := dev.conn
			paired := true
			type commandResult struct {
				reply  replyBody
				status int
			}
			result := make(chan commandResult, 1)
			go func() {
				reply, status := h.executeCommand(context.Background(), deviceAccount(id), id, json.RawMessage(`{"type":"projects.list"}`), func(*device) bool { return paired }, "")
				result <- commandResult{reply, status}
			}()
			var env commandEnvelope
			select {
			case raw := <-conn.cmds:
				if err := json.Unmarshal(raw, &env); err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("command not queued")
			}
			h.mu.Lock()
			p := h.pending[env.CommandID]
			delete(h.pending, env.CommandID) // Bridge already accepted the reply.
			p.ch <- replyBody{OK: true, Result: json.RawMessage(`{"private":"must-not-be-released"}`)}
			switch action {
			case "ban":
				dev.banned = true
			case "revoke":
				paired = false
			case "disconnect":
				dev.conn = nil
			}
			h.mu.Unlock()
			select {
			case got := <-result:
				if (got.status != 403 && got.status != 409) || got.reply.OK || len(got.reply.Result) != 0 {
					t.Fatalf("late authorized reply leaked: status=%d", got.status)
				}
			case <-time.After(time.Second):
				t.Fatal("command did not complete")
			}
			if h.Stats().CommandSuccess != 0 {
				t.Fatal("revoked reply counted as success")
			}
		})
	}
}

type gatedAdminBody struct {
	io.Reader
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *gatedAdminBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started); <-b.release })
	return b.Reader.Read(p)
}
func (*gatedAdminBody) Close() error { return nil }

func TestCloseDrainsAcceptedAdminMutationAndRejectsStaleAdminHub(t *testing.T) {
	dir := t.TempDir()
	h := newDeviceHub(t, Config{DataDir: dir})
	id := "admin-drain-pc"
	secret := strings.Repeat("a", 64)
	enrollDevice(t, h, id, secret)
	body, _ := json.Marshal(map[string]string{"action": "ban", "ref": adminRef(deviceAccount(id), id), "reason": "Lifecycle fixture"})
	gate := &gatedAdminBody{Reader: bytes.NewReader(body), started: make(chan struct{}), release: make(chan struct{})}
	r := httptest.NewRequest(http.MethodPost, "/salcara-hub/_admin/action", gate)
	w := httptest.NewRecorder()
	requestDone := make(chan struct{})
	go func() { h.ServeAdminHTTP(w, r); close(requestDone) }()
	<-gate.started // The admin handler is already registered in the lifecycle.
	closeDone := make(chan struct{})
	go func() { h.Close(); close(closeDone) }()
	select {
	case <-h.done:
	case <-time.After(time.Second):
		t.Fatal("Close did not start")
	}
	select {
	case <-closeDone:
		t.Fatal("Close ignored accepted admin mutation")
	default:
	}
	close(gate.release)
	<-requestDone
	<-closeDone
	if w.Code != 200 {
		t.Fatalf("accepted admin request failed: %d", w.Code)
	}
	stale := httptest.NewRecorder()
	h.ServeAdminHTTP(stale, httptest.NewRequest(http.MethodPost, "/salcara-hub/_admin/action", bytes.NewReader(body)))
	if stale.Code != 503 {
		t.Fatal("closed Hub accepted private administrator mutation")
	}
	replacement := newDeviceHub(t, Config{DataDir: dir})
	if deviceRequest(t, replacement, "/device/register", id, secret, "", Device{DeviceID: id}, "").Code != 403 {
		t.Fatal("replacement loaded before old administrator ban flushed")
	}
}
