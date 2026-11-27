package runner

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"nextbot/internal/control"
)

func (r *Runner) executeClaude(ctx context.Context, a *activeAttempt, c control.Command) (string, bool, bool, error) {
	return r.runClaude(ctx, a, c, executionPrompt(a.task), c.AttemptID, true)
}

func (r *Runner) runClaude(serverCtx context.Context, a *activeAttempt, c control.Command, prompt, workspaceID string, execution bool) (string, bool, bool, error) {
	path, err := exec.LookPath("claude")
	if err != nil {
		return "", false, true, fmt.Errorf("Claude CLI 不可用")
	}
	scratch, err := os.MkdirTemp(r.cfg.Dir, "claude-")
	if err != nil {
		return "", false, true, err
	}
	defer os.RemoveAll(scratch)
	config := "{\"mcpServers\":{}}"
	permission := []string{}
	if execution {
		var closeBridge func()
		config, closeBridge, err = r.startBridge(a, c)
		if err != nil {
			return "", false, true, err
		}
		defer closeBridge()
		permission = []string{"--permission-mode", "dontAsk", "--allowedTools", "mcp__nextbot__*"}
	}
	args := []string{"-p", "--output-format", "json", "--tools", "", "--strict-mcp-config", "--mcp-config", config, "--setting-sources", "", "--disable-slash-commands", "--no-session-persistence"}
	cmd := process(a.ctx, path, scratch, append(args, permission...), providerEnv("CLAUDE_CONFIG_DIR", r.cfg.ClaudeHome))
	cmd.Stdin = strings.NewReader(prompt)
	out := &limitedBuffer{limit: 512 << 10}
	cmd.Stdout = out
	if err = cmd.Start(); err != nil {
		return "", false, true, fmt.Errorf("无法启动 Claude 进程")
	}
	receiptErr := r.j.process(c, cmd.Process.Pid)
	if receiptErr == nil {
		receiptErr = r.receipt(serverCtx, c, "started", "", map[string]any{"workspaceId": workspaceID, "pid": cmd.Process.Pid})
	}
	waitErr := cmd.Wait()
	stopped := gone(cmd) && r.j.processStopped(c) == nil
	if receiptErr != nil {
		return "", true, stopped, receiptErr
	}
	if waitErr != nil {
		return "", true, stopped, fmt.Errorf("Claude 进程未正常退出")
	}
	var response struct {
		Result  string
		Error   bool   `json:"is_error"`
		Session string `json:"session_id"`
	}
	if err = json.Unmarshal([]byte(out.String()), &response); err != nil || response.Error {
		return "", true, stopped, fmt.Errorf("Claude 未返回有效结果")
	}
	if execution && response.Session != "" {
		err = r.report(serverCtx, c, "session_observed", map[string]any{"sessionId": response.Session})
	}
	return response.Result, true, stopped, err
}

type toolCall struct {
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

func (r *Runner) startBridge(a *activeAttempt, c control.Command) (string, func(), error) {
	bin, err := os.Executable()
	if err != nil {
		return "", nil, err
	}
	dir, err := os.MkdirTemp("/tmp", "nextbot-tools-")
	if err != nil {
		return "", nil, err
	}
	socket := filepath.Join(dir, "tools.sock")
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Net: "unix", Name: socket})
	if err != nil {
		_ = os.RemoveAll(dir)
		return "", nil, err
	}
	bearer := control.ID() + control.ID()
	var serial sync.Mutex
	mux := http.NewServeMux()
	authorized := func(fn http.HandlerFunc) http.HandlerFunc {
		return func(w http.ResponseWriter, req *http.Request) {
			if req.Header.Get("Authorization") != "Bearer "+bearer {
				w.WriteHeader(403)
				return
			}
			fn(w, req)
		}
	}
	mux.HandleFunc("GET /tools", authorized(func(w http.ResponseWriter, req *http.Request) {
		tools := []map[string]any{}
		for _, raw := range toolSpecs(a.test.ID != "") {
			entry := raw.(map[string]any)
			tools = append(tools, map[string]any{"name": entry["name"], "description": entry["description"], "inputSchema": entry["inputSchema"]})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"tools": tools})
	}))
	mux.HandleFunc("POST /call", authorized(func(w http.ResponseWriter, req *http.Request) {
		var call toolCall
		if err := json.NewDecoder(http.MaxBytesReader(w, req.Body, 5<<20)).Decode(&call); err != nil {
			w.WriteHeader(400)
			return
		}
		serial.Lock()
		defer serial.Unlock()
		result := r.tool(a, c, call.Name, call.Arguments)
		_ = json.NewEncoder(w).Encode(map[string]any{"isError": !result.OK, "content": []map[string]string{{"type": "text", "text": result.Text}}})
	}))
	mux.HandleFunc("POST /ack", authorized(func(w http.ResponseWriter, req *http.Request) {
		r.answerDelivered(a, nil)
		w.WriteHeader(204)
	}))
	server := &http.Server{Handler: mux, ReadHeaderTimeout: 3 * time.Second}
	closeBridge := func() {
		_ = server.Close()
		_ = os.RemoveAll(dir)
	}
	go func() { _ = server.Serve(listener) }()
	config, err := json.Marshal(map[string]any{"mcpServers": map[string]any{
		"nextbot": map[string]any{"command": bin, "args": []string{"tool-bridge", "--socket", socket}, "env": map[string]string{"NEXTBOT_MCP_TOKEN": bearer}},
	}})
	if err != nil {
		closeBridge()
		return "", nil, err
	}
	return string(config), closeBridge, nil
}

// ServeBridge connects a single Claude stdio MCP session to its scoped Runner socket.
func ServeBridge(ctx context.Context, socket, token string, in io.Reader, out io.Writer) error {
	if socket == "" || len(token) != 64 {
		return fmt.Errorf("tool bridge requires an attempt socket and token")
	}
	client := &http.Client{Transport: &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
		return (&net.Dialer{Timeout: 3 * time.Second}).DialContext(ctx, "unix", socket)
	}}}
	request := func(method, path string, body io.Reader) (json.RawMessage, error) {
		req, err := http.NewRequestWithContext(ctx, method, "http://nextbot-tools"+path, body)
		if err != nil {
			return nil, err
		}
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := client.Do(req)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if resp.StatusCode >= 400 {
			return nil, fmt.Errorf("task tool request rejected")
		}
		return io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	}
	scanner := bufio.NewScanner(in)
	scanner.Buffer(make([]byte, 4096), 8<<20)
	encoder := json.NewEncoder(out)
	for scanner.Scan() {
		var msg rpcMessage
		if err := json.Unmarshal(scanner.Bytes(), &msg); err != nil {
			return err
		}
		if len(msg.ID) == 0 || string(msg.ID) == "null" {
			continue
		}
		reply := map[string]any{"jsonrpc": "2.0", "id": msg.ID}
		var result any
		var err error
		answer := false
		switch msg.Method {
		case "initialize":
			var initialize struct {
				Version string `json:"protocolVersion"`
			}
			err = json.Unmarshal(msg.Params, &initialize)
			result = map[string]any{"protocolVersion": initialize.Version, "capabilities": map[string]any{"tools": map[string]any{}}, "serverInfo": map[string]string{"name": "nextbot", "version": "0.1.0"}}
		case "ping":
			result = map[string]any{}
		case "tools/list":
			result, err = request(http.MethodGet, "/tools", nil)
		case "tools/call":
			var call toolCall
			if err = json.Unmarshal(msg.Params, &call); err == nil {
				result, err = request(http.MethodPost, "/call", strings.NewReader(string(msg.Params)))
				answer = call.Name == "nb_ask_owner" && err == nil
			}
		default:
			err = errors.New("unsupported MCP method")
		}
		if err != nil {
			reply["error"] = map[string]any{"code": -32603, "message": "nextbot tool operation failed"}
		} else {
			reply["result"] = result
		}
		if err = encoder.Encode(reply); err != nil {
			return err
		}
		if answer {
			if _, err = request(http.MethodPost, "/ack", nil); err != nil {
				return err
			}
		}
	}
	return scanner.Err()
}
