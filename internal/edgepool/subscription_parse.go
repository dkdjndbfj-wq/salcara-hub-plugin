package edgepool

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/url"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"go.yaml.in/yaml/v3"
)

type SubscriptionNode struct {
	Index     int    `json:"index"`
	Name      string `json:"name"`
	Protocol  string `json:"protocol"`
	Supported bool   `json:"supported"`
	Reason    string `json:"reason"`
}

type parsedSubscription struct {
	format  string
	nodes   []SubscriptionNode
	configs []nodeConfig
}

func safeNodeName(name string, index int) string {
	name = strings.TrimSpace(name)
	if name == "" || strings.Contains(name, "://") {
		return fmt.Sprintf("节点 %d", index+1)
	}
	name = strings.Map(func(r rune) rune {
		if unicode.IsControl(r) {
			return -1
		}
		return r
	}, name)
	var limited strings.Builder
	for _, r := range name {
		if limited.Len()+utf8.RuneLen(r) > 160 {
			break
		}
		limited.WriteRune(r)
	}
	if strings.TrimSpace(limited.String()) == "" {
		return fmt.Sprintf("节点 %d", index+1)
	}
	return limited.String()
}

func decodeSubscriptionBase64(raw string) ([]byte, bool) {
	raw = strings.Join(strings.Fields(raw), "")
	for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
		if decoded, err := encoding.DecodeString(raw); err == nil && utf8.Valid(decoded) {
			return decoded, true
		}
	}
	return nil, false
}

func parseSubscription(body []byte) (parsedSubscription, error) {
	if len(body) == 0 || len(body) > subscriptionBytes || !utf8.Valid(body) || bytes.IndexByte(body, 0) >= 0 {
		return parsedSubscription{}, errors.New("subscription_format_unsupported")
	}
	raw := strings.TrimSpace(strings.TrimPrefix(string(body), "\ufeff"))
	if strings.HasPrefix(raw, "<") {
		return parsedSubscription{}, errors.New("subscription_format_unsupported")
	}
	format := "uri_list"
	if !strings.Contains(raw, "://") && !strings.ContainsAny(raw, "{}[]:") {
		if decoded, ok := decodeSubscriptionBase64(raw); ok && bytes.Contains(decoded, []byte("://")) {
			raw = strings.TrimSpace(string(decoded))
			format = "base64_uri_list"
		}
	}
	if first := strings.SplitN(raw, "\n", 2)[0]; uriListStart(first) {
		return parseURIList(raw, format)
	}
	var doc yaml.Node
	decoder := yaml.NewDecoder(strings.NewReader(raw))
	if decoder.Decode(&doc) != nil {
		return parsedSubscription{}, errors.New("subscription_format_unsupported")
	}
	var extra yaml.Node
	if decoder.Decode(&extra) != io.EOF {
		return parsedSubscription{}, errors.New("subscription_format_unsupported")
	}
	count := 0
	if !validYAMLTree(&doc, 0, &count) {
		return parsedSubscription{}, errors.New("subscription_format_unsupported")
	}
	if len(doc.Content) != 1 {
		return parsedSubscription{}, errors.New("subscription_format_unsupported")
	}
	root := doc.Content[0]
	var list *yaml.Node
	if root.Kind == yaml.SequenceNode {
		list = root
	} else if root.Kind == yaml.MappingNode {
		list = yamlFields(root)["proxies"]
	}
	if list == nil || list.Kind != yaml.SequenceNode || len(list.Content) == 0 {
		return parsedSubscription{}, errors.New("subscription_format_unsupported")
	}
	if len(list.Content) > 256 {
		return parsedSubscription{}, errors.New("subscription_limit")
	}
	result := parsedSubscription{format: "clash_yaml"}
	if json.Valid([]byte(raw)) {
		result.format = "clash_json"
	}
	seen := make(map[string]bool)
	for i, node := range list.Content {
		fields := yamlFields(node)
		protocol := strings.ToLower(yamlString(fields["type"]))
		name := safeNodeName(yamlString(fields["name"]), i)
		public := SubscriptionNode{Index: i, Name: name, Protocol: safeProtocol(protocol), Reason: "unsupported_protocol"}
		if node.Kind != yaml.MappingNode || protocol == "" {
			public.Reason = "invalid_node"
			result.nodes = append(result.nodes, public)
			continue
		}
		if requiresMihomo(protocol) {
			public.Reason = "requires_mihomo"
			result.nodes = append(result.nodes, public)
			continue
		}
		if protocol != "http" && protocol != "https" && protocol != "socks5" && protocol != "socks5h" {
			result.nodes = append(result.nodes, public)
			continue
		}
		server := yamlString(fields["server"])
		port, err := strconv.Atoi(yamlString(fields["port"]))
		if err != nil || port < 1 || port > 65535 || server == "" || strings.ContainsAny(server, "/@?#\\\r\n\t ") {
			public.Reason = "invalid_node"
			result.nodes = append(result.nodes, public)
			continue
		}
		tlsEnabled, tlsValid := yamlBoolOption(fields["tls"])
		skipVerify, skipValid := yamlBoolOption(fields["skip-cert-verify"])
		if !tlsValid || !skipValid {
			public.Reason = "invalid_node"
			result.nodes = append(result.nodes, public)
			continue
		}
		if skipVerify {
			public.Reason = "insecure_tls"
			result.nodes = append(result.nodes, public)
			continue
		}
		if (protocol == "socks5" || protocol == "socks5h") && tlsEnabled {
			public.Reason = "requires_mihomo"
			result.nodes = append(result.nodes, public)
			continue
		}
		unsupportedOptions := false
		for _, key := range []string{"dialer-proxy", "interface-name", "routing-mark", "headers", "ws-opts", "grpc-opts", "fingerprint", "client-fingerprint"} {
			if fields[key] != nil {
				unsupportedOptions = true
			}
		}
		if sni := yamlString(fields["sni"]); sni != "" && sni != server {
			unsupportedOptions = true
		}
		if unsupportedOptions {
			public.Reason = "requires_mihomo"
			result.nodes = append(result.nodes, public)
			continue
		}
		if protocol == "http" && tlsEnabled {
			protocol = "https"
		}
		public.Protocol = protocol
		u := &url.URL{Scheme: protocol, Host: net.JoinHostPort(strings.ToLower(server), strconv.Itoa(port))}
		username, password := yamlString(fields["username"]), yamlString(fields["password"])
		if username != "" || password != "" {
			u.User = url.UserPassword(username, password)
		}
		appendNative(&result, public, u.String(), seen)
	}
	return result, nil
}

func uriListStart(line string) bool {
	scheme, _, ok := strings.Cut(line, "://")
	return ok && safeProtocol(strings.ToLower(scheme)) != "unknown"
}

func safeProtocol(s string) string {
	if s == "" || len(s) > 32 {
		return "unknown"
	}
	for _, r := range s {
		if !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-') {
			return "unknown"
		}
	}
	return s
}
func requiresMihomo(s string) bool {
	switch s {
	case "ss", "ssr", "trojan", "vmess", "vless", "hysteria", "hysteria2", "hy2", "tuic", "wireguard", "snell":
		return true
	}
	return false
}
func yamlString(n *yaml.Node) string {
	if n == nil || n.Kind != yaml.ScalarNode {
		return ""
	}
	return n.Value
}
func yamlBoolOption(n *yaml.Node) (bool, bool) {
	if n == nil {
		return false, true
	}
	if n.Kind != yaml.ScalarNode || n.Tag != "!!bool" {
		return false, false
	}
	switch strings.ToLower(n.Value) {
	case "true":
		return true, true
	case "false":
		return false, true
	}
	return false, false
}
func yamlFields(n *yaml.Node) map[string]*yaml.Node {
	fields := make(map[string]*yaml.Node)
	if n != nil && n.Kind == yaml.MappingNode {
		for i := 0; i+1 < len(n.Content); i += 2 {
			fields[n.Content[i].Value] = n.Content[i+1]
		}
	}
	return fields
}

func validYAMLTree(n *yaml.Node, depth int, count *int) bool {
	*count += 1
	if depth > 16 || *count > 8192 || n.Kind == yaml.AliasNode || n.Anchor != "" {
		return false
	}
	if n.Kind == yaml.MappingNode {
		seen := make(map[string]bool)
		for i := 0; i+1 < len(n.Content); i += 2 {
			key := n.Content[i]
			if key.Kind != yaml.ScalarNode || seen[key.Value] {
				return false
			}
			seen[key.Value] = true
		}
	}
	for _, child := range n.Content {
		if !validYAMLTree(child, depth+1, count) {
			return false
		}
	}
	return true
}

func appendNative(result *parsedSubscription, public SubscriptionNode, raw string, seen map[string]bool) {
	u, err := parseProxy(raw)
	if err != nil || u == nil {
		public.Reason = "invalid_node"
		result.nodes = append(result.nodes, public)
		return
	}
	if _, err := subscriptionProxy(raw); err != nil {
		public.Reason = "unsafe_proxy_address"
		result.nodes = append(result.nodes, public)
		return
	}
	u.Path = ""
	u.Host = strings.ToLower(u.Host)
	normalized := u.String()
	if seen[normalized] {
		public.Reason = "duplicate_node"
		result.nodes = append(result.nodes, public)
		return
	}
	seen[normalized] = true
	public.Supported = true
	public.Reason = "native_proxy"
	result.nodes = append(result.nodes, public)
	result.configs = append(result.configs, nodeConfig{Name: public.Name, ProxyURL: normalized, Enabled: true, Imported: true, subscriptionIndex: public.Index})
}

func parseURIList(raw, format string) (parsedSubscription, error) {
	result := parsedSubscription{format: format}
	seen := make(map[string]bool)
	for _, line := range strings.Split(raw, "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		if len(result.nodes) >= 256 {
			return parsedSubscription{}, errors.New("subscription_limit")
		}
		if len(line) > 8192 {
			return parsedSubscription{}, errors.New("subscription_format_unsupported")
		}
		parts := strings.SplitN(line, "://", 2)
		if len(parts) != 2 {
			return parsedSubscription{}, errors.New("subscription_format_unsupported")
		}
		protocol := strings.ToLower(parts[0])
		name := ""
		if _, fragment, ok := strings.Cut(parts[1], "#"); ok {
			name, _ = url.PathUnescape(fragment)
		}
		public := SubscriptionNode{Index: len(result.nodes), Name: safeNodeName(name, len(result.nodes)), Protocol: safeProtocol(protocol), Reason: "unsupported_protocol"}
		if requiresMihomo(protocol) {
			public.Reason = "requires_mihomo"
			result.nodes = append(result.nodes, public)
			continue
		}
		if protocol != "http" && protocol != "https" && protocol != "socks5" && protocol != "socks5h" {
			result.nodes = append(result.nodes, public)
			continue
		}
		u, err := url.Parse(line)
		if err != nil {
			public.Reason = "invalid_node"
			result.nodes = append(result.nodes, public)
			continue
		}
		u.Fragment = ""
		u.RawFragment = ""
		appendNative(&result, public, u.String(), seen)
	}
	if len(result.nodes) == 0 {
		return parsedSubscription{}, errors.New("subscription_format_unsupported")
	}
	return result, nil
}
