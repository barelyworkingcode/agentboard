// Package server is the agentboard HTTP server: ingest and read routes,
// browser actions, SSE, listeners and the serve command.
package server

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"io/fs"
	"log/slog"
	"net/http"
	"net/netip"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/barelyworkingcode/agentboard/internal/store"
	"github.com/barelyworkingcode/agentboard/internal/wire"
)

var (
	RetryInterval = 30 * time.Second
	QuietTick     = 30 * time.Second
	Coalesce      = 250 * time.Millisecond
)

const (
	maxBody       = 64 << 10
	maxSSEClients = 64
	keepAlive     = 25 * time.Second
)

type Options struct {
	Allow  []netip.Prefix
	Assets fs.FS
	Logger *slog.Logger
	Now    func() time.Time
}

type Handler struct {
	st     *store.Store
	allow  []netip.Prefix
	assets fs.FS
	log    *slog.Logger
	now    func() time.Time
	mux    *http.ServeMux

	boot string
	n    atomic.Int64

	mu       sync.Mutex
	clients  map[chan struct{}]struct{}
	done     chan struct{}
	doneOnce sync.Once
}

type ingestKey struct{}

func withIngest(ctx context.Context, ingest bool) context.Context {
	return context.WithValue(ctx, ingestKey{}, ingest)
}

func listenerIngest(ctx context.Context) bool {
	v, _ := ctx.Value(ingestKey{}).(bool)
	return v
}

func NewHandler(st *store.Store, opt Options) *Handler {
	h := &Handler{
		st:      st,
		allow:   opt.Allow,
		assets:  opt.Assets,
		log:     opt.Logger,
		now:     opt.Now,
		boot:    bootID(),
		clients: make(map[chan struct{}]struct{}),
		done:    make(chan struct{}),
	}
	if h.log == nil {
		h.log = slog.Default()
	}
	if h.now == nil {
		h.now = time.Now
	}
	h.mux = h.routes()
	return h
}

func bootID() string {
	b := make([]byte, 3)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// ForListener returns the handler for one listener. The ingest flag is also
// stamped by the listener's BaseContext in serve; stamping it here keeps the
// handler correct when mounted without that server (tests, httptest).
func (h *Handler) ForListener(ingest bool) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		h.mux.ServeHTTP(w, r.WithContext(withIngest(r.Context(), ingest)))
	})
}

func (h *Handler) routes() *http.ServeMux {
	m := http.NewServeMux()
	get := func(p string, f http.HandlerFunc) { m.Handle(p, only(http.MethodGet, f)) }
	ingest := func(p string, f http.HandlerFunc) { m.Handle(p, only(http.MethodPost, h.gate(f))) }
	ui := func(p string, f http.HandlerFunc) { m.Handle(p, only(http.MethodPost, h.sameOrigin(f))) }

	get("/{$}", h.asset("index.html", "text/html; charset=utf-8"))
	get("/app.js", h.asset("app.js", "text/javascript; charset=utf-8"))
	get("/app.css", h.asset("app.css", "text/css; charset=utf-8"))
	get("/api/board", h.board)
	get("/api/sessions/{id}", h.sessionDetail)
	get("/api/clear-counts", h.clearCounts)
	get("/events", h.events)

	ingest("/api/hook", h.hook)
	ingest("/api/decisions", h.ask)
	ingest("/api/decisions/{id}/answer", h.answer)
	ingest("/api/decisions/{id}/dismiss", h.dismiss)
	ingest("/api/state", h.state)
	ingest("/api/log", h.logLine)
	ingest("/api/items", h.item)
	ingest("/api/notes", h.note)
	ingest("/api/runs/start", h.runStart)
	ingest("/api/runs/end", h.runEnd)
	ingest("/api/meter", h.meter)
	ingest("/api/prune", h.prune)

	ui("/ui/clear", h.uiClear)
	ui("/ui/sessions/{id}/delete", h.uiDelete)
	ui("/ui/decisions/{id}/dismiss", h.uiDismiss)

	m.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		writeErr(w, http.StatusNotFound, "not found")
	})
	return m
}

func only(method string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != method {
			w.Header().Set("Allow", method)
			writeErr(w, http.StatusMethodNotAllowed, "method not allowed")
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Handler) version() (int64, string) {
	n := h.n.Load()
	return n, h.boot + "." + strconv.FormatInt(n, 10)
}

// bump advances the version and wakes every SSE client.
func (h *Handler) bump() {
	h.n.Add(1)
	h.mu.Lock()
	defer h.mu.Unlock()
	for ch := range h.clients {
		select {
		case ch <- struct{}{}:
		default:
		}
	}
}

func (h *Handler) nowMS() int64 { return h.now().UnixMilli() }

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, wire.ErrorResp{Error: msg})
}

func (h *Handler) internal(w http.ResponseWriter, r *http.Request, err error) {
	h.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
	writeErr(w, http.StatusInternalServerError, "internal error")
}

func etagMatches(header, etag string) bool {
	for _, t := range strings.Split(header, ",") {
		t = strings.TrimPrefix(strings.TrimSpace(t), "W/")
		if t == etag || t == "*" {
			return true
		}
	}
	return false
}
