package standalone

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

func TestPublicClientVersions(t *testing.T) {
	manifest, err := webFiles.ReadFile("web/client-versions.json")
	var versions publicClientVersions
	if err != nil || json.Unmarshal(manifest, &versions) != nil {
		t.Fatal("missing embedded paired client versions")
	}
	s, _, _ := serverFixture(t)
	for _, path := range []string{Prefix + "/", Prefix + "/status"} {
		page := adminRequest(t, s, http.MethodGet, path, "", nil)
		if page.Code != http.StatusOK || strings.Contains(page.Body.String(), "{{MOBILE_CLIENT_VERSION}}") || strings.Contains(page.Body.String(), "{{BRIDGE_CLIENT_VERSION}}") {
			t.Fatalf("client versions were not rendered in %s", path)
		}
		for _, item := range []struct{ kind, value string }{{"mobile", versions.Mobile}, {"bridge", versions.Bridge}} {
			if !strings.Contains(page.Body.String(), `data-client-version="`+item.kind+`">v`+item.value+`<`) {
				t.Fatalf("%s does not show the manifest's %s version", path, item.kind)
			}
		}
		length := strconv.Itoa(page.Body.Len())
		if page.Header().Get("Content-Length") != length {
			t.Fatalf("rendered Content-Length is wrong for %s", path)
		}
		head := adminRequest(t, s, http.MethodHead, path, "", nil)
		if head.Code != http.StatusOK || head.Body.Len() != 0 || head.Header().Get("Content-Length") != length {
			t.Fatalf("HEAD differs from the rendered public page %s", path)
		}
	}
}
