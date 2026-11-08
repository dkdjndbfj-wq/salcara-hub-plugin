package edgepool

import (
	"context"
	"errors"
	"net"
	"net/netip"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

type fixtureGeoDB struct{ kind string }

func (d fixtureGeoDB) Lookup(ip net.IP, out any) error {
	v := reflect.ValueOf(out).Elem()
	country, asn := "HK", uint64(13335)
	if ip.String() == "8.8.8.8" {
		country, asn = "US", 15169
	}
	if d.kind == "country" {
		v.FieldByName("Country").FieldByName("Code").SetString(country)
	} else {
		v.FieldByName("Number").SetUint(asn)
		v.FieldByName("Name").SetString("Example network")
	}
	return nil
}
func fixtureGeo(p *Pool) *geoManager {
	m := p.geo
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data = &geoDataset{country: fixtureGeoDB{"country"}, asn: fixtureGeoDB{"asn"}, built: time.Now()}
	m.saved.Enabled = true
	m.saved.NextObserveAt = time.Now().Add(time.Hour).UnixMilli()
	m.open = func(raw []byte, kind string) (geoDatabase, time.Time, error) {
		if string(raw) == "invalid" {
			return nil, time.Time{}, errors.New("geo_database_invalid")
		}
		return fixtureGeoDB{kind}, time.Now(), nil
	}
	m.fetch = func(context.Context, string) ([]byte, error) { return []byte("fixture database"), nil }
	m.resolve = func(context.Context, string, string) ([]netip.Addr, error) {
		return []netip.Addr{netip.MustParseAddr("1.1.1.1")}, nil
	}
	return m
}
func waitGeo(t *testing.T, m *geoManager) GeoState {
	t.Helper()
	end := time.Now().Add(5 * time.Second)
	for time.Now().Before(end) {
		s := m.view()
		if s.Run != nil && !s.Run.Running {
			return s
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("geo job did not finish")
	return GeoState{}
}
func TestGeoObservationSelectionFreshnessAndIdentity(t *testing.T) {
	p := testPool(t, "https://hub.example")
	id := addNode(t, p, "proxy", "socks5://1.1.1.1:1080")
	m := fixtureGeo(p)
	if err := m.start("observe", m.view().Revision); err != nil {
		t.Fatal(err)
	}
	s := waitGeo(t, m)
	if s.LastError != "" || len(s.Groups) != 1 {
		t.Fatalf("state: %+v", s)
	}
	p.mu.Lock()
	n := p.saved.Nodes[len(p.saved.Nodes)-1]
	policy := p.saved.SelectionPolicy
	p.mu.Unlock()
	if got := p.regionFor(n, policy); got != "" {
		t.Fatal("observation must be opt in", got)
	}
	policy.UseObservedRegion = true
	if got := p.regionFor(n, policy); got != "HK" {
		t.Fatal(got)
	}
	n.Region = "Singapore"
	if p.regionFor(n, policy) != "Singapore" {
		t.Fatal("manual region precedence")
	}
	n.Region = ""
	m.mu.Lock()
	v := m.saved.Nodes[id]
	v.ExpiresAt = time.Now().Add(-time.Second).UnixMilli()
	m.saved.Nodes[id] = v
	m.mu.Unlock()
	if p.regionFor(n, policy) != "" || m.info(n).Status != "expired" {
		t.Fatal("expired data used for selection")
	}
	m.mu.Lock()
	v.ExpiresAt = time.Now().Add(time.Hour).UnixMilli()
	m.saved.Nodes[id] = v
	m.mu.Unlock()
	p.mu.Lock()
	p.saved.Nodes[len(p.saved.Nodes)-1].ProxyURL = "socks5://8.8.8.8:1080"
	n = p.saved.Nodes[len(p.saved.Nodes)-1]
	p.mu.Unlock()
	if m.info(n).Status != "not_observed" || len(m.view().Groups) != 0 {
		t.Fatal("replaced node retained old identity")
	}
}
func TestGeoMixedPrivateAndFailedDNS(t *testing.T) {
	for _, tc := range []struct {
		name   string
		ips    []string
		err    error
		status string
	}{
		{"mixed", []string{"1.1.1.1", "8.8.8.8"}, nil, "mixed_networks"},
		{"private", []string{"1.1.1.1", "127.0.0.1"}, nil, "direct_or_external"},
		{"failed", nil, errors.New("private error"), "dns_failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			p := testPool(t, "https://hub.example")
			addNode(t, p, "proxy", "socks5://proxy.example:1080")
			m := fixtureGeo(p)
			m.resolve = func(context.Context, string, string) ([]netip.Addr, error) {
				var ips []netip.Addr
				for _, raw := range tc.ips {
					ips = append(ips, netip.MustParseAddr(raw))
				}
				return ips, tc.err
			}
			if err := m.start("observe", 1); err != nil {
				t.Fatal(err)
			}
			waitGeo(t, m)
			p.mu.Lock()
			n := p.saved.Nodes[len(p.saved.Nodes)-1]
			p.mu.Unlock()
			info := m.info(n)
			if info.Status != tc.status || info.Country != "" || info.ASN != 0 {
				t.Fatalf("misleading network identity: %+v", info)
			}
		})
	}
}
func TestGeoInstallIsAtomicAndPreservesDisabledChoice(t *testing.T) {
	p := testPool(t, "https://hub.example")
	m := fixtureGeo(p)
	if err := m.start("install", 1); err != nil {
		t.Fatal(err)
	}
	s := waitGeo(t, m)
	if !s.Enabled || s.LastError != "" {
		t.Fatalf("first installation: %+v", s)
	}
	if err := m.configure(s.Revision, false, true); err != nil {
		t.Fatal(err)
	}
	if err := m.start("install", m.view().Revision); err != nil {
		t.Fatal(err)
	}
	s = waitGeo(t, m)
	if s.Enabled {
		t.Fatal("update re-enabled disabled observation")
	}
	m.mu.Lock()
	oldGen := m.saved.Generation
	oldData := m.data
	m.fetch = func(context.Context, string) ([]byte, error) { return []byte("invalid"), nil }
	m.mu.Unlock()
	if err := m.start("install", s.Revision); err != nil {
		t.Fatal(err)
	}
	s = waitGeo(t, m)
	m.mu.Lock()
	unchanged := m.saved.Generation == oldGen && m.data == oldData
	m.mu.Unlock()
	if s.LastError != "geo_database_invalid" || !unchanged {
		t.Fatal("failed candidate replaced good dataset", s)
	}
	entries, err := os.ReadDir(m.dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), "generation-") && e.Name() != "generation-"+oldGen {
			t.Fatal("orphan candidate", e.Name())
		}
	}
	if err := m.configure(1, true, true); err == nil || err.Error() != "revision_conflict" {
		t.Fatal("missing CAS", err)
	}
}
func TestGeoProbeBudgetAndCloseCancellation(t *testing.T) {
	p := testPool(t, "https://hub.example")
	for i := 0; i < 8; i++ {
		addNode(t, p, "proxy", "socks5://proxy.example:1080")
	}
	m := fixtureGeo(p)
	var active, peak atomic.Int32
	m.resolve = func(ctx context.Context, _, _ string) ([]netip.Addr, error) {
		n := active.Add(1)
		defer active.Add(-1)
		for old := peak.Load(); n > old && !peak.CompareAndSwap(old, n); old = peak.Load() {
		}
		<-ctx.Done()
		return nil, ctx.Err()
	}
	releases := []func(){}
	for i := 0; i < 3; i++ {
		release, ok := p.acquireProbeSlot(context.Background())
		if !ok {
			t.Fatal("slot")
		}
		releases = append(releases, release)
	}
	if err := m.start("observe", 1); err != nil {
		t.Fatal(err)
	}
	end := time.Now().Add(time.Second)
	for peak.Load() == 0 && time.Now().Before(end) {
		time.Sleep(time.Millisecond)
	}
	if err := m.configure(1, false, false); err == nil || err.Error() != "geo_busy" {
		t.Fatal("mutation during job", err)
	}
	p.Close()
	for _, release := range releases {
		release()
	}
	if peak.Load() != 1 {
		t.Fatalf("shared budget exceeded: %d", peak.Load())
	}
	if len(m.saved.Nodes) != 0 {
		t.Fatal("cancellation replaced observations")
	}
	if err := m.start("observe", m.view().Revision); err == nil {
		t.Fatal("started after close")
	}
}
func TestGeoInvalidStateDegradesWithoutBreakingPool(t *testing.T) {
	p := testPool(t, "https://hub.example")
	dir := p.cfg.DataDir
	p.Close()
	if err := writeSourcePrivate(filepath.Join(dir, "geodata", "state.json"), []byte(`{"revision":1,"generation":"../../escape"}`)); err != nil {
		t.Fatal(err)
	}
	next, err := New(Config{Origin: "https://hub.example", DataDir: dir, ControlToken: testToken, DisablePeriodicProbe: true})
	if err != nil {
		t.Fatal(err)
	}
	defer next.Close()
	if next.geo.view().LastError != "geo_state_invalid" || !next.State().GeoAvailable {
		t.Fatal("geo corruption did not fail independently")
	}
}
func TestGeoOfficialDatabaseOptIn(t *testing.T) {
	if os.Getenv("SALCARA_TEST_DBIP") != "1" {
		t.Skip("official monthly database download requires explicit opt-in")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()
	d := &geoDataset{}
	for _, kind := range []string{"country", "asn"} {
		raw, err := fetchGeoDatabase(ctx, "https://download.db-ip.com/free/dbip-"+kind+"-lite-"+time.Now().UTC().Format("2006-01")+".mmdb.gz")
		if err != nil {
			t.Fatal(kind, err)
		}
		db, built, err := openGeoDatabase(raw, kind)
		if err != nil {
			t.Fatal(kind, err)
		}
		t.Log(kind, "bytes", len(raw), "built", built)
		if kind == "country" {
			d.country = db
		} else {
			d.asn = db
		}
	}
	country, asn, org, err := d.lookup(netip.MustParseAddr("8.8.8.8"))
	if err != nil || country == "" || asn != 15169 || org == "" {
		t.Fatal(country, asn, org, err)
	}
}

func TestGeoPolicySurvivesReleaseSnapshotAndClone(t *testing.T) {
	policy := defaultSelectionPolicy()
	policy.UseObservedRegion = true
	policy.RegionMode = "prefer"
	policy.PreferredRegions = []string{"HK"}
	original := savedConfig{Revision: 4, SelectionPolicy: policy}
	for _, restored := range []savedConfig{cloneConfig(original), snapshotOf(original).config(5)} {
		if !restored.SelectionPolicy.UseObservedRegion || restored.SelectionPolicy.PreferredRegions[0] != "HK" {
			t.Fatal("release lost observed-region policy")
		}
		restored.SelectionPolicy.PreferredRegions[0] = "US"
		if original.SelectionPolicy.PreferredRegions[0] != "HK" {
			t.Fatal("snapshot aliases live policy")
		}
	}
}

func TestGeoDatabaseUpdateRetainsEvidenceUntilItsOriginalExpiry(t *testing.T) {
	p := testPool(t, "https://hub.example")
	id := addNode(t, p, "proxy", "socks5://1.1.1.1:1080")
	m := fixtureGeo(p)
	if err := m.start("observe", 1); err != nil {
		t.Fatal(err)
	}
	s := waitGeo(t, m)
	m.mu.Lock()
	before := m.saved.Nodes[id]
	m.mu.Unlock()
	if err := m.start("install", s.Revision); err != nil {
		t.Fatal(err)
	}
	s = waitGeo(t, m)
	if s.LastError != "" {
		t.Fatal(s.LastError)
	}
	m.mu.Lock()
	after := m.saved.Nodes[id]
	m.mu.Unlock()
	if before.ExpiresAt != after.ExpiresAt || after.Country != "HK" {
		t.Fatal("database update lost or extended evidence")
	}
	policy := defaultSelectionPolicy()
	policy.UseObservedRegion = true
	policy.RegionMode = "only"
	policy.AllowUnknown = false
	policy.PreferredRegions = []string{"HK"}
	p.mu.Lock()
	n := p.saved.Nodes[len(p.saved.Nodes)-1]
	p.mu.Unlock()
	if p.regionFor(n, policy) != "HK" {
		t.Fatal("update stranded strict region routing")
	}
}

func TestGeoExplicitInstallationRetriesAfterFailureAndRestart(t *testing.T) {
	p := testPool(t, "https://hub.example")
	m := fixtureGeo(p)
	m.fetch = func(context.Context, string) ([]byte, error) { return nil, errors.New("geo_download_failed") }
	if err := m.start("install", 1); err != nil {
		t.Fatal(err)
	}
	s := waitGeo(t, m)
	if !s.UpdatePending || s.NextUpdateAt <= time.Now().UnixMilli() {
		t.Fatal("failed install lost retry")
	}
	reloaded := newGeoManager(p)
	reloaded.mu.Lock()
	saved := reloaded.saved
	reloaded.mu.Unlock()
	if geoOperationDue(saved, time.Now()) != "" || geoOperationDue(saved, time.Now().Add(2*time.Hour)) != "install" {
		t.Fatal("retry schedule lost across restart")
	}
	saved.Generation = strings.Repeat("a", 32)
	saved.Month = time.Now().UTC().Format("2006-01")
	if geoOperationDue(saved, time.Now().Add(2*time.Hour)) != "install" {
		t.Fatal("current month manual update not retried")
	}
	saved.AutoUpdate = false
	if geoOperationDue(saved, time.Now().Add(2*time.Hour)) == "install" {
		t.Fatal("disabled automatic update retried")
	}
}

func TestGeoDisableRequiresStrictRegionPolicyChange(t *testing.T) {
	p := testPool(t, "https://hub.example")
	m := fixtureGeo(p)
	p.mu.Lock()
	p.saved.SelectionPolicy.UseObservedRegion = true
	p.saved.SelectionPolicy.RegionMode = "only"
	p.saved.SelectionPolicy.PreferredRegions = []string{"HK"}
	p.mu.Unlock()
	if err := m.configure(1, false, true); err == nil || err.Error() != "geo_policy_dependency" {
		t.Fatal("strict-region shutdown not fenced", err)
	}
	if !m.view().Enabled {
		t.Fatal("rejected change disabled observations")
	}
	p.mu.Lock()
	p.saved.SelectionPolicy.UseObservedRegion = false
	p.mu.Unlock()
	if err := m.configure(1, false, true); err != nil {
		t.Fatal(err)
	}
}
