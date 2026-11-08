package hub

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestDeviceNamespaceCannotEnrollThroughLegacyRegister(t *testing.T) {
	h := newDeviceHub(t, Config{})
	const id = "owner-pc"
	secret := strings.Repeat("a", 64)
	enrollDevice(t, h, id, secret)
	before := h.deviceMetadataBytes
	var accepted atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < 32; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := deviceRequest(t, h, "/bridge/register", id, secret, "", Device{DeviceID: fmt.Sprintf("other-%d", i)}, "")
			if w.Code != http.StatusForbidden {
				accepted.Add(1)
			}
		}(i)
	}
	wg.Wait()
	if accepted.Load() != 0 || len(h.accounts) != 1 || len(h.accounts[deviceAccount(id)].devices) != 1 || h.deviceMetadataBytes != before {
		t.Fatalf("device credential escaped its namespace: non-forbidden=%d namespaces=%d devices=%d metadata=%d/%d", accepted.Load(), len(h.accounts), len(h.accounts[deviceAccount(id)].devices), h.deviceMetadataBytes, before)
	}
	// The same authenticated computer may still use the legacy route to reconnect.
	w := deviceRequest(t, h, "/bridge/register", id, secret, "", Device{DeviceID: id, Name: "Updated PC"}, "")
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "Updated PC") {
		t.Fatalf("owner reconnect rejected: %d %s", w.Code, w.Body)
	}
}

// Model an on-disk record created by the old registration bypass. Keep it on
// disk so an upgrade cannot make an unauthorized identity usable again.
func oldNamespaceFixture(t *testing.T) (*Hub, string, string, string) {
	t.Helper()
	dir := t.TempDir()
	secret, goodToken, badToken := strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64)
	secretHash, goodHash, badHash := sha256.Sum256([]byte(secret)), sha256.Sum256([]byte(goodToken)), sha256.Sum256([]byte(badToken))
	st := persistedState{Version: 2, Accounts: map[string][]persistedDevice{
		deviceAccount("owner-pc"): {
			{Device: Device{DeviceID: "owner-pc"}, SecretHash: hex.EncodeToString(secretHash[:]), PairHash: hex.EncodeToString(goodHash[:])},
			{Device: Device{DeviceID: "extra-pc"}, SecretHash: hex.EncodeToString(secretHash[:]), PairHash: hex.EncodeToString(badHash[:])},
		},
	}}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, devicesFile), raw, 0600); err != nil {
		t.Fatal(err)
	}
	h := newDeviceHub(t, Config{DataDir: dir})
	return h, secret, goodToken, badToken
}

func TestPersistedExtraDeviceCannotUseOwnerCredentials(t *testing.T) {
	for _, tc := range []struct {
		name, path string
		body       any
		want       int
	}{
		{"register", "/bridge/register", Device{DeviceID: "extra-pc"}, 403},
		{"events", "/bridge/events", map[string]any{"deviceId": "extra-pc", "events": []any{}}, 403},
		{"pair-start", "/bridge/pair/start", map[string]string{"deviceId": "extra-pc"}, 403},
		{"pair-revoke", "/bridge/pair/revoke", map[string]string{"deviceId": "extra-pc"}, 403},
		{"reply-without-device", "/bridge/reply", map[string]any{"commandId": "extra-command", "ok": true}, 404},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, secret, _, _ := oldNamespaceFixture(t)
			h.mu.Lock()
			extra := h.accounts[deviceAccount("owner-pc")].devices["extra-pc"]
			extra.conn = &bridgeConn{closed: make(chan struct{}), cmds: make(chan []byte, 1)}
			h.pending["extra-command"] = &pendingCmd{account: deviceAccount("owner-pc"), deviceID: "extra-pc", ch: make(chan replyBody, 1)}
			h.mu.Unlock()
			w := deviceRequest(t, h, tc.path, "owner-pc", secret, "", tc.body, "")
			if w.Code != tc.want {
				t.Fatalf("old extra device accepted: %d %s", w.Code, w.Body)
			}
			if !extra.hasPair || h.pending["extra-command"] == nil || len(h.pairCodes) != 0 {
				t.Fatal("rejected request changed pairing or pending command")
			}
		})
	}
	t.Run("stream", func(t *testing.T) {
		h, secret, _, _ := oldNamespaceFixture(t)
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // Bound the reproducer even if the old handler opens an SSE stream.
		r := httptest.NewRequest(http.MethodGet, "/salcara-hub/v1/bridge/stream?deviceId=extra-pc", nil).WithContext(ctx)
		r.Header.Set("X-Salcara-Device-Id", "owner-pc")
		r.Header.Set("X-Salcara-Device-Secret", secret)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != http.StatusForbidden {
			t.Fatalf("extra stream accepted: %d", w.Code)
		}
	})
}

func TestPersistedExtraPairTokenRemainsInvalidAfterRestart(t *testing.T) {
	h, secret, goodToken, badToken := oldNamespaceFixture(t)
	for generation := 0; generation < 2; generation++ {
		bad := deviceRequest(t, h, "/app/devices", "", "", badToken, nil, "")
		if bad.Code != http.StatusForbidden {
			t.Fatalf("generation %d accepted old extra token: %d %s", generation, bad.Code, bad.Body)
		}
		good := deviceRequest(t, h, "/app/devices", "", "", goodToken, nil, "")
		if good.Code != http.StatusOK || strings.Contains(good.Body.String(), "extra-pc") {
			t.Fatalf("owner phone changed: %d %s", good.Code, good.Body)
		}
		enrollDevice(t, h, "owner-pc", secret)
		if len(h.snapshot().Accounts[deviceAccount("owner-pc")]) != 2 {
			t.Fatal("upgrade deleted historical records instead of rejecting unauthorized access")
		}
		if generation == 0 {
			dir := h.cfg.DataDir
			h.Close()
			h = newDeviceHub(t, Config{DataDir: dir})
		}
	}
}

func TestExtraDeviceCannotClaimStalePairAttempt(t *testing.T) {
	h, _, goodToken, _ := oldNamespaceFixture(t)
	for _, mode := range []string{"qr", "code"} {
		t.Run(mode, func(t *testing.T) {
			ticket := strings.Repeat("e", 64)
			key := pairKey(deviceAccount("owner-pc"), "extra-pc")
			attempt := &pairAttempt{qrHash: sha256.Sum256([]byte(ticket)), codeHash: sha256.Sum256([]byte("ABCDEFGH")), expires: time.Now().Add(time.Minute)}
			h.mu.Lock()
			extra := h.accounts[deviceAccount("owner-pc")].devices["extra-pc"]
			extra.conn = &bridgeConn{closed: make(chan struct{}), cmds: make(chan []byte, 1)}
			h.pairCodes[key], h.pairTickets[attempt.qrHash] = attempt, key
			h.mu.Unlock()
			var status int
			if mode == "qr" {
				status = deviceRequest(t, h, "/app/pair/qr", "", "", "", map[string]string{"deviceId": "extra-pc", "ticket": ticket}, "").Code
			} else {
				status = deviceRequest(t, h, "/app/pair/confirm", "", "", goodToken, map[string]string{"code": "ABCDEFGH"}, "").Code
			}
			if status != http.StatusForbidden {
				t.Fatalf("stale %s attempt issued extra device token: %d", mode, status)
			}
		})
	}
}

func TestLegacyAPIKeyDeviceLimitIsAtomicAndReconnectsRemainAllowed(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"group_id":7}`))
	}))
	defer upstream.Close()
	h := newDeviceHub(t, Config{Sub2APIURL: upstream.URL, AllowedGroupIDs: []int64{7}})
	secret := strings.Repeat("f", 64)
	var admitted, unexpected atomic.Int32
	var wg sync.WaitGroup
	for i := 0; i < maxDevices+24; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			w := requestHub(t, h, http.MethodPost, "/bridge/register", "sk-fixture", secret, "", Device{DeviceID: fmt.Sprintf("legacy-%d", i)})
			switch w.Code {
			case http.StatusOK:
				admitted.Add(1)
			case http.StatusTooManyRequests:
			default:
				unexpected.Add(1)
			}
		}(i)
	}
	wg.Wait()
	a := h.accounts[AccountID("sk-fixture")]
	if admitted.Load() != maxDevices || unexpected.Load() != 0 || len(a.devices) != maxDevices {
		t.Fatalf("legacy device quota: admitted=%d unexpected=%d devices=%d", admitted.Load(), unexpected.Load(), len(a.devices))
	}
	for id := range a.devices {
		w := requestHub(t, h, http.MethodPost, "/bridge/register", "sk-fixture", secret, "", Device{DeviceID: id})
		if w.Code != http.StatusOK {
			t.Fatalf("reconnect at quota: %d %s", w.Code, w.Body)
		}
		break
	}
}
