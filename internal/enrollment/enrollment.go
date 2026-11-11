// Package enrollment defines the purpose-separated wire format shared by the
// Hub and its explicitly trusted station issuer. It never handles API keys.
package enrollment

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

const (
	ChallengePurpose = "salcara-device-enrollment-challenge-v1"
	GrantPurpose     = "salcara-device-enrollment-v1"
	MaxPayloadBytes  = 8 << 10
	MaxEnvelopeBytes = 12 << 10
	MaxHeaderBytes   = 16 << 10
)

var ErrInvalid = errors.New("invalid device enrollment proof")
var requestID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

type Envelope struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

type Challenge struct {
	Purpose          string `json:"purpose"`
	Version          int    `json:"version"`
	Issuer           string `json:"issuer"`
	HubInstanceID    string `json:"hubInstanceId"`
	Audience         string `json:"audience"`
	APIBase          string `json:"apiBase"`
	PolicyRevision   string `json:"policyRevision"`
	DeviceID         string `json:"deviceId"`
	DeviceSecretHash string `json:"deviceSecretHash"`
	ChallengeID      string `json:"challengeId"`
	RequestID        string `json:"requestId"`
	IssuedAt         int64  `json:"issuedAt"`
	ExpiresAt        int64  `json:"expiresAt"`
}

type Grant struct {
	Challenge
	Subject string `json:"subject"`
	GroupID int64  `json:"groupId"`
}

var challengeFields = []string{"purpose", "version", "issuer", "hubInstanceId", "audience", "apiBase", "policyRevision", "deviceId", "deviceSecretHash", "challengeId", "requestId", "issuedAt", "expiresAt"}

// DecodeExact requires every field, with exact case, once, non-null, and no
// trailing data. Nested protocol objects are parsed separately at their boundary.
func DecodeExact(raw []byte, fields []string, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	token, err := d.Token()
	if err != nil || token != json.Delim('{') {
		return ErrInvalid
	}
	allowed, seen := map[string]bool{}, map[string]bool{}
	for _, field := range fields {
		allowed[field] = true
	}
	for d.More() {
		token, err = d.Token()
		name, ok := token.(string)
		if err != nil || !ok || !allowed[name] || seen[name] {
			return ErrInvalid
		}
		seen[name] = true
		var value json.RawMessage
		if d.Decode(&value) != nil || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return ErrInvalid
		}
	}
	if token, err = d.Token(); err != nil || token != json.Delim('}') || d.Decode(new(any)) != io.EOF || len(seen) != len(allowed) || json.Unmarshal(raw, target) != nil {
		return ErrInvalid
	}
	return nil
}

func Identifier(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, ch := range value {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.') {
			return false
		}
	}
	return true
}

func Text(value string, maximum int) bool {
	return value != "" && len(value) <= maximum && utf8.ValidString(value) && strings.IndexFunc(value, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) < 0
}

func Hex(value string, size int) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == size && value == strings.ToLower(value)
}

func RequestID(value string) bool { return requestID.MatchString(value) }

func validChallenge(c Challenge) bool {
	u, err := url.Parse(c.APIBase)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" || u.String() != c.APIBase || len(c.APIBase) > 4096 {
		return false
	}
	return c.Version == 1 && Identifier(c.Issuer) && Hex(c.HubInstanceID, 16) && Identifier(c.Audience) && Identifier(c.PolicyRevision) && Text(c.DeviceID, 128) && Hex(c.DeviceSecretHash, 32) && Identifier(c.ChallengeID) && RequestID(c.RequestID) && c.IssuedAt > 0 && c.ExpiresAt > c.IssuedAt && c.ExpiresAt-c.IssuedAt <= 120
}

// All enrollment timestamps are Unix seconds, deliberately distinct from the
// older ingress manifest's millisecond timestamps. Permit only 5s clock skew.
func ValidTime(issuedAt, expiresAt, now int64) bool {
	return issuedAt > 0 && issuedAt <= now+5 && expiresAt > now && expiresAt > issuedAt && expiresAt-issuedAt <= 120
}

func ParseEnvelope(raw []byte) (Envelope, error) {
	var envelope Envelope
	if len(raw) > MaxEnvelopeBytes || DecodeExact(raw, []string{"payload", "signature"}, &envelope) != nil {
		return Envelope{}, ErrInvalid
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(envelope.Payload)
	signature, sigErr := base64.RawURLEncoding.Strict().DecodeString(envelope.Signature)
	if err != nil || sigErr != nil || len(payload) == 0 || len(payload) > MaxPayloadBytes || len(signature) != ed25519.SignatureSize {
		return Envelope{}, ErrInvalid
	}
	return envelope, nil
}

func ParseChallenge(raw []byte) (Challenge, error) {
	var c Challenge
	if len(raw) > MaxPayloadBytes || DecodeExact(raw, challengeFields, &c) != nil || c.Purpose != ChallengePurpose || !validChallenge(c) {
		return Challenge{}, ErrInvalid
	}
	return c, nil
}

func ParseGrant(raw []byte) (Grant, error) {
	var g Grant
	fields := append(append([]string(nil), challengeFields...), "subject", "groupId")
	if len(raw) > MaxPayloadBytes || DecodeExact(raw, fields, &g) != nil || g.Purpose != GrantPurpose || !validChallenge(g.Challenge) || !Text(g.Subject, 256) || g.GroupID <= 0 {
		return Grant{}, ErrInvalid
	}
	return g, nil
}

func VerifyEnvelope(envelope Envelope, purpose string, key ed25519.PublicKey) ([]byte, error) {
	if len(key) != ed25519.PublicKeySize || (purpose != ChallengePurpose && purpose != GrantPurpose) {
		return nil, ErrInvalid
	}
	raw, err := base64.RawURLEncoding.Strict().DecodeString(envelope.Payload)
	sig, sigErr := base64.RawURLEncoding.Strict().DecodeString(envelope.Signature)
	if err != nil || sigErr != nil || len(raw) == 0 || len(raw) > MaxPayloadBytes || len(sig) != ed25519.SignatureSize || !ed25519.Verify(key, append([]byte(purpose+"\n"), raw...), sig) {
		return nil, ErrInvalid
	}
	return raw, nil
}

func SignEnvelope(value any, purpose string, key ed25519.PrivateKey) (Envelope, error) {
	if len(key) != ed25519.PrivateKeySize || (purpose != ChallengePurpose && purpose != GrantPurpose) {
		return Envelope{}, ErrInvalid
	}
	raw, err := json.Marshal(value)
	if err != nil || len(raw) == 0 || len(raw) > MaxPayloadBytes {
		return Envelope{}, ErrInvalid
	}
	sig := ed25519.Sign(key, append([]byte(purpose+"\n"), raw...))
	return Envelope{Payload: base64.RawURLEncoding.EncodeToString(raw), Signature: base64.RawURLEncoding.EncodeToString(sig)}, nil
}
