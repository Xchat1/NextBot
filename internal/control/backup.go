package control

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"time"
)

type backupObject struct {
	SHA  string `json:"sha256"`
	Size int64  `json:"size"`
}
type BackupManifest struct {
	Version  int            `json:"formatVersion"`
	Schema   int            `json:"schemaVersion"`
	Instance string         `json:"instanceId"`
	At       time.Time      `json:"createdAt"`
	Water    string         `json:"highWater"`
	Database backupObject   `json:"database"`
	Objects  []backupObject `json:"objects"`
	Excluded []string       `json:"excluded"`
}

func hashFile(path string) (backupObject, error) {
	f, err := os.Open(path)
	if err != nil {
		return backupObject{}, err
	}
	defer f.Close()
	return hashReader(f)
}
func (s *Store) Backup(dest string) (BackupManifest, error) {
	stage, err := staging(dest)
	if err != nil {
		return BackupManifest{}, err
	}
	defer os.RemoveAll(stage)
	dbPath := filepath.Join(stage, "control.db")
	if _, err = s.DB.Exec("VACUUM INTO ?", dbPath); err != nil {
		return BackupManifest{}, err
	}
	copyDB, err := Open(stage)
	if err != nil {
		return BackupManifest{}, err
	}
	m := BackupManifest{Version: 1, Schema: 1, At: now(), Excluded: []string{"device sessions", "pairing secrets", "working trees", "runner journal", "model credentials"}}
	err = copyDB.Tx(func(tx *transaction) error {
		m.Instance = meta(tx, "instance")
		m.Water = meta(tx, "seq")
		var err error
		m.Objects, err = referenced(tx)
		if err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM receipts WHERE scope IN (SELECT id FROM records WHERE kind='device'); DELETE FROM secrets"); err != nil {
			return err
		}
		return nil
	})
	if err == nil {
		_, err = copyDB.DB.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	}
	if closeErr := copyDB.DB.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return BackupManifest{}, err
	}
	m.Database, err = hashFile(dbPath)
	if err != nil {
		return m, err
	}
	for _, o := range m.Objects {
		path := filepath.Join(stage, "objects", o.SHA)
		if err = copyChecked(s.object(o.SHA), path, o); err != nil {
			return m, fmt.Errorf("object %s: %w", o.SHA, err)
		}
	}
	if err = writeManifest(stage, m); err != nil {
		return m, err
	}
	if _, err = VerifyBackup(stage); err != nil {
		return m, err
	}
	return m, publishDirectory(stage, dest)
}
func referenced(tx *transaction) ([]backupObject, error) {
	as, err := list[Artifact](tx, "artifact", "")
	if err != nil {
		return nil, err
	}
	ref := map[string]int64{}
	for _, a := range as {
		for _, e := range a.Entries {
			if !shaPattern.MatchString(e.SHA) || e.Size < 0 || e.Size > maxEntry {
				return nil, fmt.Errorf("invalid entry %s", e.ID)
			}
			ref[e.SHA] = e.Size
		}
	}
	out := make([]backupObject, 0, len(ref))
	for sha, size := range ref {
		out = append(out, backupObject{sha, size})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].SHA < out[j].SHA })
	return out, nil
}
func writeManifest(dir string, m BackupManifest) error {
	f, err := os.OpenFile(filepath.Join(dir, "manifest.json"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer f.Close()
	e := json.NewEncoder(f)
	e.SetIndent("", "  ")
	if err = e.Encode(m); err != nil {
		return err
	}
	return f.Sync()
}
func staging(dest string) (string, error) {
	if _, err := os.Lstat(dest); err == nil {
		return "", fmt.Errorf("target already exists: %s", dest)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	return os.MkdirTemp(filepath.Dir(dest), ".nextbot-stage-*")
}
func publishDirectory(stage, dest string) error {
	if err := syncDir(filepath.Join(stage, "objects")); err != nil {
		return err
	}
	if err := syncDir(stage); err != nil {
		return err
	}
	if _, err := os.Lstat(dest); err == nil {
		return fmt.Errorf("target already exists: %s", dest)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.Rename(stage, dest); err != nil {
		return err
	}
	return syncDir(filepath.Dir(dest))
}
func VerifyBackup(dir string) (BackupManifest, error) {
	var m BackupManifest
	f, err := os.Open(filepath.Join(dir, "manifest.json"))
	if err != nil {
		return m, err
	}
	defer f.Close()
	if err = json.NewDecoder(io.LimitReader(f, 32<<20)).Decode(&m); err != nil {
		return m, err
	}
	if m.Version != 1 || m.Schema != 1 || m.Instance == "" || !shaPattern.MatchString(m.Database.SHA) {
		return m, fmt.Errorf("invalid backup format")
	}
	dbPath := filepath.Join(dir, "control.db")
	hash, err := hashFile(dbPath)
	if err != nil || hash != m.Database {
		return m, fmt.Errorf("backup database checksum mismatch")
	}
	db, err := sql.Open("sqlite3", "file:"+dbPath+"?mode=ro&immutable=1")
	if err != nil {
		return m, err
	}
	defer db.Close()
	var check string
	if err = db.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		return m, fmt.Errorf("backup database integrity check failed")
	}
	var version int
	if err = db.QueryRow("PRAGMA user_version").Scan(&version); err != nil || version != m.Schema {
		return m, fmt.Errorf("backup schema does not match the manifest")
	}
	tx, err := db.Begin()
	if err != nil {
		return m, err
	}
	defer tx.Rollback()
	var instance, water string
	if err = tx.QueryRow("SELECT value FROM meta WHERE key='instance'").Scan(&instance); err != nil {
		return m, err
	}
	if err = tx.QueryRow("SELECT value FROM meta WHERE key='seq'").Scan(&water); err != nil {
		return m, err
	}
	if instance != m.Instance || water != m.Water {
		return m, fmt.Errorf("backup source or watermark does not match the manifest")
	}
	refs, err := referenced(tx)
	if err != nil {
		return m, err
	}
	if len(refs) != len(m.Objects) {
		return m, fmt.Errorf("backup object count mismatch")
	}
	sort.Slice(m.Objects, func(i, j int) bool { return m.Objects[i].SHA < m.Objects[j].SHA })
	for i, o := range refs {
		if o != m.Objects[i] {
			return m, fmt.Errorf("backup references do not match object %s", o.SHA)
		}
		if _, err = checkedBytes(filepath.Join(dir, "objects", o.SHA), o.SHA, o.Size); err != nil {
			return m, fmt.Errorf("object %s: %w", o.SHA, err)
		}
	}
	return m, nil
}
func Restore(source, dest, expected string) (BackupManifest, error) {
	m, err := VerifyBackup(source)
	if err != nil {
		return m, err
	}
	if expected == "" || m.Instance != expected {
		return m, fmt.Errorf("expected source %q, actual source %q; confirm the source instance explicitly", expected, m.Instance)
	}
	stage, err := staging(dest)
	if err != nil {
		return m, err
	}
	defer os.RemoveAll(stage)
	if err = os.Mkdir(filepath.Join(stage, "objects"), 0700); err != nil {
		return m, err
	}
	if err = copyChecked(filepath.Join(source, "control.db"), filepath.Join(stage, "control.db"), m.Database); err != nil {
		return m, err
	}
	for _, o := range m.Objects {
		if err = copyChecked(filepath.Join(source, "objects", o.SHA), filepath.Join(stage, "objects", o.SHA), o); err != nil {
			return m, err
		}
	}
	s, err := Open(stage)
	if err != nil {
		return m, err
	}
	err = s.Tx(func(tx *transaction) error {
		for k, v := range map[string]string{"stream": ID(), "floor": m.Water, "generation": inc(meta(tx, "generation"))} {
			if err := setMeta(tx, k, v); err != nil {
				return err
			}
		}
		if _, err := tx.Exec("DELETE FROM secrets; DELETE FROM records WHERE kind IN ('node','lease')"); err != nil {
			return err
		}
		ds, err := list[Device](tx, "device", "")
		if err != nil {
			return err
		}
		for _, d := range ds {
			if d.Revoked == nil {
				at := now()
				d.Revoked = &at
				if err = put(tx, "device", d.ID, "", d); err != nil {
					return err
				}
			}
		}
		cs, err := list[Command](tx, "command", "")
		if err != nil {
			return err
		}
		for _, c := range cs {
			if !commandDone(c.Phase) {
				c.Phase = "unknown"
				if err = put(tx, "command", c.ID, c.TaskID, c); err != nil {
					return err
				}
			}
		}
		rs, err := list[Request](tx, "request", "")
		if err != nil {
			return err
		}
		for _, r := range rs {
			if r.Status == "pending" {
				r.Status = "closed"
				if err = put(tx, "request", r.ID, r.TaskID, r); err != nil {
					return err
				}
			}
		}
		ts, err := list[Task](tx, "task", "")
		if err != nil {
			return err
		}
		for _, t := range ts {
			if terminal(t.State) || t.State == "draft" || t.State == "awaiting_acceptance" {
				continue
			}
			block(&t, "awaiting_input", "restore", "备份恢复后需在安装主机核对原工作区和进程")
			if err = change(tx, &t); err != nil {
				return err
			}
		}
		return emit(tx, "instance_restored", nil, "", map[string]string{"sourceInstance": m.Instance})
	})
	if err == nil {
		_, err = s.DB.Exec("PRAGMA wal_checkpoint(TRUNCATE)")
	}
	err = errors.Join(err, s.DB.Close())
	if err != nil {
		return m, err
	}
	return m, publishDirectory(stage, dest)
}
func copyChecked(source, dest string, expected backupObject) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.OpenFile(dest, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	defer out.Close()
	actual, err := hashReader(io.TeeReader(io.LimitReader(in, expected.Size+1), out))
	if err != nil {
		return err
	}
	if actual != expected {
		return fmt.Errorf("checksum or size mismatch (%s bytes)", strconv.FormatInt(actual.Size, 10))
	}
	return out.Sync()
}
