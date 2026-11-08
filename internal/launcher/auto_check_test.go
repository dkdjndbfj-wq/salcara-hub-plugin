package launcher

import (
	"net/http"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func startFixtureChecks(t *testing.T, f *fixture) <-chan struct{} {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f.manager.runAutoChecks()
	}()
	t.Cleanup(func() { f.cancel(); <-done })
	return done
}

func TestAutoChecksStartImmediatelyRepeatAndNeverInstall(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		done := startFixtureChecks(t, f)
		synctest.Wait()
		s := f.manager.snapshot()
		if f.requests.Load() != 1 || s.Status != "available" || s.Checking || !s.AutoCheck || s.CheckIntervalSeconds != 21600 {
			t.Fatal("startup did not check exactly once", f.requests.Load(), s)
		}
		checked, err := time.Parse(time.RFC3339, s.CheckedAt)
		if err != nil {
			t.Fatal("missing completion time", s)
		}
		next, err := time.Parse(time.RFC3339, s.NextCheckAt)
		if err != nil || next.Sub(checked) != defaultCheckInterval {
			t.Fatal("incorrect next automatic check", s)
		}
		time.Sleep(defaultCheckInterval - time.Second)
		synctest.Wait()
		if f.requests.Load() != 1 {
			t.Fatal("automatic check ran early", f.requests.Load())
		}
		time.Sleep(time.Second)
		synctest.Wait()
		if f.requests.Load() != 2 || f.manager.snapshot().CheckedAt == s.CheckedAt {
			t.Fatal("periodic check did not refresh the result")
		}
		if stops, starts := f.runner.counts(); stops != 0 || len(starts) != 0 {
			t.Fatal("automatic detection installed or restarted an application", stops, starts)
		}
		f.cancel()
		<-done
		time.Sleep(defaultCheckInterval)
		if f.requests.Load() != 2 {
			t.Fatal("checks continued after cancellation")
		}
	})
}

func TestManualCheckPostponesAutomaticCheck(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		startFixtureChecks(t, f)
		synctest.Wait()
		time.Sleep(3 * time.Hour)
		manual := f.manager.check()
		if f.requests.Load() != 2 || manual.Checking {
			t.Fatal("manual check failed", manual)
		}
		time.Sleep(3 * time.Hour)
		synctest.Wait()
		if f.requests.Load() != 2 {
			t.Fatal("stale automatic timer ignored the manual result")
		}
		time.Sleep(3 * time.Hour)
		synctest.Wait()
		if f.requests.Load() != 3 {
			t.Fatal("automatic checks did not resume after the manual check")
		}
	})
}

func TestDisabledAutoChecksNeverAccessNetwork(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		cfg := f.manager.cfg
		cfg.Disabled = true
		m := newManager(f.manager.ctx, cfg, f.store, f.manager.state, f.runner)
		m.client = f.manager.client
		m.runAutoChecks()
		s := m.check()
		if f.requests.Load() != 0 || s.Configured || s.AutoCheck || s.Checking || s.CheckedAt != "" || s.NextCheckAt != "" || s.CheckIntervalSeconds != 0 {
			t.Fatal("disabled updates still scheduled or performed checks", s)
		}
	})
}

func TestFailedAutoCheckRetriesWithoutStoppingHub(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		validFeed := f.feed
		f.feed = []byte(`{"schema_version":1}`)
		startFixtureChecks(t, f)
		synctest.Wait()
		s := f.manager.snapshot()
		if s.Status != "unavailable" || s.CheckedAt == "" || s.NextCheckAt == "" || s.LatestVersion != "" || s.SHA256 != "" {
			t.Fatal("failed signature check advertised an installable version", s)
		}
		f.manager.operation.Lock()
		f.feed = validFeed
		f.manager.operation.Unlock()
		time.Sleep(defaultCheckInterval)
		synctest.Wait()
		if f.requests.Load() != 2 || f.manager.snapshot().Status != "available" {
			t.Fatal("automatic check did not recover on its next attempt")
		}
		if stops, starts := f.runner.counts(); stops != 0 || len(starts) != 0 {
			t.Fatal("failed detection affected the running Hub")
		}
	})
}

func TestAutoChecksDoNotRaceAnAcceptedUpdate(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		_, start := acceptedFixture(t, f)
		defer start(false)
		startFixtureChecks(t, f)
		time.Sleep(defaultCheckInterval)
		synctest.Wait()
		if f.requests.Load() != 1 || f.manager.snapshot().Status != "updating" {
			t.Fatal("automatic check overwrote or ran during the accepted update")
		}
		start(false)
		f.manager.job.Wait()
		time.Sleep(time.Minute)
		synctest.Wait()
		if f.requests.Load() != 2 || f.manager.snapshot().Status != "available" {
			t.Fatal("automatic checks did not resume after the operation lock was released")
		}
	})
}

func TestCheckingStateSerializesRequestsAndCancelsOnShutdown(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newFixture(t)
		requests := 0
		f.manager.client = &http.Client{Transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			requests++
			<-r.Context().Done()
			return nil, r.Context().Err()
		})}
		done := startFixtureChecks(t, f)
		synctest.Wait()
		s := f.manager.snapshot()
		if !s.Checking || s.CheckedAt != "" || s.NextCheckAt != "" || requests != 1 {
			t.Fatal("pending check is not visible", s)
		}
		if !f.manager.check().Checking || requests != 1 {
			t.Fatal("concurrent manual check started a duplicate request")
		}
		_, code, start := f.manager.apply(applyRequest{Confirm: true})
		if code != http.StatusConflict || start != nil {
			t.Fatal("installation raced the active check")
		}
		f.cancel()
		<-done
		if f.manager.snapshot().Checking || f.manager.snapshot().NextCheckAt != "" {
			t.Fatal("cancelled check left a running task or future schedule")
		}
		f.manager.check()
		if requests != 1 {
			t.Fatal("cancelled manager made another request")
		}
	})
}

func TestUpdateCheckIntervalConfiguration(t *testing.T) {
	for value, want := range map[string]time.Duration{"": 6 * time.Hour, "5m": 5 * time.Minute, "12h": 12 * time.Hour, "168h": 7 * 24 * time.Hour} {
		got, err := updateCheckInterval(value)
		if err != nil || got != want {
			t.Fatalf("interval %q: got %v, %v", value, got, err)
		}
	}
	for _, value := range []string{"0", "-1h", "1m", "169h", "never", "10"} {
		_, err := ConfigFromEnv(func(key string) string {
			if key == "SALCARA_HUB_UPDATE_CHECK_INTERVAL" {
				return value
			}
			return ""
		}, "0.5.2")
		if err == nil || !strings.Contains(err.Error(), "SALCARA_HUB_UPDATE_CHECK_INTERVAL") {
			t.Fatalf("invalid interval %q accepted: %v", value, err)
		}
	}
}
