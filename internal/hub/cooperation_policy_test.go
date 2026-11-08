package hub

import "testing"

func TestCooperationPreparationIsSourceProtectedTransientRead(t *testing.T) {
	const typ = "session.cooperation.prepare"
	if !sourceExecutionCommand(typ) || sourceReadOrSafetyCommand(typ) || !transientSourceRead(typ) {
		t.Fatal("cross-task preparation must prove the same execution source as send")
	}
}
