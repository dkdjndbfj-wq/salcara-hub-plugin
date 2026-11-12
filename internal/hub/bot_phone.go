package hub

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"unicode/utf8"

	"salcara/hub/internal/enrollment"
)

const botPhoneIntentFile = "bot-phone-intents.json"
const maxRetiredBotPhones = 1024
const maxBotStateBytes = 1 << 20

var (
	ErrBotPhoneConflict       = errors.New("bot phone revision conflict")
	ErrBotPhoneUnavailable    = errors.New("bot phone persistence pending")
	ErrBotPhoneNotFound       = errors.New("bot group not found")
	ErrBotPhoneInvalidRequest = errors.New("invalid bot phone reset")
)

// Only hashes and replay metadata are durable. In particular a client-chosen
// v2 token is never written to disk, or published before its commit succeeds.
type botPhoneState struct {
	PhoneIDHash             string               `json:"phoneIdHash,omitempty"`
	PhoneTokenHash          string               `json:"phoneTokenHash,omitempty"`
	PhoneRevision           string               `json:"phoneRevision"`
	PhoneAttemptID          string               `json:"phoneAttemptId,omitempty"`
	PhoneInviteHash         string               `json:"phoneInviteHash,omitempty"`
	RetiredPhoneTokenHashes []string             `json:"retiredPhoneTokenHashes,omitempty"`
	PhoneReset              *botPhoneResetReplay `json:"phoneReset,omitempty"`
	PhoneResetRequests      []string             `json:"phoneResetRequests,omitempty"`
}

type botPhoneResetReplay struct {
	RequestID      string `json:"requestId"`
	Fingerprint    string `json:"fingerprint"`
	ResultRevision string `json:"resultRevision"`
	HubURL         string `json:"hubUrl"`
}

type BotPhoneReset struct {
	GroupID          string `json:"groupId"`
	ExpectedRevision string `json:"expectedRevision"`
	RequestID        string `json:"requestId"`
	Confirm          bool   `json:"confirm"`
	Reason           string `json:"reason"`
}

type BotPhoneResetResult struct {
	Binding       BotBindingPackage `json:"binding"`
	PhoneRevision string            `json:"phoneRevision"`
}

func ValidateBotPhoneReset(in BotPhoneReset) error {
	if !validID(in.GroupID) || strings.TrimSpace(in.GroupID) != in.GroupID || !enrollment.Hex(in.ExpectedRevision, 16) || !enrollment.RequestID(in.RequestID) || !in.Confirm || len(in.Reason) > 256 || strings.TrimSpace(in.Reason) == "" || !utf8.ValidString(in.Reason) {
		return ErrBotPhoneInvalidRequest
	}
	return nil
}

func validBotPhoneState(p botPhoneState) bool {
	if !enrollment.Hex(p.PhoneRevision, 16) || (p.PhoneIDHash == "") != (p.PhoneTokenHash == "") || len(p.RetiredPhoneTokenHashes) > maxRetiredBotPhones {
		return false
	}
	if p.PhoneTokenHash != "" && (!enrollment.Hex(p.PhoneIDHash, 32) || !enrollment.Hex(p.PhoneTokenHash, 32)) {
		return false
	}
	if (p.PhoneAttemptID == "") != (p.PhoneInviteHash == "") || p.PhoneAttemptID != "" && (!enrollment.RequestID(p.PhoneAttemptID) || !enrollment.Hex(p.PhoneInviteHash, 32) || p.PhoneTokenHash == "") {
		return false
	}
	seen := map[string]bool{}
	for _, hash := range p.RetiredPhoneTokenHashes {
		if !enrollment.Hex(hash, 32) || seen[hash] || hash == p.PhoneTokenHash {
			return false
		}
		seen[hash] = true
	}
	if len(p.PhoneResetRequests) > maxRetiredBotPhones {
		return false
	}
	requests := map[string]bool{}
	for _, id := range p.PhoneResetRequests {
		if !enrollment.RequestID(id) || requests[id] {
			return false
		}
		requests[id] = true
	}
	if x := p.PhoneReset; x != nil {
		return requests[x.RequestID] && enrollment.Hex(x.Fingerprint, 32) && enrollment.Hex(x.ResultRevision, 16) && validateBotHubURL(x.HubURL, "")
	}
	return true
}

// State files are local authority, not best-effort configuration. Reject
// duplicate (including case-confusable) fields instead of choosing a winner.
func uniqueBotJSON(raw []byte) bool {
	d := json.NewDecoder(bytes.NewReader(raw))
	var walk func(int) bool
	walk = func(depth int) bool {
		if depth > 32 {
			return false
		}
		token, err := d.Token()
		if err != nil {
			return false
		}
		open, container := token.(json.Delim)
		if !container {
			return true
		}
		if open != '{' && open != '[' {
			return false
		}
		keys := map[string]bool{}
		for d.More() {
			if open == '{' {
				key, err := d.Token()
				name, ok := key.(string)
				name = strings.ToLower(name)
				if err != nil || !ok || keys[name] {
					return false
				}
				keys[name] = true
			}
			if !walk(depth + 1) {
				return false
			}
		}
		close, err := d.Token()
		return err == nil && (open == '{' && close == json.Delim('}') || open == '[' && close == json.Delim(']'))
	}
	return walk(0) && d.Decode(new(any)) == io.EOF
}

func (h *Hub) botTokenOwnersValidLocked() bool {
	owners := map[string]bool{}
	for _, g := range h.botGroups {
		for _, hash := range append(append([]string(nil), g.RetiredPhoneTokenHashes...), g.PhoneTokenHash) {
			if hash == "" {
				continue
			}
			if owners[hash] {
				return false
			}
			owners[hash] = true
		}
	}
	return true
}

var botCredentialHeaders = []string{"Authorization", "X-Salcara-Pair-Token", "X-Salcara-Device-Id", "X-Salcara-Device-Secret", "X-Salcara-Bot-Group", "X-Salcara-Bot-Invite", "X-Salcara-Bot-Signature", "X-Salcara-Enrollment-Claim", "X-Salcara-Enrollment-Grant"}

func validBotOnlyHeaders(r *http.Request) bool {
	for _, name := range botCredentialHeaders {
		if len(r.Header.Values(name)) != 0 {
			return false
		}
	}
	return len(r.Header.Values("X-Salcara-Bot-Token")) <= 1
}

func botAnonymousHeaders(r *http.Request) bool {
	if len(r.Header.Values("X-Salcara-Bot-Token")) != 0 {
		return false
	}
	for _, name := range botCredentialHeaders {
		if len(r.Header.Values(name)) != 0 {
			return false
		}
	}
	return true
}

func containsBotHash(hashes []string, hash string) bool {
	for _, x := range hashes {
		if subtleConstantTimeEqual([]byte(hash), x) {
			return true
		}
	}
	return false
}

func (h *Hub) botPhonePendingForRequestLocked(r *http.Request) bool {
	hash, ok := botTokenHash(r)
	if !ok {
		return false
	}
	for _, g := range h.botGroups {
		if g.PhonePending && (g.PhoneTokenHash == string(hashTextBytes(hash)) || containsBotHash(g.RetiredPhoneTokenHashes, string(hashTextBytes(hash)))) {
			return true
		}
	}
	return false
}

func writeBotPhonePending(w http.ResponseWriter, code string) {
	writeJSON(w, 503, map[string]any{"code": code, "error": "手机绑定变更尚未安全保存，请保留原凭据后重试", "retryable": true})
}

func (h *Hub) writeBotSnapshot(st persistedBotState) error {
	if h.cfg.DeploymentMode != "bot" || h.cfg.DataDir == "" {
		return nil
	}
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if len(b) > maxBotStateBytes {
		return ErrBotPhoneUnavailable
	}
	return writeFileAtomic(filepath.Join(h.cfg.DataDir, botGroupFile), b, 0o600)
}

type botPhoneBindInput struct {
	Binding    BotBindingPackage `json:"binding"`
	PhoneID    string            `json:"phoneId"`
	AttemptID  string            `json:"attemptId"`
	PhoneToken string            `json:"phoneToken"`
}

func decodeBotPhoneBind(r *http.Request) (botPhoneBindInput, bool) {
	var in botPhoneBindInput
	raw, err := io.ReadAll(io.LimitReader(r.Body, (16<<10)+1))
	if err != nil || len(raw) > 16<<10 {
		return in, false
	}
	fields, err := strictObjectFields(raw, []string{"binding", "phoneId", "attemptId", "phoneToken"})
	if err != nil {
		return in, false
	}
	names := []string{"binding", "phoneId"}
	if len(fields) == 4 {
		names = append(names, "attemptId", "phoneToken")
	}
	if enrollment.DecodeExact(raw, names, &in) != nil || enrollment.DecodeExact(fields["binding"], []string{"type", "version", "hubUrl", "groupId", "invite", "signature"}, &in.Binding) != nil {
		return in, false
	}
	if !validBotPackage(in.Binding) || !validHex(in.PhoneID, 32) {
		return in, false
	}
	if len(fields) == 4 && (!enrollment.RequestID(in.AttemptID) || !enrollment.Hex(in.PhoneToken, 32)) {
		return in, false
	}
	return in, true
}

func (h *Hub) handleBotPhoneBind(w http.ResponseWriter, r *http.Request, _ string) {
	if !botAnonymousHeaders(r) {
		writeCodedError(w, 400, "ambiguous_credentials", "绑定不接受其他凭据")
		return
	}
	in, ok := decodeBotPhoneBind(r)
	if !ok || r.URL.RawQuery != "" || r.URL.ForceQuery || !validateBotHubURL(in.Binding.HubURL, h.cfg.PublicURL) {
		writeCodedError(w, 400, "invalid_request", "绑定包或手机身份无效")
		return
	}
	token := in.PhoneToken
	if token == "" {
		var err error
		token, err = randomHex(32)
		if err != nil {
			writeCodedError(w, 503, "bot_phone_bind_pending", "无法生成凭据")
			return
		}
	}
	phoneHash, tokenHash := hashText(in.PhoneID), hashText(token)
	h.enrollmentMu.Lock()
	defer h.enrollmentMu.Unlock()
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	h.mu.Lock()
	g := h.botGroupForPackageLocked(in.Binding)
	if g == nil {
		h.mu.Unlock()
		writeCodedError(w, 403, "bot_credentials_invalid", "绑定包无效或已撤销")
		return
	}
	devices := h.botDevicesLocked(g)
	if g.PhoneTokenHash != "" {
		exact := in.AttemptID != "" && g.PhoneAttemptID == in.AttemptID && g.PhoneInviteHash == hashText(in.Binding.Invite) && g.PhoneIDHash == phoneHash && g.PhoneTokenHash == tokenHash
		h.mu.Unlock()
		if exact {
			writeJSON(w, 200, map[string]any{"ok": true, "groupId": g.ID, "botToken": token, "devices": devices})
		} else {
			writeCodedError(w, 409, "bot_phone_already_bound", "该组已绑定另一手机或绑定请求")
		}
		return
	}
	if in.AttemptID == "" && len(devices) == 0 {
		h.mu.Unlock()
		writeCodedError(w, 409, "bot_group_empty", "请先连接电脑或升级手机软件")
		return
	}
	// Reserve space for a future retirement before issuing any new authority.
	if len(g.RetiredPhoneTokenHashes) >= maxRetiredBotPhones {
		h.mu.Unlock()
		writeBotPhonePending(w, "bot_phone_bind_pending")
		return
	}
	for _, other := range h.botGroups {
		if other.PhoneTokenHash == tokenHash || containsBotHash(other.RetiredPhoneTokenHashes, tokenHash) {
			h.mu.Unlock()
			writeCodedError(w, 409, "bot_phone_already_bound", "手机凭据不可重复使用")
			return
		}
	}
	revision, err := randomHex(16)
	if err != nil {
		h.mu.Unlock()
		writeBotPhonePending(w, "bot_phone_bind_pending")
		return
	}
	candidate := g.botPhoneState
	candidate.PhoneIDHash, candidate.PhoneTokenHash, candidate.PhoneRevision = phoneHash, tokenHash, revision
	candidate.PhoneAttemptID = in.AttemptID
	if in.AttemptID != "" {
		candidate.PhoneInviteHash = hashText(in.Binding.Invite)
	}
	st := h.botSnapshotLocked()
	for i := range st.Groups {
		if st.Groups[i].ID == g.ID {
			st.Groups[i].botPhoneState = candidate
		}
	}
	h.mu.Unlock()
	// saveMu covers both the durable write and publication: a background save
	// cannot overwrite the committed candidate with the old live snapshot.
	if h.writeBotSnapshot(st) != nil {
		writeBotPhonePending(w, "bot_phone_bind_pending")
		return
	}
	h.mu.Lock()
	g.botPhoneState = candidate
	h.notifyBotScopeChangedLocked(g)
	devices = h.botDevicesLocked(g)
	h.mu.Unlock()
	writeJSON(w, 200, map[string]any{"ok": true, "groupId": g.ID, "botToken": token, "devices": devices})
}

// An intent changes phone/invite lifecycle only, never computer membership.
// It survives failure of the independent main-file atomic replacement.
type botPhoneIntent struct {
	GroupID          string        `json:"groupId"`
	BaseRevision     string        `json:"baseRevision"`
	Kind             string        `json:"kind"`
	Phone            botPhoneState `json:"phone"`
	InviteHash       string        `json:"inviteHash"`
	SignatureKeyHash string        `json:"signatureKeyHash,omitempty"`
	Revoked          bool          `json:"revoked"`
}
type botPhoneIntentState struct {
	Version int              `json:"version"`
	Intents []botPhoneIntent `json:"intents"`
}

func (h *Hub) writeBotPhoneIntents(exclude string) error {
	if h.cfg.DataDir == "" {
		return nil
	}
	h.mu.Lock()
	st := botPhoneIntentState{Version: 1, Intents: []botPhoneIntent{}}
	for _, g := range h.botGroups {
		if g.phoneIntent != nil && g.ID != exclude {
			st.Intents = append(st.Intents, *g.phoneIntent)
		}
	}
	h.mu.Unlock()
	b, err := json.Marshal(st)
	if err != nil {
		return err
	}
	if len(b) > maxBotStateBytes {
		return ErrBotPhoneUnavailable
	}
	return writeFileAtomic(filepath.Join(h.cfg.DataDir, botPhoneIntentFile), b, 0o600)
}

func (h *Hub) loadBotPhoneIntents() error {
	if h.cfg.DeploymentMode != "bot" || h.cfg.DataDir == "" {
		return nil
	}
	path := filepath.Join(h.cfg.DataDir, botPhoneIntentFile)
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxBotStateBytes {
		return errors.New("invalid bot phone intents")
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	var st botPhoneIntentState
	d := json.NewDecoder(bytes.NewReader(b))
	d.DisallowUnknownFields()
	if !uniqueBotJSON(b) || d.Decode(&st) != nil || d.Decode(new(any)) != io.EOF || st.Version != 1 {
		return errors.New("invalid bot phone intents")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, intent := range st.Intents {
		g := h.botGroups[intent.GroupID]
		if g == nil || g.phoneIntent != nil || (g.PhoneRevision != intent.BaseRevision && g.PhoneRevision != intent.Phone.PhoneRevision) || !enrollment.Hex(intent.BaseRevision, 16) || !validBotPhoneState(intent.Phone) || intent.Phone.PhoneTokenHash != "" || !enrollment.Hex(intent.InviteHash, 32) || intent.SignatureKeyHash != "" || (intent.Kind != "revoke" && intent.Kind != "reset") || (intent.Kind == "revoke" && !intent.Revoked) || (intent.Kind == "reset" && (intent.Revoked || intent.Phone.PhoneReset == nil)) {
			return errors.New("invalid bot phone intent transition")
		}
		if g.PhoneRevision == intent.Phone.PhoneRevision && (!reflect.DeepEqual(g.botPhoneState, intent.Phone) || g.InviteHash != intent.InviteHash || g.Revoked != intent.Revoked || g.SignatureKeyHash != intent.SignatureKeyHash) {
			return errors.New("bot phone intent disagrees with committed revision")
		}
		h.applyBotPhoneIntentLocked(g, intent)
	}
	if !h.botTokenOwnersValidLocked() {
		return errors.New("ambiguous bot phone credential")
	}
	return nil
}

func (h *Hub) applyBotPhoneIntentLocked(g *botGroup, intent botPhoneIntent) {
	g.botPhoneState = intent.Phone
	g.InviteHash, g.SignatureKeyHash, g.Revoked = intent.InviteHash, intent.SignatureKeyHash, intent.Revoked
	g.PhonePending, g.phoneIntent = true, &intent
	h.notifyBotScopeChangedLocked(g)
}

// Call with enrollmentMu + saveMu, never mu. Pending is cleared only after the
// intent has been retired durably; no later bind can be undone by stale replay.
func (h *Hub) finishBotPhoneIntent(g *botGroup) error {
	if h.writeBotPhoneIntents("") != nil || h.writeBotSnapshot(h.botSnapshot()) != nil || h.writeBotPhoneIntents(g.ID) != nil {
		return ErrBotPhoneUnavailable
	}
	h.mu.Lock()
	g.PhonePending, g.phoneIntent = false, nil
	h.mu.Unlock()
	return nil
}

func retireBotPhone(g *botGroup, revision string) botPhoneState {
	p := g.botPhoneState
	p.RetiredPhoneTokenHashes = append([]string(nil), p.RetiredPhoneTokenHashes...)
	if p.PhoneTokenHash != "" {
		p.RetiredPhoneTokenHashes = append(p.RetiredPhoneTokenHashes, p.PhoneTokenHash)
	}
	p.PhoneIDHash, p.PhoneTokenHash, p.PhoneAttemptID, p.PhoneInviteHash = "", "", "", ""
	p.PhoneRevision = revision
	return p
}

func (h *Hub) handleBotPhoneRevoke(w http.ResponseWriter, r *http.Request, _ string) {
	if !validBotOnlyHeaders(r) {
		writeCodedError(w, 400, "ambiguous_credentials", "撤销只接受手机凭据")
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, 1))
	if err != nil || len(body) != 0 || r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeCodedError(w, 400, "invalid_request", "撤销不接受附加字段")
		return
	}
	hash, tokenOK := botTokenHash(r)
	if !tokenOK {
		writeCodedError(w, 403, "bot_credentials_invalid", "手机凭据已失效")
		return
	}
	tokenHash := string(hashTextBytes(hash))
	h.enrollmentMu.Lock()
	defer h.enrollmentMu.Unlock()
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	h.mu.Lock()
	var matched *botGroup
	for _, g := range h.botGroups {
		if g.PhoneTokenHash == tokenHash || containsBotHash(g.RetiredPhoneTokenHashes, tokenHash) {
			matched = g
			break
		}
	}
	if matched == nil {
		h.mu.Unlock()
		writeCodedError(w, 403, "bot_credentials_invalid", "手机凭据已失效")
		return
	}
	g := matched
	if g.PhonePending && (g.phoneIntent == nil || g.phoneIntent.Kind != "revoke") {
		h.mu.Unlock()
		writeBotPhonePending(w, "bot_phone_revoke_pending")
		return
	}
	if !g.PhonePending && g.PhoneTokenHash != tokenHash {
		h.mu.Unlock()
		writeJSON(w, 200, map[string]any{"ok": true, "groupId": g.ID, "invitationRevoked": true})
		return
	}
	if !g.PhonePending {
		revision, err := randomHex(16)
		if err != nil {
			h.mu.Unlock()
			writeBotPhonePending(w, "bot_phone_revoke_pending")
			return
		}
		intent := botPhoneIntent{GroupID: g.ID, BaseRevision: g.PhoneRevision, Kind: "revoke", Phone: retireBotPhone(g, revision), InviteHash: g.InviteHash, Revoked: true}
		h.applyBotPhoneIntentLocked(g, intent)
	}
	h.mu.Unlock()
	if h.finishBotPhoneIntent(g) != nil {
		writeBotPhonePending(w, "bot_phone_revoke_pending")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "groupId": g.ID, "invitationRevoked": true})
}

func (h *Hub) resetBotInvite(groupID, revision, requestID string) string {
	m := hmac.New(sha256.New, h.botSigningKey)
	_, _ = m.Write([]byte("salcara-bot-phone-reset-invite-v1\x00" + groupID + "\x00" + revision + "\x00" + requestID))
	return hex.EncodeToString(m.Sum(nil))
}

func (h *Hub) ResetBotPhone(in BotPhoneReset) (BotPhoneResetResult, error) {
	var result BotPhoneResetResult
	if err := ValidateBotPhoneReset(in); err != nil {
		return result, err
	}
	if h.cfg.DeploymentMode != "bot" {
		return result, ErrBotPhoneNotFound
	}
	raw, _ := json.Marshal(in)
	fingerprint := hashText(string(raw))
	h.enrollmentMu.Lock()
	defer h.enrollmentMu.Unlock()
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	h.mu.Lock()
	g := h.botGroups[in.GroupID]
	if g == nil {
		h.mu.Unlock()
		return result, ErrBotPhoneNotFound
	}
	if replay := g.PhoneReset; replay != nil && replay.RequestID == in.RequestID {
		if replay.Fingerprint != fingerprint || replay.ResultRevision != g.PhoneRevision || g.Revoked {
			h.mu.Unlock()
			return result, ErrBotPhoneConflict
		}
	} else {
		for _, requestID := range g.PhoneResetRequests {
			if requestID == in.RequestID {
				h.mu.Unlock()
				return result, ErrBotPhoneConflict
			}
		}
		if g.PhoneRevision != in.ExpectedRevision {
			h.mu.Unlock()
			return result, ErrBotPhoneConflict
		}
		if g.PhonePending {
			h.mu.Unlock()
			return result, ErrBotPhoneUnavailable
		}
		if len(g.PhoneResetRequests) >= maxRetiredBotPhones {
			h.mu.Unlock()
			return result, ErrBotPhoneUnavailable
		}
		revision, err := randomHex(16)
		if err != nil {
			h.mu.Unlock()
			return result, ErrBotPhoneUnavailable
		}
		p := retireBotPhone(g, revision)
		p.PhoneResetRequests = append(append([]string(nil), p.PhoneResetRequests...), in.RequestID)
		p.PhoneReset = &botPhoneResetReplay{RequestID: in.RequestID, Fingerprint: fingerprint, ResultRevision: revision, HubURL: strings.TrimRight(h.cfg.PublicURL, "/")}
		intent := botPhoneIntent{GroupID: g.ID, BaseRevision: g.PhoneRevision, Kind: "reset", Phone: p, InviteHash: hashText(h.resetBotInvite(g.ID, revision, in.RequestID))}
		h.applyBotPhoneIntentLocked(g, intent)
	}
	result.PhoneRevision = g.PhoneRevision
	replay := g.PhoneReset
	result.Binding = BotBindingPackage{Type: botBindingType, Version: botBindingVersion, HubURL: replay.HubURL, GroupID: g.ID, Invite: h.resetBotInvite(g.ID, g.PhoneRevision, in.RequestID)}
	pending := g.PhonePending
	h.mu.Unlock()
	if pending {
		if err := h.finishBotPhoneIntent(g); err != nil {
			return BotPhoneResetResult{}, err
		}
	}
	result.Binding.Signature = h.signBotPackage(result.Binding)
	return result, nil
}
