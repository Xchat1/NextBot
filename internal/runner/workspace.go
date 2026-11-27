package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"

	"nextbot/internal/control"
)

const workspaceFileLimit = 4 << 20
const workspaceTotalLimit = 128 << 20

type fileRecord struct {
	Path string `json:"path"`
	Mode string `json:"mode"`
	SHA  string `json:"sha256"`
}
type workspace struct {
	Path string
	Root *os.Root
	Base string
}

func git(ctx context.Context, dir string, env []string, input []byte, args ...string) ([]byte, error) {
	base := []string{"-c", "core.hooksPath=/dev/null", "-c", "core.fsmonitor=false", "-c", "submodule.recurse=false", "-C", dir}
	cmd := exec.CommandContext(ctx, "git", append(base, args...)...)
	cmd.Env = append(cleanEnv(), env...)
	if input != nil {
		cmd.Stdin = bytes.NewReader(input)
	}
	var stderr limitedBuffer
	stderr.limit = 2048
	cmd.Stderr = &stderr
	out, err := cmd.Output()
	if err != nil {
		return nil, fmt.Errorf("git %s: %w (%s)", args[0], err, strings.TrimSpace(stderr.String()))
	}
	return out, nil
}
func cleanEnv() []string {
	out := []string{"GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null", "GIT_TERMINAL_PROMPT=0", "GIT_OPTIONAL_LOCKS=0", "LANG=C.UTF-8"}
	for _, k := range []string{"PATH", "HOME", "TMPDIR", "XDG_RUNTIME_DIR"} {
		if v := os.Getenv(k); v != "" {
			out = append(out, k+"="+v)
		}
	}
	return out
}
func createWorkspace(ctx context.Context, dir string, p control.Project, t control.Task, attempt string) (*workspace, error) {
	if _, err := git(ctx, p.Path, nil, nil, "cat-file", "-e", t.Base+"^{commit}"); err != nil {
		return nil, fmt.Errorf("fixed base cannot be read: %w", err)
	}
	path := filepath.Join(dir, "worktrees", attempt)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	_, err := git(ctx, p.Path, nil, nil, "worktree", "add", "-b", "nextbot/"+attempt, "--", path, t.Base)
	if err != nil {
		return nil, err
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, err
	}
	return &workspace{path, root, t.Base}, nil
}
func validPath(name string) bool {
	if !fs.ValidPath(name) || name == "." || strings.ContainsAny(name, "\\\x00\r\n") {
		return false
	}
	for _, part := range strings.Split(name, "/") {
		if strings.EqualFold(part, ".git") {
			return false
		}
	}
	return true
}
func (w *workspace) safePath(name string, mayCreate bool) error {
	if !validPath(name) {
		return fmt.Errorf("path outside task files")
	}
	parts := strings.Split(name, "/")
	current := ""
	for i, p := range parts {
		current = filepath.Join(current, p)
		info, err := w.Root.Lstat(current)
		if os.IsNotExist(err) && mayCreate {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || i < len(parts)-1 && !info.IsDir() {
			return fmt.Errorf("symbolic links and non-directory parents are excluded")
		}
	}
	return nil
}
func (w *workspace) read(name string) (string, error) {
	if err := w.safePath(name, false); err != nil {
		return "", err
	}
	f, err := w.Root.Open(name)
	if err != nil {
		return "", err
	}
	defer f.Close()
	stat, err := f.Stat()
	if err != nil {
		return "", err
	}
	if !stat.Mode().IsRegular() || stat.Size() > workspaceFileLimit {
		return "", fmt.Errorf("file type or size unsupported")
	}
	b, err := io.ReadAll(io.LimitReader(f, workspaceFileLimit+1))
	return string(b), err
}
func (w *workspace) write(name, text string) error {
	if len(text) > workspaceFileLimit {
		return fmt.Errorf("file exceeds limit")
	}
	if err := w.safePath(name, true); err != nil {
		return err
	}
	mode := fs.FileMode(0644)
	if info, err := w.Root.Lstat(name); err == nil {
		if !info.Mode().IsRegular() {
			return fmt.Errorf("target is not a regular file")
		}
		mode = info.Mode().Perm()
	}
	if err := w.Root.MkdirAll(filepath.Dir(name), 0755); err != nil {
		return err
	}
	temp := filepath.Join(filepath.Dir(name), ".nextbot-"+control.ID())
	f, err := w.Root.OpenFile(temp, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode)
	if err != nil {
		return err
	}
	defer w.Root.Remove(temp)
	_, err = io.WriteString(f, text)
	if closeErr := f.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return w.Root.Rename(temp, name)
}
func (w *workspace) remove(name string) error {
	if err := w.safePath(name, false); err != nil {
		return err
	}
	info, err := w.Root.Lstat(name)
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("target is not a regular file")
	}
	return w.Root.Remove(name)
}
func (w *workspace) files(copyTo string) ([]fileRecord, error) {
	files := []fileRecord{}
	var total int64
	err := fs.WalkDir(w.Root.FS(), ".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if path == "." {
			return nil
		}
		if !validPath(path) {
			if d.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if d.IsDir() {
			if copyTo != "" {
				dest := filepath.Join(copyTo, path)
				if err := os.MkdirAll(dest, 0755); err != nil {
					return err
				}
				return os.Chmod(dest, 0755)
			}
			return nil
		}
		info, err := d.Info()
		if err != nil {
			return err
		}
		mode := "100644"
		var b []byte
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			mode = "120000"
			link, err := w.Root.Readlink(path)
			if err != nil {
				return err
			}
			if filepath.IsAbs(link) || !validPath(filepath.ToSlash(filepath.Clean(filepath.Join(filepath.Dir(path), link)))) {
				return fmt.Errorf("external symbolic link: %s", path)
			}
			b = []byte(link)
		case info.Mode().IsRegular():
			if info.Size() > workspaceFileLimit {
				return fmt.Errorf("file size limit: %s", path)
			}
			f, err := w.Root.Open(path)
			if err != nil {
				return err
			}
			b, err = io.ReadAll(io.LimitReader(f, workspaceFileLimit+1))
			f.Close()
			if err != nil {
				return err
			}
			if len(b) > workspaceFileLimit {
				return fmt.Errorf("file size limit: %s", path)
			}
			if info.Mode().Perm()&0111 != 0 {
				mode = "100755"
			}
		default:
			return fmt.Errorf("unsupported workspace file: %s", path)
		}
		total += int64(len(b))
		if len(files) >= 20000 || total > workspaceTotalLimit {
			return fmt.Errorf("workspace snapshot exceeds limit")
		}
		files = append(files, fileRecord{path, mode, control.Hash(b)})
		if copyTo == "" {
			return nil
		}
		dest := filepath.Join(copyTo, path)
		if mode == "120000" {
			return os.Symlink(string(b), dest)
		}
		perm := fs.FileMode(0444)
		if mode == "100755" {
			perm = 0555
		}
		if err := os.WriteFile(dest, b, perm); err != nil {
			return err
		}
		return os.Chmod(dest, perm)
	})
	if err != nil {
		return nil, err
	}
	sort.Slice(files, func(i, j int) bool { return files[i].Path < files[j].Path })
	return files, nil
}
func fingerprint(files []fileRecord) string {
	b, _ := json.Marshal(files)
	return control.Hash(b)
}
func (w *workspace) diff(ctx context.Context, dir string) ([]byte, string, error) {
	files, err := w.files("")
	if err != nil {
		return nil, "", err
	}
	index := filepath.Join(dir, "index-"+control.ID())
	defer os.Remove(index)
	env := []string{"GIT_INDEX_FILE=" + index}
	if _, err = git(ctx, w.Path, env, nil, "read-tree", "--empty"); err != nil {
		return nil, "", err
	}
	// Retain unchanged Git links when the parent repository contains uninitialized submodules.
	tree, err := git(ctx, w.Path, env, nil, "ls-tree", "-rz", w.Base)
	if err != nil {
		return nil, "", err
	}
	var updates bytes.Buffer
	for row := range strings.SplitSeq(string(tree), "\x00") {
		if strings.HasPrefix(row, "160000 commit ") {
			updates.WriteString(strings.Replace(row, " commit ", " ", 1))
			updates.WriteByte(0)
		}
	}
	for _, f := range files {
		var b []byte
		if f.Mode == "120000" {
			link, err := w.Root.Readlink(f.Path)
			if err != nil {
				return nil, "", err
			}
			b = []byte(link)
		} else {
			b, err = w.Root.ReadFile(f.Path)
			if err != nil {
				return nil, "", err
			}
		}
		if control.Hash(b) != f.SHA {
			return nil, "", fmt.Errorf("workspace changed while archiving")
		}
		hash, err := git(ctx, w.Path, env, b, "hash-object", "-w", "--no-filters", "--stdin")
		if err != nil {
			return nil, "", err
		}
		fmt.Fprintf(&updates, "%s %s\t%s%c", f.Mode, strings.TrimSpace(string(hash)), f.Path, 0)
	}
	if _, err = git(ctx, w.Path, env, updates.Bytes(), "update-index", "-z", "--index-info"); err != nil {
		return nil, "", err
	}
	diff, err := git(ctx, w.Path, env, nil, "diff", "--cached", "--binary", "--full-index", "--no-ext-diff", "--no-textconv", w.Base, "--")
	if err == nil && len(diff) > 8<<20 {
		return nil, "", fmt.Errorf("patch exceeds archive limit")
	}
	return diff, fingerprint(files), err
}

type limitedBuffer struct {
	data  bytes.Buffer
	limit int
	total int
}

func (b *limitedBuffer) Write(p []byte) (int, error) {
	b.total += len(p)
	maxLen := min(len(p), b.limit-b.data.Len())
	if maxLen > 0 {
		_, _ = b.data.Write(p[:maxLen])
	}
	return len(p), nil
}
func (b *limitedBuffer) String() string { return b.data.String() }
