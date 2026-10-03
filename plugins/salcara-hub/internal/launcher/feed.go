package launcher

import (
	"bytes"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"regexp"
	"strconv"
	"strings"
)

type Binary struct {
	URL    string `json:"url"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size_bytes"`
}
type Feed struct {
	Product          string            `json:"product"`
	Version          string            `json:"version"`
	LauncherProtocol int               `json:"launcher_protocol"`
	DataSchema       int               `json:"data_schema"`
	ReleaseNotes     string            `json:"release_notes"`
	Binaries         map[string]Binary `json:"binaries"`
}
type envelope struct {
	Schema    int    `json:"schema_version"`
	Publisher string `json:"publisher_key_id"`
	Payload   string `json:"payload"`
	Signature string `json:"signature"`
}

var hashPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var versionPattern = regexp.MustCompile(`^(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-([A-Za-z0-9]+(?:[.-][A-Za-z0-9]+)*))?$`)

type semanticVersion struct {
	numbers    [3]uint64
	prerelease []string
}

func parseVersion(value string) (semanticVersion, error) {
	var out semanticVersion
	if len(value) > 96 {
		return out, errors.New("version is too long")
	}
	m := versionPattern.FindStringSubmatch(value)
	if m == nil {
		return out, errors.New("a bare semantic version is required")
	}
	for i := range out.numbers {
		n, err := strconv.ParseUint(m[i+1], 10, 32)
		if err != nil {
			return out, errors.New("version component is too large")
		}
		out.numbers[i] = n
	}
	if m[4] != "" {
		out.prerelease = strings.Split(m[4], ".")
		for _, item := range out.prerelease {
			digits := true
			for _, ch := range item {
				if ch < '0' || ch > '9' {
					digits = false
					break
				}
			}
			if digits {
				if len(item) > 1 && item[0] == '0' {
					return out, errors.New("invalid numeric prerelease")
				}
				if _, err := strconv.ParseUint(item, 10, 64); err != nil {
					return out, errors.New("numeric prerelease is too large")
				}
			}
		}
	}
	return out, nil
}
func newer(latest, current string) bool {
	a, ea := parseVersion(latest)
	b, eb := parseVersion(current)
	if ea != nil || eb != nil {
		return false
	}
	for i := range a.numbers {
		if a.numbers[i] != b.numbers[i] {
			return a.numbers[i] > b.numbers[i]
		}
	}
	if len(a.prerelease) == 0 || len(b.prerelease) == 0 {
		return len(a.prerelease) == 0 && len(b.prerelease) != 0
	}
	for i := 0; i < len(a.prerelease) && i < len(b.prerelease); i++ {
		if a.prerelease[i] == b.prerelease[i] {
			continue
		}
		na, ea := strconv.ParseUint(a.prerelease[i], 10, 64)
		nb, eb := strconv.ParseUint(b.prerelease[i], 10, 64)
		if ea == nil && eb == nil {
			return na > nb
		}
		if ea == nil {
			return false
		}
		if eb == nil {
			return true
		}
		return a.prerelease[i] > b.prerelease[i]
	}
	return len(a.prerelease) > len(b.prerelease)
}

func strictDecode(data []byte, target any) error {
	// Detect duplicate keys before the ordinary strict decoder can collapse them.
	tokens := json.NewDecoder(bytes.NewReader(data))
	var value func(int) error
	value = func(depth int) error {
		if depth > 64 {
			return errors.New("JSON nesting exceeds limit")
		}
		t, err := tokens.Token()
		if err != nil {
			return err
		}
		d, isDelimiter := t.(json.Delim)
		if !isDelimiter {
			return nil
		}
		switch d {
		case '{':
			seen := map[string]bool{}
			for tokens.More() {
				key, err := tokens.Token()
				if err != nil {
					return err
				}
				name, ok := key.(string)
				if !ok || seen[name] {
					return errors.New("duplicate JSON key")
				}
				seen[name] = true
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		case '[':
			for tokens.More() {
				if err := value(depth + 1); err != nil {
					return err
				}
			}
		default:
			return errors.New("invalid JSON delimiter")
		}
		_, err = tokens.Token()
		return err
	}
	if err := value(0); err != nil {
		return err
	}
	if _, err := tokens.Token(); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(target); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		return errors.New("trailing JSON data")
	}
	return nil
}

func verifyFeed(data []byte, key ed25519.PublicKey) (Feed, error) {
	var f Feed
	var e envelope
	if len(data) == 0 || len(data) > maxFeedBytes || strictDecode(data, &e) != nil || e.Schema != 1 || e.Publisher != PublisherID {
		return f, errors.New("更新源格式或发布者身份无效")
	}
	payload, err := base64.StdEncoding.Strict().DecodeString(e.Payload)
	if err != nil || len(payload) == 0 || len(payload) > maxFeedBytes {
		return f, errors.New("更新清单编码无效")
	}
	sig, err := base64.StdEncoding.Strict().DecodeString(e.Signature)
	if err != nil || len(sig) != ed25519.SignatureSize || !ed25519.Verify(key, payload, sig) {
		return f, errors.New("更新清单签名验证失败；不会停止现有服务")
	}
	if strictDecode(payload, &f) != nil {
		return Feed{}, errors.New("签名清单包含未知字段或无效 JSON")
	}
	if _, err := parseVersion(f.Version); err != nil || f.Product != Product || f.LauncherProtocol != 1 || f.DataSchema != 1 || len(f.ReleaseNotes) > 4096 || len(f.Binaries) != 2 {
		return Feed{}, errors.New("版本、启动协议或数据格式不兼容")
	}
	for _, platform := range []string{"linux/amd64", "linux/arm64"} {
		b, ok := f.Binaries[platform]
		if !ok || !hashPattern.MatchString(b.SHA256) || b.Size < 1 || b.Size > maxBinaryBytes {
			return Feed{}, errors.New("更新二进制大小或哈希无效")
		}
		if _, err := validateURL(b.URL); err != nil {
			return Feed{}, errors.New("更新二进制地址必须为公网 HTTPS 443")
		}
	}
	return f, nil
}
