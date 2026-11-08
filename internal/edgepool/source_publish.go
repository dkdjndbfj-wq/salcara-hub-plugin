package edgepool

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

func (m *sourceManager) installKernel() error {
	m.mu.Lock()
	if m.busy || m.closed {
		m.mu.Unlock()
		return errors.New("source_busy")
	}
	m.busy = true
	m.pool.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.pool.wg.Done()
		err := m.kernel.install()
		if err == nil {
			m.pool.configMu.Lock()
			m.pool.mu.Lock()
			nodes := append([]nodeConfig{}, m.pool.saved.Nodes...)
			m.pool.mu.Unlock()
			var finish func(bool)
			finish, err = m.preparePoolRestoreOwned(m.pool.ctx, nodes, true)
			if err == nil {
				finish(true)
			}
			m.pool.configMu.Unlock()
		}
		if err != nil {
			m.kernel.setError(err.Error())
		}
		m.mu.Lock()
		m.busy = false
		m.mu.Unlock()
	}()
	return nil
}
func (m *sourceManager) restoreKernel() error {
	finish, err := m.preparePoolRestore(m.pool.ctx, m.pool.saved.Nodes)
	if err == nil {
		finish(true)
	}
	return err
}

// Caller serializes configuration commits, but holds no pool state lock.
// The old runtime stays available until the durable pool pointer is committed.
func (m *sourceManager) preparePoolRestore(ctx context.Context, nodes []nodeConfig) (func(bool), error) {
	return m.preparePoolRestoreOwned(ctx, nodes, false)
}
func (m *sourceManager) preparePoolRestoreOwned(ctx context.Context, nodes []nodeConfig, ownsBusy bool) (func(bool), error) {
	m.mu.Lock()
	if m.closed || m.busy && !ownsBusy {
		m.mu.Unlock()
		return nil, errors.New("source_busy")
	}
	m.busy = true
	var selected *kernelGeneration
	for _, n := range nodes {
		if n.SourceID == "" {
			continue
		}
		if m.index(n.SourceID) < 0 {
			if !ownsBusy {
				m.busy = false
			}
			m.mu.Unlock()
			return nil, errors.New("source_snapshot_missing")
		}
		if !strings.Contains(n.ProxyURL, "@127.0.0.1:") {
			continue
		}
		var match *kernelGeneration
		for _, g := range m.saved.Kernels {
			if g.URLs[n.ID] == n.ProxyURL {
				match = g
				break
			}
		}
		if match == nil || selected != nil && selected.ID != match.ID {
			if !ownsBusy {
				m.busy = false
			}
			m.mu.Unlock()
			return nil, errors.New("kernel_generation_missing")
		}
		selected = match
	}
	m.mu.Unlock()
	created := false
	var err error
	if selected != nil {
		selected, created, err = m.kernel.Restore(ctx, selected)
	}
	if err != nil {
		m.mu.Lock()
		if !ownsBusy {
			m.busy = false
		}
		m.mu.Unlock()
		return nil, err
	}
	var once sync.Once
	return func(committed bool) {
		once.Do(func() {
			if committed {
				m.kernel.Activate(selected)
			} else if created {
				m.kernel.Discard(selected)
			}
			m.kernel.ReleaseRestore(selected)
			m.pool.mu.Lock()
			activeNodes := append([]nodeConfig{}, m.pool.saved.Nodes...)
			m.pool.mu.Unlock()
			m.mu.Lock()
			if committed {
				m.reconcileNodes(activeNodes)
				_ = m.persistLocked(m.clone())
			}
			if !ownsBusy {
				m.busy = false
			}
			m.mu.Unlock()
		})
	}, nil
}

// A crash can occur after the pool pointer commits but before source metadata
// does. Recover exact source payloads from the staged generation/candidate;
// never guess an old password from a matching display name.
func (m *sourceManager) reconcileActive() {
	m.reconcileNodes(m.pool.saved.Nodes)
}
func (m *sourceManager) reconcileNodes(nodes []nodeConfig) {
	for i := range m.saved.Sources {
		s := &m.saved.Sources[i]
		active := []sourceNode{}
		for _, n := range nodes {
			if n.SourceID != s.ID {
				continue
			}
			found := false
			for _, g := range m.saved.Kernels {
				if g.URLs[n.ID] == n.ProxyURL {
					if raw, ok := g.Members[n.ID]; ok {
						active = append(active, raw)
						found = true
						break
					}
				}
			}
			if !found {
				if u, err := url.Parse(n.ProxyURL); err == nil && n.Imported && n.SourceKey != "" {
					active = append(active, sourceNode{SourceNodeView: SourceNodeView{Key: n.SourceKey, Name: n.Name, Protocol: u.Scheme, Supported: true, Selected: true}, Native: n.ProxyURL})
					found = true
				}
			}
			if !found {
				candidates := append([]sourceNode{}, s.Active...)
				if s.Candidate != nil {
					candidates = append(candidates, s.Candidate.Nodes...)
				}
				for _, raw := range candidates {
					if raw.Key == n.SourceKey && raw.Native != "" && raw.Native == n.ProxyURL {
						active = append(active, raw)
						found = true
						break
					}
				}
			}
			if !found {
				s.Status = "failed"
				s.Error = "source_snapshot_missing"
			}
		}
		s.Active = active
	}
}

func (m *sourceManager) apply(id, candidateID string, sourceRevision, poolRevision int64, keys []string, automatic bool) error {
	m.mu.Lock()
	i := m.index(id)
	if i < 0 {
		m.mu.Unlock()
		return errors.New("source_not_found")
	}
	if m.closed || m.busy {
		m.mu.Unlock()
		return errors.New("source_busy")
	}
	if sourceRevision != m.saved.Revision {
		m.mu.Unlock()
		return errors.New("revision_conflict")
	}
	s := m.saved.Sources[i]
	c := s.Candidate
	if c == nil || c.ID != candidateID || time.Since(time.UnixMilli(c.CreatedAt)) > 24*time.Hour {
		m.mu.Unlock()
		return errors.New("source_candidate_expired")
	}
	if len(keys) == 0 || len(keys) > maxNodes {
		m.mu.Unlock()
		return errors.New("invalid_source_selection")
	}
	chosen := map[string]sourceNode{}
	for _, key := range keys {
		if _, exists := chosen[key]; exists {
			m.mu.Unlock()
			return errors.New("invalid_source_selection")
		}
		for _, n := range c.Nodes {
			if n.Key == key && n.Supported {
				chosen[key] = n
				break
			}
		}
		if _, ok := chosen[key]; !ok {
			m.mu.Unlock()
			return errors.New("invalid_source_selection")
		}
	}
	next := m.clone()
	next.Sources[i].Status = "applying"
	next.Sources[i].Error = ""
	if err := m.persistLocked(next); err != nil {
		m.mu.Unlock()
		return err
	}
	m.busy = true
	records := append([]sourceRecord{}, m.saved.Sources...)
	m.pool.wg.Add(1)
	m.mu.Unlock()
	go func() {
		defer m.pool.wg.Done()
		err := m.publish(id, chosen, records, poolRevision, automatic)
		m.mu.Lock()
		defer m.mu.Unlock()
		m.busy = false
		i := m.index(id)
		if i < 0 || m.closed {
			return
		}
		next := m.clone()
		s := &next.Sources[i]
		if err != nil {
			s.Status = "failed"
			s.Error = err.Error()
		} else {
			s.Status = "applied"
			s.Error = ""
			s.LastApplied = time.Now().UnixMilli()
			s.Active = []sourceNode{}
			for _, n := range c.Nodes {
				if _, ok := chosen[n.Key]; ok {
					n.Selected = true
					s.Active = append(s.Active, n)
				}
			}
			s.Candidate = nil
		}
		if e := m.persistLocked(next); e != nil {
			if err == nil {
				m.saved.Sources[i].Active = s.Active
			}
			m.saved.Sources[i].Status = "storage_error"
			m.saved.Sources[i].Error = "source_storage_failed"
		}
	}()
	return nil
}
func (m *sourceManager) publish(id string, chosen map[string]sourceNode, records []sourceRecord, revision int64, automatic bool) error {
	p := m.pool
	p.mu.Lock()
	base := p.saved
	base.Nodes = append([]nodeConfig{}, p.saved.Nodes...)
	p.mu.Unlock()
	if revision == 0 && automatic {
		revision = base.Revision
	}
	if revision != base.Revision {
		return errors.New("revision_conflict")
	}
	bySource := map[string]map[string]sourceNode{}
	for _, s := range records {
		bySource[s.ID] = map[string]sourceNode{}
		for _, n := range s.Active {
			bySource[s.ID][n.Key] = n
		}
	}
	bySource[id] = chosen
	nextNodes := []nodeConfig{}
	present := map[string]bool{}
	for _, old := range base.Nodes {
		if old.SourceID != id {
			nextNodes = append(nextNodes, old)
			continue
		}
		if _, ok := chosen[old.SourceKey]; ok {
			nextNodes = append(nextNodes, old)
			present[old.SourceKey] = true
		}
	}
	keys := make([]string, 0, len(chosen))
	for key := range chosen {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		n := chosen[key]
		if present[key] {
			continue
		}
		nextNodes = append(nextNodes, nodeConfig{ID: "node-" + sourceID()[:16], Name: n.Name, Enabled: true, Weight: 1, MaxRequests: 64, MaxStreams: 16, SourceID: id, SourceKey: key})
	}
	if len(nextNodes) > maxNodes {
		return errors.New("node_limit")
	}
	kernelNodes := map[string]map[string]any{}
	for _, n := range nextNodes {
		if n.SourceID != "" {
			raw, ok := bySource[n.SourceID][n.SourceKey]
			if !ok {
				return errors.New("source_snapshot_missing")
			}
			if raw.Proxy != nil {
				kernelNodes[n.ID] = raw.Proxy
			}
		}
	}
	ctx, cancel := context.WithTimeout(p.ctx, 90*time.Second)
	defer cancel()
	var generation *kernelGeneration
	var err error
	if len(kernelNodes) > 0 {
		generation, err = m.kernel.Prepare(ctx, kernelNodes)
		if err != nil {
			return err
		}
	}
	committed := false
	defer func() {
		if !committed {
			m.kernel.Discard(generation)
		}
	}()
	targets := []nodeConfig{}
	for i := range nextNodes {
		n := &nextNodes[i]
		if n.SourceID == "" {
			continue
		}
		raw := bySource[n.SourceID][n.SourceKey]
		if raw.Native != "" {
			n.ProxyURL = raw.Native
			n.Imported = true
		} else {
			n.ProxyURL = generation.URLs[n.ID]
			n.Imported = false
		}
		if n.Enabled && (n.SourceID == id || generation != nil) {
			targets = append(targets, *n)
		}
	}
	if p.origin.Scheme != "https" {
		return errors.New("https_origin_required")
	}
	if err = m.probe(ctx, p, targets); err != nil {
		return err
	}
	final := base
	final.Nodes = nextNodes
	if final.SelectedID != "" && !enabledNode(final, final.SelectedID) {
		final.SelectedID = firstEnabled(final)
	}
	verified := map[string]bool{}
	for _, n := range targets {
		verified[n.ID] = true
	}
	usable := false
	eligible := []nodeConfig{}
	for _, n := range nextNodes {
		if !n.Enabled || n.Draining {
			continue
		}
		if final.Mode == "manual" {
			if n.ID != final.SelectedID {
				continue
			}
		} else if final.SelectionPolicy.RegionMode == "only" {
			match := false
			for _, region := range final.SelectionPolicy.PreferredRegions {
				if strings.EqualFold(p.regionFor(n, final.SelectionPolicy), region) {
					match = true
				}
			}
			if !match {
				continue
			}
		}
		if verified[n.ID] {
			usable = true
			break
		}
		eligible = append(eligible, n)
	}
	if !usable {
		for _, n := range eligible {
			if m.probe(ctx, p, []nodeConfig{n}) == nil {
				usable = true
				break
			}
			if ctx.Err() != nil {
				break
			}
		}
	}
	if !usable {
		return errors.New("no_eligible_route")
	}
	// Stage both generations before the pool pointer changes. On restart the pool
	// file is authoritative; restoreKernel only starts referenced listeners.
	if generation != nil {
		generation.Members = map[string]sourceNode{}
		for _, node := range nextNodes {
			if _, ok := generation.URLs[node.ID]; ok {
				generation.Members[node.ID] = bySource[node.SourceID][node.SourceKey]
			}
		}
		m.mu.Lock()
		next := m.clone()
		next.Kernels = []*kernelGeneration{generation}
		for _, old := range m.saved.Kernels {
			if old.ID != generation.ID && kernelReferenced(base, old) {
				next.Kernels = append(next.Kernels, old)
			}
		}
		err = m.persistLocked(next)
		m.mu.Unlock()
		if err != nil {
			return err
		}
	}
	_, _, code := p.commitConfig(&revision, func(s *savedConfig) string {
		s.Nodes = nextNodes
		if s.SelectedID != "" && !enabledNode(*s, s.SelectedID) {
			s.SelectedID = firstEnabled(*s)
		}
		return ""
	})
	if code != "" {
		return errors.New(code)
	}
	committed = true
	m.kernel.Activate(generation)
	return nil
}
func probeSourceCandidates(ctx context.Context, p *Pool, nodes []nodeConfig) error {
	var wg sync.WaitGroup
	sem := make(chan struct{}, 4)
	fail := make(chan bool, len(nodes))
	for _, n := range nodes {
		wg.Add(1)
		go func(n nodeConfig) {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
				defer func() { <-sem }()
			case <-ctx.Done():
				fail <- true
				return
			}
			releaseSlot, ok := p.acquireProbeSlot(ctx)
			if !ok {
				fail <- true
				return
			}
			defer releaseSlot()
			probeCtx, cancel := context.WithTimeout(ctx, 6*time.Second)
			defer cancel()
			proxy, err := parseProxy(n.ProxyURL)
			if err != nil {
				fail <- true
				return
			}
			transport := transportFor(proxy, true, n.Imported)
			defer transport.CloseIdleConnections()
			client := &http.Client{Transport: transport, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
			u := *p.origin
			u.Path = "/salcara-hub/statusz"
			req, _ := http.NewRequestWithContext(probeCtx, "GET", u.String(), nil)
			resp, err := client.Do(req)
			if err != nil {
				fail <- true
				return
			}
			defer resp.Body.Close()
			raw, err := io.ReadAll(io.LimitReader(resp.Body, 4097))
			var value struct {
				Status string `json:"status"`
			}
			if err != nil || resp.StatusCode != 200 || len(raw) > 4096 || json.Unmarshal(raw, &value) != nil || value.Status != "available" {
				fail <- true
			}
		}(n)
	}
	wg.Wait()
	if len(fail) > 0 {
		return errors.New("source_candidate_unreachable")
	}
	return nil
}
