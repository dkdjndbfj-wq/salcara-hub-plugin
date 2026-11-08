package hub

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"
)

// These are independent synthetic device/phone identities, not real model
// accounts. Exercise ServeHTTP, durable receipts and cursor replay together.
type concurrentPhone struct {
	id, secret, token, binding, phoneHash, ip string
	conn                                      *bridgeConn
}

func concurrentRequest(t *testing.T, h *Hub, p concurrentPhone, path string, bridge bool, body any) *httptest.ResponseRecorder {
	t.Helper()
	b, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	method := http.MethodPost
	if body == nil {
		method = http.MethodGet
	}
	r := httptest.NewRequest(method, "http://example.test/salcara-hub/v1"+path, bytes.NewReader(b))
	r.RemoteAddr = p.ip + ":1234"
	r.Header.Set("Content-Type", "application/json")
	if bridge {
		r.Header.Set("X-Salcara-Device-Id", p.id)
		r.Header.Set("X-Salcara-Device-Secret", p.secret)
		if path == "/device/register" {
			r.Header.Set("X-Salcara-Enrollment-Claim", testEnrollmentClaim(t, h))
		}
		r.Header.Set("X-Salcara-Binding-Id", p.binding)
		r.Header.Set("X-Salcara-Phone-Hash", p.phoneHash)
	} else {
		r.Header.Set("X-Salcara-Pair-Token", p.token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func concurrentPhones(t *testing.T, h *Hub, n int) []concurrentPhone {
	t.Helper()
	phones := make([]concurrentPhone, n)
	for i := range phones {
		p := concurrentPhone{id: fmt.Sprintf("parallel-pc-%02d", i), secret: fmt.Sprintf("%064x", i+1), binding: fmt.Sprintf("%064x", i+101), ip: fmt.Sprintf("192.0.2.%d", i+1)}
		phoneID := fmt.Sprintf("%064x", i+201)
		hash := sha256.Sum256([]byte(phoneID))
		p.phoneHash = hex.EncodeToString(hash[:])
		register := concurrentRequest(t, h, p, "/device/register", true, Device{DeviceID: p.id, Name: "Synthetic PC", Projects: json.RawMessage(`[]`), Tools: json.RawMessage(`[]`), Capabilities: []string{"source.guard.v1", "station.guard.v1"}})
		if register.Code != 200 {
			t.Fatalf("register %d: %d", i, register.Code)
		}
		p.conn = onlineDevice(t, h, p.id).conn
		qr := concurrentRequest(t, h, p, "/bridge/pair/start", true, map[string]string{"deviceId": p.id, "bindingId": p.binding, "phoneHash": p.phoneHash, "computerId": "physical-" + p.id})
		var ticket struct {
			Ticket string `json:"ticket"`
		}
		if qr.Code != 200 || json.Unmarshal(qr.Body.Bytes(), &ticket) != nil || ticket.Ticket == "" {
			t.Fatalf("pair start %d: %d", i, qr.Code)
		}
		claim := concurrentRequest(t, h, p, "/app/pair/qr", false, map[string]string{"deviceId": p.id, "ticket": ticket.Ticket, "phoneId": phoneID})
		var paired struct {
			Token string `json:"pair_token"`
		}
		if claim.Code != 200 || json.Unmarshal(claim.Body.Bytes(), &paired) != nil || paired.Token == "" {
			t.Fatalf("pair claim %d: %d", i, claim.Code)
		}
		p.token = paired.Token
		phones[i] = p
	}
	return phones
}

func readConcurrentEnvelope(t *testing.T, conn *bridgeConn) commandEnvelope {
	t.Helper()
	select {
	case raw := <-conn.cmds:
		var envelope commandEnvelope
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
		return envelope
	case <-time.After(10 * time.Second):
		t.Fatal("synthetic command was not delivered")
	}
	return commandEnvelope{}
}

func TestConcurrentUsersIsolateCommandsReceiptsEventsAndRevocation(t *testing.T) {
	h := newDeviceHub(t, Config{DataDir: t.TempDir(), PublicURL: "https://example.com/salcara-hub", ResourceMode: "economy", CommandTimeout: 15 * time.Second})
	phones := concurrentPhones(t, h, 32)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for i, p := range phones {
		wg.Add(1)
		go func(i int, p concurrentPhone) {
			defer wg.Done()
			<-start
			// Use a mutation to exercise durable replay. Model/context/skill
			// reads intentionally execute afresh and cannot retain snapshots.
			body := map[string]any{"deviceId": p.id, "requestId": receiptRequestID, "command": map[string]any{"type": "session.compact", "agent": "codex", "sessionKey": "codex:same-session-id", "expectedRevision": "synthetic-context", "operationId": receiptRequestID, "accountId": "synthetic-" + p.id, "controlSurface": "cli"}}
			replies := make(chan *httptest.ResponseRecorder, 2)
			// Same request UUID is deliberately reused by every independent phone,
			// and twice concurrently by each phone. Only one dispatch per owner.
			for range 2 {
				go func() { replies <- concurrentRequest(t, h, p, "/app/commands", false, body) }()
			}
			env := readConcurrentEnvelope(t, p.conn)
			if env.DeviceID != p.id || !env.Phone || env.PhoneHash != p.phoneHash || env.BindingID != p.binding {
				t.Error("command identity crossed users")
			}
			if env.Source == nil || env.Source.Mode != "open" || env.Source.Station == nil || len(env.Source.Station.APIBases) != 1 || env.Source.Station.APIBases[0] != "https://example.com" || env.Source.Station.AllowOfficialAccounts {
				t.Error("concurrent compact command lost station boundary")
			}
			foreign := concurrentRequest(t, h, phones[(i+1)%len(phones)], "/bridge/reply", true, replyBody{DeviceID: phones[(i+1)%len(phones)].id, CommandID: env.CommandID, OK: true, Result: json.RawMessage(`{"wrong":true}`)})
			if foreign.Code == 200 {
				t.Error("foreign device completed another user's command")
			}
			value, _ := json.Marshal(map[string]string{"owner": p.id})
			ack := concurrentRequest(t, h, p, "/bridge/reply", true, replyBody{DeviceID: p.id, CommandID: env.CommandID, OK: true, Result: value})
			if ack.Code != 200 {
				t.Errorf("own reply: %d", ack.Code)
			}
			for range 2 {
				got := <-replies
				var result struct {
					OK     bool `json:"ok"`
					Result struct {
						Owner string `json:"owner"`
					} `json:"result"`
				}
				if got.Code != 200 || json.Unmarshal(got.Body.Bytes(), &result) != nil || !result.OK || result.Result.Owner != p.id {
					t.Errorf("receipt crossed users: owner=%s status=%d", p.id, got.Code)
				}
			}
			if len(p.conn.cmds) != 0 {
				t.Error("retry dispatched twice")
			}
			batch := map[string]any{"deviceId": p.id, "batchId": receiptRequestID, "events": []any{map[string]any{"type": "message", "id": "same-event-id", "sessionKey": "codex:same-session-id", "role": "assistant", "text": p.id, "final": true}}}
			for range 2 {
				if got := concurrentRequest(t, h, p, "/bridge/events", true, batch); got.Code != 200 {
					t.Errorf("events: %d", got.Code)
				}
			}
			pageResult := concurrentRequest(t, h, p, "/app/events?deviceId="+p.id+"&sessionKey=codex:same-session-id&after=0", false, nil)
			var page eventPage
			if pageResult.Code != 200 || json.Unmarshal(pageResult.Body.Bytes(), &page) != nil || len(page.Events) != 1 {
				t.Errorf("duplicate or missing events: %d", pageResult.Code)
				return
			}
			var event struct{ DeviceID, Text string }
			if json.Unmarshal(page.Events[0], &event) != nil || event.DeviceID != p.id || event.Text != p.id {
				t.Error("cursor page leaked another user's content")
			}
			if i == 0 {
				if got := concurrentRequest(t, h, p, "/app/pair/revoke", false, map[string]string{"deviceId": p.id}); got.Code != 200 {
					t.Error("revoke failed")
				}
			}
		}(i, p)
	}
	close(start)
	wg.Wait()
	for i, p := range phones {
		got := concurrentRequest(t, h, p, "/app/devices", false, nil)
		if i == 0 && got.Code == 200 || i != 0 && got.Code != 200 {
			t.Errorf("revoke affected wrong phone %d: %d", i, got.Code)
		}
	}
}

func TestOneUsersFullCommandQueueDoesNotBlockAnother(t *testing.T) {
	h := newDeviceHub(t, Config{DataDir: t.TempDir(), PublicURL: "https://example.com/salcara-hub", ResourceMode: "economy", CommandTimeout: 15 * time.Second})
	phones := concurrentPhones(t, h, 2)
	first, second := phones[0], phones[1]
	n := h.CurrentResourceMode().MaxAccountPendingCommands
	var wg sync.WaitGroup
	envelopes := make([]commandEnvelope, 0, n)
	for range n {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got := concurrentRequest(t, h, first, "/app/commands", false, map[string]any{"deviceId": first.id, "command": map[string]string{"type": "projects.list"}})
			if got.Code != 200 {
				t.Errorf("drain busy owner: %d", got.Code)
			}
		}()
		envelopes = append(envelopes, readConcurrentEnvelope(t, first.conn))
	}
	full := concurrentRequest(t, h, first, "/app/commands", false, map[string]any{"deviceId": first.id, "command": map[string]string{"type": "projects.list"}})
	if full.Code != http.StatusTooManyRequests {
		t.Errorf("per-owner admission limit missing: %d", full.Code)
	}
	secondReply := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		secondReply <- concurrentRequest(t, h, second, "/app/commands", false, map[string]any{"deviceId": second.id, "command": map[string]string{"type": "projects.list"}})
	}()
	env := readConcurrentEnvelope(t, second.conn)
	if got := concurrentRequest(t, h, second, "/bridge/reply", true, replyBody{DeviceID: second.id, CommandID: env.CommandID, OK: true}); got.Code != 200 {
		t.Errorf("other owner reply: %d", got.Code)
	}
	if got := <-secondReply; got.Code != 200 {
		t.Errorf("one saturated owner blocked other owner: %d", got.Code)
	}
	for _, env := range envelopes {
		if got := concurrentRequest(t, h, first, "/bridge/reply", true, replyBody{DeviceID: first.id, CommandID: env.CommandID, OK: true}); got.Code != 200 {
			t.Errorf("drain: %d", got.Code)
		}
	}
	wg.Wait()
}
