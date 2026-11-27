package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"nextbot/internal/control"
	"nextbot/internal/runner"
	"nextbot/internal/web"
)

func main() {
	syscall.Umask(0077)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, os.Args[1:]); err != nil {
		log.Fatal(err)
	}
}
func run(ctx context.Context, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: nextbot desktop|control|runner|claim|recover-owner|project-add|backup|verify-backup|restore|pause|resume|reconcile")
	}
	fs := flag.NewFlagSet(args[0], flag.ContinueOnError)
	switch args[0] {
	case "desktop":
		return desktop(ctx, fs, args[1:], os.Stdout)
	case "control":
		dir := fs.String("data", ".nextbot/control", "control state directory")
		socket := fs.String("socket", ".nextbot/runner.sock", "private Runner socket")
		addr := fs.String("listen", "127.0.0.1:8787", "loopback HTTP bind address")
		origin := fs.String("origin", "https://nextbot.local", "browser origin")
		history := fs.Int("event-window", 10000, "number of synchronization events retained for replay")
		dev := fs.Bool("dev-http", false, "allow a loopback HTTP browser origin for development")
		uid := fs.Uint("runner-uid", uint(os.Getuid()), "authorized local Runner UID")
		gid := fs.Int("socket-gid", -1, "socket group for a separate Runner account")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return serve(ctx, *dir, *socket, *addr, *origin, *history, *dev, uint32(*uid), *gid, nil)
	case "runner":
		dir := fs.String("data", ".nextbot/runner", "local Runner state directory")
		socket := fs.String("socket", ".nextbot/runner.sock", "private Control socket")
		codexHome := fs.String("codex-home", "", "dedicated Codex login directory")
		claudeHome := fs.String("claude-home", "", "dedicated Claude Code login directory")
		codexVersion := fs.String("verified-codex-version", "", "exact Codex version verified on the installation host")
		claudeVersion := fs.String("verified-claude-version", "", "exact Claude CLI version verified for read-only review")
		codexReview := fs.String("verified-codex-review-version", "", "exact Codex version verified for read-only review")
		claudeExecution := fs.String("verified-claude-execution-version", "", "exact Claude CLI version verified for constrained execution")
		engine := fs.String("container-engine", "podman", "rootless Podman or Docker execution binary")
		tests := fs.Bool("enable-managed-tests", false, "enable the verified local rootless managed test runtime")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		absolute, err := filepath.Abs(*dir)
		if err != nil {
			return err
		}
		if strings.ContainsAny(absolute, ",\r\n") {
			return fmt.Errorf("Runner state path is invalid")
		}
		return runner.Run(ctx, runner.Config{Dir: absolute, Socket: *socket, CodexHome: *codexHome, ClaudeHome: *claudeHome, CodexVersion: *codexVersion, ClaudeVersion: *claudeVersion, CodexReviewVersion: *codexReview, ClaudeExecutionVersion: *claudeExecution, ContainerEngine: *engine, EnableManagedTests: *tests})
	case "tool-bridge":
		socket := fs.String("socket", "", "attempt tool socket")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		return runner.ServeBridge(ctx, *socket, os.Getenv("NEXTBOT_MCP_TOKEN"), os.Stdin, os.Stdout)
	case "claim", "recover-owner", "project-add", "backup", "pause", "resume", "reconcile":
		dir := fs.String("data", ".nextbot/control", "control state directory")
		origin := fs.String("origin", "https://nextbot.local", "browser origin for owner binding")
		name := fs.String("name", "", "project display name")
		repo := fs.String("repo", "", "registered Git repository worktree")
		tests := fs.String("tests", "", "optional JSON file containing registered test definitions")
		dest := fs.String("out", "", "new backup directory")
		taskID := fs.String("task", "", "task ID to reconcile")
		attemptID := fs.String("attempt", "", "verified attempt ID")
		observation := fs.String("observation", "", "host administrator’s evidence that old processes have stopped")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if args[0] == "claim" || args[0] == "recover-owner" {
			if err := control.ValidateOrigin(*origin, true); err != nil {
				return err
			}
		}
		store, err := control.Open(*dir)
		if err != nil {
			return err
		}
		defer store.DB.Close()
		switch args[0] {
		case "claim", "recover-owner":
			ticket, err := store.OwnerTicket(args[0] == "recover-owner")
			if err != nil {
				return err
			}
			fmt.Printf("%s/#claim=%s\n", *origin, ticket)
			return nil
		case "project-add":
			return addProject(ctx, store, *name, *repo, *tests)
		case "pause", "resume":
			return store.PauseDispatch(args[0] == "pause")
		case "reconcile":
			return store.ConfirmStopped(*taskID, *attemptID, *observation)
		case "backup":
			if *dest == "" {
				return fmt.Errorf("--out is required")
			}
			path, err := filepath.Abs(*dest)
			if err != nil {
				return err
			}
			m, err := store.Backup(path)
			if err != nil {
				return err
			}
			return printJSON(m)
		}
	case "verify-backup", "restore":
		src := fs.String("from", "", "backup directory")
		dest := fs.String("data", "", "new control data directory")
		instance := fs.String("source-instance", "", "confirmed source instance ID")
		if err := fs.Parse(args[1:]); err != nil {
			return err
		}
		if *src == "" {
			return fmt.Errorf("--from is required")
		}
		source, err := filepath.Abs(*src)
		if err != nil {
			return err
		}
		if args[0] == "verify-backup" {
			m, err := control.VerifyBackup(source)
			if err != nil {
				return err
			}
			return printJSON(m)
		}
		if *dest == "" {
			return fmt.Errorf("--data must be a new directory")
		}
		target, err := filepath.Abs(*dest)
		if err != nil {
			return err
		}
		m, err := control.Restore(source, target, *instance)
		if err != nil {
			return err
		}
		return printJSON(m)
	}
	return fmt.Errorf("unknown nextbot command %q", args[0])
}
func printJSON(v any) error {
	e := json.NewEncoder(os.Stdout)
	e.SetIndent("", "  ")
	return e.Encode(v)
}

var imageDigest = regexp.MustCompile(`^[a-zA-Z0-9_./:-]+@sha256:[0-9a-f]{64}$`)
var testIdentifier = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

func addProject(ctx context.Context, s *control.Store, name, repo, testFile string) error {
	if repo == "" {
		return fmt.Errorf("--repo is required")
	}
	path, err := filepath.Abs(repo)
	if err != nil {
		return err
	}
	cmd := exec.CommandContext(ctx, "git", "-C", path, "rev-parse", "--show-toplevel")
	out, err := cmd.Output()
	if err != nil {
		return fmt.Errorf("repository worktree cannot be read: %w", err)
	}
	path, err = filepath.EvalSymlinks(strings.TrimSpace(string(out)))
	if err != nil {
		return err
	}
	if name == "" {
		name = filepath.Base(path)
	}
	tests := []control.TestDefinition{}
	if testFile != "" {
		raw, err := os.ReadFile(testFile)
		if err != nil {
			return err
		}
		if err = json.Unmarshal(raw, &tests); err != nil {
			return err
		}
		if len(tests) > 32 {
			return fmt.Errorf("too many registered tests")
		}
		seen := map[string]bool{}
		for _, t := range tests {
			if !testIdentifier.MatchString(t.ID) || t.Label == "" || seen[t.ID] || !imageDigest.MatchString(t.Image) || len(t.Args) == 0 || len(t.Args) > 32 || !strings.HasPrefix(t.Args[0], "/") || t.Timeout < 1 || t.Timeout > 600 {
				return fmt.Errorf("invalid test definition %q: provide a unique ID, label, digest-pinned image, absolute entrypoint, and timeout between 1 and 600 seconds", t.ID)
			}
			for _, arg := range t.Args {
				if len(arg) > 4096 || strings.ContainsRune(arg, 0) {
					return fmt.Errorf("invalid argument in test definition %q", t.ID)
				}
			}
			seen[t.ID] = true
		}
	}
	p, err := s.AddProject(name, path, tests)
	if err != nil {
		return err
	}
	return printJSON(p)
}
func serve(ctx context.Context, dir, socket, addr, origin string, history int, dev bool, uid uint32, gid int, ready func(*control.Store, string) error) error {
	if ready == nil || origin != "" {
		if err := control.ValidateOrigin(origin, dev); err != nil {
			return err
		}
	}
	if history < 1 {
		return fmt.Errorf("event window must be positive")
	}
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return err
	}
	if ip := net.ParseIP(host); ip == nil || !ip.IsLoopback() {
		return fmt.Errorf("Control must listen on a loopback IP; use an HTTPS reverse proxy")
	}
	if err = os.MkdirAll(dir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(dir, "control.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("control state is already in use: %w", err)
	}
	store, err := control.Open(dir)
	if err != nil {
		return err
	}
	defer store.DB.Close()
	outer, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	defer outer.Close()
	if origin == "" {
		origin = "http://" + outer.Addr().String()
	}
	s := &control.Server{Store: store, Origin: origin, AllowHTTP: dev && strings.HasPrefix(origin, "http:"), History: history}
	if err = os.MkdirAll(filepath.Dir(socket), 0700); err != nil {
		return err
	}
	if info, err := os.Lstat(socket); err == nil {
		if info.Mode()&os.ModeSocket == 0 {
			return fmt.Errorf("existing Runner socket path is not a socket")
		}
		probe, err := net.DialTimeout("unix", socket, 400*time.Millisecond)
		if err == nil {
			probe.Close()
			return fmt.Errorf("Runner socket is already live")
		}
		if !errors.Is(err, syscall.ECONNREFUSED) {
			return err
		}
		if err = os.Remove(socket); err != nil {
			return err
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	inner, err := net.ListenUnix("unix", &net.UnixAddr{Name: socket, Net: "unix"})
	if err != nil {
		return err
	}
	defer inner.Close()
	if gid >= 0 {
		if err = os.Chown(socket, -1, gid); err != nil {
			return err
		}
		if err = os.Chmod(socket, 0660); err != nil {
			return err
		}
	} else if err = os.Chmod(socket, 0600); err != nil {
		return err
	}
	public := &http.Server{Handler: s.Public(web.Handler()), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 15 * time.Second, IdleTimeout: 60 * time.Second}
	internal := &http.Server{Handler: s.Internal(), ReadHeaderTimeout: 5 * time.Second, ReadTimeout: 20 * time.Second, WriteTimeout: 20 * time.Second, IdleTimeout: 30 * time.Second}
	maintenance, cancel := context.WithCancel(ctx)
	defer cancel()
	go s.Maintenance(maintenance)
	result := make(chan error, 2)
	go func() { result <- public.Serve(outer) }()
	go func() { result <- internal.Serve(peerListener{inner, uid}) }()
	defer public.Close()
	defer internal.Close()
	if ready != nil {
		if err := ready(store, origin); err != nil {
			return err
		}
	}
	log.Printf("NextBot listening on %s for %s", outer.Addr(), origin)
	select {
	case <-ctx.Done():
		_ = public.Close()
		_ = internal.Close()
		return nil
	case err = <-result:
		_ = public.Close()
		_ = internal.Close()
		if errors.Is(err, http.ErrServerClosed) {
			return nil
		}
		return err
	}
}

type peerListener struct {
	*net.UnixListener
	uid uint32
}

func (l peerListener) Accept() (net.Conn, error) {
	for {
		conn, err := l.AcceptUnix()
		if err != nil {
			return nil, err
		}
		raw, err := conn.SyscallConn()
		var uid uint32
		if err == nil {
			var socketErr error
			err = raw.Control(func(fd uintptr) {
				uid, socketErr = peerUID(int(fd))
			})
			if err == nil {
				err = socketErr
			}
		}
		if err == nil && uid == l.uid {
			return conn, nil
		}
		_ = conn.Close()
	}
}
