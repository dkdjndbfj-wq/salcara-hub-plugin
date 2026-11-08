package hub

import (
	"encoding/json"
	"strings"
	"testing"
)

func diffCommandFixture() map[string]any {
	return map[string]any{"type": "session.send", "sessionKey": "codex:s", "controlSurface": "cli", "text": "review", "contextReferences": []any{map[string]any{"kind": "diff", "sessionKey": "codex:s", "label": "old.ts", "path": "src/old.ts", "side": "old", "startLine": 10, "endLine": 11, "text": "removed\nline", "sourceRevision": strings.Repeat("a", 64), "annotation": "keep compatibility", "capturedAt": 1}}}
}
func TestDiffAnnotationsValidationAndSignedIdentity(t *testing.T) {
	cmd := diffCommandFixture()
	wire, _ := json.Marshal(cmd)
	if _, err := commandMeta(wire); err != nil {
		t.Fatal(err)
	}
	original, err := canonicalCommand(wire)
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"path", "side", "startLine", "endLine", "text", "sourceRevision", "annotation"} {
		fresh := diffCommandFixture()
		ref := fresh["contextReferences"].([]any)[0].(map[string]any)
		if field == "startLine" || field == "endLine" {
			ref[field] = 12
		} else {
			ref[field] = "changed"
		}
		changed, _ := json.Marshal(fresh)
		canonical, _ := canonicalCommand(changed)
		if shaHex(original) == shaHex(canonical) {
			t.Fatal("not signed", field)
		}
	}
	for key, value := range map[string]any{"side": "current", "sourceRevision": "HEAD", "path": "../key", "startLine": 0, "endLine": 12, "textBasis": "source", "messageId": "m", "projectPath": "/p", "blockIndex": 0, "annotation": 7, "capturedAt": nil} {
		fresh := diffCommandFixture()
		fresh["contextReferences"].([]any)[0].(map[string]any)[key] = value
		bad, _ := json.Marshal(fresh)
		if _, err := commandMeta(bad); err == nil {
			t.Fatal("accepted", key)
		}
	}
	duplicate := strings.Replace(string(wire), `"side":"old"`, `"side":"old","side":"new"`, 1)
	if _, err := commandMeta([]byte(duplicate)); err == nil {
		t.Fatal("duplicate field accepted")
	}
	if _, err := commandMeta([]byte(`{"type":"session.send","contextReferences":[{"kind":"file","legacy":true}]}`)); err != nil {
		t.Fatal("legacy reference behavior changed", err)
	}
}
