package hub

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestBotCleanupReviewStationDoesNotOverwriteBotState(t *testing.T) {
	dir := t.TempDir()
	h := newDeviceHub(t, Config{DeploymentMode: "station", DataDir: dir})
	enrollDevice(t, h, "station-cleanup-review", strings.Repeat("c", 64))
	ageDevice(t, h, "station-cleanup-review", 30)
	path := filepath.Join(dir, botGroupFile)
	prior := []byte(`{"sentinel":"previous-bot-state-must-not-be-touched-by-station-cleanup"}`)
	if err := os.WriteFile(path, prior, 0o600); err != nil {
		t.Fatal(err)
	}
	if removed, err := h.ApplyDeviceCleanup(7, 0, nil); err != nil || removed != 1 {
		t.Fatalf("station cleanup failed: removed=%d err=%v", removed, err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(prior, after) {
		t.Fatalf("station cleanup overwrote unrelated Bot state: %s", after)
	}
}

func TestBotCleanupReviewStartupRepairWriteFailureStopsServing(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("Windows open-file replacement denial")
	}
	h, _, url := botFixture(t)
	t.Cleanup(h.Close)
	p := cleanupBotGroup(t, h)
	d := botDevice("startup-orphan-write-failure", "Orphan")
	requireBotCode(t, botBodyRegister(t, h, p, d, strings.Repeat("d", 64)), 200, "")
	dir := cleanupBotCrashImage(t, h)
	path := filepath.Join(dir, devicesFile)
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var devices persistedState
	if err := json.Unmarshal(raw, &devices); err != nil {
		t.Fatal(err)
	}
	delete(devices.Accounts, deviceAccount(d.DeviceID))
	raw, err = json.Marshal(devices)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	// The file remains readable and valid for loadBotState, while Windows
	// denies the later atomic replacement until this reader is closed.
	f, err := os.Open(filepath.Join(dir, botGroupFile))
	if err != nil {
		t.Fatal(err)
	}
	next, startErr := New(Config{DeploymentMode: "bot", DataDir: dir, PublicURL: url})
	if err := f.Close(); err != nil {
		t.Fatal(err)
	}
	if next != nil {
		next.Close()
	}
	if startErr == nil {
		t.Fatal("startup served before orphan repair was durable")
	}
	if !strings.Contains(startErr.Error(), botGroupFile) {
		t.Fatalf("unexpected startup failure: %v", startErr)
	}
	// Recovery after replacement becomes possible must repair before serving.
	recovered := cleanupBotRestart(t, dir, url)
	if cleanupBotMember(t, recovered, p.GroupID, d.DeviceID) {
		t.Fatal("recovered startup kept orphan authority")
	}
}
