package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"testing"
	"time"
)

func TestDesktopLocalSessionSurvivesRestart(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	state := t.TempDir()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Jar: jar, Timeout: 3 * time.Second}
	start := func() (map[string]string, func()) {
		t.Helper()
		ctx, cancel := context.WithCancel(context.Background())
		t.Cleanup(cancel)
		reader, writer := io.Pipe()
		done := make(chan error, 1)
		go func() {
			err := desktop(ctx, flag.NewFlagSet("desktop", flag.ContinueOnError), []string{"--data", state}, writer)
			writer.CloseWithError(err)
			done <- err
		}()
		var ready map[string]string
		err := json.NewDecoder(reader).Decode(&ready)
		if err != nil {
			t.Fatal(err)
		}
		reader.Close()
		origin, err := url.Parse(ready["origin"])
		if err != nil || origin.Scheme != "http" || origin.Hostname() != "127.0.0.1" || origin.Port() == "" {
			t.Fatalf("invalid desktop origin: %v %v", origin, err)
		}
		return ready, func() {
			cancel()
			select {
			case err := <-done:
				if err != nil {
					t.Fatal(err)
				}
			case <-time.After(10 * time.Second):
				t.Fatal("desktop did not stop")
			}
		}
	}
	first, stop := start()
	if first["ticket"] == "" {
		t.Fatal("initial desktop did not issue a claim ticket")
	}
	body, err := json.Marshal(map[string]string{"ticket": first["ticket"], "deviceName": "Mac"})
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("POST", first["origin"]+"/api/v1/owner/claim", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Origin", first["origin"])
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", "desktop-test-claim")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("claim status %d", resp.StatusCode)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		resp, err := client.Get(first["origin"] + "/api/v1/nodes")
		if err != nil {
			t.Fatal(err)
		}
		var nodes struct {
			Items []struct{ Availability string }
		}
		err = json.NewDecoder(resp.Body).Decode(&nodes)
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			t.Fatalf("could not read Runner status: %d %v", resp.StatusCode, err)
		}
		if len(nodes.Items) == 1 && nodes.Items[0].Availability == "online" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("Runner could not connect over the private socket")
		}
		time.Sleep(50 * time.Millisecond)
	}
	stop()
	second, stop := start()
	defer stop()
	if second["ticket"] != "" {
		t.Fatal("desktop reissued an initial claim ticket")
	}
	resp, err = client.Get(second["origin"] + "/api/v1/session")
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("existing device session did not survive restart: %d", resp.StatusCode)
	}
}
