package hub

import (
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func escapedSizeStation() *StationPolicy {
	p := &StationPolicy{Version: 2, APIBases: []string{}}
	for i := 0; i < 32; i++ {
		prefix := fmt.Sprintf("https://api.example/team%d/", i)
		base := prefix + strings.Repeat("&", 4096-len(prefix))
		if i < 16 {
			p.APIBases = append(p.APIBases, base)
		} else {
			p.ExternalAPIBases = append(p.ExternalAPIBases, base)
		}
		p.ReverseProxyAPIBases = append(p.ReverseProxyAPIBases, base)
	}
	return p
}

func TestStationPolicySizeEscapedOverlappingListsPublish(t *testing.T) {
	p := escapedSizeStation()
	raw, _ := json.Marshal(p)
	if len(raw) < 1<<20 || len(raw) > MaxStationPolicyJSONBytes {
		t.Fatalf("fixture does not exercise escaped wire budget: %d", len(raw))
	}
	parsed, err := ParseStationPolicy(raw)
	if err != nil || len(parsed.ReverseProxyAPIBases) != 32 {
		t.Fatalf("valid full policy rejected: %v", err)
	}
	h, _, _, _, _ := sourceContractFixture(t)
	policy := h.CurrentRemoteAccessPolicy()
	policy.Station = p
	if err := h.ApplyRemoteAccessPolicy(policy, nil); err != nil {
		t.Fatal(err)
	}
	w := httptest.NewRecorder()
	h.handlePing(w, httptest.NewRequest("GET", "/ping", nil), "")
	if w.Code != 200 || w.Body.Len() > MaxSourcePolicyWireBytes || !strings.Contains(w.Body.String(), `\u0026`) {
		t.Fatalf("published policy cannot fit consumer budget: %d", w.Body.Len())
	}
	// Deployment trust-anchor input retains its original, separate small limit.
	deployment, _ := json.Marshal(policy)
	if _, err := ParseRemoteAccessPolicy(deployment); err == nil {
		t.Fatal("administrative budget enlarged deployment file budget")
	}
	if _, err := ParseStationPolicy(append(raw, []byte(strings.Repeat(" ", MaxStationPolicyJSONBytes))...)); err == nil {
		t.Fatal("unbounded station input accepted")
	}
}

func TestSourceGrantSizeEscapedAddressAndBoundedPayload(t *testing.T) {
	base := escapedSizeStation().APIBases[0]
	raw, _ := json.Marshal(SourceSnapshot{Version: 1, APIBase: base})
	if len(raw) <= 16<<10 {
		t.Fatal("fixture must exceed previous snapshot budget")
	}
	if _, err := ParseSourceSnapshot(raw); err != nil {
		t.Fatal(err)
	}
	grant := SignedSourceGrant{Payload: base64.RawURLEncoding.EncodeToString(raw), Signature: strings.Repeat("A", 86)}
	wire, _ := json.Marshal(grant)
	if _, err := ParseSignedSourceGrant(wire); err != nil {
		t.Fatal(err)
	}
	if _, err := ParseSourceSnapshot([]byte(strings.Repeat(" ", MaxSourceSnapshotBytes+1))); err == nil {
		t.Fatal("oversized snapshot accepted")
	}
	grant.Payload = strings.Repeat("A", MaxSourcePayloadBytes+1)
	wire, _ = json.Marshal(grant)
	if _, err := ParseSignedSourceGrant(wire); err == nil {
		t.Fatal("oversized base64 payload accepted")
	}
}

func TestSourceGrantSizeEscapedSignedScopeStillChecksRevocation(t *testing.T) {
	h, request, command, snapshot, private := sourceContractFixture(t)
	base := escapedSizeStation().APIBases[0]
	policy := h.CurrentRemoteAccessPolicy()
	policy.Station.APIBases = []string{base}
	if err := h.ApplyRemoteAccessPolicy(policy, nil); err != nil {
		t.Fatal(err)
	}
	snapshot.APIBase = base
	snapshot.Origin, _ = canonicalAPIBase(base)
	snapshot.PolicyRevision = h.CurrentRemoteAccessPolicy().Revision
	signSourceFixture(t, &command, snapshot, private)
	command.sourceGrantRaw, _ = json.Marshal(command.SourceGrant)
	if len(command.SourceGrant.Payload) <= 16<<10 {
		t.Fatal("fixture did not exceed old encoded-payload limit")
	}
	if code := h.evaluateSourcePolicy("receipt-owner", request, command); code != "" {
		t.Fatalf("valid signed escaped address denied: %s", code)
	}
	policy = h.CurrentRemoteAccessPolicy()
	policy.Station.APIBases = []string{}
	if err := h.ApplyRemoteAccessPolicy(policy, nil); err != nil {
		t.Fatal(err)
	}
	if code := h.evaluateSourcePolicy("receipt-owner", request, command); code == "" {
		t.Fatal("large grant survived source revocation")
	}
}
