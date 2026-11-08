package hub

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

func featureChangeFields(tool string) map[string]json.RawMessage {
	raw, _ := json.Marshal(tool)
	return map[string]json.RawMessage{"type": json.RawMessage(`"features.changed"`), "tool": raw, "sessionKey": json.RawMessage(`""`)}
}

func featureChangeFixture(t *testing.T) (*Hub, *account) {
	t.Helper()
	h := newDeviceHub(t, Config{})
	h.mu.Lock()
	a := h.accountLocked("feature-owner")
	a.devices["pc"] = &device{info: Device{DeviceID: "pc", Tools: json.RawMessage(`[{"id":"codex"},{"id":"claude"},{"id":"ThirdParty_1.a-b"},{"id":"-invalid"}]`)}}
	h.mu.Unlock()
	return h, a
}

func appendFeatureFixture(t *testing.T, a *account, deviceID string, fields map[string]json.RawMessage) *storedEvent {
	t.Helper()
	event, err := a.appendEventLocked(deviceID, fields)
	if err != nil {
		t.Fatal(err)
	}
	return event
}

func TestFeatureChangesSnapshotSelectsLatestRegisteredPureSignals(t *testing.T) {
	h, a := featureChangeFixture(t)
	h.mu.Lock()
	defer h.mu.Unlock()
	appendFeatureFixture(t, a, "pc", featureChangeFields("codex"))
	claude := appendFeatureFixture(t, a, "pc", featureChangeFields("claude"))
	extension := appendFeatureFixture(t, a, "pc", featureChangeFields("ThirdParty_1.a-b"))
	codex := appendFeatureFixture(t, a, "pc", featureChangeFields("codex"))
	appendFeatureFixture(t, a, "other-device", featureChangeFields("codex"))
	for _, tool := range []string{"unregistered", "codex:session", "-invalid", strings.Repeat("x", 65), ""} {
		appendFeatureFixture(t, a, "pc", featureChangeFields(tool))
	}
	for _, mutate := range []func(map[string]json.RawMessage){
		func(fields map[string]json.RawMessage) { fields["sessionKey"] = json.RawMessage(`"codex:session"`) },
		func(fields map[string]json.RawMessage) { fields["sessionKey"] = json.RawMessage(`null`) },
		func(fields map[string]json.RawMessage) { delete(fields, "sessionKey") },
		func(fields map[string]json.RawMessage) { fields["type"] = json.RawMessage(`"message"`) },
		func(fields map[string]json.RawMessage) {
			fields["text"] = json.RawMessage(`"must not enter the control lane"`)
		},
		func(fields map[string]json.RawMessage) {
			fields["session"] = json.RawMessage(`{"sessionKey":"codex:hidden"}`)
		},
		func(fields map[string]json.RawMessage) { fields["ts"] = json.RawMessage(`{"unexpected":"payload"}`) },
	} {
		fields := featureChangeFields("codex")
		mutate(fields)
		appendFeatureFixture(t, a, "pc", fields)
	}
	want := []featureChange{
		{Type: "features.changed", Tool: "claude", DeviceID: "pc", Seq: claude.seq},
		{Type: "features.changed", Tool: "ThirdParty_1.a-b", DeviceID: "pc", Seq: extension.seq},
		{Type: "features.changed", Tool: "codex", DeviceID: "pc", Seq: codex.seq},
	}
	page := h.readEventPageLocked(a.id, "pc", "codex:missing", a.seq, 1)
	if !reflect.DeepEqual(page.FeatureChanges, want) || len(page.Events) != 0 || !page.ResetRequired || page.NextSeq != a.seq {
		t.Fatalf("invalid control snapshot or changed missing-session cursor: %+v", page)
	}
	for _, change := range page.FeatureChanges {
		raw, err := json.Marshal(change)
		var fields map[string]json.RawMessage
		if err != nil || json.Unmarshal(raw, &fields) != nil || len(fields) != 4 {
			t.Fatalf("control snapshot contains additional payload: %s", raw)
		}
		for _, field := range []string{"type", "tool", "deviceId", "seq"} {
			if fields[field] == nil {
				t.Fatalf("control snapshot missing %s", field)
			}
		}
	}
	if got := h.readEventPageLocked("other-owner", "pc", "codex:missing", 0, 1); len(got.FeatureChanges) != 0 {
		t.Fatal("another account received control state")
	}
	if got := h.readEventPageLocked(a.id, "other-device", "codex:missing", 0, 1); len(got.FeatureChanges) != 0 {
		t.Fatal("unregistered device received control state")
	}
}

func TestFeatureChangesDoNotChangeSessionPaginationOrGapSemantics(t *testing.T) {
	h, a := featureChangeFixture(t)
	h.mu.Lock()
	defer h.mu.Unlock()
	a.resource.SessionEvents = 2
	first := appendFeatureFixture(t, a, "pc", eventFields(1, "codex:s"))
	appendFeatureFixture(t, a, "pc", eventFields(1, "codex:s"))
	third := appendFeatureFixture(t, a, "pc", eventFields(1, "codex:s"))
	last := appendFeatureFixture(t, a, "pc", eventFields(1, "codex:s"))
	signal := appendFeatureFixture(t, a, "pc", featureChangeFields("codex"))
	if len(a.sessions) != 1 || a.sessions[sessionID("pc", "")] != nil || a.sessions[sessionID("pc", "codex:s")].updated != last.seq {
		t.Fatal("runtime signal entered session storage")
	}
	page := h.readEventPageLocked(a.id, "pc", "codex:s", first.seq, 1)
	if len(page.Events) != 1 || page.NextSeq != third.seq || page.LastSeq != last.seq || page.OldestSeq != third.seq || !page.HasMore || !page.ResetRequired ||
		len(page.FeatureChanges) != 1 || page.FeatureChanges[0].Seq != signal.seq {
		t.Fatalf("gap page changed session semantics: %+v", page)
	}
	next := h.readEventPageLocked(a.id, "pc", "codex:s", page.NextSeq, 1)
	if len(next.Events) != 1 || next.NextSeq != last.seq || next.LastSeq != last.seq || next.HasMore || next.ResetRequired || !reflect.DeepEqual(next.FeatureChanges, page.FeatureChanges) {
		t.Fatalf("next page changed session semantics: %+v", next)
	}
	complete := h.readEventPageLocked(a.id, "pc", "codex:s", last.seq, 1)
	if len(complete.Events) != 0 || complete.NextSeq != last.seq || complete.LastSeq != last.seq || complete.ResetRequired || !reflect.DeepEqual(complete.FeatureChanges, page.FeatureChanges) {
		t.Fatalf("control signal advanced the session cursor: %+v", complete)
	}
	a.sessions[sessionID("pc", "codex:nil")] = &sessionBuf{}
	for _, key := range []string{"codex:missing", "codex:nil"} {
		missing := h.readEventPageLocked(a.id, "pc", key, first.seq, 1)
		if !missing.ResetRequired || len(missing.Events) != 0 || !reflect.DeepEqual(missing.FeatureChanges, page.FeatureChanges) {
			t.Fatalf("early return lost the control snapshot for %s: %+v", key, missing)
		}
	}
	future := h.readEventPageLocked(a.id, "pc", "codex:s", signal.seq+100, 1)
	if !future.ResetRequired || future.NextSeq != third.seq || !reflect.DeepEqual(future.FeatureChanges, page.FeatureChanges) {
		t.Fatalf("future-cursor reset lost the control snapshot: %+v", future)
	}
}

func TestFeatureChangesSnapshotLifetimeIsBoundedByAccountRing(t *testing.T) {
	h, a := featureChangeFixture(t)
	h.mu.Lock()
	defer h.mu.Unlock()
	a.resource.AccountEvents = 2
	appendFeatureFixture(t, a, "pc", featureChangeFields("codex"))
	appendFeatureFixture(t, a, "pc", featureChangeFields("claude"))
	if len(a.sessions) != 0 || len(h.readEventPageLocked(a.id, "pc", "codex:missing", 0, 1).FeatureChanges) != 2 {
		t.Fatal("control-only events did not stay in the account ring")
	}
	appendFeatureFixture(t, a, "pc", eventFields(1, "codex:s"))
	appendFeatureFixture(t, a, "pc", eventFields(1, "codex:s"))
	page := h.readEventPageLocked(a.id, "pc", "codex:s", 0, 1)
	if len(page.FeatureChanges) != 0 {
		t.Fatal("evicted control state escaped the bounded account ring")
	}
	raw, err := json.Marshal(page)
	if err != nil || strings.Contains(string(raw), "featureChanges") {
		t.Fatalf("empty optional snapshot was not omitted: %s", raw)
	}
}

func featureChangePhone(t *testing.T) (*Hub, *account, concurrentPhone) {
	t.Helper()
	h := newDeviceHub(t, Config{ResourceMode: "economy"})
	phone := concurrentPhones(t, h, 1)[0]
	h.mu.Lock()
	a := h.accounts[deviceAccount(phone.id)]
	a.devices[phone.id].info.Tools = json.RawMessage(`[{"id":"codex"},{"id":"claude"}]`)
	h.mu.Unlock()
	return h, a, phone
}

func TestFeatureChangesLongPollIgnoresOldSnapshotAndOtherDeviceSignals(t *testing.T) {
	h, a, phone := featureChangePhone(t)
	h.mu.Lock()
	initial := appendFeatureFixture(t, a, phone.id, featureChangeFields("codex"))
	h.mu.Unlock()
	read := holdPhoneRead(t, h, phone, emptyWaitPath(phone, 25))
	awaitReadState(t, h, func() bool { return a.eventWaiters == 1 && a.wake != nil })
	for _, operation := range []string{"same-snapshot", "other-device", "unregistered-tool", "mixed-payload"} {
		h.mu.Lock()
		previousWake := a.wake
		switch operation {
		case "same-snapshot":
			a.wakeEventWaitersLocked()
		case "other-device":
			appendFeatureFixture(t, a, "another-pc", featureChangeFields("codex"))
		case "unregistered-tool":
			appendFeatureFixture(t, a, phone.id, featureChangeFields("unregistered"))
		case "mixed-payload":
			fields := featureChangeFields("codex")
			fields["text"] = json.RawMessage(`"not a pure control signal"`)
			appendFeatureFixture(t, a, phone.id, fields)
		}
		h.mu.Unlock()
		awaitReadState(t, h, func() bool { return a.eventWaiters == 1 && a.wake != nil && a.wake != previousWake })
		select {
		case <-read.done:
			t.Fatalf("%s incorrectly completed the long poll", operation)
		default:
		}
	}
	h.mu.Lock()
	changed := appendFeatureFixture(t, a, phone.id, featureChangeFields("claude"))
	h.mu.Unlock()
	w := awaitPhoneRead(t, read)
	var page eventPage
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Events) != 0 || page.NextSeq != 0 || page.ResetRequired ||
		!reflect.DeepEqual(page.FeatureChanges, []featureChange{{Type: "features.changed", Tool: "codex", DeviceID: phone.id, Seq: initial.seq}, {Type: "features.changed", Tool: "claude", DeviceID: phone.id, Seq: changed.seq}}) {
		t.Fatalf("new runtime signal failed to wake a pure snapshot: %d %s", w.Code, w.Body)
	}
	// The unchanged session cursor is intentional. A second request receives
	// these same hints at timeout, without immediately completing in a loop.
	start := time.Now()
	w = concurrentRequest(t, h, phone, emptyWaitPath(phone, 1), false, nil)
	if w.Code != http.StatusOK || time.Since(start) < 900*time.Millisecond || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.FeatureChanges) != 2 {
		t.Fatalf("existing control snapshot spun the next long poll: %d after %v", w.Code, time.Since(start))
	}
}

func TestFeatureChangesHTTPBatchRetryAndSessionIsolation(t *testing.T) {
	h, a, phone := featureChangePhone(t)
	batch := map[string]any{"deviceId": phone.id, "batchId": "20000000-0000-4000-8000-000000000001", "events": []any{
		map[string]any{"type": "features.changed", "tool": "codex", "sessionKey": "", "ts": 123},
	}}
	first := concurrentRequest(t, h, phone, "/bridge/events", true, batch)
	if first.Code != http.StatusOK {
		t.Fatalf("control upload failed: %d %s", first.Code, first.Body)
	}
	read := holdPhoneRead(t, h, phone, emptyWaitPath(phone, 25))
	awaitReadState(t, h, func() bool { return a.eventWaiters == 1 && a.wake != nil })
	h.mu.Lock()
	before, wake := a.seq, a.wake
	h.mu.Unlock()
	duplicate := concurrentRequest(t, h, phone, "/bridge/events", true, batch)
	if duplicate.Code != http.StatusOK || !strings.Contains(duplicate.Body.String(), `"duplicate":true`) {
		t.Fatalf("control batch retry was not deduplicated: %d %s", duplicate.Code, duplicate.Body)
	}
	h.mu.Lock()
	unchanged := a.seq == before && a.wake == wake && len(a.sessions) == 0 && a.eventWaiters == 1
	h.mu.Unlock()
	if !unchanged {
		t.Fatal("duplicate upload changed the snapshot, woke readers, or created a session")
	}
	batch["batchId"] = "20000000-0000-4000-8000-000000000002"
	if w := concurrentRequest(t, h, phone, "/bridge/events", true, batch); w.Code != http.StatusOK {
		t.Fatalf("second signal upload failed: %d %s", w.Code, w.Body)
	}
	w := awaitPhoneRead(t, read)
	var page eventPage
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &page) != nil || len(page.Events) != 0 || len(page.FeatureChanges) != 1 || page.FeatureChanges[0].Seq <= before {
		t.Fatalf("new batch did not replace the tool snapshot: %d %s", w.Code, w.Body)
	}
	reset := concurrentRequest(t, h, phone, fmt.Sprintf("/app/events?deviceId=%s&sessionKey=codex:missing&after=%d&wait=25", phone.id, before), false, nil)
	if reset.Code != http.StatusOK || json.Unmarshal(reset.Body.Bytes(), &page) != nil || !page.ResetRequired || len(page.FeatureChanges) != 1 {
		t.Fatalf("reset response omitted control state: %d %s", reset.Code, reset.Body)
	}
}

func TestFeatureChangesConcurrentReadsAndSignalsStayDetached(t *testing.T) {
	h, a := featureChangeFixture(t)
	var workers sync.WaitGroup
	for _, tool := range []string{"codex", "claude", "ThirdParty_1.a-b"} {
		workers.Go(func() {
			for range 20 {
				h.mu.Lock()
				_, err := a.appendEventLocked("pc", featureChangeFields(tool))
				h.mu.Unlock()
				if err != nil {
					t.Error(err)
					return
				}
			}
		})
	}
	workers.Go(func() {
		for range 20 {
			h.mu.Lock()
			page := h.readEventPageLocked(a.id, "pc", "codex:missing", 0, 1)
			h.mu.Unlock()
			for i := range page.FeatureChanges {
				page.FeatureChanges[i].Tool = "caller mutation"
			}
		}
	})
	workers.Wait()
	h.mu.Lock()
	defer h.mu.Unlock()
	page := h.readEventPageLocked(a.id, "pc", "codex:missing", 0, 1)
	if len(page.FeatureChanges) != 3 || len(a.sessions) != 0 {
		t.Fatalf("concurrent snapshot lost a runtime or created a session: %+v", page)
	}
	for _, change := range page.FeatureChanges {
		if change.Tool == "caller mutation" || change.DeviceID != "pc" {
			t.Fatal("caller mutation entered the stored control snapshot")
		}
	}
}

func TestFeatureChangesRestartKeepsPairedSequenceMonotonic(t *testing.T) {
	cfg := Config{DataDir: t.TempDir(), ResourceMode: "economy"}
	first := newDeviceHub(t, cfg)
	phone := concurrentPhones(t, first, 1)[0]
	first.mu.Lock()
	first.accounts[deviceAccount(phone.id)].devices[phone.id].info.Tools = json.RawMessage(`[{"id":"codex"}]`)
	first.mu.Unlock()
	batch := map[string]any{"deviceId": phone.id, "events": []any{
		map[string]any{"type": "features.changed", "tool": "codex", "sessionKey": ""},
	}}
	if w := concurrentRequest(t, first, phone, "/bridge/events", true, batch); w.Code != http.StatusOK {
		t.Fatalf("first control upload failed: %d %s", w.Code, w.Body)
	}
	path := fmt.Sprintf("/app/events?deviceId=%s&sessionKey=codex:missing&after=0", phone.id)
	w := concurrentRequest(t, first, phone, path, false, nil)
	var before eventPage
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &before) != nil || len(before.FeatureChanges) != 1 {
		t.Fatalf("first paired snapshot failed: %d %s", w.Code, w.Body)
	}
	if err := first.saveNowError(); err != nil {
		t.Fatal(err)
	}
	first.Close()
	second := newDeviceHub(t, cfg)
	if w := concurrentRequest(t, second, phone, "/bridge/events", true, batch); w.Code != http.StatusOK {
		t.Fatalf("restarted control upload failed: %d %s", w.Code, w.Body)
	}
	w = concurrentRequest(t, second, phone, path, false, nil)
	var after eventPage
	if w.Code != http.StatusOK || json.Unmarshal(w.Body.Bytes(), &after) != nil || len(after.FeatureChanges) != 1 || after.FeatureChanges[0].Seq <= before.FeatureChanges[0].Seq {
		t.Fatalf("same-pair restart reused a feature sequence: status=%d before=%+v after=%s", w.Code, before.FeatureChanges, w.Body)
	}
}
