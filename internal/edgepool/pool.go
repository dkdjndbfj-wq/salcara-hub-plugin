package edgepool

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"sort"
	"sync"
	"time"
)

type Health struct {
	Status               string   `json:"status"`
	LatencyMS            *float64 `json:"latency_ms"`
	CheckedAt            int64    `json:"checked_at"`
	LastProbeAt          int64    `json:"last_probe_at"`
	Error                string   `json:"error"`
	LatencyEWMAMS        *float64 `json:"latency_ewma_ms"`
	SampleAgeMS          *int64   `json:"sample_age_ms"`
	Stale                bool     `json:"stale"`
	ConsecutiveFailures  int      `json:"consecutive_failures"`
	ConsecutiveSuccesses int      `json:"consecutive_successes"`
}

type Node struct {
	ID              string      `json:"id"`
	Name            string      `json:"name"`
	ProxyURL        string      `json:"proxy_url"`
	Enabled         bool        `json:"enabled"`
	HasCredentials  bool        `json:"has_credentials"`
	Health          Health      `json:"health"`
	Weight          int         `json:"weight"`
	MaxRequests     int         `json:"max_requests"`
	MaxStreams      int         `json:"max_streams"`
	Draining        bool        `json:"draining"`
	Recovering      bool        `json:"recovering"`
	Runtime         Runtime     `json:"runtime"`
	Region          string      `json:"region"`
	SourceID        string      `json:"source_id,omitempty"`
	Network         NetworkInfo `json:"network"`
	EffectiveRegion string      `json:"effective_region"`
}

type Event struct {
	At      int64  `json:"at"`
	Type    string `json:"type"`
	NodeID  string `json:"node_id,omitempty"`
	Message string `json:"message"`
}

type State struct {
	Revision        int64             `json:"revision"`
	Origin          string            `json:"origin"`
	Mode            string            `json:"mode"`
	SelectedID      string            `json:"selected_id"`
	ActiveID        string            `json:"active_id"`
	Nodes           []Node            `json:"nodes"`
	Events          []Event           `json:"events"`
	Status          string            `json:"status"`
	ProbeRunning    bool              `json:"probe_running"`
	Durability      string            `json:"durability"`
	Limits          Limits            `json:"limits"`
	Runtime         Runtime           `json:"runtime"`
	HealthPolicy    HealthPolicy      `json:"health_policy"`
	SelectionPolicy SelectionPolicy   `json:"selection_policy"`
	NextProbeAt     int64             `json:"next_probe_at"`
	ProbeRun        *ProbeRun         `json:"probe_run"`
	Release         ReleaseState      `json:"release"`
	GeoAvailable    bool              `json:"geo_available"`
	SourceLimits    []SourceLimit     `json:"source_limits"`
	SourceGroups    []SourceGroupView `json:"source_groups"`
}

type route struct {
	config         nodeConfig
	normal         *http.Transport
	once           *http.Transport
	health         Health
	probeFailures  int
	probeSuccesses int
	healthEpoch    uint64 // advances when newer passive evidence invalidates an in-flight probe
	runtime        *Runtime
	source         *Runtime
	recovering     bool
	recoveredAt    time.Time
}

// Pool owns configuration and transports. Close it before shutting down ingress.
type Pool struct {
	configMu          sync.Mutex // serializes durable changes and Close, never held during response writes
	mu                sync.Mutex
	cfg               Config
	origin            *url.URL
	token             [32]byte
	saved             savedConfig
	routes            map[string]*route
	active            string
	switched          time.Time
	events            []Event
	trusted           []netip.Prefix
	lock              *os.File
	durability        string
	subscriptions     *SubscriptionInspector
	sources           *sourceManager
	geo               *geoManager
	releasePreviews   map[string]*releasePreview
	releaseWake       chan struct{}
	trialDeadline     time.Time
	closeDone         chan struct{}
	observability     *poolObservability
	probing           bool
	closed            bool
	ctx               context.Context
	cancel            context.CancelFunc
	wg                sync.WaitGroup
	runtime           Runtime
	sourceRuntime     map[string]*Runtime
	sourceLimits      map[string]SourceLimit
	notify            chan struct{}
	queue             []*queueWaiter
	balanceCursor     uint64
	probeRun          *ProbeRun
	probeCancel       context.CancelFunc
	probeSlots        chan struct{}
	probeTargets      map[string]probeTarget
	pendingProbes     map[string]bool
	pendingTrigger    string
	probeWake         chan struct{}
	nextProbe         time.Time
	lastProbeFinished time.Time
}

func New(cfg Config) (*Pool, error) {
	origin, err := parseOrigin(cfg.Origin)
	if err != nil {
		return nil, err
	}
	if len(cfg.ControlToken) < 32 || len(cfg.ControlToken) > 4096 {
		return nil, errors.New("control token must contain 32 to 4096 bytes")
	}
	if cfg.DataDir == "" {
		return nil, errors.New("pool data directory is required")
	}
	trusted := make([]netip.Prefix, 0, len(cfg.TrustedProxyCIDRs))
	for _, raw := range cfg.TrustedProxyCIDRs {
		prefix, err := netip.ParsePrefix(raw)
		if err != nil {
			return nil, errors.New("invalid trusted proxy CIDR")
		}
		trusted = append(trusted, prefix)
	}
	if (cfg.ProbeTimeout != 0 && (cfg.ProbeTimeout < 100*time.Millisecond || cfg.ProbeTimeout > time.Minute)) || (cfg.ProbeInterval != 0 && cfg.ProbeInterval < time.Second) {
		return nil, errors.New("invalid probe timing")
	}
	lock, err := acquireLock(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	complete := false
	defer func() {
		if !complete {
			lock.Close()
		}
	}()
	saved, err := readSaved(cfg.DataDir)
	if err != nil {
		return nil, err
	}
	normalizeConfig(&saved)
	saved, err = recoverPendingRelease(saved)
	if err != nil {
		return nil, err
	}
	if err = validateOriginNodes(origin, saved); err != nil {
		return nil, err
	}
	durability, err := writeSaved(cfg.DataDir, saved)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	p := &Pool{cfg: cfg, origin: origin, token: sha256.Sum256([]byte(cfg.ControlToken)), saved: saved, routes: make(map[string]*route), ctx: ctx, cancel: cancel, trusted: trusted}
	p.cfg.ControlToken = ""
	p.notify = make(chan struct{})
	p.closeDone = make(chan struct{})
	p.releaseWake = make(chan struct{}, 1)
	p.releasePreviews = make(map[string]*releasePreview)
	p.probeWake = make(chan struct{}, 1)
	p.probeSlots = make(chan struct{}, 4)
	p.pendingProbes = make(map[string]bool)
	p.lock = lock
	p.durability = durability
	p.subscriptions = NewSubscriptionInspector()
	p.geo = newGeoManager(p)
	p.installLocked(saved)
	p.chooseLocked("started")
	initObservability(p)
	if !cfg.DisablePeriodicProbe && saved.HealthPolicy.Enabled {
		p.nextProbe = time.Now()
	}
	if err := initSourceManager(p); err != nil {
		p.Close()
		return nil, err
	}
	p.wg.Add(1)
	go p.probeLoop()
	p.wg.Add(1)
	go p.releaseLoop()
	p.wg.Add(1)
	go p.observabilityLoop()
	p.wg.Add(1)
	go p.geo.loop()
	complete = true
	return p, nil
}

func transportFor(proxy *url.URL, once bool, publicProxy bool) *http.Transport {
	t := &http.Transport{
		Proxy:               nil,
		DialContext:         (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 130 * time.Second,
		IdleConnTimeout: 60 * time.Second, MaxIdleConns: 64, MaxIdleConnsPerHost: 16,
		DisableKeepAlives: once,
		// HTTP/1.1 permits explicit no-replay transports for mutating requests.
		// SSE is flushed immediately and has no total response-body timeout.
		TLSNextProto: make(map[string]func(string, *tls.Conn) http.RoundTripper),
	}
	if proxy != nil {
		t.Proxy = http.ProxyURL(proxy)
		if publicProxy {
			t.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
				return dialPublicSubscription(ctx, network, address, net.DefaultResolver.LookupNetIP, (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext)
			}
		}
	}
	return t
}

func (p *Pool) installLocked(s savedConfig) {
	p.installSourceLimitsLocked(s)
	next := make(map[string]*route, len(s.Nodes))
	for _, n := range s.Nodes {
		old := p.routes[n.ID]
		if old != nil && old.config.ProxyURL == n.ProxyURL && old.config.Imported == n.Imported {
			if old.config.Enabled != n.Enabled {
				old.healthEpoch++
				if n.Enabled {
					old.health = Health{Status: "unknown"}
					old.probeFailures = 0
					old.probeSuccesses = 0
					old.recovering = true
					old.recoveredAt = time.Time{}
				}
			}
			old.config = n
			old.source = p.sourceRuntime[n.SourceID]
			next[n.ID] = old
			continue
		}
		u, _ := parseProxy(n.ProxyURL)
		stats := &Runtime{}
		if old != nil {
			stats = old.runtime
		}
		next[n.ID] = &route{config: n, normal: transportFor(u, false, n.Imported), once: transportFor(u, true, n.Imported), health: Health{Status: "unknown"}, runtime: stats, source: p.sourceRuntime[n.SourceID]}
	}
	for id, old := range p.routes {
		if next[id] != old {
			old.normal.CloseIdleConnections()
			old.once.CloseIdleConnections()
		}
	}
	p.routes = next
	p.saved = s
	p.pruneSourceRuntimesLocked()
}

func (p *Pool) eventLocked(typ, id, message string) {
	p.events = append(p.events, Event{At: time.Now().UnixMilli(), Type: typ, NodeID: id, Message: message})
	if len(p.events) > 100 {
		p.events = append([]Event(nil), p.events[len(p.events)-100:]...)
	}
}

func (p *Pool) chooseLocked(reason string) {
	now := time.Now()
	previous := p.active
	chosen := ""
	preferred := p.routes[p.saved.SelectedID]
	if p.saved.Mode == "balance" {
		p.active = ""
		return
	}
	if p.saved.Mode == "manual" {
		if preferred != nil && preferred.config.Enabled && !preferred.config.Draining {
			chosen = preferred.config.ID
		}
	} else {
		candidates := make([]*route, 0, len(p.routes))
		if p.usableLocked(preferred, now) {
			candidates = append(candidates, preferred)
		}
		for _, n := range p.saved.Nodes {
			if n.ID != p.saved.SelectedID && p.usableLocked(p.routes[n.ID], now) {
				candidates = append(candidates, p.routes[n.ID])
			}
		}
		candidates = p.filterRegionsLocked(candidates)
		if p.saved.Mode == "latency" {
			sort.SliceStable(candidates, func(i, j int) bool {
				unverifiedA, unverifiedB := p.unverifiedLocked(candidates[i], now), p.unverifiedLocked(candidates[j], now)
				if unverifiedA != unverifiedB {
					return !unverifiedA
				}
				a, b := latencyValue(candidates[i]), latencyValue(candidates[j])
				if a == nil {
					return false
				}
				if b == nil {
					return true
				}
				return *a < *b
			})
		}
		if len(candidates) > 0 {
			chosen = candidates[0].config.ID
			if p.saved.Mode == "latency" && chosen != previous {
				current := p.routes[previous]
				best := candidates[0]
				currentEligible := false
				for _, candidate := range candidates {
					if candidate == current {
						currentEligible = true
						break
					}
				}
				if currentEligible && !p.unverifiedLocked(current, now) && latencyValue(current) != nil && latencyValue(best) != nil {
					gain := *latencyValue(current) - *latencyValue(best)
					policy := p.saved.SelectionPolicy
					if gain < float64(policy.MinImprovementMS) || gain < *latencyValue(current)*float64(policy.MinImprovementPercent)/100 || now.Sub(p.switched) < time.Duration(policy.SwitchCooldownMS)*time.Millisecond {
						chosen = previous
					}
				}
			}
		}
	}
	p.active = chosen
	if chosen != previous {
		p.switched = time.Now()
		p.eventLocked("route_changed", chosen, reason)
	}
}

// State contains no proxy authentication data, including usernames.
func (p *Pool) State() State { p.mu.Lock(); defer p.mu.Unlock(); return p.stateLocked() }

func (p *Pool) stateLocked() State {
	p.chooseLocked("health_expired")
	s := State{Revision: p.saved.Revision, Origin: p.origin.String(), Mode: p.saved.Mode, SelectedID: p.saved.SelectedID, ActiveID: p.active, Nodes: make([]Node, 0, len(p.saved.Nodes)), Events: append([]Event{}, p.events...), ProbeRunning: p.probing, Status: "unavailable"}
	s.Durability = p.durability
	s.Limits = p.saved.Limits
	s.Runtime = p.runtime
	s.SourceLimits = append([]SourceLimit{}, p.saved.SourceLimits...)
	s.SourceGroups = p.sourceGroupsLocked()
	s.HealthPolicy = p.saved.HealthPolicy
	s.Release = p.releaseStateLocked()
	s.SelectionPolicy = p.saved.SelectionPolicy
	s.SelectionPolicy.PreferredRegions = append([]string{}, p.saved.SelectionPolicy.PreferredRegions...)
	if !p.nextProbe.IsZero() {
		s.NextProbeAt = p.nextProbe.UnixMilli()
	}
	if p.probeRun != nil {
		run := *p.probeRun
		s.ProbeRun = &run
	}
	if p.active != "" {
		s.Status = "ready"
		if r := p.routes[p.active]; r != nil {
			switch r.health.Status {
			case "unknown":
				s.Status = "unknown"
			case "degraded", "unhealthy":
				s.Status = "degraded"
			}
		}
	}
	if p.saved.Mode == "balance" {
		candidates := make([]*route, 0, len(p.routes))
		for _, r := range p.routes {
			if p.usableLocked(r, time.Now()) {
				candidates = append(candidates, r)
			}
		}
		for _, r := range p.filterRegionsLocked(candidates) {
			if r.health.Status == "healthy" && !p.unverifiedLocked(r, time.Now()) {
				s.Status = "ready"
				break
			}
			if r.health.Status == "degraded" {
				s.Status = "degraded"
			} else if s.Status == "unavailable" {
				s.Status = "unknown"
			}
		}
	}
	if p.closed {
		s.Status = "stopped"
	}
	for _, n := range p.saved.Nodes {
		u, _ := parseProxy(n.ProxyURL)
		public, credentials := "", false
		if u != nil {
			credentials = u.User != nil
			u.User = nil
			public = u.String()
		}
		h := p.routes[n.ID].health
		if h.LatencyMS != nil {
			value := *h.LatencyMS
			h.LatencyMS = &value
		}
		if h.LatencyEWMAMS != nil {
			value := *h.LatencyEWMAMS
			h.LatencyEWMAMS = &value
		}
		r := p.routes[n.ID]
		h.Stale = p.staleLocked(r, time.Now())
		if h.CheckedAt > 0 {
			age := max(int64(0), time.Now().UnixMilli()-h.CheckedAt)
			h.SampleAgeMS = &age
		}
		h.ConsecutiveFailures, h.ConsecutiveSuccesses = r.probeFailures, r.probeSuccesses
		if h.Stale && s.ActiveID == n.ID {
			s.Status = "unknown"
		}
		s.Nodes = append(s.Nodes, Node{ID: n.ID, Name: n.Name, ProxyURL: public, Enabled: n.Enabled, HasCredentials: credentials, Health: h, Weight: n.Weight, MaxRequests: n.MaxRequests, MaxStreams: n.MaxStreams, Draining: n.Draining, Recovering: r.recovering, Runtime: *r.runtime, Region: n.Region, SourceID: n.SourceID, Network: p.geo.info(n), EffectiveRegion: p.effectiveRegionLocked(n)})
	}
	s.GeoAvailable = p.geo != nil
	return s
}

func (p *Pool) failed(r *route) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || p.routes[r.config.ID] != r {
		return
	}
	r.health.Status = "unhealthy"
	r.health.Error = "connection_failed"
	r.probeFailures++
	r.probeSuccesses = 0
	r.healthEpoch++
	r.recovering = true
	r.recoveredAt = time.Time{}
	p.eventLocked("request_failed", r.config.ID, "upstream_connection_failed")
	p.chooseLocked("connection_failed")
	p.signalLocked()
}

func (p *Pool) Close() {
	p.configMu.Lock()
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		p.configMu.Unlock()
		<-p.closeDone
		return
	}
	p.closed = true
	p.nextProbe = time.Time{}
	p.cancel()
	p.signalLocked()
	for _, r := range p.routes {
		r.normal.CloseIdleConnections()
		r.once.CloseIdleConnections()
	}
	p.mu.Unlock()
	p.configMu.Unlock()
	if p.sources != nil {
		p.sources.Close()
	}
	if p.geo != nil {
		p.geo.Close()
	}
	p.wg.Wait()
	p.subscriptions.Close()
	_ = p.lock.Close()
	close(p.closeDone)
}
