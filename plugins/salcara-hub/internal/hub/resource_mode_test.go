package hub

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"time"
)

func resourceHub(t *testing.T, mode string) *Hub {
	t.Helper()
	h, err := New(Config{ResourceMode: mode, Logger: slog.New(slog.NewTextHandler(io.Discard, nil)), CommandTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(h.Close)
	return h
}

func eventFields(size int, session string) map[string]json.RawMessage {
	raw, _ := json.Marshal(strings.Repeat("x", size))
	sk, _ := json.Marshal(session)
	return map[string]json.RawMessage{"type": json.RawMessage(`"message.updated"`), "text": raw, "sessionKey": sk}
}

func TestResourceDefaultsAndFailedCommitLeaveOldMode(t *testing.T) {
	legacy := resourceHub(t, "")
	p := legacy.CurrentResourceMode()
	if p.ID != "" || p.AccountEvents != accountRingSize || p.SessionEvents != sessionRingSize || p.MaxAppStreams != maxAppStreams || p.MaxPendingCommands != maxPendingCommands {
		t.Fatal("legacy plugin defaults changed")
	}
	h := resourceHub(t, "economy")
	if err := h.ApplyResourceMode("performance", func() error { return errors.New("fixture disk failure") }); err == nil {
		t.Fatal("failed durable commit applied policy")
	}
	if h.CurrentResourceMode().ID != "economy" {
		t.Fatal("mode changed despite failed commit")
	}
	if err := h.ApplyResourceMode("unknown", nil); err == nil {
		t.Fatal("unknown mode accepted")
	}
	h.Close()
	if err := h.ApplyResourceMode("balanced", nil); err == nil {
		t.Fatal("closed Hub accepted resource mutation")
	}
}

func TestLiveResourceTrimBoundedBytesCursorAndBindingsRemain(t *testing.T) {
	h := resourceHub(t, "performance")
	h.mu.Lock()
	a := h.accountLocked("fixture-owner")
	a.devices["pc"] = &device{info: Device{DeviceID: "pc"}, hasSecret: true, hasPair: true, banned: true, banReason: "fixture"}
	for i := 0; i < 700; i++ {
		if _, err := a.appendEventLocked("pc", eventFields(128, "session")); err != nil {
			t.Fatal(err)
		}
	}
	oldCursor := a.seq - 650
	beforeSeq := a.seq
	h.mu.Unlock()
	if err := h.ApplyResourceMode("economy", nil); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if a.seq != beforeSeq || !a.devices["pc"].hasSecret || !a.devices["pc"].hasPair || !a.devices["pc"].banned || a.devices["pc"].banReason != "fixture" {
		t.Fatal("resource mode changed identity/ban/history cursor")
	}
	if a.events.n > 256 || a.events.bytes > 2<<20 || a.sessions[sessionID("pc", "session")].ring.n > 128 {
		t.Fatal("live ring count/byte resize failed")
	}
	if page := h.readEventPageLocked(a.id, "pc", "session", oldCursor, 500); !page.ResetRequired {
		t.Fatal("trimmed session pretended old cursor had full history")
	}
	for i := 0; i < 100; i++ {
		if _, err := a.appendEventLocked("pc", eventFields(64<<10, "large")); err != nil {
			t.Fatal(err)
		}
	}
	if a.events.bytes > 2<<20 || a.sessions[sessionID("pc", "large")].ring.bytes > 512<<10 || a.cacheBytes > 6<<20 {
		t.Fatal("event cache not byte bounded")
	}
}

func TestGlobalCacheBudgetLRUPreservesMetadataAndGapMarkers(t *testing.T) {
	h := resourceHub(t, "economy")
	h.mu.Lock()
	defer h.mu.Unlock()
	var oldest *account
	for i := 0; i < 24; i++ {
		id := string(rune('a' + i))
		a := h.accountLocked(id)
		a.devices["pc"] = &device{info: Device{DeviceID: "pc"}, hasSecret: true}
		if oldest == nil {
			oldest = a
		}
		for j := 0; j < 4; j++ {
			if _, err := a.appendEventLocked("pc", eventFields(240<<10, "session")); err != nil {
				t.Fatal(err)
			}
		}
		if h.eventCacheBytes > h.resource.GlobalEventCacheBytes {
			t.Fatal("global payload budget exceeded")
		}
	}
	if len(h.accounts) != 24 || oldest.devices["pc"] == nil || oldest.sessions[sessionID("pc", "session")] == nil {
		t.Fatal("global cache eviction deleted identities or session metadata")
	}
	if oldest.events.n != 0 || oldest.events.buf != nil || oldest.sessions[sessionID("pc", "session")].ring.buf != nil {
		t.Fatal("old LRU payload buffers not released")
	}
	if page := h.readEventPageLocked(oldest.id, "pc", "session", oldest.seq-2, 500); !page.ResetRequired {
		t.Fatal("global payload eviction lost resetRequired")
	}
	var actual int64
	for _, a := range h.accounts {
		if a.events != nil {
			actual += a.events.bytes
		}
		for _, sb := range a.sessions {
			actual += sb.ring.bytes
		}
	}
	if actual != h.eventCacheBytes {
		t.Fatal("incremental global cache accounting drifted")
	}
	if _, err := oldest.appendEventLocked("pc", eventFields(100, "session")); err != nil {
		t.Fatal(err)
	}
	if oldest.events.n != 1 || oldest.sessions[sessionID("pc", "session")].ring.n != 1 {
		t.Fatal("evicted lazy rings could not resume")
	}
}

func TestModeChangeDoesNotCancelPendingAcceptedCommand(t *testing.T) {
	h := resourceHub(t, "performance")
	conn := &bridgeConn{cmds: make(chan []byte, 4), closed: make(chan struct{})}
	h.mu.Lock()
	h.accountLocked("owner").devices["pc"] = &device{info: Device{DeviceID: "pc"}, conn: conn}
	h.mu.Unlock()
	result := make(chan int, 1)
	go func() {
		_, status := h.executeCommand(context.Background(), "owner", "pc", json.RawMessage(`{"type":"fixture"}`), nil, "")
		result <- status
	}()
	raw := <-conn.cmds
	var env commandEnvelope
	if json.Unmarshal(raw, &env) != nil {
		t.Fatal("invalid fixture envelope")
	}
	if err := h.ApplyResourceMode("economy", nil); err != nil {
		t.Fatal(err)
	}
	h.mu.Lock()
	pending := h.pending[env.CommandID]
	h.mu.Unlock()
	if pending == nil {
		t.Fatal("mode switch cancelled pending command")
	}
	pending.ch <- replyBody{OK: true, Result: json.RawMessage(`{"ok":true}`)}
	if status := <-result; status != 200 {
		t.Fatal("accepted task interrupted by resource mode")
	}
	h.mu.Lock()
	for i := 0; i < 64; i++ {
		h.pending[string(rune(i+1))] = &pendingCmd{account: "elsewhere"}
	}
	h.mu.Unlock()
	_, status := h.executeCommand(context.Background(), "owner", "pc", json.RawMessage(`{"type":"fixture"}`), nil, "")
	if status != 429 {
		t.Fatal("new command ignored current global admission limit")
	}
}
