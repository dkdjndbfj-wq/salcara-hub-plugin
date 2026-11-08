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

func TestFeatureCatalogConditionalWireRoundTrip(t *testing.T) {
	h, conn := receiptFixture(t, time.Second)
	token := strings.Repeat("ab", 32)
	h.mu.Lock()
	h.setPairLocked("receipt-owner", h.accounts["receipt-owner"].devices["pc"], sha256.Sum256([]byte(token)), true)
	h.mu.Unlock()
	command := map[string]any{"type": "features.list", "tool": "codex", "controlSurface": "cli", "accountId": "api-a", "cwd": "/project", "ifRevision": strings.Repeat("a", 64), "force": false}
	completed := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		completed <- requestHub(t, h, http.MethodPost, "/app/commands", "", "", token, map[string]any{"deviceId": "pc", "command": command})
	}()
	var envelope commandEnvelope
	select {
	case raw := <-conn.cmds:
		if err := json.Unmarshal(raw, &envelope); err != nil {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("conditional feature command not dispatched")
	}
	var forwarded map[string]any
	if err := json.Unmarshal(envelope.Command, &forwarded); err != nil || !reflect.DeepEqual(forwarded, command) {
		t.Fatal(forwarded, err)
	}
	h.mu.Lock()
	pending := h.pending[envelope.CommandID]
	h.mu.Unlock()
	result, _ := json.Marshal(map[string]any{"revision": command["ifRevision"], "notModified": true})
	pending.ch <- replyBody{OK: true, Result: result}
	reply := <-completed
	var body struct {
		OK     bool           `json:"ok"`
		Result map[string]any `json:"result"`
	}
	if err := json.Unmarshal(reply.Body.Bytes(), &body); err != nil || reply.Code != 200 || !body.OK || body.Result["notModified"] != true || body.Result["revision"] != command["ifRevision"] {
		t.Fatal(reply.Code, reply.Body.String(), err)
	}
}

func TestFeatureCatalogConditionalFieldsRemainSourceProtected(t *testing.T) {
	h, r, req, snapshot, private := sourceContractFixture(t)
	command := map[string]any{"type": "features.list", "tool": "codex", "accountId": "credential-1", "controlSurface": "cli", "cwd": "/project", "ifRevision": strings.Repeat("a", 64), "force": false}
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
	for _, field := range []string{"ifRevision", "force"} {
		changed := make(map[string]any)
		for key, value := range command {
			changed[key] = value
		}
		if field == "force" {
			changed[field] = true
		} else {
			changed[field] = strings.Repeat("b", 64)
		}
		tampered := req
		tampered.Command, _ = json.Marshal(changed)
		if code := h.evaluateSourcePolicy("receipt-owner", r, tampered); code == "" {
			t.Fatal("source grant accepted modified", field)
		}
	}
	req.SourceGrant = nil
	if code := h.evaluateSourcePolicy("receipt-owner", r, req); code != "source_grant_required" {
		t.Fatal("conditional catalog bypassed source policy", code)
	}
}
