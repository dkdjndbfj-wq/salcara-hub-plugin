package edgepool

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync"
	"time"
)

// Runtime describes business traffic since this process started. Probes are not
// included. Queue entries have no assigned node, so node queued fields stay zero.
type Runtime struct {
	ActiveRequests    int    `json:"active_requests"`
	ActiveStreams     int    `json:"active_streams"`
	QueuedRequests    int    `json:"queued_requests"`
	QueuedStreams     int    `json:"queued_streams"`
	CompletedRequests uint64 `json:"completed_requests"`
	FailedRequests    uint64 `json:"failed_requests"`
	RejectedRequests  uint64 `json:"rejected_requests"`
	CancelledRequests uint64 `json:"cancelled_requests"`
}

type queueWaiter struct{ stream bool }
type admissionError struct {
	code   string
	status int
}

func (e *admissionError) Error() string { return e.code }

func streamRequest(r *http.Request) bool {
	if r.Method != http.MethodGet {
		return false
	}
	if strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream") {
		return true
	}
	switch r.URL.Path {
	case "/salcara-hub/v1/app/stream", "/salcara-hub/v1/bridge/stream", "/salcara-hub/app/stream", "/salcara-hub/bridge/stream":
		return true
	}
	return false
}

func (p *Pool) signalLocked() {
	if p.notify != nil {
		close(p.notify)
	}
	p.notify = make(chan struct{})
}
func (p *Pool) firstWaiterLocked(stream bool) *queueWaiter {
	for _, w := range p.queue {
		if w.stream == stream {
			return w
		}
	}
	return nil
}
func (p *Pool) removeWaiterLocked(w *queueWaiter) {
	for i, v := range p.queue {
		if v == w {
			p.queue = append(p.queue[:i], p.queue[i+1:]...)
			if w.stream {
				p.runtime.QueuedStreams--
			} else {
				p.runtime.QueuedRequests--
			}
			p.signalLocked()
			return
		}
	}
}

func (p *Pool) routeCapacityLocked(r *route, stream bool) bool {
	if !p.sourceCapacityLocked(r, stream) {
		return false
	}
	if p.unverifiedLocked(r, time.Now()) || r.health.Status == "unhealthy" || r.recovering {
		if r.runtime.ActiveRequests+r.runtime.ActiveStreams >= 1 {
			return false
		}
	}
	if stream {
		return r.runtime.ActiveStreams < r.config.MaxStreams
	}
	return r.runtime.ActiveRequests < r.config.MaxRequests
}

// selectLocked and reservation share one critical section: simultaneous callers
// cannot all observe a free slot and oversubscribe it. Non-balance modes retain
// their fixed selected route and queue on its capacity rather than load-balance.
func (p *Pool) selectLocked(stream bool) (*route, bool) {
	p.chooseLocked("health_expired")
	now := time.Now()
	if p.saved.Mode != "balance" {
		r := p.routes[p.active]
		if r == nil || !r.config.Enabled || r.config.Draining {
			return nil, false
		}
		if p.saved.Mode != "manual" && !p.usableLocked(r, now) {
			return nil, false
		}
		if !p.routeCapacityLocked(r, stream) {
			return nil, true
		}
		return r, true
	}
	var best *route
	available := false
	n := len(p.saved.Nodes)
	candidates := make([]*route, 0, n)
	for offset := 0; offset < n; offset++ {
		i := (int(p.balanceCursor%uint64(n)) + offset) % n
		r := p.routes[p.saved.Nodes[i].ID]
		if !p.usableLocked(r, now) {
			continue
		}
		candidates = append(candidates, r)
	}
	regional := p.filterRegionsLocked(candidates)
	if p.saved.SelectionPolicy.RegionMode == "prefer" {
		for _, r := range regional {
			if p.routeCapacityLocked(r, stream) {
				candidates = regional
				break
			}
		}
	} else {
		candidates = regional
	}
	for _, r := range candidates {
		available = true
		if !p.routeCapacityLocked(r, stream) {
			continue
		}
		load := r.runtime.ActiveRequests
		if stream {
			load = r.runtime.ActiveStreams
		}
		if best == nil {
			best = r
			continue
		}
		bestLoad := best.runtime.ActiveRequests
		if stream {
			bestLoad = best.runtime.ActiveStreams
		}
		if (load+1)*best.config.Weight < (bestLoad+1)*r.config.Weight {
			best = r
		}
	}
	return best, available
}

type requestLease struct {
	pool   *Pool
	route  *route
	source *Runtime
	stream bool
	ctx    context.Context
	once   sync.Once
}

func (p *Pool) acquire(ctx context.Context, stream bool) (*requestLease, error) {
	p.mu.Lock()
	var waiter *queueWaiter
	var timer *time.Timer
	var timeout <-chan time.Time
	var deadline time.Time
	defer func() {
		if timer != nil {
			timer.Stop()
		}
	}()
	for {
		if ctx.Err() != nil {
			if waiter != nil {
				p.removeWaiterLocked(waiter)
			}
			p.runtime.CancelledRequests++
			p.mu.Unlock()
			return nil, ctx.Err()
		}
		if p.closed {
			if waiter != nil {
				p.removeWaiterLocked(waiter)
			}
			p.runtime.RejectedRequests++
			p.mu.Unlock()
			return nil, &admissionError{"stopped", http.StatusServiceUnavailable}
		}
		if waiter != nil && !time.Now().Before(deadline) {
			p.removeWaiterLocked(waiter)
			p.runtime.RejectedRequests++
			p.mu.Unlock()
			return nil, &admissionError{"queue_timeout", http.StatusServiceUnavailable}
		}
		r, available := p.selectLocked(stream)
		if !available {
			if waiter != nil {
				p.removeWaiterLocked(waiter)
			}
			p.runtime.RejectedRequests++
			p.mu.Unlock()
			return nil, &admissionError{"no_available_route", http.StatusServiceUnavailable}
		}
		globalAvailable := p.runtime.ActiveRequests < p.saved.Limits.MaxRequests
		if stream {
			globalAvailable = p.runtime.ActiveStreams < p.saved.Limits.MaxStreams
		}
		first := p.firstWaiterLocked(stream)
		if r != nil && globalAvailable && (first == nil || first == waiter) {
			if waiter != nil {
				p.removeWaiterLocked(waiter)
			}
			if stream {
				p.runtime.ActiveStreams++
				r.runtime.ActiveStreams++
				if r.source != nil {
					r.source.ActiveStreams++
				}
			} else {
				p.runtime.ActiveRequests++
				r.runtime.ActiveRequests++
				if r.source != nil {
					r.source.ActiveRequests++
				}
			}
			p.balanceCursor++
			lease := &requestLease{pool: p, route: r, source: r.source, stream: stream, ctx: ctx}
			p.mu.Unlock()
			return lease, nil
		}
		if waiter == nil {
			if len(p.queue) >= p.saved.Limits.QueueLimit {
				p.runtime.RejectedRequests++
				p.mu.Unlock()
				return nil, &admissionError{"queue_full", http.StatusTooManyRequests}
			}
			waiter = &queueWaiter{stream: stream}
			p.queue = append(p.queue, waiter)
			if stream {
				p.runtime.QueuedStreams++
			} else {
				p.runtime.QueuedRequests++
			}
			timer = time.NewTimer(time.Duration(p.saved.Limits.QueueTimeoutMS) * time.Millisecond)
			deadline = time.Now().Add(time.Duration(p.saved.Limits.QueueTimeoutMS) * time.Millisecond)
			timeout = timer.C
		}
		notify := p.notify
		p.mu.Unlock()
		select {
		case <-ctx.Done():
		case <-notify:
		case <-timeout:
			p.mu.Lock()
			p.removeWaiterLocked(waiter)
			if ctx.Err() != nil {
				p.runtime.CancelledRequests++
				p.mu.Unlock()
				return nil, ctx.Err()
			}
			p.runtime.RejectedRequests++
			p.mu.Unlock()
			return nil, &admissionError{"queue_timeout", http.StatusServiceUnavailable}
		}
		p.mu.Lock()
	}
}

// finish is shared by RoundTrip failures, EOF, Close and cancellation. Every
// accepted request releases exactly once, including retired/deleted routes.
func (l *requestLease) finish(outcome string, passiveFailure bool) {
	l.once.Do(func() {
		p := l.pool
		if passiveFailure && l.ctx.Err() == nil {
			p.failed(l.route)
		}
		p.mu.Lock()
		defer p.mu.Unlock()
		if l.stream {
			p.runtime.ActiveStreams--
			l.route.runtime.ActiveStreams--
			if l.source != nil {
				l.source.ActiveStreams--
			}
		} else {
			p.runtime.ActiveRequests--
			l.route.runtime.ActiveRequests--
			if l.source != nil {
				l.source.ActiveRequests--
			}
		}
		switch outcome {
		case "completed":
			p.runtime.CompletedRequests++
			l.route.runtime.CompletedRequests++
			if l.source != nil {
				l.source.CompletedRequests++
			}
		case "failed":
			p.runtime.FailedRequests++
			l.route.runtime.FailedRequests++
			if l.source != nil {
				l.source.FailedRequests++
			}
		default:
			p.runtime.CancelledRequests++
			l.route.runtime.CancelledRequests++
			if l.source != nil {
				l.source.CancelledRequests++
			}
		}
		p.pruneSourceRuntimesLocked()
		p.signalLocked()
	})
}

type leasedBody struct {
	io.ReadCloser
	lease        *requestLease
	failedStatus bool
	stop         func() bool
	closeOnce    sync.Once
	closeErr     error
}

func (b *leasedBody) finish(outcome string, passive bool) {
	if b.stop != nil {
		b.stop()
	}
	b.lease.finish(outcome, passive)
}
func (b *leasedBody) Read(data []byte) (int, error) {
	n, err := b.ReadCloser.Read(data)
	if err != nil {
		if b.lease.ctx.Err() != nil {
			b.finish("cancelled", false)
		} else if errors.Is(err, io.EOF) {
			if b.failedStatus {
				b.finish("failed", false)
			} else {
				b.finish("completed", false)
			}
		} else {
			b.finish("failed", true)
		}
	}
	return n, err
}
func (b *leasedBody) Close() error {
	b.closeOnce.Do(func() { b.closeErr = b.ReadCloser.Close() })
	if b.lease.ctx.Err() != nil {
		b.finish("cancelled", false)
	} else if b.closeErr != nil {
		b.finish("failed", true)
	} else {
		b.finish("cancelled", false)
	}
	return b.closeErr
}

func leaseResponse(lease *requestLease, response *http.Response) {
	if response.Body == nil {
		lease.finish("completed", false)
		return
	}
	body := &leasedBody{ReadCloser: response.Body, lease: lease, failedStatus: response.StatusCode >= 500 || response.StatusCode == 429}
	// Install the callback only after the body has been fully initialized. It
	// closes the actual connection before returning capacity to the scheduler.
	ready := make(chan struct{})
	body.stop = context.AfterFunc(lease.ctx, func() { <-ready; body.Close() })
	close(ready)
	response.Body = body
}
