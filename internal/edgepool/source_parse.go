package edgepool

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net"
	"net/url"
	"strconv"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Only a versioned subset of outbound fields is admitted. Provider documents
// never control listeners, DNS, TUN, paths, downloads, scripts or proxy chaining.
func parseSourceNodes(body []byte) ([]sourceNode, error) {
	parsed, err := parseSubscription(body)
	if err != nil {
		return nil, err
	}
	native := map[int]nodeConfig{}
	for _, n := range parsed.configs {
		native[n.subscriptionIndex] = n
	}
	managed := map[int]map[string]any{}
	managedErrors := map[int]string{}
	raw := strings.TrimSpace(strings.TrimPrefix(string(body), "\ufeff"))
	if parsed.format == "base64_uri_list" {
		decoded, _ := decodeSubscriptionBase64(raw)
		raw = string(decoded)
	}
	if parsed.format == "clash_yaml" || parsed.format == "clash_json" {
		var doc yaml.Node
		if yaml.Unmarshal([]byte(raw), &doc) == nil && len(doc.Content) == 1 {
			root := doc.Content[0]
			list := root
			if root.Kind == yaml.MappingNode {
				list = yamlFields(root)["proxies"]
			}
			if list != nil {
				for i, n := range list.Content {
					var v map[string]any
					if n.Decode(&v) == nil {
						managed[i] = v
					}
				}
			}
		}
	} else {
		index := 0
		for _, line := range strings.Split(raw, "\n") {
			line = strings.TrimSpace(line)
			if line == "" {
				continue
			}
			if v, e := parseManagedURI(line); e == nil {
				managed[index] = v
			} else {
				managedErrors[index] = e.Error()
			}
			index++
		}
	}
	out := make([]sourceNode, 0, len(parsed.nodes))
	for _, public := range parsed.nodes {
		n := sourceNode{SourceNodeView: SourceNodeView{Name: public.Name, Protocol: public.Protocol, Supported: public.Supported, Reason: public.Reason}}
		if config, ok := native[public.Index]; ok {
			n.Native = config.ProxyURL
			n.Key = sourceNodeKey(config.ProxyURL)
		} else if proxy, ok := managed[public.Index]; ok {
			clean, reason := normalizeKernelProxy(proxy)
			if reason == "" {
				n.Proxy = clean
				n.Key = managedNodeKey(clean)
				n.Supported = true
				n.Reason = "managed_kernel"
			} else {
				n.Reason = reason
			}
		} else if reason := managedErrors[public.Index]; reason != "" {
			n.Reason = reason
		}
		out = append(out, n)
	}
	return out, nil
}
func managedNodeKey(v map[string]any) string {
	identity := map[string]any{}
	for _, key := range []string{"type", "server", "port", "uuid", "username"} {
		if value, ok := v[key]; ok {
			identity[key] = value
		}
	}
	raw, _ := json.Marshal(identity)
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:16])
}
func normalizeKernelProxy(v map[string]any) (map[string]any, string) {
	protocol, _ := v["type"].(string)
	protocol = strings.ToLower(protocol)
	if protocol == "hy2" {
		protocol = "hysteria2"
	}
	if protocol != "ss" && protocol != "trojan" && protocol != "vmess" && protocol != "vless" && protocol != "hysteria2" {
		return nil, "unsupported_protocol"
	}
	allowed := map[string]bool{}
	for _, key := range []string{"name", "type", "server", "port", "password", "username", "uuid", "cipher", "alterId", "tls", "servername", "sni", "network", "ws-opts", "grpc-opts", "reality-opts", "client-fingerprint", "alpn", "flow", "udp", "skip-cert-verify", "tfo", "udp-over-tcp", "udp-over-tcp-version", "obfs", "obfs-password", "up", "down"} {
		allowed[key] = true
	}
	for key := range v {
		if !allowed[key] {
			return nil, "unsupported_option"
		}
	}
	for _, key := range []string{"username", "uuid", "password", "cipher", "alterId", "flow", "obfs", "obfs-password", "network", "ws-opts", "grpc-opts", "reality-opts", "sni", "servername"} {
		if _, exists := v[key]; !exists {
			continue
		}
		valid := true
		switch key {
		case "username":
			valid = false
		case "uuid":
			valid = protocol == "vmess" || protocol == "vless"
		case "password":
			valid = protocol == "ss" || protocol == "trojan" || protocol == "hysteria2"
		case "cipher":
			valid = protocol == "ss" || protocol == "vmess"
		case "alterId":
			valid = protocol == "vmess"
		case "flow", "reality-opts":
			valid = protocol == "vless"
		case "obfs", "obfs-password":
			valid = protocol == "hysteria2"
		case "sni":
			valid = protocol == "trojan" || protocol == "hysteria2"
		case "servername":
			valid = protocol == "vmess" || protocol == "vless"
		case "network", "ws-opts", "grpc-opts":
			valid = protocol == "vmess" || protocol == "vless" || protocol == "trojan"
		}
		if !valid {
			return nil, "unsupported_option"
		}
	}
	server, ok := v["server"].(string)
	if !ok || server == "" || strings.ContainsAny(server, "/@?#\\\r\n\t ") {
		return nil, "invalid_node"
	}
	port := 0
	switch p := v["port"].(type) {
	case int:
		port = p
	case float64:
		if p == float64(int(p)) {
			port = int(p)
		}
	case string:
		port, _ = strconv.Atoi(p)
	}
	if port < 1 || port > 65535 {
		return nil, "invalid_node"
	}
	if _, err := subscriptionProxy("socks5://" + net.JoinHostPort(server, strconv.Itoa(port))); err != nil {
		return nil, "unsafe_proxy_address"
	}
	if skip, exists := v["skip-cert-verify"]; exists && skip != false {
		return nil, "insecure_tls"
	}
	clean := map[string]any{"type": protocol, "server": strings.ToLower(server), "port": port, "udp": false}
	for _, key := range []string{"password", "username", "uuid", "cipher", "servername", "sni", "client-fingerprint", "flow", "obfs", "obfs-password"} {
		if value, exists := v[key]; exists {
			s, ok := value.(string)
			if !ok || len(s) > 4096 || strings.ContainsAny(s, "\r\n\x00") {
				return nil, "invalid_node"
			}
			clean[key] = s
		}
	}
	if protocol == "ss" || protocol == "trojan" || protocol == "hysteria2" {
		if password, _ := clean["password"].(string); password == "" {
			return nil, "invalid_node"
		}
	} else {
		uuid, _ := clean["uuid"].(string)
		if len(uuid) != 36 {
			return nil, "invalid_node"
		}
	}
	if protocol == "ss" {
		cipher, _ := clean["cipher"].(string)
		if cipher == "" {
			return nil, "invalid_node"
		}
	}
	if value, exists := v["tls"]; exists {
		if protocol == "ss" || (protocol == "trojan" || protocol == "hysteria2") && value != true {
			return nil, "unsupported_option"
		}
		if _, ok := value.(bool); !ok {
			return nil, "invalid_node"
		}
		clean["tls"] = value
	}
	if protocol == "trojan" || protocol == "hysteria2" {
		clean["skip-cert-verify"] = false
	}
	if value, exists := v["alterId"]; exists {
		n, ok := value.(int)
		if !ok {
			if f, yes := value.(float64); yes && f == float64(int(f)) {
				n = int(f)
				ok = true
			}
		}
		if !ok || n < 0 || n > 65535 {
			return nil, "invalid_node"
		}
		clean["alterId"] = n
	}
	if a, exists := v["alpn"]; exists {
		raw, _ := json.Marshal(a)
		var names []string
		if json.Unmarshal(raw, &names) != nil || len(names) > 8 {
			return nil, "invalid_node"
		}
		for _, name := range names {
			if len(name) > 64 || strings.ContainsAny(name, "\r\n\x00") {
				return nil, "invalid_node"
			}
		}
		clean["alpn"] = names
	}
	network, _ := v["network"].(string)
	if value, exists := v["network"]; exists {
		if _, ok := value.(string); !ok {
			return nil, "invalid_node"
		}
	}
	if network != "" && network != "tcp" && network != "ws" && network != "grpc" {
		return nil, "unsupported_option"
	}
	if network != "" {
		clean["network"] = network
	}
	for _, key := range []string{"ws-opts", "grpc-opts", "reality-opts"} {
		if value, exists := v[key]; exists {
			if key == "ws-opts" && network != "ws" || key == "grpc-opts" && network != "grpc" || key == "reality-opts" && (protocol != "vless" || clean["tls"] != true) {
				return nil, "unsupported_option"
			}
			raw, e := json.Marshal(value)
			if e != nil || len(raw) > 8192 {
				return nil, "invalid_node"
			}
			var fields map[string]any
			if json.Unmarshal(raw, &fields) != nil {
				return nil, "invalid_node"
			}
			allowedFields := map[string]bool{}
			switch key {
			case "ws-opts":
				allowedFields["path"] = true
				allowedFields["headers"] = true
			case "grpc-opts":
				allowedFields["grpc-service-name"] = true
			case "reality-opts":
				allowedFields["public-key"] = true
				allowedFields["short-id"] = true
			}
			for field, val := range fields {
				if !allowedFields[field] {
					return nil, "unsupported_option"
				}
				if field == "headers" {
					headers, ok := val.(map[string]any)
					if !ok || len(headers) > 1 {
						return nil, "unsupported_option"
					}
					for k, h := range headers {
						value, ok := h.(string)
						if !strings.EqualFold(k, "host") || !ok || strings.ContainsAny(value, "\r\n\x00") || len(value) > 253 {
							return nil, "unsupported_option"
						}
					}
				} else {
					s, ok := val.(string)
					if !ok || len(s) > 2048 || strings.ContainsAny(s, "\r\n\x00") {
						return nil, "invalid_node"
					}
				}
			}
			clean[key] = fields
		}
	}
	// Reject fields that were recognized but are not implemented by this adapter.
	for _, key := range []string{"tfo", "udp-over-tcp", "udp-over-tcp-version", "up", "down"} {
		if _, exists := v[key]; exists {
			return nil, "unsupported_option"
		}
	}
	return clean, ""
}
func parseManagedURI(raw string) (map[string]any, error) {
	protocol, rest, ok := strings.Cut(raw, "://")
	if !ok {
		return nil, errors.New("invalid_node")
	}
	protocol = strings.ToLower(protocol)
	if protocol == "vmess" {
		payload, _, _ := strings.Cut(rest, "#")
		decoded, ok := decodeSubscriptionBase64(payload)
		if !ok {
			return nil, errors.New("invalid_node")
		}
		var v map[string]any
		if json.Unmarshal(decoded, &v) != nil {
			return nil, errors.New("invalid_node")
		}
		allowed := map[string]bool{}
		for _, k := range []string{"v", "ps", "add", "port", "id", "aid", "scy", "net", "type", "host", "path", "tls", "sni", "alpn", "fp"} {
			allowed[k] = true
		}
		for k, value := range v {
			if !allowed[k] {
				return nil, errors.New("unsupported_option")
			}
			if k != "port" && k != "aid" && k != "v" {
				if _, ok := value.(string); !ok {
					return nil, errors.New("invalid_node")
				}
			}
		}
		if version, exists := v["v"]; exists && version != "2" && version != float64(2) {
			return nil, errors.New("unsupported_option")
		}
		if header, _ := v["type"].(string); header != "" && header != "none" {
			return nil, errors.New("unsupported_option")
		}
		if tls, _ := v["tls"].(string); tls != "" && tls != "none" && tls != "tls" {
			return nil, errors.New("unsupported_option")
		}
		network, _ := v["net"].(string)
		if network != "" && network != "tcp" && network != "ws" {
			return nil, errors.New("unsupported_option")
		}
		if network != "ws" && (v["path"] != nil && v["path"] != "" || v["host"] != nil && v["host"] != "") {
			return nil, errors.New("unsupported_option")
		}
		proxy := map[string]any{"type": "vmess", "server": v["add"], "port": v["port"], "uuid": v["id"], "cipher": "auto"}
		if aid, exists := v["aid"]; exists {
			n := -1
			switch a := aid.(type) {
			case string:
				parsed, e := strconv.Atoi(a)
				if e == nil {
					n = parsed
				}
			case float64:
				if a >= 0 && a <= 65535 && a == float64(int(a)) {
					n = int(a)
				}
			}
			if n < 0 || n > 65535 {
				return nil, errors.New("invalid_node")
			}
			proxy["alterId"] = n
		}
		if cipher, _ := v["scy"].(string); cipher != "" {
			proxy["cipher"] = cipher
		}
		if fp, _ := v["fp"].(string); fp != "" {
			proxy["client-fingerprint"] = fp
		}
		if alpn, _ := v["alpn"].(string); alpn != "" {
			proxy["alpn"] = strings.Split(alpn, ",")
		}
		if v["tls"] == "tls" {
			proxy["tls"] = true
		}
		if s, ok := v["sni"].(string); ok && s != "" {
			proxy["servername"] = s
		}
		if network, ok := v["net"].(string); ok && network != "" {
			proxy["network"] = network
			if network == "ws" {
				opts := map[string]any{}
				if path, ok := v["path"].(string); ok {
					opts["path"] = path
				}
				if host, ok := v["host"].(string); ok && host != "" {
					opts["headers"] = map[string]any{"Host": host}
				}
				proxy["ws-opts"] = opts
			}
		}
		return proxy, nil
	}
	if protocol == "ss" && !strings.Contains(rest, "@") {
		encoded, fragment, _ := strings.Cut(rest, "#")
		decoded, ok := decodeSubscriptionBase64(encoded)
		if !ok {
			return nil, errors.New("invalid_node")
		}
		raw = "ss://" + string(decoded)
		if fragment != "" {
			raw += "#" + fragment
		}
	}
	u, err := url.Parse(raw)
	if err != nil || u.User == nil || u.Hostname() == "" {
		return nil, errors.New("invalid_node")
	}
	if u.Path != "" && u.Path != "/" {
		return nil, errors.New("unsupported_option")
	}
	if protocol != "ss" {
		if _, present := u.User.Password(); present {
			return nil, errors.New("unsupported_option")
		}
	}
	port, err := strconv.Atoi(u.Port())
	if err != nil {
		return nil, errors.New("invalid_node")
	}
	v := map[string]any{"type": protocol, "server": u.Hostname(), "port": port}
	if protocol == "ss" {
		user := u.User.Username()
		password, ok := u.User.Password()
		if !ok {
			decoded, valid := decodeSubscriptionBase64(user)
			if !valid {
				return nil, errors.New("invalid_node")
			}
			user, password, ok = strings.Cut(string(decoded), ":")
			if !ok {
				return nil, errors.New("invalid_node")
			}
		}
		v["cipher"] = user
		v["password"] = password
	} else if protocol == "trojan" || protocol == "hysteria2" || protocol == "hy2" {
		v["password"] = u.User.Username()
	} else if protocol == "vless" {
		v["uuid"] = u.User.Username()
	} else {
		return nil, errors.New("unsupported_protocol")
	}
	query, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return nil, errors.New("invalid_node")
	}
	if query.Has("sni") && query.Has("peer") {
		return nil, errors.New("unsupported_option")
	}
	for key, values := range query {
		if len(values) != 1 {
			return nil, errors.New("invalid_node")
		}
		switch key {
		case "sni", "peer":
			if protocol == "vless" {
				v["servername"] = values[0]
			} else {
				v["sni"] = values[0]
			}
		case "security":
			if protocol == "ss" || (protocol == "trojan" || protocol == "hysteria2" || protocol == "hy2") && values[0] != "tls" {
				return nil, errors.New("unsupported_option")
			}
			if values[0] == "tls" || values[0] == "reality" {
				v["tls"] = true
			} else if values[0] != "none" {
				return nil, errors.New("unsupported_option")
			}
		case "type":
			v["network"] = values[0]
		case "flow":
			v["flow"] = values[0]
		case "encryption":
			if protocol != "vless" || values[0] != "none" {
				return nil, errors.New("unsupported_option")
			}
		case "fp":
			v["client-fingerprint"] = values[0]
		case "alpn":
			v["alpn"] = strings.Split(values[0], ",")
		case "path", "host", "serviceName", "pbk", "sid":
		case "allowInsecure", "insecure":
			if values[0] != "0" && values[0] != "false" {
				return nil, errors.New("insecure_tls")
			}
		default:
			return nil, errors.New("unsupported_option")
		}
	}
	if (query.Has("path") || query.Has("host")) && v["network"] != "ws" {
		return nil, errors.New("unsupported_option")
	}
	if query.Has("serviceName") && v["network"] != "grpc" {
		return nil, errors.New("unsupported_option")
	}
	if (query.Has("pbk") || query.Has("sid")) && query.Get("security") != "reality" {
		return nil, errors.New("unsupported_option")
	}
	if query.Get("security") == "reality" && (protocol != "vless" || query.Get("pbk") == "") {
		return nil, errors.New("unsupported_option")
	}
	if v["network"] == "ws" {
		opts := map[string]any{}
		if path := query.Get("path"); path != "" {
			opts["path"] = path
		}
		if host := query.Get("host"); host != "" {
			opts["headers"] = map[string]any{"Host": host}
		}
		v["ws-opts"] = opts
	}
	if v["network"] == "grpc" {
		v["grpc-opts"] = map[string]any{"grpc-service-name": query.Get("serviceName")}
	}
	if query.Get("security") == "reality" {
		v["reality-opts"] = map[string]any{"public-key": query.Get("pbk"), "short-id": query.Get("sid")}
	}
	return v, nil
}
