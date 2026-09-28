package server

import (
	"context"
	"fmt"
	"maps"
	"net/http"
	"time"

	"github.com/barelyworkingcode/agentboard/internal/hookstate"
)

// Run drives the quiet ticker until ctx ends, then releases SSE clients so a
// graceful shutdown is not held open by them.
func (h *Handler) Run(ctx context.Context) {
	defer h.doneOnce.Do(func() { close(h.done) })
	quiet, _ := h.quietSet(ctx)
	t := time.NewTicker(QuietTick)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			next, err := h.quietSet(ctx)
			if err != nil {
				h.log.Warn("quiet check failed", "err", err)
				continue
			}
			if !maps.Equal(quiet, next) {
				quiet = next
				h.bump()
			}
		}
	}
}

func (h *Handler) quietSet(ctx context.Context) (map[string]bool, error) {
	now := h.nowMS()
	b, err := h.st.Board(ctx, now)
	if err != nil {
		return nil, fmt.Errorf("read board for quiet set: %w", err)
	}
	open := openDecisionSessions(b.Decisions)
	set := make(map[string]bool)
	for _, s := range b.Sessions {
		if hookstate.Display(s, open[s.ID], now) == "quiet" {
			set[s.ID] = true
		}
	}
	return set, nil
}

func (h *Handler) subscribe() (chan struct{}, bool) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.clients) >= maxSSEClients {
		return nil, false
	}
	ch := make(chan struct{}, 1)
	h.clients[ch] = struct{}{}
	return ch, true
}

func (h *Handler) unsubscribe(ch chan struct{}) {
	h.mu.Lock()
	delete(h.clients, ch)
	h.mu.Unlock()
}

func (h *Handler) events(w http.ResponseWriter, r *http.Request) {
	ch, ok := h.subscribe()
	if !ok {
		writeErr(w, http.StatusServiceUnavailable, "too many event clients")
		return
	}
	defer h.unsubscribe(ch)

	rc := http.NewResponseController(w)
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusOK)

	var sent int64 = -1
	send := func() bool {
		n, v := h.version()
		if n == sent {
			return true
		}
		if _, err := fmt.Fprintf(w, "event: board\nid: %d\ndata: {\"version\":%q}\n\n", n, v); err != nil {
			return false
		}
		sent = n
		return rc.Flush() == nil
	}

	if _, err := fmt.Fprint(w, "retry: 3000\n\n"); err != nil || !send() {
		return
	}
	last := time.Now()
	ka := time.NewTicker(keepAlive)
	defer ka.Stop()
	var pending <-chan time.Time

	for {
		select {
		case <-r.Context().Done():
			return
		case <-h.done:
			return
		case <-ch:
			if pending != nil {
				continue
			}
			if wait := Coalesce - time.Since(last); wait > 0 {
				pending = time.After(wait)
				continue
			}
			if !send() {
				return
			}
			last = time.Now()
		case <-pending:
			pending = nil
			if !send() {
				return
			}
			last = time.Now()
		case <-ka.C:
			if _, err := fmt.Fprint(w, ": ka\n\n"); err != nil || rc.Flush() != nil {
				return
			}
		}
	}
}
