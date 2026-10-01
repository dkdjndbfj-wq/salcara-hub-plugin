package launcher

import (
	"context"
	"errors"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"time"
)

// No environment proxy, cookies, model key, or caller-supplied headers. Resolve
// every new connection, reject mixed public/private DNS answers, and dial the
// validated IP directly so a second DNS lookup cannot rebind the target.
func updateClient() *http.Client {
	t := &http.Transport{Proxy: nil, DisableKeepAlives: true, DisableCompression: true,
		TLSHandshakeTimeout: 10 * time.Second, ResponseHeaderTimeout: 15 * time.Second,
		DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
			d := net.Dialer{Timeout: 10 * time.Second}
			return dialPublic(ctx, network, address, net.DefaultResolver.LookupNetIP, d.DialContext)
		}}
	return &http.Client{Transport: t, Timeout: 90 * time.Second, CheckRedirect: func(r *http.Request, via []*http.Request) error {
		if len(via) >= 5 {
			return errors.New("update redirect limit exceeded")
		}
		if _, err := validateURL(r.URL.String()); err != nil {
			return err
		}
		r.Header = http.Header{"Accept": {"application/octet-stream,application/json"}, "User-Agent": {"Salcara-Hub-Launcher/1"}}
		return nil
	}}
}

func validateURL(raw string) (*url.URL, error) {
	if len(raw) == 0 || len(raw) > 8192 || strings.TrimSpace(raw) != raw {
		return nil, errors.New("invalid update URL")
	}
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" || u.Opaque != "" || u.Port() != "" && u.Port() != "443" {
		return nil, errors.New("更新源必须为无凭据的公网 HTTPS 443")
	}
	host := strings.ToLower(u.Hostname())
	if strings.Contains(host, "%") || host == "localhost" || strings.HasSuffix(host, ".localhost") || !strings.ContainsAny(host, ".:") {
		return nil, errors.New("更新不允许内部地址")
	}
	if ip, err := netip.ParseAddr(host); err == nil && !publicIP(ip) {
		return nil, errors.New("更新不允许非公网 IP")
	}
	return u, nil
}

var blockedNetworks = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"), netip.MustParsePrefix("10.0.0.0/8"), netip.MustParsePrefix("100.64.0.0/10"), netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("169.254.0.0/16"), netip.MustParsePrefix("172.16.0.0/12"), netip.MustParsePrefix("192.0.0.0/24"), netip.MustParsePrefix("192.0.2.0/24"),
	netip.MustParsePrefix("192.88.99.0/24"), netip.MustParsePrefix("192.168.0.0/16"), netip.MustParsePrefix("198.18.0.0/15"), netip.MustParsePrefix("198.51.100.0/24"),
	netip.MustParsePrefix("203.0.113.0/24"), netip.MustParsePrefix("224.0.0.0/4"), netip.MustParsePrefix("240.0.0.0/4"), netip.MustParsePrefix("2001::/23"),
	netip.MustParsePrefix("2001:db8::/32"), netip.MustParsePrefix("2002::/16"), netip.MustParsePrefix("3fff::/20"),
}

func publicIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, p := range blockedNetworks {
		if p.Contains(ip) {
			return false
		}
	}
	return true
}
func dialPublic(ctx context.Context, network, address string, lookup func(context.Context, string, string) ([]netip.Addr, error), dial func(context.Context, string, string) (net.Conn, error)) (net.Conn, error) {
	host, port, err := net.SplitHostPort(address)
	if err != nil || port != "443" {
		return nil, errors.New("invalid update port")
	}
	lookupCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addresses, err := lookup(lookupCtx, "ip", host)
	if err != nil || len(addresses) == 0 || len(addresses) > 64 {
		return nil, errors.New("update DNS lookup failed")
	}
	for _, ip := range addresses {
		if !publicIP(ip) {
			return nil, errors.New("update DNS returned a non-public address")
		}
	}
	for _, ip := range addresses {
		conn, err := dial(ctx, network, net.JoinHostPort(ip.String(), port))
		if err == nil {
			return conn, nil
		}
	}
	return nil, errors.New("update public server connection failed")
}
func get(ctx context.Context, client *http.Client, raw string, limit int64) (*http.Response, error) {
	u, err := validateURL(raw)
	if err != nil {
		return nil, err
	}
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return nil, errors.New("cannot create update request")
	}
	r.Header.Set("Accept", "application/octet-stream,application/json")
	r.Header.Set("User-Agent", "Salcara-Hub-Launcher/1")
	res, err := client.Do(r)
	if err != nil {
		return nil, errors.New("更新网络请求失败或超时")
	}
	if res.StatusCode != http.StatusOK {
		res.Body.Close()
		return nil, errors.New("更新源尚未发布或暂时不可用")
	}
	if res.ContentLength > limit || res.Header.Get("Content-Encoding") != "" {
		res.Body.Close()
		return nil, errors.New("更新下载超过大小限制或编码无效")
	}
	return res, nil
}
