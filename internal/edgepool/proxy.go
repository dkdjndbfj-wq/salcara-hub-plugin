package edgepool

import (
	"context"
	"errors"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/netip"
	"strings"
	"time"
)

type fixedTransport struct{ pool *Pool }

func (t fixedTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	p := t.pool
	// Defense in depth: neither a request URL nor a redirect may turn this
	// fixed-origin ingress into an open proxy.
	if r.URL.Scheme != p.origin.Scheme || r.URL.Host != p.origin.Host {
		return nil, errors.New("fixed origin required")
	}
	lease, err := p.acquire(r.Context(), streamRequest(r))
	if err != nil {
		return nil, err
	}
	route := lease.route
	transport := route.once
	if r.Method == http.MethodGet || r.Method == http.MethodHead || r.Method == http.MethodOptions {
		transport = route.normal
	}
	response, err := transport.RoundTrip(r)
	if err != nil {
		// A disconnected caller is not evidence that the proxy node failed.
		if r.Context().Err() == nil {
			lease.finish("failed", true)
		} else {
			lease.finish("cancelled", false)
		}
		return nil, errors.New("upstream connection failed")
	}
	if response.StatusCode == http.StatusProxyAuthRequired {
		response.Body.Close()
		lease.finish("failed", true)
		return nil, errors.New("upstream connection failed")
	}
	leaseResponse(lease, response)
	return response, nil
}

// Handler forwards only /salcara-hub to the fixed deployment origin. It does
// not expose ControlHandler, accept arbitrary destinations, or retry POSTs.
func (p *Pool) Handler() http.Handler {
	proxy := &httputil.ReverseProxy{
		Rewrite: func(pr *httputil.ProxyRequest) {
			// A fixed Hub origin must not point back to this or another Edge.
			// This marker is a loop guard, not an authentication credential.
			pr.Out.Header.Set("X-Salcara-Edge-Hop", "1")
			pr.SetURL(p.origin)
			pr.Out.Host = p.origin.Host
			for name := range pr.Out.Header {
				if strings.HasPrefix(strings.ToLower(name), "x-forwarded-") {
					pr.Out.Header.Del(name)
				}
			}
			pr.Out.Header.Del("Forwarded")
			pr.SetXForwarded()
			client, proto := p.forwardedClient(pr.In)
			pr.Out.Header.Set("X-Forwarded-For", client)
			pr.Out.Header.Set("X-Forwarded-Proto", proto)
			pr.Out.Header.Del("X-Real-IP")
		},
		Transport:     fixedTransport{pool: p},
		FlushInterval: -1 * time.Millisecond,
		ErrorLog:      log.New(io.Discard, "", 0),
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			var admission *admissionError
			if errors.As(err, &admission) {
				w.Header().Set("Retry-After", "2")
				failure(w, admission.status, admission.code)
				return
			}
			failure(w, http.StatusBadGateway, "upstream_unavailable")
		},
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if len(r.Header.Values("X-Salcara-Edge-Hop")) != 0 {
			failure(w, http.StatusLoopDetected, "origin_loop_detected")
			return
		}
		if r.Method == http.MethodConnect || r.URL.IsAbs() || !allowedPath(r) {
			failure(w, http.StatusNotFound, "not_found")
			return
		}
		ctx, cancel := context.WithCancel(r.Context())
		stop := context.AfterFunc(p.ctx, cancel)
		defer func() { stop(); cancel() }()
		proxy.ServeHTTP(w, r.WithContext(ctx))
	})
}

func (p *Pool) trustedIP(ip netip.Addr) bool {
	for _, prefix := range p.trusted {
		if prefix.Contains(ip.Unmap()) {
			return true
		}
	}
	return false
}

func (p *Pool) forwardedClient(r *http.Request) (string, string) {
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		host = r.RemoteAddr
	}
	ip, err := netip.ParseAddr(host)
	proto := "http"
	if r.TLS != nil {
		proto = "https"
	}
	if err != nil {
		return "unknown", proto
	}
	client := ip.Unmap().String()
	if !p.trustedIP(ip) {
		return client, proto
	}
	// Only a specifically trusted immediate frontend can attest the scheme.
	if value := r.Header.Get("X-Forwarded-Proto"); value == "https" || value == "http" {
		proto = value
	}
	chain := strings.Split(r.Header.Get("X-Forwarded-For"), ",")
	if len(chain) > 16 {
		return client, proto
	}
	for i := len(chain) - 1; i >= 0; i-- {
		candidate, err := netip.ParseAddr(strings.TrimSpace(chain[i]))
		if err != nil {
			return client, proto
		}
		client = candidate.Unmap().String()
		if !p.trustedIP(candidate) {
			break
		}
	}
	return client, proto
}

func allowedPath(r *http.Request) bool {
	path := r.URL.Path
	if path != "/salcara-hub" && !strings.HasPrefix(path, "/salcara-hub/") {
		return false
	}
	if strings.ContainsAny(path, "\\\x00\r\n") {
		return false
	}
	for _, segment := range strings.Split(path, "/") {
		if segment == "." || segment == ".." {
			return false
		}
	}
	escaped := strings.ToLower(r.URL.RawPath)
	return !strings.Contains(escaped, "%2f") && !strings.Contains(escaped, "%5c") && !strings.Contains(escaped, "%2e")
}
