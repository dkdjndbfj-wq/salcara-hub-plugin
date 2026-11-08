package hub

import "net/url"

// SourceAuthorizationStatus describes configured coverage, not a successful
// network probe or a proof that an upstream credential is still usable.
func (p RemoteAccessPolicy) SourceAuthorizationStatus() []map[string]any {
	if p.Station == nil {
		return nil
	}
	station := p.Station
	proxyBases := station.ReverseProxyAPIBases
	if station.Version == 1 {
		proxyBases = station.APIBases
	}
	types := []struct {
		id, source string
		bases      []string
		official   bool
	}{
		{"api", "site_api", station.APIBases, false},
		{"external_api", "site_api", station.ExternalAPIBases, false},
		{"reverse_proxy", "verified_proxy", proxyBases, false},
		{"official_account", "official_account_proxy", nil, true},
	}
	out := make([]map[string]any, 0, len(types))
	for _, category := range types {
		enabled := len(category.bases) > 0 || category.official && station.AllowOfficialAccounts
		covered := 0
		for _, base := range category.bases {
			if sourceIssuersCoverBase(p.Issuers, category.source, base) {
				covered++
			}
		}
		status := "disabled"
		if enabled {
			switch {
			case p.Mode == "disabled":
				status = "paused"
			case p.Mode == "open":
				status = "allowlist"
			case category.official:
				status = "computer_verification"
			case p.Audience != "" && covered == len(category.bases):
				status = "issuer_configured"
			default:
				status = "issuer_missing"
			}
		}
		out = append(out, map[string]any{"id": category.id, "enabled": enabled, "status": status, "addressCount": len(category.bases), "issuerCoveredCount": covered})
	}
	return out
}

// Agent-disjoint issuers are valid: acquisition selects after checking the
// command's Agent. Report configured coverage only when every declared Agent
// scope has one issuer, without claiming support for undeclared Agents.
func sourceIssuersCoverBase(issuers []SourceIssuer, source, base string) bool {
	origin, ok := canonicalAPIBase(base)
	if !ok {
		return false
	}
	unrestricted := 0
	agents := map[string]int{}
	for _, issuer := range issuers {
		u, err := url.Parse(issuer.AuthorizationURL)
		if err != nil || u.Scheme+"://"+u.Host != origin || !containsSourceValue(issuer.SourceTypes, source) || !containsSourceValue(issuer.Origins, origin) || !containsSourceValue(issuer.APIBases, base) {
			continue
		}
		if len(issuer.AgentIDs) == 0 {
			unrestricted++
			continue
		}
		seen := map[string]bool{}
		for _, agent := range issuer.AgentIDs {
			if !seen[agent] {
				agents[agent]++
				seen[agent] = true
			}
		}
	}
	if unrestricted > 1 || unrestricted == 0 && len(agents) == 0 {
		return false
	}
	for _, count := range agents {
		if unrestricted+count != 1 {
			return false
		}
	}
	return true
}

func containsSourceValue(values []string, value string) bool {
	for _, candidate := range values {
		if candidate == value {
			return true
		}
	}
	return false
}
