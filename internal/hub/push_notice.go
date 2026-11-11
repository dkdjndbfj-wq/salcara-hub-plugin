package hub

// Only authenticated event metadata is retained here. This is presentation
// state, never authority to approve a request or execute a command.
import (
	"database/sql"
	"encoding/json"
	"errors"
	"sort"
	"time"
)

const pushLegacyWindow = 60 * time.Second

type pushNoticeRecord struct {
	Kind      string `json:"kind"`
	Seq       int64  `json:"seq"`
	At        int64  `json:"at"`
	ExpiresAt int64  `json:"expiresAt,omitempty"`
}
type pushLegacyTurn struct {
	Ref     string          `json:"ref"`
	At      int64           `json:"at"`
	Started bool            `json:"started"`
	Marker  string          `json:"marker,omitempty"`
	Kinds   map[string]bool `json:"kinds"`
}
type pushNoticeSession struct {
	Updated   int64                         `json:"updated"`
	Legacy    pushLegacyTurn                `json:"legacy"`
	Turns     map[string]pushNoticeRecord   `json:"turns"`
	Requests  map[string]pushNoticeRecord   `json:"requests"`
	Seen      map[string]int64              `json:"seen,omitempty"`
	Decisions map[string]pushNoticeDecision `json:"decisions,omitempty"`
}

type pushNoticeDecision struct {
	Kind string `json:"kind"`
	Ref  string `json:"ref"`
}

func newNoticeSession() *pushNoticeSession {
	return &pushNoticeSession{Turns: map[string]pushNoticeRecord{}, Requests: map[string]pushNoticeRecord{}, Seen: map[string]int64{}, Decisions: map[string]pushNoticeDecision{}}
}

func relevantPushEvent(m map[string]json.RawMessage) bool {
	return pushEventKind(m) != "" || eventString(m, "type") == "turn" && eventString(m, "status") == "started" && eventString(m, "parentId") == ""
}

func trimNoticeMap[T any](entries map[string]T, at func(T) int64, limit int) {
	if len(entries) <= limit {
		return
	}
	keys := make([]string, 0, len(entries))
	for key := range entries {
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool { return at(entries[keys[i]]) < at(entries[keys[j]]) })
	for _, key := range keys[:len(keys)-limit] {
		delete(entries, key)
	}
}

func eventMillis(m map[string]json.RawMessage, key string) int64 {
	var n int64
	_ = json.Unmarshal(m[key], &n)
	return n
}

func pushEventKind(m map[string]json.RawMessage) string {
	switch eventString(m, "type") {
	case "approval.resolved":
		return "resolved"
	case "approval.request":
		if eventString(m, "kind") == "question" {
			return "question"
		}
		return "approval"
	case "turn":
		if eventString(m, "parentId") != "" {
			return ""
		}
		switch eventString(m, "status") {
		case "completed":
			return "done"
		case "failed":
			return "failed"
		}
	case "message":
		if eventString(m, "role") == "assistant" && eventBool(m, "final") && eventString(m, "parentId") == "" && eventString(m, "messagePhase") != "commentary" {
			return "reply"
		}
	}
	return ""
}

// h.mu and p.mu are held. On restart read the encrypted, bounded metadata row
// rather than treating an empty RAM ring as proof that an approval is pending.
func (h *Hub) pushNoticeLocked(key string) *pushNoticeSession {
	if state := h.push.notices[key]; state != nil {
		return state
	}
	state := newNoticeSession()
	if h.durable != nil {
		var raw []byte
		if h.durable.db.QueryRow(`SELECT body FROM push_notices WHERE id=? AND expires>?`, key, nowMs()).Scan(&raw) == nil {
			if plain, err := h.durable.open(raw, "push-notice:"+key); err == nil {
				_ = json.Unmarshal(plain, state)
			}
		}
	}
	if state.Turns == nil {
		state.Turns = map[string]pushNoticeRecord{}
	}
	if state.Requests == nil {
		state.Requests = map[string]pushNoticeRecord{}
	}
	if h.durable != nil && len(h.push.notices) >= 32 {
		var oldest string
		var oldestAt int64 = 1<<63 - 1
		for k, s := range h.push.notices {
			if s.Updated < oldestAt {
				oldest, oldestAt = k, s.Updated
			}
		}
		delete(h.push.notices, oldest) // encrypted row, if enabled, is still the authority
	}
	h.push.notices[key] = state
	return state
}

// observe returns a presentation candidate; it never considers subscriber
// tokens. A single source event therefore has the same identity for every phone.
func (s *pushNoticeSession) observe(m map[string]json.RawMessage, seq, now int64) (kind, ref string) {
	if s.Seen == nil {
		s.Seen = map[string]int64{}
	}
	if s.Decisions == nil {
		s.Decisions = map[string]pushNoticeDecision{}
	}
	defer func() {
		if kind != "" {
			s.Decisions[seqText(seq)] = pushNoticeDecision{Kind: kind, Ref: ref}
		}
		trimNoticeMap(s.Seen, func(at int64) int64 { return at }, 2048)
	}()
	s.Updated = now
	for k, at := range s.Seen {
		if now-at > durableDay {
			delete(s.Seen, k)
		}
	}
	for k, row := range s.Turns {
		if now-row.At > durableDay {
			delete(s.Turns, k)
		}
	}
	for k, row := range s.Requests {
		if now-row.At > durableDay && row.ExpiresAt <= now {
			delete(s.Requests, k)
		}
	}
	if eventString(m, "type") == "turn" && eventString(m, "status") == "started" && eventString(m, "parentId") == "" {
		marker := seqText(seq)
		if s.Legacy.Marker != marker {
			s.Legacy = pushLegacyTurn{Ref: "legacy:" + marker, At: now, Started: true, Marker: marker, Kinds: map[string]bool{}}
		}
		return "", ""
	}
	kind = pushEventKind(m)
	if kind == "" {
		return "", ""
	}
	if eventString(m, "type") == "message" {
		if id := eventString(m, "id"); id != "" && len(id) <= 256 {
			key := shaHex(eventString(m, "turnId") + "\x00" + id)
			if _, seen := s.Seen[key]; seen {
				return "", ""
			}
			s.Seen[key] = now
		}
	}
	if kind == "approval" || kind == "question" || kind == "resolved" {
		ref = eventString(m, "approvalId")
		if ref == "" || len(ref) > 256 {
			return "", ""
		}
		prior, exists := s.Requests[ref]
		if exists && prior.Kind == "resolved" {
			return "", ""
		}
		expiry := eventMillis(m, "expiresAt")
		if raw, present := m["expiresAt"]; present && (json.Unmarshal(raw, &expiry) != nil || expiry < 0 || expiry > 9007199254740991) {
			return "", ""
		}
		if kind != "resolved" && expiry > 0 && expiry <= now {
			return "", ""
		}
		if exists && kind != "resolved" {
			return "", ""
		}
		s.Requests[ref] = pushNoticeRecord{Kind: kind, Seq: seq, At: now, ExpiresAt: expiry}
		return kind, ref
	}
	ref = eventString(m, "turnId")
	if len(ref) > 256 {
		return "", ""
	}
	if ref == "" {
		legacy := &s.Legacy
		if legacy.Ref == "" || !legacy.Started && (now-legacy.At > pushLegacyWindow.Milliseconds() || legacy.Kinds[kind]) {
			*legacy = pushLegacyTurn{Ref: "legacy:" + seqText(seq), At: now, Kinds: map[string]bool{}}
		}
		if legacy.Kinds == nil {
			legacy.Kinds = map[string]bool{}
		}
		legacy.Kinds[kind], legacy.At = true, now
		// Terminal observation ends the start boundary; only a bounded
		// complementary result can still join it, not all future no-start turns.
		legacy.Started = false
		ref = legacy.Ref
	}
	previous, exists := s.Turns[ref]
	if exists && (previous.Kind == "failed" || kind != "failed") {
		return "", ""
	}
	s.Turns[ref] = pushNoticeRecord{Kind: kind, Seq: seq, At: now}
	return kind, ref
}

func (s *pushNoticeSession) permits(job pushJob, now int64) bool {
	kind := job.data["kind"]
	if kind == "approval" || kind == "question" || kind == "resolved" {
		r, ok := s.Requests[job.noticeRef]
		return ok && r.Kind == kind && (kind == "resolved" || r.ExpiresAt <= 0 || r.ExpiresAt > now)
	}
	r, ok := s.Turns[job.noticeRef]
	return ok && r.Kind == kind
}

// Commit the projection with the authenticated event. A resolved event cannot
// be acknowledged while only the old pending record remains durable.
func (s *durableStore) projectPushNotice(tx *sql.Tx, scope string, e *storedEvent, fields map[string]json.RawMessage, first bool) error {
	if e.sessionKey == "" || len(e.sessionKey) > 512 || !relevantPushEvent(fields) {
		return nil
	}
	key := shaHex(scope + "\x00" + e.sessionKey)
	state := newNoticeSession()
	var saved []byte
	err := tx.QueryRow(`SELECT body FROM push_notices WHERE id=? AND expires>?`, key, nowMs()).Scan(&saved)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	if err == nil {
		plain, err := s.open(saved, "push-notice:"+key)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(plain, state); err != nil {
			return err
		}
	}
	// Metadata is bounded independently from user-visible message bodies. At
	// capacity reject the batch explicitly rather than acknowledge lost alerts.
	var count int
	if err = tx.QueryRow(`SELECT COUNT(*) FROM push_notices WHERE expires>?`, nowMs()).Scan(&count); err != nil {
		return err
	}
	if len(saved) == 0 && count >= 4096 {
		return errStorageCapacity
	}
	// Decisions belong to this admitted batch. Clearing per session, not by
	// sequence distance, preserves every notification in a large atomic batch.
	if first {
		state.Decisions = map[string]pushNoticeDecision{}
	}
	state.observe(fields, e.seq, nowMs())
	raw, err := json.Marshal(state)
	if err != nil {
		return err
	}
	if len(raw) > 512<<10 {
		return errStorageCapacity
	}
	raw, err = s.seal(raw, "push-notice:"+key)
	if err != nil {
		return err
	}
	expires := nowMs() + durableDay
	for _, request := range state.Requests {
		if request.ExpiresAt > expires {
			expires = request.ExpiresAt
		}
	}
	_, err = tx.Exec(`INSERT INTO push_notices(id,body,expires) VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body,expires=excluded.expires`, key, raw, expires)
	return err
}
