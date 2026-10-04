package hub

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDeviceMetadataDefaultBudgetBoundsPublicEnrollment(t *testing.T) {
	h := newDeviceHub(t, Config{ResourceMode: "economy"})
	secret := strings.Repeat("a", 64)
	projects, _ := json.Marshal(strings.Repeat("x", 60<<10))
	for i := 0; i < 200; i++ {
		id := fmt.Sprintf("metadata-pc-%d", i)
		info := Device{DeviceID: id, Projects: projects, Tools: json.RawMessage(`[]`)}
		w := deviceRequest(t, h, "/device/register", id, secret, "", info, fmt.Sprintf("192.0.%d.%d", i/250+2, i%250+1))
		if w.Code == http.StatusTooManyRequests {
			if len(h.accounts) != i || h.deviceMetadataBytes > maxDeviceMetadataBytes {
				t.Fatal("rejection left an account or exceeded the retained metadata budget")
			}
			return
		}
		if w.Code != http.StatusOK {
			t.Fatalf("valid bounded fixture rejected: %d %s", w.Code, w.Body.String())
		}
	}
	t.Fatal("public enrollment retained metadata beyond the economy-compatible global budget")
}

func TestMetadataCapacityRejectionPreservesIdentityPublicationAndDisk(t *testing.T) {
	h := newDeviceHub(t, Config{DataDir: t.TempDir(), ResourceMode: "economy"})
	id, secret := "kept-pc", strings.Repeat("b", 64)
	enrollDevice(t, h, id, secret)
	dev := onlineDevice(t, h, id)
	h.mu.Lock()
	dev.conn = nil // keep the saver fixture stable rather than emitting changing online timestamps
	a := h.accounts[deviceAccount(id)]
	tokenHash := sha256.Sum256([]byte("test-only-pair-identity"))
	h.setPairLocked(a.id, dev, tokenHash, true)
	dev.lastSeen = 1234
	before := dev.info
	sub := &appSub{ch: make(chan sseMsg, 4), closed: make(chan struct{}), deviceID: id}
	a.apps[sub] = struct{}{}
	used := h.deviceMetadataBytes
	h.deviceMetadataLimit = used
	h.mu.Unlock()
	if err := h.saveNowError(); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(h.cfg.DataDir, devicesFile)
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	changed := before
	changed.Projects = json.RawMessage(`[{"path":"larger rejected metadata"}]`)
	// A brand-new computer is refused at capacity.
	if w := deviceRequest(t, h, "/device/register", "new-pc", secret, "", Device{DeviceID: "new-pc", Tools: json.RawMessage(`[]`)}, ""); w.Code != http.StatusTooManyRequests {
		t.Fatalf("capacity request: %d %s", w.Code, w.Body.String())
	}
	// The existing computer keeps connecting: its grown metadata is deferred, not a lockout.
	if w := deviceRequest(t, h, "/device/register", id, secret, "", changed, ""); w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"metadataDeferred":true`) {
		t.Fatalf("existing identity locked out by grown metadata at capacity: %d %s", w.Code, w.Body.String())
	}
	h.mu.Lock()
	if h.deviceMetadataBytes != used || len(h.accounts) != 1 || dev.lastSeen != 1234 || dev.info.Name != before.Name || !bytes.Equal(dev.info.Projects, before.Projects) || !dev.hasSecret || !dev.hasPair || dev.pairHash != tokenHash || len(sub.ch) != 0 {
		t.Fatal("capacity rejection changed retained metadata, identity, timestamp or publication")
	}
	h.mu.Unlock()
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, saved) {
		t.Fatal("capacity rejection changed the persistent identity state")
	}
	// The original secret still re-registers at the limit; an impostor never
	// receives capacity rejection in place of the existing authorization check.
	if w := deviceRequest(t, h, "/device/register", id, strings.Repeat("c", 64), "", before, ""); w.Code != 403 {
		t.Fatal("metadata budget changed existing identity authorization")
	}
	if w := deviceRequest(t, h, "/device/register", id, secret, "", before, ""); w.Code != 200 {
		t.Fatal("valid existing identity locked out at capacity")
	}
	if h.deviceMetadataBytes != used {
		t.Fatal("replacement counted the same metadata twice")
	}
	if err := h.saveNowError(); err != nil {
		t.Fatal(err)
	}
	h.Close()
	restored := newDeviceHub(t, Config{DataDir: h.cfg.DataDir, ResourceMode: "economy"})
	if restored.deviceMetadataBytes != used || !restored.accounts[a.id].devices[id].hasPair {
		t.Fatal("restart lost metadata accounting or the existing pair identity")
	}
}

func TestMetadataReplacementReleasesProjectedCharge(t *testing.T) {
	h := newDeviceHub(t, Config{ResourceMode: "economy"})
	secret := strings.Repeat("d", 64)
	large := Device{DeviceID: "resize-pc", Projects: json.RawMessage(`[{"name":"a larger project entry"}]`)}
	if w := deviceRequest(t, h, "/device/register", large.DeviceID, secret, "", large, ""); w.Code != 200 {
		t.Fatal("initial metadata fixture rejected")
	}
	used := h.deviceMetadataBytes
	h.deviceMetadataLimit = used
	small := Device{DeviceID: large.DeviceID, Projects: json.RawMessage(`[]`)}
	if w := deviceRequest(t, h, "/device/register", small.DeviceID, secret, "", small, ""); w.Code != 200 || h.deviceMetadataBytes >= used {
		t.Fatal("shrinking authenticated metadata did not release capacity")
	}
	if w := deviceRequest(t, h, "/device/register", large.DeviceID, secret, "", large, ""); w.Code != 200 || h.deviceMetadataBytes != used {
		t.Fatal("projected replacement incorrectly added the old metadata twice")
	}
}

func TestMetadataRejectsCollectionAndNestingAmplification(t *testing.T) {
	h := newDeviceHub(t, Config{ResourceMode: "economy"})
	for i, raw := range []string{
		"[" + strings.Repeat("{},", maxMetadataValues) + "{}]",
		strings.Repeat("[", maxMetadataDepth+2) + "0" + strings.Repeat("]", maxMetadataDepth+2),
		`[{"id":"first","id":"last"}]`,
	} {
		id := fmt.Sprintf("unsafe-metadata-%d", i)
		w := deviceRequest(t, h, "/device/register", id, strings.Repeat("e", 64), "", Device{DeviceID: id, Tools: json.RawMessage(raw)}, "")
		if w.Code != http.StatusBadRequest || len(h.accounts) != 0 || h.deviceMetadataBytes != 0 {
			t.Fatal("amplifying metadata created live identities")
		}
	}
}

func TestBoundedDeviceRestoreRefusesOversizeAndOverBudgetWithoutChangingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, devicesFile)
	st := persistedState{Version: 2, Accounts: map[string][]persistedDevice{"kept": {{Device: Device{DeviceID: "pc"}}}}}
	raw, err := json.Marshal(st)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	h := &Hub{cfg: Config{DataDir: dir}, deviceMetadataLimit: 1}
	if err := h.loadDevices(); err == nil || h.accounts != nil || h.deviceMetadataBytes != 0 {
		t.Fatal("over-budget restore partially loaded or silently dropped identities")
	}
	kept, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(kept, raw) {
		t.Fatal("failed restore changed the operator's file")
	}
	f, err := os.OpenFile(path, os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate(maxDevicesFileBytes + 1) // sparse fixture: never allocate or read the oversized content
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := h.loadDevices(); err == nil || h.accounts != nil {
		t.Fatal("oversized persisted file was loaded")
	}
	if info, err := os.Stat(path); err != nil || info.Size() != maxDevicesFileBytes+1 {
		t.Fatal("oversized failed restore changed the file")
	}
}

func TestDeviceRestoreChecksCollectionCapacityBeforeBulkDecode(t *testing.T) {
	entry := `{"device":{"deviceId":"pc"}}`
	for _, raw := range []string{
		`{"version":2,"accounts":{"a":[` + strings.Repeat(entry+",", maxDevices) + entry + `]}}`,
		`{"version":2,"accounts":{},"adminAudit":[` + strings.Repeat("{},", 200) + `{}]}`,
		`{"version":2,"accounts":{"a":[` + entry + "," + entry + `]}}`,
	} {
		if _, _, err := decodeDevicesState([]byte(raw), maxDeviceMetadataBytes); err == nil {
			t.Fatal("unbounded or ambiguous stored collection accepted")
		}
	}
}

func TestMetadataBudgetLeavesRoomForWorstCaseSavedEscaping(t *testing.T) {
	h := newDeviceHub(t, Config{ResourceMode: "economy"})
	reason := strings.Repeat("<", 256) // JSON escapes each byte to six bytes
	secret := sha256.Sum256([]byte("fixture identity"))
	h.mu.Lock()
	for i := 0; ; i++ {
		acct := fmt.Sprintf("account-%d", i/maxDevices)
		info := Device{DeviceID: fmt.Sprintf("%06d", i) + strings.Repeat("<", 120), Name: "<"}
		_, status, _ := h.registerDeviceLocked(acct, info, secret)
		if status == http.StatusTooManyRequests {
			break
		}
		if status != 0 {
			t.Fatal("bounded registration fixture failed")
		}
		dev := h.accounts[acct].devices[info.DeviceID]
		dev.banned, dev.banReason, dev.bannedAt = true, reason, 9999999999999
		dev.hasPair, dev.pairHash = true, sha256.Sum256([]byte(info.DeviceID))
		dev.lastSeen = 9999999999999
	}
	for i := 0; i < 200; i++ {
		h.adminAudit = append(h.adminAudit, AdminAudit{At: 9999999999999, Action: "disconnect", DeviceRef: strings.Repeat("a", 64), Reason: reason, Actor: "standalone-admin"})
	}
	h.mu.Unlock()
	raw, err := json.Marshal(h.snapshot())
	if err != nil || len(raw) > maxDevicesFileBytes {
		t.Fatalf("admitted metadata cannot fit its saved-file limit: %d %v", len(raw), err)
	}
	_, charge, err := decodeDevicesState(raw, maxDeviceMetadataBytes)
	if err != nil || charge != h.deviceMetadataBytes {
		t.Fatalf("admitted metadata cannot restore with the same accounting: %d %d %v", charge, h.deviceMetadataBytes, err)
	}
	t.Logf("full budget fixture: charged=%d bytes, compact saved state=%d bytes; restart charge unchanged", charge, len(raw))
}

func TestRawMetadataNormalizationKeepsRestartChargeAndJSONValues(t *testing.T) {
	info := Device{DeviceID: "canonical-pc", Tools: json.RawMessage(` [ { "name": "<script>&", "count": 1e1000 } ] `), Projects: json.RawMessage(` null `)}
	prepared, before, err := prepareDeviceMetadata(info)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Projects != nil || !bytes.Contains(prepared.Tools, []byte(`\u003c`)) || !bytes.Contains(prepared.Tools, []byte(`1e1000`)) {
		t.Fatal("opaque JSON semantics or canonical null/escaping changed")
	}
	raw, err := json.Marshal(prepared)
	if err != nil {
		t.Fatal(err)
	}
	var restored Device
	if err := json.Unmarshal(raw, &restored); err != nil {
		t.Fatal(err)
	}
	_, after, err := prepareDeviceMetadata(restored)
	if err != nil || before != after {
		t.Fatal("raw JSON encoding changed metadata admission charge after restart")
	}
}

func TestNewRejectsOversizeStateBeforeStartingSaver(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, devicesFile)
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	err = f.Truncate(maxDevicesFileBytes + 1)
	f.Close()
	if err != nil {
		t.Fatal(err)
	}
	if h, err := New(Config{DataDir: dir, ResourceMode: "economy"}); err == nil || h != nil {
		if h != nil {
			h.Close()
		}
		t.Fatal("New accepted an oversized saved device state")
	}
	if info, err := os.Stat(path); err != nil || info.Size() != maxDevicesFileBytes+1 {
		t.Fatal("failed New overwrote the original saved device state")
	}
	files, err := os.ReadDir(dir)
	if err != nil || len(files) != 1 || files[0].Name() != devicesFile {
		t.Fatal("failed New started persistence or initialized a replacement state")
	}
}

func TestPersistedDeviceV1AndV2KeepValidCredentialsAndOpaqueMetadata(t *testing.T) {
	for _, version := range []int{1, 2} {
		t.Run(fmt.Sprint(version), func(t *testing.T) {
			dir := t.TempDir()
			secretHash, pairHash := strings.Repeat("ab", 32), strings.Repeat("cd", 32)
			st := persistedState{Version: version, Accounts: map[string][]persistedDevice{"legacy-account": {{
				Device: Device{DeviceID: "legacy-pc", Tools: json.RawMessage(`[{"futureToolField":{"number":1e1000}}]`)}, SecretHash: secretHash, PairHash: pairHash,
			}}}}
			raw, err := json.Marshal(st)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, devicesFile), raw, 0600); err != nil {
				t.Fatal(err)
			}
			h := newDeviceHub(t, Config{DataDir: dir, ResourceMode: "economy"})
			dev := h.accounts["legacy-account"].devices["legacy-pc"]
			if !dev.hasSecret || !dev.hasPair || len(h.pairTokens) != 1 || !bytes.Contains(dev.info.Tools, []byte("futureToolField")) {
				t.Fatal("supported old state lost credentials or opaque forward-compatible metadata")
			}
			h.Close()
			restarted := newDeviceHub(t, Config{DataDir: dir, ResourceMode: "economy"})
			if restarted.deviceMetadataBytes != h.deviceMetadataBytes || !restarted.accounts["legacy-account"].devices["legacy-pc"].hasPair {
				t.Fatal("v1/v2 persistence upgrade lost metadata charge or credentials")
			}
		})
	}
}

func TestInvalidSavedVersionNestedFieldsAndHashesFailClosedWithoutOverwrite(t *testing.T) {
	wrap := func(record string) string { return `{"version":2,"accounts":{"account":[` + record + `]}}` }
	for name, raw := range map[string]string{
		"unsupported version":  `{"version":3,"accounts":{}}`,
		"missing version":      `{"accounts":{}}`,
		"duplicate version":    `{"version":1,"version":2,"accounts":{}}`,
		"unknown top field":    `{"version":2,"accounts":{},"futureSchema":true}`,
		"unknown record field": wrap(`{"device":{"deviceId":"pc"},"futureCredential":"bad"}`),
		"unknown device field": wrap(`{"device":{"deviceId":"pc","futureIdentity":"bad"}}`),
		"cased field alias":    wrap(`{"device":{"deviceId":"pc"},"SecretHash":""}`),
		"duplicate record":     wrap(`{"device":{"deviceId":"pc"},"pairHash":"","pairHash":""}`),
		"duplicate device key": wrap(`{"device":{"deviceId":"pc","deviceId":"other"}}`),
		"duplicate opaque key": wrap(`{"device":{"deviceId":"pc","tools":[{"id":"a","id":"b"}]}}`),
		"broken secret hash":   wrap(`{"device":{"deviceId":"pc"},"secretHash":"` + strings.Repeat("a", 63) + `"}`),
		"broken pair hash":     wrap(`{"device":{"deviceId":"pc"},"pairHash":"` + strings.Repeat("g", 64) + `"}`),
		"null secret hash":     wrap(`{"device":{"deviceId":"pc"},"secretHash":null}`),
		"invalid identity":     wrap(`{"device":{"deviceId":""}}`),
		"unknown audit field":  `{"version":2,"accounts":{},"adminAudit":[{"privateField":true}]}`,
		"duplicate audit key":  `{"version":2,"accounts":{},"adminAudit":[{"actor":"host-admin","actor":"other"}]}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			path := filepath.Join(dir, devicesFile)
			if err := os.WriteFile(path, []byte(raw), 0600); err != nil {
				t.Fatal(err)
			}
			if h, err := New(Config{DataDir: dir, ResourceMode: "economy"}); err == nil || h != nil {
				if h != nil {
					h.Close()
				}
				t.Fatal("ambiguous or damaged saved state silently loaded")
			}
			after, err := os.ReadFile(path)
			if err != nil || !bytes.Equal(after, []byte(raw)) {
				t.Fatal("failed initialization replaced the original saved identities")
			}
			files, err := os.ReadDir(dir)
			if err != nil || len(files) != 1 {
				t.Fatal("failed initialization started a persistence writer")
			}
		})
	}
}
