package hub

import (
	"crypto/sha256"
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

func botBodyRegister(t *testing.T, h *Hub, binding BotBindingPackage, device Device, secret string) *httptest.ResponseRecorder {
	t.Helper()
	return botRequest(t, h, http.MethodPost, "/v1/bot/computer/bind",
		map[string]any{"binding": binding, "device": device},
		map[string]string{"X-Salcara-Device-Id": device.DeviceID, "X-Salcara-Device-Secret": secret})
}

// Replacing only the destination with a directory makes atomic replacement
// fail on both Windows and Unix. Synchronize with all device/Bot saves, and
// restore the prior valid file before fixture shutdown or a retry.
func blockBotRegistrationSave(t *testing.T, h *Hub, filename string) func() {
	t.Helper()
	path := filepath.Join(h.cfg.DataDir, filename)
	var restore func()
	h.saveMu.Lock()
	func() {
		defer h.saveMu.Unlock()
		original, err := os.ReadFile(path)
		if err != nil && !os.IsNotExist(err) {
			t.Fatal(err)
		}
		if err == nil {
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
		}
		if err := os.Mkdir(path, 0o700); err != nil {
			t.Fatal(err)
		}
		var once sync.Once
		unblock := func() {
			once.Do(func() {
				h.saveMu.Lock()
				defer h.saveMu.Unlock()
				if err := os.Remove(path); err != nil {
					t.Error(err)
					return
				}
				if original != nil {
					if err := os.WriteFile(path, original, 0o600); err != nil {
						t.Error(err)
					}
				}
			})
		}
		t.Cleanup(unblock)
		restore = unblock
	}()
	return restore
}

func botRegistrationEpoch(t *testing.T, h *Hub, id string, pending bool) uint64 {
	t.Helper()
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.accounts[deviceAccount(id)]
	if a == nil || a.devices[id] == nil {
		t.Fatal("computer identity was not retained for safe retry")
	}
	d := a.devices[id]
	if pending != (d.registrationEpoch > d.savedRegistrationEpoch) {
		t.Fatalf("pending=%v epoch=%d saved=%d", pending, d.registrationEpoch, d.savedRegistrationEpoch)
	}
	return d.registrationEpoch
}

func assertBotRegistrationSaved(t *testing.T, h *Hub, binding BotBindingPackage, id, secret string) {
	t.Helper()
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	raw, err := os.ReadFile(filepath.Join(h.cfg.DataDir, devicesFile))
	if err != nil {
		t.Fatal(err)
	}
	var devices persistedState
	if err := json.Unmarshal(raw, &devices); err != nil {
		t.Fatal(err)
	}
	stored := devices.Accounts[deviceAccount(id)]
	if len(stored) != 1 || stored[0].Device.DeviceID != id || stored[0].SecretHash != hashText(secret) || stored[0].RegistrationPending {
		t.Fatal("confirmed computer credential was not committed exactly once")
	}
	raw, err = os.ReadFile(filepath.Join(h.cfg.DataDir, botGroupFile))
	if err != nil {
		t.Fatal(err)
	}
	var groups persistedBotState
	if err := json.Unmarshal(raw, &groups); err != nil {
		t.Fatal(err)
	}
	for _, group := range groups.Groups {
		if group.ID != binding.GroupID {
			continue
		}
		count := 0
		for _, computer := range group.Computers {
			if computer == id {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("confirmed group membership count=%d", count)
		}
		return
	}
	t.Fatal("confirmed Bot group was not committed")
}

func TestBotBodyRegistrationPendingPhoneAccessIsPerComputer(t *testing.T) {
	h, _, _ := botFixture(t)
	t.Cleanup(h.Close)
	one, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	two, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	pending, other := botDevice("phone-pending-pc", "Pending phone target"), botDevice("phone-ready-pc", "Ready phone target")
	secret := strings.Repeat("9", 64)
	for _, device := range []Device{pending, other} {
		if w := botBodyRegister(t, h, one, device, secret); w.Code != http.StatusOK {
			t.Fatalf("initial computer bind: %d %s", w.Code, w.Body)
		}
	}
	token := bindBotSourcePhone(t, h, one)
	bindBotSourcePhone(t, h, two)
	unblock := blockBotRegistrationSave(t, h, botGroupFile)
	if w := botBodyRegister(t, h, two, pending, secret); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("new membership save should fail: %d %s", w.Code, w.Body)
	}
	botRegistrationEpoch(t, h, pending.DeviceID, true)
	botRegistrationEpoch(t, h, other.DeviceID, false)
	headers := map[string]string{"X-Salcara-Bot-Token": token}
	list := botRequest(t, h, http.MethodGet, "/v1/app/devices", nil, headers)
	if list.Code != http.StatusOK || strings.Contains(list.Body.String(), pending.DeviceID) || !strings.Contains(list.Body.String(), other.DeviceID) {
		t.Fatalf("phone list did not isolate pending computer: %d %s", list.Code, list.Body)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.Header.Set("X-Salcara-Bot-Token", token)
	func() {
		h.mu.Lock()
		defer h.mu.Unlock()
		if _, d := h.botAccountAndDeviceLocked("bot:"+one.GroupID, request, pending.DeviceID); d != nil {
			t.Fatal("pending computer retained phone device authority")
		}
		if h.appAuthorizedDeviceLocked("bot:"+one.GroupID, request, pending.DeviceID) || h.appAuthorizedDeviceLocked(deviceAccount(pending.DeviceID), request, pending.DeviceID) {
			t.Fatal("pending computer retained app dispatch authority")
		}
		pendingDevice := h.accounts[deviceAccount(pending.DeviceID)].devices[pending.DeviceID]
		otherDevice := h.accounts[deviceAccount(other.DeviceID)].devices[other.DeviceID]
		if h.botSourceBindingLocked("bot:"+one.GroupID, pendingDevice) != "" {
			t.Fatal("pending computer retained source binding authority")
		}
		if _, d := h.botAccountAndDeviceLocked("bot:"+one.GroupID, request, other.DeviceID); d != otherDevice || !h.appAuthorizedDeviceLocked("bot:"+one.GroupID, request, other.DeviceID) || h.botSourceBindingLocked("bot:"+one.GroupID, otherDevice) == "" {
			t.Fatal("pending computer removed another group member's authority")
		}
	}()
	for i, device := range []Device{pending, other} {
		command := map[string]any{"deviceId": device.DeviceID, "requestId": fmt.Sprintf("aaaaaaaa-aaaa-4aaa-8aaa-%012d", i+1), "command": map[string]any{"type": "session.ping"}}
		w := botRequest(t, h, http.MethodPost, "/v1/app/commands", command, headers)
		if i == 0 && w.Code != http.StatusForbidden || i == 1 && (w.Code != http.StatusConflict || !strings.Contains(w.Body.String(), "computer_offline")) {
			t.Fatalf("phone command target=%s status=%d %s", device.DeviceID, w.Code, w.Body)
		}
	}
	unblock()
	if w := botBodyRegister(t, h, two, pending, secret); w.Code != http.StatusOK {
		t.Fatalf("pending computer did not recover: %d %s", w.Code, w.Body)
	}
	recovered := botRequest(t, h, http.MethodGet, "/v1/app/devices", nil, headers)
	if recovered.Code != http.StatusOK || !strings.Contains(recovered.Body.String(), pending.DeviceID) || !strings.Contains(recovered.Body.String(), other.DeviceID) {
		t.Fatalf("confirmed computer did not return to phone list: %d %s", recovered.Code, recovered.Body)
	}
}

func TestBotBodyRegistrationRetriesUntilBothFilesPersist(t *testing.T) {
	for _, filename := range []string{botGroupFile, devicesFile} {
		t.Run(filename, func(t *testing.T) {
			h, dir, publicURL := botFixture(t)
			t.Cleanup(h.Close)
			binding, err := h.CreateBotBindingPackage()
			if err != nil {
				t.Fatal(err)
			}
			device, secret := botDevice("pending-body-pc", "Pending computer"), strings.Repeat("a", 64)
			unblock := blockBotRegistrationSave(t, h, filename)
			failed := botBodyRegister(t, h, binding, device, secret)
			if failed.Code != http.StatusServiceUnavailable {
				t.Fatalf("first failed save: %d %s", failed.Code, failed.Body)
			}
			epoch := botRegistrationEpoch(t, h, device.DeviceID, true)
			repeated := botBodyRegister(t, h, binding, device, secret)
			if repeated.Code != http.StatusServiceUnavailable {
				t.Fatalf("retained RAM identity falsely acknowledged: %d %s", repeated.Code, repeated.Body)
			}
			if got := botRegistrationEpoch(t, h, device.DeviceID, true); got != epoch {
				t.Fatal("same pending membership retry changed the registration generation")
			}
			if wrong := botBodyRegister(t, h, binding, device, strings.Repeat("b", 64)); wrong.Code != http.StatusForbidden {
				t.Fatalf("pending credential was replaced: %d %s", wrong.Code, wrong.Body)
			}
			if filename == botGroupFile {
				if _, err := os.Stat(filepath.Join(dir, devicesFile)); !os.IsNotExist(err) {
					t.Fatal("first computer was persisted before its Bot membership")
				}
			}
			unblock()
			recovered := botBodyRegister(t, h, binding, device, secret)
			if recovered.Code != http.StatusOK {
				t.Fatalf("same identity retry failed after storage recovery: %d %s", recovered.Code, recovered.Body)
			}
			if got := botRegistrationEpoch(t, h, device.DeviceID, false); got != epoch {
				t.Fatal("storage recovery created another registration generation")
			}
			assertBotRegistrationSaved(t, h, binding, device.DeviceID, secret)
			h.Close()
			next, err := New(Config{DeploymentMode: "bot", DataDir: dir, PublicURL: publicURL})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(next.Close)
			botRegistrationEpoch(t, next, device.DeviceID, false)
			if w := botRequest(t, next, http.MethodPost, "/v1/device/register", device,
				map[string]string{"X-Salcara-Device-Id": device.DeviceID, "X-Salcara-Device-Secret": secret}); w.Code != http.StatusOK {
				t.Fatalf("confirmed computer needed another invite after restart: %d %s", w.Code, w.Body)
			}
		})
	}
}

func TestBotBodyRegistrationBackgroundSaveCannotConfirmPendingIdentity(t *testing.T) {
	h, dir, publicURL := botFixture(t)
	t.Cleanup(h.Close)
	binding, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	device, secret := botDevice("pending-restart-pc", "Pending restart"), strings.Repeat("c", 64)
	unblock := blockBotRegistrationSave(t, h, botGroupFile)
	if w := botBodyRegister(t, h, binding, device, secret); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("failed save: %d %s", w.Code, w.Body)
	}
	if err := h.saveNowError(); err != nil {
		t.Fatal(err)
	}
	botRegistrationEpoch(t, h, device.DeviceID, true)
	headers := map[string]string{"X-Salcara-Device-Id": device.DeviceID, "X-Salcara-Device-Secret": secret}
	if w := botRequest(t, h, http.MethodGet, "/v1/discovery/policy", nil, headers); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("background save made pending identity usable: %d %s", w.Code, w.Body)
	}
	unblock()
	h.Close()
	next, err := New(Config{DeploymentMode: "bot", DataDir: dir, PublicURL: publicURL})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(next.Close)
	botRegistrationEpoch(t, next, device.DeviceID, true)
	if w := botRequest(t, next, http.MethodGet, "/v1/discovery/policy", nil, headers); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("restart silently confirmed pending identity: %d %s", w.Code, w.Body)
	}
	blockAgain := blockBotRegistrationSave(t, next, botGroupFile)
	if w := botBodyRegister(t, next, binding, device, secret); w.Code != http.StatusServiceUnavailable {
		t.Fatalf("restarted pending bind skipped persistence: %d %s", w.Code, w.Body)
	}
	blockAgain()
	if w := botBodyRegister(t, next, binding, device, secret); w.Code != http.StatusOK {
		t.Fatalf("restarted pending bind did not recover: %d %s", w.Code, w.Body)
	}
	botRegistrationEpoch(t, next, device.DeviceID, false)
	assertBotRegistrationSaved(t, next, binding, device.DeviceID, secret)
}

func botRegistrationStream() *bridgeConn {
	return &bridgeConn{cmds: make(chan []byte, 1), closed: make(chan struct{}), pairChanged: make(chan struct{}, 1)}
}

func assertBotRegistrationStream(t *testing.T, stream *bridgeConn, closed bool) {
	t.Helper()
	select {
	case <-stream.closed:
		if !closed {
			t.Fatal("unchanged computer stream was interrupted")
		}
	default:
		if closed {
			t.Fatal("changed computer stream retained its old scope")
		}
	}
}

func TestBotBodyRegistrationNotifiesOnlyChangedMembership(t *testing.T) {
	h, _, _ := botFixture(t)
	t.Cleanup(h.Close)
	one, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	two, err := h.CreateBotBindingPackage()
	if err != nil {
		t.Fatal(err)
	}
	first, second := botDevice("scope-first-pc", "First"), botDevice("scope-second-pc", "Second")
	secret := strings.Repeat("d", 64)
	if w := botBodyRegister(t, h, one, first, secret); w.Code != http.StatusOK {
		t.Fatalf("first bind: %d %s", w.Code, w.Body)
	}
	bindBotSourcePhone(t, h, one)
	bindBotSourcePhone(t, h, two)
	firstStream, secondStream := botRegistrationStream(), botRegistrationStream()
	h.mu.Lock()
	h.accounts[deviceAccount(first.DeviceID)].devices[first.DeviceID].conn = firstStream
	h.mu.Unlock()
	if w := botBodyRegister(t, h, one, second, secret); w.Code != http.StatusOK {
		t.Fatalf("second computer bind: %d %s", w.Code, w.Body)
	}
	assertBotRegistrationStream(t, firstStream, false)
	h.mu.Lock()
	h.accounts[deviceAccount(second.DeviceID)].devices[second.DeviceID].conn = secondStream
	h.mu.Unlock()
	epoch := botRegistrationEpoch(t, h, first.DeviceID, false)
	oldScope := h.bridgeEventScope(deviceAccount(first.DeviceID), first.DeviceID)
	if w := botBodyRegister(t, h, two, first, secret); w.Code != http.StatusOK {
		t.Fatalf("new membership bind: %d %s", w.Code, w.Body)
	}
	if got := botRegistrationEpoch(t, h, first.DeviceID, false); got != epoch+1 {
		t.Fatalf("new membership generation=%d want=%d", got, epoch+1)
	}
	assertBotRegistrationStream(t, firstStream, true)
	assertBotRegistrationStream(t, secondStream, false)
	newScope := h.bridgeEventScope(deviceAccount(first.DeviceID), first.DeviceID)
	if oldScope == newScope {
		t.Fatal("adding another phone group did not change this computer's event scope")
	}
	reconnected := botRegistrationStream()
	h.mu.Lock()
	h.accounts[deviceAccount(first.DeviceID)].devices[first.DeviceID].conn = reconnected
	h.mu.Unlock()
	if w := botBodyRegister(t, h, two, first, secret); w.Code != http.StatusOK {
		t.Fatalf("same group reconnect: %d %s", w.Code, w.Body)
	}
	if got := botRegistrationEpoch(t, h, first.DeviceID, false); got != epoch+1 || h.bridgeEventScope(deviceAccount(first.DeviceID), first.DeviceID) != newScope {
		t.Fatal("same group reconnect changed identity or scope generation")
	}
	assertBotRegistrationStream(t, reconnected, false)
	assertBotRegistrationStream(t, secondStream, false)
}

func TestBotBodyRegistrationSerializesFirstSecretAndGroupLimit(t *testing.T) {
	t.Run("first secret", func(t *testing.T) {
		h, _, _ := botFixture(t)
		t.Cleanup(h.Close)
		binding, err := h.CreateBotBindingPackage()
		if err != nil {
			t.Fatal(err)
		}
		device := botDevice("first-secret-pc", "Concurrent computer")
		secrets := []string{strings.Repeat("e", 64), strings.Repeat("f", 64)}
		start, results := make(chan struct{}), make(chan int, 2)
		for _, secret := range secrets {
			go func(secret string) {
				<-start
				results <- botBodyRegister(t, h, binding, device, secret).Code
			}(secret)
		}
		close(start)
		counts := map[int]int{}
		for range secrets {
			counts[<-results]++
		}
		if counts[http.StatusOK] != 1 || counts[http.StatusForbidden] != 1 {
			t.Fatalf("first-secret race results=%v", counts)
		}
		h.mu.Lock()
		hash := h.accounts[deviceAccount(device.DeviceID)].devices[device.DeviceID].secretHash
		h.mu.Unlock()
		winner := secrets[0]
		if hash != sha256.Sum256([]byte(winner)) {
			winner = secrets[1]
		}
		assertBotRegistrationSaved(t, h, binding, device.DeviceID, winner)
	})
	t.Run("group limit", func(t *testing.T) {
		h, _, _ := botFixture(t)
		t.Cleanup(h.Close)
		binding, err := h.CreateBotBindingPackage()
		if err != nil {
			t.Fatal(err)
		}
		start, results := make(chan struct{}), make(chan int, maxBotComputers+1)
		for i := 0; i <= maxBotComputers; i++ {
			go func(i int) {
				<-start
				device := botDevice(fmt.Sprintf("body-limit-%d", i), "Limit computer")
				results <- botBodyRegister(t, h, binding, device, fmt.Sprintf("%064x", i+1)).Code
			}(i)
		}
		close(start)
		counts := map[int]int{}
		for i := 0; i <= maxBotComputers; i++ {
			counts[<-results]++
		}
		if counts[http.StatusOK] != maxBotComputers || counts[http.StatusConflict] != 1 {
			t.Fatalf("concurrent group admission results=%v", counts)
		}
	})
}
