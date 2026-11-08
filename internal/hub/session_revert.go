package hub

import (
	"encoding/json"
	"errors"
	"strings"
	"unicode"
)

// The Bridge resolves the actual latest user turn and serializes its mutation.
// The Hub admits only an explicit, source-authorized Codex CLI identity; the
// phone cannot select an earlier-turn count, file rollback, or fallback route.
func validateSessionRevertCommand(meta map[string]json.RawMessage) error {
	if commandString(meta, "type") != "session.revert" {
		return nil
	}
	allowed := map[string]bool{"type": true, "sessionKey": true, "expectedTurnId": true, "expectedMessageId": true,
		"operationId": true, "accountId": true, "controlSurface": true, "tool": true, "agent": true}
	for key := range meta {
		if !allowed[key] {
			return errors.New("unknown session revert field")
		}
	}
	read := func(key string, limit int) (string, bool) {
		var value string
		if json.Unmarshal(meta[key], &value) != nil || value == "" || len(value) > limit || strings.TrimSpace(value) != value || strings.IndexFunc(value, unicode.IsControl) >= 0 {
			return "", false
		}
		return value, true
	}
	typ, validType := read("type", 32)
	surface, validSurface := read("controlSurface", 16)
	session, validSession := read("sessionKey", 512)
	_, validTurn := read("expectedTurnId", 512)
	_, validMessage := read("expectedMessageId", 512)
	operation, validOperation := read("operationId", 36)
	if !validType || typ != "session.revert" || !validSurface || surface != "cli" || !validSession || !strings.HasPrefix(session, "codex:") || strings.TrimSpace(strings.TrimPrefix(session, "codex:")) == "" || !validTurn || !validMessage || !validOperation || !requestUUID.MatchString(operation) {
		return errors.New("session revert requires an exact Codex CLI turn and operation")
	}
	for _, key := range []string{"accountId", "tool", "agent"} {
		if _, exists := meta[key]; !exists {
			continue
		}
		value, ok := read(key, 128)
		if !ok || key != "accountId" && value != "codex" {
			return errors.New("invalid session revert source identity")
		}
	}
	return nil
}
