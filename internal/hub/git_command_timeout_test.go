package hub

import (
	"context"
	"encoding/json"
	"testing"
	"time"
)

func TestGitCommandWaitTimeoutDefaultsAndExplicitOverrides(t *testing.T) {
	for _, timeout := range []time.Duration{0, 10 * time.Millisecond, 45 * time.Second, 2 * time.Minute} {
		cfg := Config{CommandTimeout: timeout}
		cfg.setDefaults()
		h := &Hub{cfg: cfg}
		for _, commandType := range []string{"workspace.git.read", "workspace.git.mutate", "workspace.git.unknown", "workspace.files.read", "session.open", "session.send"} {
			want := timeout
			if want == 0 {
				want = 45 * time.Second
				if commandType == "workspace.git.read" || commandType == "workspace.git.mutate" {
					want = 95 * time.Second
				}
			}
			if got := h.commandWaitTimeout(commandType); got != want {
				t.Errorf("configured=%v command=%s got=%v want=%v", timeout, commandType, got, want)
			}
		}
	}
}

func TestGitCommandExplicitTimeoutAndPhoneCancellationReleasePending(t *testing.T) {
	for _, cancelled := range []bool{false, true} {
		h, conn := receiptFixture(t, 25*time.Millisecond)
		ctx, cancel := context.WithCancel(context.Background())
		completed := make(chan int, 1)
		go func() {
			_, status := h.executeCommand(ctx, "receipt-owner", "pc", json.RawMessage(`{"type":"workspace.git.read","action":"review","repositoryPath":"mobile"}`), func(*device) bool { return true }, "")
			completed <- status
		}()
		select {
		case <-conn.cmds:
		case <-time.After(time.Second):
			t.Fatal("Git command was not dispatched")
		}
		want := 504
		if cancelled {
			want = 499
			cancel()
		}
		select {
		case status := <-completed:
			if status != want {
				t.Errorf("cancelled=%v status=%d want=%d", cancelled, status, want)
			}
		case <-time.After(time.Second):
			t.Error("Git command ignored the configured timeout or cancellation")
		}
		cancel()
		h.mu.Lock()
		pending := len(h.pending)
		h.mu.Unlock()
		if pending != 0 {
			t.Errorf("finished Git command retained %d pending slots", pending)
		}
	}
}
