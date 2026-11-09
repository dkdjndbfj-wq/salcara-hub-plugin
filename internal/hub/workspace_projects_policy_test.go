package hub

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func workspaceProjectsCommand(action, cwd, path string) map[string]any {
	return map[string]any{"type": "workspace.projects", "controlSurface": "cli", "tool": "codex", "workspaceMode": "code", "action": action, "cwd": cwd, "path": path}
}

func TestWorkspaceProjectsUsesOnlyPairedComputerAuthority(t *testing.T) {
	for _, mode := range []string{"open", "restricted", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			h, r, req, _, _ := sourceContractFixture(t)
			p := h.CurrentRemoteAccessPolicy()
			p.Mode = mode
			if err := h.ApplyRemoteAccessPolicy(p, nil); err != nil {
				t.Fatal(err)
			}
			before := h.CurrentRemoteAccessPolicy()
			for _, workspaceMode := range []string{"code", "work"} {
				for _, action := range []string{"list", "select", "mkdir"} {
					cmd := workspaceProjectsCommand(action, "C:\\Projects\\allowed", "child")
					cmd["workspaceMode"] = workspaceMode
					if action == "mkdir" {
						cmd["name"] = "new-project"
					}
					req.Command, _ = json.Marshal(cmd)
					req.SourceGrant, req.RequestID = nil, ""
					if code, source := h.executionSourceFor("receipt-owner", r, req); code != "" || source != nil {
						t.Fatalf("paired %s %s rejected or given model authority: %s %#v", workspaceMode, action, code, source)
					}
					unpaired := r.Clone(r.Context())
					unpaired.Header.Del("X-Salcara-Pair-Token")
					if code := h.evaluateSourcePolicy("receipt-owner", unpaired, req); code != "device_authorization_expired" {
						t.Fatalf("unpaired project operation accepted: %s", code)
					}
				}
			}
			if !reflect.DeepEqual(before, h.CurrentRemoteAccessPolicy()) {
				t.Fatal("project operation changed model source permissions")
			}
			req.Command = json.RawMessage(`{"type":"models.list","tool":"codex"}`)
			code := h.evaluateSourcePolicy("receipt-owner", r, req)
			if mode == "restricted" && code != "source_grant_required" || mode == "disabled" && code != "remote_execution_disabled" {
				t.Fatalf("project authority bypassed model source policy: %s", code)
			}
		})
	}
	if !sourceWorkspaceCommand("workspace.projects") || !sourceReadOrSafetyCommand("workspace.projects") || sourceExecutionCommand("workspace.projects") {
		t.Fatal("project operation does not use the existing workspace authority")
	}
	for _, typ := range []string{"workspace.projects.exec", "workspace.projects.delete", "workspace.projects.authorize", "workspace.projects.start"} {
		if sourceWorkspaceCommand(typ) || sourceReadOrSafetyCommand(typ) || sourceExecutionCommand(typ) {
			t.Fatalf("unknown project command gained authority: %s", typ)
		}
	}
}

func TestWorkspaceProjectsStrictSchemaAndTraversal(t *testing.T) {
	h, r, req, _, _ := sourceContractFixture(t)
	req.SourceGrant, req.RequestID = nil, ""
	invalid := map[string]map[string]any{
		"wrong surface": {"controlSurface": "desktop"}, "wrong mode": {"workspaceMode": "all"},
		"unknown action": {"action": "exec"}, "empty tool": {"tool": ""}, "wrong tool type": {"tool": 2},
		"tool path": {"tool": "../codex"}, "tool whitespace": {"tool": " codex "},
		"agent bypass": {"agent": "claude"}, "session bypass": {"sessionKey": "claude:other"},
		"account bypass": {"accountId": "model-key"}, "source bypass": {"source": "open"},
		"phone root grant": {"roots": "C:\\"}, "wrong path type": {"path": []string{"child"}},
		"null path": {"path": nil}, "null cwd": {"cwd": nil}, "null cursor": {"cursor": nil},
		"case action": {"Action": "list"}, "case mode": {"WorkspaceMode": "work"},
		"traversal": {"path": "../outside"}, "nested traversal": {"path": "child/../../outside"},
		"dot path": {"path": "child/./other"}, "unix absolute": {"path": "/outside"},
		"windows absolute": {"path": "C:/outside"}, "windows traversal": {"path": "..\\outside"},
		"double separator": {"path": "child//other"}, "trailing separator": {"path": "child/"},
		"hidden directory": {"path": ".ssh"}, "system name": {"path": "NUL.txt"},
		"alternate stream": {"path": "child:other"}, "invisible path": {"path": "child\u202eother"},
		"long segment": {"path": strings.Repeat("a", 256)}, "control cwd": {"cwd": "C:\\allowed\nother"},
		"empty root with path":   {"cwd": "", "path": "child"},
		"empty root with cursor": {"cwd": "", "path": "", "cursor": "next"},
		"select without root":    {"action": "select", "cwd": "", "path": ""},
		"select with cursor":     {"action": "select", "cursor": "next"},
		"name on list":           {"name": "unexpected"}, "mkdir without name": {"action": "mkdir"},
		"mkdir traversal": {"action": "mkdir", "name": ".."},
		"mkdir nested":    {"action": "mkdir", "name": "parent/child"},
		"mkdir absolute":  {"action": "mkdir", "name": "C:\\outside"},
		"mkdir hidden":    {"action": "mkdir", "name": ".private"},
		"mkdir reserved":  {"action": "mkdir", "name": "COM1"},
	}
	for name, overrides := range invalid {
		t.Run(name, func(t *testing.T) {
			cmd := workspaceProjectsCommand("list", "C:\\Projects\\allowed", "")
			for key, value := range overrides {
				cmd[key] = value
			}
			req.Command, _ = json.Marshal(cmd)
			if code, source := h.executionSourceFor("receipt-owner", r, req); code != "source_command_invalid" || source != nil {
				t.Fatalf("invalid project request accepted: %s %#v", code, source)
			}
		})
	}
	for _, key := range []string{"controlSurface", "tool", "workspaceMode", "action", "cwd", "path"} {
		cmd := workspaceProjectsCommand("list", "", "")
		delete(cmd, key)
		raw, _ := json.Marshal(cmd)
		if _, err := commandMeta(raw); err == nil {
			t.Fatalf("missing required field accepted: %s", key)
		}
	}
	valid, _ := json.Marshal(workspaceProjectsCommand("list", "", ""))
	for _, extra := range []string{`,"action":"mkdir"}`, `,"tool":"claude"}`, `,"path":"../outside"}`} {
		raw := append(append([]byte(nil), valid[:len(valid)-1]...), extra...)
		if _, err := commandMeta(raw); err == nil {
			t.Fatalf("duplicate field accepted: %s", extra)
		}
	}
}

func workspaceProjectsRequest(t *testing.T, pair *http.Request, req commandReq) *http.Request {
	t.Helper()
	raw, err := json.Marshal(req)
	if err != nil {
		t.Fatal(err)
	}
	r := pair.Clone(pair.Context())
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return r
}

func TestWorkspaceProjectsUnknownAgentAndDirectoryDenialAreNotRetried(t *testing.T) {
	for _, rejection := range []struct{ tool, cwd, message string }{
		{"not-installed", "C:\\allowed", "agent_not_available: not-installed"},
		{"codex", "C:\\not-authorized", "project_not_authorized: allow this root on the computer"},
	} {
		t.Run(rejection.tool+rejection.cwd, func(t *testing.T) {
			h, pair, req, _, _ := sourceContractFixture(t)
			req.SourceGrant, req.RequestID = nil, ""
			cmd := workspaceProjectsCommand("select", rejection.cwd, "child")
			cmd["tool"], cmd["workspaceMode"] = rejection.tool, "work"
			req.Command, _ = json.Marshal(cmd)
			r := workspaceProjectsRequest(t, pair, req)
			finished := make(chan *httptest.ResponseRecorder, 1)
			go func() { w := httptest.NewRecorder(); h.handleAppCommands(w, r, "receipt-owner"); finished <- w }()
			conn := h.accounts["receipt-owner"].devices["pc"].conn
			var env commandEnvelope
			select {
			case raw := <-conn.cmds:
				if err := json.Unmarshal(raw, &env); err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("missing project dispatch")
			}
			if !bytes.Equal(env.Command, req.Command) || env.Source != nil || env.DeviceID != "pc" || env.BindingID != "binding-1" {
				t.Fatalf("project request changed identity or gained model authority: %#v", env)
			}
			h.mu.Lock()
			pending := h.pending[env.CommandID]
			h.mu.Unlock()
			pending.ch <- replyBody{OK: false, Error: rejection.message}
			w := <-finished
			if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), rejection.message) || len(conn.cmds) != 0 {
				t.Fatalf("Bridge denial was retried or hidden: %d %s", w.Code, w.Body.String())
			}
			files, err := os.ReadDir(filepath.Join(h.cfg.DataDir, "command-receipts"))
			if err != nil && !os.IsNotExist(err) || len(files) != 0 {
				t.Fatalf("transient project request created durable receipts: %v %v", files, err)
			}
		})
	}
}

func TestWorkspaceProjectsCannotUseOtherComputerOrLateUnpairedReply(t *testing.T) {
	for _, revoke := range []bool{false, true} {
		t.Run(map[bool]string{false: "other computer", true: "revoked before reply"}[revoke], func(t *testing.T) {
			h, pair, req, _, _ := sourceContractFixture(t)
			req.SourceGrant, req.RequestID = nil, ""
			req.Command, _ = json.Marshal(workspaceProjectsCommand("list", "", ""))
			secret, otherSecret := strings.Repeat("a", 64), strings.Repeat("b", 64)
			h.mu.Lock()
			dev := h.accounts["receipt-owner"].devices["pc"]
			dev.hasSecret, dev.secretHash = true, sha256.Sum256([]byte(secret))
			other := &device{info: Device{DeviceID: "other-pc"}, hasSecret: true, secretHash: sha256.Sum256([]byte(otherSecret)), conn: &bridgeConn{cmds: make(chan []byte, 1), closed: make(chan struct{})}}
			h.accounts["receipt-owner"].devices["other-pc"] = other
			h.mu.Unlock()
			wrongTarget := req
			wrongTarget.DeviceID = "other-pc"
			w := httptest.NewRecorder()
			h.handleAppCommands(w, workspaceProjectsRequest(t, pair, wrongTarget), "receipt-owner")
			if w.Code != http.StatusForbidden || len(other.conn.cmds) != 0 || len(dev.conn.cmds) != 0 {
				t.Fatal("pair token for one computer reached another computer")
			}
			r := workspaceProjectsRequest(t, pair, req)
			finished := make(chan *httptest.ResponseRecorder, 1)
			go func() { w := httptest.NewRecorder(); h.handleAppCommands(w, r, "receipt-owner"); finished <- w }()
			var env commandEnvelope
			select {
			case raw := <-dev.conn.cmds:
				if err := json.Unmarshal(raw, &env); err != nil {
					t.Fatal(err)
				}
			case <-time.After(time.Second):
				t.Fatal("missing project dispatch")
			}
			reply := func(deviceID, credential string) *httptest.ResponseRecorder {
				raw, _ := json.Marshal(replyBody{DeviceID: deviceID, CommandID: env.CommandID, OK: true, Result: json.RawMessage(`{"tool":"codex","workspaceMode":"code","cwd":"","path":"","roots":[{"path":"private-project","name":"private"}]}`)})
				r := httptest.NewRequest(http.MethodPost, "/bridge/reply", bytes.NewReader(raw))
				r.Header.Set("X-Salcara-Device-Id", deviceID)
				r.Header.Set("X-Salcara-Device-Secret", credential)
				w := httptest.NewRecorder()
				h.handleBridgeReply(w, r, "receipt-owner")
				return w
			}
			if got := reply("other-pc", otherSecret); got.Code != http.StatusNotFound {
				t.Fatalf("other computer supplied project response: %d", got.Code)
			}
			if revoke {
				h.mu.Lock()
				h.revokePairLocked("receipt-owner", dev)
				h.mu.Unlock()
			}
			got := reply("pc", secret)
			// Revocation may already have retired the pending receiver.
			if got.Code != http.StatusOK && (!revoke || got.Code != http.StatusNotFound) {
				t.Fatalf("unexpected Bridge reply status: %d", got.Code)
			}
			w = <-finished
			if revoke {
				if w.Code == http.StatusOK || strings.Contains(w.Body.String(), "private-project") {
					t.Fatalf("late reply exposed a revoked project: %d %s", w.Code, w.Body.String())
				}
			} else if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), "private-project") {
				t.Fatalf("correct computer response missing: %d %s", w.Code, w.Body.String())
			}
		})
	}
}

func TestWorkspaceProjectsRejectsPersistentContentReceipts(t *testing.T) {
	h := &Hub{}
	raw, _ := json.Marshal(workspaceProjectsCommand("list", "", ""))
	req := commandReq{DeviceID: "pc", RequestID: receiptRequestID, Command: raw}
	r := httptest.NewRequest(http.MethodPost, "/app/commands", nil)
	w := httptest.NewRecorder()
	if !h.handleReceiptCommand(w, r, "fixture", req, nil) || w.Code != http.StatusBadRequest || !strings.Contains(w.Body.String(), "workspace_receipt_unsupported") {
		t.Fatalf("projects entered durable receipt handling: %d %s", w.Code, w.Body.String())
	}
	req.RequestID = ""
	if h.handleReceiptCommand(httptest.NewRecorder(), r, "fixture", req, nil) {
		t.Fatal("transient projects entered receipt handling")
	}
}
