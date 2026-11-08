package hub

import (
	"bytes"
	"database/sql"
	"errors"
	"image"
	"image/color"
	"image/jpeg"
	"time"

	"golang.org/x/image/draw"
)

// One decoder per server; only 8-bit images of <=16 MP enter it. Worst-case
// decoded RGBA (64 MB), 512-square preview (1 MiB), encoder and chunk buffers fit the
// 96 MiB worker budget. 16-bit PNGs remain valid originals but are not decoded
// for a preview. This is a bound by construction, not an RSS measurement.
func (s *durableStore) thumbnail(scope, id string) ([]byte, error) {
	s.mu.Lock()
	info, state, err := s.blobInfo(scope, id)
	if err != nil || state != "ready" || !s.blobRetainedLocked(scope, id, info.ExpiresAt) {
		s.mu.Unlock()
		return nil, sql.ErrNoRows
	}
	var cached []byte
	err = s.db.QueryRow(`SELECT body FROM thumbnails WHERE scope=? AND id=? AND expires>?`, scope, id, nowMs()).Scan(&cached)
	if err == nil {
		data, e := s.open(cached, "thumbnail:"+scope+":"+id)
		if e == nil {
			_, e = s.db.Exec(`UPDATE thumbnails SET accessed=?,expires=? WHERE id=?`, nowMs(), nowMs()+7*durableDay, id)
		}
		s.mu.Unlock()
		return data, e
	}
	s.mu.Unlock()
	select {
	case s.thumbnailSlots <- struct{}{}:
		defer func() { <-s.thumbnailSlots }()
	default:
		return nil, errStorageCapacity
	}
	if _, err = s.db.Exec(`UPDATE blobs SET lease=MAX(lease,?) WHERE scope=? AND id=? AND state='ready'`, nowMs()+int64(2*time.Minute/time.Millisecond), scope, id); err != nil {
		return nil, err
	}
	reader, err := s.blobReader(scope, id)
	if err != nil {
		return nil, err
	}
	cfg, _, err := image.DecodeConfig(reader)
	if err != nil {
		return nil, err
	}
	if int64(cfg.Width)*int64(cfg.Height) > 16_000_000 || cfg.Width < 1 || cfg.Height < 1 {
		return nil, errBlobInvalid
	}
	switch cfg.ColorModel.Convert(color.White).(type) {
	case color.Gray16, color.RGBA64, color.NRGBA64:
		return nil, errors.New("16-bit image preview exceeds decoder budget")
	}
	reader, err = s.blobReader(scope, id)
	if err != nil {
		return nil, err
	}
	source, _, err := image.Decode(reader)
	if err != nil {
		return nil, err
	}
	w, h := cfg.Width, cfg.Height
	if w > 512 || h > 512 {
		if w >= h {
			h = h * 512 / w
			w = 512
		} else {
			w = w * 512 / h
			h = 512
		}
		if w < 1 {
			w = 1
		}
		if h < 1 {
			h = 1
		}
	}
	preview := image.NewRGBA(image.Rect(0, 0, w, h))
	draw.ApproxBiLinear.Scale(preview, preview.Bounds(), source, source.Bounds(), draw.Src, nil)
	var encoded bytes.Buffer
	if err = jpeg.Encode(&encoded, preview, &jpeg.Options{Quality: 75}); err != nil {
		return nil, err
	}
	if encoded.Len() > 2<<20 {
		return nil, errBlobInvalid
	}
	data := encoded.Bytes()
	ciphertext, err := s.seal(data, "thumbnail:"+scope+":"+id)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	globalLimit := s.policy.GlobalBytes / 20
	if globalLimit > 128<<20 {
		globalLimit = 128 << 20
	}
	bindingLimit := int64(32 << 20)
	for {
		var all, owned int64
		err = s.db.QueryRow(`SELECT COALESCE(SUM(bytes),0),COALESCE(SUM(CASE WHEN scope=? THEN bytes ELSE 0 END),0) FROM thumbnails`, scope).Scan(&all, &owned)
		if err != nil {
			return nil, err
		}
		if all+int64(len(ciphertext)) <= globalLimit && owned+int64(len(ciphertext)) <= bindingLimit {
			break
		}
		query := `SELECT id FROM thumbnails ORDER BY accessed LIMIT 1`
		args := []any{}
		if owned+int64(len(ciphertext)) > bindingLimit {
			query = `SELECT id FROM thumbnails WHERE scope=? ORDER BY accessed LIMIT 1`
			args = append(args, scope)
		}
		var victim string
		if err = s.db.QueryRow(query, args...).Scan(&victim); err != nil {
			return nil, errStorageCapacity
		}
		if _, err = s.db.Exec(`DELETE FROM thumbnails WHERE id=?`, victim); err != nil {
			return nil, err
		}
	}
	if err = s.capacityLocked(scope, int64(len(ciphertext))); err != nil {
		return nil, err
	}
	_, err = s.db.Exec(`INSERT INTO thumbnails(id,scope,body,bytes,accessed,expires) VALUES(?,?,?,?,?,?) ON CONFLICT(id) DO UPDATE SET body=excluded.body,bytes=excluded.bytes,accessed=excluded.accessed,expires=excluded.expires`, id, scope, ciphertext, len(ciphertext), nowMs(), nowMs()+7*durableDay)
	return data, err
}
