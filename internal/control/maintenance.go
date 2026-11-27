package control

import (
	"fmt"
	"strings"
)

func (s *Store) PauseDispatch(paused bool) error {
	return s.Tx(func(tx *transaction) error {
		value := "off"
		if paused {
			value = "on"
		}
		if value == meta(tx, "pause_dispatch") {
			return nil
		}
		if err := setMeta(tx, "pause_dispatch", value); err != nil {
			return err
		}
		return emit(tx, "dispatch_changed", nil, "", map[string]bool{"paused": paused})
	})
}

// ConfirmStopped records a host administrator's reconciliation after inspecting
// the original process group, test container and journal.
func (s *Store) ConfirmStopped(taskID, attemptID, observation string) error {
	if strings.TrimSpace(observation) == "" || len(observation) > 4000 {
		return fmt.Errorf("describe the verified process and action outcome")
	}
	return s.Tx(func(tx *transaction) error {
		t, err := found[Task](tx, "task", taskID)
		if err != nil {
			return err
		}
		if terminal(t.State) || t.State == "draft" || value(t.AttemptID) != attemptID {
			return fmt.Errorf("task no longer binds the specified active attempt")
		}
		restoring := false
		for _, b := range t.Blockers {
			if b.Source == "restore" {
				restoring = true
			}
		}
		if attemptID == "" {
			if !restoring || t.State != "queued" {
				return fmt.Errorf("task requires an attempt to reconcile")
			}
			unblock(&t, "restore")
			if err := change(tx, &t); err != nil {
				return err
			}
			return emit(tx, "reconciled_locally", &t, "", map[string]string{"outcome": "restored_queue"})
		}
		a, err := found[Attempt](tx, "attempt", attemptID)
		if err != nil {
			return err
		}
		if restoring && t.State == "awaiting_review" && a.Role == "execution" && a.EndedAt != nil {
			unblock(&t, "restore")
			if err := change(tx, &t); err != nil {
				return err
			}
			return emit(tx, "reconciled_locally", &t, a.ID, map[string]string{"outcome": "restored_review"})
		}
		if a.Status != "unknown" && !restoring {
			return fmt.Errorf("current attempt does not require manual reconciliation")
		}
		a.Result = observation
		cs, err := list[Command](tx, "command", t.ID)
		if err != nil {
			return err
		}
		for _, c := range cs {
			if c.AttemptID != a.ID || commandDone(c.Phase) {
				continue
			}
			c.Phase = "unknown"
			if err := put(tx, "command", c.ID, t.ID, c); err != nil {
				return err
			}
		}
		switch {
		case t.CancelID != "":
			err = finishExecution(tx, &t, &a, "cancelled")
		case a.Role == "review" || t.State == "awaiting_review":
			t.Blockers = []Blocker{}
			err = finishReviewFailure(tx, &t, &a, "interrupted")
		default:
			err = finishExecution(tx, &t, &a, "interrupted")
		}
		if err != nil {
			return err
		}
		return emit(tx, "reconciled_locally", &t, a.ID, map[string]string{"outcome": "verified_stopped"})
	})
}
