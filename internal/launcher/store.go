package launcher

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// References never contain executable paths. A downloaded reference contains
// the original signed envelope; version/hash/size/path are derived from it on
// every restore and execution. Bootstrap always resolves to the image binary.
type reference struct {
	Bootstrap bool   `json:"bootstrap,omitempty"`
	Envelope  []byte `json:"signed_feed,omitempty"`
}
type jobRecord struct {
	ID      string `json:"id"`
	Status  string `json:"status"`
	Message string `json:"message"`
}

// This is only a retry-suppression fingerprint, never an executable pointer.
// A different image/version may be tried; the exact unhealthy image is not
// retried on every container restart after a signed-cache fallback succeeds.
type bootstrapFailure struct {
	Version string `json:"version"`
	SHA256  string `json:"sha256"`
}
type diskState struct {
	Schema          int               `json:"schema_version"`
	Current         reference         `json:"current"`
	Previous        *reference        `json:"previous,omitempty"`
	Job             *jobRecord        `json:"last_job,omitempty"`
	FailedBootstrap *bootstrapFailure `json:"failed_bootstrap,omitempty"`
}
type release struct {
	Ref     reference
	Version string
	Path    string
	SHA256  string
	Size    int64
}
type store struct {
	dir, bootstrapPath, bootstrapVersion, platform string
	key                                            ed25519.PublicKey
}

func newStore(cfg Config, key ed25519.PublicKey) (*store, error) {
	dir := filepath.Join(cfg.DataDir, "updates")
	if err := realDirectory(cfg.DataDir); err != nil {
		return nil, err
	}
	if err := realDirectory(dir); err != nil {
		return nil, err
	}
	return &store{dir: dir, bootstrapPath: cfg.BootstrapPath, bootstrapVersion: cfg.BootstrapVersion, platform: runtime.GOOS + "/" + runtime.GOARCH, key: key}, nil
}
func realDirectory(path string) error {
	if info, err := os.Lstat(path); err == nil {
		if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
			return errors.New("update directory must be a real directory")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot inspect update directory")
	}
	if err := os.MkdirAll(path, 0700); err != nil {
		return errors.New("cannot create update directory")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("invalid update directory")
	}
	return nil
}
func (s *store) resolve(ref reference) (release, error) {
	r := release{Ref: ref}
	if ref.Bootstrap {
		if len(ref.Envelope) != 0 {
			return r, errors.New("ambiguous bootstrap reference")
		}
		r.Version, r.Path = s.bootstrapVersion, s.bootstrapPath
		f, err := openRegular(r.Path, maxBinaryBytes)
		if err != nil {
			return r, err
		}
		defer f.Close()
		digest, size, err := hashFile(f, maxBinaryBytes)
		if err != nil {
			return r, err
		}
		r.SHA256, r.Size = digest, size
		return r, nil
	}
	f, err := verifyFeed(ref.Envelope, s.key)
	if err != nil {
		return r, err
	}
	b, ok := f.Binaries[s.platform]
	if !ok {
		return r, errors.New("this platform cannot receive signed Hub updates")
	}
	r.Version, r.SHA256, r.Size = f.Version, b.SHA256, b.Size
	r.Path = filepath.Join(s.dir, "hub-"+b.SHA256)
	file, err := openVerified(r)
	if err != nil {
		return r, err
	}
	file.Close()
	return r, nil
}
func openRegular(path string, limit int64) (*os.File, error) {
	before, err := os.Lstat(path)
	if err != nil || !before.Mode().IsRegular() || before.Size() < 1 || before.Size() > limit {
		return nil, errors.New("update executable must be a bounded regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("cannot open update executable")
	}
	after, err := f.Stat()
	current, pathErr := os.Lstat(path)
	if err != nil || pathErr != nil || !after.Mode().IsRegular() || !current.Mode().IsRegular() || !os.SameFile(before, after) || !os.SameFile(after, current) {
		f.Close()
		return nil, errors.New("update executable path changed")
	}
	if runtime.GOOS != "windows" && after.Mode().Perm()&0022 != 0 {
		f.Close()
		return nil, errors.New("update executable must not be group/world writable")
	}
	return f, nil
}
func hashFile(f *os.File, limit int64) (string, int64, error) {
	h := sha256.New()
	n, err := io.Copy(h, io.LimitReader(f, limit+1))
	if err != nil || n < 1 || n > limit {
		return "", 0, errors.New("cannot hash bounded update executable")
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return "", 0, err
	}
	return hex.EncodeToString(h.Sum(nil)), n, nil
}
func openVerified(r release) (*os.File, error) {
	f, err := openRegular(r.Path, maxBinaryBytes)
	if err != nil {
		return nil, err
	}
	digest, size, err := hashFile(f, maxBinaryBytes)
	if err != nil || digest != r.SHA256 || size != r.Size {
		f.Close()
		return nil, errors.New("update executable hash/size verification failed")
	}
	return f, nil
}
func (s *store) load() (diskState, error) {
	state := diskState{Schema: 1, Current: reference{Bootstrap: true}}
	path := filepath.Join(s.dir, "state.json")
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		_, err = s.resolve(state.Current)
		return state, err
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() > 256<<10 {
		return state, errors.New("update state is not a bounded regular file")
	}
	f, err := os.Open(path)
	if err != nil {
		return state, errors.New("cannot read update state")
	}
	defer f.Close()
	after, err := f.Stat()
	if err != nil || !os.SameFile(info, after) {
		return state, errors.New("update state path changed")
	}
	raw, err := io.ReadAll(io.LimitReader(f, (256<<10)+1))
	// Decode into a zero value: omitted bootstrap in a signed reference must
	// not retain the default used only when the state file is absent.
	state = diskState{}
	if err != nil || len(raw) > 256<<10 || strictDecode(raw, &state) != nil || state.Schema != 1 {
		return state, errors.New("invalid update state; refusing arbitrary disk pointers")
	}
	if _, err = s.resolve(state.Current); err != nil {
		return state, err
	}
	if state.Previous != nil {
		if _, err = s.resolve(*state.Previous); err != nil {
			return state, err
		}
	}
	if state.Job != nil && (len(state.Job.ID) > 128 || len(state.Job.Message) > 2048 || (state.Job.Status != "updating" && state.Job.Status != "failed" && state.Job.Status != "current")) {
		return state, errors.New("invalid saved update status")
	}
	if err = validateBootstrapFailure(state.FailedBootstrap); err != nil {
		return state, err
	}
	return state, nil
}
func (s *store) save(state diskState) error {
	if err := validateBootstrapFailure(state.FailedBootstrap); err != nil {
		return err
	}
	if _, err := s.resolve(state.Current); err != nil {
		return err
	}
	if state.Previous != nil {
		if _, err := s.resolve(*state.Previous); err != nil {
			return err
		}
	}
	path := filepath.Join(s.dir, "state.json")
	if info, err := os.Lstat(path); err == nil && !info.Mode().IsRegular() {
		return errors.New("update state must not be a link or directory")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot inspect update state")
	}
	raw, err := json.Marshal(state)
	if err != nil || len(raw) > 256<<10 {
		return errors.New("cannot encode bounded update state")
	}
	temp, err := os.CreateTemp(s.dir, ".state-*.tmp")
	if err != nil {
		return errors.New("cannot write update state")
	}
	name := temp.Name()
	defer os.Remove(name)
	if err = temp.Chmod(0600); err == nil {
		_, err = temp.Write(raw)
	}
	if err == nil {
		err = temp.Sync()
	}
	closeErr := temp.Close()
	if err != nil || closeErr != nil {
		return errors.New("cannot flush update state")
	}
	if err = os.Rename(name, path); err != nil {
		return errors.New("cannot commit update state")
	}
	return syncDirectory(s.dir)
}
func validateBootstrapFailure(failure *bootstrapFailure) error {
	if failure == nil {
		return nil
	}
	if _, err := parseVersion(failure.Version); err != nil || !hashPattern.MatchString(failure.SHA256) {
		return errors.New("invalid failed image fingerprint")
	}
	return nil
}

// preferBootstrap compares the trusted image with the verified signed cache.
// Selecting a candidate does not commit it: startup health must pass first.
// Resolve the signed current reference before considering the image, so a new
// image is never used to hide a tampered or unsigned persisted reference.
func (s *store) preferBootstrap(state diskState) (diskState, bool, error) {
	current, err := s.resolve(state.Current)
	if err != nil {
		return state, false, err
	}
	if state.Current.Bootstrap || !newer(s.bootstrapVersion, current.Version) {
		return state, false, nil
	}
	image, err := s.resolve(reference{Bootstrap: true})
	if err != nil {
		return state, false, err
	}
	if failed := state.FailedBootstrap; failed != nil && failed.Version == image.Version && failed.SHA256 == image.SHA256 {
		return state, false, nil
	}
	previous := state.Current
	state.Current, state.Previous = image.Ref, &previous
	return state, true, nil
}
func syncDirectory(path string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func (s *store) keepVerifiedBinary(tempPath string, b Binary) error {
	r := release{Path: tempPath, SHA256: b.SHA256, Size: b.Size}
	file, err := openVerified(r)
	if err != nil {
		return err
	}
	file.Close()
	if err = os.Chmod(tempPath, 0500); err != nil {
		return errors.New("cannot mark verified Hub executable")
	}
	target := filepath.Join(s.dir, "hub-"+b.SHA256)
	if _, err = os.Lstat(target); err == nil {
		f, e := openVerified(release{Path: target, SHA256: b.SHA256, Size: b.Size})
		if e != nil {
			return e
		}
		f.Close()
		return nil
	} else if !errors.Is(err, os.ErrNotExist) {
		return errors.New("cannot inspect update executable cache")
	}
	// A hard link is atomic and never overwrites a preexisting path. The temp
	// file lives on the same local data volume as the immutable cache entry.
	if err = os.Link(tempPath, target); err != nil {
		return errors.New("cannot commit verified Hub executable")
	}
	return syncDirectory(s.dir)
}
