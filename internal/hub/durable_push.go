package hub

import (
	"database/sql"
	"encoding/json"
	"strconv"
	"time"
)

// Generic wake-up metadata only. Push is a hint to fetch the durable mailbox,
// never the transport for an answer or permission to replay an operation.
type durablePushJob struct {
	NoticeKey string            `json:"noticeKey,omitempty"`
	NoticeRef string            `json:"noticeRef,omitempty"`
	Key       string            `json:"key"`
	Account   string            `json:"account"`
	DeviceID  string            `json:"deviceId"`
	Entry     pushEntry         `json:"entry"`
	Collapse  string            `json:"collapse"`
	Data      map[string]string `json:"data"`
}

func (s *durableStore) savePush(job *pushJob, dedupe string) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.savePushUsing(s.db, job, dedupe)
}

type pushSQL interface {
	Exec(string, ...any) (sql.Result, error)
	QueryRow(string, ...any) *sql.Row
}

func (s *durableStore) savePushUsing(db pushSQL, job *pushJob, dedupe string) (bool, error) {
	job.durableID = shaHex(dedupe)
	raw, err := json.Marshal(durablePushJob{Key: job.key, Account: job.account, DeviceID: job.deviceID, Entry: job.entry, Collapse: job.collapse, Data: job.data, NoticeKey: job.noticeKey, NoticeRef: job.noticeRef})
	if err != nil {
		return false, err
	}
	raw, err = s.seal(raw, "push:"+job.durableID)
	if err != nil {
		return false, err
	}
	var count int
	if err = db.QueryRow(`SELECT COUNT(*) FROM push_jobs WHERE state='pending'`).Scan(&count); err != nil {
		return false, err
	}
	if count >= 4096 {
		return false, errStorageCapacity
	}
	expires := nowMs() + durableDay
	if job.data["kind"] == "approval" || job.data["kind"] == "question" {
		if declared, err := strconv.ParseInt(job.data["expiresAt"], 10, 64); err == nil && declared > 0 {
			expires = declared
		}
	}
	r, err := db.Exec(`INSERT INTO push_jobs(id,body,state,due,expires,attempts) VALUES(?,?,'pending',?,?,0) ON CONFLICT(id) DO NOTHING`, job.durableID, raw, nowMs(), expires)
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
		job := pushJob{key: disk.Key, account: disk.Account, deviceID: disk.DeviceID, entry: current, collapse: disk.Collapse, data: disk.Data, durableID: saved.id, noticeKey: disk.NoticeKey, noticeRef: disk.NoticeRef}
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
