package edgepool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"
)

type ProbeRun struct {
	ID         string `json:"id"`
	Scope      string `json:"scope"`
	Trigger    string `json:"trigger"`
	StartedAt  int64  `json:"started_at"`
	FinishedAt int64  `json:"finished_at"`
	Total      int    `json:"total"`
	Completed  int    `json:"completed"`
	Succeeded  int    `json:"succeeded"`
	Failed     int    `json:"failed"`
	Skipped    int    `json:"skipped"`
	Running    bool   `json:"running"`
	Pending    bool   `json:"pending"`
	Cancelled  bool   `json:"cancelled"`
}

type probeTarget struct {
	route           *route
	epoch           uint64
	includeDisabled bool
}

// Probe is retained for embedders. Repeated requests share the active pass;
// newly added or changed targets are coalesced into its pending follow-up.
func (p *Pool) Probe() bool {
	started, _ := p.RequestProbe("enabled", nil)
	return started
}

// CancelProbe is bound to one task ID. It cancels that task's in-flight checks
// without converting cancellation into negative health evidence or disabling
// the periodic scheduler or a separately queued follow-up task.
func (p *Pool) CancelProbe(id string) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return errors.New("stopped")
	}
	if id == "" {
		return errors.New("run_id_required")
	}
	if p.probeRun == nil || p.probeRun.ID != id {
		return errors.New("probe_run_not_found")
	}
	if !p.probeRun.Running {
		return nil
	}
	p.probeRun.Cancelled = true
	if p.probeCancel != nil {
		p.probeCancel()
	}
	return nil
}

// All fixed-origin checks share this process-wide budget, including source
// publication and release preflight. Waiting for a slot is not a node failure.
func (p *Pool) acquireProbeSlot(ctx context.Context) (func(), bool) {
	select {
	case p.probeSlots <- struct{}{}:
		if ctx.Err() != nil {
			<-p.probeSlots
			return nil, false
		}
		return func() { <-p.probeSlots }, true
	case <-ctx.Done():
		return nil, false
	}
}

func (p *Pool) RequestProbe(scope string, nodeIDs []string) (bool, error) {
	if scope == "" {
		scope = "enabled"
	}
	if scope != "enabled" && scope != "all" {
		return false, errors.New("invalid_probe_scope")
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return false, errors.New("stopped")
	}
	if nodeIDs != nil && (len(nodeIDs) == 0 || len(nodeIDs) > maxNodes) {
		return false, errors.New("invalid_probe_nodes")
	}
	wanted := make(map[string]bool)
	for _, id := range nodeIDs {
		if wanted[id] || p.routes[id] == nil {
			return false, errors.New("invalid_probe_nodes")
		}
		wanted[id] = true
	}
	targets := make(map[string]bool)
	for _, n := range p.saved.Nodes {
		if nodeIDs != nil && !wanted[n.ID] {
			continue
		}
		if scope == "all" || n.Enabled {
			targets[n.ID] = scope == "all"
		}
	}
	if nodeIDs != nil {
		scope = "selected"
	}
	return p.requestProbeLocked(targets, scope, "manual"), nil
}

func (p *Pool) requestProbeLocked(targets map[string]bool, scope, trigger string) bool {
	if p.closed {
		return false
	}
	if p.probing {
		for id, includeDisabled := range targets {
			r := p.routes[id]
			active, exists := p.probeTargets[id]
			if r == nil || exists && active.route == r && active.epoch == r.healthEpoch && (!includeDisabled || active.includeDisabled || r.config.Enabled) {
				continue
			}
			p.pendingProbes[id] = p.pendingProbes[id] || includeDisabled
		}
		if len(p.pendingProbes) > 0 {
			p.pendingTrigger = trigger
			p.probeRun.Pending = true
		}
		return false
	}
	p.startProbeLocked(targets, scope, trigger)
	return true
}

func (p *Pool) startProbeLocked(requested map[string]bool, scope, trigger string) {
	if p.closed {
		return
	}
	targets := make([]probeTarget, 0, len(requested))
	p.probeTargets = make(map[string]probeTarget)
	for _, n := range p.saved.Nodes {
		includeDisabled, ok := requested[n.ID]
		if !ok || !includeDisabled && !n.Enabled {
			continue
		}
		r := p.routes[n.ID]
		target := probeTarget{r, r.healthEpoch, includeDisabled}
		targets = append(targets, target)
		p.probeTargets[n.ID] = target
	}
	var id [8]byte
	if _, err := rand.Read(id[:]); err != nil {
		return
	}
	run := &ProbeRun{ID: hex.EncodeToString(id[:]), Scope: scope, Trigger: trigger, StartedAt: time.Now().UnixMilli(), Total: len(targets), Running: true}
	p.probeRun = run
	p.probing = true
	p.nextProbe = time.Time{}
	policy := p.effectiveHealthPolicyLocked()
	runContext, cancel := context.WithCancel(p.ctx)
	p.probeCancel = cancel
	p.wg.Add(1)
	go p.executeProbe(runContext, cancel, run, targets, policy)
	p.wakeProbeLocked()
}

func (p *Pool) executeProbe(ctx context.Context, cancel context.CancelFunc, run *ProbeRun, targets []probeTarget, policy HealthPolicy) {
	defer p.wg.Done()
	defer cancel()
	var workers sync.WaitGroup
	sem := make(chan struct{}, policy.Concurrency)
	for _, target := range targets {
		workers.Add(1)
		go func(target probeTarget) {
			defer workers.Done()
			outcome := "skipped"
			select {
			case sem <- struct{}{}:
				outcome = p.probeTarget(ctx, target, time.Duration(policy.TimeoutMS)*time.Millisecond)
				<-sem
			case <-ctx.Done():
			}
			p.mu.Lock()
			run.Completed++
			switch outcome {
			case "success":
				run.Succeeded++
			case "failed":
				run.Failed++
			default:
				run.Skipped++
			}
			p.mu.Unlock()
		}(target)
	}
	workers.Wait()
	p.mu.Lock()
	defer p.mu.Unlock()
	p.probing = false
	p.probeCancel = nil
	run.Running = false
	run.FinishedAt = time.Now().UnixMilli()
	p.lastProbeFinished = time.Now()
	if p.closed {
		run.Pending = false
		p.pendingProbes = map[string]bool{}
		return
	}
	p.chooseLocked("probe_completed")
	if len(p.pendingProbes) > 0 {
		pending, trigger := p.pendingProbes, p.pendingTrigger
		p.pendingProbes = make(map[string]bool)
		p.pendingTrigger = ""
		run.Pending = false
		p.startProbeLocked(pending, "selected", trigger)
	} else {
		p.rescheduleProbeLocked()
	}
	p.signalLocked()
}

func (p *Pool) probeTarget(runContext context.Context, target probeTarget, timeout time.Duration) string {
	release, acquired := p.acquireProbeSlot(runContext)
	if !acquired {
		return "skipped"
	}
	defer release()
	r := target.route
	p.mu.Lock()
	valid := !p.closed && p.routes[r.config.ID] == r && r.healthEpoch == target.epoch && (r.config.Enabled || target.includeDisabled)
	p.mu.Unlock()
	if !valid {
		return "skipped"
	}
	ctx, cancel := context.WithTimeout(runContext, timeout)
	defer cancel()
	u := *p.origin
	u.Path = "/salcara-hub/statusz"
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	req.Header.Set("Accept", "application/json")
	client := &http.Client{Transport: r.once, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	start := time.Now()
	resp, err := client.Do(req)
	health := Health{Status: "unhealthy", Error: "probe_failed"}
	if err == nil {
		var status struct {
			Status string `json:"status"`
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 4097))
		_ = resp.Body.Close()
		if resp.StatusCode == http.StatusOK && readErr == nil && len(body) <= 4096 && json.Unmarshal(body, &status) == nil && status.Status == "available" {
			ms := float64(time.Since(start).Microseconds()) / 1000
			health.Status, health.Error, health.LatencyMS = "healthy", "", &ms
		}
	}
	// Freshness starts only after the complete, bounded status response arrived.
	finished := time.Now()
	health.LastProbeAt = finished.UnixMilli()
	if health.Status == "healthy" {
		health.CheckedAt = health.LastProbeAt
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || runContext.Err() != nil || p.routes[r.config.ID] != r || r.healthEpoch != target.epoch || !r.config.Enabled && !target.includeDisabled {
		return "skipped"
	}
	policy := p.saved.HealthPolicy
	previous := r.health.Status
	outcome := "failed"
	if health.Status == "healthy" {
		outcome = "success"
		r.probeFailures = 0
		r.probeSuccesses++
		average := *health.LatencyMS
		if r.health.LatencyEWMAMS != nil && !p.staleLocked(r, finished) {
			average = 0.3*average + 0.7**r.health.LatencyEWMAMS
		}
		health.LatencyEWMAMS = &average
		if r.recovering {
			if r.recoveredAt.IsZero() {
				r.recoveredAt = finished
			}
			if r.probeSuccesses >= policy.RecoverySuccesses && finished.Sub(r.recoveredAt) >= time.Duration(policy.RecoveryHoldMS)*time.Millisecond {
				r.recovering = false
			}
		}
	} else {
		r.probeSuccesses = 0
		r.recoveredAt = time.Time{}
		r.probeFailures++
		health.LatencyMS, health.LatencyEWMAMS, health.CheckedAt = r.health.LatencyMS, r.health.LatencyEWMAMS, r.health.CheckedAt
		if r.probeFailures < policy.FailureThreshold && previous != "unhealthy" {
			health.Status = "unknown"
			if r.health.CheckedAt > 0 {
				health.Status = "degraded"
			}
			health.Error = "probe_retry_pending"
		} else {
			r.recovering = true
		}
	}
	r.health = health
	if previous != health.Status {
		p.eventLocked("health_changed", r.config.ID, health.Status)
	}
	p.chooseLocked("probe_result")
	p.signalLocked()
	return outcome
}

func (p *Pool) wakeProbeLocked() {
	select {
	case p.probeWake <- struct{}{}:
	default:
	}
}
func (p *Pool) rescheduleProbeLocked() {
	p.nextProbe = time.Time{}
	if !p.closed && !p.probing && !p.cfg.DisablePeriodicProbe && p.saved.HealthPolicy.Enabled {
		from := p.lastProbeFinished
		if from.IsZero() {
			from = time.Now()
		}
		p.nextProbe = from.Add(time.Duration(p.effectiveHealthPolicyLocked().IntervalMS) * time.Millisecond)
	}
	p.wakeProbeLocked()
}
func (p *Pool) probeLoop() {
	defer p.wg.Done()
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return
		}
		due := p.nextProbe
		p.mu.Unlock()
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
		case <-p.probeWake:
			if timer != nil {
				timer.Stop()
			}
		case <-tick:
			p.mu.Lock()
			if !p.closed && !p.probing && !p.nextProbe.IsZero() && !time.Now().Before(p.nextProbe) {
				targets := make(map[string]bool)
				for _, n := range p.saved.Nodes {
					if n.Enabled {
						targets[n.ID] = false
					}
				}
				p.requestProbeLocked(targets, "enabled", "automatic")
			}
			p.mu.Unlock()
		}
	}
}
