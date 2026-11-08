package hostfleet

import (
	"bytes"
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
)

func TestExistingKeyWithoutRegistryCanSave(t *testing.T) {
	f := newSSHFixture(t, false)
	dir := t.TempDir()
	key := bytes.Repeat([]byte{0x73}, 32)
	if err := os.WriteFile(filepath.Join(dir, "servers.key"), key, 0600); err != nil {
		t.Fatal(err)
	}
	m, err := New(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	m.dial = f.dial
	t.Cleanup(func() { f.Close(); m.Close() })
	check, err := m.Check(context.Background(), f.input())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := m.Save(check.CheckID, 1); err != nil {
		t.Fatalf("orphaned valid key prevented save: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "servers.key"))
	if err != nil || !bytes.Equal(got, key) {
		t.Fatal("existing encryption key was replaced")
	}
	m.Close()
	restarted, err := New(dir, "")
	if err != nil {
		t.Fatalf("cannot decrypt registry with retained key: %v", err)
	}
	defer restarted.Close()
	if len(restarted.State().Hosts) != 1 {
		t.Fatal("saved host missing")
	}
}

func TestRegistryTamperingFailsClosed(t *testing.T) {
	m, dir := newTestManager(t, nil)
	m.mu.Lock()
	err := m.persistLocked(m.saved)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	path := filepath.Join(dir, "servers.enc")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	raw[len(raw)-1] ^= 1
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	if reopened, err := New(dir, ""); err == nil {
		reopened.Close()
		t.Fatal("modified authenticated ciphertext was accepted")
	}
}

func TestRestartPersistsInterruptedJobsAndSnapshotsAreIndependent(t *testing.T) {
	m, dir := newTestManager(t, nil)
	m.mu.Lock()
	next := m.saved
	next.Jobs = []Job{{ID: "fixture-job", HostID: "fixture-host", Status: "running", Steps: []Step{{At: 10, Message: "original step"}}, CreatedAt: 10, UpdatedAt: 10}}
	err := m.persistLocked(next)
	m.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	m.Close()
	restarted, err := New(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	state := restarted.State()
	if len(state.Jobs) != 1 || state.Jobs[0].Status != "interrupted" || state.Jobs[0].Error == "" {
		t.Fatalf("running job not recovered safely: %+v", state.Jobs)
	}
	state.Jobs[0].Status = "changed externally"
	state.Jobs[0].Steps[0].Message = "changed externally"
	if got := restarted.State(); got.Jobs[0].Status != "interrupted" || got.Jobs[0].Steps[0].Message != "original step" {
		t.Fatal("public state aliases mutable registry")
	}
	restarted.Close()
	again, err := New(dir, "")
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	if got := again.State(); got.Jobs[0].Status != "interrupted" || got.Jobs[0].Steps[0].Message != "original step" {
		t.Fatal("job recovery was not persisted")
	}
}

func TestControllerSwitchRejectsStaleMutationBeforeDial(t *testing.T) {
	m, _ := newTestManager(t, nil)
	first := record{Host: Host{ID: "first", Deployed: true, Origin: "https://origin.example"}, Token: strings.Repeat("01", 32)}
	second := record{Host: Host{ID: "second", Deployed: true, Origin: "https://origin.example"}, Token: strings.Repeat("02", 32)}
	m.saved.Hosts = []record{first, second}
	m.saved.ActiveID = first.ID
	oldController := m.ControllerID()
	if oldController == "" {
		t.Fatal("missing controller identity")
	}
	m.saved.ActiveID = second.ID
	if m.ControllerID() == oldController {
		t.Fatal("different hosts share controller identity")
	}
	var dials atomic.Int64
	m.dial = func(context.Context, string, string) (net.Conn, error) {
		dials.Add(1)
		return nil, errors.New("must not dial")
	}
	if _, _, err := m.CallControlFor(context.Background(), oldController, "POST", "nodes", []byte(`{"revision":1}`)); !errors.Is(err, ErrControllerChanged) {
		t.Fatalf("expected stale controller rejection, got %v", err)
	}
	if dials.Load() != 0 {
		t.Fatal("stale operation reached another server")
	}
	if id := controllerID(first); id == controllerID(record{Host: first.Host, Token: second.Token}) {
		t.Fatal("controller token rotation preserves stale identity")
	}
	changedOrigin := first
	changedOrigin.Origin = "https://other-origin.example"
	if controllerID(first) == controllerID(changedOrigin) {
		t.Fatal("controller origin change preserves stale identity")
	}
}
