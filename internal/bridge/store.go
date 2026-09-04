package bridge

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sync"
	"time"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"
)

// Store preserves the existing inbox schema. Each multi-step operation is serialized;
// SQLite transactions also make acknowledgement and wake-state changes atomic.
type Store struct {
	mu sync.Mutex
	db *sql.DB
}

func openStore(path string) (*Store, error) {
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if _, err := os.Lstat(p); err == nil {
			if _, err = owned(p, 0, true); err != nil {
				return nil, err
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
	}
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
	if err != nil {
		return nil, err
	}
	_ = unix.Close(fd)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000;
CREATE TABLE IF NOT EXISTS inbox (id TEXT PRIMARY KEY, wire_id TEXT NOT NULL, peer TEXT NOT NULL, received REAL NOT NULL, state TEXT NOT NULL, data TEXT NOT NULL, UNIQUE(peer,wire_id));
CREATE TABLE IF NOT EXISTS outbound (id TEXT PRIMARY KEY, created REAL NOT NULL, target TEXT NOT NULL, state TEXT NOT NULL, detail TEXT);
CREATE TABLE IF NOT EXISTS metadata (key TEXT PRIMARY KEY, value TEXT NOT NULL);`)
	if err != nil {
		closeQuietly(db)
		return nil, err
	}
	return &Store{db: db}, nil
}
func (s *Store) Close() error { return s.db.Close() }

type querier interface {
	Exec(string, ...any) (sql.Result, error)
	QueryRow(string, ...any) *sql.Row
	Query(string, ...any) (*sql.Rows, error)
}

func meta(q querier, key string, out any) error {
	var v string
	err := q.QueryRow("SELECT value FROM metadata WHERE key=?", key).Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return err
	}
	return json.Unmarshal([]byte(v), out)
}
func setMeta(q querier, key string, v any) error {
	_, err := q.Exec("INSERT OR REPLACE INTO metadata VALUES (?,?)", key, string(compact(v)))
	return err
}
func (s *Store) Meta(key string, out any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return meta(s.db, key, out)
}
func (s *Store) SetMeta(key string, v any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	return setMeta(s.db, key, v)
}
func count(q querier, state, after string) (int, error) {
	var row int64
	if after != "" {
		if err := q.QueryRow("SELECT rowid FROM inbox WHERE id=?", after).Scan(&row); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return 0, errors.New("inbox cursor is no longer available; restart the read without --after")
			}
			return 0, err
		}
	}
	var n int
	err := q.QueryRow("SELECT count(*) FROM inbox WHERE state=? AND rowid>?", state, row).Scan(&n)
	return n, err
}
func (s *Store) Count(state string) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return count(s.db, state, "")
}
func (s *Store) Accept(message Object, identity, state string) (accepted Object, reason string, evicted Object, err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, "", nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var exists int
	err = tx.QueryRow("SELECT 1 FROM inbox WHERE peer=? AND wire_id=?", identity, str(message, "wire_id")).Scan(&exists)
	if err == nil {
		return nil, "duplicate", nil, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return nil, "", nil, err
	}
	now := float64(time.Now().UnixNano()) / 1e9
	rows, err := tx.Query("SELECT data FROM inbox WHERE peer=? AND received>?", identity, now-10)
	if err != nil {
		return nil, "", nil, err
	}
	n := 0
	duplicate := false
	for rows.Next() {
		var raw string
		if err = rows.Scan(&raw); err != nil {
			closeQuietly(rows)
			return nil, "", nil, err
		}
		var old Object
		if err = json.Unmarshal([]byte(raw), &old); err != nil {
			closeQuietly(rows)
			return nil, "", nil, err
		}
		n++
		if old["body"] == message["body"] {
			duplicate = true
		}
	}
	err = rows.Err()
	closeQuietly(rows)
	if err != nil {
		return nil, "", nil, err
	}
	if duplicate {
		return nil, "duplicate", nil, nil
	}
	if n >= 20 {
		return nil, "rate_limit", nil, nil
	}
	n, err = count(tx, state, "")
	if err != nil {
		return nil, "", nil, err
	}
	if state == "unread" && n >= 50 {
		return nil, "full_queue", nil, nil
	}
	if state == "held" && n >= 100 {
		var id, raw string
		if err = tx.QueryRow("SELECT id,data FROM inbox WHERE state='held' ORDER BY received LIMIT 1").Scan(&id, &raw); err != nil {
			return nil, "", nil, err
		}
		if err = json.Unmarshal([]byte(raw), &evicted); err != nil {
			return nil, "", nil, err
		}
		if _, err = tx.Exec("UPDATE inbox SET state='expired' WHERE id=?", id); err != nil {
			return nil, "", nil, err
		}
	}
	accepted = clone(message)
	accepted["id"] = uuid.NewString()
	accepted["received_at"] = now
	_, err = tx.Exec("INSERT INTO inbox VALUES (?,?,?,?,?,?)", accepted["id"], accepted["wire_id"], identity, now, state, string(compact(accepted)))
	if err != nil {
		return nil, "", nil, err
	}
	err = tx.Commit()
	return
}
func readRows(q querier, state, after string, limit int) ([]Object, error) {
	var row int64
	if after != "" {
		if err := q.QueryRow("SELECT rowid FROM inbox WHERE id=?", after).Scan(&row); err != nil {
			return nil, errors.New("inbox cursor is no longer available; restart the read without --after")
		}
	}
	rows, err := q.Query("SELECT data,state FROM inbox WHERE state=? AND rowid>? ORDER BY rowid LIMIT ?", state, row, limit)
	if err != nil {
		return nil, err
	}
	defer closeQuietly(rows)
	items := []Object{}
	for rows.Next() {
		var raw, st string
		if err = rows.Scan(&raw, &st); err != nil {
			return nil, err
		}
		var m Object
		if err = json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, err
		}
		m["state"] = st
		items = append(items, m)
	}
	return items, rows.Err()
}
func (s *Store) Read(state string, limit int) ([]Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return readRows(s.db, state, "", limit)
}
func (s *Store) Page(state, after string, limit int, info Object) (Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	available, err := count(s.db, state, after)
	if err != nil {
		return nil, err
	}
	var row int64
	if after != "" {
		if err = s.db.QueryRow("SELECT rowid FROM inbox WHERE id=?", after).Scan(&row); err != nil {
			return nil, err
		}
	}
	rows, err := s.db.Query("SELECT data,state FROM inbox WHERE state=? AND rowid>? ORDER BY rowid LIMIT ?", state, row, limit)
	if err != nil {
		return nil, err
	}
	defer closeQuietly(rows)
	result := clone(info)
	messages := []Object{}
	result["messages"] = messages
	result["remaining"] = available
	result["has_more"] = available > 0
	result["next_after"] = nil
	size := len(compact(Object{"result": result})) + 129
	// Decode only as far as the byte budget, rather than materializing every
	// maximum-size message before deciding which ones fit in the response.
	for rows.Next() {
		var raw, state string
		if err = rows.Scan(&raw, &state); err != nil {
			return nil, err
		}
		var m Object
		if err = json.Unmarshal([]byte(raw), &m); err != nil {
			return nil, err
		}
		m["state"] = state
		n := len(compact(m)) + 1
		if size+n > MaxReadResponse {
			if len(messages) == 0 {
				return nil, errors.New("stored message exceeds the read response budget")
			}
			break
		}
		size += n
		messages = append(messages, m)
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	result["messages"] = messages
	result["remaining"] = available - len(messages)
	result["has_more"] = available > len(messages)
	if available > len(messages) {
		result["next_after"] = messages[len(messages)-1]["id"]
	}
	return result, nil
}
func (s *Store) Get(id string) (Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var raw, state string
	err := s.db.QueryRow("SELECT data,state FROM inbox WHERE id=?", id).Scan(&raw, &state)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var m Object
	if err = json.Unmarshal([]byte(raw), &m); err != nil {
		return nil, err
	}
	m["state"] = state
	return m, nil
}
func transition(q querier, ids []string, before, after string) ([]string, error) {
	changed := []string{}
	if after == "unread" {
		n, err := count(q, "unread", "")
		if err != nil {
			return nil, err
		}
		var releasing int
		seen := map[string]bool{}
		for _, id := range ids {
			if seen[id] {
				continue
			}
			seen[id] = true
			var st string
			err = q.QueryRow("SELECT state FROM inbox WHERE id=?", id).Scan(&st)
			if err != nil && !errors.Is(err, sql.ErrNoRows) {
				return nil, err
			}
			if st == before {
				releasing++
			}
		}
		if n+releasing > 50 {
			return nil, errors.New("accepted inbox is full; acknowledge messages first")
		}
	}
	for _, id := range ids {
		r, err := q.Exec("UPDATE inbox SET state=? WHERE id=? AND state=?", after, id, before)
		if err != nil {
			return nil, err
		}
		n, err := r.RowsAffected()
		if err != nil {
			return nil, err
		}
		if n > 0 {
			changed = append(changed, id)
		}
	}
	if len(changed) > 0 && before == "unread" {
		var rev int64
		if err := meta(q, "ack_revision", &rev); err != nil {
			return nil, err
		}
		if err := setMeta(q, "ack_revision", rev+1); err != nil {
			return nil, err
		}
		var wake Object
		if err := meta(q, "wake", &wake); err != nil {
			return nil, err
		}
		if wake["submitted"] == true && wake["needs_manual_action"] != true {
			if err := setMeta(q, "wake", nil); err != nil {
				return nil, err
			}
		}
	}
	n, err := count(q, "unread", "")
	if err != nil {
		return nil, err
	}
	if n == 0 {
		if err = setMeta(q, "wake", nil); err != nil {
			return nil, err
		}
		if err = setMeta(q, "delivery_waiting", nil); err != nil {
			return nil, err
		}
	}
	return changed, nil
}
func (s *Store) Transition(ids []string, before, after string) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	changed, err := transition(tx, ids, before, after)
	if err != nil {
		return nil, err
	}
	return changed, tx.Commit()
}
func (s *Store) Expire() ([]Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	items, err := readRows(tx, "held", "", 100)
	if err != nil {
		return nil, err
	}
	old := []Object{}
	ids := []string{}
	now := float64(time.Now().UnixNano()) / 1e9
	for _, m := range items {
		if number(m, "received_at", now) < now-300 {
			old = append(old, m)
			ids = append(ids, str(m, "id"))
		}
	}
	if _, err = transition(tx, ids, "held", "expired"); err != nil {
		return nil, err
	}
	for _, statement := range []struct {
		query string
		args  []any
	}{
		{"DELETE FROM inbox WHERE state NOT IN ('unread','held') AND received<?", []any{now - 604800}},
		{"DELETE FROM inbox WHERE id IN (SELECT id FROM inbox WHERE state NOT IN ('unread','held') ORDER BY received DESC LIMIT -1 OFFSET 1000)", nil},
		{"DELETE FROM outbound WHERE created<?", []any{now - 604800}},
	} {
		if _, err = tx.Exec(statement.query, statement.args...); err != nil {
			return nil, err
		}
	}
	return old, tx.Commit()
}
func (s *Store) Outgoing(frame Object, target Peer) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, err := s.db.Exec("INSERT INTO outbound VALUES (?,?,?,?,?)", frame["msg_id"], float64(time.Now().UnixNano())/1e9, string(compact(target)), "sending", nil)
	return err
}
func (s *Store) Sent(id, state string, detail any) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	clause := ""
	if state == "sent_unconfirmed" || state == "failed" {
		clause = " AND state IN ('sending','sent_unconfirmed')"
	}
	_, err := s.db.Exec("UPDATE outbound SET state=?,detail=? WHERE id=?"+clause, state, string(compact(detail)), id)
	return err
}
func (s *Store) SentStatus(id string) ([]Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	query := "SELECT id,created,target,state,detail FROM outbound ORDER BY created DESC LIMIT 20"
	args := []any{}
	if id != "" {
		query = "SELECT id,created,target,state,detail FROM outbound WHERE id=?"
		args = append(args, id)
	}
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer closeQuietly(rows)
	out := []Object{}
	for rows.Next() {
		var id, target, state string
		var created float64
		var detail sql.NullString
		if err = rows.Scan(&id, &created, &target, &state, &detail); err != nil {
			return nil, err
		}
		var t, d any
		if err = json.Unmarshal([]byte(target), &t); err != nil {
			return nil, err
		}
		if detail.Valid {
			if err = json.Unmarshal([]byte(detail.String), &d); err != nil {
				return nil, err
			}
		}
		out = append(out, Object{"id": id, "created": created, "target": t, "state": state, "detail": d})
	}
	return out, rows.Err()
}
func (s *Store) Correlate(frame Object, pid int, start string) error {
	switch str(frame, "status") {
	case "held", "declined", "expired", "released", "refused", "dropped", "accepted", "read":
	default:
		return nil
	}
	ids := []any{frame["orig_msg_id"]}
	if dropped, ok := frame["dropped_msg_ids"].([]any); ok {
		ids = append(ids, dropped...)
	}
	if len(ids) > 101 {
		ids = ids[:101]
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, item := range ids {
		id, ok := item.(string)
		if !ok {
			continue
		}
		var raw, current string
		err := s.db.QueryRow("SELECT target,state FROM outbound WHERE id=?", id).Scan(&raw, &current)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return err
		}
		var target Peer
		if err = json.Unmarshal([]byte(raw), &target); err != nil {
			return err
		}
		if target.PID == pid && target.Start == start {
			// An acknowledgement is terminal. Acceptance may arrive after a
			// later receipt because status frames use separate connections.
			if current == "read" {
				continue
			}
			if str(frame, "status") == "accepted" && current != "sending" && current != "sent_unconfirmed" && current != "held" && current != "released" {
				continue
			}
			if _, err = s.db.Exec("UPDATE outbound SET state=?,detail=? WHERE id=?", frame["status"], string(compact(frame)), id); err != nil {
				return err
			}
		}
	}
	return nil
}

// BeginWake records an ambiguous attempt before any potentially successful write.
// A crash at this boundary may require acknowledgement; it never causes a blind replay.
func (s *Store) BeginWake(id string) (Object, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()
	var existing Object
	if err = meta(tx, "wake", &existing); err != nil {
		return nil, err
	}
	n, err := count(tx, "unread", "")
	if err != nil {
		return nil, err
	}
	if n == 0 || existing != nil {
		return nil, nil
	}
	var rev int64
	if err = meta(tx, "ack_revision", &rev); err != nil {
		return nil, err
	}
	wake := Object{"id": id, "name": "cross_session_inbox_" + stringsWithoutHyphens(id), "state": "attempted", "submitted": false, "needs_manual_action": true, "ack_revision": rev, "created_at": time.Now().Unix()}
	if err = setMeta(tx, "wake", wake); err != nil {
		return nil, err
	}
	return wake, tx.Commit()
}
func stringsWithoutHyphens(s string) string {
	out := ""
	for _, c := range s {
		if c != '-' {
			out += string(c)
		}
	}
	return out
}
func (s *Store) FinishWake(id, state string, detail string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var wake Object
	if err = meta(tx, "wake", &wake); err != nil {
		return err
	}
	if str(wake, "id") != id {
		return nil
	}
	var rev int64
	if err = meta(tx, "ack_revision", &rev); err != nil {
		return err
	}
	n, err := count(tx, "unread", "")
	if err != nil {
		return err
	}
	if state == "rejected" {
		wake = nil
	} else {
		wake["state"] = state
		wake["detail"] = detail
		wake["submitted"] = state == "submitted" || state == "recorded"
		wake["needs_manual_action"] = state == "uncertain" || state == "attempted"
		if n == 0 || (wake["submitted"] == true && int64(number(wake, "ack_revision", -1)) != rev) {
			wake = nil
		}
	}
	if err = setMeta(tx, "wake", wake); err != nil {
		return err
	}
	return tx.Commit()
}
func (s *Store) ResetDelivery() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	var wake Object
	if err = meta(tx, "wake", &wake); err != nil {
		return err
	}
	if wake != nil {
		return fmt.Errorf("a notification is pending; read and acknowledge the inbox before changing delivery")
	}
	for _, key := range []string{"retry_at", "delivery_waiting", "delivery_error"} {
		if err = setMeta(tx, key, nil); err != nil {
			return err
		}
	}
	return tx.Commit()
}
