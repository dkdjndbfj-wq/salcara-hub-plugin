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
	"sync"
	"time"
)

/*
Push notifications for the paired phone (Firebase Cloud Messaging).

The phone registers its FCM token through /app/push/register with its pair
token. The Hub keeps it next to that pairing (hash only binds it; a new
pairing or an unpair makes it stale automatically) in push-tokens.json, apart
from devices.json, so older Hubs can still read their own state.

When the computer reports that a turn completed or failed, or that it needs an
approval or an answer, the Hub sends one data message. The message carries no
task text: only the kind, the agent name and the ids the phone needs to open
the thread; the phone writes a generic notification itself. Nothing is sent
while the phone is actively talking to the Hub (it shows the news itself), and
the same news is sent at most once even if the computer retries an upload.
*/

const (
	pushTokensFile   = "push-tokens.json"
	maxPushToken     = 4096
	pushActiveWindow = 15 * time.Second
	pushDedupeWindow = 10 * time.Minute
	pushQueueSize    = 256
)

type pushEntry struct {
	PairHash  string `json:"pairHash"`
	Token     string `json:"token"`
	Platform  string `json:"platform"`
	UpdatedAt int64  `json:"updatedAt"`
}

type pushJob struct {
	key      string
	token    string
	collapse string
	data     map[string]string
}

type pushState struct {
	sender *fcmSender
	path   string
	logger *slog.Logger

	mu      sync.Mutex
	entries map[string]pushEntry // pairKey(account, device)
	recent  map[string]int64     // dedupe key -> unix ms
	queue   chan pushJob
}

func newPushState(cfg *Config) (*pushState, error) {
	p := &pushState{entries: map[string]pushEntry{}, recent: map[string]int64{}, queue: make(chan pushJob, pushQueueSize), logger: cfg.Logger}
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
					if validPushToken(e.Token) && len(e.PairHash) == 64 {
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
		if old, ok := p.entries[key]; ok && old.Token == entry.Token && old.PairHash == entry.PairHash && old.Platform == entry.Platform {
			return
		}
		p.entries[key] = *entry
	}
	p.saveLocked()
}

func (p *pushState) dropIfToken(key, token string) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if e, ok := p.entries[key]; ok && e.Token == token {
		delete(p.entries, key)
		p.saveLocked()
	}
}

// ---------------------------------------------------------------------------
// /app/push/register  {deviceId, token, platform}; an empty token unregisters.

type pushRegisterBody struct {
	DeviceID string `json:"deviceId"`
	Token    string `json:"token"`
	Platform string `json:"platform"`
}

func (h *Hub) handlePushRegister(w http.ResponseWriter, r *http.Request, acct string) {
	var body pushRegisterBody
	if !decodeBody(w, r, 16<<10, &body) {
		return
	}
	if !validID(body.DeviceID) || (body.Token != "" && !validPushToken(body.Token)) || (body.Platform != "" && body.Platform != "android") {
		writeError(w, http.StatusBadRequest, "推送登记参数不正确")
		return
	}
	h.mu.Lock()
	a := h.accounts[acct]
	dev := h.pairedDeviceLocked(acct, r)
	if a == nil || dev == nil || a.devices[body.DeviceID] != dev {
		h.mu.Unlock()
		writeError(w, http.StatusForbidden, "这台电脑尚未与手机配对")
		return
	}
	pairHash := hex.EncodeToString(dev.pairHash[:])
	h.mu.Unlock()
	if !h.push.enabled() {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true, "push": "off"})
		return
	}
	key := pairKey(acct, body.DeviceID)
	if body.Token == "" {
		h.push.set(key, nil)
	} else {
		h.push.set(key, &pushEntry{PairHash: pairHash, Token: body.Token, Platform: "android", UpdatedAt: nowMs()})
	}
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
	if !p.enabled() || dev == nil || !dev.hasPair || dev.banned {
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
	case "approval.request":
		kind = "ask"
		if eventString(m, "kind") == "question" {
			kind = "question"
		}
		ref = eventString(m, "approvalId")
		if ref == "" {
			ref = seqText(seq)
		}
	default:
		return
	}
	if sessionKey == "" {
		return
	}
	// The phone is talking to the Hub right now (app open, or its own watch running): it shows this itself.
	if nowMs()-dev.lastAppSeen < pushActiveWindow.Milliseconds() {
		return
	}
	key := pairKey(acct, dev.info.DeviceID)
	pairHash := hex.EncodeToString(dev.pairHash[:])
	tool := eventString(m, "tool")
	if tool == "" && a != nil {
		if s := a.sessions[sessionID(dev.info.DeviceID, sessionKey)]; s != nil {
			tool = s.tool
		}
	}
	dedupe := key + "\x00" + sessionKey + "\x00" + kind + "\x00" + ref
	now := nowMs()
	p.mu.Lock()
	defer p.mu.Unlock()
	entry, ok := p.entries[key]
	if !ok || entry.PairHash != pairHash {
		return
	}
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
	p.recent[dedupe] = now
	// Same notification ids as the phone's own background watch, so one replaces the other.
	idRef := ref
	if kind == "done" || kind == "failed" {
		idRef = seqText(seq)
	}
	prefix := kind
	if kind == "question" {
		prefix = "ask"
	}
	job := pushJob{key: key, token: entry.Token, collapse: kind + ":" + sessionKey, data: map[string]string{
		"type": "salcara.task", "kind": kind, "agent": agentName(tool),
		"deviceId": dev.info.DeviceID, "sessionKey": sessionKey,
		"id": prefix + ":" + dev.info.DeviceID + ":" + sessionKey + ":" + idRef,
	}}
	select {
	case p.queue <- job:
	default: // a burst beyond the queue is dropped rather than stalling the event intake
	}
}

func seqText(n int64) string { return strconv.FormatInt(n, 10) }

// runPush sends queued messages until done is closed.
func (h *Hub) runPush(done <-chan struct{}) {
	p := h.push
	for {
		select {
		case <-done:
			return
		case job := <-p.queue:
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			err := p.sender.send(ctx, job.token, job.data, job.collapse)
			cancel()
			if errors.Is(err, errPushTokenGone) {
				p.dropIfToken(job.key, job.token)
			} else if err != nil && h.cfg.Logger != nil {
				h.cfg.Logger.Warn("push not delivered", "err", err)
			}
		}
	}
}

// forgetPushLocked drops a computer's phone token when the computer is removed.
func (h *Hub) forgetPush(acct, deviceID string) {
	if h.push != nil {
		h.push.set(pairKey(acct, deviceID), nil)
	}
}
