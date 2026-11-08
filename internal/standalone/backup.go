package standalone

// Offline, encrypted, complete-volume backup. The same lock as Hub guarantees
// one writer. Restore publishes only to a new directory after authentication
// and per-file verification; it never overwrites a live or existing volume.
import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"time"

	"database/sql"
	_ "modernc.org/sqlite"
)

const backupReceiptFile = "backup-receipt.json"
const backupMagic = "SALCARA-BACKUP-1\n"
const backupChunk = 64 << 10
const backupMaxBytes = int64(128 << 30)

type backupReceipt struct {
	CreatedAt int64  `json:"created_at"`
	Bytes     int64  `json:"bytes"`
	SHA256    string `json:"sha256"`
	Files     int    `json:"files"`
}
type backupEntry struct {
	Path   string `json:"path"`
	Size   int64  `json:"size"`
	SHA256 string `json:"sha256"`
	Mode   int64  `json:"mode"`
}
type backupManifest struct {
	Schema    int           `json:"schema"`
	CreatedAt int64         `json:"created_at"`
	Version   string        `json:"version"`
	Entries   []backupEntry `json:"entries"`
}

func backupKey(file string, create bool) ([]byte, error) {
	if !filepath.IsAbs(file) || filepath.Clean(file) != file {
		return nil, errors.New("backup key path must be absolute and clean")
	}
	info, err := os.Lstat(file)
	if errors.Is(err, os.ErrNotExist) && create {
		key := make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		f, e := os.OpenFile(file, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
		if e != nil {
			clear(key)
			return nil, e
		}
		_, err = f.Write(key)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err != nil || closeErr != nil {
			clear(key)
			return nil, errors.New("backup key could not be saved")
		}
		if err = syncBackupParent(file); err != nil {
			clear(key)
			return nil, err
		}
		return key, nil
	}
	if err != nil || !info.Mode().IsRegular() || info.Size() != 32 || runtime.GOOS != "windows" && info.Mode().Perm() != 0600 {
		return nil, errors.New("backup key must be an existing private 32-byte regular file")
	}
	f, err := os.Open(file)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	opened, err := f.Stat()
	if err != nil || !os.SameFile(info, opened) {
		return nil, errors.New("backup key changed")
	}
	if create {
		if err = syncBackupParent(file); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(io.LimitReader(f, 33))
}

func syncBackupParent(file string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	f, e := os.Open(filepath.Dir(file))
	if e != nil {
		return e
	}
	defer f.Close()
	return f.Sync()
}

type sealedBackupWriter struct {
	out     io.Writer
	aead    cipher.AEAD
	prefix  []byte
	counter uint32
	buffer  []byte
}

func newSealedBackupWriter(out io.Writer, key []byte) (*sealedBackupWriter, error) {
	prefix := make([]byte, 32)
	if _, e := rand.Read(prefix); e != nil {
		return nil, e
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(append([]byte(backupMagic), prefix...))
	derived := mac.Sum(nil)
	defer clear(derived)
	block, e := aes.NewCipher(derived)
	if e != nil {
		return nil, e
	}
	a, e := cipher.NewGCM(block)
	if e != nil {
		return nil, e
	}
	if _, e = out.Write(append([]byte(backupMagic), prefix...)); e != nil {
		return nil, e
	}
	return &sealedBackupWriter{out: out, aead: a, prefix: prefix}, nil
}
func (w *sealedBackupWriter) frame(b []byte) error {
	if w.counter == ^uint32(0) {
		return errors.New("backup frame limit")
	}
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint32(nonce[8:], w.counter)
	w.counter++
	var size [4]byte
	binary.BigEndian.PutUint32(size[:], uint32(len(b)))
	aad := append([]byte(backupMagic), size[:]...)
	if _, e := w.out.Write(size[:]); e != nil {
		return e
	}
	_, e := w.out.Write(w.aead.Seal(nil, nonce, b, aad))
	return e
}
func (w *sealedBackupWriter) Write(b []byte) (int, error) {
	n := len(b)
	for len(b) > 0 {
		take := min(len(b), backupChunk-len(w.buffer))
		w.buffer = append(w.buffer, b[:take]...)
		b = b[take:]
		if len(w.buffer) == backupChunk {
			if e := w.frame(w.buffer); e != nil {
				return 0, e
			}
			w.buffer = w.buffer[:0]
		}
	}
	return n, nil
}
func (w *sealedBackupWriter) Close() error {
	if len(w.buffer) > 0 {
		if e := w.frame(w.buffer); e != nil {
			return e
		}
	}
	return w.frame(nil)
}

type sealedBackupReader struct {
	in      io.Reader
	aead    cipher.AEAD
	prefix  []byte
	counter uint32
	buffer  []byte
	done    bool
}

func newSealedBackupReader(in io.Reader, key []byte) (io.Reader, error) {
	header := make([]byte, len(backupMagic)+32)
	if _, e := io.ReadFull(in, header); e != nil || string(header[:len(backupMagic)]) != backupMagic {
		return nil, errors.New("invalid encrypted backup header")
	}
	mac := hmac.New(sha256.New, key)
	mac.Write(header)
	derived := mac.Sum(nil)
	defer clear(derived)
	b, e := aes.NewCipher(derived)
	if e != nil {
		return nil, e
	}
	a, e := cipher.NewGCM(b)
	if e != nil {
		return nil, e
	}
	return &sealedBackupReader{in: in, aead: a, prefix: header[len(backupMagic):]}, nil
}
func (r *sealedBackupReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if len(r.buffer) > 0 {
		n := copy(p, r.buffer)
		r.buffer = r.buffer[n:]
		return n, nil
	}
	if r.done {
		return 0, io.EOF
	}
	var size [4]byte
	if _, e := io.ReadFull(r.in, size[:]); e != nil {
		return 0, errors.New("truncated encrypted backup")
	}
	n := binary.BigEndian.Uint32(size[:])
	if n > backupChunk || r.counter == ^uint32(0) {
		return 0, errors.New("invalid backup frame")
	}
	sealed := make([]byte, int(n)+r.aead.Overhead())
	if _, e := io.ReadFull(r.in, sealed); e != nil {
		return 0, errors.New("truncated encrypted backup")
	}
	nonce := make([]byte, 12)
	binary.BigEndian.PutUint32(nonce[8:], r.counter)
	r.counter++
	b, e := r.aead.Open(nil, nonce, sealed, append([]byte(backupMagic), size[:]...))
	if e != nil {
		return 0, errors.New("backup authentication failed (wrong key or damaged archive)")
	}
	if n == 0 {
		var extra [1]byte
		if count, e := r.in.Read(extra[:]); count != 0 || e != io.EOF {
			return 0, errors.New("unexpected data after backup")
		}
		r.done = true
		return 0, io.EOF
	}
	r.buffer = b
	return r.Read(p)
}

func insideDir(parent, target string) bool {
	rel, err := filepath.Rel(parent, target)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
func backupPaths(data, archive, key string) error {
	for _, p := range []string{data, archive, key} {
		if !filepath.IsAbs(p) || filepath.Clean(p) != p {
			return errors.New("backup paths must be absolute and clean")
		}
	}
	// Resolve existing parents so a directory symlink cannot place a backup
	// or its key inside the source volume despite a different textual path.
	resolvedData, e := filepath.EvalSymlinks(filepath.Dir(data))
	if e != nil {
		return e
	}
	resolvedData = filepath.Join(resolvedData, filepath.Base(data))
	resolvedArchive, e := filepath.EvalSymlinks(filepath.Dir(archive))
	if e != nil {
		return e
	}
	resolvedArchive = filepath.Join(resolvedArchive, filepath.Base(archive))
	resolvedKey, e := filepath.EvalSymlinks(filepath.Dir(key))
	if e != nil {
		return e
	}
	resolvedKey = filepath.Join(resolvedKey, filepath.Base(key))
	if insideDir(resolvedData, resolvedArchive) || insideDir(resolvedData, resolvedKey) || strings.EqualFold(resolvedArchive, resolvedKey) {
		return errors.New("archive and backup key must be distinct and outside the Hub volume")
	}
	return nil
}

func CreateBackup(cfg Config, archive, keyFile string) (backupReceipt, error) {
	var receipt backupReceipt
	if e := cfg.Validate(); e != nil {
		return receipt, e
	}
	if e := backupPaths(cfg.DataDir, archive, keyFile); e != nil {
		return receipt, e
	}
	lock, e := acquireDataLock(cfg.DataDir)
	if e != nil {
		return receipt, e
	}
	defer lock.release()
	root, e := openAuthRoot(cfg.DataDir)
	if e != nil {
		return receipt, e
	}
	defer root.Close()
	if _, e = loadAdminAccount(root, filepath.Base(cfg.AdminAccountFile)); e != nil {
		return receipt, e
	}
	key, e := backupKey(keyFile, true)
	if e != nil {
		return receipt, e
	}
	defer clear(key)
	out, e := os.OpenFile(archive, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if e != nil {
		return receipt, e
	}
	complete := false
	defer func() {
		out.Close()
		if !complete {
			os.Remove(archive)
		}
	}()
	hash := sha256.New()
	sealed, e := newSealedBackupWriter(io.MultiWriter(out, hash), key)
	if e != nil {
		return receipt, e
	}
	gz := gzip.NewWriter(sealed)
	tw := tar.NewWriter(gz)
	manifest := backupManifest{Schema: 1, CreatedAt: time.Now().UnixMilli(), Version: Version, Entries: []backupEntry{}}
	var total int64
	e = fs.WalkDir(root.FS(), ".", func(name string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if name == "." {
			return nil
		}
		if d.IsDir() {
			return nil
		}
		if name == ".salcara-hub.lock" || name == backupReceiptFile {
			return nil
		}
		info, e := d.Info()
		if e != nil {
			return e
		}
		if !info.Mode().IsRegular() {
			return errors.New("backup refuses symlinks and special files")
		}
		if !validBackupName(name) || len(manifest.Entries) >= 100000 || info.Size() < 0 || info.Size() > backupMaxBytes-total {
			return errors.New("backup file or size limit exceeded")
		}
		f, e := root.Open(filepath.FromSlash(name))
		if e != nil {
			return e
		}
		defer f.Close()
		opened, e := f.Stat()
		if e != nil || !os.SameFile(info, opened) {
			return errors.New("backup source changed")
		}
		mode := int64(0600 | (info.Mode().Perm() & 0100))
		if e = tw.WriteHeader(&tar.Header{Name: "data/" + name, Mode: mode, Size: info.Size(), Typeflag: tar.TypeReg}); e != nil {
			return e
		}
		fh := sha256.New()
		n, e := io.CopyN(io.MultiWriter(tw, fh), f, info.Size())
		if e != nil || n != info.Size() {
			return errors.New("backup source read failed")
		}
		after, e := f.Stat()
		if e != nil || after.Size() != info.Size() || !after.ModTime().Equal(info.ModTime()) {
			return errors.New("backup source changed during copy")
		}
		manifest.Entries = append(manifest.Entries, backupEntry{Path: name, Size: n, SHA256: hex.EncodeToString(fh.Sum(nil)), Mode: mode})
		total += n
		return nil
	})
	if e != nil {
		return receipt, e
	}
	raw, _ := json.Marshal(manifest)
	if e = tw.WriteHeader(&tar.Header{Name: "manifest.json", Mode: 0600, Size: int64(len(raw)), Typeflag: tar.TypeReg}); e != nil {
		return receipt, e
	}
	if _, e = tw.Write(raw); e != nil {
		return receipt, e
	}
	for _, closeFn := range []func() error{tw.Close, gz.Close, sealed.Close, out.Sync, out.Close} {
		if e = closeFn(); e != nil {
			return receipt, e
		}
	}
	if e = syncBackupParent(archive); e != nil {
		return receipt, e
	}
	if _, e = verifyBackup(archive, key, ""); e != nil {
		return receipt, e
	}
	info, e := os.Stat(archive)
	if e != nil {
		return receipt, e
	}
	receipt = backupReceipt{CreatedAt: manifest.CreatedAt, Bytes: info.Size(), SHA256: hex.EncodeToString(hash.Sum(nil)), Files: len(manifest.Entries)}
	complete = true // A receipt write failure must never delete a verified archive.
	raw, _ = json.Marshal(receipt)
	warning, e := replacePrivateBounded(root, backupReceiptFile, raw, 4096)
	if e != nil || warning {
		return receipt, errors.New("backup verified, but backup receipt could not be durably saved")
	}
	return receipt, nil
}

func validBackupName(name string) bool {
	return name != "" && len(name) <= 512 && path.Clean(name) == name && name != "." && name != ".." && filepath.IsLocal(filepath.FromSlash(name)) && !strings.HasPrefix(name, "../") && !strings.HasPrefix(name, "/") && !strings.ContainsAny(name, "\\:\x00")
}

func verifyBackup(archive string, key []byte, dest string) (backupManifest, error) {
	var manifest backupManifest
	info, e := os.Lstat(archive)
	if e != nil || !info.Mode().IsRegular() || info.Size() > backupMaxBytes+(1<<30) {
		return manifest, errors.New("backup must be a bounded regular file")
	}
	f, e := os.Open(archive)
	if e != nil {
		return manifest, e
	}
	defer f.Close()
	opened, e := f.Stat()
	if e != nil || !os.SameFile(info, opened) {
		return manifest, errors.New("backup changed")
	}
	sealed, e := newSealedBackupReader(f, key)
	if e != nil {
		return manifest, e
	}
	gz, e := gzip.NewReader(sealed)
	if e != nil {
		return manifest, e
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	var root *os.Root
	if dest != "" {
		root, e = os.OpenRoot(dest)
		if e != nil {
			return manifest, e
		}
		defer root.Close()
	}
	entries := []backupEntry{}
	seen := map[string]bool{}
	var total int64
	found := false
	for {
		h, e := tr.Next()
		if e == io.EOF {
			break
		}
		if e != nil {
			return manifest, e
		}
		if found || h.Typeflag != tar.TypeReg || h.Size < 0 {
			return manifest, errors.New("invalid backup entry")
		}
		if h.Name == "manifest.json" {
			if h.Size > 32<<20 {
				return manifest, errors.New("manifest too large")
			}
			raw, e := io.ReadAll(io.LimitReader(tr, 32<<20+1))
			if e != nil || json.Unmarshal(raw, &manifest) != nil {
				return manifest, errors.New("invalid backup manifest")
			}
			found = true
			continue
		}
		name := strings.TrimPrefix(h.Name, "data/")
		if name == h.Name || !validBackupName(name) || seen[name] || len(entries) >= 100000 || h.Size > backupMaxBytes-total || name == ".salcara-hub.lock" || (h.Mode != 0600 && h.Mode != 0700) {
			return manifest, errors.New("unsafe backup path or capacity")
		}
		seen[name] = true
		fh := sha256.New()
		var out *os.File
		var writer io.Writer = fh
		if root != nil {
			if e = root.MkdirAll(filepath.FromSlash(path.Dir(name)), 0700); e != nil {
				return manifest, e
			}
			out, e = root.OpenFile(filepath.FromSlash(name), os.O_CREATE|os.O_EXCL|os.O_WRONLY, os.FileMode(h.Mode))
			if e != nil {
				return manifest, e
			}
			writer = io.MultiWriter(out, fh)
		}
		n, e := io.Copy(writer, tr)
		if out != nil {
			if e == nil {
				e = out.Sync()
			}
			closeErr := out.Close()
			if e == nil {
				e = closeErr
			}
		}
		if e != nil || n != h.Size {
			return manifest, errors.New("backup extraction failed")
		}
		total += n
		entries = append(entries, backupEntry{Path: name, Size: n, SHA256: hex.EncodeToString(fh.Sum(nil)), Mode: h.Mode})
	}
	// Consume gzip/footer and the authenticated final frame; tar EOF alone
	// cannot establish that the encrypted archive wasn't truncated/appended.
	var trailing [1]byte
	n, e := gz.Read(trailing[:])
	if e != io.EOF || n != 0 {
		return manifest, errors.New("backup footer invalid")
	}
	if !found || manifest.Schema != 1 || !reflect.DeepEqual(entries, manifest.Entries) {
		return manifest, errors.New("backup manifest verification failed")
	}
	return manifest, nil
}

func VerifyBackup(archive, keyFile string) (int, error) {
	key, e := backupKey(keyFile, false)
	if e != nil {
		return 0, e
	}
	defer clear(key)
	m, e := verifyBackup(archive, key, "")
	return len(m.Entries), e
}

func RestoreBackup(cfg Config, archive, keyFile string) error {
	if e := cfg.Validate(); e != nil {
		return e
	}
	if e := backupPaths(cfg.DataDir, archive, keyFile); e != nil {
		return e
	}
	if _, e := os.Lstat(cfg.DataDir); !errors.Is(e, os.ErrNotExist) {
		return errors.New("restore target must not exist; choose a new volume directory")
	}
	key, e := backupKey(keyFile, false)
	if e != nil {
		return e
	}
	defer clear(key)
	stage, e := os.MkdirTemp(filepath.Dir(cfg.DataDir), ".salcara-restore-")
	if e != nil {
		return e
	}
	defer os.RemoveAll(stage)
	if _, e = verifyBackup(archive, key, stage); e != nil {
		return e
	}
	root, e := openAuthRoot(stage)
	if e != nil {
		return e
	}
	_, e = loadAdminAccount(root, filepath.Base(cfg.AdminAccountFile))
	if e == nil {
		_, e = loadMembers(root)
	}
	root.Close()
	if e != nil {
		return e
	}
	if e = verifyRestoredMailbox(stage); e != nil {
		return e
	}
	mode := cfg.DeploymentMode
	if mode == "" {
		mode = "station"
	}
	if e = ensureDeploymentMode(stage, mode); e != nil {
		return e
	}
	journal, e := openOperations(stage)
	if e != nil {
		return e
	}
	journal.close()
	if e = syncRestoredDirectories(stage); e != nil {
		return e
	}
	// Atomic publication with no replacement. On supported OSes Rename cannot
	// replace a non-empty directory. Reserve the target with an exclusive lock
	// file in its parent to serialize restores from this tool.
	reservation := cfg.DataDir + ".restore-lock"
	guard, e := os.OpenFile(reservation, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if e != nil {
		return errors.New("restore already reserved; inspect a previous interrupted restore")
	}
	defer os.Remove(reservation)
	guard.Close()
	if _, e = os.Lstat(cfg.DataDir); !errors.Is(e, os.ErrNotExist) {
		return errors.New("restore target appeared during verification")
	}
	if e = publishRestoredDirectory(stage, cfg.DataDir); e != nil {
		return e
	}
	parent, e := os.Open(filepath.Dir(cfg.DataDir))
	if e == nil {
		if runtime.GOOS != "windows" {
			e = parent.Sync()
		}
		parent.Close()
	}
	return e
}

func syncRestoredDirectories(dir string) error {
	if runtime.GOOS == "windows" {
		return nil
	}
	var dirs []string
	if err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			dirs = append(dirs, p)
		}
		return nil
	}); err != nil {
		return err
	}
	for i := len(dirs) - 1; i >= 0; i-- {
		f, err := os.Open(dirs[i])
		if err != nil {
			return err
		}
		err = f.Sync()
		f.Close()
		if err != nil {
			return err
		}
	}
	return nil
}

func verifyRestoredMailbox(dir string) error {
	file := filepath.Join(dir, "mailbox", "mailbox.sqlite")
	if _, e := os.Stat(file); errors.Is(e, os.ErrNotExist) {
		return nil
	}
	key, e := os.ReadFile(filepath.Join(dir, "mailbox", "key-v1"))
	if e != nil || len(key) != 32 {
		return errors.New("restored mailbox key missing or invalid")
	}
	defer clear(key)
	db, e := sql.Open("sqlite", file)
	if e != nil {
		return e
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	var result string
	if e = db.QueryRow("PRAGMA integrity_check").Scan(&result); e != nil || !bytes.Equal([]byte(result), []byte("ok")) {
		return fmt.Errorf("restored mailbox integrity check failed")
	}
	var check string
	if e = db.QueryRow("SELECT value FROM meta WHERE name='key-check-v1'").Scan(&check); e != nil {
		return errors.New("restored mailbox key check missing")
	}
	sealed, e := base64.RawStdEncoding.DecodeString(check)
	if e != nil {
		return errors.New("restored mailbox key check invalid")
	}
	block, e := aes.NewCipher(key)
	if e != nil {
		return e
	}
	aead, e := cipher.NewGCM(block)
	if e != nil {
		return e
	}
	if len(sealed) < aead.NonceSize() {
		return errors.New("restored mailbox key check truncated")
	}
	plain, e := aead.Open(nil, sealed[:aead.NonceSize()], sealed[aead.NonceSize():], []byte("v1:key-check"))
	defer clear(plain)
	if e != nil || string(plain) != "salcara-mailbox-key-v1" {
		return errors.New("restored mailbox key does not match database")
	}
	return nil
}
