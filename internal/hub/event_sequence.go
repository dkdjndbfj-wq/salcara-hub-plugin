package hub

import (
	"errors"
	"io"
	"os"
	"path/filepath"
	"strconv"
)

const (
	eventSequenceFile        = "event-sequence"
	eventSequenceBlock int64 = 1 << 20
	maxEventSequence   int64 = 1<<53 - 1 // exact in JavaScript
)

// initializeEventSequence runs before handlers/background workers are exposed.
// Only a numeric high-water reservation is persisted, never event content or
// credentials. A single Hub process must own the data volume (like devices.json).
func (h *Hub) initializeEventSequence() error {
	if h.cfg.DataDir != "" {
		path := filepath.Join(h.cfg.DataDir, eventSequenceFile)
		info, err := os.Lstat(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return errors.New("cannot inspect event sequence reservation")
		}
		if err == nil {
			if !info.Mode().IsRegular() || info.Size() < 1 || info.Size() > 32 {
				return errors.New("event sequence reservation must be a bounded regular file")
			}
			f, err := os.Open(path)
			if err != nil {
				return errors.New("cannot open event sequence reservation")
			}
			opened, statErr := f.Stat()
			raw, readErr := io.ReadAll(io.LimitReader(f, 33))
			f.Close()
			if statErr != nil || !os.SameFile(info, opened) || readErr != nil || len(raw) > 32 {
				return errors.New("cannot read event sequence reservation")
			}
			reserved, err := strconv.ParseInt(string(raw), 10, 64)
			if err != nil || reserved < 1 || reserved > maxEventSequence || strconv.FormatInt(reserved, 10) != string(raw) {
				return errors.New("invalid event sequence reservation")
			}
			if reserved > h.seqBase {
				h.seqBase = reserved
			}
		}
	}
	if h.seqBase < 0 || h.seqBase > maxEventSequence-eventSequenceBlock {
		return errors.New("event sequence range exhausted")
	}
	if err := h.reserveEventSequenceLocked(h.seqBase + 1); err != nil {
		return err
	}
	for _, account := range h.accounts {
		account.seq = h.seqBase
		account.sessionEvictedThrough = h.seqBase
	}
	return nil
}

// Caller holds h.mu, except during single-threaded initialization. A block is
// durable before it can be used, so even an ungraceful exit cannot reuse a
// published cursor. Fsync is amortized over a million events, not per message.
func (h *Hub) reserveEventSequenceLocked(required int64) error {
	if required < 0 || required > maxEventSequence {
		return errors.New("event sequence range exhausted")
	}
	if required <= h.seqCeiling {
		return nil
	}
	if h.cfg.DataDir == "" {
		h.seqCeiling = maxEventSequence // explicitly ephemeral deployment
		return nil
	}
	base := h.seqCeiling
	if base < h.seqBase {
		base = h.seqBase
	}
	blocks := (required-base-1)/eventSequenceBlock + 1
	if blocks <= 0 || blocks > (maxEventSequence-base)/eventSequenceBlock {
		return errors.New("event sequence range exhausted")
	}
	ceiling := base + blocks*eventSequenceBlock
	if err := writeFileAtomic(filepath.Join(h.cfg.DataDir, eventSequenceFile), []byte(strconv.FormatInt(ceiling, 10)), 0600); err != nil {
		return errors.New("cannot persist event sequence reservation")
	}
	h.seqCeiling = ceiling
	return nil
}
