package runner

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"nextbot/internal/control"
)

type Config struct {
	Dir, Socket, CodexHome, CodexVersion, ClaudeHome, ClaudeVersion, ContainerEngine string
	EnableManagedTests                                                               bool
	CodexReviewVersion, ClaudeExecutionVersion                                       string
}
type Runner struct {
	cfg      Config
	c        *client
	j        *journal
	boot     string
	mu       sync.Mutex
	active   map[string]*activeAttempt
	inflight map[string]bool
	aborted  map[string]bool
	caps     map[string]control.Capabilities
}
type activeAttempt struct {
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	task      control.Task
	workspace *workspace
	test      control.TestDefinition
	options   control.Command
	tools     map[string]chan toolResult
	testLogs  []archiveEntry
	unknown   bool
	testDone  chan struct{}
	answerAck chan error
}
type toolResult struct {
	Text string
	OK   bool
}

func Run(ctx context.Context, cfg Config) error {
	if err := os.MkdirAll(cfg.Dir, 0700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(cfg.Dir, "runner.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		return fmt.Errorf("runner state is already in use: %w", err)
	}
	j, err := openJournal(cfg.Dir)
	if err != nil {
		return err
	}
	defer j.db.Close()
	r := &Runner{cfg: cfg, c: newClient(cfg.Socket), j: j, boot: control.ID(), active: map[string]*activeAttempt{}, inflight: map[string]bool{}, aborted: map[string]bool{}}
	r.caps = r.probe(ctx)
	_, err = retry(ctx, func() (struct{}, error) { return struct{}{}, r.c.hello(ctx, r.boot, r.capabilities()) })
	if err != nil {
		return err
	}
	if err = r.reconcile(ctx); err != nil {
		return err
	}
	return r.loop(ctx)
}
func (r *Runner) loop(ctx context.Context) error {
	heartbeat := time.NewTicker(5 * time.Second)
	defer heartbeat.Stop()
	discover := time.NewTicker(30 * time.Second)
	defer discover.Stop()
	poll := time.NewTicker(450 * time.Millisecond)
	defer poll.Stop()
	defer r.stop()
	for {
		select {
		case <-ctx.Done():
			return nil
		case <-heartbeat.C:
			_, err := post[any](ctx, r.c, "heartbeat", map[string]any{"adapterCapabilities": r.capabilities()})
			var e *remoteError
			if errors.As(err, &e) && e.Code == "stale_generation" {
				_ = r.c.hello(ctx, r.boot, r.capabilities())
			}
		case <-discover.C:
			go r.refreshCapabilities(ctx)
		case <-poll.C:
			type claims struct{ Items []control.Command }
			out, err := post[claims](ctx, r.c, "commands/claim", map[string]int{"maxCount": 8})
			if err != nil {
				continue
			}
			for _, c := range out.Items {
				r.schedule(ctx, c)
			}
		}
	}
}
func (r *Runner) refreshCapabilities(ctx context.Context) {
	caps := r.probe(ctx)
	r.mu.Lock()
	defer r.mu.Unlock()
	// The heartbeat accesses capabilities through the same lock.
	r.caps = caps
}
func (r *Runner) capabilities() map[string]control.Capabilities {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.caps
}
func (r *Runner) stop() {
	r.mu.Lock()
	as := make([]*activeAttempt, 0, len(r.active))
	for _, a := range r.active {
		a.cancel()
		as = append(as, a)
	}
	r.mu.Unlock()
	for _, a := range as {
		select {
		case <-a.done:
		case <-time.After(5 * time.Second):
		}
	}
}
func (r *Runner) reconcile(ctx context.Context) error {
	reports, err := r.j.pending()
	if err != nil {
		return err
	}
	for _, p := range reports {
		if err = r.send(ctx, p.Route, p.Report); err != nil && !transient(err) {
			log.Printf("reconciliation report %s: %v", p.Report.ID, err)
		}
	}
	commands, err := r.j.unfinished()
	if err != nil {
		return err
	}
	for _, c := range commands {
		if c.Type == "resolve_base" {
			if err = r.j.record(c, "rejected"); err != nil {
				return err
			}
			continue
		}
		if err = r.receipt(ctx, c, "unknown", "runner restarted before result could be verified", nil); err != nil {
			return err
		}
		if err = r.j.record(c, "unknown"); err != nil {
			return err
		}
	}
	return nil
}
func (r *Runner) schedule(ctx context.Context, c control.Command) {
	r.mu.Lock()
	if r.inflight[c.ID] {
		r.mu.Unlock()
		return
	}
	r.inflight[c.ID] = true
	r.mu.Unlock()
	go func() {
		defer func() { r.mu.Lock(); delete(r.inflight, c.ID); r.mu.Unlock() }()
		if err := r.handle(ctx, c); err != nil && ctx.Err() == nil {
			log.Printf("command %s (%s): %v", c.ID, c.Type, err)
		}
	}()
}
func (r *Runner) handle(ctx context.Context, c control.Command) error {
	phase, err := r.j.phase(c.ID)
	if err != nil {
		return err
	}
	if phase == "intent" || phase == "" && (c.Phase == "received" || c.Phase == "started") {
		if c.Type == "resolve_base" {
			// Retrying a read-only Git lookup cannot repeat a repository mutation.
			return r.resolve(ctx, c)
		}
		return r.unknown(ctx, c, "previously received command has no verified outcome")
	}
	if phase != "" && phase != "rejected" {
		return nil
	}
	if phase == "rejected" && c.Type != "resolve_base" {
		return nil
	}
	if err = r.receipt(ctx, c, "received", "", nil); err != nil {
		return err
	}
	if err = r.j.record(c, "intent"); err != nil {
		return err
	}
	switch c.Type {
	case "resolve_base":
		err = r.resolve(ctx, c)
	case "start_execution", "start_review":
		err = r.start(ctx, c)
	case "cancel_attempt":
		err = r.cancel(ctx, c)
	case "resolve_tool", "deliver_answer", "run_registered_test":
		err = r.deliver(ctx, c)
	default:
		err = r.receipt(ctx, c, "rejected", "unknown command", nil)
	}
	if err != nil {
		return err
	}
	phase, err = r.j.phase(c.ID)
	if err != nil || phase != "intent" {
		return err
	}
	return r.j.record(c, "settled")
}
func makeReport(c control.Command, kind, phase, result string, payload map[string]any) control.Report {
	return control.Report{ID: control.ID(), TaskID: c.TaskID, AttemptID: c.AttemptID, CommandID: c.ID, Fence: c.Fence, LeaseID: c.LeaseID, Type: kind, Phase: phase, Result: result, Payload: payload}
}
func (r *Runner) receipt(ctx context.Context, c control.Command, phase, status string, payload map[string]any) error {
	return r.send(ctx, "receipts", makeReport(c, "", phase, status, payload))
}
func (r *Runner) unknown(ctx context.Context, c control.Command, message string) error {
	if err := r.receipt(ctx, c, "unknown", message, nil); err != nil {
		return err
	}
	return r.j.record(c, "unknown")
}
func (r *Runner) report(ctx context.Context, c control.Command, kind string, payload map[string]any) error {
	return r.send(ctx, "reports", makeReport(c, kind, "", "", payload))
}
func (r *Runner) send(ctx context.Context, route string, report control.Report) error {
	if err := r.j.add(route, report); err != nil {
		return err
	}
	sendOnce := func() (any, error) { return post[any](ctx, r.c, route, report) }
	var err error
	if report.Phase == "received" {
		_, err = sendOnce()
	} else {
		_, err = retry(ctx, sendOnce)
	}
	if err == nil || !transient(err) || report.Phase == "received" {
		if ackErr := r.j.acknowledge(report.ID); err == nil {
			err = ackErr
		}
	}
	return err
}
func fromMap[T any](payload any) (T, error) {
	var out T
	b, err := json.Marshal(payload)
	if err == nil {
		err = json.Unmarshal(b, &out)
	}
	return out, err
}
func str(m map[string]any, k string) string { v, _ := m[k].(string); return v }
func (r *Runner) resolve(ctx context.Context, c control.Command) error {
	out, err := git(ctx, str(c.Payload, "path"), nil, nil, "rev-parse", "--verify", "--end-of-options", str(c.Payload, "refName")+"^{commit}")
	data := map[string]any{}
	if err != nil {
		data["reason"] = "Git 引用无法解析为有效提交"
	} else {
		data["commitId"] = stringTrim(out)
	}
	return r.report(ctx, c, "base_resolved", data)
}
func (r *Runner) start(ctx context.Context, c control.Command) error {
	task, err := fromMap[control.Task](c.Payload["task"])
	if err != nil {
		return err
	}
	p, err := fromMap[control.Project](c.Payload["project"])
	if err != nil {
		return err
	}
	test, err := fromMap[control.TestDefinition](c.Payload["test"])
	if err != nil {
		return err
	}
	runCtx, cancel := context.WithTimeout(ctx, 45*time.Minute)
	defer cancel()
	a := &activeAttempt{ctx: runCtx, cancel: cancel, done: make(chan struct{}), task: task, test: test, options: c, tools: map[string]chan toolResult{}}
	r.mu.Lock()
	r.active[c.AttemptID] = a
	if r.aborted[c.AttemptID] {
		a.cancel()
	}
	r.mu.Unlock()
	defer func() { r.mu.Lock(); close(a.done); delete(r.active, c.AttemptID); r.mu.Unlock() }()
	if runCtx.Err() != nil {
		return r.receipt(ctx, c, "rejected", "cancelled before start", nil)
	}
	caps := r.capabilities()
	if c.Type == "start_review" {
		if !caps[task.Reviewer].Review {
			return r.receipt(ctx, c, "rejected", "只读复核配置不可用", nil)
		}
		return r.review(ctx, a, c)
	}
	if !caps[task.Agent].Execution || test.ID != "" && !caps[task.Agent].Tests {
		return r.receipt(ctx, c, "rejected", "执行或受管测试配置不可用", nil)
	}
	w, err := createWorkspace(runCtx, r.cfg.Dir, p, task, c.AttemptID)
	if err != nil {
		return r.receipt(ctx, c, "rejected", err.Error(), nil)
	}
	a.workspace = w
	defer w.Root.Close()
	if previous, ok := c.Payload["previousArtifact"]; ok {
		art, err := fromMap[control.Artifact](previous)
		if err != nil {
			return err
		}
		patch, err := r.entry(runCtx, art.ID, "change.patch")
		if err != nil {
			return r.receipt(ctx, c, "rejected", "无法读取上一版本差异", nil)
		}
		if len(patch) > 0 {
			if _, err = git(runCtx, w.Path, nil, patch, "apply", "--binary", "--"); err != nil {
				return r.receipt(ctx, c, "rejected", "无法在固定基准应用上一版本差异", nil)
			}
		}
	}
	var summary string
	var started, stopped bool
	var runErr error
	if task.Agent == "claude" {
		summary, started, stopped, runErr = r.executeClaude(ctx, a, c)
	} else {
		summary, started, stopped, runErr = r.execute(ctx, a, c)
	}
	r.mu.Lock()
	testDone := a.testDone
	r.mu.Unlock()
	if testDone != nil {
		select {
		case <-testDone:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	r.mu.Lock()
	unknown := a.unknown || !stopped
	a.unknown = unknown
	logs := append([]archiveEntry{}, a.testLogs...)
	r.mu.Unlock()
	if !stopped || unknown {
		return r.unknown(ctx, c, "进程或受管动作的停止结果待核对")
	}
	if !started && a.ctx.Err() == nil && runErr != nil {
		return r.receipt(ctx, c, "rejected", runErr.Error(), nil)
	}
	outcome := "succeeded"
	reason := ""
	if runErr != nil {
		outcome = "interrupted"
		reason = runErr.Error()
	}
	patch, hash, diffErr := w.diff(ctx, r.cfg.Dir)
	archiveID := ""
	if diffErr != nil {
		outcome = "interrupted"
		reason = diffErr.Error()
	} else {
		archiveID, err = r.archive(ctx, a, c, patch, hash, summary, logs)
		if err != nil {
			if ctx.Err() != nil {
				return err
			}
			outcome = "interrupted"
			reason = "归档未完成：" + err.Error()
		}
	}
	return r.report(ctx, c, "execution_finished", map[string]any{"outcome": outcome, "reason": reason, "stopped": true, "artifactVersionId": archiveID})
}
func (r *Runner) cancel(ctx context.Context, c control.Command) error {
	startID := str(c.Payload, "startCommandId")
	r.mu.Lock()
	a := r.active[c.AttemptID]
	r.aborted[c.AttemptID] = true
	inflight := r.inflight[startID]
	if a != nil {
		a.cancel()
	}
	r.mu.Unlock()
	if a != nil {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-a.done:
		}
	}
	phase, err := r.j.phase(startID)
	if err != nil {
		return err
	}
	if phase == "unknown" || phase == "intent" && a == nil && !inflight {
		return r.unknown(ctx, c, "旧尝试进程尚未核实")
	}
	if a != nil {
		r.mu.Lock()
		unsafe := a.unknown
		r.mu.Unlock()
		if unsafe {
			return r.unknown(ctx, c, "尝试或受管测试进程尚未核实")
		}
	}
	return r.report(ctx, c, "cancel_confirmed", map[string]any{"stopped": true})
}
func stringTrim(b []byte) string {
	s := string(b)
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ') {
		s = s[:len(s)-1]
	}
	return s
}
func process(ctx context.Context, path, dir string, args, env []string) *exec.Cmd {
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Dir = dir
	cmd.Env = env
	cmd.SysProcAttr = processAttributes()
	cmd.Cancel = func() error { return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) }
	cmd.WaitDelay = 3 * time.Second
	return cmd
}
func gone(cmd *exec.Cmd) bool {
	if cmd.Process == nil {
		return true
	}
	pid := cmd.Process.Pid
	if errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH) {
		return true
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	for range 10 {
		if errors.Is(syscall.Kill(-pid, 0), syscall.ESRCH) {
			return true
		}
		time.Sleep(50 * time.Millisecond)
	}
	return false
}
