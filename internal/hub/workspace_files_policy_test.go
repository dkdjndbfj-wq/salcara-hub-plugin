package hub

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

var workspaceLocalTypes = []string{"workspace.project.preview", "workspace.artifacts", "workspace.files.list", "workspace.files.read", "workspace.project.files.list", "workspace.project.files.read", "workspace.files.write",
	"workspace.terminal.open", "workspace.terminal.read", "workspace.terminal.input", "workspace.terminal.resize", "workspace.terminal.close",
	"workspace.git.read", "workspace.git.mutate", "workspace.files.mutate", "workspace.preview.file", "workspace.preview.open", "workspace.preview.frame", "workspace.preview.input", "workspace.preview.close"}

// Local workspace tools use pairing authority, never model API authority. The
// Bridge independently checks the exact project/session and operation scope.
func TestWorkspaceFilesPolicyRetainsPairingAndNarrowReadClassification(t *testing.T) {
	for _, mode := range []string{"open", "restricted", "disabled"} {
		t.Run(mode, func(t *testing.T) {
			h, r, req, _, _ := sourceContractFixture(t)
			policy := h.CurrentRemoteAccessPolicy()
			policy.Mode = mode
			if err := h.ApplyRemoteAccessPolicy(policy, nil); err != nil {
				t.Fatal(err)
			}
			for _, typ := range workspaceLocalTypes {
				req.SourceGrant = nil
				req.Command, _ = json.Marshal(map[string]any{"type": typ, "sessionKey": "codex:fixture", "controlSurface": "cli", "path": "src/main.ts"})
				if code := h.evaluateSourcePolicy("receipt-owner", r, req); code != "" {
					t.Fatalf("paired %s rejected: %s", typ, code)
				}
				if !sourceReadOrSafetyCommand(typ) || sourceExecutionCommand(typ) {
					t.Fatalf("incorrect classification: %s", typ)
				}
				unpaired := r.Clone(r.Context())
				unpaired.Header.Del("X-Salcara-Pair-Token")
				if code := h.evaluateSourcePolicy("receipt-owner", unpaired, req); code != "device_authorization_expired" {
					t.Fatalf("unpaired read authorized: %s", code)
				}
			}
		})
	}
	for _, typ := range []string{"workspace.files.delete", "workspace.preview.start", "workspace.files.exec", "workspace.project.files.write", "workspace.project.files.exec", "workspace.terminal.exec", "workspace.terminal.attach-any", "workspace.preview.navigate-any"} {
		if sourceReadOrSafetyCommand(typ) || sourceExecutionCommand(typ) {
			t.Fatalf("expanded permission to %s", typ)
		}
	}
}

func TestWorkspaceFileResponsesCannotEnterDurableReceipts(t *testing.T) {
	h := &Hub{}
	for _, typ := range workspaceLocalTypes {
		raw, _ := json.Marshal(map[string]any{"type": typ})
		req := commandReq{DeviceID: "pc", RequestID: receiptRequestID, Command: raw}
		w := httptest.NewRecorder()
		r := httptest.NewRequest(http.MethodPost, "/api/app/commands", nil)
		if !h.handleReceiptCommand(w, r, "fixture", req, nil) || w.Code != http.StatusBadRequest {
			t.Fatalf("%s can enter receipt handling: %d", typ, w.Code)
		}
		var result map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result["code"] != "workspace_receipt_unsupported" {
			t.Fatalf("%s did not reject durable read receipt", typ)
		}
		req.RequestID = ""
		if h.handleReceiptCommand(httptest.NewRecorder(), r, "fixture", req, nil) {
			t.Fatalf("%s transient read incorrectly enters receipt path", typ)
		}
	}
}
