package control

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type browser struct {
	client             *http.Client
	base, csrf, device string
}

func newBrowser(t *testing.T, base string) *browser {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &browser{client: &http.Client{Jar: jar, Timeout: 5 * time.Second}, base: base}
}
func (b *browser) request(method, path, key string, payload any) (int, []byte, error) {
	req, err := http.NewRequest(method, b.base+"/api/v1/"+path, bytes.NewReader(encode(payload)))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Origin", "http://nextbot.test")
	req.Header.Set("Idempotency-Key", key)
	req.Header.Set("X-CSRF-Token", b.csrf)
	resp, err := b.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	return resp.StatusCode, body, err
}
func (b *browser) call(t *testing.T, method, path, key string, body any, status int) []byte {
	t.Helper()
	got, data, err := b.request(method, path, key, body)
	if err != nil || got != status {
		t.Fatalf("%s %s = %d %s, want %d (%v)", method, path, got, data, status, err)
	}
	return data
}
func (b *browser) session(t *testing.T) {
	t.Helper()
	s := parse[map[string]string](t, b.call(t, "GET", "session", "", nil, 200))
	b.csrf = s["csrfToken"]
	b.device = s["deviceId"]
}
func parse[T any](t *testing.T, b []byte) T {
	t.Helper()
	v, err := decode[T](b)
	if err != nil {
		t.Fatal(err)
	}
	return v
}
func transact(t *testing.T, s *Store, fn func(*transaction) error) {
	t.Helper()
	if err := s.Tx(fn); err != nil {
		t.Fatal(err)
	}
}
func step(t *testing.T, s *Server) { t.Helper(); transact(t, s.Store, s.maintain) }
func taskGet(t *testing.T, b *browser, id string) Task {
	t.Helper()
	return parse[Task](t, b.call(t, "GET", "tasks/"+id, "", nil, 200))
}
func taskCreate(t *testing.T, b *browser, p Project, r Resolution, review bool) Task {
	t.Helper()
	fields := input{"projectId": p.ID, "title": "Synthetic test", "goal": "Fix export", "scope": "export.sh", "acceptanceCriteria": "Maintain headers", "executionAgent": "codex", "testId": "test", "requireTest": true}
	if review {
		fields["reviewAgent"] = "claude"
	}
	task := parse[Task](t, b.call(t, "POST", "tasks", ID(), fields, 201))
	path := "tasks/" + task.ID + "/submit"
	fields["resolutionId"], fields["baseCommit"], fields["expectedRevision"] = r.ID, r.Commit, task.Revision
	key := ID()
	first := b.call(t, "POST", path, key, fields, 200)
	again := b.call(t, "POST", path, key, fields, 200)
	if !bytes.Equal(first, again) {
		t.Fatal("submit replay changed its response")
	}
	return parse[Task](t, first)
}
func runReport(t *testing.T, s *Server, c Command, kind, phase string, payload map[string]any) {
	t.Helper()
	r := Report{ID: ID(), TaskID: c.TaskID, AttemptID: c.AttemptID, CommandID: c.ID, LeaseID: c.LeaseID, Fence: c.Fence, Type: kind, Phase: phase, Payload: payload}
	route := "reports"
	if phase != "" {
		route = "receipts"
	}
	transact(t, s.Store, func(tx *transaction) error {
		i, err := decode[input](encode(r))
		if err != nil {
			return err
		}
		first, err := s.report(tx, i, route)
		if err != nil {
			return err
		}
		second, err := s.report(tx, i, route)
		if err != nil {
			return err
		}
		if !bytes.Equal(encode(first.data), encode(second.data)) {
			t.Fatal("report replay changed result")
		}
		return nil
	})
}
func claim(t *testing.T, s *Server, n Node, kind string) Command {
	t.Helper()
	var cs []Command
	transact(t, s.Store, func(tx *transaction) error {
		r, err := s.claimCommands(tx, n)
		if err != nil {
			return err
		}
		cs, err = decode[[]Command](encode(r.data.(map[string]any)["items"]))
		return err
	})
	for _, c := range cs {
		if c.Type == kind {
			return c
		}
	}
	t.Fatalf("no %s command among %v", kind, cs)
	return Command{}
}

func TestOwnerWorkflow(t *testing.T) {
	root := t.TempDir()
	s, err := Open(filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	server := &Server{Store: s, Origin: "http://nextbot.test", AllowHTTP: true, History: 1000}
	public := httptest.NewServer(server.Public(http.NotFoundHandler()))
	defer public.Close()
	desktop, mobile := newBrowser(t, public.URL), newBrowser(t, public.URL)
	ticket, err := s.OwnerTicket(false)
	if err != nil {
		t.Fatal(err)
	}
	desktop.call(t, "POST", "owner/claim", ID(), input{"ticket": ticket, "deviceName": "Desktop"}, 201)
	mobile.call(t, "POST", "owner/claim", ID(), input{"ticket": ticket, "deviceName": "Foreign"}, 403)
	desktop.session(t)
	pair := parse[input](t, desktop.call(t, "POST", "pairings", ID(), input{}, 201))
	mobile.call(t, "POST", "pairings/claim", ID(), input{"token": pair["token"], "deviceName": "Phone"}, 201)
	newBrowser(t, public.URL).call(t, "POST", "pairings/claim", ID(), input{"token": pair["token"], "deviceName": "Additional"}, 403)
	mobile.session(t)
	p, err := s.AddProject("Fixture", "/fixture", []TestDefinition{{ID: "test", Label: "Regression", Args: []string{"/bin/test"}, Timeout: 10}})
	if err != nil {
		t.Fatal(err)
	}
	n := Node{ID: "local", LastSeen: now(), Availability: "online", Capabilities: map[string]Capabilities{"codex": {Authenticated: true, Execution: true, Tests: true}, "claude": {Authenticated: true, Review: true}}}
	transact(t, s, func(tx *transaction) error { return saveNode(tx, n) })
	v := parse[Resolution](t, desktop.call(t, "POST", "projects/"+p.ID+"/base-resolutions", ID(), input{"refName": "HEAD"}, 202))
	rc := claim(t, server, n, "resolve_base")
	base := "0123456789abcdef0123456789abcdef01234567"
	runReport(t, server, rc, "base_resolved", "", map[string]any{"commitId": base})
	v = parse[Resolution](t, desktop.call(t, "GET", "projects/"+p.ID+"/base-resolutions/"+v.ID, "", nil, 200))
	task := taskCreate(t, desktop, p, v, true)
	step(t, server)
	start := claim(t, server, n, "start_execution")
	runReport(t, server, start, "", "received", nil)
	runReport(t, server, start, "", "started", map[string]any{"workspaceId": start.AttemptID})
	next := taskCreate(t, desktop, p, v, false)
	step(t, server)
	next = taskGet(t, desktop, next.ID)
	if next.State != "queued" || next.AttemptID != nil {
		t.Fatalf("second write escaped the queue: %+v", next)
	}
	desktop.call(t, "POST", "tasks/"+next.ID+"/cancel", ID(), input{"expectedRevision": next.Revision}, 200)
	decision := func(kind string, frozen input) (string, input) {
		id := ID()
		frozen["inputDigest"] = Digest("input", frozen)
		runReport(t, server, start, kind, "", map[string]any{"requestId": id, "frozenInput": frozen})
		current := taskGet(t, mobile, task.ID)
		return id, input{"taskId": task.ID, "attemptId": start.AttemptID, "expectedRevision": current.Revision, "inputDigest": frozen["inputDigest"]}
	}
	for range 30 {
		id, command := decision("question_requested", input{"question": "Header only?"})
		command["outcome"], command["answerText"] = "answer", "synthetic"
		path := "requests/" + id + "/decide"
		a, b := race(t, desktop, mobile, path, command, command)
		if a+b != 200+409 || a == b {
			t.Fatalf("question decisions returned %d and %d", a, b)
		}
		path = "requests/" + id
		body := parse[Request](t, mobile.call(t, "GET", path, "", nil, 200))
		if body.Decision == nil || body.Decision.DeviceID == "" {
			t.Fatal("decision lost original actor")
		}
		answer := claim(t, server, n, "deliver_answer")
		runReport(t, server, answer, "", "finished", nil)
	}
	for range 30 {
		freeze := input{"testId": "test", "snapshotHash": Hash([]byte("race contents")), "attemptId": start.AttemptID, "definitionDigest": Digest("test_definition", p.Tests[0])}
		id, allow := decision("approval_requested", freeze)
		deny := input{}
		for k, v := range allow {
			deny[k] = v
		}
		allow["outcome"], deny["outcome"] = "allow_once", "deny"
		path := "requests/" + id
		a, b := race(t, desktop, mobile, path+"/decide", allow, deny)
		if a+b != 200+409 || a == b {
			t.Fatalf("approval decisions returned %d and %d", a, b)
		}
		req := parse[Request](t, mobile.call(t, "GET", path, "", nil, 200))
		command := "resolve_tool"
		if req.Decision.Outcome == "allow_once" {
			command = "run_registered_test"
		}
		selected := claim(t, server, n, command)
		if selected.Payload["requestId"] != id {
			t.Fatal("incorrectly dispatched a second command for an already decided request")
		}
		runReport(t, server, selected, "", "finished", nil)
	}
	expiredID, expired := decision("approval_requested", input{"testId": "test", "snapshotHash": Hash([]byte("timeout contents")), "attemptId": start.AttemptID, "definitionDigest": Digest("test_definition", p.Tests[0])})
	transact(t, s, func(tx *transaction) error {
		req, err := get[Request](tx, "request", expiredID)
		if err != nil {
			return err
		}
		req.ExpiresAt = now().Add(-time.Second)
		return put(tx, "request", req.ID, req.TaskID, req)
	})
	step(t, server)
	expired["outcome"] = "allow_once"
	desktop.call(t, "POST", "requests/"+expiredID+"/decide", ID(), expired, 409)
	expiry := claim(t, server, n, "resolve_tool")
	runReport(t, server, expiry, "", "finished", nil)
	snap := parse[snapshot](t, mobile.call(t, "GET", "snapshot", "", nil, 200))
	frozen := input{"testId": "test", "snapshotHash": Hash([]byte("fixture contents")), "attemptId": start.AttemptID, "definitionDigest": Digest("test_definition", p.Tests[0])}
	id, body := decision("approval_requested", frozen)
	body["outcome"] = "allow_once"
	path := "requests/" + id + "/decide"
	key := ID()
	allow := mobile.call(t, "POST", path, key, body, 200)
	if got := desktop.call(t, "POST", path, key, body, 200); !bytes.Equal(allow, got) {
		t.Fatal("cross-device replay changed approval")
	}
	deny := input{}
	for k, v := range body {
		deny[k] = v
	}
	deny["outcome"] = "deny"
	desktop.call(t, "POST", path, ID(), deny, 409)
	action := claim(t, server, n, "run_registered_test")
	runReport(t, server, action, "", "received", nil)
	runReport(t, server, action, "", "started", nil)
	runReport(t, server, action, "", "finished", nil)
	bodies := map[string][]byte{
		"change.patch":        []byte("synthetic diff"),
		"input-manifest.json": []byte(frozen.str("snapshotHash")),
		"run.json":            encode(input{"requestId": id, "testId": "test", "exitCode": 0, "inputDigest": frozen.str("inputDigest"), "snapshotHash": frozen.str("snapshotHash")}),
	}
	var entries []Entry
	for _, name := range []string{"change.patch", "input-manifest.json", "run.json"} {
		b := bodies[name]
		e := Entry{ID: name, Size: int64(len(b)), SHA: Hash(b), MediaType: "text/plain; charset=utf-8", Kind: "diff"}
		if name == "run.json" {
			e.MediaType = "application/json"
			e.Kind = "observed_run"
		}
		if err = writeObject(s.object(""), e, bytes.NewReader(b)); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, e)
	}
	var art Artifact
	transact(t, s, func(tx *transaction) error {
		u, err := server.initUpload(tx, uploadInput{Attempt: start.AttemptID, Key: start.ID, Base: base, Entries: entries})
		if err != nil {
			return err
		}
		obj := u.data.(map[string]any)
		data, err := server.finishUpload(tx, obj["uploadId"].(string), input{"attemptId": start.AttemptID, "manifestDigest": obj["manifestDigest"]})
		if err == nil {
			art = data.data.(Artifact)
		}
		return err
	})
	if !art.Evidence {
		t.Fatal("current version did not recognize the observed run")
	}
	runReport(t, server, start, "execution_finished", "", map[string]any{"stopped": true, "outcome": "succeeded", "artifactVersionId": art.ID})
	task = taskGet(t, desktop, task.ID)
	accept := input{"expectedRevision": task.Revision, "artifactVersionId": art.ID, "outcome": "accept"}
	acceptPath := "tasks/" + task.ID + "/acceptance"
	desktop.call(t, "POST", acceptPath, ID(), accept, 409)
	step(t, server)
	review := claim(t, server, n, "start_review")
	runReport(t, server, review, "", "started", map[string]any{"workspaceId": art.ID})
	runReport(t, server, review, "review_finished", "", map[string]any{"artifactVersionId": art.ID, "outcome": "pass", "reason": "Synthetic fixture satisfies requirements", "evidenceRefs": []string{"change.patch", "run.json"}})
	task = taskGet(t, mobile, task.ID)
	accept["expectedRevision"] = task.Revision
	a, b := race(t, desktop, mobile, acceptPath, accept, accept)
	if a+b != 200+409 || a == b {
		t.Fatalf("acceptance decisions returned %d and %d", a, b)
	}
	task = taskGet(t, mobile, task.ID)
	if task.State != "completed" {
		t.Fatalf("expected completed, got %s", task.State)
	}
	buf := mobile.call(t, "GET", "artifacts/"+art.ID+"/entries/change.patch", "", nil, 200)
	if !bytes.Equal(buf, bodies["change.patch"]) {
		t.Fatal("archive bytes changed")
	}
	step(t, server)
	if taskGet(t, mobile, next.ID).State != "cancelled" {
		t.Fatal("cancelled task resumed")
	}
	transact(t, s, func(tx *transaction) error { return setMeta(tx, "floor", meta(tx, "seq")) })
	mobile.call(t, "GET", "events?streamId="+snap.Stream+"&afterSeq="+snap.HighWater, "", nil, 410)
	backup := filepath.Join(root, "backup")
	manifest, err := s.Backup(backup)
	if err != nil {
		t.Fatal(err)
	}
	restored := filepath.Join(root, "restored")
	if _, err = Restore(backup, restored, manifest.Instance); err != nil {
		t.Fatal(err)
	}
	copyDB, err := Open(restored)
	if err != nil {
		t.Fatal(err)
	}
	defer copyDB.DB.Close()
	transact(t, copyDB, func(tx *transaction) error {
		if meta(tx, "stream") == snap.Stream {
			t.Fatal("restore reused old stream")
		}
		dev, err := get[Device](tx, "device", mobile.device)
		if err != nil {
			return err
		}
		if dev.Revoked == nil {
			t.Fatal("restore retained device access")
		}
		archived, err := get[Artifact](tx, "artifact", art.ID)
		if err != nil {
			return err
		}
		return copyDB.verify(archived)
	})
	desktop.call(t, "POST", "devices/"+mobile.device+"/revoke", ID(), input{}, 200)
	mobile.call(t, "GET", "tasks/"+task.ID, "", nil, 401)
	diffEntry, _ := findEntry(art.Entries, "change.patch")
	if err = os.Remove(s.object(diffEntry.SHA)); err != nil {
		t.Fatal(err)
	}
	desktop.call(t, "GET", "artifacts/"+art.ID, "", nil, 409)
	if taskGet(t, desktop, task.ID).State != "completed" {
		t.Fatal("damaged archive rewrote completed state")
	}
}
func race(t *testing.T, a, b *browser, path string, ab, bb input) (int, int) {
	t.Helper()
	statuses := make([]int, 2)
	var wg sync.WaitGroup
	for i, item := range []struct {
		c    *browser
		body input
	}{{a, ab}, {b, bb}} {
		wg.Go(func() {
			st, _, err := item.c.request("POST", path, ID(), item.body)
			if err != nil {
				t.Error(err)
			}
			statuses[i] = st
		})
	}
	wg.Wait()
	return statuses[0], statuses[1]
}
func TestArchiveRejectsCorruptBytes(t *testing.T) {
	e := Entry{SHA: Hash([]byte("good")), Size: 4}
	err := writeObject(t.TempDir(), e, bytes.NewBufferString("fake"))
	var a *apiError
	if !errors.As(err, &a) || a.Code != "digest_mismatch" {
		t.Fatalf("expected digest mismatch, got %v", err)
	}
}
