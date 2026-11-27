package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"nextbot/internal/control"
	"nextbot/internal/runner"
)

func desktop(ctx context.Context, flags *flag.FlagSet, args []string, stdout io.Writer) error {
	location := flags.String("data", "", "private desktop state directory")
	codexHome := flags.String("codex-home", "", "dedicated Codex login directory")
	codexVersion := flags.String("verified-codex-version", "", "verified exact Codex version for file execution")
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
		controlDone <- serve(ctx, filepath.Join(dir, "control"), socket, "127.0.0.1:0", "", 10000, true, uint32(os.Getuid()), -1,
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
