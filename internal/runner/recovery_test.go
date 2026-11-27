package runner

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"testing"

	"nextbot/internal/control"
)

func TestRestartRecoversTestEvidence(t *testing.T) {
	for _, haveArchive := range []bool{false, true} {
		name := "test finished while execution unresolved"
		if haveArchive {
			name = "final artifact published before completion response"
		}
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			j, err := openJournal(dir)
			if err != nil {
				t.Fatal(err)
			}
			base := strings.Repeat("a", 40)
			start := control.Command{ID: control.ID(), TaskID: control.ID(), AttemptID: control.ID(), Type: "start_execution",
				Payload: map[string]any{"task": control.Task{Base: base}}}
			test := control.Command{ID: control.ID(), TaskID: start.TaskID, AttemptID: start.AttemptID, Type: "run_registered_test", Phase: "started"}
			if err := j.record(start, "intent"); err != nil {
				t.Fatal(err)
			}
			if err := j.record(test, "intent"); err != nil {
				t.Fatal(err)
			}
			result := archiveEntry{"test.json", "observed_run", jsonMedia, []byte(`{"exitCode":0,"inputDigest":"original-digest"}`)}
			output := archiveEntry{"test.txt", "execution_log", textMedia, []byte("original test output\n")}
			finishTest := makeReport(test, "", "finished", "test exited", map[string]any{"exitCode": 0})
			if err := j.finishTest(test, finishTest, []archiveEntry{result, output}); err != nil {
				t.Fatal(err)
			}

			uploaded := make(map[string][]byte)
			var manifest []control.Entry
			var finalized int
			reportBodies := make(map[string][]byte)
			var applied []control.Report
			var lostResponse context.CancelFunc
			c := &client{http: &http.Client{Transport: transport(func(req *http.Request) (*http.Response, error) {
				body, err := io.ReadAll(req.Body)
				if err != nil {
					t.Fatal(err)
				}
				response := "{}"
				switch route := strings.TrimPrefix(req.URL.Path, "/internal/v1/nodes/local/"); {
				case route == "artifact-uploads":
					var upload struct {
						Attempt  string `json:"attemptId"`
						Key      string `json:"uploadKey"`
						Base     string `json:"baseCommit"`
						Manifest []control.Entry
					}
					if err := json.Unmarshal(body, &upload); err != nil {
						t.Fatal(err)
					}
					if upload.Attempt != start.AttemptID || upload.Key != start.ID || upload.Base != base {
						t.Fatalf("archive is not bound to the original execution: %+v", upload)
					}
					if manifest != nil && !reflect.DeepEqual(manifest, upload.Manifest) {
						t.Fatal("retry changed the original archive manifest")
					}
					manifest = upload.Manifest
					missing := []string{}
					for _, entry := range manifest {
						if _, found := uploaded[entry.ID]; !found {
							missing = append(missing, entry.ID)
						}
					}
					names, err := json.Marshal(missing)
					if err != nil {
						t.Fatal(err)
					}
					response = `{"uploadId":"upload","manifestDigest":"digest","missingEntryIds":` + string(names) + `}`
				case strings.HasPrefix(route, "artifact-uploads/upload/entries/"):
					uploaded[strings.TrimPrefix(route, "artifact-uploads/upload/entries/")] = body
				case route == "artifact-uploads/upload/finalize":
					finalized++
					response = `{"artifactVersionId":"original-artifact"}`
				case route == "reports" || route == "receipts":
					var report control.Report
					if err := json.Unmarshal(body, &report); err != nil {
						t.Fatal(err)
					}
					if prior, handled := reportBodies[report.ID]; handled {
						if !bytes.Equal(prior, body) {
							t.Fatal("replayed report changed payload")
						}
					} else {
						reportBodies[report.ID] = body
						applied = append(applied, report)
					}
					if lostResponse != nil && report.Type == "execution_finished" {
						lostResponse()
						return nil, context.Canceled
					}
				default:
					t.Fatalf("unexpected Runner request %s", route)
				}
				return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(response))}, nil
			})}}

			if haveArchive {
				checkpoint := executionArchive{Base: base, Outcome: "succeeded", ReportID: control.ID(),
					Entries: executionEntries([]byte("original patch"), "original fingerprint", "original summary", []archiveEntry{result, output})}
				if err := j.saveArchive(start.ID, checkpoint); err != nil {
					t.Fatal(err)
				}
				ctx, cancel := context.WithCancel(context.Background())
				lostResponse = cancel
				r := &Runner{j: j, c: c}
				if err := r.publishExecution(ctx, start, checkpoint); err != context.Canceled {
					t.Fatalf("wanted interrupted completion response, got %v", err)
				}
				lostResponse = nil
			}
			if err := j.db.Close(); err != nil {
				t.Fatal(err)
			}
			j, err = openJournal(dir)
			if err != nil {
				t.Fatal(err)
			}
			defer j.db.Close()
			r := &Runner{j: j, c: c}
			ctx := context.Background()
			for range 2 {
				if err := r.reconcile(ctx); err != nil {
					t.Fatal(err)
				}
				if err := r.handle(ctx, test); err != nil {
					t.Fatal(err)
				}
			}
			if !bytes.Equal(uploaded[result.ID], result.Bytes) || !bytes.Equal(uploaded[output.ID], output.Bytes) {
				t.Fatalf("recovered archive lost original test evidence: %v", uploaded)
			}
			if got := slices.DeleteFunc(slices.Clone(applied), func(p control.Report) bool { return p.CommandID != test.ID }); len(got) != 1 || got[0].ID != finishTest.ID {
				t.Fatalf("test was resubmitted or its original result was lost: %+v", got)
			}
			if haveArchive {
				if finalized != 2 || string(uploaded["change.patch"]) != "original patch" ||
					!slices.ContainsFunc(applied, func(p control.Report) bool {
						return p.Type == "execution_finished" && p.Payload["artifactVersionId"] == "original-artifact"
					}) {
					t.Fatalf("checkpoint was not replayed with the original artifact: finalized %d, reports %+v", finalized, applied)
				}
			} else if finalized != 1 || uploaded["input-manifest.json"] != nil || uploaded["recovery.txt"] == nil ||
				!slices.ContainsFunc(applied, func(p control.Report) bool { return p.CommandID == start.ID && p.Phase == "unknown" }) {
				t.Fatalf("partial archive did not preserve the unresolved execution: finalized %d, reports %+v", finalized, applied)
			}
		})
	}
}
