package hub

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"time"
)

const iosPushDigestWindow = time.Second

type pushDeliveryTarget struct {
	key, account, deviceID string
	entry                  pushEntry
}

type iosPushDigest struct {
	due  time.Time
	jobs []pushJob
}

func deliveryTarget(job pushJob) pushDeliveryTarget {
	return pushDeliveryTarget{key: job.key, account: job.account, deviceID: job.deviceID, entry: job.entry}
}

func iosDigestEligible(job pushJob) bool {
	return job.entry.Platform == "ios" && (job.data["kind"] == "done" || job.data["kind"] == "reply")
}

// Only ordinary iOS results wait for this short window. Android receives the
// original jobs for its native grouping. Requests, failures, and resolutions
// never sit behind the ordinary-result timer.
func (h *Hub) runPush(done <-chan struct{}) {
	p := h.push
	retry := time.NewTicker(5 * time.Second)
	defer retry.Stop()
	timer := time.NewTimer(time.Hour)
	timer.Stop()
	defer timer.Stop()
	var tick <-chan time.Time
	groups := map[pushDeliveryTarget]*iosPushDigest{}
	resetTimer := func() {
		if !timer.Stop() {
			select {
			case <-timer.C:
			default:
			}
		}
		tick = nil
		var due time.Time
		for _, group := range groups {
			if due.IsZero() || group.due.Before(due) {
				due = group.due
			}
		}
		if !due.IsZero() {
			timer.Reset(time.Until(due))
			tick = timer.C
		}
	}
	for {
		select {
		case <-done:
			// Buffered source jobs remain pending in the durable outbox.
			return
		case <-retry.C:
			h.recoverPushJobs()
		case <-tick:
			now := time.Now()
			for target, group := range groups {
				if !group.due.After(now) {
					delete(groups, target)
					h.deliverPushJobs(group.jobs)
				}
			}
			resetTimer()
		case job := <-p.queue:
			if !iosDigestEligible(job) {
				h.deliverPushJobs([]pushJob{job})
				continue
			}
			target := deliveryTarget(job)
			group := groups[target]
			if group == nil {
				group = &iosPushDigest{due: time.Now().Add(iosPushDigestWindow)}
				groups[target] = group
			}
			group.jobs = append(group.jobs, job)
			resetTimer()
		}
	}
}

func (h *Hub) currentDeliveryJobs(jobs []pushJob) []pushJob {
	live := make([]pushJob, 0, len(jobs))
	for _, job := range jobs {
		if h.currentPushJob(job) && pushPayloadUnexpired(job.data, time.Now()) {
			live = append(live, job)
		} else {
			h.finishPush(job, false)
		}
	}
	return live
}

// Each original outbox row owns its completion/retry, not the digest. Rebuild
// after OAuth too: one invalidated task must not discard another valid result.
func (h *Hub) deliverPushJobs(jobs []pushJob) {
	live := h.currentDeliveryJobs(jobs)
	if len(live) == 0 {
		return
	}
	if len(live) > 1 {
		target := deliveryTarget(live[0])
		for _, job := range live {
			if deliveryTarget(job) != target || !iosDigestEligible(job) {
				for _, single := range live {
					h.deliverPushJobs([]pushJob{single})
				}
				return
			}
		}
	}
	if _, _, fits := iosDigestPayload(live); !fits {
		// Do not truncate identity metadata to fit FCM's 4 KiB payload limit.
		// Oversized digests retain every task as an individual notification.
		for _, single := range live {
			h.deliverPushJobs([]pushJob{single})
		}
		return
	}
	entry := live[0].entry
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	err := h.push.sender.sendPrepared(ctx, entry.Token, entry.Platform, func() (map[string]string, string, bool) {
		live = h.currentDeliveryJobs(live)
		if len(live) == 0 {
			return nil, "", false
		}
		data, collapse, _ := iosDigestPayload(live)
		return data, collapse, true
	})
	cancel()
	if errors.Is(err, errPushTokenGone) && len(live) > 0 {
		h.push.dropIfCurrent(live[0])
	} else if err != nil && h.cfg.Logger != nil {
		h.cfg.Logger.Warn("push not delivered", "err", err)
	}
	for _, job := range live {
		h.finishPush(job, err != nil && !errors.Is(err, errPushTokenGone))
	}
}

type iosDigestTask struct {
	ID                string `json:"id"`
	DeviceID          string `json:"deviceId"`
	SessionKey        string `json:"sessionKey"`
	TurnID            string `json:"turnId,omitempty"`
	TurnRef           string `json:"turnRef,omitempty"`
	NotificationScope string `json:"notificationScope,omitempty"`
}

func iosDigestPayload(jobs []pushJob) (map[string]string, string, bool) {
	first := jobs[0]
	if len(jobs) == 1 {
		return first.data, first.collapse, true
	}
	tasks := make([]iosDigestTask, 0, len(jobs))
	seen := map[string]bool{}
	for _, job := range jobs {
		if seen[job.data["id"]] {
			continue
		}
		seen[job.data["id"]] = true
		tasks = append(tasks, iosDigestTask{ID: job.data["id"], DeviceID: job.deviceID, SessionKey: job.data["sessionKey"], TurnID: job.data["turnId"], TurnRef: job.data["turnRef"], NotificationScope: job.entry.NotificationScope})
	}
	if len(tasks) <= 1 {
		return first.data, first.collapse, true
	}
	id := "summary:" + shaHex(first.key+"\x00"+first.entry.PairHash+"\x00"+first.entry.NotificationScope)
	if first.entry.NotificationScope != "" {
		id = first.entry.NotificationScope + ":" + id
	}
	encoded, _ := json.Marshal(tasks)
	count := strconv.Itoa(len(tasks))
	data := map[string]string{"type": "salcara.task", "kind": "done", "agent": "Agent", "id": id,
		"deviceId": first.deviceID, "sessionKey": first.data["sessionKey"],
		"notificationCount": count, "count": count, "taskIds": string(encoded)}
	if first.entry.NotificationScope != "" {
		data["notificationScope"] = first.entry.NotificationScope
	}
	var expiry int64
	for _, job := range jobs {
		if value, err := strconv.ParseInt(job.data["expiresAt"], 10, 64); err == nil && value > 0 && (expiry == 0 || value < expiry) {
			expiry = value
		}
	}
	if expiry > 0 {
		data["expiresAt"] = strconv.FormatInt(expiry, 10)
	}
	raw, _ := json.Marshal(data)
	return data, id, len(raw) <= 3500 // reserve room for APNs/FCM envelope fields
}
