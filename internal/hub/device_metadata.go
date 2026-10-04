package hub

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
)

const (
	// Reserve room for event caches, command replies, serialization and the
	// launcher even in the 96 MiB economy mode. This is an admission budget,
	// not a promise about total RSS or the number of registered computers.
	maxDeviceMetadataBytes = 16 << 20
	maxDevicesFileBytes    = 16 << 20
	deviceMetadataReserve  = 2048
	maxMetadataDepth       = 32
	maxMetadataValues      = 4096
)

func accountMetadataCost(id string) int64 { return 1024 + 6*int64(len(id)) }

// Count JSON values before any administrative view can expand compact arrays
// into slices of structs. Raw bytes alone undercount collections such as [{}].
func metadataValues(raw []byte) (int64, error) {
	if len(raw) == 0 {
		return 0, nil
	}
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber() // count opaque numeric values without narrowing their JSON representation
	var values int64
	var walk func(int) error
	walk = func(depth int) error {
		values++
		if depth > maxMetadataDepth || values > maxMetadataValues {
			return errors.New("device metadata nesting or collection capacity exceeded")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		delim, ok := token.(json.Delim)
		if !ok {
			return nil
		}
		keys := map[string]bool{}
		for d.More() {
			if delim == '{' {
				key, err := d.Token()
				name, ok := key.(string)
				if err != nil || !ok || keys[name] {
					return errors.New("duplicate device metadata JSON field")
				}
				keys[name] = true
			}
			if err := walk(depth + 1); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	}
	if err := walk(0); err != nil {
		return 0, err
	}
	if _, err := d.Token(); err != io.EOF {
		return 0, errors.New("invalid trailing device metadata")
	}
	return values, nil
}

func prepareDeviceMetadata(info Device) (Device, int64, error) {
	if !validDeviceInfo(info) || len(info.Tools)+len(info.Projects) > maxDeviceBody {
		return Device{}, 0, errors.New("invalid or oversized device metadata")
	}
	// Charge and retain the same compact, HTML-escaped JSON representation
	// which Marshal saves. Otherwise escaping can increase raw-field charges
	// after restart, and a nil RawMessage would become a newly charged null.
	for _, field := range []*json.RawMessage{&info.Tools, &info.Projects} {
		canonical, err := json.Marshal(*field)
		if err != nil {
			return Device{}, 0, errors.New("invalid device metadata JSON")
		}
		if bytes.Equal(canonical, []byte("null")) {
			*field = nil
		} else {
			*field = canonical
		}
	}
	encoded, err := json.Marshal(info)
	if err != nil || len(encoded) > maxDeviceBody {
		return Device{}, 0, errors.New("invalid or oversized encoded device metadata")
	}
	tools, err := metadataValues(info.Tools)
	if err != nil {
		return Device{}, 0, err
	}
	projects, err := metadataValues(info.Projects)
	if err != nil {
		return Device{}, 0, err
	}
	// Raw and encoded copies, identity/map overhead, and conservatively charged
	// decoded collection slots all share the same global admission budget.
	raw := len(info.DeviceID) + len(info.Name) + len(info.OS) + len(info.Version) + len(info.Tools) + len(info.Projects)
	return info, int64(deviceMetadataReserve+raw+len(encoded)) + 128*(tools+projects), nil
}

// A typed decoder rejects unknown fields, while this token pass also rejects
// duplicate nested keys before json.Unmarshal can silently choose the last one.
func decodeStoredRecord(raw []byte, target any) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var fields map[string]bool
	switch target.(type) {
	case *persistedDevice:
		fields = map[string]bool{"device": true, "lastSeen": true, "secretHash": true, "pairHash": true, "banned": true, "banReason": true, "bannedAt": true}
	case *AdminAudit:
		fields = map[string]bool{"at": true, "action": true, "device_ref": true, "reason": true, "actor": true}
	}
	var walk func(int, map[string]bool, bool) error
	walk = func(depth int, allowed map[string]bool, stringRequired bool) error {
		if depth > maxMetadataDepth+8 {
			return errors.New("persisted device JSON nesting capacity exceeded")
		}
		token, err := d.Token()
		if err != nil {
			return err
		}
		if _, ok := token.(string); stringRequired && !ok {
			return errors.New("persisted device credential hash must be a string")
		}
		delim, ok := token.(json.Delim)
		if allowed != nil && (!ok || delim != '{') {
			return errors.New("persisted device record must be an object")
		}
		if !ok {
			return nil
		}
		keys := map[string]bool{}
		for d.More() {
			var childFields map[string]bool
			var childString bool
			if delim == '{' {
				key, err := d.Token()
				name, ok := key.(string)
				if err != nil || !ok || keys[name] {
					return errors.New("duplicate persisted device JSON field")
				}
				if allowed != nil && !allowed[name] {
					return errors.New("unknown persisted device JSON field")
				}
				keys[name] = true
				childString = depth == 0 && (name == "secretHash" || name == "pairHash")
				if depth == 0 && name == "device" {
					childFields = map[string]bool{"deviceId": true, "name": true, "os": true, "version": true, "tools": true, "projects": true}
				}
			}
			if err := walk(depth+1, childFields, childString); err != nil {
				return err
			}
		}
		_, err = d.Token()
		return err
	}
	if err := walk(0, fields, false); err != nil {
		return err
	}
	if _, err := d.Token(); err != io.EOF {
		return errors.New("trailing persisted device record")
	}
	d = json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	return d.Decode(target)
}

// Decode each bounded entry individually. Unmarshalling the entire file first
// would allocate huge slices for tiny [{}] entries before capacity checks run.
func decodeDevicesState(raw []byte, limit int64) (persistedState, int64, error) {
	st := persistedState{Accounts: map[string][]persistedDevice{}}
	d := json.NewDecoder(bytes.NewReader(raw))
	expect := func(want json.Delim) error {
		got, err := d.Token()
		if err != nil || got != want {
			return errors.New("invalid persisted device structure")
		}
		return nil
	}
	decodeBounded := func(maximum int, target any) error {
		var value json.RawMessage
		if err := d.Decode(&value); err != nil || len(value) > maximum {
			return errors.New("persisted device field capacity exceeded")
		}
		return decodeStoredRecord(value, target)
	}
	var charged int64
	if err := expect('{'); err != nil {
		return st, 0, err
	}
	seen := map[string]bool{}
	for d.More() {
		key, err := d.Token()
		name, ok := key.(string)
		if err != nil || !ok || seen[name] {
			return st, 0, errors.New("invalid or duplicate persisted device field")
		}
		seen[name] = true
		switch name {
		case "version":
			err = d.Decode(&st.Version)
		case "accounts":
			if err = expect('{'); err != nil {
				break
			}
			for d.More() && err == nil {
				key, e := d.Token()
				id, ok := key.(string)
				if e != nil || !ok || !validID(id) || len(st.Accounts) >= maxDeviceAccounts {
					err = errors.New("persisted device account capacity exceeded")
					break
				}
				if _, exists := st.Accounts[id]; exists {
					err = errors.New("duplicate persisted device account")
					break
				}
				charged += accountMetadataCost(id)
				if charged > limit {
					err = errors.New("persisted device metadata capacity exceeded")
					break
				}
				if err = expect('['); err != nil {
					break
				}
				devices := []persistedDevice{}
				ids := map[string]bool{}
				for d.More() && err == nil {
					if len(devices) >= maxDevices {
						err = errors.New("persisted device capacity exceeded")
						break
					}
					var pd persistedDevice
					if err = decodeBounded(maxDeviceBody+4096, &pd); err != nil {
						break
					}
					var charge int64
					pd.Device, charge, err = prepareDeviceMetadata(pd.Device)
					if err != nil || ids[pd.Device.DeviceID] || len(pd.BanReason) > 256 {
						err = errors.New("invalid or duplicate persisted device metadata")
						break
					}
					if pd.SecretHash != "" && !hexHash(pd.SecretHash) || pd.PairHash != "" && !hexHash(pd.PairHash) {
						err = errors.New("invalid persisted device credential hash")
						break
					}
					charged += charge
					if charged > limit {
						err = errors.New("persisted device metadata capacity exceeded")
						break
					}
					ids[pd.Device.DeviceID] = true
					devices = append(devices, pd)
				}
				if err == nil {
					err = expect(']')
					st.Accounts[id] = devices
				}
			}
			if err == nil {
				err = expect('}')
			}
		case "adminAudit":
			if err = expect('['); err != nil {
				break
			}
			for d.More() && err == nil {
				if len(st.Audit) >= 200 {
					err = errors.New("persisted device audit capacity exceeded")
					break
				}
				var entry AdminAudit
				if err = decodeBounded(4096, &entry); err == nil {
					st.Audit = append(st.Audit, entry)
				}
			}
			if err == nil {
				err = expect(']')
			}
		default:
			err = errors.New("unknown persisted device field")
		}
		if err != nil {
			return st, 0, err
		}
	}
	if err := expect('}'); err != nil {
		return st, 0, err
	}
	if _, err := d.Token(); err != io.EOF {
		return st, 0, errors.New("trailing persisted device data")
	}
	if st.Version != 1 && st.Version != 2 {
		return st, 0, errors.New("unsupported persisted device state version")
	}
	return st, charged, nil
}
