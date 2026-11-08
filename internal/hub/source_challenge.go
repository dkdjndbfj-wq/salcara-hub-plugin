package hub

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

func sourceGrantSnapshot(grant SignedSourceGrant) (SourceSnapshot, error) {
	raw, err := base64.RawURLEncoding.Strict().DecodeString(grant.Payload)
	if err != nil {
		return SourceSnapshot{}, err
	}
	return ParseSourceSnapshot(raw)
}

// SourceChallenge is a non-secret, Hub-created authorization request. The
// original command is retained so acquisition never hashes a mobile reserialization.
type SourceChallenge struct {
	ChallengeID    string          `json:"challengeId"`
	Audience       string          `json:"audience"`
	PolicyRevision string          `json:"policyRevision"`
	DeviceID       string          `json:"deviceId"`
	BindingID      string          `json:"bindingId"`
	RequestID      string          `json:"requestId"`
	CommandHash    string          `json:"commandHash"`
	Command        json.RawMessage `json:"command"`
	ExpiresAt      int64           `json:"expiresAt"`
}

type sourceChallengeState struct {
	SourceChallenge
	account     string
	authAccount string
	acquiring   bool
	grant       *SignedSourceGrant
}

// sourceIdentityLocked separates the caller's authentication (pair or Bot
// token) from the per-device account used for dispatch and durable receipts.
func (h *Hub) sourceIdentityLocked(acct string, r *http.Request, deviceID string) (string, *device, string) {
	if strings.HasPrefix(acct, "bot:") {
		actual, dev := h.botAccountAndDeviceLocked(acct, r, deviceID)
		return actual, dev, h.botSourceBindingLocked(acct, dev)
	}
	dev := h.pairedDeviceLocked(acct, r)
	if dev == nil || dev.info.DeviceID != deviceID || dev.banned {
		return acct, nil, ""
	}
	return acct, dev, dev.bindingID
}

func (s *sourceChallengeState) authenticationAccount() string {
	if s.authAccount != "" {
		return s.authAccount
	}
	return s.account // Existing paired challenges and in-process integrations.
}

func (h *Hub) sweepSourceChallengesLocked(now int64) {
	for id, challenge := range h.sourceChallenges {
		if challenge.ExpiresAt <= now {
			delete(h.sourceChallenges, id)
		}
	}
}

func sourceFailure(w http.ResponseWriter, status int, code string) {
	writeJSON(w, status, map[string]any{"error": "无法取得可信远程来源授权", "code": code, "retryable": false})
}

func (h *Hub) handleSourceChallenge(w http.ResponseWriter, r *http.Request, acct string) {
	var req commandReq
	if !decodeCommandRequest(w, r, &req) {
		return
	}
	meta, err := commandMeta(req.Command)
	canonical, canonicalErr := canonicalCommand(req.Command)
	if err != nil || canonicalErr != nil || !commandMetadataStringsValid(meta) || !sourceExecutionCommand(commandString(meta, "type")) || !requestUUID.MatchString(req.RequestID) || req.SourceGrant != nil {
		sourceFailure(w, 400, "source_command_invalid")
		return
	}
	h.remoteAccessGate.RLock()
	defer h.remoteAccessGate.RUnlock()
	h.mu.Lock()
	actualAcct, dev, binding := h.sourceIdentityLocked(acct, r, req.DeviceID)
	if dev == nil || binding == "" {
		h.mu.Unlock()
		sourceFailure(w, 403, "device_authorization_expired")
		return
	}
	p := h.userPolicyLocked(req.DeviceID)
	if p.Mode != "restricted" {
		h.mu.Unlock()
		sourceFailure(w, 403, "source_challenge_unavailable")
		return
	}
	if !deviceSupportsSourceGuard(dev) || !deviceHasCapability(dev, "source.grant.acquire.v1") || !deviceHasCapability(dev, p.Station.requiredCapability()) {
		h.mu.Unlock()
		sourceFailure(w, 403, "source_acquisition_unsupported")
		return
	}
	if !p.Station.supportedVersion() || p.Audience == "" || len(p.Issuers) == 0 && !p.Station.AllowOfficialAccounts || h.receipts == nil || h.receipts.dir == "" {
		h.mu.Unlock()
		sourceFailure(w, 403, "source_policy_unavailable")
		return
	}
	if commandString(meta, "accountId") == "" || commandAgentMetadata(meta) == "" {
		h.mu.Unlock()
		sourceFailure(w, 400, "source_metadata_required")
		return
	}
	if h.sourceChallenges == nil {
		h.sourceChallenges = map[string]*sourceChallengeState{}
	}
	now, perDevice, commandBytes := time.Now().Unix(), 0, 0
	for id, challenge := range h.sourceChallenges {
		if challenge.ExpiresAt <= now {
			delete(h.sourceChallenges, id)
			continue
		}
		if challenge.account == actualAcct && challenge.DeviceID == req.DeviceID {
			perDevice++
		}
		commandBytes += len(challenge.Command)
	}
	if len(h.sourceChallenges) >= 256 || perDevice >= 32 || commandBytes+len(req.Command) > 8<<20 {
		h.mu.Unlock()
		sourceFailure(w, 429, "source_challenge_capacity")
		return
	}
	challenge := SourceChallenge{ChallengeID: "challenge-" + newID(), Audience: p.Audience, PolicyRevision: p.Revision, DeviceID: req.DeviceID, BindingID: binding, RequestID: req.RequestID, CommandHash: shaHex(canonical), Command: append(json.RawMessage(nil), req.Command...), ExpiresAt: now + 120}
	h.sourceChallenges[challenge.ChallengeID] = &sourceChallengeState{SourceChallenge: challenge, account: actualAcct, authAccount: acct}
	h.mu.Unlock()
	writeJSON(w, 200, map[string]any{"challengeId": challenge.ChallengeID, "expiresAt": challenge.ExpiresAt})
}

func deviceHasCapability(dev *device, value string) bool {
	if dev != nil {
		for _, capability := range dev.info.Capabilities {
			if capability == value {
				return true
			}
		}
	}
	return false
}

func (h *Hub) sourceChallengeCurrentLocked(id, acct string, dev *device) *sourceChallengeState {
	challenge := h.sourceChallenges[id]
	if dev == nil {
		return nil
	}
	p := h.userPolicyLocked(dev.info.DeviceID)
	if challenge == nil || dev.banned || h.userDisabledLocked(dev.info.DeviceID) || challenge.account != acct || challenge.DeviceID != dev.info.DeviceID || challenge.ExpiresAt <= time.Now().Unix() || p.Mode != "restricted" || challenge.PolicyRevision != p.Revision || challenge.Audience != p.Audience {
		return nil
	}
	binding := dev.bindingID
	if strings.HasPrefix(challenge.authenticationAccount(), "bot:") {
		binding = h.botSourceBindingLocked(challenge.authenticationAccount(), dev)
	}
	if binding == "" || challenge.BindingID != binding {
		return nil
	}
	return challenge
}

func (h *Hub) sourceChallengeForCallerLocked(id, acct string, r *http.Request) (*sourceChallengeState, *device) {
	state := h.sourceChallenges[id]
	if state == nil || state.authenticationAccount() != acct {
		return nil, nil
	}
	actual, dev, binding := h.sourceIdentityLocked(acct, r, state.DeviceID)
	if binding == "" || binding != state.BindingID {
		return nil, nil
	}
	return h.sourceChallengeCurrentLocked(id, actual, dev), dev
}

func (h *Hub) handleBridgeSourceChallenge(w http.ResponseWriter, r *http.Request, acct string) {
	id, deviceID := r.URL.Query().Get("challengeId"), r.Header.Get("X-Salcara-Device-Id")
	h.mu.Lock()
	var dev *device
	if account := h.accounts[acct]; account != nil {
		dev = account.devices[deviceID]
	}
	if dev == nil || !dev.hasSecret || !matchesDeviceSecret(dev, r) {
		h.mu.Unlock()
		sourceFailure(w, 403, "device_authorization_expired")
		return
	}
	challenge := h.sourceChallengeCurrentLocked(id, acct, dev)
	if challenge == nil || !challenge.acquiring {
		h.mu.Unlock()
		sourceFailure(w, 403, "source_challenge_expired")
		return
	}
	copyChallenge, policy := challenge.SourceChallenge, h.userPolicyLocked(deviceID)
	h.mu.Unlock()
	writeJSON(w, 200, map[string]any{"challenge": copyChallenge, "policy": policy})
}

func (h *Hub) handleSourceGrant(w http.ResponseWriter, r *http.Request, acct string) {
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(60 * time.Second))
	defer rc.SetReadDeadline(time.Time{}) //nolint:errcheck
	var input struct {
		ChallengeID string `json:"challengeId"`
	}
	raw, readErr := io.ReadAll(http.MaxBytesReader(w, r.Body, 1024))
	if readErr != nil {
		// Close before clearing the deadline: net/http otherwise tries to drain
		// an incomplete body while finishing the response on this connection.
		_ = r.Body.Close()
		w.Header().Set("Connection", "close")
	}
	if readErr != nil || decodeExactObject(raw, []string{"challengeId"}, &input) != nil || !policyID(input.ChallengeID) {
		sourceFailure(w, 400, "source_challenge_invalid")
		return
	}
	h.mu.Lock()
	challenge, _ := h.sourceChallengeForCallerLocked(input.ChallengeID, acct, r)
	if challenge == nil {
		h.mu.Unlock()
		sourceFailure(w, 403, "source_challenge_expired")
		return
	}
	execAcct := challenge.account
	if challenge.grant != nil {
		grant := *challenge.grant
		h.mu.Unlock()
		writeJSON(w, 200, map[string]any{"sourceGrant": grant})
		return
	}
	if challenge.acquiring {
		h.mu.Unlock()
		sourceFailure(w, 409, "source_acquisition_pending")
		return
	}
	challenge.acquiring = true
	frozen := challenge.SourceChallenge
	h.mu.Unlock()
	defer func() {
		h.mu.Lock()
		challenge.acquiring = false
		if challenge.grant == nil {
			delete(h.sourceChallenges, frozen.ChallengeID)
			challenge.Command = nil
		}
		h.mu.Unlock()
	}()
	command, _ := json.Marshal(map[string]any{"type": "source.grant.acquire", "challengeId": frozen.ChallengeID})
	rep, status := h.executeCommandGuarded(r.Context(), execAcct, frozen.DeviceID, command, func(dev *device) bool {
		current, currentDevice := h.sourceChallengeForCallerLocked(frozen.ChallengeID, acct, r)
		return current != nil && currentDevice == dev
	}, "", nil, nil)
	if status == 499 {
		return
	}
	if status != 200 || !rep.OK {
		sourceFailure(w, 403, "source_acquisition_unsupported")
		return
	}
	h.remoteAccessGate.RLock()
	defer h.remoteAccessGate.RUnlock()
	grant, err := ParseSignedSourceGrant(rep.Result)
	if err != nil {
		// This alternative is accepted only from executeCommandGuarded's
		// authenticated reply for this exact source.grant.acquire command.
		grant, err = h.officialSourceGrant(rep.Result, frozen)
	}
	if err != nil {
		sourceFailure(w, 403, "source_grant_invalid")
		return
	}
	req := commandReq{DeviceID: frozen.DeviceID, Command: frozen.Command, RequestID: frozen.RequestID, SourceGrant: &grant}
	if code := h.evaluateSourcePolicy(acct, r, req); code != "" {
		sourceFailure(w, 403, code)
		return
	}
	// Acquisition grants always carry the nonce, including when older external
	// integrations still submit version-one grants directly to /app/commands.
	snapshot, err := sourceGrantSnapshot(grant)
	if err != nil || snapshot.ChallengeID != frozen.ChallengeID {
		sourceFailure(w, 403, "source_challenge_mismatch")
		return
	}
	h.mu.Lock()
	if current, _ := h.sourceChallengeForCallerLocked(frozen.ChallengeID, acct, r); current == nil {
		h.mu.Unlock()
		sourceFailure(w, 403, "source_challenge_expired")
		return
	}
	challenge.grant = &grant
	challenge.Command = nil // the hash/binding suffice after acquisition; do not retain private prompts/chunks
	h.mu.Unlock()
	writeJSON(w, 200, map[string]any{"sourceGrant": grant})
}

// officialSourceGrant never accepts a provider token. The Bridge has proved the
// selected protected vault entry locally; the Hub binds that assertion to its
// own still-current challenge and signs only a short-lived public snapshot.
func (h *Hub) officialSourceGrant(raw []byte, challenge SourceChallenge) (SignedSourceGrant, error) {
	var envelope struct {
		OfficialAccount json.RawMessage `json:"officialAccount"`
	}
	if len(raw) > MaxSignedSourceGrantBytes || decodeExactObject(raw, []string{"officialAccount"}, &envelope) != nil {
		return SignedSourceGrant{}, errors.New("invalid official source assertion")
	}
	snapshot, err := ParseSourceSnapshot(envelope.OfficialAccount)
	p := h.deviceRemoteAccessPolicy(challenge.DeviceID)
	if err != nil || p.Mode != "restricted" || p.Station == nil || !p.Station.AllowOfficialAccounts || snapshot.Issuer != stationOfficialIssuer || snapshot.SourceType != "official_account_proxy" || snapshot.Origin != officialAccountOrigin || snapshot.APIBase != officialAccountAPIBase || snapshot.ControlSurface != "cli" || (snapshot.Agent != "codex" && snapshot.Agent != "claude") || snapshot.ChallengeID != challenge.ChallengeID || snapshot.Audience != challenge.Audience || snapshot.PolicyRevision != challenge.PolicyRevision || snapshot.DeviceID != challenge.DeviceID || snapshot.BindingID != challenge.BindingID || snapshot.RequestID != challenge.RequestID || snapshot.CommandHash != challenge.CommandHash {
		return SignedSourceGrant{}, errors.New("invalid official source assertion")
	}
	snapshot.IssuedAt = time.Now().Unix()
	snapshot.ExpiresAt = min(snapshot.IssuedAt+120, challenge.ExpiresAt)
	payload, err := json.Marshal(snapshot)
	if err != nil {
		return SignedSourceGrant{}, err
	}
	return SignedSourceGrant{Payload: base64.RawURLEncoding.EncodeToString(payload), Signature: base64.RawURLEncoding.EncodeToString(ed25519.Sign(h.officialPrivateKey, payload))}, nil
}

func (h *Hub) sourceChallengeMatches(acct, authAccount, binding string, req commandReq, snapshot SourceSnapshot) bool {
	h.mu.Lock()
	defer h.mu.Unlock()
	challenge := h.sourceChallenges[snapshot.ChallengeID]
	return challenge != nil && challenge.account == acct && challenge.authenticationAccount() == authAccount && challenge.BindingID == binding && challenge.DeviceID == req.DeviceID && challenge.RequestID == req.RequestID && challenge.CommandHash == snapshot.CommandHash && challenge.PolicyRevision == snapshot.PolicyRevision && challenge.Audience == snapshot.Audience && challenge.ExpiresAt > time.Now().Unix()
}

// Receipts retain replay authority after a completed command. Removing its
// ephemeral nonce releases acquisition capacity (including attachment chunks)
// without allowing that request ID to be dispatched again.
func (h *Hub) retireCompletedSourceChallenge(acct string, r *http.Request, req commandReq) {
	if req.SourceGrant == nil {
		return
	}
	snapshot, err := sourceGrantSnapshot(*req.SourceGrant)
	if err != nil || snapshot.ChallengeID == "" || !h.cachedReceiptReplay(acct, r, req) {
		return
	}
	h.mu.Lock()
	challenge := h.sourceChallenges[snapshot.ChallengeID]
	if challenge != nil && challenge.account == acct && challenge.DeviceID == req.DeviceID && challenge.RequestID == req.RequestID && challenge.CommandHash == snapshot.CommandHash {
		delete(h.sourceChallenges, snapshot.ChallengeID)
	}
	h.mu.Unlock()
}
