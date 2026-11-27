package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/http/httputil"
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
	start := func() (map[string]string, func()) { return startTestDesktop(t, state) }
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

func startTestDesktop(t *testing.T, state string, extra ...string) (map[string]string, func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		err := desktop(ctx, flag.NewFlagSet("desktop", flag.ContinueOnError), append([]string{"--data", state}, extra...), writer)
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

func TestDesktopPhonePairingThroughHTTPSProxy(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	upstream, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	_, port, err := net.SplitHostPort(upstream.Addr().String())
	upstream.Close()
	if err != nil {
		t.Fatal(err)
	}
	target := &url.URL{Scheme: "http", Host: "127.0.0.1:" + port}
	proxy := httptest.NewTLSServer(httputil.NewSingleHostReverseProxy(target))
	defer proxy.Close()
	ready, stop := startTestDesktop(t, t.TempDir(), "--phone-origin", proxy.URL, "--phone-proxy-port", port)
	defer stop()
	local, remote := &http.Client{Timeout: 5 * time.Second}, proxy.Client()
	local.Jar, err = cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	remote.Jar, err = cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	remote.Timeout = 5 * time.Second
	call := func(client *http.Client, requestURL, origin, method, path, key, csrf string, data map[string]string, status int) (*http.Response, map[string]string) {
		t.Helper()
		raw, err := json.Marshal(data)
		if err != nil {
			t.Fatal(err)
		}
		req, err := http.NewRequest(method, requestURL+"/api/v1/"+path, bytes.NewReader(raw))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Origin", origin)
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Idempotency-Key", "desktop-phone-test-"+key)
		req.Header.Set("X-CSRF-Token", csrf)
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, err := io.ReadAll(resp.Body)
		if err != nil || resp.StatusCode != status {
			t.Fatalf("%s %s: %d %s (%v), want %d", method, path, resp.StatusCode, body, err, status)
		}
		fields := make(map[string]string)
		if path == "session" || path == "pairings" || path == "tasks" {
			var decoded map[string]any
			if err := json.Unmarshal(body, &decoded); err != nil {
				t.Fatal(err)
			}
			for name, value := range decoded {
				if value, ok := value.(string); ok {
					fields[name] = value
				}
			}
		}
		return resp, fields
	}
	owner := map[string]string{"ticket": ready["ticket"], "deviceName": "Mac"}
	call(remote, proxy.URL, proxy.URL, "POST", "owner/claim", "claim-phone", "", owner, 404)
	localClaim, _ := call(local, ready["origin"], ready["origin"], "POST", "owner/claim", "claim-mac", "", owner, 201)
	if cookies := localClaim.Cookies(); len(cookies) != 1 || cookies[0].Secure || !cookies[0].HttpOnly {
		t.Fatalf("local claim cookie flags are invalid: %+v", cookies)
	}
	_, mac := call(local, ready["origin"], "", "GET", "session", "", "", nil, 200)
	if mac["pairingOrigin"] != proxy.URL {
		t.Fatalf("pairing link should use %s, got %q", proxy.URL, mac["pairingOrigin"])
	}
	_, pair := call(local, ready["origin"], ready["origin"], "POST", "pairings", "pair", mac["csrfToken"], map[string]string{}, 201)
	phone := map[string]string{"token": pair["token"], "deviceName": "Phone"}
	call(remote, proxy.URL, ready["origin"], "POST", "pairings/claim", "wrong-origin", "", phone, 403)
	phoneClaim, _ := call(remote, proxy.URL, proxy.URL, "POST", "pairings/claim", "pair-phone", "", phone, 201)
	if cookies := phoneClaim.Cookies(); len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteStrictMode {
		t.Fatalf("HTTPS claim cookie flags are invalid: %+v", cookies)
	}
	call(remote, proxy.URL, proxy.URL, "POST", "pairings/claim", "repeat-pair", "", phone, 403)
	_, mobile := call(remote, proxy.URL, "", "GET", "session", "", "", nil, 200)
	if mobile["instanceId"] == "" || mac["instanceId"] != mobile["instanceId"] || mac["deviceId"] == mobile["deviceId"] {
		t.Fatalf("expected distinct devices on the same instance: %v %v", mac["deviceId"], mobile["deviceId"])
	}
	_, task := call(local, ready["origin"], ready["origin"], "POST", "tasks", "desktop-draft", mac["csrfToken"], map[string]string{"title": "跨端草稿"}, 201)
	call(remote, proxy.URL, "", "GET", "tasks/"+task["taskId"], "", "", nil, 200)
	call(local, ready["origin"], ready["origin"], "POST", "devices/"+mobile["deviceId"]+"/revoke", "revoke-phone", mac["csrfToken"], map[string]string{}, 200)
	call(remote, proxy.URL, "", "GET", "session", "", "", nil, 401)

	wrongHost, err := http.NewRequest("GET", ready["origin"]+"/healthz", nil)
	if err != nil {
		t.Fatal(err)
	}
	wrongHost.Host = "unexpected.example"
	resp, err := local.Do(wrongHost)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Fatalf("unknown Host was served: %d", resp.StatusCode)
	}
}
