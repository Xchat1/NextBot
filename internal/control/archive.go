package control

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
)

const maxEntry = 8 << 20
const maxArchive = 24 << 20

var shaPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
var entryPattern = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]{0,79}$`)

type uploadInput struct {
	Attempt string  `json:"attemptId"`
	Key     string  `json:"uploadKey"`
	Base    string  `json:"baseCommit"`
	Entries []Entry `json:"manifest"`
}

func manifestDigest(attempt, base string, entries []Entry) string {
	return Digest("manifest", map[string]any{"attemptId": attempt, "baseCommit": base, "manifest": entries})
}
func (s *Store) object(sha string) string { return filepath.Join(s.Dir, "objects", sha) }
func checkedBytes(path, sha string, size int64) ([]byte, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	b, err := io.ReadAll(io.LimitReader(f, size+1))
	if err != nil {
		return nil, err
	}
	if int64(len(b)) != size || Hash(b) != sha {
		return nil, fmt.Errorf("object byte length or checksum mismatch")
	}
	return b, nil
}
func (s *Store) entry(e Entry) ([]byte, error) {
	if !shaPattern.MatchString(e.SHA) || e.Size < 0 || e.Size > maxEntry {
		return nil, fail(409, "artifact_unavailable", "无效的归档索引")
	}
	b, err := checkedBytes(s.object(e.SHA), e.SHA, e.Size)
	if err != nil {
		return nil, conflict("artifact_unavailable", "归档条目缺失或校验失败", map[string]string{"entryId": e.ID})
	}
	return b, nil
}
func (s *Store) verify(a Artifact) error {
	for _, e := range a.Entries {
		if _, err := s.entry(e); err != nil {
			return err
		}
	}
	return nil
}
func (s *Server) artifactBytes(w http.ResponseWriter, r *http.Request) {
	var b []byte
	var e Entry
	err := s.Tx(func(tx *transaction) error {
		if _, err := authenticate(tx, r); err != nil {
			return err
		}
		a, err := found[Artifact](tx, "artifact", r.PathValue("id"))
		if err != nil {
			return err
		}
		e, err = findEntry(a.Entries, r.PathValue("entry"))
		if err != nil {
			return err
		}
		b, err = s.entry(e)
		return err
	})
	if err != nil {
		writeError(w, err)
		return
	}
	serveEntry(w, e, b)
}
func findEntry(entries []Entry, id string) (Entry, error) {
	for _, x := range entries {
		if x.ID == id {
			return x, nil
		}
	}
	return Entry{}, fail(404, "not_found", "归档条目不存在")
}
func serveEntry(w http.ResponseWriter, e Entry, b []byte) {
	w.Header().Set("Content-Type", e.MediaType)
	w.Header().Set("ETag", "\""+e.SHA+"\"")
	w.Header().Set("Content-Disposition", "inline")
	_, _ = io.Copy(w, bytes.NewReader(b))
}
func (s *Server) initUpload(tx *transaction, i uploadInput) (result, error) {
	a, err := found[Attempt](tx, "attempt", i.Attempt)
	if err != nil {
		return result{}, err
	}
	t, err := found[Task](tx, "task", a.TaskID)
	if err != nil {
		return result{}, err
	}
	if a.Role != "execution" || t.Base != i.Base || len(i.Key) < 8 || len(i.Key) > 128 || len(i.Entries) == 0 || len(i.Entries) > 32 {
		return result{}, invalid("归档清单无效")
	}
	sort.Slice(i.Entries, func(a, b int) bool { return i.Entries[a].ID < i.Entries[b].ID })
	var total int64
	for pos, e := range i.Entries {
		if !entryPattern.MatchString(e.ID) || !shaPattern.MatchString(e.SHA) || e.Size < 0 || e.Size > maxEntry || pos > 0 && i.Entries[pos-1].ID == e.ID {
			return result{}, invalid("归档条目标识、长度或摘要无效")
		}
		switch e.MediaType {
		case "text/plain; charset=utf-8", "text/x-diff; charset=utf-8", "application/json":
		default:
			return result{}, invalid("归档类型不支持")
		}
		switch e.Kind {
		case "diff", "agent_statement", "observed_run", "dry_run", "not_run", "execution_log", "input_manifest":
		default:
			return result{}, invalid("证据类型不支持")
		}
		total += e.Size
	}
	if total > maxArchive {
		return result{}, invalid("归档大小超过限制")
	}
	digest := manifestDigest(i.Attempt, i.Base, i.Entries)
	id := Hash([]byte(i.Attempt + ":" + i.Key))
	u, err := get[Upload](tx, "upload", id)
	if err == nil && u.Digest != digest {
		return result{}, conflict("idempotency_conflict", "归档清单已变更", nil)
	}
	if errors.Is(err, missing) {
		u = Upload{ID: id, AttemptID: a.ID, Base: i.Base, Entries: i.Entries, Digest: digest}
		if err = put(tx, "upload", id, a.ID, u); err != nil {
			return result{}, err
		}
	} else if err != nil {
		return result{}, err
	}
	missingIDs := []string{}
	for _, e := range u.Entries {
		if _, err = s.entry(e); err != nil {
			missingIDs = append(missingIDs, e.ID)
		}
	}
	return result{201, map[string]any{"uploadId": id, "missingEntryIds": missingIDs, "manifestDigest": digest}}, nil
}
func writeObject(dir string, e Entry, reader io.Reader) error {
	f, err := os.CreateTemp(dir, ".incoming-*")
	if err != nil {
		return err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	h := sha256.New()
	n, err := io.Copy(io.MultiWriter(f, h), io.LimitReader(reader, e.Size+1))
	if err != nil {
		return err
	}
	if n != e.Size || hex.EncodeToString(h.Sum(nil)) != e.SHA {
		return fail(422, "digest_mismatch", "归档长度或摘要不匹配")
	}
	if err = f.Chmod(0400); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), filepath.Join(dir, e.SHA)); err != nil {
		return err
	}
	return syncDir(dir)
}
func syncDir(path string) error {
	d, err := os.Open(path)
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func (s *Server) finishUpload(tx *transaction, id string, i input) (result, error) {
	u, err := found[Upload](tx, "upload", id)
	if err != nil {
		return result{}, err
	}
	if u.AttemptID != i.str("attemptId") || u.Digest != i.str("manifestDigest") {
		return result{}, conflict("stale_target", "归档目标或清单不匹配", nil)
	}
	a, err := found[Attempt](tx, "attempt", u.AttemptID)
	if err != nil {
		return result{}, err
	}
	if a.ArtifactID != "" {
		prior, err := found[Artifact](tx, "artifact", a.ArtifactID)
		if err != nil {
			return result{}, err
		}
		if prior.Digest != Digest("content", map[string]any{"baseCommit": u.Base, "manifest": u.Entries}) {
			return result{}, conflict("idempotency_conflict", "尝试已有其他交付", nil)
		}
		prior.Integrity = "available"
		return ok(prior), s.verify(prior)
	}
	t, err := found[Task](tx, "task", a.TaskID)
	if err != nil {
		return result{}, err
	}
	existing, err := list[Artifact](tx, "artifact", t.ID)
	if err != nil {
		return result{}, err
	}
	art := Artifact{ID: ID(), AttemptID: a.ID, TaskID: t.ID, Base: u.Base, Version: len(existing) + 1, Entries: u.Entries, Integrity: "available", Digest: Digest("content", map[string]any{"baseCommit": u.Base, "manifest": u.Entries})}
	if err = s.verify(art); err != nil {
		return result{}, err
	}
	art.Evidence, err = s.hasEvidence(tx, a, t, art)
	if err != nil {
		return result{}, err
	}
	a.ArtifactID = art.ID
	if err = put(tx, "artifact", art.ID, t.ID, art); err != nil {
		return result{}, err
	}
	if err = put(tx, "attempt", a.ID, t.ID, a); err != nil {
		return result{}, err
	}
	if err = emit(tx, "artifact_published", &t, a.ID, map[string]any{"artifactVersionId": art.ID, "versionNumber": art.Version, "integrityStatus": art.Integrity}); err != nil {
		return result{}, err
	}
	return ok(art), nil
}
func (s *Store) hasEvidence(tx *transaction, a Attempt, t Task, art Artifact) (bool, error) {
	type run struct {
		RequestID    string `json:"requestId"`
		TestID       string `json:"testId"`
		InputDigest  string `json:"inputDigest"`
		SnapshotHash string `json:"snapshotHash"`
		ExitCode     *int   `json:"exitCode"`
	}
	manifest, err := findEntry(art.Entries, "input-manifest.json")
	if err != nil {
		return false, nil
	}
	b, err := s.entry(manifest)
	if err != nil {
		return false, err
	}
	snapshot := strings.TrimSpace(string(b))
	for _, e := range art.Entries {
		if e.Kind != "observed_run" || e.MediaType != "application/json" {
			continue
		}
		b, err := s.entry(e)
		if err != nil {
			return false, err
		}
		v, err := decode[run](b)
		if err != nil || v.ExitCode == nil || *v.ExitCode != 0 || v.TestID != t.TestID || v.SnapshotHash != snapshot {
			continue
		}
		req, err := get[Request](tx, "request", v.RequestID)
		if errors.Is(err, missing) {
			continue
		}
		if err != nil {
			return false, err
		}
		if req.AttemptID != a.ID || req.Decision == nil || req.Decision.Outcome != "allow_once" || req.Input["inputDigest"] != v.InputDigest || req.Input["snapshotHash"] != v.SnapshotHash {
			continue
		}
		c, err := get[Command](tx, "command", req.CommandID)
		if err != nil {
			return false, err
		}
		if c.Phase == "finished" {
			return true, nil
		}
	}
	return false, nil
}
func hashReader(r io.Reader) (backupObject, error) {
	h := sha256.New()
	n, err := io.Copy(h, r)
	return backupObject{SHA: hex.EncodeToString(h.Sum(nil)), Size: n}, err
}
