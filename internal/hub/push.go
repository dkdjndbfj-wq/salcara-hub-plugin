package hub

import (
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
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
system banners while foregrounded. Approvals/questions have their own identities
and resolved events silently cancel them; no push authorizes a response.
*/

const (
	pushTokensFile = "push-tokens.json"
	maxPushToken   = 4096
	pushQueueSize  = 256
)

type pushEntry struct {
	PairHash          string `json:"pairHash"`
	Token             string `json:"token"`
	Platform          string `json:"platform"`
	NotificationScope string `json:"notificationScope,omitempty"`
	UpdatedAt         int64  `json:"updatedAt"`
	revision          uint64 // process-local; durable jobs rebind only exact stable registrations
}

type pushJob struct {
	dedupe    string
	noticeKey string
	noticeRef string
	durableID string
	key       string
	account   string
	deviceID  string
	entry     pushEntry
	collapse  string
	data      map[string]string
}

type pushState struct {
	sender *fcmSender
	path   string
	logger *slog.Logger

	mu       sync.Mutex
	entries  map[string]pushEntry // pairKey(account, device)
	notices  map[string]*pushNoticeSession
	queue    chan pushJob
	revision uint64
}

func newPushState(cfg *Config) (*pushState, error) {
	p := &pushState{entries: map[string]pushEntry{}, notices: map[string]*pushNoticeSession{}, queue: make(chan pushJob, pushQueueSize), logger: cfg.Logger}
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

func (p *pushState) unregisterScope(key, scope string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if current, ok := p.entries[key]; ok && (scope == "" || scope == current.NotificationScope) {
		delete(p.entries, key)
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
	pairHash := h.pushBindingLocked(acct, dev)
	if pairHash == "" {
		h.mu.Unlock()
		writeError(w, http.StatusForbidden, "手机通知授权已失效")
		return
	}
	if !h.push.enabled() {
		h.mu.Unlock()
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "push": "off"})
		return
	}
	key := pairKey(acct, body.DeviceID)
	if body.Token == "" {
		h.push.unregisterScope(key, body.NotificationScope)
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
	case "deepseek":
		return "DeepSeek"
	case "hermes":
		return "Hermes"
	}
	return "Agent"
}

func (h *Hub) pushEventLocked(acct string, a *account, dev *device, m map[string]json.RawMessage, seq int64) {
	p := h.push
	if !p.enabled() || dev == nil || !relevantPushEvent(m) {
		return
	}
	sessionKey := eventString(m, "sessionKey")
	if sessionKey == "" || len(sessionKey) > 512 {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	noticeKey := shaHex(h.durableScopeLocked(acct, dev) + "\x00" + sessionKey)
	state := h.pushNoticeLocked(noticeKey)
	state.Decisions = map[string]pushNoticeDecision{}
	kind, ref := state.observe(m, seq, nowMs())
	h.enqueuePushJobs(h.pushJobsLocked(acct, a, dev, m, seq, noticeKey, kind, ref))
}

func (h *Hub) pushJobsLocked(acct string, a *account, dev *device, m map[string]json.RawMessage, seq int64, noticeKey, kind, ref string) []pushJob {
	if kind == "" {
		return nil
	}
	sessionKey := eventString(m, "sessionKey")
	var jobs []pushJob
	tool := eventString(m, "tool")
	if tool == "" && a != nil {
		if s := a.sessions[sessionID(dev.info.DeviceID, sessionKey)]; s != nil {
			tool = s.tool
		}
	}
	for _, recipient := range h.pushAccountsLocked(acct, dev) {
		key := pairKey(recipient, dev.info.DeviceID)
		entry, ok := h.push.entries[key]
		binding := h.pushBindingLocked(recipient, dev)
		if !ok || binding == "" || entry.PairHash != binding {
			continue
		}
		lane := "task"
		if kind == "approval" || kind == "question" || kind == "resolved" {
			lane = "approval"
		}
		id := lane + ":" + dev.info.DeviceID + ":" + sessionKey + ":" + ref
		if entry.NotificationScope != "" {
			id = entry.NotificationScope + ":" + id
		}
		data := map[string]string{"type": "salcara.task", "kind": kind, "agent": agentName(tool),
			"deviceId": dev.info.DeviceID, "sessionKey": sessionKey, "id": id,
			"eventType": eventString(m, "type"), "seq": seqText(seq), "eventTs": seqText(eventMillis(m, "ts")), "ts": seqText(eventMillis(m, "ts"))}
		if entry.NotificationScope != "" {
			data["notificationScope"] = entry.NotificationScope
		}
		if lane == "approval" {
			data["approvalId"] = ref
			if expires := eventMillis(m, "expiresAt"); expires > 0 {
				data["expiresAt"] = seqText(expires)
			}
		} else {
			data["turnRef"] = ref
		}
		for _, field := range []string{"turnId", "status", "role", "messagePhase", "parentId"} {
			if value := eventString(m, field); value != "" && len(value) <= 256 {
				data[field] = value
			}
		}
		if eventBool(m, "final") {
			data["final"] = "true"
		}
		if value := eventString(m, "id"); value != "" && len(value) <= 256 {
			data["eventId"] = value
		}
		job := pushJob{key: key, account: recipient, deviceID: dev.info.DeviceID, entry: entry, collapse: id, data: data, noticeKey: noticeKey, noticeRef: ref}
		dedupe := key + "\x00" + entry.PairHash + "\x00" + entry.NotificationScope + "\x00" + sessionKey + "\x00" + lane + "\x00" + ref
		// Upgrades retain presentation identity but cannot be consumed by an old job.
		if kind == "failed" || kind == "resolved" {
			dedupe += "\x00" + kind
		}
		job.dedupe = dedupe
		jobs = append(jobs, job)
	}
	return jobs
}

func (h *Hub) enqueuePushJobs(jobs []pushJob) {
	for _, job := range jobs {
		select {
		case h.push.queue <- job:
		default:
			if h.push.logger != nil {
				h.push.logger.Warn("push queue full; durable retry required")
			}
		}
	}
}
func seqText(n int64) string { return strconv.FormatInt(n, 10) }

// Recheck both the pairing and the exact registration generation after queue
// and OAuth waits. Never retarget an old event using a replacement registration.
func (h *Hub) currentPushJob(job pushJob) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	dev := h.pushDeviceLocked(job.account, job.deviceID)
	if binding := h.pushBindingLocked(job.account, dev); binding == "" || binding != job.entry.PairHash {
		return false
	}
	h.push.mu.Lock()
	defer h.push.mu.Unlock()
	entry, ok := h.push.entries[job.key]
	if !ok || entry != job.entry {
		return false
	}
	if job.noticeKey != "" {
		return h.pushNoticeLocked(job.noticeKey).permits(job, nowMs())
	}
	return true
}

// forgetPushLocked drops a computer's phone token when the computer is removed.
func (h *Hub) forgetPush(acct, deviceID string) {
	if h.push != nil {
		h.push.set(pairKey(acct, deviceID), nil)
	}
}
