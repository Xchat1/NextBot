package runner

import (
	"database/sql"
	"encoding/json"
	"errors"
	"path/filepath"
	"time"

	"nextbot/internal/control"
)

type journal struct{ db *sql.DB }
type pending struct {
	Route  string         `json:"route"`
	Report control.Report `json:"report"`
}

func openJournal(dir string) (*journal, error) {
	db, err := sql.Open("sqlite3", filepath.Join(dir, "runner.db")+"?_busy_timeout=5000")
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	_, err = db.Exec(`
PRAGMA journal_mode=WAL;
PRAGMA synchronous=FULL;
CREATE TABLE IF NOT EXISTS jobs (id TEXT PRIMARY KEY, phase TEXT NOT NULL, body BLOB NOT NULL);
CREATE TABLE IF NOT EXISTS processes (command TEXT PRIMARY KEY, pid INTEGER NOT NULL, boot TEXT NOT NULL, birth TEXT NOT NULL, observed TEXT NOT NULL, stopped TEXT);
CREATE TABLE IF NOT EXISTS outbox (seq INTEGER PRIMARY KEY AUTOINCREMENT, id TEXT UNIQUE NOT NULL, body BLOB NOT NULL);
`)
	if err != nil {
		db.Close()
		return nil, err
	}
	return &journal{db}, nil
}
func (j *journal) phase(id string) (string, error) {
	var phase string
	err := j.db.QueryRow("SELECT phase FROM jobs WHERE id=?", id).Scan(&phase)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	return phase, err
}
func (j *journal) record(c control.Command, phase string) error {
	b, err := json.Marshal(c)
	if err != nil {
		return err
	}
	_, err = j.db.Exec("INSERT INTO jobs VALUES(?,?,?) ON CONFLICT(id) DO UPDATE SET phase=excluded.phase,body=excluded.body", c.ID, phase, b)
	return err
}
func (j *journal) add(route string, r control.Report) error {
	b, err := json.Marshal(pending{route, r})
	if err != nil {
		return err
	}
	_, err = j.db.Exec("INSERT OR IGNORE INTO outbox(id,body) VALUES(?,?)", r.ID, b)
	return err
}
func (j *journal) acknowledge(id string) error {
	_, err := j.db.Exec("DELETE FROM outbox WHERE id=?", id)
	return err
}
func (j *journal) pending() ([]pending, error) {
	return readRows[pending](j.db, "SELECT body FROM outbox ORDER BY seq")
}
func (j *journal) unfinished() ([]control.Command, error) {
	return readRows[control.Command](j.db, "SELECT body FROM jobs WHERE phase='intent' ORDER BY rowid")
}
func readRows[T any](db *sql.DB, query string) ([]T, error) {
	rows, err := db.Query(query)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		var b []byte
		var value T
		if err = rows.Scan(&b); err != nil {
			return nil, err
		}
		if err = json.Unmarshal(b, &value); err != nil {
			return nil, err
		}
		out = append(out, value)
	}
	return out, rows.Err()
}

func (j *journal) process(c control.Command, pid int) error {
	boot, birth, err := processIdentity(pid)
	if err != nil {
		return err
	}
	_, err = j.db.Exec("INSERT INTO processes(command,pid,boot,birth,observed) VALUES(?,?,?,?,?)", c.ID, pid, boot, birth, time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
func (j *journal) processStopped(c control.Command) error {
	_, err := j.db.Exec("UPDATE processes SET stopped=? WHERE command=?", time.Now().UTC().Format(time.RFC3339Nano), c.ID)
	return err
}
