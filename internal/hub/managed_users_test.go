package hub

import (
	"crypto/sha256"
	"encoding/json"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestManagedUserSignedExceptionRetiresOldGrants(t *testing.T) {
	h, r, req, snapshot, key := sourceContractFixture(t)
	policy := &StationPolicy{Version: 2, ExternalAPIBases: []string{"https://external.example/team"}}
	managedChange(t, h, ManagedUserChange{Action: "save", Name: "授权测试", Station: policy})
	u := h.managedUsers.Users[0]
	managedChange(t, h, ManagedUserChange{Action: "assign", ID: u.ID, DeviceRef: adminRef("receipt-owner", "pc")})
	const token = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	h.mu.Lock()
	d := h.accounts["receipt-owner"].devices["pc"]
	d.info.Capabilities = append(d.info.Capabilities, "station.guard.v2")
	h.setPairLocked("receipt-owner", d, sha256.Sum256([]byte(token)), true)
	h.mu.Unlock()
	r.Header.Set("X-Salcara-Pair-Token", token)
	snapshot.Origin = "https://external.example"
	snapshot.APIBase = "https://external.example/team"
	snapshot.PolicyRevision = h.deviceRemoteAccessPolicy("pc").Revision
	signSourceFixture(t, &req, snapshot, key)
	if code := h.evaluateSourcePolicy("receipt-owner", r, req); code != "" {
		t.Fatalf("signed exception denied: %s", code)
	}
	if h.CurrentRemoteAccessPolicy().Station.allowsSnapshot(snapshot) {
		t.Fatal("exception leaked into global source list")
	}
	managedChange(t, h, ManagedUserChange{Action: "save", ID: u.ID, Name: u.Name, Note: "权限更新", Station: policy})
	if code := h.evaluateSourcePolicy("receipt-owner", r, req); code != "" {
		t.Fatalf("metadata-only save retired an unchanged grant: %s", code)
	}
	policy.ExternalAPIBases = append(policy.ExternalAPIBases, "https://another.example/team")
	managedChange(t, h, ManagedUserChange{Action: "save", ID: u.ID, Name: u.Name, Station: policy})
	if code := h.evaluateSourcePolicy("receipt-owner", r, req); code != "source_policy_stale" {
		t.Fatalf("old grant not retired: %s", code)
	}
}

func managedChange(t *testing.T, h *Hub, in ManagedUserChange) {
	t.Helper()
	in.ExpectedRevision = h.managedUsers.Revision
	in.Confirm = true
	in.Reason = "用户管理回归验证"
	if err := h.ApplyManagedUserChange(in); err != nil {
		t.Fatal(err)
	}
}

func TestManagedUserEnrollmentDisablePolicyAndRestart(t *testing.T) {
	dir := t.TempDir()
	h := newDeviceHub(t, Config{DataDir: dir, PublicURL: "https://hub.example/salcara-hub"})
	managedChange(t, h, ManagedUserChange{Action: "save", Name: "测试用户", Station: &StationPolicy{Version: 2, ExternalAPIBases: []string{"https://external.example/team"}}})
	u := h.managedUsers.Users[0]
	claim, _, err := h.CreateDeviceEnrollmentClaimForUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	id, secret := "user-owned-computer", strings.Repeat("a", 64)
	w := deviceRequestWithEnrollmentClaim(t, h, "/device/register", id, secret, "", Device{DeviceID: id, Capabilities: []string{"station.guard.v2", "source.guard.v1"}}, "", claim, false)
	if w.Code != 200 {
		t.Fatalf("enrollment: %d %s", w.Code, w.Body.String())
	}
	dev := onlineDevice(t, h, id)
	token := claimQR(t, h, id, issueQR(t, h, id, secret))
	if h.managedUsers.Devices[id].UserID != u.ID {
		t.Fatal("invited user was not assigned")
	}
	if w := deviceRequest(t, h, "/app/devices", "", "", token, nil, ""); w.Code != 200 {
		t.Fatal("active phone denied")
	}
	p := h.deviceRemoteAccessPolicy(id)
	if p.Station.Version != 2 || len(p.Station.ExternalAPIBases) != 1 || p.Revision == h.CurrentRemoteAccessPolicy().Revision {
		t.Fatal("user source exception missing")
	}
	managedChange(t, h, ManagedUserChange{Action: "save", ID: u.ID, Name: u.Name, Note: "仅更新备注", Station: u.Station})
	if dev.conn == nil || h.deviceRemoteAccessPolicy(id).Revision != p.Revision {
		t.Fatal("metadata-only save interrupted a connection or rotated authorization")
	}
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Salcara-Pair-Token", token)
	req := commandReq{DeviceID: id, Command: json.RawMessage(`{"type":"session.start","tool":"codex"}`)}
	code, source := h.executionSourceFor(deviceAccount(id), r, req)
	if code != "" || source == nil || source.PolicyRevision != p.Revision || len(source.Station.ExternalAPIBases) != 1 {
		t.Fatalf("wrong effective dispatch policy: %s %+v", code, source)
	}
	claim2, _, err := h.CreateDeviceEnrollmentClaimForUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	stale := ManagedUserChange{Action: "save", ID: u.ID, Name: u.Name, ExpectedRevision: h.managedUsers.Revision, Confirm: true, Reason: "旧页面"}
	phone := concurrentPhone{id: id, token: token, ip: "192.0.2.91"}
	read := holdPhoneRead(t, h, phone, emptyWaitPath(phone, 25))
	awaitReadState(t, h, func() bool { return h.eventWaiters == 1 })
	managedChange(t, h, ManagedUserChange{Action: "save", ID: u.ID, Name: u.Name, Station: u.Station, Disabled: true})
	if w := awaitPhoneRead(t, read); w.Code != 423 {
		t.Fatalf("disabled user's outstanding read survived: %d", w.Code)
	}
	if err := h.ApplyManagedUserChange(stale); err != ErrManagedUserConflict {
		t.Fatal("stale editor overwrote disable")
	}
	if dev.conn != nil {
		t.Fatal("disabled device stayed connected")
	}
	if page := h.AdminSnapshotPage(AdminQuery{UserID: u.ID, Filter: "paired"}).(map[string]any); page["total"].(int) != 1 {
		t.Fatal("disabled user lost the retained pairing in the device view")
	}
	if page := h.AdminSnapshotPage(AdminQuery{UserID: u.ID, Filter: "unpaired"}).(map[string]any); page["total"].(int) != 0 {
		t.Fatal("paired device also appeared as unpaired because its user is disabled")
	}
	if w := deviceRequest(t, h, "/app/devices", "", "", token, nil, ""); w.Code != 423 || !strings.Contains(w.Body.String(), `"code":"user_disabled"`) {
		t.Fatalf("disabled phone must be denied without retiring its pairing: %d %s", w.Code, w.Body.String())
	}
	if w := deviceRequest(t, h, "/device/register", id, secret, "", Device{DeviceID: id}, ""); w.Code != 403 {
		t.Fatal("disabled device reconnected")
	}
	hash, _ := enrollmentClaimHash(claim2)
	if h.enrollmentClaimValidLocked(hash) {
		t.Fatal("disabled invitation usable")
	}
	h.Close()
	h = newDeviceHub(t, Config{DataDir: dir, PublicURL: "https://hub.example/salcara-hub"})
	if !h.userDisabledLocked(id) {
		t.Fatal("disabled state lost on restart")
	}
	managedChange(t, h, ManagedUserChange{Action: "save", ID: u.ID, Name: u.Name, Station: u.Station})
	if w := deviceRequest(t, h, "/app/devices", "", "", token, nil, ""); w.Code != 200 {
		t.Fatalf("enable lost retained pairing: %d", w.Code)
	}
	if h.deviceRemoteAccessPolicy(id).Revision == p.Revision {
		t.Fatal("reenable resurrected policy epoch")
	}
	global := h.CurrentRemoteAccessPolicy()
	global.Mode = "disabled"
	if err := h.ApplyRemoteAccessPolicy(global, nil); err != nil {
		t.Fatal(err)
	}
	if code := h.evaluateSourcePolicy(deviceAccount(id), r, req); code != "remote_execution_disabled" {
		t.Fatalf("exception bypassed global pause: %s", code)
	}
}

func TestManagedUserReassignmentCannotRestoreOldPhoneAfterCrash(t *testing.T) {
	dir := t.TempDir()
	h := newDeviceHub(t, Config{DataDir: dir})
	id, secret := "assigned-computer", strings.Repeat("b", 64)
	enrollDevice(t, h, id, secret)
	onlineDevice(t, h, id)
	token := claimQR(t, h, id, issueQR(t, h, id, secret))
	if err := h.saveNowError(); err != nil {
		t.Fatal(err)
	}
	oldDevices, err := os.ReadFile(filepath.Join(dir, devicesFile))
	if err != nil {
		t.Fatal(err)
	}
	managedChange(t, h, ManagedUserChange{Action: "save", Name: "新使用者"})
	u := h.managedUsers.Users[0]
	managedChange(t, h, ManagedUserChange{Action: "assign", ID: u.ID, DeviceRef: adminRef(deviceAccount(id), id)})
	h.Close()
	// Simulate a crash after the membership commit but before devices.json
	// recorded pair revocation. The membership tombstone must still reject it.
	if err := os.WriteFile(filepath.Join(dir, devicesFile), oldDevices, 0600); err != nil {
		t.Fatal(err)
	}
	h = newDeviceHub(t, Config{DataDir: dir})
	if err := h.saveNowError(); err != nil {
		t.Fatal(err)
	}
	if w := deviceRequest(t, h, "/app/devices", "", "", token, nil, ""); w.Code != 403 {
		t.Fatalf("old phone survived reassignment crash: %d %s", w.Code, w.Body.String())
	}
	page := h.AdminSnapshotPage(AdminQuery{UserID: u.ID}).(map[string]any)
	if page["total"].(int) != 1 || page["devices"].([]AdminDevice)[0].Paired {
		t.Fatal("reassigned device reported old pairing")
	}
}

func TestManagedUserPersistenceFailureAndInvitationRevoke(t *testing.T) {
	dir := t.TempDir()
	h := newDeviceHub(t, Config{DataDir: dir})
	managedChange(t, h, ManagedUserChange{Action: "save", Name: "保留原权限"})
	u := h.managedUsers.Users[0]
	claim, _, err := h.CreateDeviceEnrollmentClaimForUser(u.ID)
	if err != nil {
		t.Fatal(err)
	}
	hash, _ := enrollmentClaimHash(claim)
	rows := h.AdminEnrollmentClaims().(map[string]any)["claims"].([]map[string]any)
	if strings.Contains(rows[0]["ref"].(string), claim) {
		t.Fatal("claim secret exposed in list")
	}
	if err := h.RevokeEnrollmentClaim(rows[0]["ref"].(string)); err != nil {
		t.Fatal(err)
	}
	if h.enrollmentClaimValidLocked(hash) {
		t.Fatal("revoked invitation usable")
	}
	path := filepath.Join(dir, managedUsersFile)
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(path, 0700); err != nil {
		t.Fatal(err)
	}
	err = h.ApplyManagedUserChange(ManagedUserChange{Action: "save", ID: u.ID, Name: u.Name, Disabled: true, ExpectedRevision: h.managedUsers.Revision, Confirm: true, Reason: "验证保存失败"})
	if err == nil || h.managedUsers.Users[0].Disabled {
		t.Fatal("failed save changed live permission")
	}
}

func TestManagedUserBotDeviceCannotBeIndividuallyReassigned(t *testing.T) {
	h, _, _ := botFixture(t)
	defer h.Close()
	packageValue, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	id := "bot-user-device"
	w := botRequest(t, h, "POST", "/v1/bot/computer/bind", map[string]any{"binding": packageValue, "device": Device{DeviceID: id}}, map[string]string{"X-Salcara-Device-Id": id, "X-Salcara-Device-Secret": strings.Repeat("a", 64)})
	if w.Code != 200 {
		t.Fatalf("bind: %d %s", w.Code, w.Body.String())
	}
	managedChange(t, h, ManagedUserChange{Action: "save", Name: "不可拆组"})
	before := h.managedUsers.Revision
	err = h.ApplyManagedUserChange(ManagedUserChange{Action: "assign", ID: h.managedUsers.Users[0].ID, DeviceRef: adminRef(deviceAccount(id), id), ExpectedRevision: before, Confirm: true, Reason: "验证组授权边界"})
	if err == nil || !strings.Contains(err.Error(), "整组凭据") || before != h.managedUsers.Revision || h.managedUsers.Devices[id].UserID != "" {
		t.Fatal("group credential cannot be revoked by a per-device reassignment")
	}
}
