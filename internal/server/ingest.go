package server

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/barelyworkingcode/agentboard/internal/hookstate"
	"github.com/barelyworkingcode/agentboard/internal/store"
	"github.com/barelyworkingcode/agentboard/internal/wire"
)

var (
	repoRe     = regexp.MustCompile(`^[A-Za-z0-9-]+/[A-Za-z0-9._-]+$`)
	itemRepoRe = regexp.MustCompile(`^(?:[A-Za-z0-9-]+/)?[A-Za-z0-9._-]+$`)
	runRe      = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	sessionRe  = regexp.MustCompile(`^[A-Za-z0-9._:-]{1,128}$`)
)

const (
	limitText      = 2000
	limitStateNote = 200
	limitTitle     = 200
	limitShort     = 64
	limitProject   = 128
	limitBranch    = 255
)

var hookEvents = map[string]bool{
	"SessionStart": true, "UserPromptSubmit": true, "PreToolUse": true, "PostToolUse": true,
	"PostToolUseFailure": true, "Notification": true, "Stop": true, "SessionEnd": true,
}

// truncate cuts s to at most n bytes without splitting a UTF-8 sequence.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	i := n
	for i > 0 && !utf8.RuneStart(s[i]) {
		i--
	}
	return s[:i]
}

// cleanCtx sanitises labels; a bad value is blanked, never rejected.
func cleanCtx(c wire.Ctx) wire.Ctx {
	c.Machine = truncate(c.Machine, limitShort)
	c.Name = truncate(c.Name, limitShort)
	c.Project = truncate(c.Project, limitProject)
	c.Branch = truncate(c.Branch, limitBranch)
	if !repoRe.MatchString(c.Repo) {
		c.Repo = ""
	}
	if !runRe.MatchString(c.Run) {
		c.Run = ""
	}
	if !sessionRe.MatchString(c.Session) {
		c.Session = ""
	}
	if c.Issue < 0 {
		c.Issue = 0
	}
	return c
}

func blank(s string) bool { return strings.TrimSpace(s) == "" }

// decode reads a JSON body into v. An empty body leaves v zero. It writes the
// error response and returns false on failure.
func decode(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.ContentLength > maxBody {
		writeErr(w, http.StatusRequestEntityTooLarge, "request body too large")
		return false
	}
	err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBody)).Decode(v)
	var tooBig *http.MaxBytesError
	switch {
	case err == nil, errors.Is(err, io.EOF):
		return true
	case errors.As(err, &tooBig):
		writeErr(w, http.StatusRequestEntityTooLarge, "request body too large")
	default:
		writeErr(w, http.StatusBadRequest, "invalid JSON body")
	}
	return false
}

func pathID(r *http.Request) (int64, bool) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	return id, err == nil && id > 0
}

// storeErr maps store sentinels to status codes, and bumps nothing.
func (h *Handler) storeErr(w http.ResponseWriter, r *http.Request, err error) {
	switch {
	case errors.Is(err, store.ErrNotFound):
		writeErr(w, http.StatusNotFound, err.Error())
	case errors.Is(err, store.ErrClosed), errors.Is(err, store.ErrOpenDecision):
		writeErr(w, http.StatusConflict, err.Error())
	case errors.Is(err, store.ErrNoSession), errors.Is(err, store.ErrNoRun):
		writeErr(w, http.StatusBadRequest, err.Error())
	default:
		h.internal(w, r, err)
	}
}

// commit finishes a write: on success it bumps the version and replies.
func (h *Handler) commit(w http.ResponseWriter, r *http.Request, err error, status int, resp any) {
	if err != nil {
		h.storeErr(w, r, err)
		return
	}
	h.bump()
	if status == http.StatusNoContent {
		w.WriteHeader(status)
		return
	}
	writeJSON(w, status, resp)
}

func (h *Handler) hook(w http.ResponseWriter, r *http.Request) {
	var p wire.HookPost
	if !decode(w, r, &p) {
		return
	}
	p.Ctx = cleanCtx(p.Ctx)
	if !hookEvents[p.Event] {
		writeErr(w, http.StatusBadRequest, "unknown hook event")
		return
	}
	if p.Ctx.Session == "" {
		writeErr(w, http.StatusBadRequest, "ctx.session is required")
		return
	}
	err := h.st.ApplyHook(r.Context(), p, h.nowMS(), hookstate.Apply)
	h.commit(w, r, err, http.StatusNoContent, nil)
}

func (h *Handler) ask(w http.ResponseWriter, r *http.Request) {
	var p wire.AskPost
	if !decode(w, r, &p) {
		return
	}
	c := cleanCtx(p.Ctx)
	kind := p.Kind
	if kind == "" {
		kind = "question"
	}
	switch {
	case kind != "question" && kind != "review" && kind != "notify":
		writeErr(w, http.StatusBadRequest, "kind must be question, review or notify")
		return
	case blank(p.Question):
		writeErr(w, http.StatusBadRequest, "question is required")
		return
	case kind != "notify" && blank(p.Rec):
		writeErr(w, http.StatusBadRequest, "rec is required unless kind is notify")
		return
	}
	id, err := h.st.Ask(r.Context(), c, kind, truncate(p.Question, limitText), truncate(p.Rec, limitText), h.nowMS())
	h.commit(w, r, err, http.StatusOK, wire.IDResp{ID: id})
}

func (h *Handler) answer(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	var p wire.AnswerPost
	if !decode(w, r, &p) {
		return
	}
	if blank(p.Text) {
		writeErr(w, http.StatusBadRequest, "text is required")
		return
	}
	err := h.st.Answer(r.Context(), cleanCtx(p.Ctx), id, truncate(p.Text, limitText), h.nowMS())
	h.commit(w, r, err, http.StatusOK, wire.DecisionResp{ID: id, Status: "answered"})
}

func (h *Handler) dismiss(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	var p wire.CtxPost
	if !decode(w, r, &p) {
		return
	}
	err := h.st.Dismiss(r.Context(), cleanCtx(p.Ctx), id, h.nowMS())
	h.commit(w, r, err, http.StatusOK, wire.DecisionResp{ID: id, Status: "dismissed"})
}

func (h *Handler) state(w http.ResponseWriter, r *http.Request) {
	var p wire.StatePost
	if !decode(w, r, &p) {
		return
	}
	c := cleanCtx(p.Ctx)
	if c.Session == "" {
		writeErr(w, http.StatusBadRequest, "ctx.session is required")
		return
	}
	err := h.st.SetStateNote(r.Context(), c, truncate(p.Note, limitStateNote), h.nowMS())
	h.commit(w, r, err, http.StatusOK, wire.OKResp{OK: true})
}

func (h *Handler) logLine(w http.ResponseWriter, r *http.Request) {
	var p wire.LogPost
	if !decode(w, r, &p) {
		return
	}
	if blank(p.Text) {
		writeErr(w, http.StatusBadRequest, "text is required")
		return
	}
	id, err := h.st.AppendLog(r.Context(), cleanCtx(p.Ctx), truncate(p.Text, limitText), h.nowMS())
	h.commit(w, r, err, http.StatusOK, wire.IDResp{ID: id})
}

func truncPtr(p *string, n int) *string {
	if p == nil {
		return nil
	}
	s := truncate(*p, n)
	return &s
}

func (h *Handler) item(w http.ResponseWriter, r *http.Request) {
	var p wire.ItemPost
	if !decode(w, r, &p) {
		return
	}
	switch {
	case !itemRepoRe.MatchString(p.Repo):
		writeErr(w, http.StatusBadRequest, "repo must be owner/name or name")
		return
	case p.Number < 1:
		writeErr(w, http.StatusBadRequest, "number must be at least 1")
		return
	case p.PR != nil && *p.PR < 1:
		writeErr(w, http.StatusBadRequest, "pr must be at least 1")
		return
	}
	p.Ctx = cleanCtx(p.Ctx)
	if !strings.Contains(p.Repo, "/") && p.Ctx.Repo == "" {
		writeErr(w, http.StatusBadRequest, "repo needs owner/name (no git repo here to infer the owner)")
		return
	}
	p.Title = truncPtr(p.Title, limitTitle)
	p.State = truncPtr(p.State, limitShort)
	p.Tier = truncPtr(p.Tier, limitShort)
	key, err := h.st.UpsertItem(r.Context(), p.Ctx, p, h.nowMS())
	h.commit(w, r, err, http.StatusOK, wire.ItemResp{Key: key})
}

func (h *Handler) note(w http.ResponseWriter, r *http.Request) {
	var p wire.NotePost
	if !decode(w, r, &p) {
		return
	}
	switch {
	case p.Kind != "well" && p.Kind != "less":
		writeErr(w, http.StatusBadRequest, "kind must be well or less")
		return
	case blank(p.Text):
		writeErr(w, http.StatusBadRequest, "text is required")
		return
	}
	id, err := h.st.AddNote(r.Context(), cleanCtx(p.Ctx), p.Kind, truncate(p.Text, limitText), h.nowMS())
	h.commit(w, r, err, http.StatusOK, wire.IDResp{ID: id})
}

func (h *Handler) runStart(w http.ResponseWriter, r *http.Request) {
	var p wire.RunPost
	if !decode(w, r, &p) {
		return
	}
	if !runRe.MatchString(p.Name) {
		writeErr(w, http.StatusBadRequest, "bad run name")
		return
	}
	run, err := h.st.StartRun(r.Context(), cleanCtx(p.Ctx), p.Name, h.nowMS())
	h.commit(w, r, err, http.StatusOK, run)
}

func (h *Handler) runEnd(w http.ResponseWriter, r *http.Request) {
	var p wire.RunPost
	if !decode(w, r, &p) {
		return
	}
	run, err := h.st.EndRun(r.Context(), cleanCtx(p.Ctx), truncate(p.Name, limitShort), h.nowMS())
	h.commit(w, r, err, http.StatusOK, run)
}

func (h *Handler) meter(w http.ResponseWriter, r *http.Request) {
	var p wire.MeterPost
	if !decode(w, r, &p) {
		return
	}
	p.Ctx = cleanCtx(p.Ctx)
	if !runRe.MatchString(p.Run) {
		p.Run = ""
	}
	vals := []*int{p.OpenStart, p.OpenNow, p.Filed, p.Closed}
	given := false
	for _, v := range vals {
		if v == nil {
			continue
		}
		if *v < 0 {
			writeErr(w, http.StatusBadRequest, "meter values must not be negative")
			return
		}
		given = true
	}
	switch {
	case !given:
		writeErr(w, http.StatusBadRequest, "no meter value given")
		return
	case p.Run == "" && p.Ctx.Run == "":
		writeErr(w, http.StatusBadRequest, "no run: pass run or set ctx.run")
		return
	}
	err := h.st.SetMeter(r.Context(), p.Ctx, p, h.nowMS())
	h.commit(w, r, err, http.StatusOK, wire.OKResp{OK: true})
}

func (h *Handler) prune(w http.ResponseWriter, r *http.Request) {
	var p wire.PrunePost
	if !decode(w, r, &p) {
		return
	}
	switch p.Mode {
	case "ended_all":
	case "ended_older", "quiet":
		if p.OlderThanS < 60 {
			writeErr(w, http.StatusBadRequest, "older_than_s must be at least 60")
			return
		}
	default:
		writeErr(w, http.StatusBadRequest, "mode must be ended_older, ended_all or quiet")
		return
	}
	res, err := h.st.Prune(r.Context(), p.Mode, time.Duration(p.OlderThanS)*time.Second, h.nowMS())
	h.commit(w, r, err, http.StatusOK, res)
}

func (h *Handler) uiClear(w http.ResponseWriter, r *http.Request) {
	var p wire.ClearPost
	if !decode(w, r, &p) {
		return
	}
	var older time.Duration
	switch p.Mode {
	case "ended_older":
		older = 24 * time.Hour
	case "quiet":
		older = 2 * time.Hour
	case "ended_all":
	default:
		writeErr(w, http.StatusBadRequest, "mode must be ended_older, ended_all or quiet")
		return
	}
	res, err := h.st.Prune(r.Context(), p.Mode, older, h.nowMS())
	h.commit(w, r, err, http.StatusOK, res)
}

func (h *Handler) uiDelete(w http.ResponseWriter, r *http.Request) {
	err := h.st.DeleteSession(r.Context(), r.PathValue("id"))
	h.commit(w, r, err, http.StatusOK, map[string]int{"deleted": 1})
}

func (h *Handler) uiDismiss(w http.ResponseWriter, r *http.Request) {
	id, ok := pathID(r)
	if !ok {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	err := h.st.Dismiss(r.Context(), wire.Ctx{}, id, h.nowMS())
	h.commit(w, r, err, http.StatusOK, wire.DecisionResp{ID: id, Status: "dismissed"})
}
