package hub

// Keep these wire budgets aligned with the Bridge protocol/source_limits.go.
// At most 32 distinct 4096-byte bases may occur twice (API and proxy), and
// encoding/json expands a legal '&' in a path to six bytes (\u0026).
const (
	MaxStationPolicyJSONBytes = 2 << 20
	MaxSourcePolicyWireBytes  = 4 << 20
	maxSourceIssuerJSONBytes  = 256 << 10
	MaxSourceSnapshotBytes    = 64 << 10
	MaxSourcePayloadBytes     = (MaxSourceSnapshotBytes*4 + 2) / 3
	MaxSignedSourceGrantBytes = 96 << 10
)
