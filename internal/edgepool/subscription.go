package edgepool

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"
)

type SubscriptionPreview struct {
	Format      string             `json:"format"`
	Total       int                `json:"total"`
	Supported   int                `json:"supported"`
	Unsupported int                `json:"unsupported"`
	Nodes       []SubscriptionNode `json:"nodes"`
	Warnings    []string           `json:"warnings"`
	PreviewID   string             `json:"preview_id"`
	ExpiresAt   int64              `json:"expires_at"`
}

type subscriptionEntry struct {
	nodes   []nodeConfig
	expires time.Time
	timer   *time.Timer
}

// SubscriptionInspector checks compatibility without executing rules, fetching
// provider URLs from the document, or publishing source URLs and credentials.
type SubscriptionInspector struct {
	mu     sync.Mutex
	client *http.Client
	cache  map[string]*subscriptionEntry
	limit  chan struct{}
	now    func() time.Time
	ttl    time.Duration
	closed bool
}

func NewSubscriptionInspector() *SubscriptionInspector {
	return &SubscriptionInspector{client: subscriptionClient(net.DefaultResolver.LookupNetIP, (&net.Dialer{Timeout: 3 * time.Second}).DialContext), cache: make(map[string]*subscriptionEntry), limit: make(chan struct{}, 2), now: time.Now, ttl: 10 * time.Minute}
}

// Inspect performs a bounded public-HTTPS fetch and returns a secret-free preview.
// Supported means native transport compatibility, not a successful network probe.
func (s *SubscriptionInspector) Inspect(ctx context.Context, rawURL string) (SubscriptionPreview, error) {
	select {
	case s.limit <- struct{}{}:
		defer func() { <-s.limit }()
	default:
		return SubscriptionPreview{}, errors.New("subscription_limit")
	}
	body, err := fetchSubscription(ctx, s.client, rawURL)
	if err != nil {
		return SubscriptionPreview{}, err
	}
	parsed, err := parseSubscription(body)
	if err != nil {
		return SubscriptionPreview{}, err
	}
	preview := SubscriptionPreview{Format: parsed.format, Total: len(parsed.nodes), Supported: len(parsed.configs), Unsupported: len(parsed.nodes) - len(parsed.configs), Nodes: parsed.nodes, Warnings: []string{"仅检测配置兼容性，连通性需导入后探测。", "仅提取节点；订阅中的规则、DNS、脚本、TUN和外部资源不会执行。"}}
	if preview.Unsupported > 0 {
		preview.Warnings = append(preview.Warnings, "需要 Mihomo 的协议无法直接导入，可连接已部署 Mihomo 的 mixed/SOCKS 监听端口。")
	}
	var random [16]byte
	if _, err = rand.Read(random[:]); err != nil {
		return SubscriptionPreview{}, errors.New("subscription_limit")
	}
	id := hex.EncodeToString(random[:])
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	if s.closed || len(s.cache) >= 8 {
		return SubscriptionPreview{}, errors.New("subscription_limit")
	}
	expires := s.now().Add(s.ttl)
	entry := &subscriptionEntry{nodes: parsed.configs, expires: expires}
	entry.timer = time.AfterFunc(s.ttl, func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		if s.cache[id] == entry {
			delete(s.cache, id)
		}
	})
	s.cache[id] = entry
	preview.PreviewID = id
	preview.ExpiresAt = expires.UnixMilli()
	return preview, nil
}

func (s *SubscriptionInspector) expireLocked() {
	for id, entry := range s.cache {
		if !s.now().Before(entry.expires) {
			entry.timer.Stop()
			delete(s.cache, id)
		}
	}
}
func (s *SubscriptionInspector) previewNodes(id string) ([]nodeConfig, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.expireLocked()
	entry := s.cache[id]
	if entry == nil || s.closed {
		return nil, errors.New("preview_expired")
	}
	return append([]nodeConfig{}, entry.nodes...), nil
}
func (s *SubscriptionInspector) consume(id string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if entry := s.cache[id]; entry != nil {
		entry.timer.Stop()
		delete(s.cache, id)
	}
}
func (s *SubscriptionInspector) Close() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.closed = true
	for id, entry := range s.cache {
		entry.timer.Stop()
		delete(s.cache, id)
	}
	if t, ok := s.client.Transport.(*http.Transport); ok {
		t.CloseIdleConnections()
	}
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(status int) {
	w.status = status
	w.ResponseWriter.WriteHeader(status)
}

func (p *Pool) inspectSubscription(w http.ResponseWriter, r *http.Request) {
	var body struct {
		URL string `json:"url"`
	}
	if !decode(w, r, &body) {
		return
	}
	preview, err := p.subscriptions.Inspect(r.Context(), body.URL)
	if err != nil {
		failure(w, 400, err.Error())
		return
	}
	answer(w, 200, preview)
}

func (p *Pool) importSubscription(w http.ResponseWriter, r *http.Request) {
	var body struct {
		PreviewID        string `json:"preview_id"`
		ExpectedRevision *int64 `json:"expected_revision"`
		NodeIndexes      []int  `json:"node_indexes"`
	}
	if !decode(w, r, &body) {
		return
	}
	nodes, err := p.subscriptions.previewNodes(body.PreviewID)
	if err != nil {
		failure(w, 400, "preview_expired")
		return
	}
	if len(nodes) == 0 {
		failure(w, 400, "subscription_no_supported_nodes")
		return
	}
	if body.NodeIndexes != nil {
		byIndex := make(map[int]nodeConfig, len(nodes))
		for _, node := range nodes {
			byIndex[node.subscriptionIndex] = node
		}
		selected := make([]nodeConfig, 0, len(body.NodeIndexes))
		seen := make(map[int]bool)
		for _, index := range body.NodeIndexes {
			node, ok := byIndex[index]
			if !ok || seen[index] {
				failure(w, 400, "invalid_request")
				return
			}
			seen[index] = true
			selected = append(selected, node)
		}
		if len(selected) == 0 {
			failure(w, 400, "subscription_no_supported_nodes")
			return
		}
		nodes = selected
	}
	recorded := &statusWriter{ResponseWriter: w}
	p.change(recorded, body.ExpectedRevision, func(s *savedConfig) string {
		seen := make(map[string]bool)
		for _, n := range s.Nodes {
			seen[n.ProxyURL] = true
		}
		added := 0
		for _, node := range nodes {
			if seen[node.ProxyURL] {
				continue
			}
			if len(s.Nodes) >= maxNodes {
				return "node_limit"
			}
			var random [8]byte
			if _, err := rand.Read(random[:]); err != nil {
				return "subscription_limit"
			}
			node.ID = "node-" + hex.EncodeToString(random[:])
			s.Nodes = append(s.Nodes, node)
			seen[node.ProxyURL] = true
			added++
		}
		if added == 0 {
			return "subscription_no_supported_nodes"
		}
		if s.SelectedID == "" {
			s.SelectedID = firstEnabled(*s)
		}
		return ""
	})
	if recorded.status == http.StatusOK {
		p.subscriptions.consume(body.PreviewID)
		p.Probe()
	}
}
