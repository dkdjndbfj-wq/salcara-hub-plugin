package hub

import (
	"crypto/sha256"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestFeatureSyncPagesAndDeltasForwardWithoutFlattening(t *testing.T) {
	for _, continuation := range []bool{false, true} {
		t.Run(map[bool]string{false: "delta", true: "continuation"}[continuation], func(t *testing.T) {
			h, conn := receiptFixture(t, time.Second)
			token := strings.Repeat("ab", 32)
			h.mu.Lock()
			h.setPairLocked("receipt-owner", h.accounts["receipt-owner"].devices["pc"], sha256.Sum256([]byte(token)), true)
			h.mu.Unlock()
			command := map[string]any{"type": "features.list", "tool": "codex", "controlSurface": "cli", "accountId": "api-a", "cwd": "/project", "syncVersion": float64(1), "pageSize": float64(200)}
			if continuation {
				command["cursor"] = strings.Repeat("b", 32) + ":200"
			} else {
				command["ifRevision"] = strings.Repeat("a", 64)
			}
			done := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				done <- requestHub(t, h, http.MethodPost, "/app/commands", "", "", token, map[string]any{"deviceId": "pc", "command": command})
			}()
			var envelope commandEnvelope
			select {
			case raw := <-conn.cmds:
				if err := json.Unmarshal(raw, &envelope); err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("page request not forwarded")
			}
			var forwarded map[string]any
			if err := json.Unmarshal(envelope.Command, &forwarded); err != nil || !reflect.DeepEqual(forwarded, command) {
				t.Fatal(forwarded, err)
			}
			payload := map[string]any{"syncVersion": float64(1), "kind": "delta", "revision": strings.Repeat("c", 64), "baseRevision": strings.Repeat("a", 64), "collaborationModes": []any{"default", "plan"}, "skillsSupported": true, "skills": []any{map[string]any{"name": "new", "description": "changed"}}, "removedSkills": []any{map[string]any{"name": "removed"}}, "totalSkills": float64(1200), "totalChanges": float64(300), "offset": float64(200), "nextCursor": strings.Repeat("b", 32) + ":202"}
			h.mu.Lock()
			pending := h.pending[envelope.CommandID]
			h.mu.Unlock()
			encoded, _ := json.Marshal(payload)
			pending.ch <- replyBody{OK: true, Result: encoded}
			reply := <-done
			var body struct {
				OK     bool           `json:"ok"`
				Result map[string]any `json:"result"`
			}
			if err := json.Unmarshal(reply.Body.Bytes(), &body); err != nil || reply.Code != 200 || !body.OK || !reflect.DeepEqual(body.Result, payload) {
				t.Fatal(reply.Code, reply.Body.String(), err)
			}
		})
	}
}

func TestFeatureSyncCursorAndNegotiationRemainSourceProtected(t *testing.T) {
	h, r, req, snapshot, private := sourceContractFixture(t)
	command := map[string]any{"type": "features.list", "tool": "codex", "accountId": "credential-1", "controlSurface": "cli", "cwd": "/project", "syncVersion": 1, "pageSize": 200, "cursor": strings.Repeat("a", 32) + ":200"}
	req.Command, _ = json.Marshal(command)
	canonical, err := canonicalCommand(req.Command)
	if err != nil {
		t.Fatal(err)
	}
	snapshot.CommandHash, snapshot.Model = shaHex(canonical), ""
	signSourceFixture(t, &req, snapshot, private)
	if code := h.evaluateSourcePolicy("receipt-owner", r, req); code != "" {
		t.Fatal(code)
	}
	for _, field := range []string{"syncVersion", "pageSize", "cursor"} {
		changed := map[string]any{}
		for key, value := range command {
			changed[key] = value
		}
		if field == "cursor" {
			changed[field] = strings.Repeat("b", 32) + ":200"
		} else {
			changed[field] = 2
		}
		tampered := req
		tampered.Command, _ = json.Marshal(changed)
		if code := h.evaluateSourcePolicy("receipt-owner", r, tampered); code == "" {
			t.Fatal("tampered page accepted", field)
		}
	}
}
