package hub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func projectlessCommand() map[string]any {
	return map[string]any{"type": "session.start", "tool": "codex", "cwd": "", "projectless": true, "controlSurface": "cli", "workspaceMode": "code", "prompt": "new task", "operationId": receiptRequestID}
}

func TestProjectlessStrictFieldsAndSourceAuthority(t *testing.T) {
	for _, mode := range []string{"code", "work"} {
		cmd := projectlessCommand()
		cmd["workspaceMode"] = mode
		raw, _ := json.Marshal(cmd)
		if _, err := commandMeta(raw); err != nil {
			t.Fatal(err)
		}
	}
	invalid := []map[string]any{{"projectless": false}, {"projectless": nil}, {"projectless": "true"}, {"projectless": 1}, {"cwd": "C:/other"}, {"cwd": " "}, {"cwd": nil}, {"workspaceMode": ""}, {"workspaceMode": " code"}, {"controlSurface": "desktop"}, {"entrypoint": "claude-desktop"}, {"entrypoint": nil}, {"operationId": "../escape"}, {"operationId": strings.ToUpper(receiptRequestID[:8]) + "-GGGG-4000-8000-000000000001"}, {"type": " session.start"}, {"tool": " codex"}, {"type": "session.send"}, {"type": "workspace.explorer"}, {"sessionKey": "codex:other"}, {"Projectless": true}}
	for i, change := range invalid {
		cmd := projectlessCommand()
		for key, value := range change {
			cmd[key] = value
		}
		raw, _ := json.Marshal(cmd)
		if _, err := commandMeta(raw); err == nil {
			t.Fatalf("accepted invalid case %d", i)
		}
	}
	for _, key := range []string{"cwd", "controlSurface", "workspaceMode", "operationId", "tool", "projectless"} {
		cmd := projectlessCommand()
		delete(cmd, key)
		raw, _ := json.Marshal(cmd)
		if _, err := commandMeta(raw); err == nil {
			t.Fatalf("accepted missing %s", key)
		}
	}
	raw, _ := json.Marshal(projectlessCommand())
	duplicate := append(append([]byte(nil), raw[:len(raw)-1]...), []byte(`,"projectless":false}`)...)
	if _, err := commandMeta(duplicate); err == nil {
		t.Fatal("accepted duplicate flag")
	}
	h, pair, req, snapshot, private := sourceContractFixture(t)
	cmd := projectlessCommand()
	cmd["accountId"] = "credential-1"
	cmd["model"] = "gpt"
	req.Command, _ = json.Marshal(cmd)
	req.SourceGrant = nil
	if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code != "source_grant_required" {
		t.Fatalf("flag bypassed model source: %s", code)
	}
	canonical, _ := canonicalCommand(req.Command)
	snapshot.CommandHash = shaHex(canonical)
	signSourceFixture(t, &req, snapshot, private)
	if code, source := h.executionSourceFor("receipt-owner", pair, req); code != "" || source == nil {
		t.Fatalf("valid source lost: %s %#v", code, source)
	}
	policy := h.CurrentRemoteAccessPolicy()
	policy.Mode = "disabled"
	if err := h.ApplyRemoteAccessPolicy(policy, nil); err != nil {
		t.Fatal(err)
	}
	if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code != "remote_execution_disabled" {
		t.Fatal("flag bypassed execution disable", code)
	}
}

func TestProjectlessDurablePrepareForwardReplayAndRestart(t *testing.T) {
	h, conn, token, _ := receiptLookupFixture(t)
	cmd := projectlessCommand()
	raw, _ := json.Marshal(cmd)
	prepared := prepareHTTP(t, h, token, receiptRequestID, raw)
	cmd["operationId"] = prepared["operationId"]
	raw, _ = json.Marshal(cmd)
	body := map[string]any{"deviceId": "pc", "requestId": prepared["operationId"], "executionTicket": prepared["ticket"], "command": json.RawMessage(raw)}
	finished := make(chan *httptest.ResponseRecorder, 1)
	go func() { finished <- lookupReceiptHTTP(h, token, "receipt-owner", body) }()
	var envelope commandEnvelope
	select {
	case value := <-conn.cmds:
		if err := json.Unmarshal(value, &envelope); err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("missing projectless dispatch")
	}
	var forwarded map[string]any
	if json.Unmarshal(envelope.Command, &forwarded) != nil || forwarded["projectless"] != true || forwarded["cwd"] != "" || forwarded["operationId"] != prepared["operationId"] {
		t.Fatal("lost explicit mode or original operation", string(envelope.Command))
	}
	h.mu.Lock()
	pending := h.pending[envelope.CommandID]
	h.mu.Unlock()
	if pending == nil {
		t.Fatal("missing reply receiver")
	}
	pending.ch <- replyBody{OK: true, Result: json.RawMessage(`{"sessionKey":"codex:independent"}`)}
	if result := <-finished; result.Code != 200 || !strings.Contains(result.Body.String(), `"sessionKey":"codex:independent"`) {
		t.Fatalf("start: %d %s", result.Code, result.Body.String())
	}
	h.receipts.close()
	restarted, err := newCommandReceipts(h.cfg.DataDir)
	if err != nil {
		t.Fatal(err)
	}
	restarted.store = h.durable
	h.receipts = restarted
	result := lookupReceiptHTTP(h, token, "receipt-owner", body)
	if result.Code != 200 || !strings.Contains(result.Body.String(), `"sessionKey":"codex:independent"`) || len(conn.cmds) != 0 {
		t.Fatalf("restart lost exact receipt or redispatched: %d %s", result.Code, result.Body.String())
	}
	cmd["prompt"] = "different task"
	changed, _ := json.Marshal(cmd)
	body["command"] = json.RawMessage(changed)
	if result = lookupReceiptHTTP(h, token, "receipt-owner", body); result.Code != 409 || len(conn.cmds) != 0 {
		t.Fatalf("changed body reused original operation: %d %s", result.Code, result.Body.String())
	}
	delete(cmd, "projectless")
	cmd["cwd"] = "C:/other"
	cmd["prompt"] = "new task"
	changed, _ = json.Marshal(cmd)
	body["command"] = json.RawMessage(changed)
	if result = lookupReceiptHTTP(h, token, "receipt-owner", body); result.Code != 409 || len(conn.cmds) != 0 {
		t.Fatal("normal project collided with projectless receipt")
	}
}

func explorerCommand(action, cwd, path string) map[string]any {
	return map[string]any{"type": "workspace.explorer", "controlSurface": "cli", "tool": "codex", "workspaceMode": "code", "action": action, "cwd": cwd, "path": path}
}

func TestWorkspaceExplorerStrictSchemaAndPairedAuthority(t *testing.T) {
	h, pair, req, _, _ := sourceContractFixture(t)
	req.SourceGrant = nil
	req.RequestID = ""
	for _, mode := range []string{"open", "restricted", "disabled"} {
		policy := h.CurrentRemoteAccessPolicy()
		policy.Mode = mode
		if err := h.ApplyRemoteAccessPolicy(policy, nil); err != nil {
			t.Fatal(err)
		}
		for _, cmd := range []map[string]any{explorerCommand("list", "", ""), explorerCommand("list", "C:/", "Windows"), explorerCommand("list", "/", "home/user/.config"), explorerCommand("mkdir", "C:/", "Users/user/.projects")} {
			if cmd["action"] == "mkdir" {
				cmd["name"] = "new"
			}
			req.Command, _ = json.Marshal(cmd)
			if code, source := h.executionSourceFor("receipt-owner", pair, req); code != "" || source != nil {
				t.Fatalf("paired metadata gained model authority: %s %#v", code, source)
			}
			unpaired := pair.Clone(pair.Context())
			unpaired.Header.Del("X-Salcara-Pair-Token")
			if code := h.evaluateSourcePolicy("receipt-owner", unpaired, req); code != "device_authorization_expired" {
				t.Fatal("unpaired browsing accepted", code)
			}
		}
	}
	changes := []map[string]any{{"action": "select"}, {"action": "delete"}, {"cwd": "", "path": "Users"}, {"cwd": "", "cursor": ""}, {"path": "../Windows"}, {"path": "Users/../Windows"}, {"path": "Users//other"}, {"path": "/absolute"}, {"path": "C:/absolute"}, {"path": ".hidden/../escape"}, {"path": "NUL.txt"}, {"path": "x\u202ey"}, {"path": nil}, {"controlSurface": "desktop"}, {"workspaceMode": "other"}, {"tool": "../codex"}, {"accountId": "api_00112233"}, {"projectless": true}, {"source": "open"}, {"name": "unexpected"}, {"action": "mkdir"}, {"action": "mkdir", "name": ".."}, {"action": "mkdir", "name": "child/path"}, {"action": "mkdir", "name": "COM1"}, {"action": "mkdir", "name": "new", "cursor": ""}, {"cursor": "bad cursor"}}
	for i, change := range changes {
		cmd := explorerCommand("list", "C:/", "")
		for key, value := range change {
			cmd[key] = value
		}
		raw, _ := json.Marshal(cmd)
		if _, err := commandMeta(raw); err == nil {
			t.Fatalf("accepted invalid explorer case %d", i)
		}
	}
	for _, key := range []string{"controlSurface", "tool", "workspaceMode", "action", "cwd", "path"} {
		cmd := explorerCommand("list", "", "")
		delete(cmd, key)
		raw, _ := json.Marshal(cmd)
		if _, err := commandMeta(raw); err == nil {
			t.Fatalf("missing field %s", key)
		}
	}
	if !sourceWorkspaceCommand("workspace.explorer") || sourceExecutionCommand("workspace.explorer") || sourceWorkspaceCommand("workspace.explorer.exec") {
		t.Fatal("explorer authority classification changed")
	}
}

func TestWorkspaceExplorerTransientRepliesAndRevocation(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "exact reply", true: "revoked reply"}[revoke], func(t *testing.T) {
			h, pair, req, _, _ := sourceContractFixture(t)
			req.SourceGrant = nil
			req.RequestID = ""
			req.Command, _ = json.Marshal(explorerCommand("list", "C:/", "Windows"))
			conn := h.accounts["receipt-owner"].devices["pc"].conn
			wrong := req
			wrong.DeviceID = "other-pc"
			w := httptest.NewRecorder()
			h.handleAppCommands(w, workspaceProjectsRequest(t, pair, wrong), "receipt-owner")
			if w.Code != http.StatusForbidden || len(conn.cmds) != 0 {
				t.Fatal("wrong computer received explorer")
			}
			withReceipt := req
			withReceipt.RequestID = receiptRequestID
			w = httptest.NewRecorder()
			h.handleAppCommands(w, workspaceProjectsRequest(t, pair, withReceipt), "receipt-owner")
			if w.Code != http.StatusBadRequest || len(conn.cmds) != 0 {
				t.Fatal("explorer accepted content receipt")
			}
			finished := make(chan *httptest.ResponseRecorder, 1)
			go func() {
				w := httptest.NewRecorder()
				h.handleAppCommands(w, workspaceProjectsRequest(t, pair, req), "receipt-owner")
				finished <- w
			}()
			var env commandEnvelope
			select {
			case value := <-conn.cmds:
				if json.Unmarshal(value, &env) != nil {
					t.Fatal("invalid envelope")
				}
			case <-time.After(3 * time.Second):
				t.Fatal("missing explorer dispatch")
			}
			if !bytes.Equal(env.Command, req.Command) || env.Source != nil || env.DeviceID != "pc" || env.BindingID != "binding-1" {
				t.Fatal("changed explorer identity")
			}
			h.mu.Lock()
			pending := h.pending[env.CommandID]
			if revoke {
				h.revokePairLocked("receipt-owner", h.accounts["receipt-owner"].devices["pc"])
			}
			h.mu.Unlock()
			pending.ch <- replyBody{OK: true, Result: json.RawMessage(`{"tool":"codex","workspaceMode":"code","cwd":"C:/","path":"Windows","directory":{"path":"Windows","entries":[{"name":"System32","path":"Windows/System32","kind":"directory"}]}}`)}
			w = <-finished
			if revoke {
				if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "System32") {
					t.Fatal("revoked directory reply exposed")
				}
			} else if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "System32") {
				t.Fatal("directory reply missing")
			}
			files, err := os.ReadDir(filepath.Join(h.cfg.DataDir, "command-receipts"))
			if err != nil && !os.IsNotExist(err) || len(files) != 0 {
				t.Fatal("directory content was persisted")
			}
		})
	}
}

func TestNewConversationCapabilitiesReflectDurableSupport(t *testing.T) {
	h := &Hub{}
	caps, _ := h.commandCapabilities()
	if slices.Contains(caps, "session.projectless.v1") || !slices.Contains(caps, "workspace.explorer.v1") {
		t.Fatal("ephemeral capability mismatch")
	}
	h, _ = receiptFixture(t, time.Second)
	caps, _ = h.commandCapabilities()
	if !slices.Contains(caps, "session.projectless.v1") || !slices.Contains(caps, "workspace.explorer.v1") {
		t.Fatal("complete capability missing")
	}
}
