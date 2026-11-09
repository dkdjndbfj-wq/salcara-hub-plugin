package hub

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

const ingressStateFile = "ingress-registry.json"
const ingressManifestTTL = 24 * time.Hour

type IngressEndpoint struct {
	ID         string `json:"id"`
	Name       string `json:"name"`
	URL        string `json:"url"`
	Enabled    bool   `json:"enabled"`
	Priority   int    `json:"priority"`
	Status     string `json:"status"`
	VerifiedAt int64  `json:"verified_at"`
	Error      string `json:"error"`
}
type IngressVerification struct {
	ID        string `json:"id"`
	Running   bool   `json:"running"`
	Total     int    `json:"total"`
	Completed int    `json:"completed"`
	Succeeded int    `json:"succeeded"`
	Failed    int    `json:"failed"`
}
type IngressAdminState struct {
	Revision          int64                `json:"revision"`
	Enabled           bool                 `json:"enabled"`
	Configured        bool                 `json:"configured"`
	CanonicalURL      string               `json:"canonical_url"`
	HubInstanceID     string               `json:"hub_instance_id"`
	IdentityPublicKey string               `json:"identity_public_key"`
	DefaultPriority   int                  `json:"default_priority"`
	Endpoints         []IngressEndpoint    `json:"endpoints"`
	Verification      *IngressVerification `json:"verification"`
	LastError         string               `json:"last_error"`
}
type IngressEndpointInput struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	URL      string `json:"url"`
	Enabled  bool   `json:"enabled"`
	Priority int    `json:"priority"`
}
type IngressSave struct {
	ExpectedRevision *int64                 `json:"expected_revision"`
	Enabled          bool                   `json:"enabled"`
	DefaultPriority  int                    `json:"default_priority"`
	Endpoints        []IngressEndpointInput `json:"endpoints"`
}
type IngressManifestEndpoint struct {
	ID       string `json:"id"`
	URL      string `json:"url"`
	Priority int    `json:"priority"`
}
type IngressManifest struct {
	Version           int                       `json:"version"`
	HubInstanceID     string                    `json:"hub_instance_id"`
	IdentityPublicKey string                    `json:"identity_public_key"`
	CanonicalURL      string                    `json:"canonical_url"`
	Revision          int64                     `json:"revision"`
	IssuedAt          int64                     `json:"issued_at"`
	ExpiresAt         int64                     `json:"expires_at"`
	Endpoints         []IngressManifestEndpoint `json:"endpoints"`
	SignedPayload     string                    `json:"signed_payload,omitempty"`
	Signature         string                    `json:"signature,omitempty"`
}
type IngressProof struct {
	Version       int    `json:"version"`
	HubInstanceID string `json:"hub_instance_id"`
	Nonce         string `json:"nonce"`
	IssuedAt      int64  `json:"issued_at"`
	ExpiresAt     int64  `json:"expires_at"`
	Signature     string `json:"signature"`
}
type ingressDisk struct {
	Version         int               `json:"version"`
	Revision        int64             `json:"revision"`
	Enabled         bool              `json:"enabled"`
	CanonicalURL    string            `json:"canonical_url"`
	HubInstanceID   string            `json:"hub_instance_id"`
	SigningSeed     string            `json:"signing_seed"`
	DefaultPriority int               `json:"default_priority"`
	Endpoints       []IngressEndpoint `json:"endpoints"`
}
type ingressRegistry struct {
	mu                        sync.Mutex
	saved                     ingressDisk
	key                       ed25519.PrivateKey
	dir, canonical, lastError string
	job                       *IngressVerification
	closed                    bool
	ctx                       context.Context
	cancel                    context.CancelFunc
	wg                        sync.WaitGroup
	client                    *http.Client
}

func ingressHex(bytes int) string {
	raw := make([]byte, bytes)
	if _, err := rand.Read(raw); err != nil {
		return ""
	}
	return hex.EncodeToString(raw)
}
func ingressValidHex(value string, bytes int) bool {
	return len(value) == bytes*2 && strings.Trim(value, "0123456789abcdef") == ""
}
func ingressPublicIP(ip netip.Addr) bool {
	if !ip.IsValid() || ip.Zone() != "" {
		return false
	}
	ip = ip.Unmap()
	if !ip.IsGlobalUnicast() || ip.IsPrivate() || ip.Is6() && !netip.MustParsePrefix("2000::/3").Contains(ip) {
		return false
	}
	for _, raw := range []string{"0.0.0.0/8", "100.64.0.0/10", "127.0.0.0/8", "169.254.0.0/16", "192.0.0.0/24", "192.0.2.0/24", "198.18.0.0/15", "198.51.100.0/24", "203.0.113.0/24", "224.0.0.0/4", "240.0.0.0/4", "2001::/23", "2001:db8::/32", "2002::/16", "3fff::/20"} {
		if netip.MustParsePrefix(raw).Contains(ip) {
			return false
		}
	}
	return true
}
func ingressAPIURL(raw string) (string, error) {
	u, err := url.Parse(raw)
	if err != nil || len(raw) > 512 || u == nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawPath != "" || u.Opaque != "" || strings.ContainsAny(raw, "%#\\\r\n\t ") {
		return "", errors.New("invalid_ingress_url")
	}
	host := strings.ToLower(u.Hostname())
	if host == "localhost" || strings.HasSuffix(host, ".localhost") || strings.HasSuffix(host, ".local") || !strings.ContainsAny(host, ".:") || strings.Contains(host, "%") {
		return "", errors.New("invalid_ingress_url")
	}
	if ip, e := netip.ParseAddr(host); e == nil && !ingressPublicIP(ip) {
		return "", errors.New("invalid_ingress_url")
	}
	if port := u.Port(); port != "" {
		n, e := strconv.Atoi(port)
		if e != nil || n < 1 || n > 65535 {
			return "", errors.New("invalid_ingress_url")
		}
	}
	// An ingress identifies the complete Hub endpoint, not a hostname or path
	// prefix. Preserve explicit mount prefixes and reject ambiguous spellings
	// instead of cleaning paths that a reverse proxy might interpret differently.
	if strings.Contains(u.Path, "//") || u.EscapedPath() != u.Path {
		return "", errors.New("invalid_ingress_url")
	}
	for _, segment := range strings.Split(u.Path, "/") {
		if segment == "." || segment == ".." {
			return "", errors.New("invalid_ingress_url")
		}
		for _, ch := range segment {
			if !(ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9' || ch == '.' || ch == '_' || ch == '~' || ch == '-') {
				return "", errors.New("invalid_ingress_url")
			}
		}
	}
	hubPath := strings.TrimSuffix(u.Path, "/")
	switch {
	case hubPath == "":
		hubPath = "/salcara-hub/v1"
	case strings.HasSuffix(hubPath, "/salcara-hub"):
		hubPath += "/v1"
	case strings.HasSuffix(hubPath, "/v1"):
	default:
		return "", errors.New("invalid_ingress_url")
	}
	u.Host = strings.ToLower(u.Host)
	if u.Port() == "443" {
		u.Host = u.Hostname()
		if strings.Contains(u.Host, ":") {
			u.Host = "[" + u.Host + "]"
		}
	}
	u.Path = hubPath
	normalized := u.String()
	if len(normalized) > 512 {
		return "", errors.New("invalid_ingress_url")
	}
	return normalized, nil
}
func ingressClient() *http.Client {
	return &http.Client{Timeout: 5 * time.Second, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }, Transport: &http.Transport{Proxy: nil, DisableKeepAlives: true, TLSHandshakeTimeout: 3 * time.Second, ResponseHeaderTimeout: 3 * time.Second, DialContext: func(ctx context.Context, network, address string) (net.Conn, error) {
		host, port, err := net.SplitHostPort(address)
		if err != nil {
			return nil, errors.New("ingress_connect_failed")
		}
		ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", host)
		if err != nil || len(ips) == 0 || len(ips) > 64 {
			return nil, errors.New("ingress_connect_failed")
		}
		for _, ip := range ips {
			if !ingressPublicIP(ip) {
				return nil, errors.New("ingress_unsafe_address")
			}
		}
		for _, ip := range ips {
			conn, e := (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, network, net.JoinHostPort(ip.String(), port))
			if e == nil {
				return conn, nil
			}
		}
		return nil, errors.New("ingress_connect_failed")
	}}}
}
func readIngressDisk(dir string) (ingressDisk, error) {
	path := filepath.Join(dir, ingressStateFile)
	info, err := os.Lstat(path)
	if err != nil {
		return ingressDisk{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > 64<<10 || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0 {
		return ingressDisk{}, errors.New("invalid ingress registry")
	}
	f, err := os.Open(path)
	if err != nil {
		return ingressDisk{}, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return ingressDisk{}, errors.New("ingress registry changed")
	}
	dec := json.NewDecoder(io.LimitReader(f, (64<<10)+1))
	dec.DisallowUnknownFields()
	var disk ingressDisk
	if dec.Decode(&disk) != nil {
		return ingressDisk{}, errors.New("invalid ingress registry")
	}
	var extra any
	if dec.Decode(&extra) != io.EOF {
		return ingressDisk{}, errors.New("invalid ingress registry")
	}
	if disk.Version != 1 || disk.Revision < 1 || !ingressValidHex(disk.HubInstanceID, 16) || !ingressValidHex(disk.SigningSeed, 32) || disk.DefaultPriority < 1 || disk.DefaultPriority > 1000 || len(disk.Endpoints) > 7 {
		return ingressDisk{}, errors.New("invalid ingress registry")
	}
	if disk.CanonicalURL != "" {
		canonical, err := ingressAPIURL(disk.CanonicalURL)
		if err != nil || canonical != disk.CanonicalURL {
			return ingressDisk{}, errors.New("invalid ingress registry")
		}
	}
	seen := map[string]bool{}
	for _, e := range disk.Endpoints {
		normalized, err := ingressAPIURL(e.URL)
		validStatus := e.Status == "unverified" || e.Status == "verifying" || e.Status == "verified" || e.Status == "failed" || e.Status == "interrupted"
		if err != nil || normalized != e.URL || e.URL == disk.CanonicalURL || !ingressValidHex(e.ID, 16) || seen[e.ID] || seen[e.URL] || e.Priority < 1 || e.Priority > 1000 || strings.TrimSpace(e.Name) == "" || len(e.Name) > 80 || strings.IndexFunc(e.Name, unicode.IsControl) >= 0 || !validStatus || len(e.Error) > 80 || e.VerifiedAt < 0 || e.Status == "verified" && (e.VerifiedAt == 0 || e.VerifiedAt > time.Now().Add(time.Minute).UnixMilli()) {
			return ingressDisk{}, errors.New("invalid ingress registry")
		}
		seen[e.ID] = true
		seen[e.URL] = true
	}
	return disk, nil
}
func writeIngressDisk(dir string, disk ingressDisk) error {
	path := filepath.Join(dir, ingressStateFile)
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || runtime.GOOS != "windows" && info.Mode().Perm()&0077 != 0) {
		return errors.New("ingress_storage_failed")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("ingress_storage_failed")
	}
	raw, err := json.Marshal(disk)
	if err != nil || len(raw) > 64<<10 {
		return errors.New("ingress_storage_failed")
	}
	f, err := os.CreateTemp(dir, ".ingress-*")
	if err != nil {
		return errors.New("ingress_storage_failed")
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(raw)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil || closeErr != nil || os.Rename(name, path) != nil {
		return errors.New("ingress_storage_failed")
	}
	if runtime.GOOS != "windows" {
		if d, e := os.Open(dir); e == nil {
			_ = d.Sync()
			_ = d.Close()
		}
	}
	return nil
}
func newIngressRegistry(dir, publicURL string) *ingressRegistry {
	ctx, cancel := context.WithCancel(context.Background())
	r := &ingressRegistry{dir: dir, ctx: ctx, cancel: cancel, client: ingressClient(), saved: ingressDisk{Version: 1, Revision: 1, DefaultPriority: 100, Endpoints: []IngressEndpoint{}}}
	r.canonical, _ = ingressAPIURL(publicURL)
	if dir == "" {
		r.lastError = "ingress_persistence_required"
		return r
	}
	disk, err := readIngressDisk(dir)
	if errors.Is(err, os.ErrNotExist) {
		disk = r.saved
		disk.HubInstanceID = ingressHex(16)
		disk.SigningSeed = ingressHex(32)
		disk.CanonicalURL = r.canonical
		if disk.HubInstanceID == "" || disk.SigningSeed == "" || writeIngressDisk(dir, disk) != nil {
			r.lastError = "ingress_storage_failed"
			return r
		}
	} else if err != nil {
		r.lastError = "ingress_storage_failed"
		return r
	}
	seed, _ := hex.DecodeString(disk.SigningSeed)
	r.key = ed25519.NewKeyFromSeed(seed)
	if disk.CanonicalURL != r.canonical {
		disk.CanonicalURL = r.canonical
		disk.Enabled = false
		disk.Revision++
		for i := range disk.Endpoints {
			disk.Endpoints[i].Status = "unverified"
			disk.Endpoints[i].VerifiedAt = 0
		}
		if writeIngressDisk(dir, disk) != nil {
			r.lastError = "ingress_storage_failed"
			r.key = nil
			return r
		}
	}
	for i := range disk.Endpoints {
		if disk.Endpoints[i].Status == "verifying" {
			disk.Endpoints[i].Status = "interrupted"
			disk.Endpoints[i].Error = "verification_interrupted"
		}
	}
	r.saved = disk
	return r
}
func (r *ingressRegistry) close() {
	r.mu.Lock()
	r.closed = true
	r.cancel()
	r.mu.Unlock()
	r.wg.Wait()
	r.client.CloseIdleConnections()
}
func (r *ingressRegistry) configured() bool {
	return r.canonical != "" && len(r.key) == ed25519.PrivateKeySize
}
func (r *ingressRegistry) stateLocked() IngressAdminState {
	s := IngressAdminState{Revision: r.saved.Revision, Enabled: r.saved.Enabled, Configured: r.configured(), CanonicalURL: r.canonical, HubInstanceID: r.saved.HubInstanceID, DefaultPriority: r.saved.DefaultPriority, Endpoints: append([]IngressEndpoint{}, r.saved.Endpoints...), LastError: r.lastError}
	if len(r.key) > 0 {
		s.IdentityPublicKey = base64.StdEncoding.EncodeToString(r.key.Public().(ed25519.PublicKey))
	}
	if r.job != nil {
		copy := *r.job
		s.Verification = &copy
	}
	return s
}
func (h *Hub) IngressState() IngressAdminState {
	r := h.ingress
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.stateLocked()
}
func (r *ingressRegistry) persistLocked(next ingressDisk) error {
	if err := writeIngressDisk(r.dir, next); err != nil {
		r.lastError = "ingress_storage_failed"
		return err
	}
	r.saved = next
	r.lastError = ""
	return nil
}
func (h *Hub) SaveIngress(input IngressSave) (IngressAdminState, error) {
	r := h.ingress
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return IngressAdminState{}, errors.New("stopped")
	}
	if !r.configured() {
		return IngressAdminState{}, errors.New("ingress_not_configured")
	}
	if r.job != nil && r.job.Running {
		return IngressAdminState{}, errors.New("ingress_busy")
	}
	if input.ExpectedRevision == nil {
		return IngressAdminState{}, errors.New("expected_revision_required")
	}
	if *input.ExpectedRevision != r.saved.Revision {
		return IngressAdminState{}, errors.New("revision_conflict")
	}
	if len(input.Endpoints) > 7 || input.DefaultPriority < 1 || input.DefaultPriority > 1000 {
		return IngressAdminState{}, errors.New("invalid_ingress_configuration")
	}
	next := r.saved
	next.Revision++
	next.Enabled = input.Enabled
	next.DefaultPriority = input.DefaultPriority
	next.Endpoints = []IngressEndpoint{}
	seen := map[string]bool{}
	for _, in := range input.Endpoints {
		address, err := ingressAPIURL(in.URL)
		name := strings.TrimSpace(in.Name)
		if err != nil || address == r.canonical || seen[address] || in.Priority < 1 || in.Priority > 1000 || name == "" || len(name) > 80 || strings.IndexFunc(name, unicode.IsControl) >= 0 {
			return IngressAdminState{}, errors.New("invalid_ingress_configuration")
		}
		seen[address] = true
		entry := IngressEndpoint{ID: in.ID, Name: name, URL: address, Enabled: in.Enabled, Priority: in.Priority, Status: "unverified"}
		if entry.ID == "" {
			entry.ID = ingressHex(16)
			if entry.ID == "" {
				return IngressAdminState{}, errors.New("ingress_storage_failed")
			}
		} else {
			found := false
			for _, old := range r.saved.Endpoints {
				if old.ID == entry.ID {
					found = true
					if old.URL == address {
						entry.Status = old.Status
						entry.VerifiedAt = old.VerifiedAt
						entry.Error = old.Error
					}
					break
				}
			}
			if !found || seen[entry.ID] {
				return IngressAdminState{}, errors.New("invalid_ingress_configuration")
			}
		}
		seen[entry.ID] = true
		next.Endpoints = append(next.Endpoints, entry)
	}
	if err := r.persistLocked(next); err != nil {
		return IngressAdminState{}, err
	}
	return r.stateLocked(), nil
}
func (h *Hub) VerifyIngress(expected *int64, id string) (IngressAdminState, error) {
	r := h.ingress
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return IngressAdminState{}, errors.New("stopped")
	}
	if !r.configured() {
		return IngressAdminState{}, errors.New("ingress_not_configured")
	}
	if expected == nil {
		return IngressAdminState{}, errors.New("expected_revision_required")
	}
	if *expected != r.saved.Revision {
		return IngressAdminState{}, errors.New("revision_conflict")
	}
	if r.job != nil && r.job.Running {
		return IngressAdminState{}, errors.New("ingress_busy")
	}
	chosen := []IngressEndpoint{}
	next := r.saved
	next.Endpoints = append([]IngressEndpoint{}, next.Endpoints...)
	for i := range next.Endpoints {
		e := &next.Endpoints[i]
		if id == e.ID || id == "" && e.Enabled {
			chosen = append(chosen, *e)
			e.Status = "verifying"
			e.Error = ""
			e.VerifiedAt = 0
		}
	}
	if len(chosen) == 0 {
		return IngressAdminState{}, errors.New("ingress_not_found")
	}
	job := &IngressVerification{ID: ingressHex(16), Running: true, Total: len(chosen)}
	if job.ID == "" {
		return IngressAdminState{}, errors.New("ingress_storage_failed")
	}
	next.Revision++
	if err := r.persistLocked(next); err != nil {
		return IngressAdminState{}, err
	}
	r.job = job
	r.wg.Add(1)
	go r.verify(job, chosen)
	return r.stateLocked(), nil
}
func ingressProofMessage(proof IngressProof) []byte {
	return []byte(fmt.Sprintf("salcara-ingress-proof-v1\n%s\n%s\n%d\n%d", proof.HubInstanceID, proof.Nonce, proof.IssuedAt, proof.ExpiresAt))
}
func (r *ingressRegistry) checkEndpoint(ctx context.Context, address string) string {
	r.mu.Lock()
	hubID := r.saved.HubInstanceID
	publicKey := append(ed25519.PublicKey{}, r.key.Public().(ed25519.PublicKey)...)
	r.mu.Unlock()
	nonce := ingressHex(32)
	if nonce == "" {
		return "ingress_verification_failed"
	}
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, address+"/ingress/identity?nonce="+nonce, nil)
	req.Header.Set("Accept", "application/json")
	resp, err := r.client.Do(req)
	if err != nil {
		return "ingress_connect_failed"
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, (16<<10)+1))
	if err != nil || len(raw) > 16<<10 || resp.StatusCode != 200 {
		return "ingress_verification_failed"
	}
	var proof IngressProof
	if json.Unmarshal(raw, &proof) != nil || proof.Version != 1 || proof.HubInstanceID != hubID || proof.Nonce != nonce {
		return "ingress_identity_mismatch"
	}
	now := time.Now().UnixMilli()
	if proof.IssuedAt < now-30000 || proof.IssuedAt > now+5000 || proof.ExpiresAt <= now || proof.ExpiresAt-proof.IssuedAt != 30000 {
		return "ingress_proof_expired"
	}
	sig, err := base64.StdEncoding.DecodeString(proof.Signature)
	if err != nil || !ed25519.Verify(publicKey, ingressProofMessage(proof), sig) {
		return "ingress_identity_mismatch"
	}
	return ""
}
func (r *ingressRegistry) verify(job *IngressVerification, endpoints []IngressEndpoint) {
	defer r.wg.Done()
	ctx, cancel := context.WithTimeout(r.ctx, 30*time.Second)
	defer cancel()
	gate := make(chan struct{}, 2)
	var workers sync.WaitGroup
	for _, entry := range endpoints {
		workers.Add(1)
		go func(entry IngressEndpoint) {
			defer workers.Done()
			code := "verification_interrupted"
			select {
			case gate <- struct{}{}:
				code = r.checkEndpoint(ctx, entry.URL)
				<-gate
			case <-ctx.Done():
			}
			r.mu.Lock()
			defer r.mu.Unlock()
			job.Completed++
			if r.closed {
				return
			}
			next := r.saved
			next.Revision++
			next.Endpoints = append([]IngressEndpoint{}, next.Endpoints...)
			for i := range next.Endpoints {
				e := &next.Endpoints[i]
				if e.ID == entry.ID && e.URL == entry.URL {
					e.Status = "failed"
					e.Error = code
					if code == "" {
						e.Status = "verified"
						e.VerifiedAt = time.Now().UnixMilli()
					}
					break
				}
			}
			if err := r.persistLocked(next); err != nil {
				code = "ingress_storage_failed"
			}
			if code == "" {
				job.Succeeded++
			} else {
				job.Failed++
			}
		}(entry)
	}
	workers.Wait()
	r.mu.Lock()
	job.Running = false
	r.mu.Unlock()
}
func ingressManifestMessage(payload []byte) []byte {
	return append([]byte("salcara-ingress-manifest-v1\n"), payload...)
}
func (h *Hub) handleIngressManifest(w http.ResponseWriter, req *http.Request, _ string) {
	r := h.ingress
	r.mu.Lock()
	defer r.mu.Unlock()
	if !r.configured() || req.URL.RawQuery != "" || req.URL.ForceQuery {
		writeError(w, 404, "ingress_not_configured")
		return
	}
	now := time.Now()
	m := IngressManifest{Version: 1, HubInstanceID: r.saved.HubInstanceID, IdentityPublicKey: base64.StdEncoding.EncodeToString(r.key.Public().(ed25519.PublicKey)), CanonicalURL: r.canonical, Revision: r.saved.Revision, IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(ingressManifestTTL).UnixMilli(), Endpoints: []IngressManifestEndpoint{{"canonical", r.canonical, r.saved.DefaultPriority}}}
	if r.saved.Enabled {
		for _, e := range r.saved.Endpoints {
			if e.Enabled && e.Status == "verified" {
				m.Endpoints = append(m.Endpoints, IngressManifestEndpoint{e.ID, e.URL, e.Priority})
			}
		}
	}
	sort.SliceStable(m.Endpoints, func(i, j int) bool { return m.Endpoints[i].Priority < m.Endpoints[j].Priority })
	// Sign the exact unsigned JSON bytes so clients can authenticate the complete
	// endpoint list without reproducing JSON serialization across platforms.
	payload, err := json.Marshal(m)
	if err != nil {
		writeError(w, 500, "ingress_manifest_failed")
		return
	}
	m.SignedPayload = base64.StdEncoding.EncodeToString(payload)
	m.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(r.key, ingressManifestMessage(payload)))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, m)
}
func (h *Hub) handleIngressIdentity(w http.ResponseWriter, req *http.Request, _ string) {
	r := h.ingress
	r.mu.Lock()
	defer r.mu.Unlock()
	query, err := url.ParseQuery(req.URL.RawQuery)
	if !r.configured() || err != nil || len(query) != 1 || len(query["nonce"]) != 1 || !ingressValidHex(query.Get("nonce"), 32) {
		writeError(w, 400, "invalid_ingress_challenge")
		return
	}
	now := time.Now()
	proof := IngressProof{Version: 1, HubInstanceID: r.saved.HubInstanceID, Nonce: query.Get("nonce"), IssuedAt: now.UnixMilli(), ExpiresAt: now.Add(30 * time.Second).UnixMilli()}
	proof.Signature = base64.StdEncoding.EncodeToString(ed25519.Sign(r.key, ingressProofMessage(proof)))
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, 200, proof)
}
