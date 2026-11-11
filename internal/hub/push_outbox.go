package hub

import (
	"database/sql"
	"encoding/json"
)

// Called under h.mu, in the same SQL transaction as events and presentation
// state. Crash after HTTP acknowledgement cannot lose the not-yet-enqueued job.
func (h *Hub) stagePushBatchLocked(tx *sql.Tx, acct string, a *account, dev *device, events []*storedEvent) ([]pushJob, error) {
	if !h.push.enabled() {
		return nil, nil
	}
	h.push.mu.Lock()
	defer h.push.mu.Unlock()
	var jobs []pushJob
	scope := h.durableScopeLocked(acct, dev)
	for _, e := range events {
		var fields map[string]json.RawMessage
		if json.Unmarshal(e.data, &fields) != nil || e.sessionKey == "" || !relevantPushEvent(fields) {
			continue
		}
		key := shaHex(scope + "\x00" + e.sessionKey)
		var raw []byte
		if err := tx.QueryRow(`SELECT body FROM push_notices WHERE id=?`, key).Scan(&raw); err != nil {
			return nil, err
		}
		plain, err := h.durable.open(raw, "push-notice:"+key)
		if err != nil {
			return nil, err
		}
		state := newNoticeSession()
		if err = json.Unmarshal(plain, state); err != nil {
			return nil, err
		}
		decision := state.Decisions[seqText(e.seq)]
		for _, job := range h.pushJobsLocked(acct, a, dev, fields, e.seq, key, decision.Kind, decision.Ref) {
			if !state.permits(job, nowMs()) {
				continue
			}
			inserted, err := h.durable.savePushUsing(tx, &job, job.dedupe)
			if err != nil {
				return nil, err
			}
			if inserted {
				jobs = append(jobs, job)
			}
		}
		// Invalidate only after commit at the caller; no staged state is public.
	}
	return jobs, nil
}

func (h *Hub) invalidatePushBatchLocked(acct string, dev *device, events []*storedEvent) {
	h.push.mu.Lock()
	defer h.push.mu.Unlock()
	scope := h.durableScopeLocked(acct, dev)
	for _, e := range events {
		delete(h.push.notices, shaHex(scope+"\x00"+e.sessionKey))
	}
}
