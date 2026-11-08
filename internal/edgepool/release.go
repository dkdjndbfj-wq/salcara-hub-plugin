package edgepool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"
)

const releaseTrialDuration = 120 * time.Second
const releaseHistoryLimit = 8

// Snapshots never contain the journal itself. Credentials are persisted only in
// the private atomic configuration file, never returned by the management API.
type poolSnapshot struct {
	Mode            string          `json:"mode"`
	SelectedID      string          `json:"selected_id"`
	Nodes           []nodeConfig    `json:"nodes"`
	Limits          Limits          `json:"limits"`
	HealthPolicy    HealthPolicy    `json:"health_policy"`
	SelectionPolicy SelectionPolicy `json:"selection_policy"`
	SourceLimits    []SourceLimit   `json:"source_limits,omitempty"`
}
type releaseVersion struct {
	ID        string       `json:"id"`
	Revision  int64        `json:"revision"`
	CreatedAt int64        `json:"created_at"`
	Reason    string       `json:"reason"`
	Snapshot  poolSnapshot `json:"snapshot"`
}
type ReleaseTrial struct {
	ID           string `json:"id"`
	BaseRevision int64  `json:"base_revision"`
	Revision     int64  `json:"revision"`
	StartedAt    int64  `json:"started_at"`
	ExpiresAt    int64  `json:"expires_at"`
}
type pendingRelease struct {
	ReleaseTrial
	Previous poolSnapshot `json:"previous"`
}
type ReleasePreview struct {
	ID           string `json:"id"`
	Status       string `json:"status"`
	BaseRevision int64  `json:"base_revision"`
	CreatedAt    int64  `json:"created_at"`
	ExpiresAt    int64  `json:"expires_at"`
	Total        int    `json:"total"`
	Completed    int    `json:"completed"`
	Healthy      int    `json:"healthy"`
	Summary      string `json:"summary"`
	Reason       string `json:"reason"`
}
type ReleaseHistory struct {
	ID         string `json:"id"`
	Revision   int64  `json:"revision"`
	CreatedAt  int64  `json:"created_at"`
	Reason     string `json:"reason"`
	Revertible bool   `json:"revertible"`
}
type ReleaseState struct {
	Trial     *ReleaseTrial    `json:"trial"`
	Previews  []ReleasePreview `json:"previews"`
	History   []ReleaseHistory `json:"history"`
	LastError string           `json:"last_error"`
}
type releaseObservation struct {
	health    Health
	route     *route
	epoch     uint64
	startedAt int64
}
type releasePreview struct {
	public       ReleasePreview
	base         savedConfig
	candidate    savedConfig
	observations map[string]releaseObservation
}

func releaseID() string {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return ""
	}
	return hex.EncodeToString(bytes[:])
}
func snapshotOf(s savedConfig) poolSnapshot {
	policy := s.SelectionPolicy
	policy.PreferredRegions = append([]string{}, policy.PreferredRegions...)
	return poolSnapshot{Mode: s.Mode, SelectedID: s.SelectedID, Nodes: append([]nodeConfig{}, s.Nodes...), Limits: s.Limits, HealthPolicy: s.HealthPolicy, SelectionPolicy: policy, SourceLimits: append([]SourceLimit{}, s.SourceLimits...)}
}
func (s poolSnapshot) config(revision int64) savedConfig {
	policy := s.SelectionPolicy
	policy.PreferredRegions = append([]string{}, policy.PreferredRegions...)
	return savedConfig{Revision: revision, Mode: s.Mode, SelectedID: s.SelectedID, Nodes: append([]nodeConfig{}, s.Nodes...), Limits: s.Limits, HealthPolicy: s.HealthPolicy, SelectionPolicy: policy, SourceLimits: append([]SourceLimit{}, s.SourceLimits...)}
}
func cloneConfig(s savedConfig) savedConfig {
	next := snapshotOf(s).config(s.Revision)
	next.History = append([]releaseVersion{}, s.History...)
	next.Trial = s.Trial // journal objects are immutable after publication
	next.ReleaseError = s.ReleaseError
	return next
}
func validateReleaseJournal(s savedConfig) error {
	if len(s.History) > releaseHistoryLimit || len(s.ReleaseError) > 96 {
		return errors.New("invalid release journal")
	}
	seen := map[string]bool{}
	for _, h := range s.History {
		if !nodeID.MatchString(h.ID) || seen[h.ID] || h.Revision < 1 || h.CreatedAt < 1 || len(h.Reason) > 96 || validateSaved(h.Snapshot.config(h.Revision)) != nil {
			return errors.New("invalid release history")
		}
		seen[h.ID] = true
	}
	if t := s.Trial; t != nil {
		if !nodeID.MatchString(t.ID) || t.BaseRevision < 1 || t.Revision != t.BaseRevision+1 || t.StartedAt < 1 || t.ExpiresAt-t.StartedAt != releaseTrialDuration.Milliseconds() || validateSaved(t.Previous.config(t.BaseRevision)) != nil {
			return errors.New("invalid pending release")
		}
	}
	return nil
}
func addReleaseHistory(next *savedConfig, previous savedConfig, reason string) bool {
	id := releaseID()
	if id == "" {
		return false
	}
	next.History = append(append([]releaseVersion{}, previous.History...), releaseVersion{id, previous.Revision, time.Now().UnixMilli(), reason, snapshotOf(previous)})
	if len(next.History) > releaseHistoryLimit {
		next.History = next.History[len(next.History)-releaseHistoryLimit:]
	}
	return true
}

// A recovery operation may restore proxy routes, but must not invent permission
// to send directly. Even an explicit history restore preserves this restriction.
func restoreGuarded(current savedConfig, snapshot poolSnapshot) (savedConfig, bool) {
	next := snapshot.config(current.Revision + 1)
	next.History = append([]releaseVersion{}, current.History...)
	direct := map[string]bool{}
	for _, n := range current.Nodes {
		if n.Enabled && n.ProxyURL == "" {
			direct[n.ID] = true
		}
	}
	guarded := false
	for i := range next.Nodes {
		n := &next.Nodes[i]
		if n.Enabled && n.ProxyURL == "" && !direct[n.ID] {
			n.Enabled = false
			guarded = true
		}
	}
	if !enabledNode(next, next.SelectedID) {
		next.SelectedID = firstEnabled(next)
	}
	if guarded {
		next.ReleaseError = "rollback_preserved_direct_block"
	}
	return next, guarded
}
func recoverPendingRelease(s savedConfig) (savedConfig, error) {
	if s.Trial == nil {
		return s, nil
	}
	if s.Trial.Revision != s.Revision {
		next := cloneConfig(s)
		next.Trial = nil
		next.Revision++
		next.ReleaseError = "trial_superseded"
		return next, nil
	}
	next, _ := restoreGuarded(s, s.Trial.Previous)
	if !addReleaseHistory(&next, s, "restart_rollback") {
		return savedConfig{}, errors.New("cannot allocate recovery history")
	}
	return next, nil
}

func (p *Pool) releaseStateLocked() ReleaseState {
	s := ReleaseState{Previews: []ReleasePreview{}, History: []ReleaseHistory{}, LastError: p.saved.ReleaseError}
	if p.saved.Trial != nil {
		trial := p.saved.Trial.ReleaseTrial
		s.Trial = &trial
	}
	for _, preview := range p.releasePreviews {
		v := preview.public
		if v.Status == "ready" && time.Now().UnixMilli() >= v.ExpiresAt {
			v.Status = "rejected"
			v.Reason = "preview_expired"
		}
		s.Previews = append(s.Previews, v)
	}
	sort.Slice(s.Previews, func(i, j int) bool { return s.Previews[i].CreatedAt < s.Previews[j].CreatedAt })
	for i := len(p.saved.History) - 1; i >= 0; i-- {
		h := p.saved.History[i]
		s.History = append(s.History, ReleaseHistory{h.ID, h.Revision, h.CreatedAt, h.Reason, p.saved.Trial == nil})
	}
	return s
}

func (p *Pool) prepareCandidate(expected *int64, summary string, apply func(*savedConfig) string) (State, int, string) {
	p.configMu.Lock()
	defer p.configMu.Unlock()
	p.mu.Lock()
	if code, status := p.releaseCheckLocked(expected); code != "" {
		p.mu.Unlock()
		return State{}, status, code
	}
	if p.saved.Trial != nil {
		p.mu.Unlock()
		return State{}, 409, "trial_in_progress"
	}
	now := time.Now()
	for id, v := range p.releasePreviews {
		if v.public.Status != "checking" && now.UnixMilli() >= v.public.ExpiresAt {
			delete(p.releasePreviews, id)
		}
	}
	if len(p.releasePreviews) >= 4 {
		oldest := ""
		var at int64
		for id, v := range p.releasePreviews {
			if v.public.Status != "checking" && (oldest == "" || v.public.CreatedAt < at) {
				oldest = id
				at = v.public.CreatedAt
			}
		}
		if oldest == "" {
			p.mu.Unlock()
			return State{}, 409, "preflight_busy"
		}
		delete(p.releasePreviews, oldest)
	}
	base := cloneConfig(p.saved)
	candidate := cloneConfig(p.saved)
	p.mu.Unlock()
	if code := apply(&candidate); code != "" {
		return State{}, 400, code
	}
	normalizeConfig(&candidate)
	if validateSaved(candidate) != nil {
		return State{}, 400, "invalid_configuration"
	}
	if validateOriginNodes(p.origin, candidate) != nil {
		return State{}, 400, "https_origin_required"
	}
	id := releaseID()
	if id == "" {
		return State{}, 500, "release_id_unavailable"
	}
	preview := &releasePreview{public: ReleasePreview{ID: id, Status: "checking", BaseRevision: base.Revision, CreatedAt: now.UnixMilli(), ExpiresAt: now.Add(10 * time.Minute).UnixMilli(), Summary: summary}, base: base, candidate: candidate, observations: map[string]releaseObservation{}}
	for _, n := range candidate.Nodes {
		if n.Enabled {
			preview.public.Total++
		}
	}
	p.mu.Lock()
	p.releasePreviews[id] = preview
	p.wg.Add(1)
	state := p.stateLocked()
	p.mu.Unlock()
	go p.preflightRelease(preview)
	return state, http.StatusAccepted, ""
}

func (p *Pool) preflightRelease(v *releasePreview) {
	defer p.wg.Done()
	ctx, cancel := context.WithTimeout(p.ctx, 60*time.Second)
	defer cancel()
	sem := make(chan struct{}, min(4, v.candidate.HealthPolicy.Concurrency))
	var workers sync.WaitGroup
	for _, n := range v.candidate.Nodes {
		if !n.Enabled {
			continue
		}
		workers.Add(1)
		go func(n nodeConfig) {
			defer workers.Done()
			ob := releaseObservation{health: Health{Status: "unhealthy", Error: "preflight_failed"}}
			select {
			case sem <- struct{}{}:
				ob = p.preflightNode(ctx, n, time.Duration(min(v.candidate.HealthPolicy.TimeoutMS, 6000))*time.Millisecond)
				<-sem
			case <-ctx.Done():
			}
			p.mu.Lock()
			v.observations[n.ID] = ob
			v.public.Completed++
			if ob.health.Status == "healthy" {
				v.public.Healthy++
			}
			p.mu.Unlock()
		}(n)
	}
	workers.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	v.public.Status = "rejected"
	v.public.Reason = "no_verified_route"
	if p.closed || ctx.Err() != nil {
		v.public.Reason = "preflight_cancelled"
		return
	}
	old := map[string]nodeConfig{}
	for _, n := range v.base.Nodes {
		old[n.ID] = n
	}
	usable := false
	for _, n := range v.candidate.Nodes {
		if !n.Enabled {
			continue
		}
		good := v.observations[n.ID].health.Status == "healthy"
		prior, exists := old[n.ID]
		if (!exists || !prior.Enabled || prior.ProxyURL != n.ProxyURL || prior.Imported != n.Imported) && !good {
			v.public.Reason = "changed_node_unreachable"
			return
		}
		eligible := good && !n.Draining
		if v.candidate.Mode == "manual" {
			eligible = eligible && n.ID == v.candidate.SelectedID
		} else if v.candidate.SelectionPolicy.RegionMode == "only" {
			match := false
			for _, region := range v.candidate.SelectionPolicy.PreferredRegions {
				if strings.EqualFold(p.regionFor(n, v.candidate.SelectionPolicy), region) {
					match = true
				}
			}
			eligible = eligible && match
		}
		usable = usable || eligible
	}
	if usable {
		v.public.Status = "ready"
		v.public.Reason = ""
		v.public.ExpiresAt = time.Now().Add(30 * time.Second).UnixMilli()
	}
}

func (p *Pool) preflightNode(ctx context.Context, n nodeConfig, timeout time.Duration) releaseObservation {
	ob := releaseObservation{health: Health{Status: "unhealthy", Error: "preflight_failed"}, startedAt: time.Now().UnixMilli()}
	release, acquired := p.acquireProbeSlot(ctx)
	if !acquired {
		return ob
	}
	defer release()
	proxy, _ := parseProxy(n.ProxyURL)
	transport := transportFor(proxy, true, n.Imported)
	p.mu.Lock()
	if old := p.routes[n.ID]; old != nil && old.config.ProxyURL == n.ProxyURL && old.config.Imported == n.Imported {
		transport = old.once.Clone()
		ob.route = old
		ob.epoch = old.healthEpoch
	}
	p.mu.Unlock()
	defer transport.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	u := *p.origin
	u.Path = "/salcara-hub/statusz"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	started := time.Now()
	resp, err := client.Do(req)
	if err == nil {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4097))
		_ = resp.Body.Close()
		var status struct {
			Status string `json:"status"`
		}
		if resp.StatusCode == 200 && readErr == nil && len(body) <= 4096 && json.Unmarshal(body, &status) == nil && status.Status == "available" {
			latency := float64(time.Since(started).Microseconds()) / 1000
			ob.health = Health{Status: "healthy", LatencyMS: &latency, LatencyEWMAMS: &latency, CheckedAt: time.Now().UnixMilli()}
		}
	}
	ob.health.LastProbeAt = time.Now().UnixMilli()
	return ob
}

func (p *Pool) releaseCheckLocked(expected *int64) (string, int) {
	if p.closed {
		return "stopped", 503
	}
	if expected == nil {
		return "expected_revision_required", 400
	}
	if *expected != p.saved.Revision {
		return "revision_conflict", 409
	}
	return "", 200
}

func (p *Pool) startRelease(expected *int64, id string) (State, int, string) {
	p.configMu.Lock()
	defer p.configMu.Unlock()
	p.mu.Lock()
	if code, status := p.releaseCheckLocked(expected); code != "" {
		p.mu.Unlock()
		return State{}, status, code
	}
	if p.saved.Trial != nil {
		p.mu.Unlock()
		return State{}, 409, "trial_in_progress"
	}
	v := p.releasePreviews[id]
	if v == nil {
		p.mu.Unlock()
		return State{}, 404, "preview_not_found"
	}
	if v.public.BaseRevision != p.saved.Revision {
		p.mu.Unlock()
		return State{}, 409, "revision_conflict"
	}
	if v.public.Status != "ready" {
		p.mu.Unlock()
		return State{}, 409, "preview_not_ready"
	}
	if time.Now().UnixMilli() >= v.public.ExpiresAt {
		p.mu.Unlock()
		return State{}, 409, "preview_expired"
	}
	next := cloneConfig(v.candidate)
	next.Revision = p.saved.Revision + 1
	trialID := releaseID()
	if trialID == "" || !addReleaseHistory(&next, p.saved, "trial_start") {
		p.mu.Unlock()
		return State{}, 500, "release_id_unavailable"
	}
	now := time.Now()
	next.Trial = &pendingRelease{ReleaseTrial{trialID, p.saved.Revision, next.Revision, now.UnixMilli(), now.Add(releaseTrialDuration).UnixMilli()}, snapshotOf(p.saved)}
	next.ReleaseError = ""
	p.mu.Unlock()
	state, status, code := p.persistRelease(next, "trial_started")
	if code != "" {
		return state, status, code
	}
	p.mu.Lock()
	p.trialDeadline = now.Add(releaseTrialDuration)
	for id, ob := range v.observations {
		r := p.routes[id]
		if r == nil {
			continue
		}
		// Preserve newer passive failures and normal probe evidence. A reenabled
		// route still has to satisfy the configured recovery streak and hold.
		if r == ob.route && (r.healthEpoch != ob.epoch || r.health.LastProbeAt > ob.startedAt) {
			continue
		}
		if r != ob.route && ob.route != nil {
			continue
		}
		r.health = ob.health
		if ob.health.Status == "healthy" {
			r.probeFailures = 0
			r.probeSuccesses++
			if r.recovering && r.recoveredAt.IsZero() {
				r.recoveredAt = time.Now()
			}
		}
	}
	delete(p.releasePreviews, id)
	p.chooseLocked("trial_started")
	state = p.stateLocked()
	p.mu.Unlock()
	p.wakeRelease()
	return state, 200, ""
}

func (p *Pool) confirmRelease(expected *int64, id string) (State, int, string) {
	p.configMu.Lock()
	defer p.configMu.Unlock()
	p.mu.Lock()
	if code, status := p.releaseCheckLocked(expected); code != "" {
		p.mu.Unlock()
		return State{}, status, code
	}
	t := p.saved.Trial
	if t == nil || id == "" || t.ID != id {
		p.mu.Unlock()
		return State{}, 409, "trial_not_found"
	}
	if t.Revision != p.saved.Revision {
		p.mu.Unlock()
		return State{}, 409, "trial_superseded"
	}
	if !time.Now().Before(p.trialDeadline) {
		p.mu.Unlock()
		return State{}, 409, "trial_expired"
	}
	next := cloneConfig(p.saved)
	next.Trial = nil
	next.ReleaseError = ""
	next.Revision++
	p.mu.Unlock()
	return p.persistRelease(next, "trial_confirmed")
}

func (p *Pool) rollbackRelease(expected *int64, trialID, versionID string) (State, int, string) {
	p.configMu.Lock()
	defer p.configMu.Unlock()
	p.mu.Lock()
	if code, status := p.releaseCheckLocked(expected); code != "" {
		p.mu.Unlock()
		return State{}, status, code
	}
	if (trialID == "") == (versionID == "") {
		p.mu.Unlock()
		return State{}, 400, "rollback_target_required"
	}
	var previous poolSnapshot
	if trialID != "" {
		t := p.saved.Trial
		if t == nil || t.ID != trialID {
			p.mu.Unlock()
			return State{}, 409, "trial_not_found"
		}
		if t.Revision != p.saved.Revision {
			p.mu.Unlock()
			return State{}, 409, "trial_superseded"
		}
		previous = t.Previous
	} else {
		if p.saved.Trial != nil {
			p.mu.Unlock()
			return State{}, 409, "trial_in_progress"
		}
		found := false
		for _, v := range p.saved.History {
			if v.ID == versionID {
				previous = v.Snapshot
				found = true
				break
			}
		}
		if !found {
			p.mu.Unlock()
			return State{}, 404, "version_not_found"
		}
	}
	next, _ := restoreGuarded(p.saved, previous)
	if !addReleaseHistory(&next, p.saved, "manual_rollback") {
		p.mu.Unlock()
		return State{}, 500, "release_id_unavailable"
	}
	p.mu.Unlock()
	return p.persistRestoredRelease(next, "configuration_rolled_back")
}

// Restoring a private snapshot is not enough when it points at a managed
// listener whose old process has already retired. Prepare that dependency before
// publishing the pool pointer, while preserving the current working generation.
// The writer lock is held throughout; the routing mutex is never held here.
func (p *Pool) persistRestoredRelease(next savedConfig, event string) (State, int, string) {
	ctx, cancel := context.WithTimeout(p.ctx, 20*time.Second)
	defer cancel()
	finish, err := p.sources.preparePoolRestore(ctx, next.Nodes)
	if err != nil {
		p.mu.Lock()
		if p.saved.Trial != nil {
			p.saved.ReleaseError = "rollback_pending"
		}
		p.eventLocked("rollback_pending", "", "rollback_dependency_unavailable")
		p.mu.Unlock()
		return State{}, 409, "rollback_dependency_unavailable"
	}
	state, status, code := p.persistRelease(next, event)
	if finish != nil {
		finish(code == "")
	}
	return state, status, code
}

// Caller holds configMu. Disk IO stays outside mu, as with ordinary mutations.
func (p *Pool) persistRelease(next savedConfig, event string) (State, int, string) {
	if validateSaved(next) != nil || validateReleaseJournal(next) != nil {
		return State{}, 400, "invalid_configuration"
	}
	if validateOriginNodes(p.origin, next) != nil {
		return State{}, 400, "https_origin_required"
	}
	durability, err := writeSaved(p.cfg.DataDir, next)
	if err != nil {
		return State{}, 500, "persistence_failed"
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	state := p.installCommittedLocked(next, durability, event)
	if next.Trial == nil {
		p.trialDeadline = time.Time{}
	}
	p.wakeRelease()
	return state, 200, ""
}
func (p *Pool) wakeRelease() {
	select {
	case p.releaseWake <- struct{}{}:
	default:
	}
}

func (p *Pool) releaseLoop() {
	defer p.wg.Done()
	for {
		p.mu.Lock()
		due := p.trialDeadline
		closed := p.closed
		p.mu.Unlock()
		if closed {
			return
		}
		var timer *time.Timer
		var tick <-chan time.Time
		if !due.IsZero() {
			timer = time.NewTimer(max(time.Duration(0), time.Until(due)))
			tick = timer.C
		}
		select {
		case <-p.ctx.Done():
			if timer != nil {
				timer.Stop()
			}
			return
		case <-p.releaseWake:
			if timer != nil {
				timer.Stop()
			}
		case <-tick:
			if !p.expireRelease() {
				// Retain the persisted responsibility and retry a failed disk write.
				retry := time.NewTimer(time.Second)
				select {
				case <-retry.C:
				case <-p.ctx.Done():
					retry.Stop()
					return
				}
			}
		}
	}
}

func (p *Pool) expireRelease() bool {
	p.configMu.Lock()
	defer p.configMu.Unlock()
	p.mu.Lock()
	if p.closed || p.saved.Trial == nil || time.Now().Before(p.trialDeadline) {
		p.mu.Unlock()
		return true
	}
	next, err := recoverPendingRelease(p.saved)
	if err != nil {
		p.saved.ReleaseError = "rollback_pending"
		p.mu.Unlock()
		return false
	}
	if len(next.History) > 0 && next.History[len(next.History)-1].Reason == "restart_rollback" {
		next.History[len(next.History)-1].Reason = "deadline_rollback"
	}
	p.mu.Unlock()
	_, _, code := p.persistRestoredRelease(next, "trial_expired_rollback")
	if code != "" {
		p.mu.Lock()
		p.saved.ReleaseError = "rollback_pending"
		p.eventLocked("rollback_pending", "", "persistence_failed")
		p.mu.Unlock()
		return false
	}
	return true
}

func (p *Pool) serveReleases(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/v1/releases/") {
		return false
	}
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		failure(w, 405, "method_not_allowed")
		return true
	}
	var state State
	var status int
	var code string
	switch r.URL.Path {
	case "/v1/releases/prepare":
		var body struct {
			ExpectedRevision *int64          `json:"expected_revision"`
			Action           string          `json:"action"`
			Payload          json.RawMessage `json:"payload"`
		}
		if !decode(w, r, &body) {
			return true
		}
		innerExpected, apply, parseCode := decodeMutation(body.Action, body.Payload)
		if parseCode != "" {
			failure(w, 400, parseCode)
			return true
		}
		if innerExpected != nil {
			failure(w, 400, "invalid_request")
			return true
		}
		state, status, code = p.prepareCandidate(body.ExpectedRevision, body.Action, apply)
	case "/v1/releases/start":
		var body struct {
			ExpectedRevision *int64 `json:"expected_revision"`
			PreviewID        string `json:"preview_id"`
		}
		if !decode(w, r, &body) {
			return true
		}
		state, status, code = p.startRelease(body.ExpectedRevision, body.PreviewID)
	case "/v1/releases/confirm":
		var body struct {
			ExpectedRevision *int64 `json:"expected_revision"`
			TrialID          string `json:"trial_id"`
		}
		if !decode(w, r, &body) {
			return true
		}
		state, status, code = p.confirmRelease(body.ExpectedRevision, body.TrialID)
	case "/v1/releases/rollback":
		var body struct {
			ExpectedRevision *int64 `json:"expected_revision"`
			TrialID          string `json:"trial_id"`
			VersionID        string `json:"version_id"`
		}
		if !decode(w, r, &body) {
			return true
		}
		state, status, code = p.rollbackRelease(body.ExpectedRevision, body.TrialID, body.VersionID)
	default:
		failure(w, 404, "not_found")
		return true
	}
	if code != "" {
		failure(w, status, code)
	} else {
		answer(w, status, state)
	}
	return true
}
