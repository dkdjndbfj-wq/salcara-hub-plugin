package edgepool

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"
)

type SourceNodeView struct {
	Key       string `json:"key"`
	Name      string `json:"name"`
	Protocol  string `json:"protocol"`
	Supported bool   `json:"supported"`
	Reason    string `json:"reason"`
	Change    string `json:"change"`
	Selected  bool   `json:"selected"`
}
type sourceNode struct {
	SourceNodeView
	Native string         `json:"native,omitempty"`
	Proxy  map[string]any `json:"proxy,omitempty"`
}
type sourceCandidate struct {
	ID        string       `json:"id"`
	CreatedAt int64        `json:"created_at"`
	Nodes     []sourceNode `json:"nodes"`
	Removed   int          `json:"removed"`
	Digest    string       `json:"digest"`
}
type sourceRecord struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	URL         string           `json:"url"`
	Enabled     bool             `json:"enabled"`
	AutoApply   bool             `json:"auto_apply"`
	IntervalMS  int64            `json:"interval_ms"`
	LastAttempt int64            `json:"last_attempt"`
	LastSuccess int64            `json:"last_success"`
	LastApplied int64            `json:"last_applied"`
	NextRefresh int64            `json:"next_refresh"`
	Status      string           `json:"status"`
	Error       string           `json:"error"`
	Failures    int              `json:"failures"`
	Candidate   *sourceCandidate `json:"candidate,omitempty"`
	Active      []sourceNode     `json:"active"`
}
type sourceDisk struct {
	Revision int64               `json:"revision"`
	Sources  []sourceRecord      `json:"sources"`
	Kernels  []*kernelGeneration `json:"kernels,omitempty"`
}
type SourceView struct {
	ID          string           `json:"id"`
	Name        string           `json:"name"`
	Enabled     bool             `json:"enabled"`
	AutoApply   bool             `json:"auto_apply"`
	IntervalMS  int64            `json:"interval_ms"`
	HasURL      bool             `json:"has_url"`
	LastAttempt int64            `json:"last_attempt"`
	LastSuccess int64            `json:"last_success"`
	LastApplied int64            `json:"last_applied"`
	NextRefresh int64            `json:"next_refresh"`
	Status      string           `json:"status"`
	Error       string           `json:"error"`
	CandidateID string           `json:"candidate_id"`
	CandidateAt int64            `json:"candidate_at"`
	Removed     int              `json:"removed"`
	Nodes       []SourceNodeView `json:"nodes"`
}
type KernelView struct {
	Version     string `json:"version"`
	Installed   bool   `json:"installed"`
	Status      string `json:"status"`
	Error       string `json:"error"`
	Generations int    `json:"generations"`
}
type SourcesState struct {
	SourceRevision int64        `json:"source_revision"`
	Sources        []SourceView `json:"sources"`
	Kernel         KernelView   `json:"kernel"`
	Busy           bool         `json:"busy"`
}
type sourceManager struct {
	mu     sync.Mutex
	pool   *Pool
	key    []byte
	saved  sourceDisk
	busy   bool
	closed bool
	kernel *kernelManager
	probe  func(context.Context, *Pool, []nodeConfig) error
}

func initSourceManager(p *Pool) error {
	m := &sourceManager{pool: p, saved: sourceDisk{Revision: 1, Sources: []sourceRecord{}}, probe: probeSourceCandidates}
	if err := m.load(); err != nil {
		return err
	}
	m.kernel = newKernelManager(p.cfg.DataDir, p.ctx)
	p.sources = m
	m.reconcileActive()
	if err := m.restoreKernel(); err != nil {
		m.kernel.setError("restore_failed")
	}
	p.wg.Add(1)
	go func() { defer p.wg.Done(); m.loop() }()
	return nil
}
func (m *sourceManager) Close() { m.mu.Lock(); m.closed = true; m.mu.Unlock(); m.kernel.Close() }
func (m *sourceManager) snapshot() SourcesState {
	kernel := m.kernel.View()
	m.mu.Lock()
	defer m.mu.Unlock()
	out := SourcesState{SourceRevision: m.saved.Revision, Sources: []SourceView{}, Busy: m.busy, Kernel: kernel}
	for _, s := range m.saved.Sources {
		v := SourceView{ID: s.ID, Name: s.Name, Enabled: s.Enabled, AutoApply: s.AutoApply, IntervalMS: s.IntervalMS, HasURL: s.URL != "", LastAttempt: s.LastAttempt, LastSuccess: s.LastSuccess, LastApplied: s.LastApplied, NextRefresh: s.NextRefresh, Status: s.Status, Error: s.Error, Nodes: []SourceNodeView{}}
		if c := s.Candidate; c != nil {
			v.CandidateID = c.ID
			v.CandidateAt = c.CreatedAt
			v.Removed = c.Removed
			for _, n := range c.Nodes {
				v.Nodes = append(v.Nodes, n.SourceNodeView)
			}
		}
		out.Sources = append(out.Sources, v)
	}
	return out
}
func (m *sourceManager) index(id string) int {
	for i := range m.saved.Sources {
		if m.saved.Sources[i].ID == id {
			return i
		}
	}
	return -1
}
func (m *sourceManager) clone() sourceDisk {
	next := m.saved
	next.Sources = append([]sourceRecord{}, m.saved.Sources...)
	next.Revision++
	return next
}
func (m *sourceManager) save(expected int64, s sourceRecord) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.busy {
		return errors.New("source_busy")
	}
	if expected != m.saved.Revision {
		return errors.New("revision_conflict")
	}
	i := m.index(s.ID)
	if s.ID != "" && i < 0 {
		return errors.New("source_not_found")
	}
	if s.Name == "" || len(s.Name) > 160 || strings.ContainsAny(s.Name, "\r\n\x00") || s.IntervalMS < 300000 || s.IntervalMS > 604800000 {
		return errors.New("invalid_source")
	}
	if s.URL != "" {
		if _, err := subscriptionURL(s.URL); err != nil {
			return err
		}
	} else if i >= 0 {
		s.URL = m.saved.Sources[i].URL
	} else {
		return errors.New("subscription_url_invalid")
	}
	next := m.clone()
	if i < 0 {
		if len(next.Sources) >= 16 {
			return errors.New("source_limit")
		}
		s.ID = sourceID()
		s.Status = "idle"
		s.Active = []sourceNode{}
		s.NextRefresh = time.Now().UnixMilli()
		next.Sources = append(next.Sources, s)
	} else {
		old := next.Sources[i]
		old.Name = s.Name
		old.Enabled = s.Enabled
		old.AutoApply = s.AutoApply
		old.IntervalMS = s.IntervalMS
		if old.URL != s.URL {
			old.URL = s.URL
			old.Candidate = nil
			old.Status = "idle"
			old.Error = ""
		}
		old.NextRefresh = time.Now().Add(time.Duration(s.IntervalMS) * time.Millisecond).UnixMilli()
		next.Sources[i] = old
	}
	return m.persistLocked(next)
}
func sourceNodeKey(raw string) string {
	u, _ := url.Parse(raw)
	user := ""
	if u.User != nil {
		user = u.User.Username()
	}
	sum := sha256.Sum256([]byte(strings.ToLower(u.Scheme+"|"+u.Host) + "|" + user))
	return hex.EncodeToString(sum[:16])
}
func sourceDigest(n sourceNode) string {
	if n.Native != "" {
		sum := sha256.Sum256([]byte(n.Native))
		return hex.EncodeToString(sum[:])
	}
	return kernelNodeDigest(n.Proxy)
}
func (m *sourceManager) parse(body []byte, old []sourceNode) (*sourceCandidate, error) {
	nodes, err := parseSourceNodes(body)
	if err != nil {
		return nil, err
	}
	if len(nodes) == 0 {
		return nil, errors.New("subscription_no_supported_nodes")
	}
	previous := map[string]sourceNode{}
	for _, n := range old {
		previous[n.Key] = n
	}
	seen := map[string]bool{}
	supported := 0
	for i := range nodes {
		n := &nodes[i]
		if n.Key == "" {
			continue
		}
		if seen[n.Key] {
			return nil, errors.New("source_identity_ambiguous")
		}
		seen[n.Key] = true
		if n.Supported {
			supported++
		}
		n.Change = "added"
		if before, ok := previous[n.Key]; ok {
			n.Selected = true
			n.Change = "unchanged"
			if sourceDigest(before) != sourceDigest(*n) {
				n.Change = "updated"
			} else if before.Name != n.Name {
				n.Change = "renamed"
			}
		}
	}
	if supported == 0 {
		return nil, errors.New("subscription_no_supported_nodes")
	}
	removed := 0
	for key := range previous {
		if !seen[key] {
			removed++
		}
	}
	sum := sha256.Sum256(body)
	return &sourceCandidate{ID: sourceID(), CreatedAt: time.Now().UnixMilli(), Nodes: nodes, Removed: removed, Digest: hex.EncodeToString(sum[:])}, nil
}
func (m *sourceManager) loop() {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-m.pool.ctx.Done():
			return
		case <-tick.C:
			m.kernel.reap()
			m.mu.Lock()
			id := ""
			if !m.busy && !m.closed {
				now := time.Now().UnixMilli()
				for _, s := range m.saved.Sources {
					if s.Enabled && s.NextRefresh > 0 && s.NextRefresh <= now {
						id = s.ID
						break
					}
				}
			}
			m.mu.Unlock()
			if id != "" {
				_ = m.refresh(id)
			}
		}
	}
}
func (m *sourceManager) refresh(id string) error {
	m.mu.Lock()
	i := m.index(id)
	if i < 0 {
		m.mu.Unlock()
		return errors.New("source_not_found")
	}
	if m.busy || m.closed {
		m.mu.Unlock()
		return errors.New("source_busy")
	}
	next := m.clone()
	next.Sources[i].Status = "refreshing"
	next.Sources[i].Error = ""
	next.Sources[i].LastAttempt = time.Now().UnixMilli()
	if err := m.persistLocked(next); err != nil {
		m.mu.Unlock()
		return err
	}
	m.busy = true
	s := m.saved.Sources[i]
	m.pool.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.pool.wg.Done()
		body, err := fetchSubscription(m.pool.ctx, m.pool.subscriptions.client, s.URL)
		var c *sourceCandidate
		if err == nil {
			c, err = m.parse(body, s.Active)
		}
		m.mu.Lock()
		defer m.mu.Unlock()
		m.busy = false
		i = m.index(id)
		if m.closed || i < 0 {
			return
		}
		next := m.clone()
		s := &next.Sources[i]
		s.Status = "review"
		s.Error = ""
		delay := time.Duration(s.IntervalMS) * time.Millisecond
		if err != nil {
			s.Status = "failed"
			s.Error = err.Error()
			s.Failures++
			delay = time.Minute * time.Duration(1<<min(s.Failures-1, 6))
		} else {
			s.Candidate = c
			s.LastSuccess = time.Now().UnixMilli()
			s.Failures = 0
		}
		s.NextRefresh = time.Now().Add(delay + time.Duration(time.Now().UnixNano()%int64(max(delay/10, time.Millisecond)))).UnixMilli()
		if e := m.persistLocked(next); e != nil {
			m.saved.Sources[i].Status = "storage_error"
			m.saved.Sources[i].Error = "source_storage_failed"
			m.saved.Sources[i].NextRefresh = time.Now().Add(time.Minute).UnixMilli()
			return
		}
		if err == nil && s.AutoApply && len(s.Active) > 0 && c.Removed == 0 {
			keys := []string{}
			for _, n := range c.Nodes {
				if n.Selected {
					keys = append(keys, n.Key)
				}
			}
			rev := m.saved.Revision
			candidateID := c.ID
			go func() { _ = m.apply(id, candidateID, rev, 0, keys, true) }()
		}
	}()
	return nil
}
func (p *Pool) serveSources(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/v1/sources/") && !strings.HasPrefix(r.URL.Path, "/v1/kernel/") {
		return false
	}
	m := p.sources
	if m == nil {
		failure(w, 503, "source_unavailable")
		return true
	}
	if r.URL.Path == "/v1/sources/state" && r.Method == http.MethodGet {
		answer(w, 200, m.snapshot())
		return true
	}
	if r.Method != http.MethodPost {
		failure(w, 405, "method_not_allowed")
		return true
	}
	var b struct {
		ExpectedSourceRevision int64    `json:"expected_source_revision"`
		ExpectedRevision       int64    `json:"expected_revision"`
		ID                     string   `json:"id"`
		Name                   string   `json:"name"`
		URL                    string   `json:"url"`
		Enabled                bool     `json:"enabled"`
		AutoApply              bool     `json:"auto_apply"`
		IntervalMS             int64    `json:"interval_ms"`
		CandidateID            string   `json:"candidate_id"`
		NodeKeys               []string `json:"node_keys"`
	}
	if !decode(w, r, &b) {
		return true
	}
	var err error
	switch r.URL.Path {
	case "/v1/sources/save":
		err = m.save(b.ExpectedSourceRevision, sourceRecord{ID: b.ID, Name: strings.TrimSpace(b.Name), URL: b.URL, Enabled: b.Enabled, AutoApply: b.AutoApply, IntervalMS: b.IntervalMS})
	case "/v1/sources/refresh":
		err = m.refresh(b.ID)
	case "/v1/sources/apply":
		err = m.apply(b.ID, b.CandidateID, b.ExpectedSourceRevision, b.ExpectedRevision, b.NodeKeys, false)
	case "/v1/sources/delete":
		err = m.remove(b.ID, b.ExpectedSourceRevision)
	case "/v1/kernel/install":
		err = m.installKernel()
	default:
		failure(w, 404, "not_found")
		return true
	}
	if err != nil {
		code := 400
		if err.Error() == "revision_conflict" || err.Error() == "source_busy" {
			code = 409
		}
		failure(w, code, err.Error())
	} else {
		answer(w, 202, m.snapshot())
	}
	return true
}

func (m *sourceManager) remove(id string, expected int64) error {
	// Serialize with commit/rollback, including versions that still need this
	// source's encrypted credentials. Never delete behind a pending restore.
	m.pool.configMu.Lock()
	defer m.pool.configMu.Unlock()
	m.pool.mu.Lock()
	referenced := m.pool.sourceReferencedLocked(id)
	m.pool.mu.Unlock()
	if referenced {
		return errors.New("source_in_use")
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.busy || m.closed {
		return errors.New("source_busy")
	}
	if expected != m.saved.Revision {
		return errors.New("revision_conflict")
	}
	i := m.index(id)
	if i < 0 {
		return errors.New("source_not_found")
	}
	next := m.clone()
	next.Sources = append(next.Sources[:i], next.Sources[i+1:]...)
	return m.persistLocked(next)
}

// Retired connections still hold this subscription's identity even after its
// configuration versions have aged out. Keeping the source record until they
// drain also bounds the number of visible retired groups by the source limit.
func (p *Pool) sourceReferencedLocked(id string) bool {
	if sourceReferenced(p.saved, id) {
		return true
	}
	stats := p.sourceRuntime[id]
	return stats != nil && (stats.ActiveRequests > 0 || stats.ActiveStreams > 0)
}

func sourceReferenced(s savedConfig, id string) bool {
	limitsReference := func(limits []SourceLimit) bool {
		for _, limit := range limits {
			if limit.SourceID == id {
				return true
			}
		}
		return false
	}
	check := func(nodes []nodeConfig) bool {
		for _, n := range nodes {
			if n.SourceID == id {
				return true
			}
		}
		return false
	}
	if check(s.Nodes) || limitsReference(s.SourceLimits) {
		return true
	}
	if s.Trial != nil && (check(s.Trial.Previous.Nodes) || limitsReference(s.Trial.Previous.SourceLimits)) {
		return true
	}
	for _, version := range s.History {
		if check(version.Snapshot.Nodes) || limitsReference(version.Snapshot.SourceLimits) {
			return true
		}
	}
	return false
}

// Implementations in source_publish.go and kernel.go keep long-running work out
// of the configuration lock and return only bounded, secret-free status.
