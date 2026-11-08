package hub

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
)

var toolsSideQuestionActions = []string{"open", "read", "features", "send", "stop", "approve", "close"}

func newToolsSideQuestionFixture(t *testing.T, action string) *sourceReadReceiptFixture {
	t.Helper()
	f := newSourceReadReceiptFixture(t, "session.side-question.tools")
	f.req.Command = json.RawMessage(fmt.Sprintf(`{"type":"session.side-question.tools","action":%q,"accountId":"credential-1","tool":"codex","controlSurface":"cli","sessionKey":"codex:s","chainId":"chain-1","operationId":"operation-1"}`, action))
	return f
}

func TestToolsSideQuestionRequiresExecutionAuthorityForEveryAction(t *testing.T) {
	if !sourceExecutionCommand("session.side-question.tools") || sourceReadOrSafetyCommand("session.side-question.tools") || transientSourceRead("session.side-question.tools") {
		t.Fatal("tool conversation was not classified as temporary execution")
	}
	for _, action := range toolsSideQuestionActions {
		for _, disabled := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/disabled=%t", action, disabled), func(t *testing.T) {
				f := newToolsSideQuestionFixture(t, action)
				f.req.SourceGrant = nil
				code := "source_grant_required"
				if disabled {
					f.h.cfg.RemoteAccess.Mode = "disabled"
					code = "remote_execution_disabled"
				}
				w := awaitSourceReadResponse(t, f.start(t, context.Background(), f.req))
				if w.Code != http.StatusForbidden || !bytes.Contains(w.Body.Bytes(), []byte(code)) || len(f.conn.cmds) != 0 {
					t.Fatalf("unauthorized action reached Bridge: %d %s", w.Code, w.Body.String())
				}
				assertSourceReadReceiptCount(t, f.h, 0)
			})
		}
	}
}

func TestToolsSideQuestionActionsHaveNoHubReceipts(t *testing.T) {
	for _, action := range toolsSideQuestionActions {
		t.Run(action, func(t *testing.T) {
			f := newToolsSideQuestionFixture(t, action)
			for i := 0; i < 2; i++ {
				f.authorize(t) // Even the same request ID requires a fresh source grant.
				done := f.start(t, context.Background(), f.req)
				env, pending := f.pending(t)
				if env.Source == nil || env.Source.Mode != "restricted" || env.Source.CredentialHandle != "credential-1" || env.BindingID != "binding-1" {
					t.Fatal("tool conversation lost source or phone binding isolation")
				}
				result := fmt.Sprintf(`{"private":"temporary-content-%d"}`, i)
				pending.ch <- replyBody{OK: true, Result: json.RawMessage(result)}
				w := awaitSourceReadResponse(t, done)
				if w.Code != http.StatusOK || !bytes.Contains(w.Body.Bytes(), []byte(result)) {
					t.Fatalf("temporary result failed: %d %s", w.Code, w.Body.String())
				}
				assertSourceReadReleased(t, f.h)
				assertSourceReadReceiptCount(t, f.h, 0)
			}
			if f.h.cachedReceiptReplay("receipt-owner", f.pair, f.req) {
				t.Fatal("tool conversation became a replayable Hub receipt")
			}
			w := awaitSourceReadResponse(t, f.start(t, context.Background(), f.req))
			if w.Code != http.StatusForbidden || !bytes.Contains(w.Body.Bytes(), []byte("source_challenge_expired")) || len(f.conn.cmds) != 0 {
				t.Fatalf("consumed source grant was replayed: %d %s", w.Code, w.Body.String())
			}
			f.req.ReceiptOnly = true
			w = awaitSourceReadResponse(t, f.start(t, context.Background(), f.req))
			if w.Code != http.StatusBadRequest || !bytes.Contains(w.Body.Bytes(), []byte("command_receipt_unsupported")) || len(f.conn.cmds) != 0 {
				t.Fatalf("receipt-only request reached Bridge: %d %s", w.Code, w.Body.String())
			}
			assertSourceReadReceiptCount(t, f.h, 0)
		})
	}
}

func TestToolsSideQuestionRejectsCallerOwnerAndAmbiguousActions(t *testing.T) {
	valid := `{"type":"session.side-question.tools","action":"open","controlSurface":"cli","sessionKey":"codex:s"}`
	for _, raw := range []string{
		strings.Replace(valid, `"action":"open"`, `"action":"open","owner":"forged"`, 1),
		strings.Replace(valid, `"action":"open"`, `"action":"open","Owner":"forged"`, 1),
		strings.Replace(valid, `"action":"open"`, `"action":"open","Action":"send"`, 1),
		strings.Replace(valid, `"action":"open"`, `"action":"open","action":"send"`, 1),
		strings.Replace(valid, `"action":"open"`, `"action":"unknown"`, 1),
		strings.Replace(valid, `"action":"open"`, `"action":null`, 1),
		strings.Replace(valid, `"action":"open",`, ``, 1),
		strings.Replace(valid, `"cli"`, `"desktop"`, 1),
		strings.Replace(valid, `"codex:s"`, `" "`, 1),
	} {
		if _, err := commandMeta(json.RawMessage(raw)); err == nil {
			t.Fatalf("ambiguous or forged command accepted: %s", raw)
		}
	}
	for _, action := range toolsSideQuestionActions {
		if _, err := commandMeta(json.RawMessage(strings.Replace(valid, `"open"`, fmt.Sprintf("%q", action), 1))); err != nil {
			t.Fatalf("valid action %q rejected: %v", action, err)
		}
	}
}

func TestToolsSideQuestionSourceGrantBindsFullCommand(t *testing.T) {
	for _, field := range []string{"sessionKey", "chainId", "operationId", "action"} {
		t.Run(field, func(t *testing.T) {
			f := newToolsSideQuestionFixture(t, "open")
			f.authorize(t)
			var command map[string]any
			if err := json.Unmarshal(f.req.Command, &command); err != nil {
				t.Fatal(err)
			}
			command[field] = "another"
			if field == "action" {
				command[field] = "send"
			}
			f.req.Command, _ = json.Marshal(command)
			w := awaitSourceReadResponse(t, f.start(t, context.Background(), f.req))
			if w.Code != http.StatusForbidden || !bytes.Contains(w.Body.Bytes(), []byte("source_command_mismatch")) || len(f.conn.cmds) != 0 {
				t.Fatalf("modified action reached Bridge: %d %s", w.Code, w.Body.String())
			}
			assertSourceReadReceiptCount(t, f.h, 0)
		})
	}
}

func TestToolsSideQuestionSourceIdentityIsolation(t *testing.T) {
	for _, field := range []string{"device", "binding", "credential"} {
		t.Run(field, func(t *testing.T) {
			f := newToolsSideQuestionFixture(t, "send")
			f.authorize(t)
			switch field {
			case "device":
				f.snapshot.DeviceID = "other-device"
			case "binding":
				f.snapshot.BindingID = "other-binding"
			case "credential":
				f.snapshot.CredentialHandle = "other-credential"
			}
			signSourceFixture(t, &f.req, f.snapshot, f.private)
			w := awaitSourceReadResponse(t, f.start(t, context.Background(), f.req))
			if w.Code != http.StatusForbidden || len(f.conn.cmds) != 0 {
				t.Fatalf("cross-identity action reached Bridge: %d %s", w.Code, w.Body.String())
			}
			assertSourceReadReceiptCount(t, f.h, 0)
		})
	}
}

func TestToolsSideQuestionDropsCancelledAndRevokedResults(t *testing.T) {
	for _, action := range toolsSideQuestionActions {
		for _, revoke := range []string{"cancel", "policy", "binding"} {
			t.Run(action+"/"+revoke, func(t *testing.T) {
				f := newToolsSideQuestionFixture(t, action)
				f.authorize(t)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				done := f.start(t, ctx, f.req)
				_, pending := f.pending(t)
				if revoke == "cancel" {
					cancel()
				} else {
					f.h.remoteAccessGate.Lock()
					f.h.mu.Lock()
					if revoke == "policy" {
						f.h.cfg.RemoteAccess.Mode, f.h.cfg.RemoteAccess.Revision = "disabled", "revoked"
					} else {
						f.h.accounts["receipt-owner"].devices["pc"].bindingID = "replacement-binding"
					}
					f.h.mu.Unlock()
					f.h.remoteAccessGate.Unlock()
					pending.ch <- replyBody{OK: true, Result: json.RawMessage(`{"private":"retired-content"}`)}
				}
				w := awaitSourceReadResponse(t, done)
				if bytes.Contains(w.Body.Bytes(), []byte("retired-content")) || revoke == "cancel" && w.Body.Len() != 0 || revoke != "cancel" && w.Code != http.StatusForbidden {
					t.Fatalf("retired result was released: %d %s", w.Code, w.Body.String())
				}
				assertSourceReadReleased(t, f.h)
				assertSourceReadReceiptCount(t, f.h, 0)
			})
		}
	}
}
