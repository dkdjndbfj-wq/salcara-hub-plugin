package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

// This field does not grant execution authority. Source authorization and
// canonical command hashing continue to apply to the complete start payload.
func validateProjectlessCommand(meta map[string]json.RawMessage) error {
	flag, present := meta["projectless"]
	if !present {
		// Preserve legacy parsing of absent fields; the Bridge still requires
		// an authorized cwd. An explicit empty cwd cannot mean projectless.
		if commandString(meta, "type") == "session.start" && bytes.Equal(bytes.TrimSpace(meta["cwd"]), []byte(`""`)) {
			return errors.New("empty cwd requires explicit projectless start")
		}
		return nil
	}
	invalid := errors.New("invalid projectless start command")
	if !bytes.Equal(bytes.TrimSpace(flag), []byte("true")) || commandString(meta, "type") != "session.start" {
		return invalid
	}
	for key := range meta {
		switch key {
		case "type", "tool", "cwd", "projectless", "prompt", "controlSurface", "workspaceMode", "model", "effort", "approval", "permissionMode", "collaborationMode", "skills", "contextReferences", "accountId", "attachments", "operationId":
		default:
			return invalid
		}
	}
	var kind, tool, cwd, surface, mode, operation string
	if json.Unmarshal(meta["type"], &kind) != nil || kind != "session.start" ||
		json.Unmarshal(meta["tool"], &tool) != nil || tool != strings.TrimSpace(tool) || !policyID(tool) ||
		json.Unmarshal(meta["cwd"], &cwd) != nil || !bytes.Equal(bytes.TrimSpace(meta["cwd"]), []byte(`""`)) || cwd != "" ||
		json.Unmarshal(meta["controlSurface"], &surface) != nil || surface != "cli" ||
		json.Unmarshal(meta["workspaceMode"], &mode) != nil || mode != "code" && mode != "work" ||
		json.Unmarshal(meta["operationId"], &operation) != nil || !requestUUID.MatchString(operation) {
		return invalid
	}
	return nil
}

var explorerCursor = regexp.MustCompile(`^[A-Za-z0-9_-]{1,512}$`)

// Explorer returns directory metadata for the paired computer. Browsing a
// drive is not a grant to an Agent; the Bridge verifies and records only the
// exact newly created directory. Never resolve remote paths on the Hub host.
func validateWorkspaceExplorerCommand(meta map[string]json.RawMessage) error {
	if commandString(meta, "type") != "workspace.explorer" {
		return nil
	}
	invalid := errors.New("invalid workspace explorer command")
	values := make(map[string]string, len(meta))
	for key, raw := range meta {
		switch key {
		case "type", "controlSurface", "tool", "workspaceMode", "action", "cwd", "path", "cursor", "name":
		default:
			return invalid
		}
		var value string
		if bytes.Equal(bytes.TrimSpace(raw), []byte("null")) || json.Unmarshal(raw, &value) != nil || !utf8.ValidString(value) || len(value) > 4096 || strings.IndexFunc(value, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) >= 0 {
			return invalid
		}
		values[key] = value
	}
	for _, key := range []string{"type", "controlSurface", "tool", "workspaceMode", "action", "cwd", "path"} {
		if _, exists := values[key]; !exists {
			return invalid
		}
	}
	if values["type"] != "workspace.explorer" || values["controlSurface"] != "cli" || !policyID(values["tool"]) || values["workspaceMode"] != "code" && values["workspaceMode"] != "work" || !explorerRelativePath(values["path"]) {
		return invalid
	}
	action, cwd := values["action"], values["cwd"]
	_, hasCursor := values["cursor"]
	_, hasName := values["name"]
	if action != "list" && action != "mkdir" || cwd == "" && (action != "list" || values["path"] != "" || hasCursor) {
		return invalid
	}
	if action == "mkdir" {
		if hasCursor || !hasName || values["name"] == "" || strings.Contains(values["name"], "/") || !explorerRelativePath(values["name"]) {
			return invalid
		}
	} else if hasName || hasCursor && !explorerCursor.MatchString(values["cursor"]) {
		return invalid
	}
	return nil
}

func explorerRelativePath(value string) bool {
	if value == "" {
		return true
	}
	if len(value) > 4096 || strings.ContainsAny(value, "\\:*?\"<>|\x00") {
		return false
	}
	for _, part := range strings.Split(value, "/") {
		if part == "" || part == "." || part == ".." || len(part) > 255 || strings.TrimSpace(part) != part || strings.HasSuffix(part, ".") {
			return false
		}
		for _, r := range part {
			if unicode.IsControl(r) || unicode.Is(unicode.Cf, r) {
				return false
			}
		}
		name := strings.ToLower(strings.SplitN(part, ".", 2)[0])
		if name == "con" || name == "prn" || name == "aux" || name == "nul" || name == "conin$" || name == "conout$" || len(name) == 4 && (strings.HasPrefix(name, "com") || strings.HasPrefix(name, "lpt")) && name[3] >= '1' && name[3] <= '9' {
			return false
		}
	}
	return true
}
