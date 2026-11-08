package edgepool

import (
	"errors"
	"sort"
	"strings"
)

// SourceLimit is an administrator-set aggregate for one subscription identity.
// Absence means no additional cap; it never guesses shared bandwidth from ASN.
type SourceLimit struct {
	SourceID    string `json:"source_id"`
	MaxRequests int    `json:"max_requests"`
	MaxStreams  int    `json:"max_streams"`
}
type SourceGroupView struct {
	SourceID         string   `json:"source_id"`
	Limited          bool     `json:"limited"`
	MaxRequests      int      `json:"max_requests"`
	MaxStreams       int      `json:"max_streams"`
	NodeIDs          []string `json:"node_ids"`
	Runtime          Runtime  `json:"runtime"`
	RequestAvailable bool     `json:"request_available"`
	StreamAvailable  bool     `json:"stream_available"`
	Retired          bool     `json:"retired"`
}

func validateSourceLimits(limits []SourceLimit) error {
	if len(limits) > 32 {
		return errors.New("invalid_source_limits")
	}
	seen := map[string]bool{}
	for _, limit := range limits {
		if len(limit.SourceID) != 32 || strings.Trim(limit.SourceID, "0123456789abcdef") != "" || seen[limit.SourceID] || limit.MaxRequests < 1 || limit.MaxRequests > 4096 || limit.MaxStreams < 1 || limit.MaxStreams > 4096 {
			return errors.New("invalid_source_limits")
		}
		seen[limit.SourceID] = true
	}
	return nil
}

// These ledgers are separate from route generations. A lease captures its
// original ledger, so removing, moving or replacing a node cannot erase work
// already in flight or charge its completion to a different subscription.
func (p *Pool) installSourceLimitsLocked(s savedConfig) {
	if p.sourceRuntime == nil {
		p.sourceRuntime = map[string]*Runtime{}
	}
	p.sourceLimits = map[string]SourceLimit{}
	ensure := func(id string) {
		if id != "" && p.sourceRuntime[id] == nil {
			p.sourceRuntime[id] = &Runtime{}
		}
	}
	for _, limit := range s.SourceLimits {
		p.sourceLimits[limit.SourceID] = limit
		ensure(limit.SourceID)
	}
	for _, n := range s.Nodes {
		ensure(n.SourceID)
	}
}
func (p *Pool) pruneSourceRuntimesLocked() {
	current := map[string]bool{}
	for _, n := range p.saved.Nodes {
		current[n.SourceID] = true
	}
	for id, stats := range p.sourceRuntime {
		_, limited := p.sourceLimits[id]
		if !current[id] && !limited && stats.ActiveRequests == 0 && stats.ActiveStreams == 0 {
			delete(p.sourceRuntime, id)
		}
	}
}
func (p *Pool) sourceCapacityLocked(r *route, stream bool) bool {
	limit, limited := p.sourceLimits[r.config.SourceID]
	if !limited {
		return true
	}
	if r.source == nil {
		return false
	}
	if stream {
		return r.source.ActiveStreams < limit.MaxStreams
	}
	return r.source.ActiveRequests < limit.MaxRequests
}
func (p *Pool) sourceGroupsLocked() []SourceGroupView {
	groups := make([]SourceGroupView, 0, len(p.sourceRuntime))
	for id, stats := range p.sourceRuntime {
		limit, limited := p.sourceLimits[id]
		g := SourceGroupView{SourceID: id, Limited: limited, MaxRequests: limit.MaxRequests, MaxStreams: limit.MaxStreams, NodeIDs: []string{}, Runtime: *stats, RequestAvailable: !limited || stats.ActiveRequests < limit.MaxRequests, StreamAvailable: !limited || stats.ActiveStreams < limit.MaxStreams}
		for _, n := range p.saved.Nodes {
			if n.SourceID == id {
				g.NodeIDs = append(g.NodeIDs, n.ID)
			}
		}
		sort.Strings(g.NodeIDs)
		// Never expose an unused zero-occupancy ledger as a current source.
		if len(g.NodeIDs) == 0 && !limited && stats.ActiveRequests == 0 && stats.ActiveStreams == 0 {
			continue
		}
		g.Retired = len(g.NodeIDs) == 0 && (stats.ActiveRequests > 0 || stats.ActiveStreams > 0)
		groups = append(groups, g)
	}
	sort.Slice(groups, func(i, j int) bool { return groups[i].SourceID < groups[j].SourceID })
	return groups
}
