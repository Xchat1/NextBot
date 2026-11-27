package control

import (
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var gitCommit = regexp.MustCompile(`^[a-f0-9]{40}([a-f0-9]{24})?$`)

func revised(t Task, i input) error {
	if i.str("expectedRevision") != t.Revision {
		return conflict("revision_conflict", "任务已有更新", map[string]any{"revision": t.Revision})
	}
	return nil
}
func unavailable(message string) error { return fail(422, "capability_unavailable", message) }
func transition(message string) error  { return conflict("invalid_transition", message, nil) }
func nodeReady(n Node) bool {
	return n.Availability == "online" && now().Sub(n.LastSeen) <= 15*time.Second
}
func checkCombo(n Node, t Task) error {
	if !nodeReady(n) {
		return unavailable("执行节点尚未联机")
	}
	if reason := capabilityIssue(n, t); reason != "" {
		return unavailable(reason)
	}
	return nil
}
func capabilityIssue(n Node, t Task) string {
	c := n.Capabilities[t.Agent]
	if reason := adapterIssue(t.Agent, c, c.Execution, "受限文件编辑能力不可用"); reason != "" {
		return reason
	}
	if t.TestID != "" && !c.Tests {
		return "节点的受管测试执行器未启用或不可用，检查节点设置与容器执行器"
	}
	if t.Reviewer != "" {
		if t.Reviewer == t.Agent {
			return "独立复核需要与执行不同的 Agent"
		}
		reviewer := n.Capabilities[t.Reviewer]
		return adapterIssue(t.Reviewer, reviewer, reviewer.Review, "独立只读复核能力不可用")
	}
	return ""
}
func adapterIssue(agent string, capability Capabilities, supported bool, fallback string) string {
	switch {
	case capability.Authenticated && supported:
		return ""
	case capability.Reason != "":
		return agent + " · " + capability.Reason
	case !capability.Installed:
		return agent + " CLI 尚未安装或未被节点检测到"
	case !capability.Authenticated:
		return agent + " CLI 登录不可用，请在执行节点重新登录"
	default:
		return agent + " · " + fallback
	}
}
func applyDraft(t *Task, i input) error {
	for k, p := range map[string]*string{"projectId": &t.ProjectID, "title": &t.Title, "goal": &t.Goal, "scope": &t.Scope, "acceptanceCriteria": &t.Criteria, "refName": &t.Ref, "executionAgent": &t.Agent, "reviewAgent": &t.Reviewer, "testId": &t.TestID} {
		if v, ok := i[k]; ok {
			s, ok := v.(string)
			if !ok || len(s) > 16000 {
				return invalid("任务字段无效")
			}
			*p = strings.TrimSpace(s)
		}
	}
	if v, ok := i["requireTest"]; ok {
		b, ok := v.(bool)
		if !ok {
			return invalid("测试要求无效")
		}
		t.RequireTest = b
	}
	return nil
}
func (s *Server) action(tx *transaction, id, route string, ss session, i input) (result, error) {
	switch route {
	case "pairings":
		raw, pid, exp := token(), ID(), now().Add(5*time.Minute)
		if _, err := tx.Exec("DELETE FROM secrets WHERE kind='pairing' AND subject=?", ss.Device.ID); err != nil {
			return result{}, err
		}
		if err := addSecret(tx, "pairing", raw, ss.Device.ID, exp); err != nil {
			return result{}, err
		}
		return result{201, map[string]any{"pairingId": pid, "token": raw, "expiresAt": exp}}, nil
	case "devices/{id}/revoke":
		d, err := found[Device](tx, "device", id)
		if err != nil {
			return result{}, err
		}
		if d.Revoked == nil {
			at := now()
			d.Revoked = &at
			if err = put(tx, "device", d.ID, "", d); err != nil {
				return result{}, err
			}
			if _, err = tx.Exec("DELETE FROM secrets WHERE subject=? AND kind IN ('session','pairing')", d.ID); err != nil {
				return result{}, err
			}
			if err = emit(tx, "device_revoked", nil, "", map[string]any{"deviceId": id, "actorDeviceId": ss.Device.ID, "revokedAt": at}); err != nil {
				return result{}, err
			}
		}
		return ok(d), nil
	case "projects/{id}/base-resolutions":
		p, err := found[Project](tx, "project", id)
		if err != nil {
			return result{}, err
		}
		ref := strings.TrimSpace(i.str("refName"))
		if ref == "" || len(ref) > 250 || strings.HasPrefix(ref, "-") || strings.ContainsAny(ref, "\r\n\x00") {
			return result{}, invalid("请输入 Git 基准引用")
		}
		n, err := get[Node](tx, "node", p.NodeID)
		if err != nil && !errors.Is(err, missing) {
			return result{}, err
		}
		if !nodeReady(n) {
			return result{}, unavailable("节点离线，无法解析提交")
		}
		v := Resolution{ID: ID(), ProjectID: id, NodeID: p.NodeID, Ref: ref, Status: "pending", ExpiresAt: now().Add(10 * time.Minute)}
		if err = put(tx, "resolution", v.ID, id, v); err != nil {
			return result{}, err
		}
		_, err = newCommand(tx, "resolve_base", nil, nil, map[string]any{"resolutionId": v.ID, "projectId": p.ID, "path": p.Path, "refName": ref})
		return result{202, v}, err
	case "tasks":
		t := Task{ID: ID(), State: "draft", Revision: "0", CreatedBy: ss.Device.ID, Agent: "codex", Blockers: []Blocker{}}
		if src := i.str("sourceTaskId"); src != "" {
			old, err := found[Task](tx, "task", src)
			if err != nil {
				return result{}, err
			}
			t.ProjectID, t.Title, t.Goal, t.Scope, t.Criteria = old.ProjectID, old.Title, old.Goal, old.Scope, old.Criteria
			t.Ref, t.Agent, t.Reviewer, t.TestID, t.RequireTest, t.SourceID = old.Ref, old.Agent, old.Reviewer, old.TestID, old.RequireTest, src
		}
		if err := applyDraft(&t, i); err != nil {
			return result{}, err
		}
		if t.ProjectID != "" {
			if _, err := found[Project](tx, "project", t.ProjectID); err != nil {
				return result{}, err
			}
		}
		if err := change(tx, &t); err != nil {
			return result{}, err
		}
		return result{201, t}, nil
	case "requests/{id}/decide":
		return s.decide(tx, id, ss, i)
	}
	t, err := found[Task](tx, "task", id)
	if err != nil {
		return result{}, err
	}
	if route == "tasks/{id}/acceptance" {
		return s.accept(tx, t, ss, i)
	}
	if err := revised(t, i); err != nil {
		return result{}, err
	}
	switch route {
	case "draft", "tasks/{id}/submit":
		if t.State != "draft" {
			return result{}, transition("只有草稿可以编辑或提交")
		}
		if err = applyDraft(&t, i); err != nil {
			return result{}, err
		}
		var p Project
		if t.ProjectID != "" {
			p, err = found[Project](tx, "project", t.ProjectID)
			if err != nil {
				return result{}, err
			}
		}
		if route != "draft" {
			if t.Title == "" || t.Goal == "" || t.Scope == "" || t.Criteria == "" || t.ProjectID == "" {
				return result{}, invalid("请填写项目、标题、目标、范围与验收条件")
			}
			v, err := found[Resolution](tx, "resolution", i.str("resolutionId"))
			if err != nil {
				return result{}, err
			}
			if v.ProjectID != p.ID || v.NodeID != p.NodeID || v.Status != "resolved" || !now().Before(v.ExpiresAt) || v.Commit != i.str("baseCommit") {
				return result{}, conflict("stale_target", "请重新解析并确认基准版本", nil)
			}
			t.Base, t.Ref = v.Commit, v.Ref
			if t.RequireTest && t.TestID == "" {
				return result{}, invalid("必需测试必须选择一个登记项")
			}
			if t.TestID != "" && registered(p, t.TestID).ID == "" {
				return result{}, invalid("测试未在该项目登记")
			}
			n, err := found[Node](tx, "node", p.NodeID)
			if err != nil {
				return result{}, err
			}
			if err = checkCombo(n, t); err != nil {
				return result{}, err
			}
			queue(&t)
		}
		if err = change(tx, &t); err != nil {
			return result{}, err
		}
		return ok(t), emit(tx, "owner_command", &t, "", map[string]string{"action": route, "deviceId": ss.Device.ID})
	case "tasks/{id}/cancel":
		return s.cancel(tx, t, ss, i)
	case "tasks/{id}/recover":
		a, err := found[Attempt](tx, "attempt", i.str("attemptId"))
		if err != nil {
			return result{}, err
		}
		if t.CancelID != "" || a.ID != value(t.AttemptID) || a.TaskID != t.ID {
			return result{}, conflict("stale_target", "尝试已变更", nil)
		}
		if i.str("choice") != "new_attempt" || a.EndedAt == nil || a.Status != "interrupted" || terminal(t.State) {
			return result{}, conflict("outcome_unknown", "当前没有已经核实可用的恢复方式", nil)
		}
		queue(&t)
		if a.Role == "review" {
			t.State = "awaiting_review"
		}
		if err = change(tx, &t); err != nil {
			return result{}, err
		}
		return ok(t), emit(tx, "owner_command", &t, a.ID, map[string]string{"action": "new_attempt", "deviceId": ss.Device.ID})
	}
	return result{}, fail(404, "not_found", "操作不存在")
}
func registered(p Project, id string) TestDefinition {
	for _, test := range p.Tests {
		if test.ID == id {
			return test
		}
	}
	return TestDefinition{}
}

func (s *Server) cancel(tx *transaction, t Task, ss session, i input) (result, error) {
	if terminal(t.State) {
		return result{}, transition("任务已结束")
	}
	if t.CancelID != "" {
		return result{202, map[string]string{"commandId": t.CancelID, "taskId": t.ID, "revision": t.Revision}}, nil
	}
	if t.AttemptID != nil && (t.State == "queued" || t.State == "running" || t.State == "awaiting_review" || t.State == "reviewing") {
		a, err := found[Attempt](tx, "attempt", *t.AttemptID)
		if err != nil {
			return result{}, err
		}
		start, err := found[Command](tx, "command", a.CommandID)
		if err != nil {
			return result{}, err
		}
		if a.EndedAt == nil && start.Phase != "pending" {
			c, err := newCommand(tx, "cancel_attempt", &t, &a, map[string]any{"startCommandId": a.CommandID, "reason": i.str("reason"), "deviceId": ss.Device.ID})
			if err != nil {
				return result{}, err
			}
			t.CancelID = c.ID
			if err = closeRequests(tx, t.ID, a.ID); err != nil {
				return result{}, err
			}
			if err = change(tx, &t); err != nil {
				return result{}, err
			}
			if err = emit(tx, "cancel_requested", &t, a.ID, map[string]string{"deviceId": ss.Device.ID, "commandId": c.ID}); err != nil {
				return result{}, err
			}
			return result{202, map[string]string{"commandId": c.ID, "taskId": t.ID, "revision": t.Revision}}, nil
		}
		if a.EndedAt == nil {
			start.Phase = "rejected"
			a.Status = "cancelled"
			at := now()
			a.EndedAt = &at
			if err = put(tx, "command", start.ID, t.ID, start); err != nil {
				return result{}, err
			}
			if err = put(tx, "attempt", a.ID, t.ID, a); err != nil {
				return result{}, err
			}
			if err = release(tx, a); err != nil {
				return result{}, err
			}
		}
	}
	t.State = "cancelled"
	t.Blockers = []Blocker{}
	if err := closeRequests(tx, t.ID, value(t.AttemptID)); err != nil {
		return result{}, err
	}
	if err := change(tx, &t); err != nil {
		return result{}, err
	}
	return ok(t), emit(tx, "task_cancelled", &t, value(t.AttemptID), map[string]string{"deviceId": ss.Device.ID})
}
func (s *Server) decide(tx *transaction, id string, ss session, i input) (result, error) {
	req, err := found[Request](tx, "request", id)
	if err != nil {
		return result{}, err
	}
	t, err := found[Task](tx, "task", req.TaskID)
	if err != nil {
		return result{}, err
	}
	if req.Status == "expired" || !now().Before(req.ExpiresAt) && req.Status == "pending" {
		return result{}, conflict("request_expired", "请求已到期", nil)
	}
	if req.Decision != nil {
		return result{}, conflict("already_decided", "请求已有生效决定", map[string]any{"decision": req.Decision, "revision": t.Revision})
	}
	if req.Status != "pending" || t.State != "running" || t.CancelID != "" {
		return result{}, conflict("request_closed", "请求已关闭", nil)
	}
	if err = revised(t, i); err != nil {
		return result{}, err
	}
	if i.str("taskId") != t.ID || i.str("attemptId") != req.AttemptID || value(t.AttemptID) != req.AttemptID || i.str("inputDigest") != req.Input["inputDigest"] {
		return result{}, conflict("stale_target", "请求目标已有更新", nil)
	}
	outcome := i.str("outcome")
	if req.Kind == "action_approval" && (outcome != "allow_once" && outcome != "deny") || req.Kind == "user_question" && (outcome != "answer" || strings.TrimSpace(i.str("answerText")) == "") {
		return result{}, invalid("请选择有效的答复或授权决定")
	}
	d := Decision{ID: ID(), Outcome: outcome, At: now(), DeviceID: ss.Device.ID}
	if outcome == "answer" {
		d.Answer = i.str("answerText")
	}
	if err = settleRequest(tx, &t, &req, d, "decided"); err != nil {
		return result{}, err
	}
	return ok(map[string]any{"decision": d, "revision": t.Revision, "task": t}), nil
}
func settleRequest(tx *transaction, t *Task, r *Request, d Decision, status string) error {
	r.Decision = &d
	r.Status = status
	unblock(t, r.ID)
	a, err := get[Attempt](tx, "attempt", r.AttemptID)
	if err != nil {
		return err
	}
	kind := "resolve_tool"
	if d.Outcome == "allow_once" {
		kind = "run_registered_test"
	} else if d.Outcome == "answer" {
		kind = "deliver_answer"
	}
	c, err := newCommand(tx, kind, t, &a, map[string]any{"requestId": r.ID, "decisionId": d.ID, "answerText": d.Answer, "outcome": d.Outcome, "frozenInput": r.Input, "expiresAt": r.ExpiresAt})
	if err != nil {
		return err
	}
	r.CommandID = c.ID
	if err = put(tx, "request", r.ID, t.ID, r); err != nil {
		return err
	}
	if err = change(tx, t); err != nil {
		return err
	}
	return emit(tx, "request_decided", t, a.ID, map[string]any{"requestId": r.ID, "kind": r.Kind, "decisionId": d.ID, "outcome": d.Outcome, "deviceId": d.DeviceID})
}

// Expiry can revoke an unreceived command without guessing about work the
// Runner has already been allowed to start.
func expireUnreceivedDecision(tx *transaction, r *Request) error {
	if r.Decision == nil || (r.Decision.Outcome != "allow_once" && r.Decision.Outcome != "answer") {
		return nil
	}
	c, err := get[Command](tx, "command", r.CommandID)
	if err != nil {
		return err
	}
	if c.Phase != "pending" && c.Phase != "leased" {
		return nil
	}
	t, err := get[Task](tx, "task", r.TaskID)
	if err != nil {
		return err
	}
	a, err := get[Attempt](tx, "attempt", r.AttemptID)
	if err != nil {
		return err
	}
	c.Phase, c.Result = "rejected", "请求已过期，节点未获得执行许可"
	if err := put(tx, "command", c.ID, c.TaskID, c); err != nil {
		return err
	}
	r.Result = "expired"
	if err := put(tx, "request", r.ID, r.TaskID, *r); err != nil {
		return err
	}
	if t.State == "running" && t.CancelID == "" && value(t.AttemptID) == a.ID && a.EndedAt == nil {
		if _, err := newCommand(tx, "resolve_tool", &t, &a, map[string]any{"requestId": r.ID, "outcome": "expired"}); err != nil {
			return err
		}
	}
	return emit(tx, "command_changed", &t, a.ID, map[string]string{"commandId": c.ID, "type": c.Type, "phase": c.Phase, "outcome": r.Result})
}

func closeRequests(tx *transaction, task, attempt string) error {
	rs, err := list[Request](tx, "request", task)
	if err != nil {
		return err
	}
	for _, r := range rs {
		if r.AttemptID == attempt && r.Status == "pending" {
			r.Status = "closed"
			if err = put(tx, "request", r.ID, task, r); err != nil {
				return err
			}
		}
	}
	return nil
}
func (s *Server) accept(tx *transaction, t Task, ss session, i input) (result, error) {
	aid := i.str("artifactVersionId")
	if prior, err := get[Decision](tx, "acceptance", aid); err == nil {
		a, err := get[Artifact](tx, "artifact", aid)
		if err != nil {
			return result{}, err
		}
		if a.TaskID == t.ID {
			return result{}, conflict("already_decided", "此版本已有验收决定", map[string]any{"decision": prior, "revision": t.Revision})
		}
	} else if !errors.Is(err, missing) {
		return result{}, err
	}
	if err := revised(t, i); err != nil {
		return result{}, err
	}
	if aid == "" || aid != value(t.ArtifactID) {
		return result{}, conflict("stale_target", "只能决定当前版本", nil)
	}
	if t.State != "awaiting_acceptance" || t.CancelID != "" {
		return result{}, transition("此版本尚未达到验收状态")
	}
	a, err := found[Artifact](tx, "artifact", aid)
	if err != nil {
		return result{}, err
	}
	outcome := i.str("outcome")
	d := Decision{ID: ID(), Outcome: outcome, At: now(), DeviceID: ss.Device.ID, Reason: strings.TrimSpace(i.str("reason"))}
	switch outcome {
	case "accept":
		if err = s.verify(a); err != nil {
			return result{}, err
		}
		if t.RequireTest && !a.Evidence {
			return result{}, fail(422, "evidence_missing", "当前版本缺少成功的必需测试记录")
		}
		if t.Reviewer != "" && (a.Review == nil || a.Review.Outcome != "pass" || a.Review.AttemptID != value(t.AttemptID)) {
			return result{}, transition("当前版本缺少有效的独立复核结论")
		}
		t.State = "completed"
		t.Blockers = []Blocker{}
	case "request_rework":
		if d.Reason == "" {
			return result{}, invalid("请填写返工原因")
		}
		if !rework(&t, d.Reason) {
			if err = change(tx, &t); err != nil {
				return result{}, err
			}
			e := conflict("rework_limit", "已达到两轮返工上限", map[string]any{"revision": t.Revision})
			return result{e.Status, errorBody(e)}, nil
		}
	default:
		return result{}, invalid("验收决定无效")
	}
	if err = put(tx, "acceptance", aid, t.ID, d); err != nil {
		return result{}, err
	}
	if err = change(tx, &t); err != nil {
		return result{}, err
	}
	if err = emit(tx, "acceptance_recorded", &t, value(t.AttemptID), map[string]any{"artifactVersionId": aid, "decisionId": d.ID, "outcome": outcome, "deviceId": d.DeviceID}); err != nil {
		return result{}, err
	}
	return ok(map[string]any{"decision": d, "task": t}), nil
}
func rework(t *Task, reason string) bool {
	if t.ReworkCount >= 2 {
		block(t, "awaiting_input", "rework_limit", "已达到两轮返工上限")
		return false
	}
	t.ReworkCount++
	t.Feedback = reason
	queue(t)
	return true
}
func release(tx *transaction, a Attempt) error {
	_, err := tx.Exec("DELETE FROM write_slot WHERE node=? AND attempt=?", a.NodeID, a.ID)
	return err
}
func (s *Server) maintain(tx *transaction) error {
	n, err := get[Node](tx, "node", "local")
	if err != nil && !errors.Is(err, missing) {
		return err
	}
	if n.Availability == "online" && !nodeReady(n) {
		n.Availability = "offline"
		if err = saveNode(tx, n); err != nil {
			return err
		}
	}
	rs, err := list[Request](tx, "request", "")
	if err != nil {
		return err
	}
	for _, r := range rs {
		if now().Before(r.ExpiresAt) {
			continue
		}
		switch {
		case r.Status == "pending":
			t, err := get[Task](tx, "task", r.TaskID)
			if err != nil {
				return err
			}
			if err := settleRequest(tx, &t, &r, Decision{ID: ID(), Outcome: "expired", At: now()}, "expired"); err != nil {
				return err
			}
		case r.Status == "decided" && r.Result == "":
			if err := expireUnreceivedDecision(tx, &r); err != nil {
				return err
			}
		}
	}
	ts, err := list[Task](tx, "task", "")
	if err != nil {
		return err
	}
	sort.SliceStable(ts, func(i, j int) bool { return ts[i].QueuedAt.Before(ts[j].QueuedAt) })
	var occupied int
	if err = tx.QueryRow("SELECT count(*) FROM write_slot WHERE node='local'").Scan(&occupied); err != nil {
		return err
	}
	online := nodeReady(n)
	for _, t := range ts {
		if terminal(t.State) || t.State == "draft" {
			continue
		}
		var changed bool
		if !online && (t.State == "queued" || t.State == "running" || t.State == "awaiting_review" || t.State == "reviewing") {
			changed = block(&t, "node_disconnected", "local", "执行节点失联")
		} else {
			changed = unblock(&t, "local")
		}
		waitingForExecution := t.State == "queued" && t.AttemptID == nil
		reason := ""
		if online && waitingForExecution && t.CancelID == "" {
			reason = capabilityIssue(n, t)
		}
		if reason != "" {
			changed = block(&t, "capability_unavailable", "execution_capability", reason) || changed
		} else {
			changed = unblock(&t, "execution_capability") || changed
		}
		if changed {
			if err = change(tx, &t); err != nil {
				return err
			}
		}
		if !online || t.CancelID != "" || len(t.Blockers) > 0 || meta(tx, "pause_dispatch") == "on" {
			continue
		}
		if waitingForExecution && occupied == 0 {
			if err = s.dispatch(tx, &t, n, "execution"); err != nil {
				return err
			}
			occupied = 1
		}
		if t.State == "awaiting_review" && n.Capabilities[t.Reviewer].Review && n.Capabilities[t.Reviewer].Authenticated {
			if t.AttemptID != nil {
				a, err := get[Attempt](tx, "attempt", *t.AttemptID)
				if err != nil {
					return err
				}
				if a.Role == "review" && a.EndedAt == nil {
					continue
				}
			}
			if err = s.dispatch(tx, &t, n, "review"); err != nil {
				return err
			}
		}
	}
	if s.History > 0 {
		seq, _ := strconv.ParseInt(meta(tx, "seq"), 10, 64)
		floor, _ := strconv.ParseInt(meta(tx, "floor"), 10, 64)
		if cutoff := seq - int64(s.History); cutoff > floor {
			return setMeta(tx, "floor", strconv.FormatInt(cutoff, 10))
		}
	}
	return nil
}
func (s *Server) dispatch(tx *transaction, t *Task, n Node, role string) error {
	p, err := get[Project](tx, "project", t.ProjectID)
	if err != nil {
		return err
	}
	attempts, err := list[Attempt](tx, "attempt", t.ID)
	if err != nil {
		return err
	}
	a := Attempt{ID: ID(), TaskID: t.ID, Ordinal: len(attempts) + 1, Role: role, NodeID: n.ID, Agent: t.Agent, Status: "dispatching", Recovery: []string{}}
	payload := map[string]any{"task": t, "project": p, "test": registered(p, t.TestID)}
	if role == "execution" {
		a.Fence = inc(meta(tx, "fence"))
		if err = setMeta(tx, "fence", a.Fence); err != nil {
			return err
		}
		if _, err = tx.Exec("INSERT INTO write_slot VALUES(?,?)", n.ID, a.ID); err != nil {
			return err
		}
		if t.ArtifactID != nil {
			art, err := get[Artifact](tx, "artifact", *t.ArtifactID)
			if err != nil {
				return err
			}
			if s.verify(art) == nil {
				payload["previousArtifact"] = art
			}
		}
	} else {
		art, err := get[Artifact](tx, "artifact", value(t.ArtifactID))
		if err != nil {
			return err
		}
		if err = s.verify(art); err != nil {
			if block(t, "awaiting_input", "artifact_unavailable", "当前归档损坏或缺失") {
				return change(tx, t)
			}
			return nil
		}
		a.Agent = t.Reviewer
		a.ArtifactID = art.ID
		a.ProducerID = art.AttemptID
		payload["artifact"] = art
	}
	c, err := newCommand(tx, "start_"+role, t, &a, payload)
	if err != nil {
		return err
	}
	a.CommandID = c.ID
	t.AttemptID = &a.ID
	if err = put(tx, "attempt", a.ID, t.ID, a); err != nil {
		return err
	}
	if err = change(tx, t); err != nil {
		return err
	}
	return emit(tx, "attempt_changed", t, a.ID, map[string]string{"status": a.Status, "role": a.Role, "commandId": c.ID, "nodeId": n.ID})
}
func saveNode(tx *transaction, n Node) error {
	if err := put(tx, "node", n.ID, "", n); err != nil {
		return err
	}
	return emit(tx, "node_changed", nil, "", map[string]any{"nodeId": n.ID, "availability": n.Availability, "lastSeenAt": n.LastSeen})
}
func markUnknown(tx *transaction, t *Task, a *Attempt, message string) error {
	a.Status = "unknown"
	a.Result = message
	if err := put(tx, "attempt", a.ID, t.ID, *a); err != nil {
		return err
	}
	if block(t, "awaiting_input", "outcome:"+a.ID, message) {
		return change(tx, t)
	}
	return nil
}
func finishExecution(tx *transaction, t *Task, a *Attempt, status string) error {
	a.Status = status
	at := now()
	a.EndedAt = &at
	if err := put(tx, "attempt", a.ID, t.ID, *a); err != nil {
		return err
	}
	if err := closeRequests(tx, t.ID, a.ID); err != nil {
		return err
	}
	if err := release(tx, *a); err != nil {
		return err
	}
	t.Blockers = []Blocker{}
	switch status {
	case "cancelled":
		t.State = "cancelled"
	case "failed":
		t.State = "failed"
	case "interrupted":
		a.Recovery = []string{"new_attempt"}
		if err := put(tx, "attempt", a.ID, t.ID, *a); err != nil {
			return err
		}
		block(t, "awaiting_input", "recovery:"+a.ID, "执行已退出，可以开启新的尝试")
	case "succeeded":
		if a.ArtifactID == "" {
			return fmt.Errorf("finished execution missing archive")
		}
		t.ArtifactID = &a.ArtifactID
		if t.Reviewer == "" {
			t.State = "awaiting_acceptance"
		} else {
			t.State = "awaiting_review"
		}
	}
	if status != "succeeded" && a.ArtifactID != "" && t.ArtifactID == nil {
		t.ArtifactID = &a.ArtifactID
	}
	return change(tx, t)
}
