package runner

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
CREATE TABLE IF NOT EXISTS test_evidence (command TEXT PRIMARY KEY, attempt TEXT NOT NULL, body BLOB NOT NULL);
CREATE INDEX IF NOT EXISTS test_evidence_attempt ON test_evidence(attempt,command);
CREATE TABLE IF NOT EXISTS execution_archives (command TEXT PRIMARY KEY, body BLOB NOT NULL);
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
func readRows[T any](db *sql.DB, query string, args ...any) ([]T, error) {
	rows, err := db.Query(query, args...)
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

func (j *journal) finishTest(c control.Command, report control.Report, entries []archiveEntry) error {
	encodedEntries, err := json.Marshal(entries)
	if err != nil {
		return err
	}
	receipt, err := json.Marshal(pending{"receipts", report})
	if err != nil {
		return err
	}
	tx, err := j.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE processes SET stopped=? WHERE command=?", time.Now().UTC().Format(time.RFC3339Nano), c.ID); err != nil {
		return err
	}
	if len(entries) > 0 {
		if _, err = tx.Exec("INSERT INTO test_evidence VALUES(?,?,?)", c.ID, c.AttemptID, encodedEntries); err != nil {
			return err
		}
	}
	if _, err = tx.Exec("INSERT INTO outbox(id,body) VALUES(?,?)", report.ID, receipt); err != nil {
		return err
	}
	if _, err = tx.Exec("UPDATE jobs SET phase='settled' WHERE id=?", c.ID); err != nil {
		return err
	}
	return tx.Commit()
}

func (j *journal) testEvidence(attemptID string) ([]archiveEntry, error) {
	tests, err := readRows[[]archiveEntry](j.db, "SELECT body FROM test_evidence WHERE attempt=? ORDER BY command", attemptID)
	var entries []archiveEntry
	for _, test := range tests {
		entries = append(entries, test...)
	}
	return entries, err
}

func (j *journal) saveArchive(commandID string, archive executionArchive) error {
	body, err := json.Marshal(archive)
	if err != nil {
		return err
	}
	_, err = j.db.Exec("INSERT INTO execution_archives VALUES(?,?)", commandID, body)
	return err
}

func (j *journal) executionArchive(commandID string) (*executionArchive, error) {
	var body []byte
	err := j.db.QueryRow("SELECT body FROM execution_archives WHERE command=?", commandID).Scan(&body)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var archive executionArchive
	err = json.Unmarshal(body, &archive)
	return &archive, err
}

func (j *journal) completeArchive(c control.Command) error {
	tx, err := j.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err = tx.Exec("UPDATE jobs SET phase='settled' WHERE id=?", c.ID); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM execution_archives WHERE command=?", c.ID); err != nil {
		return err
	}
	if _, err = tx.Exec("DELETE FROM test_evidence WHERE attempt=?", c.AttemptID); err != nil {
		return err
	}
	return tx.Commit()
}

func (j *journal) process(c control.Command, pid int) error {
	stat, err := os.ReadFile("/proc/" + strconv.Itoa(pid) + "/stat")
	if err != nil {
		return err
	}
	end := strings.LastIndexByte(string(stat), ')')
	if end < 0 {
		return fmt.Errorf("cannot identify the spawned process")
	}
	fields := strings.Fields(string(stat[end+1:]))
	if len(fields) < 20 {
		return fmt.Errorf("cannot identify the spawned process")
	}
	boot, err := os.ReadFile("/proc/sys/kernel/random/boot_id")
	if err != nil {
		return err
	}
	_, err = j.db.Exec("INSERT INTO processes(command,pid,boot,birth,observed) VALUES(?,?,?,?,?)", c.ID, pid, strings.TrimSpace(string(boot)), fields[19], time.Now().UTC().Format(time.RFC3339Nano))
	return err
}
func (j *journal) processStopped(c control.Command) error {
	_, err := j.db.Exec("UPDATE processes SET stopped=? WHERE command=?", time.Now().UTC().Format(time.RFC3339Nano), c.ID)
	return err
}
