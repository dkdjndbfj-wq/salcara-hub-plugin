package hub

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

type botStorageFixture struct {
	h                          *Hub
	pack                       BotBindingPackage
	token, deviceID, scope, id string
	png                        []byte
}

func newBotStorageFixture(t *testing.T) botStorageFixture {
	t.Helper()
	h, _, _ := botFixture(t)
	t.Cleanup(h.Close)
	h.durable.volumeProbe = healthyMailboxVolume
	f := botStorageFixture{h: h, deviceID: "storage-scope-computer", id: "att_0123456789abcdef0123456789abcdef", png: tinyPNG(t)}
	f.pack, f.token = botScopePhone(t, h, f.deviceID)
	auth := httptest.NewRequest(http.MethodGet, "/", nil)
	auth.Header.Set("X-Salcara-Bot-Token", f.token)
	_, f.scope, _ = h.appStorageIdentity("bot:"+f.pack.GroupID, auth, f.deviceID)
	if f.scope == "" {
		t.Fatal("storage fixture lacks phone scope")
	}
	return f
}

func (f botStorageFixture) seedBlob(t *testing.T) {
	t.Helper()
	if ready, err := f.h.durable.putBlobPart(f.scope, f.id, "image/png", 0, 1, f.png); err != nil || !ready {
		t.Fatalf("seed blob: ready=%v error=%v", ready, err)
	}
	if err := f.h.durable.retainCommandBlobs(f.scope, []byte(`{"sessionKey":"storage-session","attachments":["`+f.id+`"]}`), "operation:storage"); err != nil {
		t.Fatal(err)
	}
}

func (f botStorageFixture) request(method, path string, body []byte) *http.Request {
	r := httptest.NewRequest(method, path, bytes.NewReader(body))
	r.Header.Set("X-Salcara-Bot-Token", f.token)
	return r
}

func (f botStorageFixture) revoke(t *testing.T, pending bool) {
	t.Helper()
	want := http.StatusOK
	if pending {
		blockBotRegistrationSave(t, f.h, botGroupFile)
		want = http.StatusServiceUnavailable
	}
	w := botRequest(t, f.h, http.MethodPost, "/v1/bot/phone/revoke", nil, map[string]string{"X-Salcara-Bot-Token": f.token})
	if w.Code != want {
		t.Fatalf("revoke: %d %s", w.Code, w.Body)
	}
}

func waitBotStorageSQL(t *testing.T, h *Hub, before int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for h.durable.db.Stats().WaitCount <= before {
		if time.Now().After(deadline) {
			t.Fatal("request did not reach its SQL wait after authorization")
		}
		time.Sleep(time.Millisecond)
	}
}

func requireBotStorageDenied(t *testing.T, w *httptest.ResponseRecorder, pending bool) {
	t.Helper()
	status, code := http.StatusForbidden, "bot_credentials_invalid"
	if pending {
		status, code = http.StatusServiceUnavailable, "bot_phone_revoke_pending"
	}
	if w.Code != status || !strings.Contains(w.Body.String(), code) {
		t.Fatalf("stale storage authority: %d %s", w.Code, w.Body)
	}
}

func TestBotStorageBlobReadsRecheckAfterSQLWait(t *testing.T) {
	for _, mode := range []string{"raw-original", "raw-thumbnail-cached", "raw-thumbnail-new", "command-original", "command-thumbnail-cached", "command-thumbnail-new"} {
		for _, pending := range []bool{false, true} {
			name := mode + "/retired"
			if pending {
				name = mode + "/pending"
			}
			t.Run(name, func(t *testing.T) {
				f := newBotStorageFixture(t)
				f.seedBlob(t)
				variant := "original"
				if strings.Contains(mode, "thumbnail") {
					variant = "thumbnail"
				}
				request := func() *http.Request {
					if strings.HasPrefix(mode, "raw-") {
						return f.request(http.MethodGet, "/v1/app/blobs?deviceId="+f.deviceID+"&id="+f.id+"&variant="+variant, nil)
					}
					raw, _ := json.Marshal(map[string]any{"deviceId": f.deviceID, "command": map[string]any{"type": "attachment.read", "id": f.id, "sessionKey": "storage-session", "variant": variant}})
					return f.request(http.MethodPost, "/v1/app/commands", raw)
				}
				control := httptest.NewRecorder()
				f.h.ServeHTTP(control, request())
				if control.Code != http.StatusOK || control.Body.Len() == 0 {
					t.Fatalf("legitimate image read: %d %s", control.Code, control.Body)
				}
				if mode == "raw-original" && !bytes.Equal(control.Body.Bytes(), f.png) {
					t.Fatal("legitimate original image changed")
				}
				if strings.HasSuffix(mode, "-new") {
					if _, err := f.h.durable.db.Exec(`DELETE FROM thumbnails WHERE id=?`, f.id); err != nil {
						t.Fatal(err)
					}
				} else if strings.Contains(mode, "thumbnail") {
					if _, err := f.h.durable.db.Exec(`UPDATE thumbnails SET accessed=100 WHERE id=?`, f.id); err != nil {
						t.Fatal(err)
					}
				}
				conn, err := f.h.durable.db.Conn(context.Background())
				if err != nil {
					t.Fatal(err)
				}
				defer conn.Close()
				waits := f.h.durable.db.Stats().WaitCount
				read := holdHTTPRead(t, f.h, request())
				waitBotStorageSQL(t, f.h, waits)
				f.revoke(t, pending)
				_ = conn.Close()
				w := awaitPhoneRead(t, read)
				requireBotStorageDenied(t, w, pending)
				if bytes.Equal(w.Body.Bytes(), f.png) || strings.Contains(w.Body.String(), base64.StdEncoding.EncodeToString(f.png)) {
					t.Fatal("revoked request released the old image")
				}
				if strings.Contains(mode, "thumbnail") {
					var accessed int64
					err := f.h.durable.db.QueryRow(`SELECT accessed FROM thumbnails WHERE id=?`, f.id).Scan(&accessed)
					if strings.HasSuffix(mode, "-new") && err == nil || strings.HasSuffix(mode, "-cached") && (err != nil || accessed != 100) {
						t.Fatalf("revoked thumbnail request mutated cache: accessed=%d error=%v", accessed, err)
					}
				}
			})
		}
	}
}

func TestBotStorageEventAckRechecksAfterSQLWait(t *testing.T) {
	for _, pending := range []bool{false, true} {
		name := "retired"
		if pending {
			name = "pending"
		}
		t.Run(name, func(t *testing.T) {
			f := newBotStorageFixture(t)
			event, err := prepareStoredEvent(f.deviceID, 2, map[string]json.RawMessage{"type": json.RawMessage(`"message"`), "sessionKey": json.RawMessage(`"storage-session"`)})
			if err != nil {
				t.Fatal(err)
			}
			if err := f.h.durable.storeEvents(f.scope, "", "", []*storedEvent{event}); err != nil {
				t.Fatal(err)
			}
			request := func(through int) *http.Request {
				raw, _ := json.Marshal(map[string]any{"deviceId": f.deviceID, "sessionKey": "storage-session", "through": through, "storageEpoch": f.h.durable.epoch})
				return f.request(http.MethodPost, "/v1/app/events/ack", raw)
			}
			control := httptest.NewRecorder()
			f.h.ServeHTTP(control, request(1))
			if control.Code != http.StatusOK {
				t.Fatalf("legitimate event ack: %d %s", control.Code, control.Body)
			}
			conn, err := f.h.durable.db.Conn(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer conn.Close()
			waits := f.h.durable.db.Stats().WaitCount
			read := holdHTTPRead(t, f.h, request(2))
			waitBotStorageSQL(t, f.h, waits)
			f.revoke(t, pending)
			_ = conn.Close()
			requireBotStorageDenied(t, awaitPhoneRead(t, read), pending)
			var seq int64
			if err := f.h.durable.db.QueryRow(`SELECT seq FROM acks WHERE scope=? AND session=?`, f.scope, shaHex("storage-session")).Scan(&seq); err != nil || seq != 1 {
				t.Fatalf("revoked request changed event ack: seq=%d error=%v", seq, err)
			}
		})
	}
}

type botStorageBodyGate struct {
	io.Reader
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (r *botStorageBodyGate) Read(data []byte) (int, error) {
	r.once.Do(func() { close(r.started); <-r.release })
	return r.Reader.Read(data)
}

func TestBotStorageRawUploadRechecksAfterBodyWait(t *testing.T) {
	for _, exists := range []bool{false, true} {
		name := "new"
		if exists {
			name = "idempotent-existing"
		}
		t.Run(name, func(t *testing.T) {
			f := newBotStorageFixture(t)
			if exists {
				f.seedBlob(t)
			}
			gate := &botStorageBodyGate{Reader: bytes.NewReader(f.png), started: make(chan struct{}), release: make(chan struct{})}
			r := httptest.NewRequest(http.MethodPost, "/v1/app/blobs?deviceId="+f.deviceID+"&attachmentId="+f.id, gate)
			r.ContentLength = int64(len(f.png))
			r.Header.Set("X-Salcara-Bot-Token", f.token)
			r.Header.Set("Content-Type", "image/png")
			read := holdHTTPRead(t, f.h, r)
			var releaseOnce sync.Once
			release := func() { releaseOnce.Do(func() { close(gate.release) }) }
			defer release()
			awaitSSESignal(t, gate.started, "upload did not reach body wait")
			// This call completes while the network body is still blocked.
			f.revoke(t, false)
			release()
			requireBotStorageDenied(t, awaitPhoneRead(t, read), false)
			var count int
			if err := f.h.durable.db.QueryRow(`SELECT COUNT(*) FROM blobs WHERE scope=? AND id=?`, f.scope, f.id).Scan(&count); err != nil || !exists && count != 0 || exists && count != 1 {
				t.Fatalf("revoked upload changed stored object: count=%d error=%v", count, err)
			}
		})
	}
}

func TestBotStorageRawStreamStopsBeforeNextPart(t *testing.T) {
	f := newBotStorageFixture(t)
	half := len(f.png) / 2
	for part, data := range [][]byte{f.png[:half], f.png[half:]} {
		if _, err := f.h.durable.putBlobPart(f.scope, f.id, "image/png", part, 2, data); err != nil {
			t.Fatal(err)
		}
	}
	r := f.request(http.MethodGet, "/v1/app/blobs?deviceId="+f.deviceID+"&id="+f.id, nil)
	w, _, done := startBlockedSSERequest(t, f.h, r, false)
	awaitSSESignal(t, w.blocked, "blob stream did not reach its network write")
	// Already-admitted bytes may be in the network writer. Revocation must
	// not wait for that network write, and no next part may be released.
	f.revoke(t, false)
	w.released.Do(func() { close(w.release) })
	awaitSSESignal(t, done, "revoked image stream did not terminate")
	if w.Code != http.StatusOK || !bytes.Equal(w.Body.Bytes(), f.png[:half]) || w.Header().Get("Content-Type") != "image/png" {
		t.Fatalf("stream did not stop cleanly at the next part: status=%d bytes=%d", w.Code, w.Body.Len())
	}
}
