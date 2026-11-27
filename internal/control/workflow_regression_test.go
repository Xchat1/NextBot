package control

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestExpiredApprovalReleasesWaitingRunner(t *testing.T) {
	for _, phase := range []string{"pending", "leased", "received", "started"} {
		t.Run(phase, func(t *testing.T) {
			s, err := Open(filepath.Join(t.TempDir(), "control"))
			if err != nil {
				t.Fatal(err)
			}
			defer s.DB.Close()
			server := &Server{Store: s}
			node := Node{ID: "local", Availability: "online", LastSeen: now()}
			attempt := Attempt{ID: ID(), NodeID: node.ID, Fence: "1", Status: "running"}
			task := Task{ID: ID(), State: "running", Revision: "1", AttemptID: &attempt.ID}
			attempt.TaskID = task.ID
			request := Request{ID: ID(), Kind: "action_approval", TaskID: task.ID, AttemptID: attempt.ID,
				Status: "pending", ExpiresAt: now().Add(-time.Minute)}
			decision := Decision{ID: ID(), Outcome: "allow_once", At: now().Add(-2 * time.Minute), DeviceID: "phone"}
			transact(t, s, func(tx *transaction) error {
				if err := saveNode(tx, node); err != nil {
					return err
				}
				if err := put(tx, "attempt", attempt.ID, task.ID, attempt); err != nil {
					return err
				}
				if err := settleRequest(tx, &task, &request, decision, "decided"); err != nil {
					return err
				}
				command, err := get[Command](tx, "command", request.CommandID)
				if err != nil {
					return err
				}
				command.Phase = phase
				if phase != "pending" {
					command.LeaseID = ID()
					command.LeaseAt = now().Add(time.Minute)
					if err := put(tx, "lease", command.LeaseID, command.ID, map[string]string{"commandId": command.ID}); err != nil {
						return err
					}
				}
				return put(tx, "command", command.ID, task.ID, command)
			})

			lateReceipt := func(code string) {
				t.Helper()
				transact(t, s, func(tx *transaction) error {
					command, err := get[Command](tx, "command", request.CommandID)
					if err != nil {
						return err
					}
					report := Report{ID: ID(), TaskID: task.ID, AttemptID: attempt.ID,
						CommandID: command.ID, LeaseID: command.LeaseID, Fence: command.Fence, Phase: "received"}
					var rejected *apiError
					args, err := decode[input](encode(report))
					if err != nil {
						return err
					}
					_, err = server.report(tx, args, "receipts")
					if !errors.As(err, &rejected) || rejected.Code != code {
						t.Fatalf("late receipt error = %v, want %s", err, code)
					}
					return nil
				})
			}
			if phase == "leased" {
				lateReceipt("request_expired")
			}
			step(t, server)
			step(t, server)
			if phase == "leased" {
				lateReceipt("request_closed")
			}
			var notices []Command
			transact(t, s, func(tx *transaction) error {
				command, err := get[Command](tx, "command", request.CommandID)
				if err != nil {
					return err
				}
				commands, err := list[Command](tx, "command", task.ID)
				if err != nil {
					return err
				}
				for _, candidate := range commands {
					if candidate.Type == "resolve_tool" {
						notices = append(notices, candidate)
					}
				}
				if phase == "received" || phase == "started" {
					if len(notices) != 0 || command.Phase != phase {
						t.Fatalf("admitted command was replaced: %s, notices %d", command.Phase, len(notices))
					}
					return nil
				}
				if command.Phase != "rejected" || len(notices) != 1 {
					t.Fatalf("want rejected test and one release notice, got %s and %d notices", command.Phase, len(notices))
				}
				return nil
			})
			if len(notices) == 0 {
				return
			}
			notice := claim(t, server, node, "resolve_tool")
			if notice.AttemptID != attempt.ID || notice.Payload["requestId"] != request.ID || notice.Payload["outcome"] != "expired" {
				t.Fatalf("incorrect expiration notice: %+v", notice)
			}
			runReport(t, server, notice, "", "received", nil)
			runReport(t, server, notice, "", "finished", nil)
			transact(t, s, func(tx *transaction) error {
				settled, err := get[Request](tx, "request", request.ID)
				if err != nil {
					return err
				}
				if settled.Decision == nil || settled.Decision.ID != decision.ID || settled.Result != "expired" {
					t.Fatalf("approval or expired result was overwritten: %+v", settled)
				}
				return nil
			})
		})
	}
}

func TestQueuedTaskExplainsCapabilityLossAndResumes(t *testing.T) {
	s, err := Open(filepath.Join(t.TempDir(), "control"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.DB.Close()
	server := &Server{Store: s}
	project, err := s.AddProject("Fixture", "/fixture", []TestDefinition{{ID: "test"}})
	if err != nil {
		t.Fatal(err)
	}
	task := Task{ID: ID(), ProjectID: project.ID, Agent: "codex", TestID: "test", State: "queued", Revision: "1"}
	node := Node{ID: "local", Availability: "online", LastSeen: now()}
	transact(t, s, func(tx *transaction) error { return put(tx, "task", task.ID, project.ID, task) })
	for _, item := range []struct {
		capability Capabilities
		message    string
	}{
		{Capabilities{Installed: true, Execution: true}, "登录"},
		{Capabilities{Installed: true, Authenticated: true, Reason: "受限协议握手失败"}, "握手失败"},
		{Capabilities{Installed: true, Authenticated: true, Execution: true}, "受管测试"},
		{Capabilities{Installed: true, Authenticated: true, Execution: true, Tests: true}, ""},
	} {
		node.Capabilities = map[string]Capabilities{"codex": item.capability}
		transact(t, s, func(tx *transaction) error { return saveNode(tx, node) })
		step(t, server)
		step(t, server)
		transact(t, s, func(tx *transaction) error {
			got, err := get[Task](tx, "task", task.ID)
			if err != nil {
				return err
			}
			attempts, err := list[Attempt](tx, "attempt", task.ID)
			if err != nil {
				return err
			}
			if item.message == "" {
				if len(got.Blockers) != 0 || got.AttemptID == nil || len(attempts) != 1 {
					t.Fatalf("task was not dispatched once after recovery: %+v, attempts %d", got, len(attempts))
				}
			} else if got.State != "queued" || len(attempts) != 0 || len(got.Blockers) != 1 ||
				got.Blockers[0].Kind != "capability_unavailable" || !strings.Contains(got.Blockers[0].Message, item.message) {
				t.Fatalf("want queued task with a single blocker containing %q, got %+v", item.message, got)
			}
			return nil
		})
	}
}
