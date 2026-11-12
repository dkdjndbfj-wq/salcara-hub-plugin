package hub

import "database/sql"

type cleanupBudget struct {
	records int
	bytes   int64
}

func (b *cleanupBudget) take(size int64) bool {
	size += 128
	if b.records == 0 || size > b.bytes {
		return false
	}
	b.records--
	b.bytes -= size
	return true
}

// Each pass removes at most 500 logical items and 50 MiB of retained content,
// including reply bodies and thumbnails. Restart resumes marked blob deletes.
func (h *Hub) RunStorageCleanup() error {
	if h.durable == nil {
		return nil
	}
	s := h.durable
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cleanupLocked(nowMs())
}
func (s *durableStore) cleanupLocked(now int64) error {
	budget := cleanupBudget{records: 500, bytes: 50 << 20}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	type deadEvent struct {
		scope, session string
		seq, size      int64
	}
	rows, err := tx.Query(`SELECT scope,seq,session,bytes FROM events WHERE expires<=? ORDER BY expires LIMIT 500`, now)
	if err != nil {
		return err
	}
	var events []deadEvent
	for rows.Next() {
		var e deadEvent
		if err = rows.Scan(&e.scope, &e.seq, &e.session, &e.size); err != nil {
			rows.Close()
			return err
		}
		if !budget.take(e.size) {
			break
		}
		events = append(events, e)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, e := range events {
		if _, err = tx.Exec(`UPDATE event_heads SET evicted=MAX(evicted,?) WHERE scope=? AND (session=? OR session=?)`, e.seq, e.scope, e.session, shaHex("")); err != nil {
			return err
		}
		if _, err = tx.Exec(`DELETE FROM events WHERE scope=? AND seq=?`, e.scope, e.seq); err != nil {
			return err
		}
	}
	rows, err = tx.Query(`SELECT rowid,scope,id FROM blob_refs WHERE expires<=? LIMIT ?`, now, budget.records)
	if err != nil {
		return err
	}
	type deadRef struct {
		row       int64
		scope, id string
	}
	var refs []deadRef
	for rows.Next() {
		var r deadRef
		if err = rows.Scan(&r.row, &r.scope, &r.id); err != nil {
			rows.Close()
			return err
		}
		if !budget.take(256) {
			break
		}
		refs = append(refs, r)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, r := range refs {
		if _, err = tx.Exec(`DELETE FROM blob_refs WHERE rowid=?`, r.row); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE blobs SET expires=MAX(expires,?) WHERE id=? AND scope=? AND state='ready' AND NOT EXISTS(SELECT 1 FROM blob_refs WHERE id=? AND scope=? AND ref NOT LIKE 'session:%')`, now+durableDay, r.id, r.scope, r.id, r.scope); err != nil {
			return err
		}
	}
	// Bodies are removed without erasing the anti-replay identity.
	for _, lane := range []struct{ query, remove string }{
		{`SELECT id,bytes FROM replies WHERE expires<=? AND body IS NOT NULL LIMIT ?`, `UPDATE replies SET body=NULL,bytes=0,state=CASE WHEN state IN ('uncertain','truncated','cancelled','dispatch_expired','queue_unavailable') THEN state ELSE 'expired' END WHERE id=?`},
		{`SELECT id,COALESCE(length(body),0) FROM push_jobs WHERE expires<=? LIMIT ?`, `DELETE FROM push_jobs WHERE id=?`},
		{`SELECT id,length(body) FROM push_notices WHERE expires<=? LIMIT ?`, `DELETE FROM push_notices WHERE id=?`},
		{`SELECT rowid,0 FROM event_budgets WHERE expires<=? LIMIT ?`, `DELETE FROM event_budgets WHERE rowid=?`},
		{`SELECT id,bytes FROM thumbnails WHERE expires<=? LIMIT ?`, `DELETE FROM thumbnails WHERE id=?`},
	} {
		if err = purgeMailboxLane(tx, &budget, lane.query, lane.remove, now); err != nil {
			return err
		}
	}
	if err = tx.Commit(); err != nil {
		return err
	}
	// Reserve room for the complete logical blob (including its thumbnail).
	rows, err = s.db.Query(`SELECT b.id,b.bytes+65536+COALESCE((SELECT bytes FROM thumbnails t WHERE t.id=b.id),0) FROM blobs b WHERE b.lease<=? AND (b.state='deleting' OR b.expires<=? AND NOT EXISTS(SELECT 1 FROM blob_refs r WHERE r.id=b.id AND r.scope=b.scope AND r.ref NOT LIKE 'session:%')) ORDER BY b.expires LIMIT ?`, now, now, budget.records)
	if err != nil {
		return err
	}
	var ids []string
	for rows.Next() {
		var id string
		var size int64
		if err = rows.Scan(&id, &size); err != nil {
			rows.Close()
			return err
		}
		if !budget.take(size) {
			break
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err = s.db.Exec(`UPDATE blobs SET state='deleting' WHERE id=?`, id); err != nil {
			return err
		}
		tx, err = s.db.Begin()
		if err != nil {
			return err
		}
		for _, q := range []string{`DELETE FROM blob_parts WHERE id=?`, `DELETE FROM thumbnails WHERE id=?`, `DELETE FROM blob_refs WHERE id=?`, `DELETE FROM blobs WHERE id=? AND state='deleting'`} {
			if _, err = tx.Exec(q, id); err != nil {
				break
			}
		}
		if err == nil {
			err = tx.Commit()
		} else {
			_ = tx.Rollback()
		}
		if err != nil {
			return err
		}
	}
	s.lastCleanup = now
	if _, err = s.db.Exec(`PRAGMA wal_checkpoint(TRUNCATE)`); err != nil {
		return err
	}
	_, err = s.db.Exec(`PRAGMA incremental_vacuum(2048)`)
	return err
}
func purgeMailboxLane(tx *sql.Tx, budget *cleanupBudget, query, remove string, now int64) error {
	rows, err := tx.Query(query, now, budget.records)
	if err != nil {
		return err
	}
	var ids []any
	for rows.Next() {
		var id any
		var size int64
		if err = rows.Scan(&id, &size); err != nil {
			rows.Close()
			return err
		}
		if !budget.take(size) {
			break
		}
		ids = append(ids, id)
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return err
	}
	for _, id := range ids {
		if _, err = tx.Exec(remove, id); err != nil {
			return err
		}
	}
	return nil
}
