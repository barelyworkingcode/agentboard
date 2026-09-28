package server

import (
	"errors"
	"io/fs"
	"net/http"

	"github.com/barelyworkingcode/agentboard/internal/hookstate"
	"github.com/barelyworkingcode/agentboard/internal/store"
	"github.com/barelyworkingcode/agentboard/internal/wire"
)

func (h *Handler) asset(name, contentType string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.assets == nil {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		b, err := fs.ReadFile(h.assets, name)
		if errors.Is(err, fs.ErrNotExist) {
			writeErr(w, http.StatusNotFound, "not found")
			return
		}
		if err != nil {
			h.internal(w, r, err)
			return
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(b)
	}
}

// notModified sets the validators and reports whether the client's copy is
// current, in which case a 304 has been written.
func notModified(w http.ResponseWriter, r *http.Request, version string) bool {
	etag := `"` + version + `"`
	w.Header().Set("ETag", etag)
	w.Header().Set("Cache-Control", "no-cache")
	if inm := r.Header.Get("If-None-Match"); inm != "" && etagMatches(inm, etag) {
		w.WriteHeader(http.StatusNotModified)
		return true
	}
	return false
}

func openDecisionSessions(ds []wire.Decision) map[string]bool {
	open := make(map[string]bool)
	for _, d := range ds {
		if d.Status == "open" && d.Ctx.Session != "" {
			open[d.Ctx.Session] = true
		}
	}
	return open
}

// displayBoard reads the board and fills each session's display.
func (h *Handler) displayBoard(r *http.Request, now int64) (wire.Board, error) {
	b, err := h.st.Board(r.Context(), now)
	if err != nil {
		return b, err
	}
	open := openDecisionSessions(b.Decisions)
	for i := range b.Sessions {
		s := &b.Sessions[i]
		s.Display = hookstate.Display(*s, open[s.ID], now)
	}
	return b, nil
}

func (h *Handler) board(w http.ResponseWriter, r *http.Request) {
	_, v := h.version()
	if notModified(w, r, v) {
		return
	}
	now := h.nowMS()
	b, err := h.displayBoard(r, now)
	if err != nil {
		w.Header().Del("ETag")
		h.internal(w, r, err)
		return
	}
	b.Version = v
	writeJSON(w, http.StatusOK, b)
}

func (h *Handler) sessionDetail(w http.ResponseWriter, r *http.Request) {
	_, v := h.version()
	if notModified(w, r, v) {
		return
	}
	now := h.nowMS()
	d, err := h.st.SessionDetail(r.Context(), r.PathValue("id"))
	if err != nil {
		w.Header().Del("ETag")
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "session not found")
			return
		}
		h.internal(w, r, err)
		return
	}
	d.Version, d.Now = v, now
	d.Session.Display = hookstate.Display(d.Session, openDecisionSessions(d.Decisions)[d.Session.ID], now)
	writeJSON(w, http.StatusOK, d)
}

func (h *Handler) clearCounts(w http.ResponseWriter, r *http.Request) {
	cc, err := h.st.ClearCounts(r.Context(), h.nowMS())
	if err != nil {
		h.internal(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, cc)
}
