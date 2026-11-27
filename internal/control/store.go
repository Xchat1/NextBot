package control

import (
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"time"

	"github.com/gowebpki/jcs"
	_ "github.com/mattn/go-sqlite3"
)

type Store struct {
	DB  *sql.DB
	Dir string
}
type transaction = sql.Tx

var missing = sql.ErrNoRows

func Open(dir string) (*Store, error) {
	if err := os.MkdirAll(filepath.Join(dir, "objects"), 0700); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite3", filepath.Join(dir, "control.db")+"?_busy_timeout=5000&_txlock=immediate")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	err = initialize(db)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &Store{db, dir}, nil
}
func initialize(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version > 1 {
		return fmt.Errorf("unsupported database schema %d", version)
	}
	if _, err := db.Exec("PRAGMA journal_mode=WAL; PRAGMA synchronous=FULL; PRAGMA busy_timeout=5000; PRAGMA foreign_keys=ON"); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.Exec(`
CREATE TABLE IF NOT EXISTS meta (key TEXT PRIMARY KEY, value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS records (kind TEXT NOT NULL, id TEXT NOT NULL, parent TEXT NOT NULL, body BLOB NOT NULL, PRIMARY KEY(kind,id));
CREATE INDEX IF NOT EXISTS records_parent ON records(kind,parent,id);
CREATE TABLE IF NOT EXISTS events (seq INTEGER PRIMARY KEY, task TEXT NOT NULL, body BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS events_task ON events(task,seq);
CREATE TABLE IF NOT EXISTS receipts (scope TEXT NOT NULL, key TEXT NOT NULL, digest TEXT NOT NULL, status INTEGER NOT NULL, body BLOB NOT NULL, PRIMARY KEY(scope,key));
CREATE TABLE IF NOT EXISTS secrets (kind TEXT NOT NULL, hash TEXT NOT NULL, subject TEXT NOT NULL, expires INTEGER NOT NULL, PRIMARY KEY(kind,hash));
CREATE INDEX IF NOT EXISTS secrets_subject ON secrets(kind,subject);
CREATE TABLE IF NOT EXISTS write_slot (node TEXT PRIMARY KEY, attempt TEXT NOT NULL UNIQUE);
PRAGMA user_version=1;
`)
	if err != nil {
		return err
	}
	for k, v := range map[string]string{"instance": ID(), "owner": ID(), "stream": ID(), "seq": "0", "floor": "0", "fence": "0", "generation": "0", "pause_dispatch": "off"} {
		if _, err := tx.Exec("INSERT OR IGNORE INTO meta VALUES(?,?)", k, v); err != nil {
			return err
		}
	}
	return tx.Commit()
}
func (s *Store) Tx(fn func(*transaction) error) error {
	tx, err := s.DB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}
func ID() string    { return hex.EncodeToString(randBytes(16)) }
func token() string { return hex.EncodeToString(randBytes(32)) }
func randBytes(n int) []byte {
	v := make([]byte, n)
	if _, err := rand.Read(v); err != nil {
		panic(err)
	}
	return v
}
func Hash(b []byte) string { h := sha256.Sum256(b); return hex.EncodeToString(h[:]) }
func Digest(domain string, value any) string {
	padding := map[string]any{"domain": domain, "formatVersion": 1, "value": value}
	canonical, err := jcs.Transform(encode(padding))
	if err != nil {
		panic(err)
	}
	return "sha256:" + Hash(canonical)
}
func encode(value any) []byte {
	b, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return b
}
func decode[T any](v []byte) (T, error) { var t T; err := json.Unmarshal(v, &t); return t, err }
func now() time.Time                    { return time.Now().UTC() }
func meta(tx *transaction, key string) string {
	var s string
	if err := tx.QueryRow("SELECT value FROM meta WHERE key=?", key).Scan(&s); err != nil {
		panic(err)
	}
	return s
}
func setMeta(tx *transaction, key, value string) error {
	_, err := tx.Exec("UPDATE meta SET value=? WHERE key=?", value, key)
	return err
}
func inc(v string) string { n, _ := strconv.ParseInt(v, 10, 64); return strconv.FormatInt(n+1, 10) }
func put(tx *transaction, k, id, p string, value any) error {
	_, err := tx.Exec("INSERT INTO records VALUES(?,?,?,?) ON CONFLICT(kind,id) DO UPDATE SET parent=excluded.parent,body=excluded.body", k, id, p, encode(value))
	return err
}
func get[T any](tx *transaction, k, id string) (T, error) {
	var v T
	var b []byte
	err := tx.QueryRow("SELECT body FROM records WHERE kind=? AND id=?", k, id).Scan(&b)
	if err != nil {
		return v, err
	}
	return decode[T](b)
}
func list[T any](tx *transaction, k, p string) ([]T, error) {
	query := "SELECT body FROM records WHERE kind=?"
	args := []any{k}
	if p != "" {
		query += " AND parent=?"
		args = append(args, p)
	}
	rows, err := tx.Query(query+" ORDER BY id", args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []T{}
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		item, err := decode[T](b)
		if err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}
func emit(tx *transaction, kind string, t *Task, attempt string, data any) error {
	seq := inc(meta(tx, "seq"))
	e := Event{Stream: meta(tx, "stream"), Seq: seq, Type: kind, At: now(), AttemptID: attempt, Data: data}
	if t != nil {
		e.TaskID = t.ID
		e.Revision = t.Revision
	}
	if _, err := tx.Exec("INSERT INTO events VALUES(?,?,?)", seq, e.TaskID, encode(e)); err != nil {
		return err
	}
	return setMeta(tx, "seq", seq)
}
func change(tx *transaction, t *Task) error {
	t.Revision = inc(t.Revision)
	t.UpdatedAt = now()
	if err := put(tx, "task", t.ID, t.ProjectID, t); err != nil {
		return err
	}
	return emit(tx, "task_changed", t, value(t.AttemptID), map[string]any{"state": t.State, "blockers": t.Blockers, "currentAttemptId": t.AttemptID, "currentArtifactVersionId": t.ArtifactID})
}
func value(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}
func block(t *Task, kind, source, message string) bool {
	for index, b := range t.Blockers {
		if b.Kind == kind && b.Source == source {
			if b.Message == message {
				return false
			}
			t.Blockers[index].Message = message
			return true
		}
	}
	t.Blockers = append(t.Blockers, Blocker{kind, source, message})
	return true
}
func unblock(t *Task, source string) bool {
	out := []Blocker{}
	for _, b := range t.Blockers {
		if b.Source != source {
			out = append(out, b)
		}
	}
	changed := len(out) != len(t.Blockers)
	t.Blockers = out
	return changed
}
func terminal(state string) bool {
	return state == "completed" || state == "cancelled" || state == "failed"
}
func queue(t *Task) {
	t.State = "queued"
	t.QueuedAt = now()
	t.AttemptID = nil
	t.Blockers = []Blocker{}
	t.CancelID = ""
}
func items[T any](data []T) map[string]any { return map[string]any{"items": data, "nextCursor": nil} }
func newCommand(tx *transaction, kind string, t *Task, a *Attempt, payload map[string]any) (Command, error) {
	c := Command{ID: ID(), Type: kind, Phase: "pending", Payload: payload, CreatedAt: now()}
	if t != nil {
		c.TaskID = t.ID
	}
	if a != nil {
		c.AttemptID = a.ID
		c.Fence = a.Fence
	}
	return c, put(tx, "command", c.ID, c.TaskID, c)
}
func found[T any](tx *transaction, k, id string) (T, error) {
	v, err := get[T](tx, k, id)
	if errors.Is(err, missing) {
		return v, fail(404, "not_found", "找不到指定对象")
	}
	return v, err
}
