package hub

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/url"
	"reflect"
	"strconv"
	"strings"
	"time"
)

// SourceIssuer is an operator-configured trust anchor. It contains no secret.
type SourceIssuer struct {
	ID          string   `json:"id"`
	PublicKey   string   `json:"publicKey"`
	SourceTypes []string `json:"sourceTypes"`
	AgentIDs    []string `json:"agentIds,omitempty"`
	// Empty scopes retain legacy operator trust. New issuers should register
	// both the origin and the complete normalized upstream API base.
	Origins          []string `json:"origins,omitempty"`
	APIBases         []string `json:"apiBases,omitempty"`
	AuthorizationURL string   `json:"authorizationUrl,omitempty"`
	key              ed25519.PublicKey
}

// RemoteAccessPolicy deliberately does not reuse ResourceMode. Pairing and
// resource limits are orthogonal to whether an origin may execute remotely.
type RemoteAccessPolicy struct {
	Mode     string         `json:"mode"` // open | restricted | disabled
	Revision string         `json:"revision"`
	Audience string         `json:"audience"`
	Issuers  []SourceIssuer `json:"issuers,omitempty"`
	Station  *StationPolicy `json:"station,omitempty"`
}

// SignedSourceGrant is sent alongside a command. The payload is a signed,
// short-lived SourceSnapshot; no API key or provider token belongs here.
type SignedSourceGrant struct {
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

type SourceSnapshot struct {
	Version          int    `json:"version"`
	Issuer           string `json:"issuer"`
	Audience         string `json:"audience"`
	DeviceID         string `json:"deviceId"`
	BindingID        string `json:"bindingId"`
	CredentialHandle string `json:"credentialHandle"`
	Agent            string `json:"agent"`
	ControlSurface   string `json:"controlSurface"`
	SourceType       string `json:"sourceType"`
	Origin           string `json:"origin"`
	APIBase          string `json:"apiBase,omitempty"`
	ConfigRevision   string `json:"configRevision"`
	PolicyRevision   string `json:"policyRevision"`
	CommandHash      string `json:"commandHash"`
	RequestID        string `json:"requestId"`
	ChallengeID      string `json:"challengeId,omitempty"`
	Model            string `json:"model,omitempty"`
	IssuedAt         int64  `json:"issuedAt"`
	ExpiresAt        int64  `json:"expiresAt"`
}

// ExecutionSource is the non-secret source contract forwarded to a Bridge.
// It is constructed by the Hub only after the current source policy has been
// checked; phone JSON cannot inject or override this envelope field.
type ExecutionSource struct {
	Mode             string         `json:"mode"` // open | restricted
	PolicyRevision   string         `json:"policyRevision"`
	Issuer           string         `json:"issuer,omitempty"`
	SourceType       string         `json:"sourceType,omitempty"`
	CredentialHandle string         `json:"credentialHandle,omitempty"`
	Origin           string         `json:"origin,omitempty"`
	APIBase          string         `json:"apiBase,omitempty"`
	ConfigRevision   string         `json:"configRevision,omitempty"`
	Agent            string         `json:"agent,omitempty"`
	ControlSurface   string         `json:"controlSurface,omitempty"`
	Model            string         `json:"model,omitempty"`
	Station          *StationPolicy `json:"station,omitempty"`
}

func policyID(s string) bool {
	if len(s) < 1 || len(s) > 128 {
		return false
	}
	for _, ch := range s {
		if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '-' || ch == '_' || ch == '.') {
			return false
		}
	}
	return true
}
func validSourceType(s string) bool {
	return s == "site_api" || s == "verified_proxy" || s == "official_account_proxy"
}

func (p *RemoteAccessPolicy) normalize() error {
	p.Mode = strings.TrimSpace(p.Mode)
	if p.Mode == "" {
		p.Mode = "open"
	}
	if p.Mode != "open" && p.Mode != "restricted" && p.Mode != "disabled" {
		return errors.New("invalid remote access mode")
	}
	if p.Revision == "" {
		p.Revision = "1"
	}
	if p.Station != nil {
		p.Station = cloneStationPolicy(p.Station)
		if err := p.Station.normalize(); err != nil {
			return err
		}
		if p.Station.AllowOfficialAccounts && p.Audience == "" {
			p.Audience = "station"
		}
	}
	if !policyID(p.Revision) || p.Audience != "" && !policyID(p.Audience) || len(p.Issuers) > 32 {
		return errors.New("invalid remote source policy")
	}
	p.Issuers = append([]SourceIssuer(nil), p.Issuers...)
	seen := map[string]bool{}
	for i := range p.Issuers {
		issuer := &p.Issuers[i]
		key, err := base64.RawURLEncoding.Strict().DecodeString(issuer.PublicKey)
		if !policyID(issuer.ID) || issuer.ID == stationOfficialIssuer || seen[issuer.ID] || err != nil || len(key) != ed25519.PublicKeySize || len(issuer.SourceTypes) < 1 || len(issuer.SourceTypes) > 3 || len(issuer.AgentIDs) > 32 || len(issuer.Origins) > 32 || len(issuer.APIBases) > 32 {
			return errors.New("invalid remote source issuer")
		}
		seen[issuer.ID] = true
		issuer.SourceTypes = append([]string(nil), issuer.SourceTypes...)
		for _, source := range issuer.SourceTypes {
			if !validSourceType(source) {
				return errors.New("invalid remote source class")
			}
		}
		issuer.AgentIDs = append([]string(nil), issuer.AgentIDs...)
		for _, agent := range issuer.AgentIDs {
			if !policyID(agent) {
				return errors.New("invalid remote source agent")
			}
		}
		issuer.Origins = append([]string(nil), issuer.Origins...)
		issuer.APIBases = append([]string(nil), issuer.APIBases...)
		origins, bases := map[string]bool{}, map[string]bool{}
		for _, origin := range issuer.Origins {
			if !canonicalOrigin(origin) || origins[origin] {
				return errors.New("invalid remote source issuer origin")
			}
			origins[origin] = true
		}
		for _, base := range issuer.APIBases {
			origin, ok := canonicalAPIBase(base)
			if !ok || bases[base] || len(origins) > 0 && !origins[origin] {
				return errors.New("invalid remote source issuer API base")
			}
			bases[base] = true
		}
		if issuer.AuthorizationURL != "" && !validAuthorizationURL(issuer.AuthorizationURL) {
			return errors.New("invalid remote source issuer authorization URL")
		}
		issuer.key = ed25519.PublicKey(key)
	}
	// The station allowance must not silently enlarge the independent issuer
	// document. Bound its encoded form, including HTML escaping, before publish.
	if raw, err := json.Marshal(p.Issuers); err != nil || len(raw) > maxSourceIssuerJSONBytes {
		return errors.New("remote source issuer policy too large")
	}
	return nil
}

func ParseRemoteAccessPolicy(data []byte) (RemoteAccessPolicy, error) {
	var policy RemoteAccessPolicy
	if len(data) > 32<<10 {
		return policy, errors.New("remote source policy too large")
	}
	if err := decodeExactObject(data, []string{"mode", "revision", "audience", "issuers", "station"}, &policy); err != nil {
		return policy, errors.New("invalid remote source policy JSON")
	}
	var fields map[string]json.RawMessage
	_ = json.Unmarshal(data, &fields)
	var issuers []json.RawMessage
	if raw, ok := fields["issuers"]; ok {
		if json.Unmarshal(raw, &issuers) != nil {
			return policy, errors.New("invalid remote source issuer JSON")
		}
		for _, issuer := range issuers {
			if decodeExactObject(issuer, []string{"id", "publicKey", "sourceTypes", "agentIds", "origins", "apiBases", "authorizationUrl"}, new(SourceIssuer)) != nil {
				return policy, errors.New("invalid remote source issuer JSON")
			}
		}
	}
	err := policy.normalize()
	return policy, err
}

// Workspace tools use the paired computer/project authority, independent of an AI provider.
func sourceWorkspaceCommand(typ string) bool {
	switch typ {
	case "workspace.artifacts":
		return true
	case "workspace.project.preview", "workspace.git.read", "workspace.git.mutate", "workspace.files.mutate", "workspace.preview.file", "workspace.preview.open", "workspace.preview.frame", "workspace.preview.input", "workspace.preview.close", "workspace.files.write", "workspace.terminal.open", "workspace.terminal.read", "workspace.terminal.input", "workspace.terminal.resize", "workspace.terminal.close":
		return true
	}
	return false
}
func sourceReadOrSafetyCommand(typ string) bool {
	if sourceWorkspaceCommand(typ) {
		return true
	}
	switch typ {
	case "workspace.project.files.list", "workspace.project.files.read", "remote.connections.sync":
		return true
	case "device.ping", "agents.status", "projects.list", "workspace.files.list", "workspace.files.read", "sessions.list", "session.open", "session.describe", "attachment.read", "desktop.status", "desktop.sessions.list", "desktop.session.open", "session.interrupt", "desktop.session.interrupt", "remote.station.receipt":
		return true
	}
	return false
}
func sourceExecutionCommand(typ string) bool {
	if typ == "session.side-question.tools" || typ == "session.cooperation.prepare" || typ == "session.actions" || typ == "session.action" || typ == "sessions.archived" || typ == "session.workflows" || typ == "session.workflow" || typ == "session.side-question.catalog" || typ == "session.side-question.ask" || typ == "session.side-question.open" || typ == "session.side-question.read" || typ == "session.side-question.send" || typ == "session.side-question.delete" {
		return true
	}
	switch typ {
	case "session.start", "session.send", "session.continue", "session.steer", "approval.respond", "desktop.session.send", "desktop.approval.respond", "agents.api.set", "remote.station.switch", "desktop.navigate", "attachment.put", "models.list", "features.list", "session.context", "session.compact", "session.revert":
		return true
	}
	return false
}

func canonicalOrigin(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.Path != "" || u.RawPath != "" || len(value) > 2048 {
		return false
	}
	if value != strings.ToLower(value) || strings.HasSuffix(u.Hostname(), ".") || u.Scheme == "https" && u.Port() == "443" || u.Scheme == "http" && u.Port() == "80" {
		return false
	}
	if port := u.Port(); port != "" {
		parsed, err := strconv.Atoi(port)
		if err != nil || parsed < 1 || parsed > 65535 || strconv.Itoa(parsed) != port {
			return false
		}
	}
	if strings.HasPrefix(u.Host, "[") && net.ParseIP(u.Hostname()) == nil {
		return false
	}
	host := u.Hostname()
	if u.Port() != "" {
		host = net.JoinHostPort(host, u.Port())
	} else if strings.Contains(host, ":") {
		host = "[" + host + "]"
	}
	if host != u.Host {
		return false
	}
	if u.Scheme == "https" {
		return true
	}
	ip := net.ParseIP(u.Hostname())
	return u.Scheme == "http" && (u.Hostname() == "localhost" || ip != nil && ip.IsLoopback())
}

// canonicalAPIBase uses the same root convention as the Bridge: optional
// path prefixes are retained, but a trailing slash or /v1 is not part of the
// base. Do not collapse paths or compare them by prefix at a trust boundary.
func canonicalAPIBase(value string) (string, bool) {
	u, err := url.Parse(value)
	if err != nil || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || strings.Contains(value, "%") || len(value) > 4096 {
		return "", false
	}
	origin := u.Scheme + "://" + u.Host
	if !canonicalOrigin(origin) || strings.HasSuffix(value, "/") || strings.HasSuffix(strings.ToLower(u.Path), "/v1") || strings.Contains(u.Path, "//") || strings.ContainsAny(u.Path, "\\\r\n\x00") {
		return "", false
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return "", false
		}
	}
	return origin, u.String() == value
}

func validAuthorizationURL(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.Scheme != "https" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || len(value) > 4096 {
		return false
	}
	origin := u.Scheme + "://" + u.Host
	if !canonicalOrigin(origin) || strings.Contains(u.Path, "//") || strings.ContainsAny(u.Path, "\\\r\n\x00") {
		return false
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	return u.String() == value
}

func issuerScopeAllows(issuer SourceIssuer, snapshot SourceSnapshot) bool {
	if len(issuer.Origins) > 0 {
		allowed := false
		for _, origin := range issuer.Origins {
			allowed = allowed || origin == snapshot.Origin
		}
		if !allowed {
			return false
		}
	}
	if len(issuer.APIBases) > 0 {
		for _, base := range issuer.APIBases {
			if base == snapshot.APIBase {
				return true
			}
		}
		return false
	}
	return true
}

// decodeExactObject prevents issuer, Hub, and Bridge implementations from
// interpreting different members of the same signed JSON object. Every
// declared name has exactly one spelling, and null is not a string/integer.
func decodeExactObject(raw []byte, fields []string, target any) error {
	meta, err := strictObjectFields(raw, fields)
	if err != nil {
		return err
	}
	known := make(map[string]bool, len(fields))
	for _, field := range fields {
		known[field] = true
	}
	for name, value := range meta {
		if !known[name] || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return errors.New("invalid signed source field")
		}
	}
	return json.Unmarshal(raw, target)
}

// ParseSignedSourceGrant is reusable by acquisition adapters. It parses only
// the public envelope; it does not establish the issuer's credential proof.
func ParseSignedSourceGrant(raw []byte) (SignedSourceGrant, error) {
	var grant SignedSourceGrant
	if len(raw) > MaxSignedSourceGrantBytes || decodeExactObject(raw, []string{"payload", "signature"}, &grant) != nil || grant.Payload == "" || grant.Signature == "" || len(grant.Payload) > MaxSourcePayloadBytes || len(grant.Signature) > 128 {
		return grant, errors.New("invalid signed source grant")
	}
	return grant, nil
}

// ParseSourceSnapshot parses the exact signed payload before verification.
// Semantic checks (issuer, binding, scope, TTL, hash) remain Hub decisions.
func ParseSourceSnapshot(raw []byte) (SourceSnapshot, error) {
	var snapshot SourceSnapshot
	fields := []string{"version", "issuer", "audience", "deviceId", "bindingId", "credentialHandle", "agent", "controlSurface", "sourceType", "origin", "apiBase", "configRevision", "policyRevision", "commandHash", "requestId", "challengeId", "model", "issuedAt", "expiresAt"}
	if len(raw) > MaxSourceSnapshotBytes || decodeExactObject(raw, fields, &snapshot) != nil {
		return snapshot, errors.New("invalid source snapshot")
	}
	return snapshot, nil
}

// commandMeta rejects duplicate and case-confusable keys. encoding/json's
// ordinary struct decoding accepts "Type" as "type" and lets a later key win;
// a policy boundary must inspect the exact command the Bridge will execute.
func commandMeta(raw json.RawMessage) (map[string]json.RawMessage, error) {
	// Every metadata field consulted by policy (including model/sessionKey) is
	// protected.  Rejecting case-confusable spellings is important because
	// encoding/json accepts them case-insensitively while this parser performs
	// exact-key lookups; otherwise a signed {"model":...} could be paired with
	// a later {"Model":...} value observed by the Bridge.
	meta, err := strictObjectFields(raw, []string{"type", "tool", "agent", "controlSurface", "accountId", "model", "sessionKey", "contextReferences"})
	if err == nil {
		err = validateDiffAnnotations(meta)
	}
	if err == nil {
		err = validateToolsSideQuestionCommand(meta)
	}
	if err == nil {
		err = validateSessionRevertCommand(meta)
	}
	return meta, err
}

// strictObjectFields parses exactly one JSON object and retains the original
// field values. The standard encoding/json struct decoder is intentionally not
// used at this trust boundary: it accepts case-insensitive field names and
// silently lets duplicate keys overwrite an earlier value. That is unsafe for
// signed commands because the phone may sign one value while the Hub/Bridge
// observes another.
func strictObjectFields(raw []byte, protected []string) (map[string]json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	first, err := decoder.Token()
	if err != nil || first != json.Delim('{') {
		return nil, errors.New("command must be an object")
	}
	result := map[string]json.RawMessage{}
	for decoder.More() {
		token, err := decoder.Token()
		if err != nil {
			return nil, err
		}
		name, ok := token.(string)
		if !ok || name != strings.TrimSpace(name) || name == "" {
			return nil, errors.New("invalid command key")
		}
		for _, field := range protected {
			if strings.EqualFold(name, field) && name != field {
				return nil, errors.New("case-confusable command metadata")
			}
		}
		if _, exists := result[name]; exists {
			return nil, errors.New("duplicate command metadata")
		}
		var value json.RawMessage
		if err := decoder.Decode(&value); err != nil {
			return nil, err
		}
		result[name] = value
	}
	last, err := decoder.Token()
	if err != nil || last != json.Delim('}') {
		return nil, errors.New("command must be an object")
	}
	// Token() stops after the closing brace. Reject a second JSON value rather
	// than allowing canonicalCommand/json.Unmarshal to disagree about what was
	// signed and what the Bridge later receives.
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("command must contain one JSON object")
	}
	return result, nil
}

func commandString(meta map[string]json.RawMessage, name string) string {
	var value string
	if json.Unmarshal(meta[name], &value) != nil {
		return ""
	}
	return strings.TrimSpace(value)
}

func commandMetadataStringsValid(meta map[string]json.RawMessage) bool {
	for _, name := range []string{"type", "tool", "agent", "controlSurface", "accountId", "model", "sessionKey"} {
		if raw, ok := meta[name]; ok {
			var value string
			if json.Unmarshal(raw, &value) != nil || strings.ContainsAny(value, "\r\n\x00") || len(value) > 4096 {
				return false
			}
		}
	}
	return true
}

func (h *Hub) evaluateSourcePolicy(acct string, r *http.Request, req commandReq) string {
	h.mu.Lock()
	execAcct, dev, binding := h.sourceIdentityLocked(acct, r, req.DeviceID)
	paired := dev != nil && dev.info.DeviceID == req.DeviceID && !dev.banned && !h.userDisabledLocked(req.DeviceID)
	p := h.userPolicyLocked(req.DeviceID)
	stationGuard, sourceGuard := deviceHasCapability(dev, p.Station.requiredCapability()), deviceSupportsSourceGuard(dev)
	h.mu.Unlock()
	if !paired {
		return "device_authorization_expired"
	}
	meta, err := commandMeta(req.Command)
	if err != nil {
		return "source_command_invalid"
	}
	if !commandMetadataStringsValid(meta) {
		return "source_command_invalid"
	}
	cmdType, accountID := commandString(meta, "type"), commandString(meta, "accountId")
	if cmdType == "" {
		return "source_command_invalid"
	}
	if sourceReadOrSafetyCommand(cmdType) {
		return ""
	}
	if p.Mode == "disabled" {
		return "remote_execution_disabled"
	}
	// The global execution switch applies to every policy version. Preserve
	// only the legacy v1 Bot source contract while execution is enabled; v2
	// additionally applies the same station source policy to Bot callers.
	if strings.HasPrefix(acct, "bot:") && (p.Station == nil || p.Station.Version == 1) {
		return ""
	}
	if !sourceExecutionCommand(cmdType) {
		return "source_command_unsupported"
	}
	if !stationGuard {
		return "station_guard_unsupported"
	}
	if !p.Station.supportedVersion() || cmdType != "remote.station.switch" && !p.Station.HasSources() {
		return "station_policy_unavailable"
	}
	if p.Mode == "open" {
		return ""
	}
	// Transport handover never reads target models or credentials. The Bridge
	// independently confirms the target Hub and rebinds before B can execute.
	// Attached grants retain all checks so stale signed attempts cannot revive.
	if cmdType == "remote.station.switch" && req.SourceGrant == nil {
		return ""
	}
	if p.Mode == "restricted" && !sourceGuard {
		return "source_guard_unsupported"
	}
	if p.Audience == "" || len(p.Issuers) == 0 && !p.Station.AllowOfficialAccounts {
		return "source_policy_unavailable"
	}
	if req.SourceGrant == nil {
		return "source_grant_required"
	}
	grant := req.SourceGrant
	if len(req.sourceGrantRaw) > 0 {
		parsed, err := ParseSignedSourceGrant(req.sourceGrantRaw)
		if err != nil {
			return "source_grant_invalid"
		}
		grant = &parsed
	}
	if len(grant.Payload) > MaxSourcePayloadBytes || len(grant.Signature) > 128 {
		return "source_grant_invalid"
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(grant.Payload)
	if err != nil {
		return "source_grant_invalid"
	}
	snapshot, err := ParseSourceSnapshot(payload)
	if err != nil {
		return "source_grant_invalid"
	}
	signature, err := base64.RawURLEncoding.Strict().DecodeString(grant.Signature)
	if err != nil {
		return "source_grant_invalid"
	}
	trusted := false
	agentAllowed := false
	if snapshot.Issuer == stationOfficialIssuer {
		if !p.Station.AllowOfficialAccounts || snapshot.SourceType != "official_account_proxy" || snapshot.Origin != officialAccountOrigin || snapshot.APIBase != officialAccountAPIBase || snapshot.ControlSurface != "cli" || snapshot.ChallengeID == "" || !ed25519.Verify(h.officialPublicKey, payload, signature) {
			return "source_issuer_denied"
		}
		trusted = true
		// The built-in connector attests only the Bridge's production managed
		// gateway routes. An extension needs its own registered source connector.
		agentAllowed = snapshot.Agent == "codex" || snapshot.Agent == "claude"
	}
	for _, issuer := range p.Issuers {
		if issuer.ID != snapshot.Issuer {
			continue
		}
		if !ed25519.Verify(issuer.key, payload, signature) {
			return "source_grant_invalid"
		}
		if !issuerScopeAllows(issuer, snapshot) {
			return "source_scope_denied"
		}
		for _, source := range issuer.SourceTypes {
			if source == snapshot.SourceType {
				trusted = true
			}
		}
		if len(issuer.AgentIDs) == 0 {
			agentAllowed = snapshot.Agent == "codex" || snapshot.Agent == "claude" || snapshot.Agent == "hermes"
		} else {
			for _, agent := range issuer.AgentIDs {
				if agent == snapshot.Agent {
					agentAllowed = true
				}
			}
		}
	}
	if !trusted {
		return "source_issuer_denied"
	}
	if !agentAllowed {
		return "source_agent_denied"
	}
	if cmdType != "remote.station.switch" && !p.Station.allowsSnapshot(snapshot) {
		return "source_station_denied"
	}
	now := time.Now().Unix()
	if snapshot.Version != 1 || !policyID(snapshot.CredentialHandle) || !policyID(snapshot.ConfigRevision) || !policyID(snapshot.Agent) || (snapshot.ControlSurface != "cli" && snapshot.ControlSurface != "desktop") || !canonicalOrigin(snapshot.Origin) {
		return "source_grant_invalid"
	}
	if snapshot.APIBase != "" {
		if origin, ok := canonicalAPIBase(snapshot.APIBase); !ok || origin != snapshot.Origin {
			return "source_grant_invalid"
		}
	}
	if snapshot.IssuedAt > now+30 || snapshot.IssuedAt < now-330 || snapshot.ExpiresAt <= now || snapshot.ExpiresAt <= snapshot.IssuedAt || snapshot.ExpiresAt-snapshot.IssuedAt > 300 {
		return "source_grant_expired"
	}
	canonical, err := canonicalCommand(req.Command)
	if err != nil || snapshot.CommandHash != shaHex(canonical) {
		return "source_command_mismatch"
	}
	if snapshot.Audience != p.Audience || snapshot.PolicyRevision != p.Revision || snapshot.RequestID != req.RequestID || req.RequestID == "" {
		return "source_policy_stale"
	}
	if binding == "" || snapshot.DeviceID != req.DeviceID || snapshot.BindingID != binding {
		return "source_binding_mismatch"
	}
	if accountID == "" || accountID != snapshot.CredentialHandle {
		return "source_credential_mismatch"
	}
	if declared := commandString(meta, "controlSurface"); declared != "" && declared != snapshot.ControlSurface {
		return "source_control_surface_mismatch"
	}
	declaredAgent := commandAgentMetadata(meta)
	if declaredAgent == "" || declaredAgent != snapshot.Agent {
		return "source_agent_mismatch"
	}
	if snapshot.ControlSurface == "desktop" && !strings.HasPrefix(cmdType, "desktop.") || snapshot.ControlSurface == "cli" && strings.HasPrefix(cmdType, "desktop.") {
		return "source_control_surface_mismatch"
	}
	declaredModel := commandString(meta, "model")
	if declaredModel != snapshot.Model {
		return "source_model_mismatch"
	}
	if snapshot.ChallengeID != "" && !h.sourceChallengeMatches(execAcct, acct, binding, req, snapshot) {
		return "source_challenge_expired"
	}
	return ""
}

func commandAgentMetadata(meta map[string]json.RawMessage) string {
	values := []string{}
	for _, name := range []string{"agent", "tool"} {
		if value := commandString(meta, name); value != "" {
			values = append(values, value)
		}
	}
	if raw, ok := meta["sessionKey"]; ok {
		var sessionKey string
		if json.Unmarshal(raw, &sessionKey) == nil {
			if index := strings.IndexByte(sessionKey, ':'); index > 0 {
				values = append(values, sessionKey[:index])
			}
		}
	}
	if len(values) == 0 {
		return ""
	}
	agent := strings.TrimSuffix(values[0], "-desktop")
	for _, value := range values[1:] {
		if strings.TrimSuffix(value, "-desktop") != agent {
			return ""
		}
	}
	return agent
}

func deviceSupportsSourceGuard(dev *device) bool {
	if dev == nil {
		return false
	}
	for _, capability := range dev.info.Capabilities {
		if capability == "source.guard.v1" {
			return true
		}
	}
	return false
}

// executionSourceFor converts a just-validated command into the minimal
// non-secret source metadata sent to the Bridge. The policy is evaluated and
// then decoded again while the caller holds remoteAccessGate.RLock; no mode or
// issuer change can occur between those two operations.
func (h *Hub) executionSourceFor(acct string, r *http.Request, req commandReq) (string, *ExecutionSource) {
	if code := h.evaluateSourcePolicy(acct, r, req); code != "" {
		return code, nil
	}
	p := h.deviceRemoteAccessPolicy(req.DeviceID)
	meta, err := commandMeta(req.Command)
	if err != nil {
		return "source_command_invalid", nil
	}
	typ := commandString(meta, "type")
	if !sourceExecutionCommand(typ) {
		return "", nil
	}
	if p.Mode == "open" || typ == "remote.station.switch" && req.SourceGrant == nil {
		return "", &ExecutionSource{Mode: "open", PolicyRevision: p.Revision, Station: cloneStationPolicy(p.Station)}
	}
	// restricted was already signature-checked by evaluateSourcePolicy. Decode
	// its payload to forward only the public execution contract, never payload
	// or signature material.
	if req.SourceGrant == nil {
		return "source_grant_required", nil
	}
	grant := *req.SourceGrant
	if len(req.sourceGrantRaw) > 0 {
		grant, err = ParseSignedSourceGrant(req.sourceGrantRaw)
		if err != nil {
			return "source_grant_invalid", nil
		}
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(grant.Payload)
	if err != nil {
		return "source_grant_invalid", nil
	}
	snapshot, err := ParseSourceSnapshot(payload)
	if err != nil {
		return "source_grant_invalid", nil
	}
	return "", &ExecutionSource{Mode: "restricted", PolicyRevision: p.Revision, Issuer: snapshot.Issuer, SourceType: snapshot.SourceType, CredentialHandle: snapshot.CredentialHandle, Origin: snapshot.Origin, APIBase: snapshot.APIBase, ConfigRevision: snapshot.ConfigRevision, Agent: snapshot.Agent, ControlSurface: snapshot.ControlSurface, Model: snapshot.Model, Station: cloneStationPolicy(p.Station)}
}

// cloneRemoteAccessPolicy returns the runtime policy without exposing the
// parsed key cache as mutable shared state. The public JSON form intentionally
// contains only public keys; issuer.key is an internal verification cache.
func cloneRemoteAccessPolicy(p RemoteAccessPolicy) RemoteAccessPolicy {
	out := p
	out.Station = cloneStationPolicy(p.Station)
	out.Issuers = make([]SourceIssuer, len(p.Issuers))
	for i, issuer := range p.Issuers {
		out.Issuers[i] = issuer
		out.Issuers[i].SourceTypes = append([]string(nil), issuer.SourceTypes...)
		out.Issuers[i].AgentIDs = append([]string(nil), issuer.AgentIDs...)
		out.Issuers[i].Origins = append([]string(nil), issuer.Origins...)
		out.Issuers[i].APIBases = append([]string(nil), issuer.APIBases...)
		out.Issuers[i].key = append(ed25519.PublicKey(nil), issuer.key...)
	}
	return out
}

// CurrentRemoteAccessPolicy is a read-only snapshot for the standalone admin
// console. It deliberately returns no private material (the policy stores
// only Ed25519 public keys).
func (h *Hub) CurrentRemoteAccessPolicy() RemoteAccessPolicy {
	h.mu.Lock()
	defer h.mu.Unlock()
	return cloneRemoteAccessPolicy(h.cfg.RemoteAccess)
}

// ApplyRemoteAccessPolicy changes only the execution-source gate. It does not
// cancel running tasks, revoke pairings, or alter the resource/cache mode.
// The before callback is the persistence commit performed by standalone; an
// error leaves the in-memory policy untouched.
func (h *Hub) ApplyRemoteAccessPolicy(policy RemoteAccessPolicy, before func() error) error {
	return h.ApplyRemoteAccessPolicyCommit(policy, func(RemoteAccessPolicy) error {
		if before != nil {
			return before()
		}
		return nil
	})
}

// ApplyRemoteAccessPolicyCommit passes the exact effective policy to the
// persistence callback. Any security-policy content change receives a new
// epoch, including when an operator supplies a previously used revision.
func (h *Hub) ApplyRemoteAccessPolicyCommit(policy RemoteAccessPolicy, before func(RemoteAccessPolicy) error) error {
	if policy.Station == nil {
		policy.Station = defaultStationPolicy(h.cfg.PublicURL, h.cfg.Prefix)
	}
	if err := policy.normalize(); err != nil {
		return err
	}
	h.lifecycleMu.Lock()
	if h.stopped {
		h.lifecycleMu.Unlock()
		return errors.New("Hub is stopping")
	}
	h.activeRequests.Add(1)
	h.lifecycleMu.Unlock()
	defer h.activeRequests.Done()
	// Block only the short final policy/dispatch critical section. A task that
	// has already been sent is not interrupted by a mode change.
	h.remoteAccessGate.Lock()
	defer h.remoteAccessGate.Unlock()
	h.mu.Lock()
	defer h.mu.Unlock()
	oldContent, newContent := cloneRemoteAccessPolicy(h.cfg.RemoteAccess), cloneRemoteAccessPolicy(policy)
	oldContent.Revision, newContent.Revision = "", ""
	if !reflect.DeepEqual(oldContent, newContent) {
		policy.Revision = NewRemoteAccessRevision()
	}
	if before != nil {
		if err := before(cloneRemoteAccessPolicy(policy)); err != nil {
			return err
		}
	}
	h.cfg.RemoteAccess = cloneRemoteAccessPolicy(policy)
	h.adminAudit = append(h.adminAudit, AdminAudit{At: nowMs(), Action: "remote-access", Reason: policy.Mode, Actor: "standalone-admin"})
	if len(h.adminAudit) > 200 {
		h.adminAudit = h.adminAudit[len(h.adminAudit)-200:]
	}
	h.markDirty()
	return nil
}

// NewRemoteAccessRevision is an opaque policy epoch. It is generated before
// the atomic persistence callback so restart restores the same epoch.
func NewRemoteAccessRevision() string { return "mode-" + newID() }

// RemoteSourceDiscovery is an operator-configured authorization contract, not
// proof that an API account is eligible. Clients must never choose a different
// endpoint from command JSON or infer eligibility from these public labels.
func (h *Hub) RemoteSourceDiscovery() RemoteAccessPolicy {
	return h.CurrentRemoteAccessPolicy()
}
