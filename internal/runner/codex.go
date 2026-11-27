package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"nextbot/internal/control"
)

type rpcMessage struct {
	ID     json.RawMessage `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  json.RawMessage `json:"error,omitempty"`
}
type rpc struct {
	encoder *json.Encoder
	scanner *bufio.Scanner
	seq     int
}

func (p *rpc) next() (rpcMessage, error) {
	var m rpcMessage
	if !p.scanner.Scan() {
		if err := p.scanner.Err(); err != nil {
			return m, err
		}
		return m, io.EOF
	}
	err := json.Unmarshal(p.scanner.Bytes(), &m)
	return m, err
}
func (p *rpc) call(method string, params any) (json.RawMessage, error) {
	p.seq++
	if err := p.encoder.Encode(map[string]any{"id": p.seq, "method": method, "params": params}); err != nil {
		return nil, err
	}
	for {
		m, err := p.next()
		if err != nil {
			return nil, err
		}
		if m.Method != "" && len(m.ID) > 0 {
			if err = p.reject(m.ID); err != nil {
				return nil, err
			}
			continue
		}
		var id int
		_ = json.Unmarshal(m.ID, &id)
		if id != p.seq {
			continue
		}
		if len(m.Error) > 0 && string(m.Error) != "null" {
			return nil, fmt.Errorf("Codex app-server rejected %s: %s", method, string(m.Error))
		}
		return m.Result, nil
	}
}
func (p *rpc) reject(id json.RawMessage) error {
	return p.encoder.Encode(map[string]any{"id": id, "error": map[string]any{"code": -32601, "message": "This tool or approval route is disabled in NextBot."}})
}
func (p *rpc) initialized() error {
	_, err := p.call("initialize", map[string]any{"clientInfo": map[string]string{"name": "nextbot", "title": "NextBot", "version": "0.1.0"}, "capabilities": map[string]bool{"experimentalApi": true}})
	if err != nil {
		return err
	}
	return p.encoder.Encode(map[string]any{"method": "initialized"})
}
func providerEnv(homeKey, home string) []string {
	env := cleanEnv()
	if home != "" {
		env = append(env, homeKey+"="+home)
	}
	for _, name := range []string{"OPENAI_API_KEY", "ANTHROPIC_API_KEY", "HTTPS_PROXY", "HTTP_PROXY", "NO_PROXY", "SSL_CERT_FILE", "SSL_CERT_DIR"} {
		if v := os.Getenv(name); v != "" {
			env = append(env, name+"="+v)
		}
	}
	return env
}
func (r *Runner) probe(ctx context.Context) map[string]control.Capabilities {
	out := map[string]control.Capabilities{}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	for _, spec := range []struct{ name, home, key, execution, review string }{
		{"codex", r.cfg.CodexHome, "CODEX_HOME", r.cfg.CodexVersion, r.cfg.CodexReviewVersion},
		{"claude", r.cfg.ClaudeHome, "CLAUDE_CONFIG_DIR", r.cfg.ClaudeExecutionVersion, r.cfg.ClaudeVersion},
	} {
		c := control.Capabilities{}
		path, err := exec.LookPath(spec.name)
		if err != nil {
			c.Reason = "未安装 CLI"
			out[spec.name] = c
			continue
		}
		c.Installed = true
		env := providerEnv(spec.key, spec.home)
		version, err := process(ctx, path, r.cfg.Dir, []string{"--version"}, env).Output()
		if err == nil {
			c.Version = strings.TrimSpace(string(version))
		}
		if spec.name == "codex" {
			status, err := process(ctx, path, r.cfg.Dir, []string{"login", "status"}, env).CombinedOutput()
			c.Authenticated = err == nil && strings.Contains(string(status), "Logged in") || os.Getenv("OPENAI_API_KEY") != ""
		} else {
			status, err := process(ctx, path, r.cfg.Dir, []string{"auth", "status", "--json"}, env).Output()
			var auth struct {
				LoggedIn bool `json:"loggedIn"`
			}
			_ = json.Unmarshal(status, &auth)
			c.Authenticated = err == nil && auth.LoggedIn || os.Getenv("ANTHROPIC_API_KEY") != ""
		}
		runVerified := spec.execution != "" && c.Version == spec.execution
		reviewVerified := spec.review != "" && c.Version == spec.review
		if !runVerified && !reviewVerified {
			c.Reason = "当前 CLI 版本尚未由主机管理员确认执行配置"
			out[spec.name] = c
			continue
		}
		if !c.Authenticated {
			c.Reason = "CLI 登录不可用"
			out[spec.name] = c
			continue
		}
		if spec.name == "codex" {
			probeDir, err := os.MkdirTemp(r.cfg.Dir, "probe-")
			if err == nil {
				err = r.probeCodex(ctx, probeDir)
				_ = os.RemoveAll(probeDir)
			}
			if err != nil {
				c.Reason = "Codex 受限协议握手失败"
				out[spec.name] = c
				continue
			}
		}
		c.Execution = runVerified
		c.Review = reviewVerified
		c.Questions = runVerified
		c.Cancel = true
		c.Tests = runVerified && r.cfg.EnableManagedTests && r.engineReady(ctx)
		out[spec.name] = c
	}
	return out
}
func (r *Runner) codex(ctx context.Context, dir string) (*exec.Cmd, *rpc, *limitedBuffer, error) {
	path, err := exec.LookPath("codex")
	if err != nil {
		return nil, nil, nil, err
	}
	args := []string{"--disable", "shell_tool", "--disable", "unified_exec", "--disable", "multi_agent", "--disable", "exec_permission_approvals", "--disable", "apps", "-c", "web_search=\"disabled\"", "-c", "mcp_servers={}", "app-server", "--listen", "stdio://"}
	cmd := process(ctx, path, dir, args, providerEnv("CODEX_HOME", r.cfg.CodexHome))
	log := &limitedBuffer{limit: 8192}
	cmd.Stderr = log
	in, err := cmd.StdinPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	out, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, err
	}
	scanner := bufio.NewScanner(out)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	if err = cmd.Start(); err != nil {
		return nil, nil, nil, err
	}
	return cmd, &rpc{encoder: json.NewEncoder(in), scanner: scanner}, log, nil
}
func threadConfig(dir string, tools []any) map[string]any {
	return map[string]any{
		"cwd": dir, "approvalPolicy": "never", "sandbox": "read-only", "environments": []any{},
		"dynamicTools": tools, "ephemeral": true, "baseInstructions": "You are a constrained code editor. Use only the NextBot file, question and registered-test tools.",
	}
}
func (r *Runner) probeCodex(ctx context.Context, dir string) error {
	pc, cancel := context.WithCancel(ctx)
	defer cancel()
	cmd, rpc, _, err := r.codex(pc, dir)
	if err != nil {
		return err
	}
	defer func() { cancel(); _ = cmd.Wait(); _ = gone(cmd) }()
	if err = rpc.initialized(); err != nil {
		return err
	}
	_, err = rpc.call("thread/start", threadConfig(dir, toolSpecs(false)))
	return err
}
func toolSpecs(test bool) []any {
	spec := func(name, description string, required ...string) any {
		if required == nil {
			required = []string{}
		}
		props := map[string]any{}
		for _, p := range required {
			props[p] = map[string]string{"type": "string"}
		}
		return map[string]any{"type": "function", "name": name, "description": description, "deferLoading": false, "inputSchema": map[string]any{"type": "object", "properties": props, "required": required, "additionalProperties": false}}
	}
	tools := []any{
		spec("nb_list_files", "List files relative to the current task worktree."),
		spec("nb_read_file", "Read a UTF-8 task file by relative path.", "path"),
		spec("nb_write_file", "Create or replace a UTF-8 task file.", "path", "text"),
		spec("nb_delete_file", "Delete a task file by relative path.", "path"),
		spec("nb_ask_owner", "Ask the task owner a question and wait for an answer.", "question"),
	}
	if test {
		tools = append(tools, spec("nb_request_test", "Request approval to run the assigned registered test on the current files."))
	}
	return tools
}
func executionPrompt(t control.Task) string {
	prompt := fmt.Sprintf("Task: %s\nGoal: %s\nScope: %s\nAcceptance: %s\nPrevious feedback: %s\nUse NextBot tools to inspect and edit files. Submit a factual summary. Describe tests as run only when a NextBot test tool returned a result.", t.Title, t.Goal, t.Scope, t.Criteria, t.Feedback)
	if t.TestID != "" {
		prompt += "\nRequest the registered test through nb_request_test after editing; if files change afterward, request it again."
	}
	return prompt
}
func (r *Runner) execute(ctx context.Context, a *activeAttempt, c control.Command) (string, bool, bool, error) {
	return r.runCodex(ctx, a, c, toolSpecs(a.test.ID != ""), executionPrompt(a.task), c.AttemptID)
}
func (r *Runner) answerDelivered(a *activeAttempt, delivery error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if a.answerAck != nil {
		a.answerAck <- delivery
		a.answerAck = nil
	}
}
func (r *Runner) runCodex(serverCtx context.Context, a *activeAttempt, c control.Command, tools []any, prompt, workspaceID string) (string, bool, bool, error) {
	scratch, err := os.MkdirTemp(r.cfg.Dir, "codex-")
	if err != nil {
		return "", false, true, err
	}
	defer os.RemoveAll(scratch)
	cliCtx, stop := context.WithCancel(a.ctx)
	defer stop()
	cmd, rpc, _, err := r.codex(cliCtx, scratch)
	if err != nil {
		return "", false, true, err
	}
	var summary string
	runErr := func() error {
		if err := r.j.process(c, cmd.Process.Pid); err != nil {
			return fmt.Errorf("记录 Codex 进程身份失败: %w", err)
		}
		if err := r.receipt(serverCtx, c, "started", "", map[string]any{"workspaceId": workspaceID, "pid": cmd.Process.Pid}); err != nil {
			return err
		}
		if err := rpc.initialized(); err != nil {
			return err
		}
		res, err := rpc.call("thread/start", threadConfig(scratch, tools))
		if err != nil {
			return err
		}
		var thread struct{ Thread struct{ ID string } }
		if json.Unmarshal(res, &thread) != nil || thread.Thread.ID == "" {
			return fmt.Errorf("Codex did not return a thread ID")
		}
		if c.Type == "start_execution" {
			if err = r.report(serverCtx, c, "session_observed", map[string]any{"sessionId": thread.Thread.ID}); err != nil {
				return err
			}
		}
		_, err = rpc.call("turn/start", map[string]any{"threadId": thread.Thread.ID, "environments": []any{}, "input": []any{map[string]any{"type": "text", "text": prompt, "text_elements": []any{}}}})
		if err != nil {
			return err
		}
		for {
			m, err := rpc.next()
			if err != nil {
				return err
			}
			switch m.Method {
			case "item/tool/call":
				if len(tools) == 0 {
					if err := rpc.reject(m.ID); err != nil {
						return err
					}
					continue
				}
				var req struct {
					Tool      string
					Arguments map[string]any
				}
				if err = json.Unmarshal(m.Params, &req); err != nil {
					return err
				}
				result := r.tool(a, c, req.Tool, req.Arguments)
				err = rpc.encoder.Encode(map[string]any{"id": m.ID, "result": map[string]any{"success": result.OK, "contentItems": []any{map[string]string{"type": "inputText", "text": result.Text}}}})
				if req.Tool == "nb_ask_owner" {
					r.answerDelivered(a, err)
				}
				if err != nil {
					return err
				}
			case "item/completed":
				var change struct{ Item struct{ Type, Text string } }
				if json.Unmarshal(m.Params, &change) == nil && change.Item.Type == "agentMessage" {
					summary = change.Item.Text
				}
			case "turn/completed":
				var event struct {
					Turn struct {
						Status string
						Error  struct {
							Info struct {
								Connection struct {
									Status int `json:"httpStatusCode"`
								} `json:"httpConnectionFailed"`
							} `json:"codexErrorInfo"`
						}
					}
				}
				if json.Unmarshal(m.Params, &event) != nil {
					return fmt.Errorf("Codex 返回无效的回合状态")
				}
				if status := event.Turn.Error.Info.Connection.Status; status > 0 {
					return fmt.Errorf("模型服务请求失败 (HTTP %d)", status)
				}
				if event.Turn.Status != "completed" {
					return fmt.Errorf("Codex turn did not complete (%s)", event.Turn.Status)
				}
				return nil
			default:
				if m.Method != "" && len(m.ID) > 0 {
					if err = rpc.reject(m.ID); err != nil {
						return err
					}
				}
			}
		}
	}()
	stop()
	_ = cmd.Wait()
	if errors.Is(runErr, io.EOF) {
		runErr = fmt.Errorf("Codex app-server exited before delivering a result")
	}
	if a.ctx.Err() != nil && runErr == nil {
		runErr = a.ctx.Err()
	}
	stopped := gone(cmd)
	if stopped && r.j.processStopped(c) != nil {
		stopped = false
	}
	return summary, true, stopped, runErr
}
func (r *Runner) tool(a *activeAttempt, c control.Command, name string, arg map[string]any) toolResult {
	if a.ctx.Err() != nil {
		return toolResult{"Attempt stopped", false}
	}
	var text string
	var err error
	switch name {
	case "nb_list_files":
		var files []fileRecord
		files, err = a.workspace.files("")
		if err == nil {
			var names []string
			for _, f := range files {
				names = append(names, f.Path)
			}
			text = strings.Join(names, "\n")
		}
	case "nb_read_file":
		text, err = a.workspace.read(str(arg, "path"))
	case "nb_write_file":
		err = a.workspace.write(str(arg, "path"), str(arg, "text"))
		text = "File written"
	case "nb_delete_file":
		err = a.workspace.remove(str(arg, "path"))
		text = "File removed"
	case "nb_ask_owner":
		q := strings.TrimSpace(str(arg, "question"))
		if q == "" || len(q) > 4000 {
			return toolResult{"Invalid question", false}
		}
		return r.waitForOwner(a, c, "question_requested", map[string]any{"question": q, "attemptId": c.AttemptID})
	case "nb_request_test":
		if a.test.ID == "" {
			return toolResult{"No registered test assigned", false}
		}
		return r.requestTest(a, c)
	default:
		return toolResult{"Tool is outside the task capability set", false}
	}
	if err != nil {
		return toolResult{err.Error(), false}
	}
	if len(text) > workspaceFileLimit {
		return toolResult{"Tool response exceeds the size limit", false}
	}
	return toolResult{text, true}
}
func (r *Runner) waitForOwner(a *activeAttempt, c control.Command, kind string, frozen map[string]any) toolResult {
	id := control.ID()
	if supplied := str(frozen, "snapshotId"); supplied != "" {
		id = supplied
	}
	frozen["inputDigest"] = control.Digest("input", frozen)
	ch := make(chan toolResult, 1)
	r.mu.Lock()
	a.tools[id] = ch
	r.mu.Unlock()
	defer func() { r.mu.Lock(); delete(a.tools, id); r.mu.Unlock() }()
	if err := r.report(a.ctx, c, kind, map[string]any{"requestId": id, "frozenInput": frozen}); err != nil {
		return toolResult{"Could not submit the request", false}
	}
	select {
	case res := <-ch:
		return res
	case <-a.ctx.Done():
		return toolResult{"Attempt stopped", false}
	}
}
func (r *Runner) requestTest(a *activeAttempt, c control.Command) toolResult {
	id := control.ID()
	base := filepath.Join(r.cfg.Dir, "snapshots")
	if err := os.MkdirAll(base, 0700); err != nil {
		return toolResult{err.Error(), false}
	}
	path := filepath.Join(base, id)
	if err := os.Mkdir(path, 0755); err != nil {
		return toolResult{err.Error(), false}
	}
	defer func() {
		r.mu.Lock()
		done := a.testDone
		r.mu.Unlock()
		if done != nil {
			<-done
		}
		_ = os.RemoveAll(path)
	}()
	if err := os.Chmod(path, 0755); err != nil {
		return toolResult{err.Error(), false}
	}
	files, err := a.workspace.files(path)
	if err != nil {
		return toolResult{err.Error(), false}
	}
	return r.waitForOwner(a, c, "approval_requested", map[string]any{
		"snapshotId": id, "snapshotHash": fingerprint(files), "attemptId": c.AttemptID, "workspaceId": c.AttemptID,
		"testId": a.test.ID, "label": a.test.Label, "image": a.test.Image, "args": a.test.Args, "timeoutSeconds": a.test.Timeout,
		"definitionDigest": control.Digest("test_definition", a.test), "scope": "isolated, offline, read-only input",
	})
}
