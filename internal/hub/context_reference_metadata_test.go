package hub

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestContextReferenceMetadataSurvivesHubForwarding(t *testing.T) {
	h := resourceHub(t, "economy")
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.accountLocked("reference-fixture")
	fields := map[string]json.RawMessage{
		"type": json.RawMessage(`"message"`), "role": json.RawMessage(`"user"`), "sessionKey": json.RawMessage(`"codex:target"`),
		"text": json.RawMessage(`"original request"`), "clientId": json.RawMessage(`"11111111-2222-4333-8444-555555555555"`),
		"contextReferences": json.RawMessage(`[{"kind":"message","sessionKey":"codex:source","label":"message","text":"😀","messageId":"exact-id","startOffset":5,"endOffset":7,"capturedAt":1,"annotation":"请解释\n这句话","textBasis":"rendered","blockIndex":2,"sourceRevision":"aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}]`),
	}
	stored, err := a.appendEventLocked("fixture-pc", fields)
	if err != nil {
		t.Fatal(err)
	}
	var got map[string]json.RawMessage
	if json.Unmarshal(stored.data, &got) != nil {
		t.Fatal("invalid stored event")
	}
	for _, key := range []string{"text", "clientId", "contextReferences"} {
		var before, after any
		if json.Unmarshal(fields[key], &before) != nil || json.Unmarshal(got[key], &after) != nil || !reflect.DeepEqual(before, after) {
			t.Fatal("metadata changed", key)
		}
	}
}
