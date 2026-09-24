package main

import (
	"fmt"
	"net/http"
	"sync"
	"time"
)

// hub distributes "something changed" signals to open clients
// (Server-Sent Events). No payload — clients simply reload. Each connection
// belongs to a user: signals only go to those affected (changed), otherwise
// every account would see when others are active.
type hub struct {
	mu   sync.Mutex
	subs map[chan struct{}]int64 // channel → user
}

func newHub() *hub {
	return &hub{subs: map[chan struct{}]int64{}}
}

func (h *hub) subscribe(uid int64) chan struct{} {
	ch := make(chan struct{}, 1)
	h.mu.Lock()
	h.subs[ch] = uid
	h.mu.Unlock()
	return ch
}

func (h *hub) unsubscribe(ch chan struct{}) {
	h.mu.Lock()
	delete(h.subs, ch)
	h.mu.Unlock()
}

// notifyUsers is non-blocking: full channels (the client is already behind
// on a signal) are skipped — one signal is enough, since it reloads
// completely anyway.
func (h *hub) notifyUsers(uids []int64) {
	want := map[int64]bool{}
	for _, u := range uids {
		want[u] = true
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch, uid := range h.subs {
		if !want[uid] {
			continue
		}
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// notifyAll: all connections (accounts created/removed, auto-archive).
func (h *hub) notifyAll() {
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.subs {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

// changed notifies uid and everyone who shares a list with uid — only they
// can see a change made by uid. extra: additional people affected (a member
// just removed no longer sees the list).
func (s *server) changed(uid int64, extra ...int64) {
	uids := append([]int64{uid}, extra...)
	rows, err := s.db.Query("SELECT DISTINCT b.user_id FROM list_access a JOIN list_access b ON a.list_id = b.list_id WHERE a.user_id = ?", uid)
	if err != nil {
		s.hub.notifyAll() // better too many than a missed change
		return
	}
	for rows.Next() {
		var id int64
		if rows.Scan(&id) == nil {
			uids = append(uids, id)
		}
	}
	rows.Close()
	s.hub.notifyUsers(uids)
}

// sseHeartbeat keeps proxies (nginx proxy_read_timeout) and mobile-carrier
// NATs alive; the session is also checked on every beat.
var sseHeartbeat = 25 * time.Second

func (s *server) handleEvents(w http.ResponseWriter, r *http.Request) {
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, http.StatusInternalServerError, "error.streaming")
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("X-Accel-Buffering", "no")
	fmt.Fprint(w, "retry: 3000\n\n")
	flusher.Flush()

	ch := s.hub.subscribe(userID(r))
	defer s.hub.unsubscribe(ch)
	heartbeat := time.NewTicker(sseHeartbeat)
	defer heartbeat.Stop()

	for {
		select {
		case <-r.Context().Done():
			return
		case <-s.shutdown:
			// Close on shutdown instead of letting srv.Shutdown run into its
			// timeout. The client reconnects on its own after the restart.
			return
		case <-ch:
			fmt.Fprint(w, "data: changed\n\n")
			flusher.Flush()
		case <-heartbeat.C:
			// Logged out (also "log out everywhere", passkey deleted): close the
			// stream instead of continuing to report change events to the browser.
			if !s.authed(r) {
				return
			}
			fmt.Fprint(w, ": ping\n\n")
			flusher.Flush()
		}
	}
}
