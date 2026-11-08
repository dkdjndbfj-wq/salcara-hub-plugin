package edgepool

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strconv"
	"strings"
	"time"
)

const subscriptionBytes = 1 << 20

var subscriptionBlocked = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"), netip.MustParsePrefix("203.0.113.0/24"),
	netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001::/23"), netip.MustParsePrefix("2001:db8::/32"),
	netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
}

func publicSubscriptionIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, prefix := range subscriptionBlocked {
		if prefix.Contains(ip) {
			return false
		}
	}
	return true
}

func subscriptionURL(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) == 0 || len(raw) > 8192 || strings.TrimSpace(raw) != raw || u == nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" {
		return nil, errors.New("subscription_url_invalid")
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, "%") || host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || !strings.ContainsAny(host, ".:") {
		return nil, errors.New("subscription_url_invalid")
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("subscription_url_invalid")
		}
	}
	if ip, err := netip.ParseAddr(host); err == nil && !publicSubscriptionIP(ip) {
		return nil, errors.New("subscription_url_invalid")
	}
	return u, nil
}

type subscriptionResolver func(context.Context, string, string) ([]netip.Addr, error)
type subscriptionDialer func(context.Context, string, string) (net.Conn, error)

func subscriptionProxy(raw string) (*url.URL, error) {
	u, err := parseProxy(raw)
	if err != nil || u == nil {
		return nil, errors.New("unsafe_proxy_address")
	}
	check := *u
	check.Scheme = "https"
	check.User = nil
	if _, err := subscriptionURL(check.String()); err != nil {
		return nil, errors.New("unsafe_proxy_address")
	}
	return u, nil
}

func dialPublicSubscription(ctx context.Context, network, address string, lookup subscriptionResolver, dial subscriptionDialer) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return nil, errors.New("subscription_fetch_failed")
	}
	addresses, err := lookup(ctx, "ip", host)
	if err != nil || len(addresses) == 0 || len(addresses) > 64 {
		return nil, errors.New("subscription_fetch_failed")
	}
	for _, ip := range addresses {
		if !publicSubscriptionIP(ip) {
			return nil, errors.New("subscription_fetch_failed")
		}
	}
	for _, ip := range addresses {
		conn, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, errors.New("subscription_fetch_failed")
}

func subscriptionClient(lookup subscriptionResolver, dial subscriptionDialer) *http.Client {
	transport := &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 3 * time.Second, TLSNextProto: make(map[string]func(string, *tls.Conn) http.RoundTripper),
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			return dialPublicSubscription(ctx, network, address, lookup, dial)
		}}
	return &http.Client{Transport: transport, Timeout: 5 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) > 3 {
			return errors.New("subscription_fetch_failed")
		}
		if _, err := subscriptionURL(r.URL.String()); err != nil {
			return err
		}
		r.Header = http.Header{"Accept": []string{"application/yaml,application/json,text/plain"}, "User-Agent": []string{"Salcara-Subscription-Inspector/1"}}
		return nil
	}}
}

func fetchSubscription(ctx context.Context, client *http.Client, raw string) ([]byte, error) {
	u, err := subscriptionURL(raw)
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	req.Header.Set("Accept", "application/yaml,application/json,text/plain")
	req.Header.Set("User-Agent", "Salcara-Subscription-Inspector/1")
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("subscription_fetch_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.ContentLength > subscriptionBytes {
		return nil, errors.New("subscription_fetch_failed")
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, subscriptionBytes+1))
	if err != nil || len(body) > subscriptionBytes {
		return nil, errors.New("subscription_fetch_failed")
	}
	return body, nil
}
