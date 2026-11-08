package hub

import (
	"encoding/json"
	"reflect"
	"testing"
)

func TestBrowserNavigationSurvivesHubLiveAndReplay(t *testing.T) {
	for _, tool := range []string{"codex", "claude"} {
		t.Run(tool, func(t *testing.T) {
			h := resourceHub(t, "economy")
			h.mu.Lock()
			defer h.mu.Unlock()
			account := h.accountLocked("browser-owner")
			stream := &appSub{deviceID: "browser-pc", ch: make(chan sseMsg, 2), closed: make(chan struct{})}
			other := &appSub{deviceID: "another-pc", ch: make(chan sseMsg, 2), closed: make(chan struct{})}
			account.apps[stream], account.apps[other] = struct{}{}, struct{}{}
			fields := map[string]json.RawMessage{
				"type": json.RawMessage(`"tool"`), "id": json.RawMessage(`"native-open-1"`),
				"status": json.RawMessage(`"done"`), "output": json.RawMessage(`"original output"`),
				"browser": json.RawMessage(`{"action":"navigate","url":"https://example.test/docs?q=one%20two#setup","pageId":"playwright:tab-1"}`),
				"computer": json.RawMessage(`{"version":1,"surface":"browser","action":"navigate","provider":"playwright"}`),
			}
			fields["sessionKey"], _ = json.Marshal(tool + ":owner")
			fields["tool"], _ = json.Marshal(tool)
			stored, err := account.appendEventLocked("browser-pc", fields)
			if err != nil {
				t.Fatal(err)
			}
			assertMetadata := func(raw []byte) {
				t.Helper()
				var got map[string]json.RawMessage
				if json.Unmarshal(raw, &got) != nil {
					t.Fatal("invalid forwarded event")
				}
				for _, key := range []string{"browser", "computer", "sessionKey", "tool", "id", "status", "output"} {
					var before, after any
					if json.Unmarshal(fields[key], &before) != nil || json.Unmarshal(got[key], &after) != nil || !reflect.DeepEqual(before, after) {
						t.Fatalf("Hub lost or changed %s: %s", key, raw)
					}
				}
				if string(got["deviceId"]) != `"browser-pc"` {
					t.Fatal("browser metadata crossed computer scope")
				}
			}
			assertMetadata(stored.data)
			select {
			case live := <-stream.ch:
				if live.event != "event" || live.seq != stored.seq {
					t.Fatal("live event lost its native ordering")
				}
				assertMetadata(live.data)
			default:
				t.Fatal("browser event not delivered to its computer's live stream")
			}
			select {
			case <-other.ch:
				t.Fatal("browser event leaked to another computer's stream")
			default:
			}
			for _, scope := range []string{"", tool + ":owner"} {
				page := h.readEventPageLocked(account.id, "browser-pc", scope, stored.seq-1, 10)
				if len(page.Events) != 1 {
					t.Fatalf("missing browser event in %q replay: %+v", scope, page)
				}
				assertMetadata(page.Events[0])
			}
		})
	}
}
