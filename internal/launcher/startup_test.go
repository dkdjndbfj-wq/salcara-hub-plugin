package launcher

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

type startupRunner struct {
	start func(context.Context, release) error
	stop  func() error
}

func (r startupRunner) Start(ctx context.Context, next release) error { return r.start(ctx, next) }
func (r startupRunner) Stop() error {
	if r.stop != nil {
		return r.stop()
	}
	return nil
}

func cachedFixture(t *testing.T) (*fixture, diskState) {
	t.Helper()
	f := newFixture(t)
	_, start := acceptedFixture(t, f)
	start(true)
	f.manager.job.Wait()
	state, err := f.store.load()
	if err != nil || state.Current.Bootstrap {
		t.Fatal("fixture did not confirm the signed cache", err)
	}
	return f, state
}

func TestNewImageSupersedesVerifiedCacheOnlyAfterHealthyStart(t *testing.T) {
	f, state := cachedFixture(t)
	f.store.bootstrapVersion = "0.5.0"
	before, err := os.ReadFile(filepath.Join(f.store.dir, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	candidate, changed, err := f.store.preferBootstrap(state)
	if err != nil || !changed || !candidate.Current.Bootstrap || candidate.Previous == nil || !bytes.Equal(candidate.Previous.Envelope, state.Current.Envelope) {
		t.Fatal("new image did not preserve verified cache as fallback", err)
	}
	after, _ := os.ReadFile(filepath.Join(f.store.dir, "state.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("image selection committed state before health acceptance")
	}
	starts := 0
	runner := startupRunner{start: func(_ context.Context, next release) error {
		starts++
		persisted, err := f.store.load()
		if err != nil || persisted.Current.Bootstrap || next.Version != "0.5.0" {
			t.Fatal("startup was not a verified uncommitted image candidate", err)
		}
		return nil
	}}
	confirmed, err := f.store.startConfirmed(context.Background(), state, runner)
	if err != nil || starts != 1 || !confirmed.Current.Bootstrap || confirmed.Previous == nil || confirmed.Previous.Bootstrap || confirmed.FailedBootstrap != nil {
		t.Fatal("healthy image was not committed with signed fallback", err)
	}
	restored, err := f.store.load()
	if err != nil || !restored.Current.Bootstrap || restored.Job == nil || restored.Job.Status != "current" {
		t.Fatal("confirmed image did not survive restart", err)
	}
	previous, err := f.store.resolve(*restored.Previous)
	if err != nil || previous.Version != "0.4.0" {
		t.Fatal("previous signed cache cannot be resolved", err)
	}
}

func TestImageDoesNotDowngradeEqualOrNewerVerifiedCache(t *testing.T) {
	for _, version := range []string{"0.3.0", "0.4.0-dev", "0.4.0"} {
		t.Run(version, func(t *testing.T) {
			f, state := cachedFixture(t)
			f.store.bootstrapVersion = version
			candidate, changed, err := f.store.preferBootstrap(state)
			if err != nil || changed || candidate.Current.Bootstrap || !bytes.Equal(candidate.Current.Envelope, state.Current.Envelope) {
				t.Fatal("old/equal image displaced confirmed signed cache", err)
			}
			runner := &fakeRunner{}
			if _, err = f.store.startConfirmed(context.Background(), state, runner); err != nil {
				t.Fatal(err)
			}
			_, starts := runner.counts()
			if len(starts) != 1 || starts[0] != "0.4.0" {
				t.Fatal("startup downgraded signed cache", starts)
			}
		})
	}
}

func TestUnhealthyImageFallsBackAndIsNotRetriedOnEveryRestart(t *testing.T) {
	f, state := cachedFixture(t)
	f.store.bootstrapVersion = "0.5.0"
	keep := filepath.Join(f.manager.cfg.DataDir, "pairing-image-fixture.json")
	original := []byte(`{"fixture":"pairing data preserved across image fallback"}`)
	if err := os.WriteFile(keep, original, 0600); err != nil {
		t.Fatal(err)
	}
	runner := &fakeRunner{failVersion: "0.5.0"}
	rolledBack, err := f.store.startConfirmed(context.Background(), state, runner)
	if err != nil || rolledBack.Current.Bootstrap || rolledBack.Previous == nil || !rolledBack.Previous.Bootstrap || rolledBack.FailedBootstrap == nil || rolledBack.Job.Status != "failed" {
		t.Fatal("unhealthy image did not persist signed fallback", err)
	}
	_, starts := runner.counts()
	if len(starts) != 2 || starts[0] != "0.5.0" || starts[1] != "0.4.0" {
		t.Fatal("unhealthy image did not return to verified cache", starts)
	}
	if raw, err := os.ReadFile(keep); err != nil || !bytes.Equal(raw, original) {
		t.Fatal("image update touched pairing data", err)
	}
	restored, err := f.store.load()
	if err != nil {
		t.Fatal(err)
	}
	restart := &fakeRunner{}
	if _, err = f.store.startConfirmed(context.Background(), restored, restart); err != nil {
		t.Fatal(err)
	}
	_, starts = restart.counts()
	if len(starts) != 1 || starts[0] != "0.4.0" {
		t.Fatal("container restart retried known unhealthy image", starts)
	}
	// A new image identity permits another attempt without weakening any cache
	// signature/hash checks. The real image binary is immutable to the Hub.
	if err = os.Chmod(f.store.bootstrapPath, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(f.store.bootstrapPath, []byte("replacement image fixture"), 0500); err != nil {
		t.Fatal(err)
	}
	if _, changed, err := f.store.preferBootstrap(restored); err != nil || !changed {
		t.Fatal("different image hash could not be retried", err)
	}
	if err = os.WriteFile(f.store.bootstrapPath, []byte("trusted image fixture"), 0500); err != nil {
		t.Fatal(err)
	}
	f.store.bootstrapVersion = "0.5.1"
	if _, changed, err := f.store.preferBootstrap(restored); err != nil || !changed {
		t.Fatal("different image version could not be retried", err)
	}
	successful := &fakeRunner{}
	confirmed, err := f.store.startConfirmed(context.Background(), restored, successful)
	if err != nil || !confirmed.Current.Bootstrap || confirmed.FailedBootstrap != nil {
		t.Fatal("successful replacement kept stale failure fingerprint", err)
	}
}

func TestImageCannotHideTamperedSignedCache(t *testing.T) {
	for _, kind := range []string{"binary", "signature", "schema", "ambiguous-reference"} {
		t.Run(kind, func(t *testing.T) {
			f, state := cachedFixture(t)
			f.store.bootstrapVersion = "0.5.0"
			switch kind {
			case "binary":
				current, err := f.store.resolve(state.Current)
				if err != nil {
					t.Fatal(err)
				}
				if err = os.Chmod(current.Path, 0700); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(current.Path, []byte("tampered cache"), 0500); err != nil {
					t.Fatal(err)
				}
			case "signature":
				state.Current.Envelope = []byte(`{"schema_version":1}`)
			case "schema":
				feed := testFeed(f.binary)
				feed.DataSchema = 2
				state.Current.Envelope = fixtureFeed(t, f.private, feed)
			case "ambiguous-reference":
				state.Current.Bootstrap = true
			}
			runner := &fakeRunner{}
			if _, err := f.store.startConfirmed(context.Background(), state, runner); err == nil {
				t.Fatal("new image bypassed invalid signed cache")
			}
			if _, starts := runner.counts(); len(starts) != 0 {
				t.Fatal("started an executable before reference validation", starts)
			}
		})
	}
}

func TestPreviouslyConfirmedImageFailureIsNotRetriedAfterCacheFallback(t *testing.T) {
	f, state := cachedFixture(t)
	f.store.bootstrapVersion = "0.5.0"
	confirmed, err := f.store.startConfirmed(context.Background(), state, &fakeRunner{})
	if err != nil || !confirmed.Current.Bootstrap {
		t.Fatal("fixture could not confirm image upgrade", err)
	}
	rolledBack, err := f.store.startConfirmed(context.Background(), confirmed, &fakeRunner{failVersion: "0.5.0"})
	if err != nil || rolledBack.Current.Bootstrap || rolledBack.FailedBootstrap == nil {
		t.Fatal("previously confirmed image did not remember its startup failure", err)
	}
	if _, changed, err := f.store.preferBootstrap(rolledBack); err != nil || changed {
		t.Fatal("same failed previously-confirmed image was selected again", err)
	}
}

func TestImageHealthFailureAndInterruptedStartupKeepLastConfirmedState(t *testing.T) {
	for _, kind := range []string{"both-unhealthy", "cancelled"} {
		t.Run(kind, func(t *testing.T) {
			f, state := cachedFixture(t)
			f.store.bootstrapVersion = "0.5.0"
			before, _ := os.ReadFile(filepath.Join(f.store.dir, "state.json"))
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			starts := 0
			runner := startupRunner{start: func(_ context.Context, _ release) error {
				starts++
				if kind == "cancelled" {
					cancel()
				}
				return errors.New("fixture startup failure")
			}}
			if _, err := f.store.startConfirmed(ctx, state, runner); err == nil {
				t.Fatal("failed startup was accepted")
			}
			after, _ := os.ReadFile(filepath.Join(f.store.dir, "state.json"))
			if !bytes.Equal(before, after) || (kind == "cancelled" && starts != 1) || (kind == "both-unhealthy" && starts != 2) {
				t.Fatal("failed/interrupted startup changed confirmation", starts)
			}
		})
	}
}

func TestImageCommitFailureStopsUncommittedProcess(t *testing.T) {
	f, state := cachedFixture(t)
	f.store.bootstrapVersion = "0.5.0"
	statePath := filepath.Join(f.store.dir, "state.json")
	// Simulate a state-path obstruction only after the health check completes.
	// No real data is deleted: this is an isolated synthetic test directory.
	stops := 0
	runner := startupRunner{start: func(_ context.Context, _ release) error {
		if err := os.Rename(statePath, statePath+".saved-fixture"); err != nil {
			t.Fatal(err)
		}
		if err := os.Mkdir(statePath, 0700); err != nil {
			t.Fatal(err)
		}
		return nil
	}, stop: func() error { stops++; return nil }}
	if _, err := f.store.startConfirmed(context.Background(), state, runner); err == nil || stops != 1 {
		t.Fatal("uncommitted healthy image kept running", err, stops)
	}
}

func TestCancelledHealthyStartupDoesNotCommitImage(t *testing.T) {
	f, state := cachedFixture(t)
	f.store.bootstrapVersion = "0.5.0"
	before, _ := os.ReadFile(filepath.Join(f.store.dir, "state.json"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stops := 0
	runner := startupRunner{start: func(_ context.Context, _ release) error { cancel(); return nil }, stop: func() error { stops++; return nil }}
	if _, err := f.store.startConfirmed(ctx, state, runner); !errors.Is(err, context.Canceled) || stops != 1 {
		t.Fatal("cancelled image was accepted or left running", err, stops)
	}
	after, _ := os.ReadFile(filepath.Join(f.store.dir, "state.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("cancelled healthy image changed last confirmation")
	}
}

func TestCancelledFallbackDoesNotRecordAnUnhealthyImage(t *testing.T) {
	f, state := cachedFixture(t)
	f.store.bootstrapVersion = "0.5.0"
	before, _ := os.ReadFile(filepath.Join(f.store.dir, "state.json"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	stops := 0
	runner := startupRunner{start: func(_ context.Context, next release) error {
		if next.Version == "0.5.0" {
			return errors.New("fixture unhealthy image")
		}
		cancel()
		return nil
	}, stop: func() error { stops++; return nil }}
	if _, err := f.store.startConfirmed(ctx, state, runner); !errors.Is(err, context.Canceled) || stops != 1 {
		t.Fatal("cancelled fallback was committed or left running", err, stops)
	}
	after, _ := os.ReadFile(filepath.Join(f.store.dir, "state.json"))
	if !bytes.Equal(before, after) {
		t.Fatal("cancelled fallback changed last confirmation or failure metadata")
	}
}

func TestFailedImageFingerprintIsBoundedDataNotExecutablePointer(t *testing.T) {
	f, state := cachedFixture(t)
	for _, failure := range []*bootstrapFailure{
		{Version: "../arbitrary", SHA256: strings.Repeat("a", 64)},
		{Version: "0.5.0", SHA256: "../../bin/sh"},
		{Version: "0.5.0", SHA256: strings.Repeat("a", 65)},
	} {
		state.FailedBootstrap = failure
		if err := f.store.save(state); err == nil {
			t.Fatal("saved malformed retry-suppression fingerprint")
		}
	}
	state.FailedBootstrap = &bootstrapFailure{Version: "0.5.0", SHA256: strings.Repeat("a", 64)}
	if err := f.store.save(state); err != nil {
		t.Fatal("valid bounded failure metadata could not be saved", err)
	}
	if _, err := f.store.load(); err != nil {
		t.Fatal("valid bounded failure metadata could not be restored", err)
	}
}
