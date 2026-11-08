package edgepool

import (
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/oschwald/maxminddb-golang"
)

const geoProvider = "DB-IP Lite (CC BY 4.0)"
const geoTTL = 24 * time.Hour
const geoDatabaseAge = 45 * 24 * time.Hour

// This describes the proxy SERVER addresses, not a measured exit location or
// the user's network. No client IP is sent to a third-party lookup service.
type NetworkInfo struct {
	Status       string   `json:"status"`
	Country      string   `json:"country"`
	ASN          uint     `json:"asn"`
	Organization string   `json:"organization"`
	Addresses    []string `json:"addresses"`
	Source       string   `json:"source"`
	Method       string   `json:"method"`
	ObservedAt   int64    `json:"observed_at"`
	ExpiresAt    int64    `json:"expires_at"`
}
type geoObservation struct {
	NetworkInfo
	ConfigKey string `json:"config_key"`
}
type geoDisk struct {
	Revision      int64                     `json:"revision"`
	Enabled       bool                      `json:"enabled"`
	AutoUpdate    bool                      `json:"auto_update"`
	UpdatePending bool                      `json:"update_pending"`
	Generation    string                    `json:"generation"`
	Month         string                    `json:"month"`
	BuildAt       int64                     `json:"build_at"`
	InstalledAt   int64                     `json:"installed_at"`
	NextUpdateAt  int64                     `json:"next_update_at"`
	NextObserveAt int64                     `json:"next_observe_at"`
	Hashes        map[string]string         `json:"hashes"`
	Nodes         map[string]geoObservation `json:"nodes"`
}
type GeoRun struct {
	ID        string `json:"id"`
	Kind      string `json:"kind"`
	Total     int    `json:"total"`
	Completed int    `json:"completed"`
	Running   bool   `json:"running"`
}
type GeoState struct {
	Revision       int64          `json:"revision"`
	Enabled        bool           `json:"enabled"`
	AutoUpdate     bool           `json:"auto_update"`
	UpdatePending  bool           `json:"update_pending"`
	Installed      bool           `json:"installed"`
	Expired        bool           `json:"expired"`
	Month          string         `json:"month"`
	BuildAt        int64          `json:"build_at"`
	InstalledAt    int64          `json:"installed_at"`
	NextUpdateAt   int64          `json:"next_update_at"`
	NextObserveAt  int64          `json:"next_observe_at"`
	Source         string         `json:"source"`
	AttributionURL string         `json:"attribution_url"`
	LastError      string         `json:"last_error"`
	Run            *GeoRun        `json:"run"`
	Groups         []NetworkGroup `json:"groups"`
}
type NetworkGroup struct {
	ASN          uint     `json:"asn"`
	Organization string   `json:"organization"`
	Country      string   `json:"country"`
	NodeIDs      []string `json:"node_ids"`
}
type geoDatabase interface{ Lookup(net.IP, any) error }
type geoDataset struct {
	country, asn geoDatabase
	built        time.Time
}
type geoManager struct {
	mu           sync.Mutex
	pool         *Pool
	dir          string
	saved        geoDisk
	data         *geoDataset
	busy, closed bool
	run          *GeoRun
	lastError    string
	fetch        func(context.Context, string) ([]byte, error)
	open         func([]byte, string) (geoDatabase, time.Time, error)
	resolve      subscriptionResolver
}

func newGeoManager(p *Pool) *geoManager {
	m := &geoManager{pool: p, dir: filepath.Join(p.cfg.DataDir, "geodata"), saved: geoDisk{Revision: 1, AutoUpdate: true, Nodes: map[string]geoObservation{}, Hashes: map[string]string{}}, fetch: fetchGeoDatabase, open: openGeoDatabase, resolve: net.DefaultResolver.LookupNetIP}
	if info, e := os.Lstat(m.dir); e == nil && (!info.IsDir() || info.Mode()&os.ModeSymlink != 0) {
		m.lastError = "geo_storage_failed"
		return m
	}
	if os.MkdirAll(m.dir, 0700) != nil {
		m.lastError = "geo_storage_failed"
		return m
	}
	raw, err := readSourcePrivate(filepath.Join(m.dir, "state.json"), 256<<10)
	if err == nil {
		if !decodeJSON(raw, &m.saved) || !validGeoDisk(m.saved) {
			m.lastError = "geo_state_invalid"
			m.saved = geoDisk{Revision: 1, AutoUpdate: true, Nodes: map[string]geoObservation{}}
			return m
		}
		if m.saved.Nodes == nil {
			m.saved.Nodes = map[string]geoObservation{}
		}
		if m.saved.Generation != "" {
			data, e := m.loadDataset(m.saved)
			if e != nil {
				m.lastError = "geo_database_invalid"
			} else {
				m.data = data
				// Metadata from the verified databases is authoritative.
				m.saved.BuildAt = data.built.UnixMilli()
			}
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		m.lastError = "geo_state_invalid"
	}
	m.prune(m.saved.Generation)
	return m
}
func validGeoGeneration(id string) bool {
	return id == "" || len(id) == 32 && strings.Trim(id, "0123456789abcdef") == ""
}
func validGeoDisk(s geoDisk) bool {
	if s.Revision < 1 || len(s.Nodes) > maxNodes || !validGeoGeneration(s.Generation) {
		return false
	}
	if s.Generation != "" {
		if _, err := time.Parse("2006-01", s.Month); err != nil || len(s.Hashes) != 2 || s.BuildAt <= 0 || s.InstalledAt <= 0 {
			return false
		}
		for _, kind := range []string{"country", "asn"} {
			if len(s.Hashes[kind]) != 64 || strings.Trim(s.Hashes[kind], "0123456789abcdef") != "" {
				return false
			}
		}
	}
	for id, n := range s.Nodes {
		if len(id) == 0 || len(id) > 128 || len(n.ConfigKey) != 32 || strings.Trim(n.ConfigKey, "0123456789abcdef") != "" ||
			n.Source != geoProvider || n.Method != "server_address" || n.ObservedAt <= 0 || n.ObservedAt > time.Now().Add(time.Minute).UnixMilli() ||
			n.ExpiresAt > n.ObservedAt+geoTTL.Milliseconds()+1000 || len(n.Addresses) > 64 || n.Organization != cleanOrganization(n.Organization) ||
			(n.Country != "" && (len(n.Country) != 2 || strings.Trim(n.Country, "ABCDEFGHIJKLMNOPQRSTUVWXYZ") != "")) {
			return false
		}
		switch n.Status {
		case "unknown", "direct_or_external", "dns_failed", "observed", "lookup_failed", "mixed_networks", "interrupted":
		default:
			return false
		}
		for _, raw := range n.Addresses {
			ip, err := netip.ParseAddr(raw)
			if err != nil || !publicSubscriptionIP(ip) {
				return false
			}
		}
	}
	return true
}
func (m *geoManager) loadDataset(saved geoDisk) (*geoDataset, error) {
	d := &geoDataset{}
	for _, kind := range []string{"country", "asn"} {
		raw, e := readSourcePrivate(filepath.Join(m.dir, "generation-"+saved.Generation, kind+".mmdb"), 64<<20)
		if e != nil {
			return nil, e
		}
		sum := sha256.Sum256(raw)
		if hex.EncodeToString(sum[:]) != saved.Hashes[kind] {
			return nil, errors.New("hash mismatch")
		}
		reader, built, e := m.open(raw, kind)
		if e != nil {
			return nil, e
		}
		if d.built.IsZero() || built.Before(d.built) {
			d.built = built
		}
		if kind == "country" {
			d.country = reader
		} else {
			d.asn = reader
		}
	}
	return d, nil
}
func openGeoDatabase(raw []byte, kind string) (geoDatabase, time.Time, error) {
	reader, err := maxminddb.FromBytes(raw)
	if err != nil {
		return nil, time.Time{}, errors.New("geo_database_invalid")
	}
	// DB-IP uses DBIP-Country-Lite / DBIP-ASN-Lite database types.
	if !strings.HasPrefix(strings.ToLower(reader.Metadata.DatabaseType), "dbip-"+kind+"-") || reader.Metadata.BinaryFormatMajorVersion != 2 || reader.Verify() != nil {
		return nil, time.Time{}, errors.New("geo_database_invalid")
	}
	built := time.Unix(int64(reader.Metadata.BuildEpoch), 0)
	if built.After(time.Now().Add(24*time.Hour)) || time.Since(built) > geoDatabaseAge {
		return nil, time.Time{}, errors.New("geo_database_expired")
	}
	return reader, built, nil
}
func fetchGeoDatabase(ctx context.Context, address string) ([]byte, error) {
	client := subscriptionClient(net.DefaultResolver.LookupNetIP, (&net.Dialer{Timeout: 10 * time.Second}).DialContext)
	client.Timeout = 2 * time.Minute
	client.CheckRedirect = func(r *http.Request, via []*http.Request) error {
		if len(via) > 2 || r.URL.Scheme != "https" || r.URL.Hostname() != "download.db-ip.com" {
			return errors.New("redirect denied")
		}
		return nil
	}
	req, _ := http.NewRequestWithContext(ctx, "GET", address, nil)
	resp, err := client.Do(req)
	if err != nil {
		return nil, errors.New("geo_download_failed")
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 || resp.ContentLength > 32<<20 {
		return nil, errors.New("geo_download_failed")
	}
	compressed, err := io.ReadAll(io.LimitReader(resp.Body, (32<<20)+1))
	if err != nil || len(compressed) > 32<<20 {
		return nil, errors.New("geo_download_failed")
	}
	gz, err := gzip.NewReader(bytes.NewReader(compressed))
	if err != nil {
		return nil, errors.New("geo_database_invalid")
	}
	defer gz.Close()
	raw, err := io.ReadAll(io.LimitReader(gz, (64<<20)+1))
	if err != nil || len(raw) > 64<<20 {
		return nil, errors.New("geo_database_invalid")
	}
	return raw, nil
}
func (m *geoManager) persist(next geoDisk) error {
	raw, err := json.Marshal(next)
	if err != nil || len(raw) > 256<<10 || writeSourcePrivate(filepath.Join(m.dir, "state.json"), raw) != nil {
		return errors.New("geo_storage_failed")
	}
	m.saved = next
	return nil
}
func (m *geoManager) view() GeoState {
	m.pool.mu.Lock()
	nodes := append([]nodeConfig{}, m.pool.saved.Nodes...)
	m.pool.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	s := m.saved
	out := GeoState{Revision: s.Revision, Enabled: s.Enabled, AutoUpdate: s.AutoUpdate, Installed: m.data != nil, Month: s.Month, BuildAt: s.BuildAt, InstalledAt: s.InstalledAt, NextUpdateAt: s.NextUpdateAt, NextObserveAt: s.NextObserveAt, Source: geoProvider, AttributionURL: "https://db-ip.com", LastError: m.lastError, Groups: []NetworkGroup{}}
	out.Expired = m.data != nil && time.Now().After(m.data.built.Add(geoDatabaseAge))
	out.UpdatePending = s.UpdatePending
	if m.run != nil {
		run := *m.run
		out.Run = &run
	}
	groups := map[uint]*NetworkGroup{}
	if s.Enabled && m.data != nil && !out.Expired {
		for _, config := range nodes {
			n, ok := s.Nodes[config.ID]
			if !ok || n.ConfigKey != geoConfigKey(config) || n.ASN == 0 || n.ExpiresAt <= time.Now().UnixMilli() || n.Status != "observed" {
				continue
			}
			g := groups[n.ASN]
			if g == nil {
				g = &NetworkGroup{ASN: n.ASN, Organization: n.Organization, Country: n.Country, NodeIDs: []string{}}
				groups[n.ASN] = g
			}
			if g.Country != n.Country {
				g.Country = ""
			}
			g.NodeIDs = append(g.NodeIDs, config.ID)
		}
	}
	for _, g := range groups {
		sort.Strings(g.NodeIDs)
		out.Groups = append(out.Groups, *g)
	}
	sort.Slice(out.Groups, func(i, j int) bool { return out.Groups[i].ASN < out.Groups[j].ASN })
	return out
}
func geoConfigKey(n nodeConfig) string {
	identity := n.SourceID + "|" + n.SourceKey
	if n.SourceID == "" {
		u, _ := url.Parse(n.ProxyURL)
		if u != nil {
			identity = u.Scheme + "|" + strings.ToLower(u.Host)
		}
	}
	sum := sha256.Sum256([]byte(identity))
	return hex.EncodeToString(sum[:16])
}
func (m *geoManager) info(n nodeConfig) NetworkInfo {
	empty := NetworkInfo{Status: "not_observed", Addresses: []string{}, Source: geoProvider, Method: "server_address"}
	if m == nil {
		return empty
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !m.saved.Enabled || m.data == nil {
		return empty
	}
	value, ok := m.saved.Nodes[n.ID]
	if !ok || value.ConfigKey != geoConfigKey(n) {
		return empty
	}
	out := value.NetworkInfo
	out.Addresses = append([]string{}, out.Addresses...)
	if out.ExpiresAt <= time.Now().UnixMilli() || time.Now().After(m.data.built.Add(geoDatabaseAge)) {
		out.Status = "expired"
	}
	return out
}
func (m *geoManager) configure(expected int64, enabled, auto bool) error {
	m.pool.mu.Lock()
	defer m.pool.mu.Unlock()
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.busy {
		return errors.New("geo_busy")
	}
	if m.saved.Revision != expected {
		return errors.New("revision_conflict")
	}
	if enabled && m.data == nil {
		return errors.New("geo_not_installed")
	}
	if !enabled && m.saved.Enabled && m.pool.saved.SelectionPolicy.UseObservedRegion && m.pool.saved.SelectionPolicy.RegionMode == "only" {
		return errors.New("geo_policy_dependency")
	}
	if enabled && time.Now().After(m.data.built.Add(geoDatabaseAge)) {
		return errors.New("geo_database_expired")
	}
	next := m.saved
	next.Revision++
	next.Enabled = enabled
	next.AutoUpdate = auto
	if enabled {
		next.NextObserveAt = time.Now().UnixMilli()
	}
	return m.persist(next)
}
func (m *geoManager) start(kind string, expected int64) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed || m.busy {
		return errors.New("geo_busy")
	}
	if expected != m.saved.Revision {
		return errors.New("revision_conflict")
	}
	if kind == "observe" && (m.data == nil || !m.saved.Enabled) {
		return errors.New("geo_not_enabled")
	}
	if kind == "observe" && time.Now().After(m.data.built.Add(geoDatabaseAge)) {
		return errors.New("geo_database_expired")
	}
	if kind == "install" {
		next := m.saved
		next.Revision++
		next.UpdatePending = true
		next.NextUpdateAt = time.Now().Add(time.Hour).UnixMilli()
		if err := m.persist(next); err != nil {
			return err
		}
	}
	m.busy = true
	m.lastError = ""
	m.run = &GeoRun{ID: sourceID(), Kind: kind, Running: true}
	m.pool.wg.Add(1)
	go func() {
		defer m.pool.wg.Done()
		var err error
		if kind == "install" {
			err = m.install()
		} else {
			err = m.observe()
		}
		m.mu.Lock()
		m.busy = false
		m.run.Running = false
		if err != nil {
			m.lastError = err.Error()
			next := m.saved
			next.Revision++
			if kind == "install" {
				next.NextUpdateAt = time.Now().Add(time.Hour).UnixMilli()
			} else {
				next.NextObserveAt = time.Now().Add(10 * time.Minute).UnixMilli()
			}
			if m.persist(next) != nil {
				m.saved.NextUpdateAt = next.NextUpdateAt
				m.saved.NextObserveAt = next.NextObserveAt
				m.lastError = "geo_storage_failed"
			}
		}
		m.mu.Unlock()
		m.pool.mu.Lock()
		m.pool.chooseLocked("network_observation")
		m.pool.signalLocked()
		m.pool.mu.Unlock()
	}()
	return nil
}
func (m *geoManager) install() error {
	ctx, cancel := context.WithTimeout(m.pool.ctx, 4*time.Minute)
	defer cancel()
	now := time.Now()
	month := now.UTC().Format("2006-01")
	id := sourceID()
	dir := filepath.Join(m.dir, "generation-"+id)
	if os.Mkdir(dir, 0700) != nil {
		return errors.New("geo_storage_failed")
	}
	committed := false
	defer func() {
		if !committed {
			m.removeGeneration(id)
		}
	}()
	hashes := map[string]string{}
	data := &geoDataset{}
	m.mu.Lock()
	m.run.Total = 2
	m.mu.Unlock()
	for _, kind := range []string{"country", "asn"} {
		raw, err := m.fetch(ctx, "https://download.db-ip.com/free/dbip-"+kind+"-lite-"+month+".mmdb.gz")
		if err != nil {
			return err
		}
		reader, built, err := m.open(raw, kind)
		if err != nil {
			return err
		}
		if data.built.IsZero() || built.Before(data.built) {
			data.built = built
		}
		if kind == "country" {
			data.country = reader
		} else {
			data.asn = reader
		}
		sum := sha256.Sum256(raw)
		hashes[kind] = hex.EncodeToString(sum[:])
		if writeSourcePrivate(filepath.Join(dir, kind+".mmdb"), raw) != nil {
			return errors.New("geo_storage_failed")
		}
		m.mu.Lock()
		m.run.Completed++
		m.mu.Unlock()
	}
	m.mu.Lock()
	if m.closed || ctx.Err() != nil {
		m.mu.Unlock()
		return errors.New("geo_install_interrupted")
	}
	next := m.saved
	next.Revision++
	next.Generation = id
	next.Month = month
	next.BuildAt = data.built.UnixMilli()
	next.InstalledAt = now.UnixMilli()
	next.NextUpdateAt = now.Add(24 * time.Hour).UnixMilli()
	// Installing for the first time enables observations. Monthly updates
	// preserve an operator's explicit choice to disable them.
	if m.saved.Generation == "" {
		next.Enabled = true
	}
	// Keep still-valid observations while the next observation is pending.
	// Clearing them during a monthly update would strand strict-region routes.
	// Their original expiry is retained; installing data never extends evidence.
	next.NextObserveAt = now.UnixMilli()
	next.Hashes = hashes
	next.UpdatePending = false
	err := m.persist(next)
	if err == nil {
		m.data = data
		committed = true
	}
	m.mu.Unlock()
	if err != nil {
		return err
	}
	m.prune(id)
	return nil
}
func (m *geoManager) removeGeneration(id string) {
	if id == "" || !validGeoGeneration(id) {
		return
	}
	root, e := filepath.Abs(m.dir)
	if e != nil {
		return
	}
	dir := filepath.Join(root, "generation-"+id)
	if filepath.Dir(dir) != root {
		return
	}
	info, e := os.Lstat(dir)
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return
	}
	_ = os.RemoveAll(dir)
}
func (m *geoManager) prune(keep string) {
	entries, _ := os.ReadDir(m.dir)
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), "generation-") {
			id := strings.TrimPrefix(entry.Name(), "generation-")
			if id != keep {
				m.removeGeneration(id)
			}
		}
	}
}
func (m *geoManager) Close() { m.mu.Lock(); m.closed = true; m.mu.Unlock() }
func (m *geoManager) loop() {
	defer m.pool.wg.Done()
	ticker := time.NewTicker(time.Minute)
	defer ticker.Stop()
	for {
		select {
		case <-m.pool.ctx.Done():
			return
		case <-ticker.C:
			m.mu.Lock()
			next := m.saved
			ready := !m.closed && !m.busy
			now := time.Now()
			m.mu.Unlock()
			if !ready {
				continue
			}
			if kind := geoOperationDue(next, now); kind != "" {
				if kind == "check" {
					m.mu.Lock()
					if !m.closed && !m.busy && m.saved.Revision == next.Revision {
						next.Revision++
						next.NextUpdateAt = now.Add(24 * time.Hour).UnixMilli()
						if m.persist(next) != nil {
							m.lastError = "geo_storage_failed"
							m.saved.NextUpdateAt = now.Add(time.Hour).UnixMilli()
						}
					}
					m.mu.Unlock()
				} else {
					_ = m.start(kind, next.Revision)
				}
			}
		}
	}
}
func geoOperationDue(next geoDisk, now time.Time) string {
	if next.AutoUpdate && next.NextUpdateAt > 0 && next.NextUpdateAt <= now.UnixMilli() &&
		(next.UpdatePending || next.Generation != "" && next.Month != now.UTC().Format("2006-01")) {
		return "install"
	}
	if next.Generation != "" && next.Enabled && next.NextObserveAt <= now.UnixMilli() {
		return "observe"
	}
	if next.Generation != "" && next.AutoUpdate && next.NextUpdateAt > 0 && next.NextUpdateAt <= now.UnixMilli() {
		return "check"
	}
	return ""
}
func (p *Pool) serveGeo(w http.ResponseWriter, r *http.Request) bool {
	if !strings.HasPrefix(r.URL.Path, "/v1/geodata/") {
		return false
	}
	m := p.geo
	if r.URL.Path == "/v1/geodata/state" && r.Method == "GET" {
		answer(w, 200, m.view())
		return true
	}
	if r.Method != "POST" {
		failure(w, 405, "method_not_allowed")
		return true
	}
	var input struct {
		ExpectedRevision int64 `json:"expected_revision"`
		Enabled          bool  `json:"enabled"`
		AutoUpdate       bool  `json:"auto_update"`
	}
	if !decode(w, r, &input) {
		return true
	}
	var err error
	switch r.URL.Path {
	case "/v1/geodata/install":
		err = m.start("install", input.ExpectedRevision)
	case "/v1/geodata/observe":
		err = m.start("observe", input.ExpectedRevision)
	case "/v1/geodata/configure":
		err = m.configure(input.ExpectedRevision, input.Enabled, input.AutoUpdate)
	default:
		failure(w, 404, "not_found")
		return true
	}
	if err != nil {
		code := 400
		if err.Error() == "revision_conflict" || err.Error() == "geo_busy" {
			code = 409
		}
		failure(w, code, err.Error())
	} else {
		answer(w, 202, m.view())
	}
	return true
}
