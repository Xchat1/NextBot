package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"nextbot/internal/control"
	"nextbot/internal/runner"
)

func desktop(ctx context.Context, flags *flag.FlagSet, args []string, stdout io.Writer) error {
	location := flags.String("data", "", "private desktop state directory")
	codexHome := flags.String("codex-home", "", "dedicated Codex login directory")
	codexVersion := flags.String("verified-codex-version", "", "verified exact Codex version for file execution")
	claudeHome := flags.String("claude-home", "", "dedicated Claude Code login directory")
	claudeVersion := flags.String("verified-claude-version", "", "verified exact Claude Code version for read-only review")
	requireReview := flags.Bool("require-review", false, "require independent review for newly submitted tasks")
	phoneOrigin := flags.String("phone-origin", "", "external HTTPS origin served by a private reverse proxy")
	phonePort := flags.Int("phone-proxy-port", 18787, "loopback proxy upstream port when phone access is enabled")
	parent := flags.Int("parent-pid", 0, "exit if the desktop app exits")
	if err := flags.Parse(args); err != nil {
		return err
	}
	dir, err := filepath.Abs(*location)
	if err != nil {
		return err
	}
	if *location == "" || strings.ContainsAny(dir, ",\r\n") {
		return fmt.Errorf("desktop requires a valid data directory")
	}
	addr := "127.0.0.1:0"
	if *phoneOrigin != "" {
		if *phonePort < 1 || *phonePort > 65535 {
			return fmt.Errorf("phone proxy port must be between 1 and 65535")
		}
		addr = net.JoinHostPort("127.0.0.1", strconv.Itoa(*phonePort))
	}
	socketDir, err := os.MkdirTemp("/tmp", "nextbot-desktop-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(socketDir)
	socket := filepath.Join(socketDir, "runner.sock")
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	if *parent > 0 {
		go watchDesktopParent(ctx, *parent, cancel)
	}
	controlReady := make(chan struct{})
	controlDone := make(chan error, 1)
	go func() {
		controlDone <- serve(ctx, controlConfig{
			dir: filepath.Join(dir, "control"), socket: socket, addr: addr,
			phoneOrigin: *phoneOrigin, history: 10000, devHTTP: true, uid: uint32(os.Getuid()),
			gid: -1, requireReview: *requireReview,
		},
			func(store *control.Store, origin string) error {
				ticket, err := store.OwnerTicket(false)
				if errors.Is(err, control.ErrOwnerClaimed) {
					ticket, err = "", nil
				}
				if err != nil {
					return err
				}
				if err := json.NewEncoder(stdout).Encode(map[string]string{"origin": origin, "ticket": ticket}); err != nil {
					return err
				}
				close(controlReady)
				return nil
			})
	}()
	select {
	case err := <-controlDone:
		return err
	case <-controlReady:
	}
	runnerDone := make(chan error, 1)
	go func() {
		runnerDone <- runner.Run(ctx, runner.Config{
			Dir: filepath.Join(dir, "runner"), Socket: socket, CodexHome: *codexHome, CodexVersion: *codexVersion,
			ClaudeHome: *claudeHome, ClaudeVersion: *claudeVersion,
		})
	}()
	select {
	case err := <-controlDone:
		cancel()
		return errors.Join(err, desktopStopError(<-runnerDone))
	case err := <-runnerDone:
		cancel()
		return errors.Join(desktopStopError(err), <-controlDone)
	}
}

func desktopStopError(err error) error {
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}

func watchDesktopParent(ctx context.Context, parent int, cancel context.CancelFunc) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for os.Getppid() == parent {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
	cancel()
}
