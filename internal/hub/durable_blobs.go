package hub

import (
	"bytes"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"

	_ "golang.org/x/image/webp"
)

const blobMaxBytes = 8 << 20
const blobChunkBytes = 150000 // preserves the existing phone base64 chunk contract
var durableAttachmentID = regexp.MustCompile(`^att_[a-f0-9]{32}$`)
var errBlobConflict = errors.New("attachment identity conflict")
var errBlobInvalid = errors.New("invalid image upload")

type durableBlobInfo struct {
	ID        string `json:"id"`
	BlobID    string `json:"blobId"`
	Mime      string `json:"mime"`
	Size      int64  `json:"size"`
	Width     int    `json:"width"`
	Height    int    `json:"height"`
	ExpiresAt int64  `json:"expiresAt"`
	Hash      string `json:"sha256"`
}

func validImageMIME(m string) bool { return m == "image/png" || m == "image/jpeg" || m == "image/webp" }

func (s *durableStore) blobInfo(scope, id string) (durableBlobInfo, string, error) {
	var b durableBlobInfo
	var state string
	err := s.db.QueryRow(`SELECT id,mime,bytes,width,height,expires,hash,state FROM blobs WHERE scope=? AND id=?`, scope, id).Scan(&b.ID, &b.Mime, &b.Size, &b.Width, &b.Height, &b.ExpiresAt, &b.Hash, &state)
	b.BlobID = b.ID
	return b, state, err
}

// Every uploaded part is independently authenticated and flushed before ACK.
// Restart recovery never needs to decode or accumulate the complete image.
func (s *durableStore) putBlobPart(scope, id, mime string, index, total int, data []byte) (bool, error) {
	if !durableAttachmentID.MatchString(id) || !validImageMIME(mime) || total < 1 || total > 64 || index < 0 || index >= total || len(data) == 0 || len(data) > 256<<10 {
		return false, errBlobInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var state, oldMime, oldScope string
	var oldTotal int
	var size int64
	err := s.db.QueryRow(`SELECT scope,state,mime,total,bytes FROM blobs WHERE id=?`, id).Scan(&oldScope, &state, &oldMime, &oldTotal, &size)
	if errors.Is(err, sql.ErrNoRows) {
		var pending int
		if err = s.db.QueryRow(`SELECT COUNT(*) FROM blobs WHERE state='uploading'`).Scan(&pending); err != nil {
			return false, err
		}
		if pending >= 64 {
			return false, errStorageCapacity
		}
		if err = s.capacityLocked(scope, int64(len(data))+256); err != nil {
			return false, err
		}
		_, err = s.db.Exec(`INSERT INTO blobs(id,scope,state,bytes,mime,created,expires,total) VALUES(?,?,'uploading',0,?,?,?,?)`, id, scope, mime, nowMs(), nowMs()+int64(time.Hour/time.Millisecond), total)
		oldScope, oldMime, oldTotal, state = scope, mime, total, "uploading"
	}
	if err != nil {
		return false, err
	}
	if oldScope != scope || oldMime != mime || oldTotal != total || (state != "ready" && state != "uploading") {
		return false, errBlobConflict
	}
	digest := sha256.Sum256(data)
	digestText := hex.EncodeToString(digest[:])
	var oldDigest string
	var oldSize int
	err = s.db.QueryRow(`SELECT digest,bytes FROM blob_parts WHERE id=? AND part=?`, id, index).Scan(&oldDigest, &oldSize)
	if err == nil {
		if oldDigest != digestText {
			return false, errBlobConflict
		}
		if state == "uploading" {
			// A crash can occur after the last part commits but before the ready
			// marker is written. Repeating that part must finish verification.
			var count int
			if err = s.db.QueryRow(`SELECT COUNT(*) FROM blob_parts WHERE id=?`, id).Scan(&count); err != nil {
				return false, err
			}
			if count == total {
				return s.finalizeBlobLocked(scope, id, mime)
			}
		}
		return state == "ready", nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if state == "ready" {
		return false, errBlobConflict
	}
	if size+int64(len(data)) > blobMaxBytes {
		return false, errBlobInvalid
	}
	if err = s.capacityLocked(scope, int64(len(data))+64); err != nil {
		return false, err
	}
	encrypted, err := s.seal(data, "blob:"+scope+":"+id+":"+strconv.Itoa(index))
	if err != nil {
		return false, err
	}
	tx, err := s.db.Begin()
	if err != nil {
		return false, err
	}
	defer tx.Rollback()
	if _, err = tx.Exec(`INSERT INTO blob_parts(id,part,body,bytes,digest) VALUES(?,?,?,?,?)`, id, index, encrypted, len(data), digestText); err != nil {
		return false, err
	}
	if _, err = tx.Exec(`UPDATE blobs SET bytes=bytes+?,expires=? WHERE id=? AND state='uploading'`, len(data), nowMs()+int64(time.Hour/time.Millisecond), id); err != nil {
		return false, err
	}
	if err = tx.Commit(); err != nil {
		return false, err
	}
	var count int
	if err = s.db.QueryRow(`SELECT COUNT(*) FROM blob_parts WHERE id=?`, id).Scan(&count); err != nil {
		return false, err
	}
	if count != total {
		return false, nil
	}
	return s.finalizeBlobLocked(scope, id, mime)
}

type durableBlobReader struct {
	s            *durableStore
	scope, id    string
	index, total int
	part         *bytes.Reader
}

func (b *durableBlobReader) Read(p []byte) (int, error) {
	for b.part == nil || b.part.Len() == 0 {
		if b.index >= b.total {
			return 0, io.EOF
		}
		var data []byte
		if err := b.s.db.QueryRow(`SELECT body FROM blob_parts WHERE id=? AND part=?`, b.id, b.index).Scan(&data); err != nil {
			return 0, err
		}
		data, err := b.s.open(data, "blob:"+b.scope+":"+b.id+":"+strconv.Itoa(b.index))
		if err != nil {
			return 0, err
		}
		b.index++
		b.part = bytes.NewReader(data)
	}
	return b.part.Read(p)
}
func (s *durableStore) blobReader(scope, id string) (*durableBlobReader, error) {
	var total int
	err := s.db.QueryRow(`SELECT total FROM blobs WHERE id=? AND scope=?`, id, scope).Scan(&total)
	return &durableBlobReader{s: s, scope: scope, id: id, total: total}, err
}
func (s *durableStore) finalizeBlobLocked(scope, id, mime string) (bool, error) {
	reader, err := s.blobReader(scope, id)
	if err != nil {
		return false, err
	}
	hash := sha256.New()
	tee := io.TeeReader(reader, hash)
	cfg, format, err := image.DecodeConfig(tee)
	if err != nil || cfg.Width <= 0 || cfg.Height <= 0 || int64(cfg.Width)*int64(cfg.Height) > 16_000_000 || "image/"+format != mime {
		_, _ = s.db.Exec(`UPDATE blobs SET state='rejected' WHERE id=? AND scope=?`, id, scope)
		return false, errBlobInvalid
	}
	if _, err = io.CopyBuffer(io.Discard, tee, make([]byte, 256<<10)); err != nil {
		return false, err
	}
	_, err = s.db.Exec(`UPDATE blobs SET state='ready',hash=?,width=?,height=?,expires=?,lease=0 WHERE id=? AND scope=?`, hex.EncodeToString(hash.Sum(nil)), cfg.Width, cfg.Height, nowMs()+durableDay, id, scope)
	return err == nil, err
}

// Reference lifetime is independent of reads and retries. Only authenticated
// commands/events add refs; the last expired ref starts a separate 24h grace.
func (s *durableStore) addBlobRefs(tx *sql.Tx, scope string, raw []byte, ref string, expires int64) error {
	var value any
	if json.Unmarshal(raw, &value) != nil {
		return nil
	}
	ids := map[string]bool{}
	var walk func(any, int)
	walk = func(v any, depth int) {
		if depth > 8 || len(ids) > 64 {
			return
		}
		switch x := v.(type) {
		case map[string]any:
			for key, item := range x {
				if key == "attachments" {
					if list, ok := item.([]any); ok {
						for _, a := range list {
							switch att := a.(type) {
							case string:
								if durableAttachmentID.MatchString(att) {
									ids[att] = true
								}
							case map[string]any:
								if id, ok := att["id"].(string); ok && durableAttachmentID.MatchString(id) {
									ids[id] = true
								}
							}
						}
					}
				} else {
					walk(item, depth+1)
				}
			}
		case []any:
			for _, item := range x {
				walk(item, depth+1)
			}
		}
	}
	walk(value, 0)
	refs := []string{ref}
	if obj, ok := value.(map[string]any); ok {
		if session, ok := obj["sessionKey"].(string); ok && session != "" {
			refs = append(refs, "session:"+shaHex(session))
		}
	}
	for id := range ids {
		result, err := tx.Exec(`INSERT INTO blob_refs(scope,id,ref,expires) SELECT ?,?,?,? WHERE EXISTS(SELECT 1 FROM blobs WHERE scope=? AND id=? AND state='ready') ON CONFLICT(scope,id,ref) DO NOTHING`, scope, id, ref, expires, scope, id)
		if err != nil {
			return err
		}
		inserted, _ := result.RowsAffected()
		if inserted == 0 {
			continue
		}
		for _, reference := range refs[1:] {
			if _, err := tx.Exec(`INSERT INTO blob_refs(scope,id,ref,expires) VALUES(?,?,?,?) ON CONFLICT(scope,id,ref) DO UPDATE SET expires=MAX(expires,excluded.expires)`, scope, id, reference, expires); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *durableStore) retainCommandBlobs(scope string, raw []byte, ref string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = s.addBlobRefs(tx, scope, raw, ref, nowMs()+int64(s.policy.RetentionDays)*durableDay); err != nil {
		return err
	}
	return tx.Commit()
}

func (h *Hub) handleBlobs(w http.ResponseWriter, r *http.Request, acct string) {
	s := h.durable
	if s == nil {
		writeError(w, 503, "持久存储未启用")
		return
	}
	q := r.URL.Query()
	deviceID := q.Get("deviceId")
	var scope string
	var authorized bool
	bridge := strings.Contains(r.URL.Path, "/bridge/")
	if bridge {
		h.mu.Lock()
		if a := h.accounts[acct]; a != nil {
			if d := a.devices[deviceID]; d != nil && !d.banned && h.matchesReadyDeviceSecret(d, r) && (d.bindingID == "" || d.hasPair && r.Header.Get("X-Salcara-Binding-Id") == d.bindingID && r.Header.Get("X-Salcara-Phone-Hash") == d.phoneHash) {
				if !h.eventScopeMatchesLocked(acct, d, r.Header.Get("X-Salcara-Event-Scope")) {
					h.mu.Unlock()
					writeEventScopeChanged(w)
					return
				}
				scope = h.durableScopeLocked(acct, d)
				authorized = true
			}
		}
		h.mu.Unlock()
	} else {
		_, scope, authorized = h.appStorageIdentity(acct, r, deviceID)
	}
	if !authorized {
		writeError(w, 403, "图片授权已失效")
		return
	}
	if r.Method == http.MethodGet {
		id := q.Get("id")
		if !durableAttachmentID.MatchString(id) {
			writeError(w, 400, "图片编号无效")
			return
		}
		if q.Get("variant") == "thumbnail" {
			data, err := s.thumbnail(scope, id)
			if err != nil {
				writeBlobError(w, err)
				return
			}
			w.Header().Set("Content-Type", "image/jpeg")
			w.Header().Set("Content-Length", strconv.Itoa(len(data)))
			w.Header().Set("Cache-Control", "private, no-store")
			_, _ = w.Write(data)
			return
		}
		s.mu.Lock()
		info, state, err := s.blobInfo(scope, id)
		if err != nil || state != "ready" || !s.blobRetainedLocked(scope, id, info.ExpiresAt) {
			s.mu.Unlock()
			writeJSON(w, 410, map[string]any{"code": "attachment_expired", "error": "图片已过期"})
			return
		}
		// The lease protects an in-flight stream from a concurrent cleanup pass.
		_, err = s.db.Exec(`UPDATE blobs SET lease=MAX(lease,?) WHERE id=?`, nowMs()+int64(2*time.Minute/time.Millisecond), id)
		s.mu.Unlock()
		if err != nil {
			writeError(w, 503, "图片暂时不可用")
			return
		}
		reader, err := s.blobReader(scope, id)
		if err != nil {
			writeError(w, 503, "图片暂时不可用")
			return
		}
		w.Header().Set("Content-Type", info.Mime)
		w.Header().Set("Content-Length", strconv.FormatInt(info.Size, 10))
		w.Header().Set("Cache-Control", "private, no-store")
		_, _ = io.CopyBuffer(w, reader, make([]byte, 256<<10))
		return
	}
	if r.Method != http.MethodPost {
		writeError(w, 405, "method not allowed")
		return
	}
	select {
	case s.uploadSlots <- struct{}{}:
		defer func() { <-s.uploadSlots }()
	default:
		writeError(w, 429, "同时上传图片过多，请稍后重试")
		return
	}
	mime := strings.TrimSpace(strings.Split(r.Header.Get("Content-Type"), ";")[0])
	id := q.Get("attachmentId")
	if id == "" {
		id = "att_" + newID()
	}
	if !validImageMIME(mime) || !durableAttachmentID.MatchString(id) || r.ContentLength <= 0 || r.ContentLength > blobMaxBytes {
		writeError(w, 400, "图片格式或长度无效")
		return
	}
	rc := http.NewResponseController(w)
	_ = rc.SetReadDeadline(time.Now().Add(time.Minute))
	defer rc.SetReadDeadline(time.Time{})
	r.Body = http.MaxBytesReader(w, r.Body, blobMaxBytes)
	// Raw callers and phone chunk callers use different chunk sizes. A ready
	// object is compared by whole-content digest, independent of chunk layout.
	info, state, existingErr := s.blobInfo(scope, id)
	if existingErr == nil && state == "ready" {
		hash := sha256.New()
		n, err := io.CopyBuffer(hash, r.Body, make([]byte, 256<<10))
		if err != nil || n != info.Size || info.Mime != mime || hex.EncodeToString(hash.Sum(nil)) != info.Hash {
			writeError(w, 409, "图片编号已用于其他内容")
			return
		}
		writeJSON(w, 200, info)
		return
	}
	total := int((r.ContentLength + (256 << 10) - 1) / (256 << 10))
	remaining := r.ContentLength
	for index := 0; index < total; index++ {
		n := int64(256 << 10)
		if remaining < n {
			n = remaining
		}
		data := make([]byte, int(n))
		if _, err := io.ReadFull(r.Body, data); err != nil {
			writeError(w, 400, "图片上传未完成")
			return
		}
		remaining -= n
		if _, err := s.putBlobPart(scope, id, mime, index, total, data); err != nil {
			writeBlobError(w, err)
			return
		}
	}
	var extra [1]byte
	if n, err := r.Body.Read(extra[:]); n > 0 || err != io.EOF {
		writeError(w, 400, "图片长度不符")
		return
	}
	info, state, err := s.blobInfo(scope, id)
	if err != nil || state != "ready" {
		writeError(w, 503, "图片尚未完成保存")
		return
	}
	writeJSON(w, 200, info)
}
func writeBlobError(w http.ResponseWriter, err error) {
	status := 503
	code := "attachment_storage_unavailable"
	message := "图片尚未保存，请重试"
	if errors.Is(err, errBlobInvalid) {
		status = 400
		code = "attachment_invalid"
		message = "图片内容、尺寸或类型无效（最大 8 MB、1600 万像素）"
	}
	if errors.Is(err, errBlobConflict) {
		status = 409
		code = "attachment_conflict"
		message = "图片编号或分片与原内容不一致"
	}
	if errors.Is(err, errStorageCapacity) {
		status = 507
		code = "storage_capacity"
		message = "图片存储容量不足，请稍后重试"
	}
	writeJSON(w, status, map[string]any{"code": code, "error": message})
}
func (s *durableStore) blobRetainedLocked(scope, id string, expires int64) bool {
	if expires > nowMs() {
		return true
	}
	var count int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM blob_refs WHERE scope=? AND id=? AND expires>? AND ref NOT LIKE 'session:%'`, scope, id, nowMs()).Scan(&count)
	return count > 0
}

func (h *Hub) handleStoredAttachment(w http.ResponseWriter, r *http.Request, acct string, req commandReq) bool {
	if h.durable == nil {
		return false
	}
	var in struct {
		Type       string `json:"type"`
		ID         string `json:"id"`
		Mime       string `json:"mime"`
		Index      int    `json:"index"`
		Total      int    `json:"total"`
		Data       string `json:"data"`
		SessionKey string `json:"sessionKey"`
		Offset     int    `json:"offset"`
		Length     int    `json:"length"`
		Variant    string `json:"variant"`
	}
	if json.Unmarshal(req.Command, &in) != nil || in.Type != "attachment.put" && in.Type != "attachment.read" {
		return false
	}
	_, scope, ok := h.appStorageIdentity(acct, r, req.DeviceID)
	if !ok {
		writeError(w, 403, "图片授权已失效")
		return true
	}
	if in.Type == "attachment.put" {
		select {
		case h.durable.uploadSlots <- struct{}{}:
			defer func() { <-h.durable.uploadSlots }()
		default:
			writeError(w, 429, "同时上传图片过多，请稍后重试")
			return true
		}
		if len(in.Data) > 200000 {
			writeBlobError(w, errBlobInvalid)
			return true
		}
		data, err := base64.StdEncoding.DecodeString(in.Data)
		if err != nil {
			writeBlobError(w, errBlobInvalid)
			return true
		}
		complete, err := h.durable.putBlobPart(scope, in.ID, in.Mime, in.Index, in.Total, data)
		if err != nil {
			writeBlobError(w, err)
			return true
		}
		// The part digest is the durable idempotency fence. Release its verified
		// ephemeral grant so a 54-part image cannot exhaust 32 acquisition slots.
		// A later upload call reacquires authority and compares the same bytes.
		if req.SourceGrant != nil {
			h.remoteAccessGate.RLock()
			if code, _ := h.executionSourceFor(acct, r, req); code == "" {
				if snapshot, e := sourceGrantSnapshot(*req.SourceGrant); e == nil {
					h.mu.Lock()
					challenge := h.sourceChallenges[snapshot.ChallengeID]
					if challenge != nil && challenge.DeviceID == req.DeviceID && challenge.RequestID == req.RequestID && challenge.CommandHash == snapshot.CommandHash && challenge.BindingID == snapshot.BindingID {
						delete(h.sourceChallenges, snapshot.ChallengeID)
					}
					h.mu.Unlock()
				}
			}
			h.remoteAccessGate.RUnlock()
		}
		writeJSON(w, 200, map[string]any{"ok": true, "result": map[string]any{"id": in.ID, "complete": complete}})
		return true
	}
	s := h.durable
	s.mu.Lock()
	info, state, err := s.blobInfo(scope, in.ID)
	if errors.Is(err, sql.ErrNoRows) {
		s.mu.Unlock()
		if in.Variant == "thumbnail" {
			writeJSON(w, 410, map[string]any{"code": "thumbnail_unavailable", "error": "此图片暂时没有可用缩略图"})
			return true
		}
		return false
	} // legacy Bridge-only images
	if err != nil || state != "ready" || !s.blobRetainedLocked(scope, in.ID, info.ExpiresAt) {
		s.mu.Unlock()
		writeJSON(w, 410, map[string]any{"code": "attachment_expired", "error": "图片已过期"})
		return true
	}
	var bound int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM blob_refs WHERE scope=? AND id=? AND ref=? AND expires>?`, scope, in.ID, "session:"+shaHex(in.SessionKey), nowMs()).Scan(&bound)
	if in.SessionKey == "" || bound == 0 {
		s.mu.Unlock()
		writeError(w, 403, "图片不属于此会话")
		return true
	}
	if in.Variant == "thumbnail" {
		s.mu.Unlock()
		data, err := s.thumbnail(scope, in.ID)
		if err != nil {
			writeBlobError(w, err)
			return true
		}
		if in.Offset < 0 || in.Offset > len(data) {
			writeError(w, 400, "图片偏移无效")
			return true
		}
		if in.Length <= 0 || in.Length > 90000 {
			in.Length = 90000
		}
		next := in.Offset + in.Length
		if next > len(data) {
			next = len(data)
		}
		cfg, _, err := image.DecodeConfig(bytes.NewReader(data))
		if err != nil {
			writeError(w, 503, "缩略图暂时不可用")
			return true
		}
		digest := sha256.Sum256(data)
		writeJSON(w, 200, map[string]any{"ok": true, "result": map[string]any{"id": in.ID, "variant": "thumbnail", "mime": "image/jpeg", "size": len(data), "width": cfg.Width, "height": cfg.Height, "sha256": hex.EncodeToString(digest[:]), "offset": in.Offset, "nextOffset": next, "complete": next >= len(data), "data": base64.StdEncoding.EncodeToString(data[in.Offset:next])}})
		return true
	}
	if in.Variant != "" && in.Variant != "original" {
		s.mu.Unlock()
		writeError(w, 400, "图片版本无效")
		return true
	}
	if in.Offset < 0 || in.Offset > int(info.Size) {
		s.mu.Unlock()
		writeError(w, 400, "图片偏移无效")
		return true
	}
	if in.Length <= 0 || in.Length > 90000 {
		in.Length = 90000
	}
	_, err = s.db.Exec(`UPDATE blobs SET lease=MAX(lease,?) WHERE id=?`, nowMs()+int64(2*time.Minute/time.Millisecond), in.ID)
	s.mu.Unlock()
	if err != nil {
		writeError(w, 503, "图片暂时不可用")
		return true
	}
	reader, err := s.blobReader(scope, in.ID)
	if err != nil {
		writeError(w, 503, "图片暂时不可用")
		return true
	}
	if _, err = io.CopyN(io.Discard, reader, int64(in.Offset)); err != nil {
		writeError(w, 503, "图片暂时不可用")
		return true
	}
	data, err := io.ReadAll(io.LimitReader(reader, int64(in.Length)))
	if err != nil {
		writeError(w, 503, "图片暂时不可用")
		return true
	}
	next := in.Offset + len(data)
	writeJSON(w, 200, map[string]any{"ok": true, "result": map[string]any{"id": in.ID, "mime": info.Mime, "size": info.Size, "offset": in.Offset, "nextOffset": next, "complete": int64(next) >= info.Size, "data": base64.StdEncoding.EncodeToString(data)}})
	return true
}
