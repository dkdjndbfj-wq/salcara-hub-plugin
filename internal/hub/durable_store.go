package hub

// The mailbox has one SQLite writer. Payloads are authenticated ciphertext;
// indexes contain scoped hashes, sequence numbers and expiration times only.
// WAL + FULL makes an HTTP acknowledgement a durable boundary, not a RAM hint.
import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	_ "modernc.org/sqlite"
)

const durableDay = int64(24 * time.Hour / time.Millisecond)

var errStorageCapacity = errors.New("durable storage capacity reached")

type StoragePolicy struct {
	OfflineQueueEnabled bool  `json:"offlineQueueEnabled"`
	RetentionDays       int   `json:"retentionDays"`
	GlobalBytes         int64 `json:"globalBytes"`
	BindingBytes        int64 `json:"bindingBytes"`
}

type StorageStatus struct {
	Enabled           bool          `json:"enabled"`
	Policy            StoragePolicy `json:"policy"`
	DiskBytes         int64         `json:"diskBytes"`
	PayloadBytes      int64         `json:"payloadBytes"`
	ImageBytes        int64         `json:"imageBytes"`
	ThumbnailBytes    int64         `json:"thumbnailBytes"`
	ReplyBytes        int64         `json:"replyBytes"`
	QueueBytes        int64         `json:"queueBytes"`
	EventCount        int64         `json:"eventCount"`
	OperationCount    int64         `json:"operationCount"`
	BlobCount         int64         `json:"blobCount"`
	LastCleanupAt     int64         `json:"lastCleanupAt"`
	Warning           bool          `json:"warning"`
	FreeDiskBytes     int64         `json:"freeDiskBytes"`
	ReservedDiskBytes int64         `json:"reservedDiskBytes"`
	LowDisk           bool          `json:"lowDisk"`
}

type durableStore struct {
	mu             sync.Mutex
	db             *sql.DB
	dir            string
	aead           cipher.AEAD
	key            []byte
	epoch          string
	policy         StoragePolicy
	lastCleanup    int64
	uploadSlots    chan struct{}
	thumbnailSlots chan struct{}
	volumeProbe    func(string) (int64, int64, error)
}

func newDurableStore(dataDir string) (_ *durableStore, err error) {
	if dataDir == "" {
		return nil, nil
	}
	dir := filepath.Join(dataDir, "mailbox")
	if err = os.MkdirAll(filepath.Join(dir, "blobs"), 0700); err != nil {
		return nil, err
	}
	keyPath := filepath.Join(dir, "key-v1")
	key, err := os.ReadFile(keyPath)
	if errors.Is(err, os.ErrNotExist) {
		// Losing the key must never silently create a different decryption key.
		if _, e := os.Stat(filepath.Join(dir, "mailbox.sqlite")); e == nil {
			return nil, errors.New("mailbox encryption key missing")
		}
		key = make([]byte, 32)
		if _, err = rand.Read(key); err != nil {
			return nil, err
		}
		var f *os.File
		f, err = os.OpenFile(keyPath, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
		if err != nil {
			return nil, err
		}
		_, err = f.Write(key)
		if err == nil {
			err = f.Sync()
		}
		closeErr := f.Close()
		if err == nil {
			err = closeErr
		}
		if err != nil {
			return nil, err
		}
		if d, e := os.Open(dir); e == nil {
			_ = d.Sync()
			_ = d.Close()
		}
	}
	if err != nil || len(key) != 32 {
		return nil, errors.New("invalid mailbox encryption key")
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "mailbox.sqlite"))
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	defer func() {
		if err != nil {
			_ = db.Close()
		}
	}()
	var schema *sql.Tx
	defer func() {
		if schema != nil {
			_ = schema.Rollback()
		}
	}()
	for _, q := range []string{
		`PRAGMA auto_vacuum=INCREMENTAL`, `PRAGMA journal_mode=WAL`, `PRAGMA synchronous=FULL`, `PRAGMA busy_timeout=5000`, `PRAGMA foreign_keys=ON`, `PRAGMA wal_autocheckpoint=256`,
		`CREATE TABLE IF NOT EXISTS meta (name TEXT PRIMARY KEY, value TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS quota_owners (owner TEXT PRIMARY KEY, scope TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS replies (id TEXT PRIMARY KEY, owner TEXT NOT NULL, body BLOB, status INTEGER NOT NULL, expires INTEGER NOT NULL, state TEXT NOT NULL, bytes INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS operations (id TEXT PRIMARY KEY, owner TEXT NOT NULL, intent TEXT NOT NULL, hash TEXT NOT NULL, deadline INTEGER NOT NULL, ticket TEXT NOT NULL, UNIQUE(owner,intent))`,
		`CREATE TABLE IF NOT EXISTS events (scope TEXT NOT NULL, seq INTEGER NOT NULL, session TEXT NOT NULL, body BLOB NOT NULL, expires INTEGER NOT NULL, bytes INTEGER NOT NULL, PRIMARY KEY(scope,seq))`,
		`CREATE INDEX IF NOT EXISTS events_session ON events(scope,session,seq)`,
		`CREATE TABLE IF NOT EXISTS event_budgets (scope TEXT NOT NULL, id TEXT NOT NULL, bytes INTEGER NOT NULL, truncated INTEGER NOT NULL DEFAULT 0, expires INTEGER NOT NULL, PRIMARY KEY(scope,id))`,
		`CREATE TABLE IF NOT EXISTS event_heads (scope TEXT NOT NULL, session TEXT NOT NULL, last_seq INTEGER NOT NULL, evicted INTEGER NOT NULL DEFAULT 0, PRIMARY KEY(scope,session))`,
		`CREATE TABLE IF NOT EXISTS batches (scope TEXT NOT NULL, id TEXT NOT NULL, digest TEXT NOT NULL, accepted INTEGER NOT NULL, last_seq INTEGER NOT NULL, PRIMARY KEY(scope,id))`,
		`CREATE TABLE IF NOT EXISTS acks (scope TEXT NOT NULL, session TEXT NOT NULL, seq INTEGER NOT NULL, PRIMARY KEY(scope,session))`,
		`CREATE TABLE IF NOT EXISTS blobs (id TEXT PRIMARY KEY, scope TEXT NOT NULL, state TEXT NOT NULL, bytes INTEGER NOT NULL, mime TEXT NOT NULL, created INTEGER NOT NULL, expires INTEGER NOT NULL, lease INTEGER NOT NULL DEFAULT 0, total INTEGER NOT NULL, hash TEXT NOT NULL DEFAULT '', width INTEGER NOT NULL DEFAULT 0, height INTEGER NOT NULL DEFAULT 0)`,
		`CREATE TABLE IF NOT EXISTS blob_parts (id TEXT NOT NULL, part INTEGER NOT NULL, body BLOB NOT NULL, bytes INTEGER NOT NULL, digest TEXT NOT NULL, PRIMARY KEY(id,part))`,
		`CREATE TABLE IF NOT EXISTS blob_refs (scope TEXT NOT NULL, id TEXT NOT NULL, ref TEXT NOT NULL, expires INTEGER NOT NULL, PRIMARY KEY(scope,id,ref))`,
		`CREATE TABLE IF NOT EXISTS offline_queue (id TEXT PRIMARY KEY, owner TEXT NOT NULL, scope TEXT NOT NULL, deadline INTEGER NOT NULL, state TEXT NOT NULL, body BLOB, bytes INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS push_jobs (id TEXT PRIMARY KEY, body BLOB, state TEXT NOT NULL, due INTEGER NOT NULL, expires INTEGER NOT NULL, attempts INTEGER NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS thumbnails (id TEXT PRIMARY KEY, scope TEXT NOT NULL, body BLOB NOT NULL, bytes INTEGER NOT NULL, accessed INTEGER NOT NULL, expires INTEGER NOT NULL)`,
	} {
		if strings.HasPrefix(q, "CREATE ") {
			if schema == nil {
				schema, err = db.Begin()
				if err != nil {
					return nil, err
				}
			}
			_, err = schema.Exec(q)
		} else {
			_, err = db.Exec(q)
		}
		if err != nil {
			return nil, err
		}
	}
	if schema != nil {
		if err = schema.Commit(); err != nil {
			return nil, err
		}
		schema = nil
	}
	s := &durableStore{db: db, dir: dir, aead: aead, key: key, uploadSlots: make(chan struct{}, 2), thumbnailSlots: make(chan struct{}, 1), policy: StoragePolicy{RetentionDays: 7, GlobalBytes: 5 << 30, BindingBytes: 256 << 20}}
	if total, _, e := storageVolume(dir); e == nil && total/10 < s.policy.GlobalBytes {
		s.policy.GlobalBytes = total / 10
	}
	var policy string
	if db.QueryRow(`SELECT value FROM meta WHERE name='policy'`).Scan(&policy) == nil {
		if json.Unmarshal([]byte(policy), &s.policy) != nil || !validStoragePolicy(s.policy) {
			return nil, errors.New("invalid mailbox policy")
		}
	}
	err = db.QueryRow(`SELECT value FROM meta WHERE name='epoch'`).Scan(&s.epoch)
	if errors.Is(err, sql.ErrNoRows) {
		s.epoch = newID()
		_, err = db.Exec(`INSERT INTO meta(name,value) VALUES('epoch',?)`, s.epoch)
	}
	if err != nil {
		return nil, err
	}
	var quick string
	if err = db.QueryRow(`PRAGMA quick_check`).Scan(&quick); err != nil || quick != "ok" {
		return nil, errors.New("mailbox integrity check failed")
	}
	var check string
	err = db.QueryRow(`SELECT value FROM meta WHERE name='key-check-v1'`).Scan(&check)
	if errors.Is(err, sql.ErrNoRows) {
		var sealed []byte
		sealed, err = s.seal([]byte("salcara-mailbox-key-v1"), "key-check")
		if err == nil {
			_, err = db.Exec(`INSERT INTO meta(name,value) VALUES('key-check-v1',?)`, base64.RawStdEncoding.EncodeToString(sealed))
		}
	} else if err == nil {
		var sealed, clear []byte
		sealed, err = base64.RawStdEncoding.DecodeString(check)
		if err == nil {
			clear, err = s.open(sealed, "key-check")
		}
		if err == nil && string(clear) != "salcara-mailbox-key-v1" {
			err = errors.New("mailbox key does not match")
		}
	}
	if err != nil {
		return nil, err
	}
	// A claimed operation may have reached an external Agent before a crash.
	// Its encrypted intent is discarded; only exact receipt/history evidence
	// can resolve it, never an automatic second dispatch.
	if _, err = db.Exec(`UPDATE offline_queue SET state='uncertain',body=NULL,bytes=0 WHERE state='claimed'`); err != nil {
		return nil, err
	}
	return s, nil
}

func validStoragePolicy(p StoragePolicy) bool {
	return p.RetentionDays >= 1 && p.RetentionDays <= 30 && p.GlobalBytes >= 64<<20 && p.GlobalBytes <= 1<<40 && p.BindingBytes >= 16<<20 && p.BindingBytes <= p.GlobalBytes
}
func (s *durableStore) seal(data []byte, aad string) ([]byte, error) {
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, data, []byte("v1:"+aad)), nil
}
func (s *durableStore) open(data []byte, aad string) ([]byte, error) {
	n := s.aead.NonceSize()
	if len(data) < n {
		return nil, errors.New("invalid ciphertext")
	}
	return s.aead.Open(nil, data[:n], data[n:], []byte("v1:"+aad))
}
func (s *durableStore) close() {
	if s != nil {
		_ = s.db.Close()
	}
}
func (s *durableStore) diskBytes() int64 {
	var total int64
	_ = filepath.WalkDir(s.dir, func(path string, d os.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			if info, e := d.Info(); e == nil {
				total += info.Size()
			}
		}
		return nil
	})
	return total
}
func (s *durableStore) capacityLocked(scope string, n int64) error {
	if s.diskBytes() >= s.policy.GlobalBytes*85/100 && nowMs()-s.lastCleanup > 30000 {
		if err := s.cleanupLocked(nowMs()); err != nil {
			return err
		}
	}
	var owned int64
	var binding string
	if s.db.QueryRow(`SELECT scope FROM quota_owners WHERE owner=?`, scope).Scan(&binding) == nil {
		scope = binding
	}
	if err := s.db.QueryRow(`SELECT COALESCE((SELECT SUM(bytes) FROM events WHERE scope=?),0)+COALESCE((SELECT SUM(bytes) FROM blobs WHERE scope=?),0)+COALESCE((SELECT SUM(bytes) FROM replies WHERE owner=? OR owner IN(SELECT owner FROM quota_owners WHERE scope=?)),0)+COALESCE((SELECT SUM(bytes) FROM thumbnails WHERE scope=?),0)+COALESCE((SELECT SUM(bytes) FROM offline_queue WHERE scope=?),0)`, scope, scope, scope, scope, scope, scope).Scan(&owned); err != nil {
		return err
	}
	if owned+n > s.policy.BindingBytes || s.diskBytes()+n+(4<<20) > s.policy.GlobalBytes {
		return errStorageCapacity
	}
	probe := s.volumeProbe
	if probe == nil {
		probe = storageVolume
	}
	if total, free, err := probe(s.dir); err == nil {
		reserve := total / 20
		if reserve < 2<<30 {
			reserve = 2 << 30
		}
		if free-n < reserve {
			return errStorageCapacity
		}
	}
	return nil
}

// Persist a bounded result before releasing its HTTP response. Even oversized
// or unknown results retain a terminal marker, without claiming a full body.
func (s *durableStore) saveReply(id, owner string, rep replyBody, status int, known bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	state := "uncertain"
	if known {
		state = "completed"
	}
	var data []byte
	var err error
	if known {
		data, err = json.Marshal(rep)
		if err != nil {
			return err
		}
		if len(data) > maxCachedReplyBytes {
			data = nil
			state = "truncated"
		}
	}
	if len(data) > 0 {
		if err = s.capacityLocked(owner, int64(len(data))); err != nil {
			data = nil
			state = "truncated"
		} else {
			data, err = s.seal(data, "reply:"+id)
			if err != nil {
				return err
			}
		}
	}
	_, err = s.db.Exec(`INSERT INTO replies(id,owner,body,status,expires,state,bytes) VALUES(?,?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body,status=excluded.status,expires=excluded.expires,state=excluded.state,bytes=excluded.bytes`, id, owner, data, status, nowMs()+int64(s.policy.RetentionDays)*durableDay, state, len(data))
	return err
}
func (s *durableStore) reply(id string) (replyBody, int, string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var rep replyBody
	var data []byte
	var status int
	var expires int64
	var state string
	err := s.db.QueryRow(`SELECT body,status,expires,state FROM replies WHERE id=?`, id).Scan(&data, &status, &expires, &state)
	if err != nil {
		return rep, 0, "", err
	}
	if expires <= nowMs() {
		return rep, status, "expired", nil
	}
	if len(data) > 0 {
		data, err = s.open(data, "reply:"+id)
		if err == nil {
			err = json.Unmarshal(data, &rep)
		}
	}
	return rep, status, state, err
}

func (h *Hub) DurableStorageStatus() StorageStatus {
	s := h.durable
	if s == nil {
		return StorageStatus{}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	out := StorageStatus{Enabled: true, Policy: s.policy, DiskBytes: s.diskBytes(), LastCleanupAt: s.lastCleanup}
	_ = s.db.QueryRow(`SELECT COUNT(*),COALESCE(SUM(bytes),0) FROM events`).Scan(&out.EventCount, &out.PayloadBytes)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM operations`).Scan(&out.OperationCount)
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM blobs`).Scan(&out.BlobCount)
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(bytes),0) FROM blobs`).Scan(&out.ImageBytes)
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(bytes),0) FROM thumbnails`).Scan(&out.ThumbnailBytes)
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(bytes),0) FROM replies`).Scan(&out.ReplyBytes)
	_ = s.db.QueryRow(`SELECT COALESCE(SUM(bytes),0) FROM offline_queue`).Scan(&out.QueueBytes)
	out.PayloadBytes += out.ImageBytes + out.ThumbnailBytes + out.ReplyBytes + out.QueueBytes
	out.Warning = out.DiskBytes >= s.policy.GlobalBytes*70/100
	probe := s.volumeProbe
	if probe == nil {
		probe = storageVolume
	}
	if total, free, err := probe(s.dir); err == nil {
		out.FreeDiskBytes = free
		out.ReservedDiskBytes = total / 20
		if out.ReservedDiskBytes < 2<<30 {
			out.ReservedDiskBytes = 2 << 30
		}
		out.LowDisk = free < out.ReservedDiskBytes
		out.Warning = out.Warning || out.LowDisk
	}
	return out
}
func (h *Hub) SetStoragePolicy(p StoragePolicy) error {
	if !validStoragePolicy(p) {
		return errors.New("invalid storage policy")
	}
	s := h.durable
	if s == nil {
		return errors.New("storage unavailable")
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, _ := json.Marshal(p)
	if _, err := s.db.Exec(`INSERT INTO meta(name,value) VALUES('policy',?) ON CONFLICT(name) DO UPDATE SET value=excluded.value`, string(raw)); err != nil {
		return err
	}
	s.policy = p
	return nil
}
