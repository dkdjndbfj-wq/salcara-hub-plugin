package edgepool

import (
	"encoding/json"
	"errors"
	"math"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"time"
)

const observationInterval = time.Minute
const observationRetention = 24 * time.Hour
const observationLimit = 1440
const observationFile = "edge-observability.json"
const observationFileLimit = 24 << 20

// Runtime counters in a trend sample are deltas within the sampled bucket;
// active and queued fields remain instantaneous gauges. Live State.Runtime is
// still cumulative since process startup and is deliberately not restored.
type NodeTrend struct {
	ID            string   `json:"id"`
	Status        string   `json:"status"`
	LatencyEWMAMS *float64 `json:"latency_ewma_ms"`
	Stale         bool     `json:"stale"`
	Runtime       Runtime  `json:"runtime"`
}
type TrendSample struct {
	At       int64       `json:"at"`
	Revision int64       `json:"revision"`
	ActiveID string      `json:"active_id"`
	Mode     string      `json:"mode"`
	Runtime  Runtime     `json:"runtime"`
	Nodes    []NodeTrend `json:"nodes"`
}
type RouteCandidate struct {
	ID                    string   `json:"id"`
	Eligible              bool     `json:"eligible"`
	Reason                string   `json:"reason"`
	Explanation           string   `json:"explanation"`
	Region                string   `json:"region"`
	LatencyEWMAMS         *float64 `json:"latency_ewma_ms"`
	CanRequest            bool     `json:"can_request"`
	CanStream             bool     `json:"can_stream"`
	RequestUtilization    float64  `json:"request_utilization"`
	StreamUtilization     float64  `json:"stream_utilization"`
	SourceID              string   `json:"source_id"`
	SourceRequestCapacity bool     `json:"source_request_capacity"`
	SourceStreamCapacity  bool     `json:"source_stream_capacity"`
}
type RouteDecision struct {
	Mode            string           `json:"mode"`
	ActiveID        string           `json:"active_id"`
	Reason          string           `json:"reason"`
	Explanation     string           `json:"explanation"`
	RequestCapacity bool             `json:"request_capacity"`
	StreamCapacity  bool             `json:"stream_capacity"`
	Candidates      []RouteCandidate `json:"candidates"`
}
type ObservabilityState struct {
	SampleIntervalMS int64         `json:"sample_interval_ms"`
	RetentionMS      int64         `json:"retention_ms"`
	CounterSemantics string        `json:"counter_semantics"`
	Samples          []TrendSample `json:"samples"`
	Decision         RouteDecision `json:"decision"`
	LastError        string        `json:"last_error"`
}
type observationDisk struct {
	Version int           `json:"version"`
	Samples []TrendSample `json:"samples"`
}
type runtimeBaseline struct {
	identity *Runtime
	value    Runtime
}
type poolObservability struct {
	samples   []TrendSample
	lastError string
	total     Runtime
	nodes     map[string]runtimeBaseline
}

func initObservability(p *Pool) {
	o := &poolObservability{samples: []TrendSample{}, nodes: map[string]runtimeBaseline{}}
	p.observability = o
	raw, err := readSourcePrivate(filepath.Join(p.cfg.DataDir, observationFile), observationFileLimit)
	if err == nil {
		var disk observationDisk
		if !decodeJSON(raw, &disk) || validateObservations(disk) != nil {
			o.lastError = "telemetry_load_failed"
		} else {
			o.samples = disk.Samples
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		o.lastError = "telemetry_load_failed"
	}
	p.recordObservationLocked(time.Now())
}
func validateObservations(disk observationDisk) error {
	if disk.Version != 1 || len(disk.Samples) > observationLimit {
		return errors.New("invalid telemetry")
	}
	var last int64
	for _, s := range disk.Samples {
		if s.At < 1 || s.At <= last || s.At%observationInterval.Milliseconds() != 0 || s.Revision < 1 || len(s.Nodes) > maxNodes || !validObservationRuntime(s.Runtime) {
			return errors.New("invalid telemetry")
		}
		if s.Mode != "manual" && s.Mode != "failover" && s.Mode != "latency" && s.Mode != "balance" {
			return errors.New("invalid telemetry")
		}
		if s.ActiveID != "" && !nodeID.MatchString(s.ActiveID) {
			return errors.New("invalid telemetry")
		}
		last = s.At
		seen := map[string]bool{}
		for _, n := range s.Nodes {
			if !nodeID.MatchString(n.ID) || seen[n.ID] || !validObservationRuntime(n.Runtime) {
				return errors.New("invalid telemetry")
			}
			seen[n.ID] = true
			if n.Status != "unknown" && n.Status != "healthy" && n.Status != "degraded" && n.Status != "unhealthy" {
				return errors.New("invalid telemetry")
			}
			if n.LatencyEWMAMS != nil && (*n.LatencyEWMAMS < 0 || math.IsNaN(*n.LatencyEWMAMS) || math.IsInf(*n.LatencyEWMAMS, 0)) {
				return errors.New("invalid telemetry")
			}
		}
	}
	return nil
}
func validObservationRuntime(r Runtime) bool {
	return r.ActiveRequests >= 0 && r.ActiveStreams >= 0 && r.QueuedRequests >= 0 && r.QueuedStreams >= 0 && r.ActiveRequests <= 4096 && r.ActiveStreams <= 4096 && r.QueuedRequests <= 4096 && r.QueuedStreams <= 4096
}
func counterDelta(current, previous uint64) uint64 {
	if current < previous {
		return current
	}
	return current - previous
}
func runtimeDelta(current, previous Runtime) Runtime {
	next := current
	next.CompletedRequests = counterDelta(current.CompletedRequests, previous.CompletedRequests)
	next.FailedRequests = counterDelta(current.FailedRequests, previous.FailedRequests)
	next.RejectedRequests = counterDelta(current.RejectedRequests, previous.RejectedRequests)
	next.CancelledRequests = counterDelta(current.CancelledRequests, previous.CancelledRequests)
	return next
}
func addCounters(latest, previous Runtime) Runtime {
	latest.CompletedRequests += previous.CompletedRequests
	latest.FailedRequests += previous.FailedRequests
	latest.RejectedRequests += previous.RejectedRequests
	latest.CancelledRequests += previous.CancelledRequests
	return latest
}
func copyLatency(value *float64) *float64 {
	if value == nil {
		return nil
	}
	v := *value
	return &v
}

func (p *Pool) recordObservationLocked(now time.Time) {
	o := p.observability
	if o == nil {
		return
	}
	at := now.Truncate(observationInterval).UnixMilli()
	if len(o.samples) > 0 && at < o.samples[len(o.samples)-1].At {
		o.lastError = "telemetry_clock_changed"
		return
	}
	p.chooseLocked("health_expired")
	s := TrendSample{At: at, Revision: p.saved.Revision, ActiveID: p.active, Mode: p.saved.Mode, Runtime: runtimeDelta(p.runtime, o.total), Nodes: []NodeTrend{}}
	o.total = p.runtime
	baselines := make(map[string]runtimeBaseline, len(p.routes))
	for _, n := range p.saved.Nodes {
		r := p.routes[n.ID]
		previous := o.nodes[n.ID]
		if previous.identity != r.runtime {
			previous.value = Runtime{}
		}
		s.Nodes = append(s.Nodes, NodeTrend{n.ID, r.health.Status, copyLatency(latencyValue(r)), p.staleLocked(r, now), runtimeDelta(*r.runtime, previous.value)})
		baselines[n.ID] = runtimeBaseline{r.runtime, *r.runtime}
	}
	o.nodes = baselines
	if len(o.samples) > 0 && o.samples[len(o.samples)-1].At == at {
		old := o.samples[len(o.samples)-1]
		s.Runtime = addCounters(s.Runtime, old.Runtime)
		before := map[string]NodeTrend{}
		for _, n := range old.Nodes {
			before[n.ID] = n
		}
		for i := range s.Nodes {
			n := &s.Nodes[i]
			if prior, ok := before[n.ID]; ok {
				n.Runtime = addCounters(n.Runtime, prior.Runtime)
			}
		}
		o.samples[len(o.samples)-1] = s
	} else {
		o.samples = append(o.samples, s)
	}
	cutoff := at - observationRetention.Milliseconds() + observationInterval.Milliseconds()
	first := 0
	for first < len(o.samples) && o.samples[first].At < cutoff {
		first++
	}
	if len(o.samples)-first > observationLimit {
		first = len(o.samples) - observationLimit
	}
	if first > 0 {
		o.samples = append([]TrendSample{}, o.samples[first:]...)
	}
}
func (p *Pool) persistObservation(now time.Time) {
	p.mu.Lock()
	p.recordObservationLocked(now)
	samples := append([]TrendSample{}, p.observability.samples...)
	p.mu.Unlock()
	// Published samples are immutable; new samples replace rather than mutate
	// their nested nodes. Encoding and syncing never hold the routing mutex.
	raw, err := json.Marshal(observationDisk{1, samples})
	if err == nil && len(raw) <= observationFileLimit {
		err = writeSourcePrivate(filepath.Join(p.cfg.DataDir, observationFile), raw)
	} else {
		err = errors.New("telemetry limit")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if err != nil {
		p.observability.lastError = "telemetry_persistence_failed"
	} else if p.observability.lastError != "telemetry_clock_changed" {
		p.observability.lastError = ""
	}
}
func (p *Pool) observabilityLoop() {
	defer p.wg.Done()
	tick := time.NewTicker(observationInterval)
	defer tick.Stop()
	for {
		select {
		case now := <-tick.C:
			p.persistObservation(now)
		case <-p.ctx.Done():
			p.persistObservation(time.Now())
			return
		}
	}
}

func decisionExplanation(code string) string {
	switch code {
	case "manual_selection":
		return "按管理员指定的节点转发；失败不会自动改走其他节点。"
	case "manual_unavailable":
		return "指定节点不存在、已停用或正在排空，当前没有可用入口线路。"
	case "weighted_capacity":
		return "按节点权重和当前占用分配每个新请求；普通请求和长连接分别计算容量。"
	case "failover_precedence":
		return "在健康和地区筛选后优先使用指定节点，失败时按配置顺序切换。"
	case "lowest_latency":
		return "从符合健康与地区条件的节点中，优先选择已验证且平滑延迟较低的节点。"
	case "switch_cooldown":
		return "更快节点的改善已达到门槛，但仍在切换冷却期内。"
	case "improvement_below_threshold":
		return "其他节点的延迟改善尚未同时达到毫秒和百分比门槛。"
	case "no_eligible_route":
		return "没有节点同时满足启停、健康、排空和地区限制。"
	case "disabled":
		return "节点已停用。"
	case "draining":
		return "节点正在排空，只保留已有连接。"
	case "manual_unselected":
		return "手动模式下只使用指定节点。"
	case "unhealthy":
		return "最近健康证据表明节点不可用。"
	case "stale_disallowed":
		return "健康样本已过期，当前策略不接受未验证节点。"
	case "unknown_disallowed":
		return "节点尚未验证，当前策略不接受未验证节点。"
	case "region_excluded":
		return "节点不在当前允许或优先使用的地区中。"
	case "capacity_full":
		return "节点普通请求和长连接容量均已用尽。"
	case "source_capacity_full":
		return "该订阅来源的共享普通请求和长连接额度均已用尽；增加同来源节点不会扩大此额度。"
	case "source_request_capacity_full":
		return "该订阅来源的共享普通请求额度已用尽；长连接仍按独立额度接入。"
	case "source_stream_capacity_full":
		return "该订阅来源的共享长连接额度已用尽；普通请求仍按独立额度接入。"
	case "limited_trial":
		return "节点尚未验证或正在恢复，只允许一条试探连接。"
	case "selected":
		return "当前固定入口正在使用此节点。"
	case "eligible":
		return "节点满足当前选路条件。"
	}
	return "当前按已保存的策略选路。"
}

func (p *Pool) decisionLocked(now time.Time) RouteDecision {
	d := RouteDecision{Mode: p.saved.Mode, ActiveID: p.active, RequestCapacity: p.runtime.ActiveRequests < p.saved.Limits.MaxRequests, StreamCapacity: p.runtime.ActiveStreams < p.saved.Limits.MaxStreams, Candidates: []RouteCandidate{}}
	base := []*route{}
	// Preserve configured preference order, including ties in latency mode.
	if r := p.routes[p.saved.SelectedID]; p.usableLocked(r, now) {
		base = append(base, r)
	}
	for _, n := range p.saved.Nodes {
		if n.ID != p.saved.SelectedID && p.usableLocked(p.routes[n.ID], now) {
			base = append(base, p.routes[n.ID])
		}
	}
	regional := p.filterRegionsLocked(base)
	allowed := map[string]bool{}
	preferredRequest, preferredStream := false, false
	for _, r := range regional {
		allowed[r.config.ID] = true
		if p.regionMatchesLocked(r) {
			preferredRequest = preferredRequest || p.routeCapacityLocked(r, false)
			preferredStream = preferredStream || p.routeCapacityLocked(r, true)
		}
	}
	anyEligible := false
	for _, n := range p.saved.Nodes {
		r := p.routes[n.ID]
		c := RouteCandidate{ID: n.ID, Eligible: true, Reason: "eligible", Region: p.effectiveRegionLocked(n), LatencyEWMAMS: copyLatency(latencyValue(r)), CanRequest: p.routeCapacityLocked(r, false), CanStream: p.routeCapacityLocked(r, true), RequestUtilization: float64(r.runtime.ActiveRequests) / float64(n.MaxRequests), StreamUtilization: float64(r.runtime.ActiveStreams) / float64(n.MaxStreams)}
		c.SourceID = n.SourceID
		c.SourceRequestCapacity = p.sourceCapacityLocked(r, false)
		c.SourceStreamCapacity = p.sourceCapacityLocked(r, true)
		switch {
		case !n.Enabled:
			c.Eligible = false
			c.Reason = "disabled"
		case n.Draining:
			c.Eligible = false
			c.Reason = "draining"
		case p.saved.Mode == "manual" && n.ID != p.saved.SelectedID:
			c.Eligible = false
			c.Reason = "manual_unselected"
		case p.saved.Mode != "manual" && r.health.Status == "unhealthy":
			c.Eligible = false
			c.Reason = "unhealthy"
		case p.saved.Mode != "manual" && !p.saved.SelectionPolicy.AllowUnknown && p.staleLocked(r, now):
			c.Eligible = false
			c.Reason = "stale_disallowed"
		case p.saved.Mode != "manual" && !p.saved.SelectionPolicy.AllowUnknown && p.unverifiedLocked(r, now):
			c.Eligible = false
			c.Reason = "unknown_disallowed"
		case p.saved.Mode != "manual" && !allowed[n.ID]:
			if p.saved.Mode == "balance" && p.saved.SelectionPolicy.RegionMode == "prefer" {
				c.CanRequest = c.CanRequest && !preferredRequest
				c.CanStream = c.CanStream && !preferredStream
				if preferredRequest && preferredStream {
					c.Eligible = false
					c.Reason = "region_excluded"
				}
			} else {
				c.Eligible = false
				c.Reason = "region_excluded"
			}
		}
		if !c.Eligible {
			c.CanRequest = false
			c.CanStream = false
		} else {
			anyEligible = true
			if !c.SourceRequestCapacity && !c.SourceStreamCapacity {
				c.Reason = "source_capacity_full"
			} else if !c.SourceRequestCapacity {
				c.Reason = "source_request_capacity_full"
			} else if !c.SourceStreamCapacity {
				c.Reason = "source_stream_capacity_full"
			} else if !c.CanRequest && !c.CanStream {
				c.Reason = "capacity_full"
			} else if p.unverifiedLocked(r, now) || r.recovering || r.health.Status == "unhealthy" {
				c.Reason = "limited_trial"
			} else if p.active == n.ID {
				c.Reason = "selected"
			}
		}
		c.Explanation = decisionExplanation(c.Reason)
		d.Candidates = append(d.Candidates, c)
	}
	d.Reason = "no_eligible_route"
	switch p.saved.Mode {
	case "manual":
		d.Reason = "manual_selection"
		if p.active == "" {
			d.Reason = "manual_unavailable"
		}
	case "balance":
		if anyEligible {
			d.Reason = "weighted_capacity"
		}
	case "failover":
		if p.active != "" {
			d.Reason = "failover_precedence"
		}
	case "latency":
		if p.active != "" {
			d.Reason = "lowest_latency"
			sort.SliceStable(regional, func(i, j int) bool {
				a, b := regional[i], regional[j]
				au, bu := p.unverifiedLocked(a, now), p.unverifiedLocked(b, now)
				if au != bu {
					return !au
				}
				av, bv := latencyValue(a), latencyValue(b)
				return av != nil && (bv == nil || *av < *bv)
			})
			if current := p.routes[p.active]; current != nil && len(regional) > 0 && regional[0] != current && latencyValue(current) != nil && latencyValue(regional[0]) != nil {
				gain := *latencyValue(current) - *latencyValue(regional[0])
				policy := p.saved.SelectionPolicy
				if gain < float64(policy.MinImprovementMS) || gain < *latencyValue(current)*float64(policy.MinImprovementPercent)/100 {
					d.Reason = "improvement_below_threshold"
				} else if now.Sub(p.switched) < time.Duration(policy.SwitchCooldownMS)*time.Millisecond {
					d.Reason = "switch_cooldown"
				}
			}
		}
	}
	d.Explanation = decisionExplanation(d.Reason)
	return d
}

// Observability exposes pool totals by default. A single node can be requested
// without returning a multi-megabyte matrix or any provider URLs/credentials.
func (p *Pool) Observability(node string, window time.Duration) ObservabilityState {
	p.mu.Lock()
	defer p.mu.Unlock()
	now := time.Now()
	p.chooseLocked("health_expired")
	out := ObservabilityState{SampleIntervalMS: observationInterval.Milliseconds(), RetentionMS: observationRetention.Milliseconds(), CounterSemantics: "bucket_deltas_and_instantaneous_gauges", Samples: []TrendSample{}, Decision: p.decisionLocked(now)}
	if p.observability == nil {
		return out
	}
	out.LastError = p.observability.lastError
	if window <= 0 || window > observationRetention {
		window = observationRetention
	}
	cutoff := now.Truncate(observationInterval).UnixMilli() - window.Milliseconds() + observationInterval.Milliseconds()
	for _, s := range p.observability.samples {
		if s.At < cutoff {
			continue
		}
		copy := s
		copy.Nodes = []NodeTrend{}
		if node != "" {
			for _, n := range s.Nodes {
				if n.ID == node {
					n.LatencyEWMAMS = copyLatency(n.LatencyEWMAMS)
					copy.Nodes = append(copy.Nodes, n)
					break
				}
			}
		}
		out.Samples = append(out.Samples, copy)
	}
	return out
}
func (p *Pool) serveObservability(w http.ResponseWriter, r *http.Request) bool {
	if r.URL.Path != "/v1/observability" && r.URL.Path != "/v1/observability/query" {
		return false
	}
	node := ""
	window := observationRetention
	if r.URL.Path == "/v1/observability" {
		if r.Method != http.MethodGet {
			w.Header().Set("Allow", "GET")
			failure(w, 405, "method_not_allowed")
			return true
		}
	} else {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			failure(w, 405, "method_not_allowed")
			return true
		}
		var body struct {
			NodeID   string `json:"node_id"`
			WindowMS int64  `json:"window_ms"`
		}
		if !decode(w, r, &body) {
			return true
		}
		if body.NodeID != "" && !nodeID.MatchString(body.NodeID) || body.WindowMS != 0 && (body.WindowMS < observationInterval.Milliseconds() || body.WindowMS > observationRetention.Milliseconds()) {
			failure(w, 400, "invalid_observability_query")
			return true
		}
		node = body.NodeID
		if body.WindowMS > 0 {
			window = time.Duration(body.WindowMS) * time.Millisecond
		}
	}
	answer(w, 200, p.Observability(node, window))
	return true
}
