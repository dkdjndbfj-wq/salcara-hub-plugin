package edgepool

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"runtime"
)

// Source URLs, snapshots and credentials share one authenticated encrypted
// registry. The key is private and must be protected with the data-volume backup.
func readSourcePrivate(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() || !privateMode(info) || info.Size() > limit {
		return nil, errors.New("invalid private source file")
	}
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("private source file changed")
	}
	return io.ReadAll(io.LimitReader(f, limit+1))
}
func writeSourcePrivate(path string, data []byte) error {
	if info, err := os.Lstat(path); err == nil && (!info.Mode().IsRegular() || !privateMode(info)) {
		return errors.New("invalid private source target")
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".source-*")
	if err != nil {
		return err
	}
	name := f.Name()
	defer os.Remove(name)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if err = os.Rename(name, path); err != nil {
		return err
	}
	// A rename has already committed. Runtime state must follow that committed
	// file even when a platform cannot confirm directory durability.
	if runtime.GOOS != "windows" {
		if d, e := os.Open(filepath.Dir(path)); e == nil {
			_ = d.Sync()
			_ = d.Close()
		}
	}
	return nil
}
func sourceAEAD(key []byte) (cipher.AEAD, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	return cipher.NewGCM(block)
}
func sourceID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("OS randomness unavailable")
	}
	return hex.EncodeToString(b[:])
}
func (m *sourceManager) persistLocked(next sourceDisk) error {
	raw, err := json.Marshal(next)
	if err != nil || len(raw) > 8<<20 {
		return errors.New("source_registry_limit")
	}
	aead, err := sourceAEAD(m.key)
	if err != nil {
		return errors.New("source_storage_failed")
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err = rand.Read(nonce); err != nil {
		return errors.New("source_storage_failed")
	}
	sealed := aead.Seal(nonce, nonce, raw, []byte("salcara-sources-v1"))
	if writeSourcePrivate(filepath.Join(m.pool.cfg.DataDir, "sources.enc"), sealed) != nil {
		return errors.New("source_storage_failed")
	}
	m.saved = next
	return nil
}
func (m *sourceManager) load() error {
	keyPath := filepath.Join(m.pool.cfg.DataDir, "sources.key")
	key, err := readSourcePrivate(keyPath, 32)
	if errors.Is(err, os.ErrNotExist) {
		if _, e := os.Lstat(filepath.Join(m.pool.cfg.DataDir, "sources.enc")); !errors.Is(e, os.ErrNotExist) {
			return errors.New("source key unavailable")
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return err
		}
		if err = writeSourcePrivate(keyPath, key); err != nil {
			return err
		}
	} else if err != nil {
		return err
	}
	if len(key) != 32 {
		return errors.New("invalid source key")
	}
	m.key = key
	data, err := readSourcePrivate(filepath.Join(m.pool.cfg.DataDir, "sources.enc"), 9<<20)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	aead, _ := sourceAEAD(key)
	if len(data) < aead.NonceSize() {
		return errors.New("invalid source registry")
	}
	raw, err := aead.Open(nil, data[:aead.NonceSize()], data[aead.NonceSize():], []byte("salcara-sources-v1"))
	if err != nil || json.Unmarshal(raw, &m.saved) != nil || m.saved.Revision < 1 || len(m.saved.Sources) > 16 || len(m.saved.Kernels) > releaseHistoryLimit+3 {
		return errors.New("invalid source registry")
	}
	for i := range m.saved.Sources {
		s := &m.saved.Sources[i]
		if s.Status == "refreshing" || s.Status == "applying" {
			s.Status = "interrupted"
			s.Error = "operation_interrupted"
		}
		if s.Candidate != nil && len(s.Candidate.Nodes) > 256 {
			return errors.New("invalid source candidate")
		}
	}
	return nil
}
