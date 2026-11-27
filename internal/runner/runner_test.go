package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"nextbot/internal/control"
)

func TestWorkspaceBoundariesAndDiff(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	repo := filepath.Join(dir, "source")
	if err := os.Mkdir(repo, 0700); err != nil {
		t.Fatal(err)
	}
	runGit := func(args ...string) string {
		t.Helper()
		value, err := git(ctx, repo, nil, nil, args...)
		if err != nil {
			t.Fatal(err)
		}
		return strings.TrimSpace(string(value))
	}
	runGit("init")
	if err := os.WriteFile(filepath.Join(repo, "existing.txt"), []byte("baseline\n"), 0600); err != nil {
		t.Fatal(err)
	}
	runGit("add", "existing.txt")
	runGit("-c", "user.email=test@example.invalid", "-c", "user.name=Fixture", "commit", "-m", "baseline")
	base := runGit("rev-parse", "HEAD")
	if err := os.WriteFile(filepath.Join(repo, "existing.txt"), []byte("operator edit\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p := control.Project{Path: repo}
	task := control.Task{Base: base}
	a, err := createWorkspace(ctx, dir, p, task, control.ID())
	if err != nil {
		t.Fatal(err)
	}
	defer a.Root.Close()
	b, err := createWorkspace(ctx, dir, p, task, control.ID())
	if err != nil {
		t.Fatal(err)
	}
	defer b.Root.Close()
	if content, err := a.read("existing.txt"); err != nil || content != "baseline\n" {
		t.Fatalf("worktree failed to use fixed commit: %q %v", content, err)
	}
	if err := a.write("new.txt", "task-only\n"); err != nil {
		t.Fatal(err)
	}
	if _, err = b.read("new.txt"); !os.IsNotExist(err) {
		t.Fatalf("second worktree observed a new file: %v", err)
	}
	if err = a.write(".git", "bad"); err == nil {
		t.Fatal("write reached Git metadata")
	}
	if err = a.write("../escape", "bad"); err == nil {
		t.Fatal("write escaped worktree")
	}
	if err = os.Symlink(".git", filepath.Join(a.Path, "alias")); err != nil {
		t.Fatal(err)
	}
	if _, err = a.read("alias"); err == nil {
		t.Fatal("read followed a symlink to Git metadata")
	}
	if err = os.Remove(filepath.Join(a.Path, "alias")); err != nil {
		t.Fatal(err)
	}
	diff, _, err := a.diff(ctx, dir)
	if err != nil || !bytes.Contains(diff, []byte("+task-only")) {
		t.Fatalf("missing task diff %q: %v", diff, err)
	}
	src, err := os.ReadFile(filepath.Join(repo, "existing.txt"))
	if err != nil || string(src) != "operator edit\n" || runGit("rev-parse", "HEAD") != base {
		t.Fatal("source worktree or base ref changed")
	}
}

type transport func(*http.Request) (*http.Response, error)

func (t transport) RoundTrip(req *http.Request) (*http.Response, error) { return t(req) }

func TestMissingJournalDoesNotReplayPreviouslyReceivedTest(t *testing.T) {
	j, err := openJournal(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer j.db.Close()
	reports := []control.Report{}
	r := &Runner{j: j, c: &client{http: &http.Client{Transport: transport(func(req *http.Request) (*http.Response, error) {
		var body control.Report
		if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
			return nil, err
		}
		reports = append(reports, body)
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader("{}"))}, nil
	})}}}
	c := control.Command{ID: control.ID(), Type: "run_registered_test", Phase: "received", AttemptID: control.ID()}
	if err = r.handle(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	if err = r.handle(context.Background(), c); err != nil {
		t.Fatal(err)
	}
	phase, err := j.phase(c.ID)
	if err != nil || phase != "unknown" || len(reports) != 1 || reports[0].Phase != "unknown" {
		t.Fatalf("unexpected replay outcome: %s %v %v", phase, reports, err)
	}
}
func TestClaudeToolBridgeUsesScopedWorkspace(t *testing.T) {
	dir := t.TempDir()
	root, err := os.OpenRoot(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	ctx := context.Background()
	r := &Runner{}
	a := &activeAttempt{ctx: ctx, workspace: &workspace{Path: dir, Root: root}}
	config, closeBridge, err := r.startBridge(a, control.Command{})
	if err != nil {
		t.Fatal(err)
	}
	defer closeBridge()
	var parsed struct {
		Servers map[string]struct {
			Args []string
			Env  map[string]string
		} `json:"mcpServers"`
	}
	if err = json.Unmarshal([]byte(config), &parsed); err != nil {
		t.Fatal(err)
	}
	input := strings.Join([]string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2024-11-05"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"nb_write_file","arguments":{"path":"one.txt","text":"expected"}}}`,
		`{"jsonrpc":"2.0","id":4,"method":"tools/call","params":{"name":"nb_write_file","arguments":{"path":"../outside","text":"bad"}}}`,
		`{"jsonrpc":"2.0","id":5,"method":"tools/call","params":{"name":"shell","arguments":{}}}`,
	}, "\n")
	var out bytes.Buffer
	bridge := parsed.Servers["nextbot"]
	if len(bridge.Args) != 3 {
		t.Fatalf("invalid tool bridge arguments: %v", bridge.Args)
	}
	if err = ServeBridge(ctx, bridge.Args[2], bridge.Env["NEXTBOT_MCP_TOKEN"], strings.NewReader(input), &out); err != nil {
		t.Fatal(err)
	}
	lines := strings.Split(strings.TrimSpace(out.String()), "\n")
	if len(lines) != 5 || !strings.Contains(lines[1], "nb_read_file") || !strings.Contains(lines[2], `"isError":false`) || !strings.Contains(lines[3], `"isError":true`) || !strings.Contains(lines[4], `"isError":true`) {
		t.Fatalf("unexpected broker replies: %s", out.String())
	}
	b, err := os.ReadFile(filepath.Join(dir, "one.txt"))
	if err != nil || string(b) != "expected" {
		t.Fatalf("scoped write missing: %s %v", b, err)
	}
}
