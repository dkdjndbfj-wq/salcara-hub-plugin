package hub

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestStoredConversationMetadataSurvivesHubForwarding(t *testing.T) {
	h := resourceHub(t, "economy")
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.accountLocked("fixture-owner")
	beforeSeq := a.seq
	raw := []byte(`{"type":"message","sessionKey":"codex:fixture","role":"user","text":"fixture","clientId":"fixture-correlation","messageOrigin":{"kind":"delegation"},"messagePhase":"commentary","errorCode":"model_capacity","retrying":true,"session":{"agentNickname":"Atlas","agentRole":"explorer","forkedFromSessionKey":"codex:parent"}}`)
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	stored, err := a.appendEventLocked("fixture-pc", fields)
	if err != nil {
		t.Fatal(err)
	}
	var forwarded map[string]json.RawMessage
	if err := json.Unmarshal(stored.data, &forwarded); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"clientId", "messageOrigin", "messagePhase", "errorCode", "retrying", "session"} {
		var before, after any
		if err := json.Unmarshal(fields[key], &before); err != nil {
			t.Fatal(err)
		}
		if err := json.Unmarshal(forwarded[key], &after); err != nil {
			t.Fatal(err)
		}
		if !reflect.DeepEqual(before, after) {
			t.Fatalf("Hub discarded %s", key)
		}
	}
	if stored.seq != beforeSeq+1 || stored.deviceID != "fixture-pc" || stored.sessionKey != "codex:fixture" {
		t.Fatal("metadata changed the station cursor or session identity")
	}
}
