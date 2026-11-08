package hub

import (
	"encoding/json"
	"time"
)

// Generic wake-up metadata only. Push is a hint to fetch the durable mailbox,
// never the transport for an answer or permission to replay an operation.
type durablePushJob struct {
	Key      string            `json:"key"`
	Account  string            `json:"account"`
	DeviceID string            `json:"deviceId"`
	Entry    pushEntry         `json:"entry"`
	Collapse string            `json:"collapse"`
	Data     map[string]string `json:"data"`
}

func (s *durableStore) savePush(job *pushJob, dedupe string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	job.durableID = shaHex(dedupe)
	raw, err := json.Marshal(durablePushJob{Key: job.key, Account: job.account, DeviceID: job.deviceID, Entry: job.entry, Collapse: job.collapse, Data: job.data})
	if err != nil {
		return false, err
	}
	raw, err = s.seal(raw, "push:"+job.durableID)
	if err != nil {
		return false, err
	}
	var count int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM push_jobs WHERE state='pending'`).Scan(&count); err != nil {
		return false, err
	}
	if count >= 4096 {
		return false, errStorageCapacity
	}
	r, err := s.db.Exec(`INSERT INTO push_jobs(id,body,state,due,expires,attempts) VALUES(?,?,'pending',?,?,0) ON CONFLICT(id) DO NOTHING`, job.durableID, raw, nowMs(), nowMs()+int64(time.Hour/time.Millisecond))
	if err != nil {
		return false, err
	}
	n, _ := r.RowsAffected()
	return n == 1, nil
}
func (h *Hub) recoverPushJobs() {
	s := h.durable
	if s == nil || !h.push.enabled() {
		return
	}
	rows, err := s.db.Query(`SELECT id,body FROM push_jobs WHERE state='pending' AND due<=? AND expires>? ORDER BY due LIMIT 32`, nowMs(), nowMs())
	if err != nil {
		return
	}
	type saved struct {
		id   string
		body []byte
	}
	var jobs []saved
	for rows.Next() {
		var j saved
		if rows.Scan(&j.id, &j.body) == nil {
			jobs = append(jobs, j)
		}
	}
	rows.Close()
	for _, saved := range jobs {
		raw, err := s.open(saved.body, "push:"+saved.id)
		if err != nil {
			continue
		}
		var disk durablePushJob
		if json.Unmarshal(raw, &disk) != nil {
			continue
		}
		// Process-local revision is not persisted. Rebind it only after matching
		// every stable registration field, never move a job to a new token.
		p := h.push
		p.mu.Lock()
		current, ok := p.entries[disk.Key]
		stored := disk.Entry
		stored.revision = current.revision
		valid := ok && stored == current
		p.mu.Unlock()
		if !valid {
			_, _ = s.db.Exec(`UPDATE push_jobs SET state='done',body=NULL WHERE id=?`, saved.id)
			continue
		}
		job := pushJob{key: disk.Key, account: disk.Account, deviceID: disk.DeviceID, entry: current, collapse: disk.Collapse, data: disk.Data, durableID: saved.id}
		// Reserve the retry slot before queueing; restart may safely resend this
		// generic idempotent notification using its stable notification id.
		_, err = s.db.Exec(`UPDATE push_jobs SET due=? WHERE id=? AND state='pending'`, nowMs()+int64(time.Minute/time.Millisecond), saved.id)
		if err != nil {
			continue
		}
		select {
		case p.queue <- job:
		default:
			return
		}
	}
}
func (h *Hub) finishPush(job pushJob, retry bool) {
	s := h.durable
	if s == nil || job.durableID == "" {
		return
	}
	if !retry {
		_, _ = s.db.Exec(`UPDATE push_jobs SET state='done',body=NULL WHERE id=?`, job.durableID)
		return
	}
	_, _ = s.db.Exec(`UPDATE push_jobs SET attempts=attempts+1,due=? WHERE id=? AND state='pending'`, nowMs()+int64(time.Minute/time.Millisecond), job.durableID)
}
