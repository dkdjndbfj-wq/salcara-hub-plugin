package hub

import (
	"encoding/json"
	"testing"
)

func TestSessionContextAndCompactionRemainSourceBound(t *testing.T) {
	for _, typ := range []string{"session.context", "session.compact"} {
		t.Run(typ, func(t *testing.T) {
			if !sourceExecutionCommand(typ) || sourceReadOrSafetyCommand(typ) {
				t.Fatal("context control bypasses source policy")
			}
			h, r, req, snapshot, private := sourceContractFixture(t)
			command := map[string]any{"type": typ, "tool": "codex", "sessionKey": "codex:s", "controlSurface": "cli", "accountId": "credential-1"}
			if typ == "session.compact" {
				command["expectedRevision"], command["operationId"] = "runtime-1", "11111111-2222-4333-8444-555555555555"
			}
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
			for _, field := range []string{"sessionKey", "accountId", "expectedRevision", "operationId"} {
				changed := make(map[string]any)
				for k, v := range command {
					changed[k] = v
				}
				changed[field] = "changed"
				tampered := req
				tampered.Command, _ = json.Marshal(changed)
				if code := h.evaluateSourcePolicy("receipt-owner", r, tampered); code == "" {
					t.Fatal("accepted modified", field)
				}
			}
			req.SourceGrant = nil
			if code := h.evaluateSourcePolicy("receipt-owner", r, req); code != "source_grant_required" {
				t.Fatal(code)
			}
		})
	}
}
