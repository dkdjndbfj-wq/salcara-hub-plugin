package edgepool

import (
	"errors"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// HealthPolicy controls bounded, nonoverlapping probes of the fixed Hub origin.
// Intervals are measured from completion of the preceding batch.
type HealthPolicy struct {
	Enabled           bool `json:"enabled"`
	IntervalMS        int  `json:"interval_ms"`
	TimeoutMS         int  `json:"timeout_ms"`
	Concurrency       int  `json:"concurrency"`
	FailureThreshold  int  `json:"failure_threshold"`
	RecoverySuccesses int  `json:"recovery_successes"`
	RecoveryHoldMS    int  `json:"recovery_hold_ms"`
	StaleAfterMS      int  `json:"stale_after_ms"`
}

type SelectionPolicy struct {
	SwitchCooldownMS      int      `json:"switch_cooldown_ms"`
	MinImprovementMS      int      `json:"min_improvement_ms"`
	MinImprovementPercent int      `json:"min_improvement_percent"`
	PreferredRegions      []string `json:"preferred_regions"`
	RegionMode            string   `json:"region_mode"`
	AllowUnknown          bool     `json:"allow_unknown"`
	UseObservedRegion     bool     `json:"use_observed_region"`
}

func defaultHealthPolicy() HealthPolicy {
	return HealthPolicy{true, 30000, 6000, 4, 3, 2, 5000, 120000}
}
func defaultSelectionPolicy() SelectionPolicy {
	return SelectionPolicy{120000, 30, 20, []string{}, "off", true, false}
}
func validRegion(region string) bool {
	return utf8.ValidString(region) && len(region) <= 32 && region == strings.TrimSpace(region) && strings.IndexFunc(region, invalidRegionRune) < 0
}
func invalidRegionRune(r rune) bool { return unicode.IsControl(r) || unicode.In(r, unicode.Cf) }
func trimRegionInput(region string) string {
	// Do not erase forbidden controls while trimming normal surrounding space.
	if strings.IndexFunc(region, invalidRegionRune) >= 0 {
		return region
	}
	return strings.TrimSpace(region)
}
func validatePolicies(h HealthPolicy, s SelectionPolicy) error {
	if h.IntervalMS < 5000 || h.IntervalMS > 3600000 || h.TimeoutMS < 100 || h.TimeoutMS > 30000 || h.TimeoutMS > h.IntervalMS ||
		h.Concurrency < 1 || h.Concurrency > 16 || h.FailureThreshold < 1 || h.FailureThreshold > 10 || h.RecoverySuccesses < 1 || h.RecoverySuccesses > 10 ||
		h.RecoveryHoldMS < 0 || h.RecoveryHoldMS > 600000 || h.StaleAfterMS < h.IntervalMS+h.TimeoutMS || h.StaleAfterMS > 86400000 {
		return errors.New("invalid health policy")
	}
	if s.SwitchCooldownMS < 0 || s.SwitchCooldownMS > 3600000 || s.MinImprovementMS < 0 || s.MinImprovementMS > 60000 || s.MinImprovementPercent < 0 || s.MinImprovementPercent > 100 ||
		(s.RegionMode != "off" && s.RegionMode != "prefer" && s.RegionMode != "only") || len(s.PreferredRegions) > maxNodes {
		return errors.New("invalid selection policy")
	}
	seen := map[string]bool{}
	for _, region := range s.PreferredRegions {
		key := strings.ToLower(region)
		if region == "" || !validRegion(region) || seen[key] {
			return errors.New("invalid preferred regions")
		}
		seen[key] = true
	}
	if s.RegionMode != "off" && len(s.PreferredRegions) == 0 {
		return errors.New("preferred regions required")
	}
	return nil
}

type healthPolicyInput struct {
	Enabled           *bool `json:"enabled"`
	IntervalMS        *int  `json:"interval_ms"`
	TimeoutMS         *int  `json:"timeout_ms"`
	Concurrency       *int  `json:"concurrency"`
	FailureThreshold  *int  `json:"failure_threshold"`
	RecoverySuccesses *int  `json:"recovery_successes"`
	RecoveryHoldMS    *int  `json:"recovery_hold_ms"`
	StaleAfterMS      *int  `json:"stale_after_ms"`
}

func (v *healthPolicyInput) apply(h *HealthPolicy) {
	if v == nil {
		return
	}
	if v.Enabled != nil {
		h.Enabled = *v.Enabled
	}
	for _, pair := range []struct {
		from *int
		to   *int
	}{{v.IntervalMS, &h.IntervalMS}, {v.TimeoutMS, &h.TimeoutMS}, {v.Concurrency, &h.Concurrency}, {v.FailureThreshold, &h.FailureThreshold}, {v.RecoverySuccesses, &h.RecoverySuccesses}, {v.RecoveryHoldMS, &h.RecoveryHoldMS}, {v.StaleAfterMS, &h.StaleAfterMS}} {
		if pair.from != nil {
			*pair.to = *pair.from
		}
	}
}

type selectionPolicyInput struct {
	SwitchCooldownMS      *int      `json:"switch_cooldown_ms"`
	MinImprovementMS      *int      `json:"min_improvement_ms"`
	MinImprovementPercent *int      `json:"min_improvement_percent"`
	PreferredRegions      *[]string `json:"preferred_regions"`
	RegionMode            *string   `json:"region_mode"`
	AllowUnknown          *bool     `json:"allow_unknown"`
	UseObservedRegion     *bool     `json:"use_observed_region"`
}

func (v *selectionPolicyInput) apply(s *SelectionPolicy) {
	if v == nil {
		return
	}
	if v.SwitchCooldownMS != nil {
		s.SwitchCooldownMS = *v.SwitchCooldownMS
	}
	if v.MinImprovementMS != nil {
		s.MinImprovementMS = *v.MinImprovementMS
	}
	if v.MinImprovementPercent != nil {
		s.MinImprovementPercent = *v.MinImprovementPercent
	}
	if v.RegionMode != nil {
		s.RegionMode = *v.RegionMode
	}
	if v.AllowUnknown != nil {
		s.AllowUnknown = *v.AllowUnknown
	}
	if v.UseObservedRegion != nil {
		s.UseObservedRegion = *v.UseObservedRegion
	}
	if v.PreferredRegions != nil {
		s.PreferredRegions = make([]string, len(*v.PreferredRegions))
		for i, region := range *v.PreferredRegions {
			s.PreferredRegions[i] = trimRegionInput(region)
		}
	}
}

func (p *Pool) effectiveHealthPolicyLocked() HealthPolicy {
	h := p.saved.HealthPolicy
	// Preserve the original embedding/test overrides without persisting them.
	if p.cfg.ProbeInterval != 0 {
		h.IntervalMS = int(p.cfg.ProbeInterval / time.Millisecond)
	}
	if p.cfg.ProbeTimeout != 0 {
		h.TimeoutMS = int(p.cfg.ProbeTimeout / time.Millisecond)
	}
	return h
}
func (p *Pool) staleLocked(r *route, now time.Time) bool {
	return r.health.CheckedAt > 0 && now.UnixMilli()-r.health.CheckedAt >= int64(p.saved.HealthPolicy.StaleAfterMS)
}
func (p *Pool) unverifiedLocked(r *route, now time.Time) bool {
	return r.health.Status == "unknown" || p.staleLocked(r, now)
}
func (p *Pool) usableLocked(r *route, now time.Time) bool {
	return r != nil && r.config.Enabled && !r.config.Draining && r.health.Status != "unhealthy" &&
		(p.saved.SelectionPolicy.AllowUnknown || !p.unverifiedLocked(r, now))
}
func (p *Pool) regionMatchesLocked(r *route) bool {
	regionValue := p.effectiveRegionLocked(r.config)
	if regionValue == "" {
		return false
	}
	for _, region := range p.saved.SelectionPolicy.PreferredRegions {
		if strings.EqualFold(region, regionValue) {
			return true
		}
	}
	return false
}
func (p *Pool) filterRegionsLocked(routes []*route) []*route {
	policy := p.saved.SelectionPolicy
	if policy.RegionMode == "off" {
		return routes
	}
	preferred := make([]*route, 0, len(routes))
	for _, r := range routes {
		if p.regionMatchesLocked(r) {
			preferred = append(preferred, r)
		}
	}
	if policy.RegionMode == "only" || len(preferred) > 0 {
		return preferred
	}
	return routes
}
func latencyValue(r *route) *float64 {
	if r.health.LatencyEWMAMS != nil {
		return r.health.LatencyEWMAMS
	}
	return r.health.LatencyMS
}
