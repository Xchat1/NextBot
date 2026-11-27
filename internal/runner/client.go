package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"

	"nextbot/internal/control"
)

const prefix = "http://nextbot.local/internal/v1/nodes/local/"

type remoteError struct {
	Status int
	Code   string
}

func (e *remoteError) Error() string { return fmt.Sprintf("control response %d: %s", e.Status, e.Code) }

type client struct {
	http                   *http.Client
	mu                     sync.RWMutex
	connection, generation string
}

func newClient(socket string) *client {
	d := net.Dialer{Timeout: 3 * time.Second}
	return &client{http: &http.Client{Timeout: 10 * time.Second, Transport: &http.Transport{
		DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return d.DialContext(ctx, "unix", socket) },
	}}}
}
func (c *client) send(ctx context.Context, method, path, media string, data []byte) ([]byte, error) {
	r, err := http.NewRequestWithContext(ctx, method, prefix+path, bytes.NewReader(data))
	if err != nil {
		return nil, err
	}
	c.mu.RLock()
	r.Header.Set("NextBot-Connection", c.connection)
	r.Header.Set("NextBot-Generation", c.generation)
	c.mu.RUnlock()
	if media != "" {
		r.Header.Set("Content-Type", media)
	}
	resp, err := c.http.Do(r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	b, err := io.ReadAll(io.LimitReader(resp.Body, 25<<20))
	if err != nil {
		return nil, err
	}
	if resp.StatusCode >= 400 {
		var body struct{ Error struct{ Code string } }
		_ = json.Unmarshal(b, &body)
		return nil, &remoteError{resp.StatusCode, body.Error.Code}
	}
	return b, nil
}
func post[T any](ctx context.Context, c *client, path string, data any) (T, error) {
	var out T
	payload, err := json.Marshal(data)
	if err != nil {
		return out, err
	}
	b, err := c.send(ctx, "POST", path, "application/json", payload)
	if err == nil {
		err = json.Unmarshal(b, &out)
	}
	return out, err
}
func (c *client) hello(ctx context.Context, boot string, caps map[string]control.Capabilities) error {
	type hello struct {
		Connection string `json:"connectionId"`
		Generation string `json:"generation"`
	}
	h, err := post[hello](ctx, c, "hello", map[string]any{"bootId": boot, "protocolVersion": "1", "adapterCapabilities": caps})
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.connection, c.generation = h.Connection, h.Generation
	return nil
}
func transient(err error) bool {
	var e *remoteError
	return !errors.As(err, &e) || e.Status >= 500 || e.Code == "stale_generation"
}
func retry[T any](ctx context.Context, fn func() (T, error)) (T, error) {
	for {
		out, err := fn()
		if err == nil || !transient(err) {
			return out, err
		}
		select {
		case <-ctx.Done():
			return out, ctx.Err()
		case <-time.After(time.Second):
		}
	}
}
