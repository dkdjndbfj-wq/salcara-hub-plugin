package hub

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestDesktopCompatibilityStopsOnlyNewWork(t *testing.T) {
	h, conn := receiptFixture(t, time.Second)
	h.cfg.DesktopMinimumVersion = "1.6.5"
	h.cfg.DesktopUpdateURL = "https://example.test/download"
	for _, version := range []string{"1.6.4", "unknown", "", "1.5.99"} {
		h.accounts["receipt-owner"].devices["pc"].info.Version = version
		for _, command := range []string{`{"type":"session.start"}`, `{"type":"session.send"}`, `{"type":"session.workflow","kind":"goal.start"}`, `{"type":"session.action","action":"fork"}`, `{"type":"workspace.terminal.open"}`} {
			rep, status := h.executeCommand(context.Background(), "receipt-owner", "pc", json.RawMessage(command), nil, "")
			if status != 426 || !strings.Contains(rep.Error, "update_required") || len(conn.cmds) != 0 {
				t.Fatalf("unsupported client dispatched: %s %s %d", version, command, status)
			}
		}
	}
	for _, command := range []string{`{"type":"approval.respond"}`, `{"type":"session.interrupt"}`, `{"type":"session.steer"}`, `{"type":"session.workflow"}`, `{"type":"session.workflow","kind":"goal.pause"}`, `{"type":"session.history"}`} {
		if h.desktopUpdateRequired("1.6.4", json.RawMessage(command)) {
			t.Fatalf("existing task blocked: %s", command)
		}
	}
	for _, version := range []string{"1.6.5", "1.10.0", "v1.6.5", "1.6.5+build"} {
		if h.desktopUpdateRequired(version, json.RawMessage(`{"type":"session.start"}`)) {
			t.Fatalf("supported version blocked: %s", version)
		}
	}
}

func TestDesktopCompatibilityInvalidConfigurationFailsStartup(t *testing.T) {
	for _, minimum := range []string{"1.6", "v1.6.5", "1.6.5-beta", "1.6.99999999999999999999999"} {
		if _, err := New(Config{DesktopMinimumVersion: minimum}); err == nil {
			t.Fatalf("accepted %s", minimum)
		}
	}
	if err := ValidateDesktopCompatibility("1.6.5", "http://example.test"); err == nil {
		t.Fatal("accepted insecure download link")
	}
}

func TestDesktopVersionRecheckedBeforeDispatch(t *testing.T) {
	h, conn := receiptFixture(t, time.Second)
	h.cfg.DesktopMinimumVersion = "1.6.5"
	h.accounts["receipt-owner"].devices["pc"].info.Version = "1.6.5"
	_, status := h.executeCommandGuarded(context.Background(), "receipt-owner", "pc", json.RawMessage(`{"type":"session.send"}`), nil, "", &h.remoteAccessGate, func() (string, *ExecutionSource) {
		h.mu.Lock()
		h.accounts["receipt-owner"].devices["pc"].info.Version = "1.6.4"
		h.mu.Unlock()
		return "", nil
	})
	if status != 426 || len(conn.cmds) != 0 {
		t.Fatal("stale admission dispatched")
	}
}
