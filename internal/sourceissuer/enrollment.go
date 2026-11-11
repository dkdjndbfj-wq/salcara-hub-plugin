package sourceissuer

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"salcara/hub/internal/enrollment"
)

func enrollmentFailure(w http.ResponseWriter, status int, code string) {
	reply(w, status, map[string]string{"code": code, "error": "无法完成电脑登记授权"})
}

func (s *Service) serveEnrollment(w http.ResponseWriter, r *http.Request, raw []byte) {
	var input struct {
		Version    int             `json:"version"`
		Challenge  json.RawMessage `json:"challenge"`
		Credential json.RawMessage `json:"credential"`
	}
	var credential struct {
		Type  string `json:"type"`
		Value string `json:"value"`
	}
	if exact(raw, []string{"version", "challenge", "credential"}, &input) != nil || input.Version != 1 || exact(input.Credential, []string{"type", "value"}, &credential) != nil || credential.Type != "api_key" || !validCredential(credential.Value) {
		enrollmentFailure(w, http.StatusBadRequest, "invalid_request")
		return
	}
	envelope, err := enrollment.ParseEnvelope(input.Challenge)
	if err != nil {
		enrollmentFailure(w, http.StatusBadRequest, "invalid_request")
		return
	}
	// The unverified payload selects an existing pinned trust entry only; its
	// advertised identity can never introduce a new trust anchor.
	payload, err := base64.RawURLEncoding.Strict().DecodeString(envelope.Payload)
	if err != nil {
		enrollmentFailure(w, http.StatusBadRequest, "invalid_request")
		return
	}
	challenge, err := enrollment.ParseChallenge(payload)
	if err != nil {
		enrollmentFailure(w, http.StatusForbidden, "enrollment_grant_invalid")
		return
	}
	var public ed25519.PublicKey
	for _, trust := range s.cfg.EnrollmentHubs {
		if trust.Audience == challenge.Audience && trust.HubInstanceID == challenge.HubInstanceID {
			public, _ = base64.StdEncoding.Strict().DecodeString(trust.IdentityPublicKey)
			break
		}
	}
	if challenge.Issuer != s.cfg.ID || len(public) != ed25519.PublicKeySize {
		enrollmentFailure(w, http.StatusForbidden, "enrollment_issuer_untrusted")
		return
	}
	if _, err := enrollment.VerifyEnvelope(envelope, enrollment.ChallengePurpose, public); err != nil {
		enrollmentFailure(w, http.StatusForbidden, "enrollment_grant_invalid")
		return
	}
	if !enrollment.ValidTime(challenge.IssuedAt, challenge.ExpiresAt, time.Now().Unix()) {
		enrollmentFailure(w, http.StatusForbidden, "enrollment_challenge_expired")
		return
	}
	var route *Route
	for i := range s.cfg.Routes {
		if s.cfg.Routes[i].APIBase == challenge.APIBase {
			route = &s.cfg.Routes[i]
			break
		}
	}
	eligible := false
	if route != nil && route.EnrollmentVerifierURL != "" {
		for _, group := range route.Groups {
			eligible = eligible || group.AllowDeviceEnrollment && contains(group.Audiences, challenge.Audience)
		}
	}
	if !eligible {
		enrollmentFailure(w, http.StatusForbidden, "api_enrollment_denied")
		return
	}
	if !s.allow(credential.Value, time.Now()) {
		enrollmentFailure(w, http.StatusTooManyRequests, "issuer_rate_limited")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	subject, groupID, status := s.authenticateEnrollment(ctx, route.EnrollmentVerifierURL, credential.Value)
	if status != http.StatusOK {
		code := "api_verifier_unavailable"
		if status == http.StatusUnauthorized {
			code = "api_credentials_invalid"
		}
		enrollmentFailure(w, status, code)
		return
	}
	allowed := false
	for _, group := range route.Groups {
		allowed = allowed || group.ID == groupID && group.AllowDeviceEnrollment && contains(group.Audiences, challenge.Audience)
	}
	if !allowed {
		enrollmentFailure(w, http.StatusForbidden, "api_enrollment_denied")
		return
	}
	if r.Context().Err() != nil || ctx.Err() != nil {
		enrollmentFailure(w, http.StatusServiceUnavailable, "api_verifier_unavailable")
		return
	}
	now := time.Now().Unix()
	if !enrollment.ValidTime(challenge.IssuedAt, challenge.ExpiresAt, now) {
		enrollmentFailure(w, http.StatusForbidden, "enrollment_challenge_expired")
		return
	}
	grant := enrollment.Grant{Challenge: challenge, Subject: subject, GroupID: groupID}
	grant.Purpose = enrollment.GrantPurpose
	grant.IssuedAt, grant.ExpiresAt = now, min(now+120, challenge.ExpiresAt)
	result, err := enrollment.SignEnvelope(grant, enrollment.GrantPurpose, s.key)
	if err != nil {
		enrollmentFailure(w, http.StatusInternalServerError, "issuer_failed")
		return
	}
	// Match the public wire parser before returning a proof the Hub cannot use.
	signed, err := base64.RawURLEncoding.Strict().DecodeString(result.Payload)
	if err != nil {
		enrollmentFailure(w, http.StatusInternalServerError, "issuer_failed")
		return
	}
	if _, err := enrollment.ParseGrant(signed); err != nil {
		enrollmentFailure(w, http.StatusInternalServerError, "issuer_failed")
		return
	}
	reply(w, http.StatusOK, result)
}

func (s *Service) authenticateEnrollment(ctx context.Context, endpoint, key string) (string, int64, int) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return "", 0, http.StatusServiceUnavailable
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Accept", "application/json")
	resp, err := s.http.Do(req)
	if err != nil {
		return "", 0, http.StatusServiceUnavailable
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		return "", 0, http.StatusUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return "", 0, http.StatusServiceUnavailable
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
	var identity struct {
		Version int    `json:"version"`
		Subject string `json:"subject"`
		GroupID int64  `json:"groupId"`
	}
	if err != nil || len(raw) > 4096 || exact(raw, []string{"version", "subject", "groupId"}, &identity) != nil || identity.Version != 1 || identity.GroupID <= 0 || !validEnrollmentSubject(identity.Subject) {
		return "", 0, http.StatusServiceUnavailable
	}
	return identity.Subject, identity.GroupID, http.StatusOK
}

func validEnrollmentSubject(subject string) bool {
	id, ok := strings.CutPrefix(subject, "sub2api-user:")
	if !ok {
		return false
	}
	n, err := strconv.ParseInt(id, 10, 64)
	return err == nil && n > 0 && strconv.FormatInt(n, 10) == id
}
