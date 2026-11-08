package sourceissuer

import (
	"encoding/base64"
	"net/http"
	"strings"
	"testing"

	"salcara/hub/internal/hub"
)

func TestSourceGrantSizeEscapedAddressThroughIssuer(t *testing.T) {
	s, _ := fixtureService(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"group_id":7}`))
	}))
	v := fixtureSnapshot()
	prefix := "https://station.example/"
	v.APIBase = prefix + strings.Repeat("&", 4096-len(prefix))
	s.cfg.Routes[0].APIBase = v.APIBase
	request := requestBody(v, strings.Repeat("&", 4096))
	if len(request) <= 24<<10 {
		t.Fatal("fixture does not exercise escaped request size")
	}
	w := call(s, request)
	if w.Code != 200 {
		t.Fatalf("escaped source grant failed: %d %s", w.Code, w.Body.String())
	}
	grant, err := hub.ParseSignedSourceGrant(w.Body.Bytes())
	if err != nil {
		t.Fatal(err)
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(grant.Payload)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := hub.ParseSourceSnapshot(payload)
	if err != nil || actual.APIBase != v.APIBase || len(payload) <= 16<<10 {
		t.Fatalf("signed full address lost: bytes=%d err=%v", len(payload), err)
	}
	if w := call(s, []byte(strings.Repeat(" ", hub.MaxSignedSourceGrantBytes+1))); w.Code != 400 {
		t.Fatal("oversized issuer request accepted")
	}
}
