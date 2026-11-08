package hub

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

/*
Push notifications for the paired phone (Firebase Cloud Messaging).

The phone registers its FCM token through /app/push/register with its pair
token. The Hub keeps it next to that pairing (hash only binds it; a new
pairing or an unpair makes it stale automatically) in push-tokens.json, apart
from devices.json, so older Hubs can still read their own state.

When the computer reports that a turn completed or failed, or a final assistant
reply, the Hub sends one generic push. It carries no task text: only the kind,
agent and ids needed to open the thread. Android writes its own notification;
iOS receives a generic APNs alert through FCM. The receiving app suppresses
system banners while foregrounded. Approvals and questions stay in the
conversation; retries are deduped.
*/

const (
	pushTokensFile   = "push-tokens.json"
	maxPushToken     = 4096
	pushDedupeWindow = 10 * time.Minute
	pushQueueSize    = 256
)

type pushEntry struct {
	PairHash          string `json:"pairHash"`
	Token             string `json:"token"`
	Platform          string `json:"platform"`
	NotificationScope string `json:"notificationScope,omitempty"`
	UpdatedAt         int64  `json:"updatedAt"`
	revision          uint64 // process-local; queued jobs never survive a restart
}

type pushJob struct {
	durableID string
	key       string
	account   string
	deviceID  string
	entry     pushEntry
	collapse  string
	data      map[string]string
}

type turnNotice struct {
	at int64
}

type pushState struct {
	sender *fcmSender
	path   string
	logger *slog.Logger

	mu          sync.Mutex
	entries     map[string]pushEntry  // pairKey(account, device)
	recent      map[string]int64      // dedupe key -> unix ms
	turnNotices map[string]turnNotice // completion and final reply share one notice per turn
	queue       chan pushJob
	revision    uint64
}

func newPushState(cfg *Config) (*pushState, error) {
	p := &pushState{entries: map[string]pushEntry{}, recent: map[string]int64{}, turnNotices: map[string]turnNotice{}, queue: make(chan pushJob, pushQueueSize), logger: cfg.Logger}
	if len(cfg.FCMCredentials) > 0 {
		sender, err := newFCMSender(cfg.FCMCredentials, cfg.HTTPClient)
		if err != nil {
			return nil, err
		}
		p.sender = sender
	}
	if cfg.DataDir != "" {
		p.path = filepath.Join(cfg.DataDir, pushTokensFile)
		raw, err := os.ReadFile(p.path)
		switch {
		case errors.Is(err, os.ErrNotExist):
		case err != nil:
			return nil, err
		default:
			var saved struct {
				Version int                  `json:"version"`
				Entries map[string]pushEntry `json:"entries"`
			}
			// A damaged file only costs the phones one re-registration.
			if json.Unmarshal(raw, &saved) == nil && saved.Version == 1 {
				for k, e := range saved.Entries {
					if validPushToken(e.Token) && validPairHash(e.PairHash) && (e.NotificationScope == "" || validNotificationScope(e.NotificationScope)) {
						p.revision++
						e.revision = p.revision
						p.entries[k] = e
					}
				}
			}
		}
	}
	return p, nil
}

func (p *pushState) enabled() bool { return p != nil && p.sender != nil }

func validPushToken(token string) bool {
	if len(token) < 20 || len(token) > maxPushToken {
		return false
	}
	for _, c := range token {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '-' || c == '_' || c == ':' || c == '.') {
			return false
		}
	}
	return true
}

// This opaque, non-secret routing identifier is derived by the phone from its
// Hub/device/pairing identity. It must never contain a raw pairing credential.
func validNotificationScope(scope string) bool {
	return len(scope) == 68 && strings.HasPrefix(scope, "ns1_") && validPairHash(scope[4:])
}

// saveLocked writes the token file; p.mu held. Small and rare, so written in place.
func (p *pushState) saveLocked() {
	if p.path == "" {
		return
	}
	data, _ := json.Marshal(map[string]any{"version": 1, "entries": p.entries})
	if err := writeFileAtomic(p.path, data, 0o600); err != nil && p.logger != nil {
		p.logger.Warn("push tokens not saved", "err", err)
	}
}

func (p *pushState) set(key string, entry *pushEntry) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if entry == nil {
		if _, ok := p.entries[key]; !ok {
			return
		}
		delete(p.entries, key)
	} else {
		if old, ok := p.entries[key]; ok && old.Token == entry.Token && old.PairHash == entry.PairHash && old.Platform == entry.Platform && old.NotificationScope == entry.NotificationScope {
			return
		}
		p.revision++
		owned := *entry
		owned.revision = p.revision
		p.entries[key] = owned
	}
	p.saveLocked()
}

func (p *pushState) dropIfCurrent(job pushJob) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[job.key]; ok && e == job.entry {
		delete(p.entries, job.key)
		p.saveLocked()
	}
}

// ---------------------------------------------------------------------------
// /app/push/register  {deviceId, token, platform, notificationScope}; an empty token unregisters.

type pushRegisterBody struct {
	DeviceID          string `json:"deviceId"`
	Token             string `json:"token"`
	Platform          string `json:"platform"`
	NotificationScope string `json:"notificationScope,omitempty"`
}

func (b *pushRegisterBody) UnmarshalJSON(raw []byte) error {
	type plain pushRegisterBody
	if err := decodeExactObject(raw, []string{"deviceId", "token", "platform", "notificationScope"}, (*plain)(b)); err != nil {
		return err
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(raw, &fields)
	if _, supplied := fields["notificationScope"]; supplied && !validNotificationScope(b.NotificationScope) {
		return errors.New("invalid notification scope")
	}
	return nil
}

func (h *Hub) handlePushRegister(w http.ResponseWriter, r *http.Request, acct string) {
	var body pushRegisterBody
	if !decodeBody(w, r, 16<<10, &body) {
		return
	}
	if !validID(body.DeviceID) || (body.Token != "" && !validPushToken(body.Token)) || (body.Platform != "" && body.Platform != "android" && body.Platform != "ios") || (body.NotificationScope != "" && !validNotificationScope(body.NotificationScope)) {
		writeError(w, http.StatusBadRequest, "推送登记参数不正确")
		return
	}
	h.mu.Lock()
	actualAcct := acct
	var dev *device
	if strings.HasPrefix(acct, "bot:") {
		actualAcct, dev = h.botAccountAndDeviceLocked(acct, r, body.DeviceID)
	} else {
		dev = h.pairedDeviceLocked(acct, r)
	}
	a := h.accounts[actualAcct]
	if a == nil || dev == nil || a.devices[body.DeviceID] != dev {
		h.mu.Unlock()
		writeError(w, http.StatusForbidden, "这台电脑尚未与手机配对")
		return
	}
	pairHash := hex.EncodeToString(dev.pairHash[:])
	if !h.push.enabled() {
		h.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "push": "off"})
		return
	}
	key := pairKey(acct, body.DeviceID)
	if body.Token == "" {
		h.push.set(key, nil)
	} else {
		platform := body.Platform
		if platform == "" {
			platform = "android"
		}
		h.push.set(key, &pushEntry{PairHash: pairHash, Token: body.Token, Platform: platform, NotificationScope: body.NotificationScope, UpdatedAt: nowMs()})
	}
	// Keep pairing and registration in one admission boundary. A delayed old
	// registration/unregister must not replace the new phone's registration.
	h.mu.Unlock()
	writeJSON(w, http.StatusOK, map[string]any{"ok": true, "push": "fcm"})
}

// ---------------------------------------------------------------------------
// Deciding what to send (h.mu held; never blocks).

func eventString(m map[string]json.RawMessage, key string) string {
	var s string
	if raw, ok := m[key]; ok {
		_ = json.Unmarshal(raw, &s)
	}
	return s
}

func eventBool(m map[string]json.RawMessage, key string) bool {
	var value bool
	if raw, ok := m[key]; ok {
		_ = json.Unmarshal(raw, &value)
	}
	return value
}

func agentName(tool string) string {
	switch tool {
	case "codex":
		return "Codex"
	case "claude":
		return "Claude Code"
	case "claude-desktop":
		return "Claude Desktop"
	}
	return "Agent"
}

func (h *Hub) pushEventLocked(acct string, a *account, dev *device, m map[string]json.RawMessage, seq int64) {
	p := h.push
	if !p.enabled() || dev == nil || !dev.hasPair || dev.banned || !h.managedPairAllowedLocked(dev) {
		return
	}
	typ, sessionKey := eventString(m, "type"), eventString(m, "sessionKey")
	var kind, ref string
	switch typ {
	case "turn":
		switch eventString(m, "status") {
		case "completed":
			kind = "done"
		case "failed":
			kind = "failed"
		default:
			return
		}
		ref = seqText(seq)
		if turn := eventString(m, "turnId"); turn != "" {
			ref = turn
		}
	case "message":
		if eventString(m, "role") != "assistant" || !eventBool(m, "final") || eventString(m, "parentId") != "" || eventString(m, "messagePhase") == "commentary" {
			return
		}
		kind = "reply"
		ref = eventString(m, "id")
		if ref == "" {
			ref = seqText(seq)
		}
	default:
		return
	}
	if sessionKey == "" {
		return
	}
	// Always enqueue eligible news. `lastAppSeen` is only a recent-request
	// timestamp, not reliable foreground state: suppressing here can lose a
	// short task that finishes just after the phone backgrounds. The Android
	// receiver checks actual process foreground state before showing a banner.
	key := pairKey(acct, dev.info.DeviceID)
	pairHash := hex.EncodeToString(dev.pairHash[:])
	tool := eventString(m, "tool")
	if tool == "" && a != nil {
		if s := a.sessions[sessionID(dev.info.DeviceID, sessionKey)]; s != nil {
			tool = s.tool
		}
	}
	now := nowMs()
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.entries[key]
	if !ok || entry.PairHash != pairHash {
		return
	}
	turnID := eventString(m, "turnId")
	turnNoticeKey := key + "\x00" + entry.PairHash + "\x00" + entry.NotificationScope + "\x00" + sessionKey + "\x00" + turnID
	if turnID != "" {
		if previous, exists := p.turnNotices[turnNoticeKey]; exists && now-previous.at < pushDedupeWindow.Milliseconds() {
			return // one terminal/final notice per turn prevents repeated interruption.
		}
	}
	dedupe := key + "\x00" + entry.PairHash + "\x00" + entry.NotificationScope + "\x00" + sessionKey + "\x00" + kind + "\x00" + ref
	if at, seen := p.recent[dedupe]; seen && now-at < pushDedupeWindow.Milliseconds() {
		return
	}
	if len(p.recent) > 4096 {
		for k, at := range p.recent {
			if now-at >= pushDedupeWindow.Milliseconds() {
				delete(p.recent, k)
			}
		}
	}
	if len(p.turnNotices) > 4096 {
		for k, notice := range p.turnNotices {
			if now-notice.at >= pushDedupeWindow.Milliseconds() {
				delete(p.turnNotices, k)
			}
		}
		for k := range p.turnNotices {
			if len(p.turnNotices) <= 4096 {
				break
			}
			delete(p.turnNotices, k)
		}
	}
	// All updates for the same turn share one notification identity. The first
	// eligible final/terminal event owns the single alert for that turn.
	turnRef := eventString(m, "turnId")
	if turnRef == "" || len(turnRef) > 256 {
		turnRef = seqText(seq)
	}
	notificationID := "task:" + dev.info.DeviceID + ":" + sessionKey + ":" + turnRef
	job := pushJob{key: key, account: acct, deviceID: dev.info.DeviceID, entry: entry, collapse: "task:" + sessionKey + ":" + turnRef, data: map[string]string{
		"type": "salcara.task", "kind": kind, "agent": agentName(tool),
		"deviceId": dev.info.DeviceID, "sessionKey": sessionKey,
		"id": notificationID,
	}}
	if entry.NotificationScope != "" {
		job.data["notificationScope"] = entry.NotificationScope
		job.collapse = entry.NotificationScope + ":" + job.collapse
	}
	if h.durable != nil {
		durableDedupe := dedupe
		if turnID != "" {
			durableDedupe = turnNoticeKey
		}
		inserted, err := h.durable.savePush(&job, durableDedupe)
		if err == nil && !inserted {
			return
		}
		if err != nil {
			job.durableID = ""
			if p.logger != nil {
				p.logger.Warn("push persistence unavailable")
			}
		}
	}
	select {
	case p.queue <- job:
		p.recent[dedupe] = now
		if turnID != "" {
			p.turnNotices[turnNoticeKey] = turnNotice{at: now}
		}
	default: // a burst beyond the queue is dropped rather than stalling event intake; a retry may enqueue it later.
		if p.logger != nil {
			p.logger.Warn("push queue full; event may be retried")
		}
	}
}

func seqText(n int64) string { return strconv.FormatInt(n, 10) }

// Recheck both the pairing and the exact registration generation after queue
// and OAuth waits. Never retarget an old event using a replacement registration.
func (h *Hub) currentPushJob(job pushJob) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	a := h.accounts[job.account]
	if a == nil {
		return false
	}
	dev := a.devices[job.deviceID]
	if dev == nil || !dev.hasPair || dev.banned || !h.managedPairAllowedLocked(dev) || hex.EncodeToString(dev.pairHash[:]) != job.entry.PairHash {
		return false
	}
	h.push.mu.Lock()
	defer h.push.mu.Unlock()
	entry, ok := h.push.entries[job.key]
	return ok && entry == job.entry
}

// runPush sends queued messages until done is closed.
func (h *Hub) runPush(done <-chan struct{}) {
	p := h.push
	retry := time.NewTicker(5 * time.Second)
	defer retry.Stop()
	for {
		select {
		case <-done:
			return
		case <-retry.C:
			h.recoverPushJobs()
		case job := <-p.queue:
			if !h.currentPushJob(job) {
				h.finishPush(job, false)
				continue
			}
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			err := p.sender.send(ctx, job.entry.Token, job.entry.Platform, job.data, job.collapse, func() bool { return h.currentPushJob(job) })
			cancel()
			if errors.Is(err, errPushTokenGone) {
				p.dropIfCurrent(job)
			} else if err != nil && h.cfg.Logger != nil {
				h.cfg.Logger.Warn("push not delivered", "err", err)
			}
			h.finishPush(job, err != nil && !errors.Is(err, errPushTokenGone))
		}
	}
}

// forgetPushLocked drops a computer's phone token when the computer is removed.
func (h *Hub) forgetPush(acct, deviceID string) {
	if h.push != nil {
		h.push.set(pairKey(acct, deviceID), nil)
	}
}
