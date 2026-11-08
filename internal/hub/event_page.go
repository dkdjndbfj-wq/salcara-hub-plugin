package hub

import (
	"bytes"
	"encoding/json"
	"net/http"
	"regexp"
	"strconv"
)

const maxEventPageBytes = 2 << 20

type eventPage struct {
	StorageEpoch   string            `json:"storageEpoch,omitempty"`
	Events         []json.RawMessage `json:"events"`
	FeatureChanges []featureChange   `json:"featureChanges,omitempty"`
	NextSeq        int64             `json:"nextSeq"`
	LastSeq        int64             `json:"lastSeq"`
	OldestSeq      int64             `json:"oldestSeq"`
	HasMore        bool              `json:"hasMore"`
	ResetRequired  bool              `json:"resetRequired"`
}

// This is a device/runtime invalidation snapshot, independent of the requested
// session's event cursor. Never forward arbitrary event payloads in this lane.
type featureChange struct {
	Type     string `json:"type"`
	Tool     string `json:"tool"`
	DeviceID string `json:"deviceId"`
	Seq      int64  `json:"seq"`
}

// Keep this aligned with the Bridge adapter registry's identifier contract.
var featureAgentID = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)

func (h *Hub) featureChangesLocked(a *account, deviceID string) []featureChange {
	if a == nil || a.events == nil || a.devices[deviceID] == nil {
		return nil
	}
	var tools []struct {
		ID string `json:"id"`
	}
	if json.Unmarshal(a.devices[deviceID].info.Tools, &tools) != nil {
		return nil
	}
	registered := make(map[string]bool, len(tools))
	for _, tool := range tools {
		if featureAgentID.MatchString(tool.ID) {
			registered[tool.ID] = true
		}
	}
	var changes []featureChange
	seen := make(map[string]bool)
	// The existing account ring bounds both the scan and snapshot lifetime.
	// Walk newest first so an older signal cannot replace the latest per tool.
	for i := a.events.n - 1; i >= 0; i-- {
		event := a.events.buf[(a.events.start+i)%len(a.events.buf)]
		if event.deviceID != deviceID || event.sessionKey != "" {
			continue
		}
		var signal struct {
			Type       string  `json:"type"`
			Tool       string  `json:"tool"`
			SessionKey *string `json:"sessionKey"`
			DeviceID   string  `json:"deviceId"`
			Seq        int64   `json:"seq"`
			TS         *int64  `json:"ts"`
		}
		decoder := json.NewDecoder(bytes.NewReader(event.data))
		decoder.DisallowUnknownFields()
		if decoder.Decode(&signal) != nil || signal.Type != "features.changed" || signal.SessionKey == nil || *signal.SessionKey != "" ||
			signal.DeviceID != deviceID || signal.Seq != event.seq || signal.Seq <= 0 || !registered[signal.Tool] || seen[signal.Tool] {
			continue
		}
		seen[signal.Tool] = true
		changes = append(changes, featureChange{Type: "features.changed", Tool: signal.Tool, DeviceID: deviceID, Seq: event.seq})
	}
	// Stable sequence order lets callers consume the snapshot without deriving
	// anything from map iteration; it does not advance the session cursor.
	for left, right := 0, len(changes)-1; left < right; left, right = left+1, right-1 {
		changes[left], changes[right] = changes[right], changes[left]
	}
	return changes
}

func featureChangesAfter(changes []featureChange, after int64) bool {
	for _, change := range changes {
		if change.Seq > after {
			return true
		}
	}
	return false
}

func eventPageLimit(r *http.Request) (int, bool) {
	raw := r.URL.Query().Get("limit")
	if raw == "" {
		return 500, true
	}
	n, err := strconv.Atoi(raw)
	return n, err == nil && n >= 1 && n <= 500
}

// readEventPageLocked is bounded by count and bytes; callers authenticate first.
// A gap asks the phone to load the original authoritative session, not pretend
// that a truncated event buffer is a complete transcript.
func (h *Hub) readEventPageLocked(acct, deviceID, sessionKey string, after int64, limit int) eventPage {
	out := eventPage{Events: []json.RawMessage{}, NextSeq: after}
	a := h.accounts[acct]
	if a == nil {
		out.ResetRequired = after > 0
		return out
	}
	out.FeatureChanges = h.featureChangesLocked(a, deviceID)
	if h.durable != nil && a.devices[deviceID] != nil {
		page, err := h.durable.eventPage(h.durableScopeLocked(acct, a.devices[deviceID]), sessionKey, after, limit)
		if err == nil {
			page.FeatureChanges = out.FeatureChanges
			return page
		}
		out.ResetRequired = true
		out.StorageEpoch = h.durable.epoch
		return out
	}
	out.LastSeq = a.seq
	source := a.events
	if sessionKey != "" {
		sb := a.sessions[sessionID(deviceID, sessionKey)]
		if sb == nil {
			out.ResetRequired = after > 0
			return out
		}
		source = sb.ring
		out.LastSeq = sb.updated
	}
	if source == nil {
		out.ResetRequired = after > 0
		return out
	}
	out.ResetRequired = after > 0 && (after < source.evictedThrough || after > a.seq)
	if sessionKey != "" && after > 0 && after < a.sessionEvictedThrough {
		out.ResetRequired = true
	}
	effectiveAfter := after
	if after > a.seq {
		effectiveAfter = 0
		out.NextSeq = 0
	}
	bytes := 0
	for i := 0; i < source.n; i++ {
		event := source.buf[(source.start+i)%len(source.buf)]
		if event.deviceID != deviceID || (sessionKey != "" && event.sessionKey != sessionKey) {
			continue
		}
		if out.OldestSeq == 0 {
			out.OldestSeq = event.seq
		}
		if event.seq <= effectiveAfter {
			continue
		}
		if len(out.Events) >= limit || (len(out.Events) > 0 && bytes+len(event.data) > maxEventPageBytes) {
			out.HasMore = true
			break
		}
		out.Events = append(out.Events, json.RawMessage(event.data))
		out.NextSeq = event.seq
		bytes += len(event.data)
	}
	return out
}
