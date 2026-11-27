package control

import (
	"errors"
	"net/http"
	"sort"
	"time"
)

const nodePrefix = "/internal/v1/nodes/local"

func (s *Server) Internal() http.Handler {
	m := http.NewServeMux()
	for _, route := range []string{"hello", "heartbeat", "commands/claim", "receipts", "reports", "artifact-uploads", "artifact-uploads/{id}/finalize"} {
		m.HandleFunc("POST "+nodePrefix+"/"+route, s.nodePost(route))
	}
	m.HandleFunc("PUT "+nodePrefix+"/artifact-uploads/{id}/entries/{entry}", s.nodeUpload)
	m.HandleFunc("GET "+nodePrefix+"/artifacts/{id}/entries/{entry}", s.nodeRead)
	return m
}
func connection(tx *transaction, r *http.Request) (Node, error) {
	n, err := get[Node](tx, "node", "local")
	if err != nil && !errors.Is(err, missing) {
		return n, err
	}
	if n.Connection == "" || r.Header.Get("NextBot-Connection") != n.Connection || r.Header.Get("NextBot-Generation") != n.Generation {
		return n, conflict("stale_generation", "节点连接需要重新登记", nil)
	}
	return n, nil
}
func (s *Server) nodePost(route string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		body, err := readBody[input](w, r, 256<<10)
		if err != nil {
			writeError(w, err)
			return
		}
		var response result
		err = s.Tx(func(tx *transaction) error {
			if route == "hello" {
				if body.str("protocolVersion") != "1" || body.str("bootId") == "" {
					return invalid("节点协议不受支持")
				}
				caps, err := decode[map[string]Capabilities](encode(body["adapterCapabilities"]))
				if err != nil {
					return invalid("节点能力无效")
				}
				n := Node{ID: "local", LastSeen: now(), Availability: "online", Capabilities: caps, Connection: ID(), Generation: inc(meta(tx, "generation")), BootID: body.str("bootId")}
				if err = setMeta(tx, "generation", n.Generation); err != nil {
					return err
				}
				if err = saveNode(tx, n); err != nil {
					return err
				}
				as, err := list[Attempt](tx, "attempt", "")
				if err != nil {
					return err
				}
				active := []string{}
				for _, a := range as {
					if a.EndedAt == nil {
						active = append(active, a.ID)
					}
				}
				response = ok(map[string]any{"connectionId": n.Connection, "generation": n.Generation, "heartbeatIntervalMs": 5000, "leaseDurationMs": 30000, "reconcileAttemptIds": active})
				return nil
			}
			n, err := connection(tx, r)
			if err != nil {
				return err
			}
			switch route {
			case "heartbeat":
				caps, err := decode[map[string]Capabilities](encode(body["adapterCapabilities"]))
				if err != nil {
					return invalid("节点能力无效")
				}
				changed := !nodeReady(n) || Digest("capabilities", n.Capabilities) != Digest("capabilities", caps)
				n.LastSeen = now()
				n.Availability = "online"
				n.Capabilities = caps
				response = ok(map[string]any{"serverTime": n.LastSeen})
				if changed {
					return saveNode(tx, n)
				}
				return put(tx, "node", n.ID, "", n)
			case "commands/claim":
				response, err = s.claimCommands(tx, n)
			case "receipts", "reports":
				response, err = s.report(tx, body, route)
			case "artifact-uploads":
				u, e := decode[uploadInput](encode(body))
				if e != nil {
					return invalid("归档清单无效")
				}
				response, err = s.initUpload(tx, u)
			case "artifact-uploads/{id}/finalize":
				response, err = s.finishUpload(tx, r.PathValue("id"), body)
			}
			return err
		})
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, response.status, response.data)
	}
}
func (s *Server) nodeUpload(w http.ResponseWriter, r *http.Request) {
	var entry Entry
	err := s.Tx(func(tx *transaction) error {
		if _, err := connection(tx, r); err != nil {
			return err
		}
		u, err := found[Upload](tx, "upload", r.PathValue("id"))
		if err != nil {
			return err
		}
		entry, err = findEntry(u.Entries, r.PathValue("entry"))
		return err
	})
	if err == nil {
		err = writeObject(s.object(""), entry, http.MaxBytesReader(w, r.Body, int64(maxEntry)+1))
	}
	if err != nil {
		writeError(w, err)
		return
	}
	w.WriteHeader(204)
}
func (s *Server) nodeRead(w http.ResponseWriter, r *http.Request) {
	var entry Entry
	var data []byte
	err := s.Tx(func(tx *transaction) error {
		if _, err := connection(tx, r); err != nil {
			return err
		}
		a, err := found[Artifact](tx, "artifact", r.PathValue("id"))
		if err != nil {
			return err
		}
		entry, err = findEntry(a.Entries, r.PathValue("entry"))
		if err != nil {
			return err
		}
		data, err = s.entry(entry)
		return err
	})
	if err != nil {
		writeError(w, err)
		return
	}
	serveEntry(w, entry, data)
}
func commandDone(phase string) bool {
	return phase == "finished" || phase == "rejected" || phase == "unknown"
}
func (s *Server) claimCommands(tx *transaction, n Node) (result, error) {
	cs, err := list[Command](tx, "command", "")
	if err != nil {
		return result{}, err
	}
	sort.SliceStable(cs, func(i, j int) bool {
		if (cs[i].Type == "cancel_attempt") != (cs[j].Type == "cancel_attempt") {
			return cs[i].Type == "cancel_attempt"
		}
		return cs[i].CreatedAt.Before(cs[j].CreatedAt)
	})
	out := []Command{}
	for _, c := range cs {
		if commandDone(c.Phase) || now().Before(c.LeaseAt) {
			continue
		}
		if c.AttemptID != "" {
			a, err := get[Attempt](tx, "attempt", c.AttemptID)
			if err != nil {
				return result{}, err
			}
			t, err := get[Task](tx, "task", c.TaskID)
			if err != nil {
				return result{}, err
			}
			if a.NodeID != n.ID || a.EndedAt != nil || value(t.AttemptID) != a.ID || t.CancelID != "" && t.CancelID != c.ID && c.Phase == "pending" {
				c.Phase = "rejected"
				if err = put(tx, "command", c.ID, c.TaskID, c); err != nil {
					return result{}, err
				}
				continue
			}
		}
		c.LeaseID = ID()
		c.LeaseAt = now().Add(30 * time.Second)
		if c.Phase == "pending" {
			c.Phase = "leased"
		}
		if err = put(tx, "command", c.ID, c.TaskID, c); err != nil {
			return result{}, err
		}
		if err = put(tx, "lease", c.LeaseID, c.ID, map[string]string{"commandId": c.ID, "attemptId": c.AttemptID, "fence": c.Fence}); err != nil {
			return result{}, err
		}
		out = append(out, c)
		if len(out) == 8 {
			break
		}
	}
	return ok(items(out)), nil
}
func (s *Server) report(tx *transaction, i input, route string) (result, error) {
	r, err := decode[Report](encode(i))
	if err != nil || len(r.ID) < 8 || len(r.ID) > 128 || r.CommandID == "" {
		return result{}, invalid("报告标识无效")
	}
	digest := Digest("report", r)
	data, status, hit, err := receipt(tx, "runner", r.ID, digest)
	if err != nil {
		return result{}, err
	}
	if hit {
		return result{status, data}, nil
	}
	c, err := found[Command](tx, "command", r.CommandID)
	if err != nil {
		return result{}, err
	}
	if r.AttemptID != c.AttemptID || r.TaskID != c.TaskID || r.Fence != c.Fence {
		return result{}, conflict("stale_target", "报告绑定目标不匹配", nil)
	}
	lease, err := found[map[string]string](tx, "lease", r.LeaseID)
	if err != nil {
		return result{}, err
	}
	if lease["commandId"] != c.ID {
		return result{}, conflict("stale_target", "租期不属于该命令", nil)
	}
	var a Attempt
	var t Task
	active := true
	if c.AttemptID != "" {
		a, err = found[Attempt](tx, "attempt", c.AttemptID)
		if err != nil {
			return result{}, err
		}
		t, err = found[Task](tx, "task", a.TaskID)
		if err != nil {
			return result{}, err
		}
		active = value(t.AttemptID) == a.ID && a.EndedAt == nil && !terminal(t.State)
	}
	var task *Task
	if t.ID != "" {
		task = &t
	}
	disposition := "applied"
	if !active {
		disposition = "historical"
		if err = emit(tx, "historical_report", task, a.ID, map[string]string{"reportId": r.ID, "type": r.Type, "phase": r.Phase}); err != nil {
			return result{}, err
		}
	} else if route == "receipts" {
		if err = s.applyReceipt(tx, &t, &a, &c, r); err != nil {
			return result{}, err
		}
	} else if err = s.applyReport(tx, &t, &a, &c, r); err != nil {
		return result{}, err
	}
	response := map[string]any{"reportId": r.ID, "disposition": disposition, "seq": meta(tx, "seq"), "revision": t.Revision}
	return ok(response), saveReceipt(tx, "runner", r.ID, digest, 200, response)
}
func (s *Server) applyReceipt(tx *transaction, t *Task, a *Attempt, c *Command, r Report) error {
	switch r.Phase {
	case "received", "started", "finished", "rejected", "unknown":
	default:
		return invalid("回执阶段无效")
	}
	if r.Phase == "received" {
		if c.LeaseID != r.LeaseID || !now().Before(c.LeaseAt) {
			return conflict("stale_target", "节点领取租期已失效", nil)
		}
		if t.CancelID != "" && t.CancelID != c.ID {
			return conflict("request_closed", "任务正在取消", nil)
		}
		if commandDone(c.Phase) {
			return conflict("request_closed", "命令已结束", nil)
		}
		if c.Type == "run_registered_test" || c.Type == "deliver_answer" {
			exp, err := time.Parse(time.RFC3339Nano, input(c.Payload).str("expiresAt"))
			if err != nil || !now().Before(exp) {
				return conflict("request_expired", "执行许可已过期", nil)
			}
		}
	}
	if commandDone(c.Phase) {
		return nil
	}
	if r.Phase == "received" && c.Phase == "started" {
		return nil
	}
	c.Phase = r.Phase
	c.Result = r.Result
	if err := put(tx, "command", c.ID, c.TaskID, *c); err != nil {
		return err
	}
	var task *Task
	if t.ID != "" {
		task = t
	}
	if err := emit(tx, "command_changed", task, a.ID, map[string]string{"commandId": c.ID, "type": c.Type, "phase": c.Phase}); err != nil {
		return err
	}
	if c.Type == "start_execution" || c.Type == "start_review" {
		if r.Phase == "started" && a.StartedAt == nil {
			at := now()
			a.StartedAt = &at
			a.Status = "running"
			a.Workspace = input(r.Payload).str("workspaceId")
			if a.Role == "execution" {
				t.State = "running"
			} else {
				t.State = "reviewing"
			}
			if err := put(tx, "attempt", a.ID, t.ID, *a); err != nil {
				return err
			}
			return change(tx, t)
		}
		if r.Phase == "rejected" {
			a.Result = r.Result
			if t.CancelID != "" {
				return nil
			}
			if a.Role == "review" {
				return finishReviewFailure(tx, t, a, "failed")
			}
			return finishExecution(tx, t, a, "failed")
		}
	}
	if r.Phase == "unknown" && a.ID != "" {
		return markUnknown(tx, t, a, "节点无法核实该命令的结果，等待人工核对")
	}
	if requestID := input(c.Payload).str("requestId"); requestID != "" {
		q, err := get[Request](tx, "request", requestID)
		if err != nil {
			return err
		}
		if q.CommandID != c.ID {
			return nil
		}
		q.Result = r.Phase
		if err = put(tx, "request", q.ID, q.TaskID, q); err != nil {
			return err
		}
	}
	return nil
}
func (s *Server) applyReport(tx *transaction, t *Task, a *Attempt, c *Command, r Report) error {
	i := input(r.Payload)
	switch r.Type {
	case "base_resolved":
		if c.Type != "resolve_base" {
			return invalid("报告不属于基准解析")
		}
		v, err := found[Resolution](tx, "resolution", input(c.Payload).str("resolutionId"))
		if err != nil {
			return err
		}
		if v.Status == "pending" {
			commit := i.str("commitId")
			if gitCommit.MatchString(commit) {
				v.Status = "resolved"
				v.Commit = commit
			} else {
				v.Status = "failed"
				v.Reason = i.str("reason")
			}
			if err = put(tx, "resolution", v.ID, v.ProjectID, v); err != nil {
				return err
			}
			if err = emit(tx, "base_resolved", nil, "", map[string]string{"resolutionId": v.ID, "status": v.Status}); err != nil {
				return err
			}
		}
	case "session_observed":
		if c.Type != "start_execution" || a.StartedAt == nil {
			return transition("运行尚未开始")
		}
		a.SessionID = i.str("sessionId")
		return put(tx, "attempt", a.ID, t.ID, *a)
	case "question_requested", "approval_requested":
		if c.Type != "start_execution" || t.State != "running" || t.CancelID != "" || a.Status != "running" {
			return transition("尝试不接受新待办")
		}
		return s.openRequest(tx, t, a, r)
	case "execution_finished":
		if c.Type != "start_execution" || !i.boolean("stopped") {
			return invalid("执行停止未经核实")
		}
		if t.CancelID != "" {
			return nil
		}
		status := i.str("outcome")
		switch status {
		case "succeeded", "failed", "interrupted":
		default:
			return invalid("执行结果无效")
		}
		if status == "succeeded" {
			art, err := found[Artifact](tx, "artifact", i.str("artifactVersionId"))
			if err != nil {
				return err
			}
			if art.AttemptID != a.ID || a.ArtifactID != art.ID {
				return conflict("stale_target", "归档不属于该尝试", nil)
			}
			if err = s.verify(art); err != nil {
				return err
			}
		}
		a.Result = i.str("reason")
		if err := finishExecution(tx, t, a, status); err != nil {
			return err
		}
	case "cancel_confirmed":
		if c.Type != "cancel_attempt" || t.CancelID != c.ID || !i.boolean("stopped") {
			return invalid("取消停止未经核实")
		}
		a.Result = "cancelled"
		if err := finishExecution(tx, t, a, "cancelled"); err != nil {
			return err
		}
	case "review_finished":
		if c.Type != "start_review" || a.Role != "review" || i.str("artifactVersionId") != a.ArtifactID || value(t.ArtifactID) != a.ArtifactID {
			return conflict("stale_target", "复核版本不匹配", nil)
		}
		if t.CancelID != "" {
			return nil
		}
		if err := s.finishReview(tx, t, a, i); err != nil {
			return err
		}
	default:
		return invalid("报告类型无效")
	}
	c.Phase = "finished"
	if err := put(tx, "command", c.ID, c.TaskID, *c); err != nil {
		return err
	}
	var task *Task
	if t.ID != "" {
		task = t
	}
	return emit(tx, "command_changed", task, a.ID, map[string]string{"commandId": c.ID, "type": c.Type, "phase": c.Phase})
}
func (s *Server) openRequest(tx *transaction, t *Task, a *Attempt, r Report) error {
	i := input(r.Payload)
	frozen := i.object("frozenInput")
	if frozen == nil || i.str("requestId") == "" {
		return invalid("待办输入无效")
	}
	digest := frozen.str("inputDigest")
	delete(frozen, "inputDigest")
	if digest != Digest("input", frozen) {
		return invalid("待办输入摘要不匹配")
	}
	frozen["inputDigest"] = digest
	q := Request{ID: i.str("requestId"), Kind: "user_question", TaskID: t.ID, AttemptID: a.ID, Status: "pending", Input: frozen, ExpiresAt: now().Add(5 * time.Minute)}
	if r.Type == "approval_requested" {
		p, err := get[Project](tx, "project", t.ProjectID)
		if err != nil {
			return err
		}
		test := registered(p, t.TestID)
		if test.ID == "" || frozen.str("testId") != test.ID || !shaPattern.MatchString(frozen.str("snapshotHash")) || frozen.str("attemptId") != a.ID || frozen.str("definitionDigest") != Digest("test_definition", test) {
			return invalid("受管测试与登记项不匹配")
		}
		q.Kind = "action_approval"
	} else if frozen.str("question") == "" {
		return invalid("问题不能为空")
	}
	if _, err := get[Request](tx, "request", q.ID); err == nil {
		return conflict("idempotency_conflict", "待办标识已使用", nil)
	} else if !errors.Is(err, missing) {
		return err
	}
	kind := "awaiting_input"
	if q.Kind == "action_approval" {
		kind = "awaiting_approval"
	}
	block(t, kind, q.ID, "")
	if err := put(tx, "request", q.ID, t.ID, q); err != nil {
		return err
	}
	if err := change(tx, t); err != nil {
		return err
	}
	return emit(tx, "request_opened", t, a.ID, map[string]any{"requestId": q.ID, "kind": q.Kind, "expiresAt": q.ExpiresAt, "inputDigest": digest})
}
func finishReviewFailure(tx *transaction, t *Task, a *Attempt, status string) error {
	at := now()
	a.EndedAt = &at
	a.Status = status
	t.State = "awaiting_review"
	block(t, "awaiting_input", "review_failure", "复核执行失败，可通过新尝试恢复")
	a.Status = "interrupted"
	a.Recovery = []string{"new_attempt"}
	if err := put(tx, "attempt", a.ID, t.ID, *a); err != nil {
		return err
	}
	return change(tx, t)
}
func (s *Server) finishReview(tx *transaction, t *Task, a *Attempt, i input) error {
	outcome := i.str("outcome")
	reason := i.str("reason")
	if outcome == "failed" {
		a.Result = reason
		return finishReviewFailure(tx, t, a, "failed")
	}
	if outcome != "pass" && outcome != "request_rework" || reason == "" {
		return invalid("复核结论无效")
	}
	art, err := get[Artifact](tx, "artifact", a.ArtifactID)
	if err != nil {
		return err
	}
	if err = s.verify(art); err != nil {
		return err
	}
	refs, err := decode[[]string](encode(i["evidenceRefs"]))
	if err != nil {
		return invalid("复核证据无效")
	}
	observed := false
	for _, ref := range refs {
		entry, err := findEntry(art.Entries, ref)
		if err != nil {
			return invalid("复核引用不属于当前版本")
		}
		if entry.Kind == "observed_run" {
			observed = true
		}
	}
	if outcome == "pass" && (len(refs) == 0 || t.RequireTest && (!art.Evidence || !observed)) {
		outcome = "request_rework"
		reason = "本版本缺少符合验收要求的证据"
	}
	art.Review = &Decision{ID: ID(), Outcome: outcome, At: now(), Reason: reason, AttemptID: a.ID, Evidence: refs}
	at := now()
	a.EndedAt = &at
	a.Status = "succeeded"
	a.Result = outcome
	if err = put(tx, "attempt", a.ID, t.ID, *a); err != nil {
		return err
	}
	if err = put(tx, "artifact", art.ID, t.ID, art); err != nil {
		return err
	}
	if outcome == "pass" {
		t.State = "awaiting_acceptance"
	} else {
		rework(t, reason)
	}
	if err = change(tx, t); err != nil {
		return err
	}
	return emit(tx, "review_recorded", t, a.ID, map[string]any{"artifactVersionId": art.ID, "reviewAttemptId": a.ID, "outcome": outcome, "evidenceRefs": refs})
}
