package edgepool

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"time"
)

type nodeInput struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	ProxyURL    *string `json:"proxy_url"`
	Enabled     *bool   `json:"enabled"`
	Weight      *int    `json:"weight"`
	MaxRequests *int    `json:"max_requests"`
	MaxStreams  *int    `json:"max_streams"`
	Draining    *bool   `json:"draining"`
	Region      *string `json:"region"`
}

type limitsInput struct {
	MaxRequests    *int `json:"max_requests"`
	MaxStreams     *int `json:"max_streams"`
	QueueLimit     *int `json:"queue_limit"`
	QueueTimeoutMS *int `json:"queue_timeout_ms"`
}

func answer(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}

func failure(w http.ResponseWriter, status int, code string) {
	answer(w, status, map[string]string{"error": code, "code": code})
}

func decode(w http.ResponseWriter, r *http.Request, value any) bool {
	reader := http.MaxBytesReader(w, r.Body, 64<<10)
	defer reader.Close()
	dec := json.NewDecoder(reader)
	dec.DisallowUnknownFields()
	if dec.Decode(value) != nil {
		failure(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		failure(w, http.StatusBadRequest, "invalid_request")
		return false
	}
	return true
}

// ControlHandler is deliberately separate from Handler. Publish it only on a
// private authenticated management channel; it accepts no token in a URL.
func (p *Pool) ControlHandler() http.Handler { return http.HandlerFunc(p.control) }

func (p *Pool) control(w http.ResponseWriter, r *http.Request) {
	header := r.Header.Get("Authorization")
	hash := sha256.Sum256([]byte(strings.TrimPrefix(header, "Bearer ")))
	if len(r.Header.Values("Authorization")) != 1 || !strings.HasPrefix(header, "Bearer ") || subtle.ConstantTimeCompare(hash[:], p.token[:]) != 1 {
		w.Header().Set("WWW-Authenticate", "Bearer")
		failure(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if r.URL.RawQuery != "" || r.URL.ForceQuery {
		failure(w, http.StatusBadRequest, "query_not_allowed")
		return
	}
	if p.serveGeo(w, r) || p.serveSources(w, r) || p.serveReleases(w, r) || p.serveObservability(w, r) {
		return
	}
	if r.URL.Path == "/v1/state" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			failure(w, 405, "method_not_allowed")
			return
		}
		answer(w, 200, p.State())
		return
	}
	if r.URL.Path != "/v1/nodes" && r.URL.Path != "/v1/nodes/delete" && r.URL.Path != "/v1/policy" && r.URL.Path != "/v1/probe" && r.URL.Path != "/v1/probe/cancel" && r.URL.Path != "/v1/subscriptions/inspect" && r.URL.Path != "/v1/subscriptions/import" {
		failure(w, 404, "not_found")
		return
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		failure(w, 405, "method_not_allowed")
		return
	}
	if r.URL.Path == "/v1/subscriptions/inspect" {
		p.inspectSubscription(w, r)
		return
	}
	if r.URL.Path == "/v1/subscriptions/import" {
		p.importSubscription(w, r)
		return
	}
	if r.URL.Path == "/v1/probe/cancel" {
		var body struct {
			RunID string `json:"run_id"`
		}
		if !decode(w, r, &body) {
			return
		}
		if err := p.CancelProbe(body.RunID); err != nil {
			status := 400
			if err.Error() == "probe_run_not_found" {
				status = 409
			}
			if err.Error() == "stopped" {
				status = 503
			}
			failure(w, status, err.Error())
			return
		}
		answer(w, http.StatusAccepted, p.State())
		return
	}
	if r.URL.Path == "/v1/probe" {
		var body struct {
			Scope   string   `json:"scope"`
			NodeIDs []string `json:"node_ids"`
		}
		if !decode(w, r, &body) {
			return
		}
		if _, err := p.RequestProbe(body.Scope, body.NodeIDs); err != nil {
			failure(w, http.StatusBadRequest, err.Error())
			return
		}
		answer(w, http.StatusAccepted, p.State())
		return
	}
	var raw json.RawMessage
	if !decode(w, r, &raw) {
		return
	}
	if r.URL.Path == "/v1/policy" {
		var fields map[string]json.RawMessage
		if json.Unmarshal(raw, &fields) == nil {
			if _, present := fields["source_limits"]; present {
				failure(w, 409, "source_limits_require_release")
				return
			}
		}
	}
	expected, apply, code := decodeMutation(strings.TrimPrefix(r.URL.Path, "/v1/"), raw)
	if code != "" {
		failure(w, 400, code)
		return
	}
	p.change(w, expected, apply)
}

func firstEnabled(s savedConfig) string {
	for _, n := range s.Nodes {
		if n.Enabled {
			return n.ID
		}
	}
	return ""
}
func enabledNode(s savedConfig, id string) bool {
	for _, n := range s.Nodes {
		if n.ID == id {
			return n.Enabled
		}
	}
	return false
}

func (p *Pool) change(w http.ResponseWriter, expected *int64, apply func(*savedConfig) string) {
	state, status, code := p.commitConfig(expected, apply)
	if code != "" {
		failure(w, status, code)
		return
	}
	answer(w, status, state)
}

// commitConfig serializes configuration writers, but keeps disk sync and all
// caller-controlled HTTP writes outside the routing-state lock. Close takes the
// same writer lock so the data lock cannot be released during persistence.
func (p *Pool) commitConfig(expected *int64, apply func(*savedConfig) string) (State, int, string) {
	p.configMu.Lock()
	defer p.configMu.Unlock()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return State{}, 503, "stopped"
	}
	if expected == nil {
		p.mu.Unlock()
		return State{}, 400, "expected_revision_required"
	}
	if *expected != p.saved.Revision {
		p.mu.Unlock()
		return State{}, 409, "revision_conflict"
	}
	if p.saved.Trial != nil {
		p.mu.Unlock()
		return State{}, 409, "trial_in_progress"
	}
	previous := p.saved
	next := cloneConfig(p.saved)
	next.Revision++
	p.mu.Unlock()
	if code := apply(&next); code != "" {
		return State{}, 400, code
	}
	normalizeConfig(&next)
	if err := validateSaved(next); err != nil {
		return State{}, 400, "invalid_configuration"
	}
	if err := validateOriginNodes(p.origin, next); err != nil {
		return State{}, 400, "https_origin_required"
	}
	if !addReleaseHistory(&next, previous, "configuration_changed") {
		return State{}, 500, "release_id_unavailable"
	}
	next.ReleaseError = ""
	durability, err := writeSaved(p.cfg.DataDir, next)
	if err != nil {
		return State{}, 500, "persistence_failed"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.installCommittedLocked(next, durability, "configuration_changed"), 200, ""
}

// Caller holds mu and the configuration writer lock. All durable publication
// paths share routing installation and diagnostic scheduling.
func (p *Pool) installCommittedLocked(next savedConfig, durability, event string) State {
	changedTargets := make(map[string]bool)
	for _, n := range next.Nodes {
		old := p.routes[n.ID]
		if n.Enabled && (old == nil || !old.config.Enabled || old.config.ProxyURL != n.ProxyURL || old.config.Imported != n.Imported) {
			changedTargets[n.ID] = false
		}
	}
	p.installLocked(next)
	p.rescheduleProbeLocked()
	// Embedders can suppress automatic network activity for deterministic tests.
	// The persisted enabled=false policy only stops periodic passes; explicit
	// configuration changes are still diagnosed, without enabling disabled nodes.
	if !p.cfg.DisablePeriodicProbe && len(changedTargets) > 0 {
		p.requestProbeLocked(changedTargets, "selected", "configuration")
	}
	p.durability = durability
	if durability == "uncertain" {
		p.eventLocked("durability_warning", "", "configuration_saved_directory_sync_uncertain")
	}
	// An explicit configuration change should not be delayed by an old cooldown.
	p.switched = time.Time{}
	p.chooseLocked("configuration_changed")
	p.signalLocked()
	p.eventLocked(event, p.active, "configuration_saved")
	return p.stateLocked()
}
