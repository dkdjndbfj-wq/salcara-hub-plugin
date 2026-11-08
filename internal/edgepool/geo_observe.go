package edgepool

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode"
	"unicode/utf8"
)

func (m *geoManager) endpointHosts(nodes []nodeConfig) map[string]string {
	hosts := map[string]string{}
	for _, n := range nodes {
		u, err := url.Parse(n.ProxyURL)
		if err == nil && u != nil {
			hosts[n.ID] = u.Hostname()
		}
	}
	sources := m.pool.sources
	if sources == nil {
		return hosts
	}
	sources.mu.Lock()
	defer sources.mu.Unlock()
	for _, n := range nodes {
		if n.SourceID == "" {
			continue
		}
		for _, s := range sources.saved.Sources {
			if s.ID != n.SourceID {
				continue
			}
			for _, raw := range s.Active {
				if raw.Key != n.SourceKey {
					continue
				}
				if raw.Proxy != nil {
					host, _ := raw.Proxy["server"].(string)
					hosts[n.ID] = host
				}
			}
		}
	}
	return hosts
}
func cleanOrganization(raw string) string {
	var out strings.Builder
	for _, r := range strings.TrimSpace(raw) {
		if unicode.IsControl(r) || unicode.In(r, unicode.Cf) {
			continue
		}
		if out.Len()+utf8.RuneLen(r) > 160 {
			break
		}
		out.WriteRune(r)
	}
	return out.String()
}
func (d *geoDataset) lookup(ip netip.Addr) (string, uint, string, error) {
	var country struct {
		Country struct {
			Code string `maxminddb:"iso_code"`
		} `maxminddb:"country"`
	}
	var asn struct {
		Number uint   `maxminddb:"autonomous_system_number"`
		Name   string `maxminddb:"autonomous_system_organization"`
	}
	address := net.IP(ip.AsSlice())
	if d.country.Lookup(address, &country) != nil || d.asn.Lookup(address, &asn) != nil {
		return "", 0, "", errors.New("geo_lookup_failed")
	}
	code := strings.ToUpper(country.Country.Code)
	if len(code) != 2 || code[0] < 'A' || code[0] > 'Z' || code[1] < 'A' || code[1] > 'Z' {
		code = ""
	}
	return code, asn.Number, cleanOrganization(asn.Name), nil
}
func (m *geoManager) observe() error {
	p := m.pool
	p.mu.Lock()
	nodes := append([]nodeConfig{}, p.saved.Nodes...)
	p.mu.Unlock()
	hosts := m.endpointHosts(nodes)
	m.mu.Lock()
	data := m.data
	m.run.Total = len(nodes)
	m.mu.Unlock()
	if data == nil {
		return errors.New("geo_not_installed")
	}
	ctx, cancel := context.WithTimeout(p.ctx, 60*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	var resultMu sync.Mutex
	results := map[string]geoObservation{}
	for _, node := range nodes {
		wg.Add(1)
		go func(n nodeConfig) {
			defer wg.Done()
			value := NetworkInfo{Status: "unknown", Addresses: []string{}, Source: geoProvider, Method: "server_address", ObservedAt: time.Now().UnixMilli()}
			expiry := time.Now().Add(geoTTL)
			if data.built.Add(geoDatabaseAge).Before(expiry) {
				expiry = data.built.Add(geoDatabaseAge)
			}
			value.ExpiresAt = expiry.UnixMilli()
			host := hosts[n.ID]
			if host == "" {
				value.Status = "direct_or_external"
			} else {
				release, ok := p.acquireProbeSlot(ctx)
				if ok {
					lookupCtx, stop := context.WithTimeout(ctx, 5*time.Second)
					addresses, err := m.resolve(lookupCtx, "ip", host)
					stop()
					release()
					if err != nil || len(addresses) == 0 || len(addresses) > 64 {
						value.Status = "dns_failed"
					} else {
						public := true
						for _, ip := range addresses {
							if !publicSubscriptionIP(ip) {
								public = false
							}
						}
						if !public {
							value.Status = "direct_or_external"
						} else {
							value.Status = "observed"
							seen := map[string]bool{}
							first := true
							mixed := false
							for _, ip := range addresses {
								ip = ip.Unmap()
								if seen[ip.String()] {
									continue
								}
								seen[ip.String()] = true
								value.Addresses = append(value.Addresses, ip.String())
								country, asn, name, err := data.lookup(ip)
								if err != nil {
									value.Status = "lookup_failed"
									mixed = true
									break
								}
								if first {
									value.Country = country
									value.ASN = asn
									value.Organization = name
									first = false
								} else if country != value.Country || asn != value.ASN {
									mixed = true
								}
							}
							sort.Strings(value.Addresses)
							if mixed {
								value.Country = ""
								value.ASN = 0
								value.Organization = ""
								if value.Status == "observed" {
									value.Status = "mixed_networks"
								}
							} else if value.Country == "" && value.ASN == 0 {
								value.Status = "unknown"
							}
						}
					}
				} else {
					value.Status = "interrupted"
				}
			}
			resultMu.Lock()
			results[n.ID] = geoObservation{NetworkInfo: value, ConfigKey: geoConfigKey(n)}
			resultMu.Unlock()
			m.mu.Lock()
			m.run.Completed++
			m.mu.Unlock()
		}(node)
	}
	wg.Wait()
	if ctx.Err() != nil {
		return errors.New("geo_observation_interrupted")
	}
	// Nodes can change while DNS is in flight. Store only still-matching identities.
	p.mu.Lock()
	current := append([]nodeConfig{}, p.saved.Nodes...)
	p.mu.Unlock()
	accepted := map[string]geoObservation{}
	for _, n := range current {
		if v, ok := results[n.ID]; ok && v.ConfigKey == geoConfigKey(n) {
			accepted[n.ID] = v
		}
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return errors.New("geo_observation_interrupted")
	}
	next := m.saved
	next.Revision++
	next.Nodes = accepted
	next.NextObserveAt = time.Now().Add(6 * time.Hour).UnixMilli()
	return m.persist(next)
}

func (p *Pool) effectiveRegionLocked(n nodeConfig) string {
	return p.regionFor(n, p.saved.SelectionPolicy)
}
func (p *Pool) regionFor(n nodeConfig, policy SelectionPolicy) string {
	if n.Region != "" || !policy.UseObservedRegion {
		return n.Region
	}
	info := p.geo.info(n)
	if info.Status == "observed" {
		return info.Country
	}
	return ""
}
