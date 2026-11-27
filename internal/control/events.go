package control

import (
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type snapshot struct {
	ID        string    `json:"snapshotId"`
	Stream    string    `json:"streamId"`
	HighWater string    `json:"highWater"`
	Tasks     []Task    `json:"tasks"`
	Pending   []Request `json:"pendingRequests"`
	Nodes     []Node    `json:"nodes"`
	Projects  []Project `json:"projects"`
	Next      *string   `json:"nextCursor"`
	Expires   time.Time `json:"-"`
	Owner     string    `json:"-"`
}

func (s *Server) snapshot(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	sid := q.Get("snapshotId")
	var snap snapshot
	err := s.Tx(func(tx *transaction) error {
		ss, err := authenticate(tx, r)
		if err != nil {
			return err
		}
		if sid != "" {
			raw, ok := s.snapshots.Load(sid)
			if !ok {
				return fail(410, "snapshot_expired", "快照已过期，请重新同步")
			}
			snap = raw.(snapshot)
			if now().After(snap.Expires) || snap.Owner != ss.Owner || snap.Stream != meta(tx, "stream") {
				return fail(410, "snapshot_expired", "快照已过期，请重新同步")
			}
			return nil
		}
		snap = snapshot{ID: ID(), Stream: meta(tx, "stream"), HighWater: meta(tx, "seq"), Owner: ss.Owner, Expires: now().Add(2 * time.Minute), Pending: []Request{}}
		if snap.Tasks, err = tasks(tx); err != nil {
			return err
		}
		if snap.Nodes, err = nodes(tx); err != nil {
			return err
		}
		if snap.Projects, err = projects(tx); err != nil {
			return err
		}
		rs, err := list[Request](tx, "request", "")
		if err != nil {
			return err
		}
		for _, item := range rs {
			if item.Status == "pending" && now().Before(item.ExpiresAt) {
				snap.Pending = append(snap.Pending, item)
			}
		}
		return nil
	})
	if err != nil {
		writeError(w, err)
		return
	}
	if sid == "" && len(snap.Tasks) > 100 {
		count := 0
		s.snapshots.Range(func(key, val any) bool {
			count++
			if count >= 64 {
				s.snapshots.Delete(key)
				return false
			}
			return true
		})
		s.snapshots.Store(snap.ID, snap)
	}
	from := 0
	if sid != "" {
		from, err = strconv.Atoi(q.Get("cursor"))
		if err != nil || from < 1 || from%100 != 0 || from >= len(snap.Tasks) {
			writeError(w, invalid("快照游标无效"))
			return
		}
	}
	to := min(len(snap.Tasks), from+100)
	if to < len(snap.Tasks) {
		next := strconv.Itoa(to)
		snap.Next = &next
	}
	snap.Tasks = snap.Tasks[from:to]
	writeJSON(w, 200, snap)
}
func eventRows(tx *transaction, task string, after int64, limit int) ([]Event, error) {
	query := "SELECT body FROM events WHERE seq>?"
	args := []any{after}
	if task != "" {
		query += " AND task=?"
		args = append(args, task)
	}
	rows, err := tx.Query(query+" ORDER BY seq LIMIT ?", append(args, limit)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	buf := []Event{}
	for rows.Next() {
		var b []byte
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		event, err := decode[Event](b)
		if err != nil {
			return nil, err
		}
		buf = append(buf, event)
	}
	return buf, rows.Err()
}
func (s *Server) events(w http.ResponseWriter, r *http.Request) {
	stream, afterText := r.URL.Query().Get("streamId"), r.URL.Query().Get("afterSeq")
	if h := r.Header.Get("Last-Event-ID"); h != "" {
		stream, afterText, _ = strings.Cut(h, ":")
	}
	after, err := strconv.ParseInt(afterText, 10, 64)
	if err != nil || after < 0 || strconv.FormatInt(after, 10) != afterText {
		writeError(w, invalid("事件读取位置无效"))
		return
	}
	controller := http.NewResponseController(w)
	refresh := time.NewTicker(time.Second)
	defer refresh.Stop()
	keepalive := time.NewTicker(10 * time.Second)
	defer keepalive.Stop()
	open := false
	for {
		var batch []Event
		err = s.Tx(func(tx *transaction) error {
			if _, err := authenticate(tx, r); err != nil {
				return err
			}
			high, _ := strconv.ParseInt(meta(tx, "seq"), 10, 64)
			floor, _ := strconv.ParseInt(meta(tx, "floor"), 10, 64)
			if stream != meta(tx, "stream") || after < floor || after > high {
				return fail(410, "cursor_expired", "事件读取位置已失效，请重新同步")
			}
			batch, err = eventRows(tx, "", after, 100)
			return err
		})
		_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
		if err != nil {
			if !open {
				writeError(w, err)
			} else if a, ok := err.(*apiError); ok && a.Status == 410 {
				_, _ = fmt.Fprint(w, "event: reset_required\ndata: {}\n\n")
				_ = controller.Flush()
			}
			return
		}
		if !open {
			w.Header().Set("Content-Type", "text/event-stream")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(200)
			open = true
			if err = controller.Flush(); err != nil {
				return
			}
		}
		for _, e := range batch {
			if _, err = fmt.Fprintf(w, "id: %s:%s\nevent: %s\ndata: %s\n\n", stream, e.Seq, e.Type, encode(e)); err != nil {
				return
			}
			after, _ = strconv.ParseInt(e.Seq, 10, 64)
		}
		if len(batch) > 0 {
			if err = controller.Flush(); err != nil {
				return
			}
		}
		if len(batch) == 100 {
			continue
		}
		select {
		case <-r.Context().Done():
			return
		case <-refresh.C:
		case <-keepalive.C:
			_ = controller.SetWriteDeadline(time.Now().Add(5 * time.Second))
			if _, err = fmt.Fprint(w, ": heartbeat\n\n"); err != nil {
				return
			}
			if err = controller.Flush(); err != nil {
				return
			}
		}
	}
}
