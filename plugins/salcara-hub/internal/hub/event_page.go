package hub

import (
	"encoding/json"
	"net/http"
	"strconv"
)

const maxEventPageBytes = 2 << 20

type eventPage struct {
	Events        []json.RawMessage `json:"events"`
	NextSeq       int64             `json:"nextSeq"`
	LastSeq       int64             `json:"lastSeq"`
	OldestSeq     int64             `json:"oldestSeq"`
	HasMore       bool              `json:"hasMore"`
	ResetRequired bool              `json:"resetRequired"`
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
