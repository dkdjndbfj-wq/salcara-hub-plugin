package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReceiptRestartAfterClockRollback(t *testing.T) {
	for _, rollback := range []time.Duration{5 * time.Minute, 48 * time.Hour, 90 * 24 * time.Hour} {
		t.Run(rollback.String(), func(t *testing.T) {
			dataDir := t.TempDir()
			command := json.RawMessage(`{"type":"session.send","text":"already accepted"}`)
			canonical, err := canonicalCommand(command)
			if err != nil {
				t.Fatal(err)
			}
			owner := shaHex("receipt-owner\x00pc\x00pair-a")
			key := shaHex(owner + "\x00" + receiptRequestID)
			// Model a genuine receipt persisted before the host clock moved back:
			// its original issue time is ahead of the current startup wall clock.
			issuedAt := time.Now().Add(rollback).UnixMilli()
			disk := durableCommandReceipt{Version: 1, Owner: owner, Hash: shaHex(canonical), ExpiresAt: issuedAt + commandReceiptTTL.Milliseconds()}
			before, err := newCommandReceipts(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			if err := before.persistLocked(key, disk); err != nil {
				t.Fatal(err)
			}
			before.close()
			path := filepath.Join(dataDir, "command-receipts", key+".json")
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}

			// Exercise the full Hub startup path, not just a parser helper.
			h, err := New(Config{DataDir: dataDir, PublicURL: "https://example.com/salcara-hub", CommandTimeout: 50 * time.Millisecond, Logger: slog.New(slog.NewTextHandler(io.Discard, nil))})
			if err != nil {
				t.Fatalf("valid receipt prevented startup after %v rollback: %v", rollback, err)
			}
			defer h.Close()
			entry := h.receipts.entries[key]
			if entry == nil || !entry.completed || entry.cached || entry.ExpiresAt != disk.ExpiresAt {
				t.Fatalf("original uncertain receipt not retained: %#v", entry)
			}
			conn := &bridgeConn{cmds: make(chan []byte, 10), closed: make(chan struct{})}
			h.mu.Lock()
			h.accountLocked("receipt-owner").devices["pc"] = &device{info: Device{DeviceID: "pc", Capabilities: []string{"source.guard.v1", "station.guard.v1"}}, conn: conn}
			h.mu.Unlock()
			_, status, code := h.executeReceipt(context.Background(), "receipt-owner", "pc", "pair-a", receiptRequestID, command, nil)
			if status != 409 || code != "command_delivery_uncertain" || len(conn.cmds) != 0 {
				t.Fatalf("same UUID was not protected after recovery: status=%d code=%q queued=%d", status, code, len(conn.cmds))
			}
			_, status, code = h.executeReceipt(context.Background(), "receipt-owner", "pc", "pair-a", receiptRequestID, json.RawMessage(`{"type":"session.send","text":"different"}`), nil)
			if status != 409 || code != "request_id_conflict" || len(conn.cmds) != 0 {
				t.Fatalf("UUID payload conflict lost after recovery: status=%d code=%q queued=%d", status, code, len(conn.cmds))
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(original, after) {
				t.Fatalf("recovery removed or rewrote the original receipt: %v", err)
			}
		})
	}
}

func TestReceiptRecoveryRejectsMalformedRecords(t *testing.T) {
	valid := durableCommandReceipt{Version: 1, Owner: shaHex("owner"), Hash: shaHex("command"), ExpiresAt: time.Now().Add(commandReceiptTTL + 5*time.Minute).UnixMilli()}
	cases := []struct {
		name   string
		change func(*durableCommandReceipt)
		raw    string
	}{
		{name: "unsupported_version", change: func(r *durableCommandReceipt) { r.Version = 3 }},
		{name: "missing_owner", change: func(r *durableCommandReceipt) { r.Owner = "" }},
		{name: "malformed_owner", change: func(r *durableCommandReceipt) { r.Owner = strings.Repeat("g", 64) }},
		{name: "short_hash", change: func(r *durableCommandReceipt) { r.Hash = "ab" }},
		{name: "malformed_hash", change: func(r *durableCommandReceipt) { r.Hash = strings.Repeat("x", 64) }},
		{name: "missing_expiry", change: func(r *durableCommandReceipt) { r.ExpiresAt = 0 }},
		{name: "negative_expiry", change: func(r *durableCommandReceipt) { r.ExpiresAt = -1 }},
		{name: "expiry_outside_timestamp_range", change: func(r *durableCommandReceipt) { r.ExpiresAt = maxCommandReceiptExpiry + 1 }},
		{name: "invalid_json", raw: "{"},
		{name: "oversized_file", raw: strings.Repeat(" ", 1025)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dataDir := t.TempDir()
			store, err := newCommandReceipts(dataDir)
			if err != nil {
				t.Fatal(err)
			}
			store.close()
			disk := valid
			if tc.change != nil {
				tc.change(&disk)
			}
			data, err := json.Marshal(disk)
			if err != nil {
				t.Fatal(err)
			}
			if tc.raw != "" {
				data = []byte(tc.raw)
			}
			path := filepath.Join(store.dir, shaHex("record")+".json")
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			loaded, err := newCommandReceipts(dataDir)
			if err == nil {
				loaded.close()
				t.Fatal("malformed receipt accepted")
			}
			if _, err := os.Stat(path); err != nil {
				t.Fatalf("malformed receipt was silently discarded: %v", err)
			}
		})
	}
}

func TestReceiptRecoveryRetainsExpiredLegacyReplayFence(t *testing.T) {
	dataDir := t.TempDir()
	store, err := newCommandReceipts(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	key := shaHex("expired")
	if err := store.persistLocked(key, durableCommandReceipt{Version: 1, Owner: shaHex("owner"), Hash: shaHex("command"), ExpiresAt: time.Now().Add(-time.Minute).UnixMilli()}); err != nil {
		t.Fatal(err)
	}
	store.close()
	loaded, err := newCommandReceipts(dataDir)
	if err != nil {
		t.Fatal(err)
	}
	defer loaded.close()
	if len(loaded.entries) != 1 {
		t.Fatal("expired legacy identity was forgotten")
	}
	if _, err := os.Stat(filepath.Join(store.dir, key+".json")); err != nil {
		t.Fatalf("legacy replay fence removed: %v", err)
	}
}
