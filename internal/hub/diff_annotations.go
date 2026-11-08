package hub

import (
	"encoding/json"
	"errors"
	"regexp"
	"strings"
	"unicode"
	"unicode/utf8"
)

var diffSessionKey = regexp.MustCompile(`(?i)^[a-z][a-z0-9_-]*:[^|]+$`)
var diffRevision = regexp.MustCompile(`^[a-f0-9]{64}$`)

// Existing reference kinds remain compatible. Diff references are a new,
// explicitly negotiated wire contract, validated before grant issuance and
// forwarding. canonicalCommand already signs every field, including snapshots.
func validateDiffAnnotations(meta map[string]json.RawMessage) error {
	var refs []json.RawMessage
	if json.Unmarshal(meta["contextReferences"], &refs) != nil {
		return nil
	}
	invalid := errors.New("context_reference_invalid: invalid diff annotation")
	hasDiff, total := false, 0
	allowed := []string{"kind", "sessionKey", "label", "text", "capturedAt", "path", "startLine", "endLine", "side", "sourceRevision", "annotation"}
	for _, raw := range refs {
		var probe struct{ Kind string }
		if json.Unmarshal(raw, &probe) != nil {
			continue
		}
		var snapshot struct{ Text, Annotation string }
		_ = json.Unmarshal(raw, &snapshot)
		total += len(snapshot.Text) + len(snapshot.Annotation)
		if probe.Kind != "diff" {
			continue
		}
		hasDiff = true
		fields, err := strictObjectFields(raw, allowed)
		if err != nil {
			return invalid
		}
		for name, value := range fields {
			found := false
			for _, key := range allowed {
				if key == name {
					found = true
					break
				}
			}
			if !found || string(value) == "null" {
				return invalid
			}
		}
		for _, name := range allowed[:len(allowed)-1] {
			if _, present := fields[name]; !present {
				return invalid
			}
		}
		var ref struct {
			Kind, SessionKey, Label, Text, Path, Side, SourceRevision string
			StartLine, EndLine, CapturedAt                            int64
			Annotation                                                *string
		}
		if json.Unmarshal(raw, &ref) != nil {
			return invalid
		}
		if !diffSafeLabel(ref.SessionKey, 256) || !diffSessionKey.MatchString(ref.SessionKey) || !diffSafeLabel(ref.Label, 200) ||
			!diffSafeLabel(ref.Path, 1024) || strings.ContainsAny(ref.Path, "\\:*?\"<>|") || (ref.Side != "old" && ref.Side != "new") || !diffRevision.MatchString(ref.SourceRevision) ||
			ref.CapturedAt < 1 || ref.CapturedAt > 9007199254740991 || ref.StartLine < 1 || ref.EndLine < ref.StartLine || ref.EndLine > 9007199254740991 ||
			ref.EndLine-ref.StartLine+1 != int64(strings.Count(ref.Text, "\n")+1) || ref.Text == "" || !diffSnapshot(ref.Text, 32<<10) ||
			(ref.Annotation != nil && !diffSnapshot(*ref.Annotation, 8<<10)) {
			return invalid
		}
		for _, segment := range strings.Split(ref.Path, "/") {
			if segment == "" || segment == "." || segment == ".." || strings.TrimSpace(segment) != segment {
				return invalid
			}
		}
	}
	if hasDiff {
		if len(refs) > 8 || total > 64<<10 {
			return invalid
		}
		typ := commandString(meta, "type")
		if typ != "session.start" && typ != "session.send" && typ != "session.continue" {
			return invalid
		}
		if _, present := meta["controlSurface"]; present && commandString(meta, "controlSurface") != "cli" {
			return invalid
		}
		for _, name := range []string{"text", "prompt"} {
			if len(commandString(meta, name)) > 64<<10 {
				return invalid
			}
		}
	}
	return nil
}

func diffSafeLabel(text string, max int) bool {
	return text != "" && len(text) <= max && utf8.ValidString(text) && strings.IndexFunc(text, func(r rune) bool { return unicode.IsControl(r) || unicode.Is(unicode.Cf, r) }) < 0
}
func diffSnapshot(text string, max int) bool {
	return len(text) <= max && utf8.ValidString(text) && strings.IndexFunc(text, func(r rune) bool { return r < 32 && r != '\t' && r != '\n' && r != '\r' || r == 127 }) < 0
}
