package hub

import (
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"time"

	"salcara/hub/internal/enrollment"
)

const maxAPIEnrollmentChallenges = 1024

type apiEnrollmentChallenge struct {
	Challenge enrollment.Challenge
	Envelope  enrollment.Envelope
}

// Stored in the same atomic devices.json record as the credential hash. No
// grant or API credential is retained or exposed in the public Device shape.
type deviceEnrollmentIdentity struct {
	Version     int    `json:"version"`
	Issuer      string `json:"issuer"`
	Subject     string `json:"subject"`
	GroupID     int64  `json:"groupId"`
	APIBase     string `json:"apiBase"`
	RequestID   string `json:"requestId"`
	ChallengeID string `json:"challengeId"`
	EnrolledAt  int64  `json:"enrolledAt"`
}

func (i *deviceEnrollmentIdentity) valid() bool {
	_, baseOK := canonicalAPIBase(i.APIBase)
	return i.Version == 1 && enrollment.Identifier(i.Issuer) && enrollment.Text(i.Subject, 256) && i.GroupID > 0 && baseOK && strings.HasPrefix(i.APIBase, "https://") && enrollment.RequestID(i.RequestID) && enrollment.Identifier(i.ChallengeID) && i.EnrolledAt > 0
}

func (i *deviceEnrollmentIdentity) subjectRef() string {
	return shaHex("salcara-enrollment-subject-v1\x00" + i.Issuer + "\x00" + i.Subject)
}

func enrollmentMetadataCost(identity *deviceEnrollmentIdentity) int64 {
	if identity == nil {
		return 0
	}
	raw, _ := json.Marshal(identity)
	return int64(256 + len(raw) + len(identity.Issuer) + len(identity.Subject) + len(identity.APIBase) + len(identity.RequestID) + len(identity.ChallengeID))
}

func enrollmentIssuer(p RemoteAccessPolicy, id, apiBase string) *SourceIssuer {
	if p.Mode == "disabled" || p.Audience == "" || !p.Station.allowsSnapshot(SourceSnapshot{SourceType: "site_api", APIBase: apiBase}) {
		return nil
	}
	for _, issuer := range p.Issuers {
		if issuer.ID != id || !issuer.DeviceEnrollment || issuer.EnrollmentURL == "" {
			continue
		}
		site := false
		for _, source := range issuer.SourceTypes {
			site = site || source == "site_api"
		}
		for _, base := range issuer.APIBases {
			if site && base == apiBase {
				return &issuer
			}
		}
	}
	return nil
}

func (h *Hub) apiEnrollmentDiscovery() any {
	identity := h.IngressState()
	if h.cfg.DeploymentMode != "station" || h.cfg.DataDir == "" || !identity.Configured {
		return nil
	}
	p := h.CurrentRemoteAccessPolicy()
	issuers := []SourceIssuer{}
	for _, issuer := range p.Issuers {
		for _, base := range issuer.APIBases {
			if enrollmentIssuer(p, issuer.ID, base) != nil {
				issuers = append(issuers, issuer)
				break
			}
		}
	}
	if len(issuers) == 0 {
		return nil
	}
	return map[string]any{"version": 1, "hubInstanceId": identity.HubInstanceID, "identityPublicKey": identity.IdentityPublicKey, "audience": p.Audience, "policyRevision": p.Revision, "issuers": issuers}
}

func (h *Hub) sweepAPIEnrollmentChallengesLocked(now int64) {
	for id, challenge := range h.apiEnrollmentChallenges {
		if challenge.Challenge.ExpiresAt <= now {
			delete(h.apiEnrollmentChallenges, id)
		}
	}
}

// A challenge authorizes only a fresh enrollment attempt, never recovery or
// replacement of an existing credential. Check every namespace: a legacy
// record with this ID must not be bypassed by a new device-only account.
// Callers hold enrollmentMu and mu, in that order.
func (h *Hub) newAPIEnrollmentTargetLocked(deviceID string) (int, string) {
	if h.userDisabledLocked(deviceID) {
		return http.StatusLocked, "user_disabled"
	}
	if h.bannedDeviceIDs[deviceID] {
		return http.StatusForbidden, "device_credentials_invalid"
	}
	for _, account := range h.accounts {
		if dev := account.devices[deviceID]; dev != nil {
			if dev.banned {
				return http.StatusForbidden, "device_credentials_invalid"
			}
			return http.StatusConflict, "enrollment_request_conflict"
		}
	}
	return 0, ""
}

func readEnrollmentJSON(w http.ResponseWriter, r *http.Request, limit int64) ([]byte, bool) {
	media, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || media != "application/json" || r.Header.Get("Content-Encoding") != "" || r.URL.RawQuery != "" || r.URL.ForceQuery {
		writeCodedError(w, 400, "invalid_request", "登记请求格式无效")
		return nil, false
	}
	controller := http.NewResponseController(w)
	_ = controller.SetReadDeadline(time.Now().Add(8 * time.Second))
	defer controller.SetReadDeadline(time.Time{}) //nolint:errcheck
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		_ = r.Body.Close()
		w.Header().Set("Connection", "close")
		writeCodedError(w, 400, "invalid_request", "登记请求过大或未完整接收")
		return nil, false
	}
	return raw, true
}

func (h *Hub) handleAPIEnrollmentChallenge(w http.ResponseWriter, r *http.Request, _ string) {
	if h.apiEnrollmentDiscovery() == nil {
		writeCodedError(w, 404, "api_enrollment_unsupported", "本站尚未配置 API 自动登记")
		return
	}
	for _, name := range []string{"Authorization", "Cookie", "X-Salcara-Device-Secret", "X-Salcara-Enrollment-Grant", "X-Salcara-Enrollment-Claim", "X-Salcara-Pair-Token", "X-Salcara-Bot-Token", "X-Salcara-Bot-Invite"} {
		if len(r.Header.Values(name)) != 0 {
			writeCodedError(w, 400, "ambiguous_credentials", "登记挑战不接收登录凭据")
			return
		}
	}
	ip := clientIP(r, h.cfg.TrustProxy)
	if h.apiEnrollmentLimiter.blocked(ip) {
		writeCodedError(w, 429, "remote_rate_limited", "登记请求过于频繁，请稍后重试")
		return
	}
	h.apiEnrollmentLimiter.fail(ip)
	raw, ok := readEnrollmentJSON(w, r, 8<<10)
	if !ok {
		return
	}
	var input struct {
		Version          int    `json:"version"`
		Issuer           string `json:"issuer"`
		APIBase          string `json:"apiBase"`
		DeviceID         string `json:"deviceId"`
		DeviceSecretHash string `json:"deviceSecretHash"`
		RequestID        string `json:"requestId"`
	}
	if enrollment.DecodeExact(raw, []string{"version", "issuer", "apiBase", "deviceId", "deviceSecretHash", "requestId"}, &input) != nil || input.Version != 1 || !enrollment.Identifier(input.Issuer) || !validID(input.DeviceID) || !enrollment.Hex(input.DeviceSecretHash, 32) || !enrollment.RequestID(input.RequestID) {
		writeCodedError(w, 400, "invalid_request", "登记挑战字段无效")
		return
	}
	h.remoteAccessGate.RLock()
	defer h.remoteAccessGate.RUnlock()
	// An API registration writes its snapshot before publishing the live device.
	// Wait for that transaction before treating a missing live record as absent.
	h.enrollmentMu.Lock()
	defer h.enrollmentMu.Unlock()
	identity := h.IngressState()
	h.mu.Lock()
	defer h.mu.Unlock()
	p := h.cfg.RemoteAccess
	if !identity.Configured || enrollmentIssuer(p, input.Issuer, input.APIBase) == nil {
		writeCodedError(w, 403, "enrollment_issuer_untrusted", "本站未允许此 API 签发登记授权")
		return
	}
	if status, code := h.newAPIEnrollmentTargetLocked(input.DeviceID); status != 0 {
		if code == "user_disabled" {
			writeManagedUserDisabled(w)
		} else {
			writeCodedError(w, status, code, "电脑身份已存在或不可重新登记，请确认原连接状态")
		}
		return
	}
	now := time.Now().Unix()
	h.sweepAPIEnrollmentChallengesLocked(now)
	for _, pending := range h.apiEnrollmentChallenges {
		c := pending.Challenge
		if c.RequestID == input.RequestID {
			if c.DeviceID == input.DeviceID && c.DeviceSecretHash == input.DeviceSecretHash && c.Issuer == input.Issuer && c.APIBase == input.APIBase && c.PolicyRevision == p.Revision {
				writeJSON(w, 200, pending.Envelope)
			} else {
				writeCodedError(w, 409, "enrollment_request_conflict", "本次登记内容已变化，请重新发起")
			}
			return
		}
	}
	for _, a := range h.accounts {
		for _, d := range a.devices {
			if d.enrollment != nil && d.enrollment.RequestID == input.RequestID {
				writeCodedError(w, 409, "enrollment_request_conflict", "登记已提交，请使用原电脑身份确认结果")
				return
			}
		}
	}
	if len(h.apiEnrollmentChallenges) >= maxAPIEnrollmentChallenges {
		writeCodedError(w, 429, "remote_rate_limited", "登记服务繁忙，请稍后重试")
		return
	}
	if r.Context().Err() != nil {
		writeCodedError(w, 503, "remote_unavailable", "本次登记已取消")
		return
	}
	c := enrollment.Challenge{Purpose: enrollment.ChallengePurpose, Version: 1, Issuer: input.Issuer, HubInstanceID: identity.HubInstanceID, Audience: p.Audience, APIBase: input.APIBase, PolicyRevision: p.Revision, DeviceID: input.DeviceID, DeviceSecretHash: input.DeviceSecretHash, ChallengeID: "enrollment-" + ingressHex(32), RequestID: input.RequestID, IssuedAt: now, ExpiresAt: now + 120}
	h.ingress.mu.Lock()
	envelope, err := enrollment.SignEnvelope(c, enrollment.ChallengePurpose, h.ingress.key)
	h.ingress.mu.Unlock()
	if err != nil || len(c.ChallengeID) != len("enrollment-")+64 {
		writeCodedError(w, 503, "remote_unavailable", "无法创建登记挑战")
		return
	}
	h.apiEnrollmentChallenges[c.ChallengeID] = apiEnrollmentChallenge{c, envelope}
	writeJSON(w, 200, envelope)
}

func parseEnrollmentHeader(value string) (enrollment.Envelope, enrollment.Grant, error) {
	if value == "" || len(value) > enrollment.MaxHeaderBytes {
		return enrollment.Envelope{}, enrollment.Grant{}, enrollment.ErrInvalid
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(value)
	if err != nil {
		return enrollment.Envelope{}, enrollment.Grant{}, enrollment.ErrInvalid
	}
	envelope, err := enrollment.ParseEnvelope(raw)
	if err != nil {
		return envelope, enrollment.Grant{}, err
	}
	payload, _ := base64.RawURLEncoding.Strict().DecodeString(envelope.Payload)
	grant, err := enrollment.ParseGrant(payload)
	return envelope, grant, err
}

func challengeMatchesGrant(c enrollment.Challenge, g enrollment.Grant) bool {
	return c.Issuer == g.Issuer && c.HubInstanceID == g.HubInstanceID && c.Audience == g.Audience && c.APIBase == g.APIBase && c.PolicyRevision == g.PolicyRevision && c.DeviceID == g.DeviceID && c.DeviceSecretHash == g.DeviceSecretHash && c.ChallengeID == g.ChallengeID && c.RequestID == g.RequestID && g.IssuedAt >= c.IssuedAt-5 && g.ExpiresAt <= c.ExpiresAt
}

// The new identity is not visible in RAM until its credential, external subject
// and used request/challenge have committed in one devices.json replacement.
func (h *Hub) handleAPIEnrollmentRegister(w http.ResponseWriter, r *http.Request, info Device, secretHash [32]byte) {
	if h.cfg.DeploymentMode != "station" || h.cfg.DataDir == "" {
		writeCodedError(w, 403, "enrollment_issuer_untrusted", "本站不接受 API 登记授权")
		return
	}
	envelope, grant, err := parseEnrollmentHeader(r.Header.Get("X-Salcara-Enrollment-Grant"))
	if err != nil || grant.DeviceID != info.DeviceID || grant.DeviceSecretHash != hex.EncodeToString(secretHash[:]) {
		writeCodedError(w, 403, "enrollment_grant_invalid", "登记授权与本电脑不符")
		return
	}
	if !enrollment.ValidTime(grant.IssuedAt, grant.ExpiresAt, time.Now().Unix()) {
		writeCodedError(w, 403, "enrollment_challenge_expired", "登记授权已过期，请重新连接")
		return
	}
	h.remoteAccessGate.RLock()
	defer h.remoteAccessGate.RUnlock()
	h.enrollmentMu.Lock()
	defer h.enrollmentMu.Unlock()
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	identity := h.IngressState()
	h.mu.Lock()
	p := h.cfg.RemoteAccess
	issuer := enrollmentIssuer(p, grant.Issuer, grant.APIBase)
	if issuer == nil {
		h.mu.Unlock()
		writeCodedError(w, 403, "enrollment_issuer_untrusted", "API 登记授权未获本站信任")
		return
	}
	if _, err = enrollment.VerifyEnvelope(envelope, enrollment.GrantPurpose, ed25519.PublicKey(issuer.key)); err != nil || !identity.Configured || grant.HubInstanceID != identity.HubInstanceID || grant.Audience != p.Audience || grant.PolicyRevision != p.Revision {
		h.mu.Unlock()
		writeCodedError(w, 403, "enrollment_grant_invalid", "登记授权已失效或不属于本站")
		return
	}
	accountID := deviceAccount(info.DeviceID)
	if h.userDisabledLocked(info.DeviceID) {
		h.mu.Unlock()
		writeManagedUserDisabled(w)
		return
	}
	if a := h.accounts[accountID]; a != nil && a.devices[info.DeviceID] != nil {
		d := a.devices[info.DeviceID]
		matches := h.matchesReadyDeviceSecret(d, r) && !d.banned && !h.bannedDeviceIDs[info.DeviceID] && d.enrollment != nil && d.enrollment.Issuer == grant.Issuer && d.enrollment.Subject == grant.Subject && d.enrollment.RequestID == grant.RequestID && d.enrollment.ChallengeID == grant.ChallengeID
		status := d.status()
		h.mu.Unlock()
		if !matches {
			writeCodedError(w, 409, "enrollment_request_conflict", "电脑已登记，不能覆盖原身份或归属")
			return
		}
		writeJSON(w, 200, map[string]any{"ok": true, "account": accountID, "device": status, "eventScopeRequired": false, "eventScope": h.bridgeEventScope(accountID, info.DeviceID)})
		return
	}
	// Another registration may have won since challenge issuance. A signed
	// challenge is not a reservation and cannot replace that winning identity.
	if status, code := h.newAPIEnrollmentTargetLocked(info.DeviceID); status != 0 {
		h.mu.Unlock()
		if code == "user_disabled" {
			writeManagedUserDisabled(w)
		} else {
			writeCodedError(w, status, code, "电脑身份已存在或不可重新登记，请确认原连接状态")
		}
		return
	}
	pending, found := h.apiEnrollmentChallenges[grant.ChallengeID]
	if !found || !enrollment.ValidTime(pending.Challenge.IssuedAt, pending.Challenge.ExpiresAt, time.Now().Unix()) {
		h.mu.Unlock()
		writeCodedError(w, 403, "enrollment_challenge_expired", "登记挑战已过期或已使用")
		return
	}
	if !challengeMatchesGrant(pending.Challenge, grant) {
		h.mu.Unlock()
		writeCodedError(w, 403, "enrollment_grant_invalid", "登记授权上下文不一致")
		return
	}
	ip := clientIP(r, h.cfg.TrustProxy)
	if h.enrollLimiter.blocked(ip) {
		h.mu.Unlock()
		writeCodedError(w, 429, "remote_rate_limited", "设备登记过于频繁")
		return
	}
	if status := h.preflightNewDeviceLocked(accountID, info); status != 0 {
		h.mu.Unlock()
		writeCodedError(w, status, deviceRegistrationFailureCode(status), "设备已停用或达到本站容量")
		return
	}
	for _, a := range h.accounts {
		for _, d := range a.devices {
			if d.enrollment != nil && d.enrollment.RequestID == grant.RequestID {
				h.mu.Unlock()
				writeCodedError(w, 409, "enrollment_request_conflict", "登记请求已用于其他电脑")
				return
			}
		}
	}
	owner, code := h.enrollmentSubjectOwnerLocked(grant.Issuer, grant.Subject)
	if code != "" {
		h.mu.Unlock()
		if code == "user_disabled" {
			writeManagedUserDisabled(w)
		} else {
			writeCodedError(w, 409, code, "来源账号归属存在冲突")
		}
		return
	}
	if old := h.managedUsers.Devices[info.DeviceID].UserID; old != "" && old != owner {
		h.mu.Unlock()
		writeCodedError(w, 409, "enrollment_request_conflict", "电脑已有其他用户归属")
		return
	}
	if owner != "" {
		user := h.managedUserLocked(owner)
		if user != nil && user.Station != nil && !user.Station.allowsSnapshot(SourceSnapshot{SourceType: "site_api", APIBase: grant.APIBase}) {
			h.mu.Unlock()
			writeCodedError(w, 403, "api_enrollment_denied", "该用户未获此 API 来源授权")
			return
		}
		if err := h.assignEnrolledUserLocked(info.DeviceID, owner); err != nil {
			h.mu.Unlock()
			writeCodedError(w, 503, "remote_unavailable", "电脑归属尚未安全保存")
			return
		}
	}
	if r.Context().Err() != nil {
		h.mu.Unlock()
		writeCodedError(w, 503, "remote_unavailable", "本次登记已取消")
		return
	}
	if info.Name == "" {
		info.Name = "我的电脑"
	}
	stored := &deviceEnrollmentIdentity{Version: 1, Issuer: grant.Issuer, Subject: grant.Subject, GroupID: grant.GroupID, APIBase: grant.APIBase, RequestID: grant.RequestID, ChallengeID: grant.ChallengeID, EnrolledAt: time.Now().Unix()}
	_, metadataCharge, _ := prepareDeviceMetadata(info)
	extraCharge := enrollmentMetadataCost(stored)
	projected := h.deviceMetadataBytes + metadataCharge + extraCharge
	if account := h.accounts[accountID]; account == nil || !account.metadataCharged {
		projected += accountMetadataCost(accountID)
	}
	if projected > h.deviceMetadataLimit {
		h.mu.Unlock()
		writeCodedError(w, 429, "device_capacity_exceeded", "本站设备存储容量已满")
		return
	}
	snapshot := h.snapshotLocked()
	snapshot.Accounts[accountID] = []persistedDevice{{Device: info, LastSeen: nowMs(), SecretHash: hex.EncodeToString(secretHash[:]), Enrollment: stored}}
	h.mu.Unlock()
	if err := h.saveSnapshotError(snapshot); err != nil {
		writeCodedError(w, 503, "remote_unavailable", "设备登记尚未安全保存，请稍后确认结果")
		return
	}
	h.mu.Lock()
	status, registerStatus, _ := h.registerDeviceLocked(accountID, info, secretHash)
	if registerStatus == 0 {
		d := h.accounts[accountID].devices[info.DeviceID]
		d.enrollment = stored
		d.metadataBytes += extraCharge
		h.deviceMetadataBytes += extraCharge
		d.savedRegistrationEpoch = d.registrationEpoch
		delete(h.apiEnrollmentChallenges, grant.ChallengeID)
		h.enrollLimiter.fail(ip)
	}
	h.mu.Unlock()
	if registerStatus != 0 {
		writeCodedError(w, 503, "remote_unavailable", "设备状态需要重新确认")
		return
	}
	writeJSON(w, 200, map[string]any{"ok": true, "account": accountID, "device": status, "eventScopeRequired": false, "eventScope": h.bridgeEventScope(accountID, info.DeviceID)})
}

func (h *Hub) enrollmentSubjectOwnerLocked(issuer, subject string) (string, string) {
	identity := &deviceEnrollmentIdentity{Issuer: issuer, Subject: subject}
	owner, known := h.managedUsers.EnrollmentSubjects[identity.subjectRef()]
	if known {
		if owner != "" {
			if user := h.managedUserLocked(owner); user == nil || user.Disabled {
				return "", "user_disabled"
			}
		}
		return owner, ""
	}
	for _, a := range h.accounts {
		for _, d := range a.devices {
			if d.enrollment != nil && d.enrollment.Issuer == issuer && d.enrollment.Subject == subject {
				candidate := h.managedUsers.Devices[d.info.DeviceID].UserID
				if candidate != "" && owner != "" && candidate != owner {
					return "", "enrollment_request_conflict"
				}
				if candidate != "" {
					owner = candidate
				}
			}
		}
	}
	if owner != "" {
		if user := h.managedUserLocked(owner); user == nil || user.Disabled {
			return "", "user_disabled"
		}
	}
	return owner, ""
}

// Confirm every pending identity generation synchronously, including a prior
// 503 whose in-memory device exists. A group mutation after this snapshot stays
// pending rather than being acknowledged by an older concurrent save.
func (h *Hub) confirmDeviceRegistration(accountID, deviceID string) error {
	if h.cfg.DataDir == "" {
		return nil
	}
	h.mu.Lock()
	a := h.accounts[accountID]
	if a == nil || a.devices[deviceID] == nil {
		h.mu.Unlock()
		return errors.New("device is missing")
	}
	d := a.devices[deviceID]
	epoch := d.registrationEpoch
	needed := epoch > d.savedRegistrationEpoch
	if needed && h.cfg.DeploymentMode == "bot" {
		member := false
		for _, group := range h.botGroups {
			if !group.Revoked && group.Computers[deviceID] {
				member = true
				break
			}
		}
		if !member {
			h.mu.Unlock()
			return errors.New("pending Bot identity requires its original invitation to restore group membership")
		}
	}
	h.mu.Unlock()
	if !needed {
		return nil
	}
	if h.cfg.DeploymentMode == "bot" {
		if err := h.saveBotStateError(); err != nil {
			return err
		}
	}
	// Other saver activity may persist a pending identity, but must never turn
	// it into a confirmed one after restart. Only this ordered commit clears
	// the marker, after Bot membership (when present) is safely on disk.
	h.saveMu.Lock()
	defer h.saveMu.Unlock()
	h.mu.Lock()
	current := h.accounts[accountID]
	if current == nil || current.devices[deviceID] != d || d.registrationEpoch != epoch {
		h.mu.Unlock()
		return errors.New("device registration changed")
	}
	snapshot := h.snapshotLocked()
	for i := range snapshot.Accounts[accountID] {
		if snapshot.Accounts[accountID][i].Device.DeviceID == deviceID {
			snapshot.Accounts[accountID][i].RegistrationPending = false
		}
	}
	h.mu.Unlock()
	if err := h.saveSnapshotError(snapshot); err != nil {
		return err
	}
	h.mu.Lock()
	if current := h.accounts[accountID]; current != nil && current.devices[deviceID] == d {
		d.savedRegistrationEpoch = max(d.savedRegistrationEpoch, epoch)
	}
	h.mu.Unlock()
	return nil
}

// Caller holds mu. Ephemeral test/embedded Hubs have no durability boundary.
func (h *Hub) deviceRegistrationPendingLocked(d *device) bool {
	return d != nil && h.cfg.DataDir != "" && d.registrationEpoch > d.savedRegistrationEpoch
}

// Validate the header before routing: one proof cannot be mixed with an invite,
// a bearer key, a phone token or a duplicate independent device identity.
func validateEnrollmentCredentials(w http.ResponseWriter, r *http.Request) bool {
	if len(r.Header.Values("X-Salcara-Enrollment-Grant")) == 0 {
		return true
	}
	for _, name := range []string{"X-Salcara-Enrollment-Grant", "X-Salcara-Device-Id", "X-Salcara-Device-Secret"} {
		if len(r.Header.Values(name)) != 1 || r.Header.Get(name) == "" {
			writeCodedError(w, 400, "ambiguous_credentials", "请只使用一组电脑登记凭据")
			return false
		}
	}
	for _, name := range []string{"Authorization", "X-Salcara-Enrollment-Claim", "X-Salcara-Bot-Invite", "X-Salcara-Bot-Group", "X-Salcara-Bot-Signature", "X-Salcara-Bot-Token", "X-Salcara-Pair-Token"} {
		if len(r.Header.Values(name)) > 0 {
			writeCodedError(w, 400, "ambiguous_credentials", "API 登记授权不能与其他凭据混用")
			return false
		}
	}
	return true
}

// Kept out of all success/error responses; encoding is only used by tests and
// consumers to build the bounded header, never by persistent device storage.
func enrollmentGrantHeader(envelope enrollment.Envelope) string {
	raw, _ := json.Marshal(envelope)
	return base64.RawURLEncoding.EncodeToString(raw)
}
