package control

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

type apiError struct {
	Status        int
	Code, Message string
	Details       any
}

func (e *apiError) Error() string                     { return e.Code }
func fail(status int, code, message string) *apiError { return &apiError{status, code, message, nil} }
func conflict(code, message string, details any) *apiError {
	return &apiError{409, code, message, details}
}
func invalid(message string) *apiError { return fail(400, "invalid_request", message) }

type session struct {
	Device                Device
	CSRF, Instance, Owner string
}
type result struct {
	status int
	data   any
}
type input map[string]any

func (i input) str(k string) string   { v, _ := i[k].(string); return v }
func (i input) boolean(k string) bool { v, _ := i[k].(bool); return v }
func (i input) object(k string) input { v, _ := i[k].(map[string]any); return v }
func ok(data any) result              { return result{200, data} }

type Server struct {
	*Store
	Origin    string
	History   int
	AllowHTTP bool
	snapshots sync.Map
}

const cookieName = "nextbot_session"

func (s *Server) Public(static http.Handler) http.Handler {
	m := http.NewServeMux()
	m.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if err := s.DB.PingContext(r.Context()); err != nil {
			http.Error(w, "unavailable", 503)
			return
		}
		writeJSON(w, 200, map[string]string{"status": "ok"})
	})
	m.HandleFunc("POST /api/v1/owner/claim", s.claim("owner"))
	m.HandleFunc("POST /api/v1/pairings/claim", s.claim("pairing"))
	m.HandleFunc("GET /api/v1/events", s.events)
	m.HandleFunc("GET /api/v1/artifacts/{id}/entries/{entry}", s.artifactBytes)
	m.HandleFunc("GET /api/v1/snapshot", s.snapshot)
	for _, p := range []string{"session", "devices", "projects", "nodes", "tasks", "tasks/{id}", "tasks/{id}/attempts", "tasks/{id}/artifacts", "tasks/{id}/timeline", "artifacts/{id}", "requests/{id}", "commands/{id}", "projects/{id}/base-resolutions/{resolution}", "diagnostics"} {
		m.HandleFunc("GET /api/v1/"+p, s.read(p))
	}
	for _, p := range []string{"pairings", "devices/{id}/revoke", "projects/{id}/base-resolutions", "tasks", "tasks/{id}/submit", "tasks/{id}/cancel", "tasks/{id}/recover", "tasks/{id}/acceptance", "requests/{id}/decide"} {
		m.HandleFunc("POST /api/v1/"+p, s.mutate(p))
	}
	m.HandleFunc("PATCH /api/v1/tasks/{id}", s.mutate("draft"))
	m.HandleFunc("/api/v1/", func(w http.ResponseWriter, r *http.Request) { writeError(w, fail(404, "not_found", "接口不存在")) })
	m.Handle("/", static)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		w.Header().Set("Content-Security-Policy", "default-src 'self'; object-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'self'")
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Cache-Control", "no-store")
		}
		if origin := r.Header.Get("Origin"); origin != "" && origin != s.Origin {
			writeError(w, fail(403, "origin_denied", "来源校验失败"))
			return
		}
		m.ServeHTTP(w, r)
	})
}
func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.WriteHeader(status)
	if data != nil {
		_ = json.NewEncoder(w).Encode(data)
	}
}
func errorBody(e *apiError) map[string]any {
	return map[string]any{"error": map[string]any{"code": e.Code, "message": e.Message, "details": e.Details}, "traceId": ID()}
}
func writeError(w http.ResponseWriter, err error) {
	var a *apiError
	if !errors.As(err, &a) {
		log.Printf("request failed: %v", err)
		a = fail(503, "service_unavailable", "服务暂时无法完成操作")
	}
	writeJSON(w, a.Status, errorBody(a))
}
func readBody[T any](w http.ResponseWriter, r *http.Request, limit int64) (T, error) {
	var out T
	typ, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || typ != "application/json" {
		return out, invalid("需要 JSON 请求体")
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, limit))
	if err != nil {
		return out, invalid("JSON 请求体超过大小限制或无法读取")
	}
	parser := json.NewDecoder(bytes.NewReader(raw))
	parser.UseNumber()
	first, err := parser.Token()
	if err != nil || first != json.Delim('{') || !uniqueJSON(parser, first) {
		return out, invalid("JSON 请求体无效或包含重复字段")
	}
	if _, err := parser.Token(); err != io.EOF {
		return out, invalid("JSON 请求体包含多余内容")
	}
	if err := json.Unmarshal(raw, &out); err != nil {
		return out, invalid("JSON 请求体无效")
	}
	return out, nil
}
func uniqueJSON(d *json.Decoder, first json.Token) bool {
	switch first {
	case json.Delim('{'):
		keys := map[string]bool{}
		for d.More() {
			key, err := d.Token()
			name, ok := key.(string)
			if err != nil || !ok || keys[name] {
				return false
			}
			keys[name] = true
			item, err := d.Token()
			if err != nil || !uniqueJSON(d, item) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim('}')
	case json.Delim('['):
		for d.More() {
			item, err := d.Token()
			if err != nil || !uniqueJSON(d, item) {
				return false
			}
		}
		end, err := d.Token()
		return err == nil && end == json.Delim(']')
	default:
		return true
	}
}
func keyOf(r *http.Request) string {
	k := r.Header.Get("Idempotency-Key")
	if len(k) < 8 || len(k) > 128 || strings.TrimSpace(k) != k {
		return ""
	}
	return k
}
func writeOrigin(s *Server, r *http.Request) error {
	if r.Header.Get("Origin") != s.Origin {
		return fail(403, "origin_denied", "来源校验失败")
	}
	return nil
}
func authenticate(tx *transaction, r *http.Request) (session, error) {
	unauthorized := fail(401, "unauthenticated", "需要已配对的设备")
	c, err := r.Cookie(cookieName)
	if err != nil || len(c.Value) != 64 {
		return session{}, unauthorized
	}
	var id string
	err = tx.QueryRow("SELECT subject FROM secrets WHERE kind='session' AND hash=? AND expires>?", Hash([]byte(c.Value)), now().Unix()).Scan(&id)
	if errors.Is(err, missing) {
		return session{}, unauthorized
	}
	if err != nil {
		return session{}, err
	}
	dev, err := get[Device](tx, "device", id)
	if errors.Is(err, missing) || dev.Revoked != nil {
		return session{}, unauthorized
	}
	if err != nil {
		return session{}, err
	}
	if now().Sub(dev.LastSeen) >= time.Minute {
		dev.LastSeen = now()
		if err := put(tx, "device", dev.ID, "", dev); err != nil {
			return session{}, err
		}
	}
	return session{dev, Hash([]byte("csrf:" + c.Value)), meta(tx, "instance"), meta(tx, "owner")}, nil
}
func addSecret(tx *transaction, kind, raw, subject string, expiry time.Time) error {
	_, err := tx.Exec("INSERT INTO secrets VALUES(?,?,?,?)", kind, Hash([]byte(raw)), subject, expiry.Unix())
	return err
}

var ErrOwnerClaimed = errors.New("owner already claimed; use the local recover-owner command if needed")

func (s *Store) OwnerTicket(recover bool) (string, error) {
	raw := token()
	err := s.Tx(func(tx *transaction) error {
		devices, err := list[Device](tx, "device", "")
		if err != nil {
			return err
		}
		if len(devices) > 0 && !recover {
			return ErrOwnerClaimed
		}
		if recover {
			for _, d := range devices {
				if d.Revoked == nil {
					t := now()
					d.Revoked = &t
					if err := put(tx, "device", d.ID, "", d); err != nil {
						return err
					}
				}
			}
			if _, err := tx.Exec("DELETE FROM secrets"); err != nil {
				return err
			}
			if err := emit(tx, "owner_recovered", nil, "", nil); err != nil {
				return err
			}
		}
		if _, err := tx.Exec("DELETE FROM secrets WHERE kind='owner'"); err != nil {
			return err
		}
		return addSecret(tx, "owner", raw, "", now().Add(5*time.Minute))
	})
	return raw, err
}
func (s *Server) claim(kind string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := writeOrigin(s, r); err != nil {
			writeError(w, err)
			return
		}
		if keyOf(r) == "" {
			writeError(w, invalid("需要 Idempotency-Key"))
			return
		}
		body, err := readBody[input](w, r, 64<<10)
		if err != nil {
			writeError(w, err)
			return
		}
		field := "token"
		if kind == "owner" {
			field = "ticket"
		}
		raw, name := body.str(field), strings.TrimSpace(body.str("deviceName"))
		if len(raw) != 64 || name == "" || len(name) > 120 {
			writeError(w, invalid("设备名或绑定票据无效"))
			return
		}
		device := Device{ID: ID(), Name: name, Bound: now(), LastSeen: now(), Current: true}
		secret := token()
		err = s.Tx(func(tx *transaction) error {
			var subject string
			err := tx.QueryRow("DELETE FROM secrets WHERE kind=? AND hash=? AND expires>? RETURNING subject", kind, Hash([]byte(raw)), now().Unix()).Scan(&subject)
			if errors.Is(err, missing) {
				return fail(403, "invalid_ticket", "绑定凭据无效或已过期")
			}
			if err != nil {
				return err
			}
			if kind == "pairing" {
				d, err := get[Device](tx, "device", subject)
				if err != nil || d.Revoked != nil {
					return fail(403, "invalid_ticket", "绑定凭据无效或已过期")
				}
			}
			if err := put(tx, "device", device.ID, "", device); err != nil {
				return err
			}
			if err := addSecret(tx, "session", secret, device.ID, now().Add(90*24*time.Hour)); err != nil {
				return err
			}
			return emit(tx, "device_bound", nil, "", map[string]string{"deviceId": device.ID})
		})
		if err != nil {
			writeError(w, err)
			return
		}
		http.SetCookie(w, &http.Cookie{Name: cookieName, Value: secret, Path: "/", Secure: !s.AllowHTTP, HttpOnly: true, SameSite: http.SameSiteStrictMode, MaxAge: 90 * 24 * 3600})
		writeJSON(w, 201, device)
	}
}
func (s *Server) read(route string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var data any
		err := s.Tx(func(tx *transaction) error {
			ss, err := authenticate(tx, r)
			if err != nil {
				return err
			}
			data, err = s.query(tx, r, ss, route)
			return err
		})
		if err != nil {
			writeError(w, err)
			return
		}
		writeJSON(w, 200, data)
	}
}
func (s *Server) mutate(route string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		var response result
		var replayed bool
		err := s.Tx(func(tx *transaction) error {
			ss, err := authenticate(tx, r)
			if err != nil {
				return err
			}
			if err := writeOrigin(s, r); err != nil {
				return err
			}
			if subtle.ConstantTimeCompare([]byte(r.Header.Get("X-CSRF-Token")), []byte(ss.CSRF)) != 1 {
				return fail(403, "csrf_denied", "会话校验失败")
			}
			k := keyOf(r)
			if k == "" {
				return invalid("需要 Idempotency-Key")
			}
			body, err := readBody[input](w, r, 128<<10)
			if err != nil {
				return err
			}
			hash := Digest("request", []any{r.Method, r.URL.Path, body})
			scope := ss.Owner
			if route == "pairings" {
				scope = ss.Device.ID
			}
			data, status, hit, err := receipt(tx, scope, k, hash)
			if err != nil {
				return err
			}
			if hit {
				response = result{status, data}
				replayed = true
				return nil
			}
			response, err = s.action(tx, r.PathValue("id"), route, ss, body)
			if err != nil {
				return err
			}
			stored := response.data
			if route == "pairings" {
				m := stored.(map[string]any)
				stored = map[string]any{"pairingId": m["pairingId"], "expiresAt": m["expiresAt"], "secretAlreadyIssued": true}
			}
			return saveReceipt(tx, scope, k, hash, response.status, stored)
		})
		if err != nil {
			writeError(w, err)
			return
		}
		if replayed {
			w.Header().Set("Idempotency-Replayed", "true")
		}
		writeJSON(w, response.status, response.data)
	}
}
func receipt(tx *transaction, scope, key, digest string) (json.RawMessage, int, bool, error) {
	var old string
	var data json.RawMessage
	var status int
	err := tx.QueryRow("SELECT digest,status,body FROM receipts WHERE scope=? AND key=?", scope, key).Scan(&old, &status, &data)
	if errors.Is(err, missing) {
		return nil, 0, false, nil
	}
	if err != nil {
		return nil, 0, false, err
	}
	if old != digest {
		return nil, 0, false, conflict("idempotency_conflict", "操作编号已被其他内容使用", nil)
	}
	return data, status, true, nil
}
func saveReceipt(tx *transaction, scope, key, digest string, status int, data any) error {
	_, err := tx.Exec("INSERT INTO receipts VALUES(?,?,?,?,?)", scope, key, digest, status, encode(data))
	return err
}
func (s *Store) AddProject(name, path string, tests []TestDefinition) (Project, error) {
	p := Project{ID: ID(), Name: name, NodeID: "local", Path: path, Tests: tests}
	return p, s.Tx(func(tx *transaction) error {
		ps, err := list[Project](tx, "project", "")
		if err != nil {
			return err
		}
		for _, old := range ps {
			if old.Path == path {
				return fmt.Errorf("repository already registered: %s", old.ID)
			}
		}
		if err := put(tx, "project", p.ID, "", p); err != nil {
			return err
		}
		return emit(tx, "project_changed", nil, "", map[string]string{"projectId": p.ID})
	})
}
func (s *Server) query(tx *transaction, r *http.Request, ss session, route string) (any, error) {
	id := r.PathValue("id")
	switch route {
	case "session":
		return map[string]string{"ownerId": ss.Owner, "deviceId": ss.Device.ID, "instanceId": ss.Instance, "csrfToken": ss.CSRF}, nil
	case "devices":
		ds, err := list[Device](tx, "device", "")
		for i := range ds {
			ds[i].Current = ds[i].ID == ss.Device.ID
		}
		return items(ds), err
	case "projects":
		ps, err := projects(tx)
		return items(ps), err
	case "nodes":
		ns, err := nodes(tx)
		return items(ns), err
	case "tasks":
		ts, err := tasks(tx)
		state := r.URL.Query().Get("state")
		if state != "" {
			out := []Task{}
			for _, t := range ts {
				if t.State == state {
					out = append(out, t)
				}
			}
			ts = out
		}
		return items(ts), err
	case "tasks/{id}":
		t, err := found[Task](tx, "task", id)
		if err != nil {
			return nil, err
		}
		req, err := list[Request](tx, "request", id)
		if err != nil {
			return nil, err
		}
		return struct {
			Task
			Requests []Request `json:"requests"`
		}{t, req}, nil
	case "tasks/{id}/attempts":
		if _, err := found[Task](tx, "task", id); err != nil {
			return nil, err
		}
		as, err := list[Attempt](tx, "attempt", id)
		sort.Slice(as, func(i, j int) bool { return as[i].Ordinal < as[j].Ordinal })
		return items(as), err
	case "tasks/{id}/artifacts":
		if _, err := found[Task](tx, "task", id); err != nil {
			return nil, err
		}
		as, err := list[Artifact](tx, "artifact", id)
		sort.Slice(as, func(i, j int) bool { return as[i].Version > as[j].Version })
		for i := range as {
			if err := s.verify(as[i]); err != nil {
				as[i].Integrity = "unavailable"
			}
			if d, err := get[Decision](tx, "acceptance", as[i].ID); err == nil {
				as[i].Acceptance = &d
			} else if !errors.Is(err, missing) {
				return nil, err
			}
		}
		return items(as), err
	case "tasks/{id}/timeline":
		if _, err := found[Task](tx, "task", id); err != nil {
			return nil, err
		}
		after, _ := strconv.ParseInt(r.URL.Query().Get("cursor"), 10, 64)
		es, err := eventRows(tx, id, after, 101)
		if err != nil {
			return nil, err
		}
		if len(es) > 100 {
			return map[string]any{"items": es[:100], "nextCursor": es[99].Seq}, nil
		}
		return items(es), nil
	case "artifacts/{id}":
		a, err := found[Artifact](tx, "artifact", id)
		if err != nil {
			return nil, err
		}
		if d, err := get[Decision](tx, "acceptance", id); err == nil {
			a.Acceptance = &d
		} else if !errors.Is(err, missing) {
			return nil, err
		}
		return a, s.verify(a)
	case "requests/{id}":
		return found[Request](tx, "request", id)
	case "commands/{id}":
		c, err := found[Command](tx, "command", id)
		c.Payload = nil
		c.LeaseID = ""
		return c, err
	case "projects/{id}/base-resolutions/{resolution}":
		v, err := found[Resolution](tx, "resolution", r.PathValue("resolution"))
		if err == nil && v.ProjectID != id {
			return nil, fail(404, "not_found", "基准解析不存在")
		}
		return v, err
	case "diagnostics":
		ts, err := list[Task](tx, "task", "")
		if err != nil {
			return nil, err
		}
		summary := make([]map[string]any, 0, len(ts))
		for _, t := range ts {
			summary = append(summary, map[string]any{"taskId": t.ID, "state": t.State, "revision": t.Revision, "attemptId": t.AttemptID, "artifactVersionId": t.ArtifactID, "updatedAt": t.UpdatedAt})
		}
		ns, err := nodes(tx)
		if err != nil {
			return nil, err
		}
		for n := range ns {
			for name, c := range ns[n].Capabilities {
				c.Reason = ""
				ns[n].Capabilities[name] = c
			}
		}
		return map[string]any{"schemaVersion": 1, "instanceId": ss.Instance, "nodes": ns, "tasks": summary, "highWater": meta(tx, "seq")}, nil
	}
	return nil, fail(404, "not_found", "接口不存在")
}
func tasks(tx *transaction) ([]Task, error) {
	ts, err := list[Task](tx, "task", "")
	sort.SliceStable(ts, func(i, j int) bool { return ts[i].UpdatedAt.After(ts[j].UpdatedAt) })
	return ts, err
}
func projects(tx *transaction) ([]Project, error) {
	ps, err := list[Project](tx, "project", "")
	for i := range ps {
		ps[i].Path = ""
	}
	return ps, err
}
func nodes(tx *transaction) ([]Node, error) {
	ns, err := list[Node](tx, "node", "")
	for i := range ns {
		n := &ns[i]
		n.Connection = ""
		n.BootID = ""
		if now().Sub(n.LastSeen) > 15*time.Second {
			n.Availability = "offline"
		}
	}
	return ns, err
}
func (s *Server) Maintenance(ctx context.Context) {
	tick := time.NewTicker(time.Second)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			s.snapshots.Range(func(k, v any) bool {
				if now().After(v.(snapshot).Expires) {
					s.snapshots.Delete(k)
				}
				return true
			})
			if err := s.Tx(func(tx *transaction) error { return s.maintain(tx) }); err != nil {
				log.Printf("maintenance: %v", err)
			}
		}
	}
}
func ValidateOrigin(raw string, allowHTTP bool) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.Path != "" || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("origin must be an exact HTTPS origin")
	}
	if u.Scheme == "https" {
		return nil
	}
	if allowHTTP && u.Scheme == "http" && (u.Hostname() == "127.0.0.1" || u.Hostname() == "localhost" || u.Hostname() == "::1") {
		return nil
	}
	return fmt.Errorf("origin requires HTTPS; development HTTP is restricted to loopback")
}
