package hub

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode"
)

// Only validate the wire contract here. The paired Bridge owns the installed
// Agent catalog, exact root grants and filesystem handles; the Hub never turns
// a project selection into model-source authority or a session start.
func validateWorkspaceProjectsCommand(meta map[string]json.RawMessage) error {
	if commandString(meta, "type") != "workspace.projects" {
		return nil
	}
	invalid := errors.New("invalid workspace projects command")
	values := make(map[string]string, len(meta))
	for key, raw := range meta {
		switch key {
		case "type", "controlSurface", "tool", "workspaceMode", "action", "cwd", "path", "cursor", "name":
		default:
			return invalid
		}
		var value string
		if string(raw) == "null" || json.Unmarshal(raw, &value) != nil || len(value) > 4096 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return invalid
		}
		values[key] = value
	}
	for _, key := range []string{"type", "controlSurface", "tool", "workspaceMode", "action", "cwd", "path"} {
		if _, present := values[key]; !present {
			return invalid
		}
	}
	if values["type"] != "workspace.projects" || values["controlSurface"] != "cli" || !policyID(values["tool"]) || values["workspaceMode"] != "code" && values["workspaceMode"] != "work" || !workspaceProjectRelativePath(values["path"]) {
		return invalid
	}
	action := values["action"]
	if action != "list" && action != "select" && action != "mkdir" || action != "list" && values["cursor"] != "" {
		return invalid
	}
	if values["cwd"] == "" && (action != "list" || values["path"] != "" || values["cursor"] != "") {
		return invalid
	}
	name, hasName := values["name"]
	if action == "mkdir" {
		if name == "" || strings.Contains(name, "/") || !workspaceProjectRelativePath(name) {
			return invalid
		}
	} else if hasName {
		return invalid
	}
	return nil
}

// Reject lexical traversal without cleaning or resolving paths on the Hub's
// operating system. Directory visibility and final-handle checks stay on the
// Bridge, whose filesystem may be on a different platform.
func workspaceProjectRelativePath(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > 1024 || strings.ContainsAny(value, "\\:*?\"<>|\x00") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || strings.HasPrefix(part, ".") || len(part) > 255 || strings.TrimSpace(part) != part || strings.HasSuffix(part, ".") {
			return false
		}
		for _, ch := range part {
			if unicode.IsControl(ch) || unicode.Is(unicode.Cf, ch) {
				return false
			}
		}
		device := strings.ToLower(strings.SplitN(part, ".", 2)[0])
		if device == "con" || device == "prn" || device == "aux" || device == "nul" || device == "conin$" || device == "conout$" || len(device) == 4 && (strings.HasPrefix(device, "com") || strings.HasPrefix(device, "lpt")) && device[3] >= '1' && device[3] <= '9' {
			return false
		}
	}
	return true
}
