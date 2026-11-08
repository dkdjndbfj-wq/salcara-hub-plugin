package hub

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestSkillMessageMetadataSurvivesHubForwarding(t *testing.T) {
	for _, tool := range []string{"codex", "claude"} {
		t.Run(tool, func(t *testing.T) {
			h := resourceHub(t, "economy")
			h.mu.Lock()
			defer h.mu.Unlock()
			a := h.accountLocked("fixture-owner")
			fields := map[string]json.RawMessage{
				"type": json.RawMessage(`"message"`), "role": json.RawMessage(`"user"`),
				"text":     json.RawMessage(`"literal $review /review"`),
				"clientId": json.RawMessage(`"11111111-2222-4333-8444-555555555555"`),
				"skills":   json.RawMessage(`[{"name":"review","path":"C:/项目/one/SKILL.md"},{"name":"review","path":"C:/项目/two/SKILL.md"}]`),
			}
			fields["sessionKey"], _ = json.Marshal(tool + ":fixture")
			stored, err := a.appendEventLocked("fixture-pc", fields)
			if err != nil {
				t.Fatal(err)
			}
			var got map[string]json.RawMessage
			if err := json.Unmarshal(stored.data, &got); err != nil {
				t.Fatal(err)
			}
			for _, key := range []string{"skills", "clientId", "text"} {
				var before, after any
				if err := json.Unmarshal(fields[key], &before); err != nil {
					t.Fatal(err)
				}
				if err := json.Unmarshal(got[key], &after); err != nil {
					t.Fatal(err)
				}
				if !reflect.DeepEqual(before, after) {
					t.Fatalf("Hub changed %s", key)
				}
			}
		})
	}
}
