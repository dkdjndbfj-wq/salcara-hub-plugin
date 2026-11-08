package hub

import (
	"encoding/json"
	"testing"
)

func TestStationOfficialConnectorAcceptsOnlyProductionManagedCodeRoutes(t *testing.T) {
	for _, agent := range []string{"codex", "claude", "hermes"} {
		t.Run(agent, func(t *testing.T) {
			h, pair, req, _, snapshot := officialStationFixture(t)
			req.Command, _ = json.Marshal(map[string]any{"type": "session.send", "accountId": snapshot.CredentialHandle, "agent": agent, "tool": agent,
				"sessionKey": agent + ":fixture", "controlSurface": "cli", "model": snapshot.Model})
			challenge := issueSourceChallengeFixture(t, h, pair, req)
			snapshot.Agent, snapshot.CommandHash, snapshot.ChallengeID = agent, challenge.CommandHash, challenge.ChallengeID
			raw, _ := json.Marshal(map[string]any{"officialAccount": snapshot})
			grant, err := h.officialSourceGrant(raw, challenge)
			if agent == "hermes" {
				if err == nil {
					t.Fatal("unimplemented official extension route received a built-in grant")
				}
				// The policy check also rejects a synthetic locally signed grant,
				// so a cached assertion cannot bypass the current route contract.
				snapshot.IssuedAt, snapshot.ExpiresAt = challenge.ExpiresAt-120, challenge.ExpiresAt
				signSourceFixture(t, &req, snapshot, h.officialPrivateKey)
				if code := h.evaluateSourcePolicy("receipt-owner", pair, req); code != "source_agent_denied" {
					t.Fatalf("unimplemented route not rejected by final policy: %s", code)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			req.SourceGrant = &grant
			if code, source := h.executionSourceFor("receipt-owner", pair, req); code != "" || source == nil || source.Agent != agent || source.SourceType != "official_account_proxy" || source.Station == nil || !source.Station.AllowOfficialAccounts {
				t.Fatalf("managed official %s rejected: %s %#v", agent, code, source)
			}
		})
	}
}

func TestStationVerifiedProxyGrantCannotEnlargeOperatorStationScope(t *testing.T) {
	for _, name := range []string{"registered", "unregistered-source", "different-origin", "different-tenant"} {
		t.Run(name, func(t *testing.T) {
			h, pair, req, snapshot, private := sourceContractFixture(t)
			p := h.CurrentRemoteAccessPolicy()
			p.Issuers[0].SourceTypes = []string{"verified_proxy"}
			if name == "unregistered-source" {
				p.Issuers[0].SourceTypes = []string{"site_api"}
			}
			// Deliberately grant the issuer two bases. Its signature still cannot
			// enlarge the Hub operator's independent station membership policy.
			p.Issuers[0].Origins = []string{"https://example.com", "https://foreign.test"}
			p.Issuers[0].APIBases = []string{"https://example.com", "https://example.com/other", "https://foreign.test"}
			if err := h.ApplyRemoteAccessPolicy(p, nil); err != nil {
				t.Fatal(err)
			}
			snapshot.PolicyRevision = h.CurrentRemoteAccessPolicy().Revision
			snapshot.SourceType = "verified_proxy"
			switch name {
			case "different-origin":
				snapshot.Origin, snapshot.APIBase = "https://foreign.test", "https://foreign.test"
			case "different-tenant":
				snapshot.APIBase = "https://example.com/other"
			}
			signSourceFixture(t, &req, snapshot, private)
			code := h.evaluateSourcePolicy("receipt-owner", pair, req)
			if name == "registered" {
				if code != "" {
					t.Fatalf("registered same-station proxy denied: %s", code)
				}
			} else if code == "" {
				t.Fatal("proxy signature expanded its registered source or station policy")
			}
		})
	}
}
