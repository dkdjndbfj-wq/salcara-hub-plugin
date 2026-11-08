package standalone

import (
	"encoding/json"
	"html"
	"regexp"
	"strings"
)

// One embedded release manifest supplies both public pages. These are the
// release's paired client versions, not a change to admission/minimum versions.
type publicClientVersions struct {
	Schema int    `json:"schema"`
	Mobile string `json:"mobile"`
	Bridge string `json:"bridge"`
}

var publicClientVersionReplacer = func() *strings.Replacer {
	data, err := webFiles.ReadFile("web/client-versions.json")
	var versions publicClientVersions
	if err != nil || json.Unmarshal(data, &versions) != nil || versions.Schema != 1 {
		panic("invalid embedded client version manifest")
	}
	versionPattern := regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)$`)
	for _, version := range []string{versions.Mobile, versions.Bridge} {
		if len(version) > 64 || !versionPattern.MatchString(version) {
			panic("invalid supported client version")
		}
	}
	return strings.NewReplacer("{{MOBILE_CLIENT_VERSION}}", html.EscapeString(versions.Mobile), "{{BRIDGE_CLIENT_VERSION}}", html.EscapeString(versions.Bridge))
}()
