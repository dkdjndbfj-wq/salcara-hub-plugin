package hub

import (
	"bytes"
	"context"
	"crypto"
	"crypto/rand"
	"crypto/rsa"
	"crypto/sha256"
	"crypto/x509"
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

// Firebase Cloud Messaging (HTTP v1) with a Google service account, standard
// library only: a signed JWT is exchanged for a one-hour OAuth token, which is
// cached and reused for every message.

const (
	fcmScope       = "https://www.googleapis.com/auth/firebase.messaging"
	googleTokenURL = "https://oauth2.googleapis.com/token"
	fcmEndpoint    = "https://fcm.googleapis.com"
	maxFCMResponse = 64 << 10
)

var (
	// errPushTokenGone: FCM says this phone's token is no longer valid (app removed or data cleared).
	errPushTokenGone = errors.New("push token no longer registered")
)

type fcmSender struct {
	projectID string
	email     string
	key       *rsa.PrivateKey
	client    *http.Client
	tokenURL  string // fixed to Google; replaced only by tests
	endpoint  string

	mu     sync.Mutex
	access string
	expiry time.Time
}

type serviceAccount struct {
	Type        string `json:"type"`
	ProjectID   string `json:"project_id"`
	PrivateKey  string `json:"private_key"`
	ClientEmail string `json:"client_email"`
}

// newFCMSender parses a Firebase service-account JSON key file.
func newFCMSender(raw []byte, client *http.Client) (*fcmSender, error) {
	if len(raw) == 0 || len(raw) > 64<<10 {
		return nil, errors.New("FCM service account file is empty or too large")
	}
	var sa serviceAccount
	if err := json.Unmarshal(raw, &sa); err != nil {
		return nil, errors.New("FCM service account file is not valid JSON")
	}
	if sa.Type != "service_account" || sa.ProjectID == "" || sa.ClientEmail == "" || sa.PrivateKey == "" {
		return nil, errors.New("FCM service account file must be a Firebase service account key (type service_account)")
	}
	if !validProjectID(sa.ProjectID) {
		return nil, errors.New("FCM service account has an invalid project_id")
	}
	block, _ := pem.Decode([]byte(sa.PrivateKey))
	if block == nil {
		return nil, errors.New("FCM service account private_key is not PEM")
	}
	parsed, err := x509.ParsePKCS8PrivateKey(block.Bytes)
	if err != nil {
		if rsaKey, err2 := x509.ParsePKCS1PrivateKey(block.Bytes); err2 == nil {
			parsed = rsaKey
		} else {
			return nil, errors.New("FCM service account private_key cannot be parsed")
		}
	}
	key, ok := parsed.(*rsa.PrivateKey)
	if !ok {
		return nil, errors.New("FCM service account private_key must be RSA")
	}
	if client == nil {
		client = &http.Client{}
	}
	return &fcmSender{projectID: sa.ProjectID, email: sa.ClientEmail, key: key, client: client, tokenURL: googleTokenURL, endpoint: fcmEndpoint}, nil
}

func validProjectID(id string) bool {
	if len(id) < 4 || len(id) > 64 {
		return false
	}
	for _, c := range id {
		if !(c >= 'a' && c <= 'z' || c >= '0' && c <= '9' || c == '-') {
			return false
		}
	}
	return true
}

func b64(data []byte) string { return base64.RawURLEncoding.EncodeToString(data) }

func (s *fcmSender) signedJWT(now time.Time) (string, error) {
	header := b64([]byte(`{"alg":"RS256","typ":"JWT"}`))
	claims, _ := json.Marshal(map[string]any{
		"iss": s.email, "scope": fcmScope, "aud": s.tokenURL,
		"iat": now.Unix(), "exp": now.Add(time.Hour).Unix(),
	})
	unsigned := header + "." + b64(claims)
	digest := sha256.Sum256([]byte(unsigned))
	sig, err := rsa.SignPKCS1v15(rand.Reader, s.key, crypto.SHA256, digest[:])
	if err != nil {
		return "", err
	}
	return unsigned + "." + b64(sig), nil
}

func (s *fcmSender) accessToken(ctx context.Context) (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now()
	if s.access != "" && now.Before(s.expiry) {
		return s.access, nil
	}
	assertion, err := s.signedJWT(now)
	if err != nil {
		return "", err
	}
	form := url.Values{"grant_type": {"urn:ietf:params:oauth:grant-type:jwt-bearer"}, "assertion": {assertion}}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := s.client.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxFCMResponse))
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("google oauth: HTTP %d", resp.StatusCode)
	}
	var out struct {
		AccessToken string `json:"access_token"`
		ExpiresIn   int64  `json:"expires_in"`
	}
	if json.Unmarshal(body, &out) != nil || out.AccessToken == "" {
		return "", errors.New("google oauth: no access token")
	}
	if out.ExpiresIn <= 0 || out.ExpiresIn > 3600 {
		out.ExpiresIn = 3600
	}
	s.access = out.AccessToken
	s.expiry = now.Add(time.Duration(out.ExpiresIn-60) * time.Second)
	return s.access, nil
}

// send delivers one high-priority data message. Returns errPushTokenGone when
// FCM reports the phone's token as unregistered or invalid.
func (s *fcmSender) send(ctx context.Context, token string, data map[string]string, collapse string) error {
	access, err := s.accessToken(ctx)
	if err != nil {
		return err
	}
	android := map[string]any{"priority": "HIGH", "ttl": "3600s"}
	if collapse != "" {
		android["collapse_key"] = collapse
	}
	payload, _ := json.Marshal(map[string]any{"message": map[string]any{"token": token, "data": data, "android": android}})
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.endpoint+"/v1/projects/"+s.projectID+"/messages:send", bytes.NewReader(payload))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+access)
	resp, err := s.client.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, maxFCMResponse))
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode == http.StatusUnauthorized {
		s.mu.Lock()
		s.access = "" // refresh next time
		s.mu.Unlock()
	}
	var failure struct {
		Error struct {
			Status  string `json:"status"`
			Details []struct {
				ErrorCode string `json:"errorCode"`
			} `json:"details"`
		} `json:"error"`
	}
	_ = json.Unmarshal(body, &failure)
	for _, d := range failure.Error.Details {
		if d.ErrorCode == "UNREGISTERED" || d.ErrorCode == "INVALID_ARGUMENT" && resp.StatusCode == http.StatusBadRequest {
			return errPushTokenGone
		}
	}
	if resp.StatusCode == http.StatusNotFound {
		return errPushTokenGone
	}
	return fmt.Errorf("fcm: HTTP %d %s", resp.StatusCode, failure.Error.Status)
}
