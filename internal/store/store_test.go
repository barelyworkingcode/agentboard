package store_test

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/barelyworkingcode/agentboard/internal/store"
	"github.com/barelyworkingcode/agentboard/internal/wire"
)

const (
	now    = int64(1_800_000_000_000)
	hour   = int64(time.Hour / time.Millisecond)
	minute = int64(time.Minute / time.Millisecond)
)

func openStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "board.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { st.Close() })
	return st
}

// putSession writes a session row whose hook state is exactly hs, through the
// store's injected state-machine seam.
func putSession(t *testing.T, st *store.Store, id string, hs wire.HookState) {
	t.Helper()
	apply := func(wire.HookState, bool, wire.HookPost, int64) (wire.HookState, *wire.Event, bool) {
		return hs, &wire.Event{At: hs.LastSeenAt, Kind: "state", State: hs.State}, true
	}
	h := wire.HookPost{Ctx: wire.Ctx{Session: id, Machine: "devbox"}, Event: "SessionStart"}
	if err := st.ApplyHook(context.Background(), h, hs.LastSeenAt, apply); err != nil {
		t.Fatalf("seed session %s: %v", id, err)
	}
}

func askFor(t *testing.T, st *store.Store, session string, at int64) int64 {
	t.Helper()
	id, err := st.Ask(context.Background(), wire.Ctx{Session: session}, "question", "Proceed?", "yes", at)
	if err != nil {
		t.Fatalf("ask for %s: %v", session, err)
	}
	return id
}

func sessionIDs(t *testing.T, st *store.Store) []string {
	t.Helper()
	b, err := st.Board(context.Background(), now)
	if err != nil {
		t.Fatalf("board: %v", err)
	}
	var ids []string
	for _, s := range b.Sessions {
		ids = append(ids, s.ID)
	}
	sort.Strings(ids)
	return ids
}

func hasLog(b wire.Board, text string) bool {
	return slices.ContainsFunc(b.Log, func(l wire.LogLine) bool { return l.Text == text })
}

// seedRetention builds one row per retention edge. Rows ending in "-open" have
// an open decision and must survive every mode.
func seedRetention(t *testing.T, st *store.Store) {
	ended := func(at int64) wire.HookState { return wire.HookState{State: "ended", LastSeenAt: at, EndedAt: at} }
	active := func(at int64, tool string) wire.HookState {
		hs := wire.HookState{State: "active", LastSeenAt: at, Tool: tool}
		if tool != "" {
			hs.ToolUseID, hs.ToolSince = "tu1", at
		}
		return hs
	}
	putSession(t, st, "ended-24h", ended(now-24*hour))
	putSession(t, st, "ended-1h", ended(now-hour))
	putSession(t, st, "ended-30h-open", ended(now-30*hour))
	askFor(t, st, "ended-30h-open", now-30*hour)
	putSession(t, st, "quiet-3h", active(now-3*hour, ""))
	putSession(t, st, "quiet-3h-open", active(now-3*hour, ""))
	askFor(t, st, "quiet-3h-open", now-3*hour)
	putSession(t, st, "quiet-30m", active(now-30*minute, ""))
	putSession(t, st, "quiet-15m", active(now-15*minute, ""))
	putSession(t, st, "tool-3h", active(now-3*hour, "Bash"))
	putSession(t, st, "idle-3h", wire.HookState{State: "idle", LastSeenAt: now - 3*hour})
	if _, err := st.AppendLog(context.Background(), wire.Ctx{Session: "ended-24h"}, "kept line", now-24*hour); err != nil {
		t.Fatalf("append log: %v", err)
	}
}

func TestPruneModes(t *testing.T) {
	all := []string{"ended-1h", "ended-24h", "ended-30h-open", "idle-3h", "quiet-15m", "quiet-30m", "quiet-3h", "quiet-3h-open", "tool-3h"}
	cases := []struct {
		mode    string
		older   time.Duration
		deleted []string
		kept    int
	}{
		{"ended_older", 24 * time.Hour, []string{"ended-24h"}, 1},
		{"ended_all", 0, []string{"ended-1h", "ended-24h"}, 1},
		{"quiet", 2 * time.Hour, []string{"quiet-3h"}, 1},
		// The quiet floor is 20 min whatever older_ms says.
		{"quiet", time.Minute, []string{"quiet-30m", "quiet-3h"}, 1},
	}
	for _, c := range cases {
		t.Run(c.mode+"/"+c.older.String(), func(t *testing.T) {
			st := openStore(t)
			seedRetention(t, st)
			res, err := st.Prune(context.Background(), c.mode, c.older, now)
			if err != nil {
				t.Fatalf("prune: %v", err)
			}
			if res.Deleted != len(c.deleted) || res.Kept != c.kept {
				t.Errorf("result = %+v, want deleted=%d kept=%d", res, len(c.deleted), c.kept)
			}
			want := slices.DeleteFunc(slices.Clone(all), func(id string) bool { return slices.Contains(c.deleted, id) })
			if got := sessionIDs(t, st); !slices.Equal(got, want) {
				t.Errorf("remaining sessions = %v, want %v", got, want)
			}
			b, _ := st.Board(context.Background(), now)
			if !hasLog(b, "kept line") || len(b.Decisions) != 2 {
				t.Errorf("prune must keep log lines and open decisions: log=%+v decisions=%d", b.Log, len(b.Decisions))
			}
		})
	}
}

func TestClearCountsExcludeOpenDecisions(t *testing.T) {
	st := openStore(t)
	seedRetention(t, st)
	got, err := st.ClearCounts(context.Background(), now)
	if err != nil {
		t.Fatalf("clear counts: %v", err)
	}
	want := wire.ClearCounts{EndedOlder24h: 1, EndedAll: 2, Quiet2h: 1}
	if got != want {
		t.Errorf("ClearCounts = %+v, want %+v", got, want)
	}
}

func TestDecisionLifecycle(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	c := wire.Ctx{Session: "s1", Machine: "devbox", Project: "relay"}
	answered := askFor(t, st, "s1", now)
	dismissed, err := st.Ask(ctx, c, "review", "Look at the diff", "merge", now+1)
	if err != nil {
		t.Fatalf("ask: %v", err)
	}

	b, _ := st.Board(ctx, now)
	if len(b.Decisions) != 2 {
		t.Fatalf("open decisions = %d, want 2", len(b.Decisions))
	}
	for _, d := range b.Decisions {
		if d.ID == dismissed && (d.Rec != "merge" || d.Kind != "review" || d.Ctx.Project != "relay" || d.Status != "open") {
			t.Errorf("decision %d = %+v, want the posted kind, rec and ctx", d.ID, d)
		}
	}

	if err := st.Answer(ctx, c, answered, "go ahead", now+2); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if err := st.Dismiss(ctx, c, dismissed, now+3); err != nil {
		t.Fatalf("dismiss: %v", err)
	}
	if b, _ := st.Board(ctx, now); len(b.Decisions) != 0 {
		t.Errorf("closed decisions still open on the board: %+v", b.Decisions)
	}
	d, err := st.SessionDetail(ctx, "s1")
	if err != nil {
		t.Fatalf("session detail: %v", err)
	}
	status := map[int64]wire.Decision{}
	for _, x := range d.Decisions {
		status[x.ID] = x
	}
	if x := status[answered]; x.Status != "answered" || x.Answer != "go ahead" {
		t.Errorf("answered decision = %+v", x)
	}
	if x := status[dismissed]; x.Status != "dismissed" {
		t.Errorf("dismissed decision = %+v", x)
	}

	errCases := []struct {
		name string
		call func() error
		want error
	}{
		{"answer twice", func() error { return st.Answer(ctx, c, answered, "again", now+4) }, store.ErrClosed},
		{"dismiss answered", func() error { return st.Dismiss(ctx, c, answered, now+4) }, store.ErrClosed},
		{"answer dismissed", func() error { return st.Answer(ctx, c, dismissed, "late", now+4) }, store.ErrClosed},
		{"answer unknown", func() error { return st.Answer(ctx, c, 9999, "x", now+4) }, store.ErrNotFound},
		{"dismiss unknown", func() error { return st.Dismiss(ctx, c, 9999, now+4) }, store.ErrNotFound},
	}
	for _, e := range errCases {
		if err := e.call(); !errors.Is(err, e.want) {
			t.Errorf("%s: err = %v, want %v", e.name, err, e.want)
		}
	}
}

func TestDeleteSession(t *testing.T) {
	ctx := context.Background()
	st := openStore(t)
	putSession(t, st, "s1", wire.HookState{State: "ended", LastSeenAt: now, EndedAt: now})
	id := askFor(t, st, "s1", now)
	if _, err := st.AppendLog(ctx, wire.Ctx{Session: "s1"}, "survives delete", now); err != nil {
		t.Fatalf("append log: %v", err)
	}

	if err := st.DeleteSession(ctx, "s1"); !errors.Is(err, store.ErrOpenDecision) {
		t.Fatalf("delete with open decision: err = %v, want ErrOpenDecision", err)
	}
	if got := sessionIDs(t, st); !slices.Equal(got, []string{"s1"}) {
		t.Fatalf("session removed despite open decision: %v", got)
	}

	if err := st.Answer(ctx, wire.Ctx{}, id, "done", now); err != nil {
		t.Fatalf("answer: %v", err)
	}
	if err := st.DeleteSession(ctx, "s1"); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := st.SessionDetail(ctx, "s1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("detail after delete: err = %v, want ErrNotFound", err)
	}
	if b, _ := st.Board(ctx, now); !hasLog(b, "survives delete") {
		t.Errorf("delete must keep the session's log lines, got %+v", b.Log)
	}
	if err := st.DeleteSession(ctx, "s1"); !errors.Is(err, store.ErrNotFound) {
		t.Errorf("delete unknown: err = %v, want ErrNotFound", err)
	}
}

func upsert(t *testing.T, st *store.Store, c wire.Ctx, p wire.ItemPost, at int64) string {
	t.Helper()
	p.Ctx = c
	key, err := st.UpsertItem(context.Background(), c, p, at)
	if err != nil {
		t.Fatalf("upsert %+v: %v", p, err)
	}
	return key
}

func items(t *testing.T, st *store.Store) map[string]wire.Item {
	t.Helper()
	b, err := st.Board(context.Background(), now)
	if err != nil {
		t.Fatalf("board: %v", err)
	}
	m := map[string]wire.Item{}
	for _, it := range b.Items {
		m[fmt.Sprintf("%s#%d", it.Repo, it.Number)] = it
	}
	return m
}

func sp(s string) *string { return &s }
func ip(n int) *int       { return &n }

func TestUpsertItemByPR(t *testing.T) {
	c := wire.Ctx{Machine: "devbox", Repo: "acme/widgets", Session: "s1"}
	seed := func(t *testing.T) *store.Store {
		st := openStore(t)
		upsert(t, st, c, wire.ItemPost{Repo: "acme/widgets", Number: 7, Title: sp("Fix the widget"), PR: ip(31), State: sp("PR open")}, now)
		upsert(t, st, c, wire.ItemPost{Repo: "acme/gadgets", Number: 40, Title: sp("Other repo"), PR: ip(31), State: sp("PR open")}, now+1)
		return st
	}

	t.Run("matches the row with that pr in the same repo", func(t *testing.T) {
		st := seed(t)
		key := upsert(t, st, c, wire.ItemPost{Repo: "acme/widgets", Number: 0, PR: ip(31), State: sp("merged")}, now+2)
		if key != "acme/widgets#7" {
			t.Errorf("key = %q, want acme/widgets#7", key)
		}
		m := items(t, st)
		if len(m) != 2 {
			t.Fatalf("items = %+v, want the 2 seeded rows only", m)
		}
		if it := m["acme/widgets#7"]; it.State != "merged" || it.PR != 31 || it.Title != "Fix the widget" {
			t.Errorf("acme/widgets#7 = %+v, want state merged, pr 31, title kept", it)
		}
		if it := m["acme/gadgets#40"]; it.State != "PR open" {
			t.Errorf("acme/gadgets#40 = %+v, a row in another repo with the same pr must not change", it)
		}
	})

	t.Run("bare repo resolves through ctx.repo", func(t *testing.T) {
		st := seed(t)
		key := upsert(t, st, c, wire.ItemPost{Repo: "widgets", Number: 0, PR: ip(31), State: sp("merged")}, now+2)
		if key != "acme/widgets#7" {
			t.Errorf("key = %q, want acme/widgets#7", key)
		}
		if it := items(t, st)["acme/widgets#7"]; it.State != "merged" {
			t.Errorf("acme/widgets#7 = %+v, want merged", it)
		}
	})

	t.Run("creates (repo, pr) when no row has that pr", func(t *testing.T) {
		st := seed(t)
		key := upsert(t, st, c, wire.ItemPost{Repo: "acme/widgets", Number: 0, PR: ip(55), State: sp("merged")}, now+2)
		if key != "acme/widgets#55" {
			t.Errorf("key = %q, want acme/widgets#55", key)
		}
		m := items(t, st)
		if len(m) != 3 {
			t.Errorf("items = %+v, want 3 rows", m)
		}
		if it, ok := m["acme/widgets#55"]; !ok || it.PR != 55 || it.State != "merged" {
			t.Errorf("acme/widgets#55 = %+v (found %v), want pr 55 state merged", it, ok)
		}
		if it := m["acme/widgets#7"]; it.State != "PR open" {
			t.Errorf("acme/widgets#7 = %+v, must not change", it)
		}
	})

	t.Run("a pr only in another repo is not matched", func(t *testing.T) {
		st := openStore(t)
		upsert(t, st, c, wire.ItemPost{Repo: "acme/gadgets", Number: 40, PR: ip(31), State: sp("PR open")}, now)
		key := upsert(t, st, c, wire.ItemPost{Repo: "acme/widgets", Number: 0, PR: ip(31), State: sp("merged")}, now+1)
		if key != "acme/widgets#31" {
			t.Errorf("key = %q, want acme/widgets#31", key)
		}
		m := items(t, st)
		if it := m["acme/gadgets#40"]; it.State != "PR open" {
			t.Errorf("acme/gadgets#40 = %+v, must not change", it)
		}
		if it, ok := m["acme/widgets#31"]; !ok || it.PR != 31 || it.State != "merged" {
			t.Errorf("acme/widgets#31 = %+v (found %v), want created with pr 31 merged", it, ok)
		}
	})

	t.Run("several rows with that pr: the most recently updated", func(t *testing.T) {
		st := seed(t)
		upsert(t, st, c, wire.ItemPost{Repo: "acme/widgets", Number: 9, PR: ip(31), State: sp("PR open")}, now+5)
		key := upsert(t, st, c, wire.ItemPost{Repo: "acme/widgets", Number: 0, PR: ip(31), State: sp("merged")}, now+6)
		if key != "acme/widgets#9" {
			t.Errorf("key = %q, want acme/widgets#9", key)
		}
		m := items(t, st)
		if m["acme/widgets#9"].State != "merged" || m["acme/widgets#7"].State != "PR open" {
			t.Errorf("items = %+v, want only #9 merged", m)
		}
	})
}

func TestUpsertItemEmptyTitleKeepsTitle(t *testing.T) {
	c := wire.Ctx{Machine: "devbox", Repo: "acme/widgets"}
	st := openStore(t)
	upsert(t, st, c, wire.ItemPost{Repo: "acme/widgets", Number: 7, Title: sp("Fix the widget"), State: sp("open")}, now)
	for i, p := range []wire.ItemPost{
		{Repo: "acme/widgets", Number: 7, State: sp("in progress")},                       // nil title
		{Repo: "acme/widgets", Number: 7, Title: sp(""), State: sp("in review")},          // empty title
		{Repo: "acme/widgets", Number: 7, PR: ip(31), State: sp("PR open")},               // parsed pr create
		{Repo: "acme/widgets", Number: 0, PR: ip(31), Title: sp(""), State: sp("merged")}, // parsed merge by pr
	} {
		upsert(t, st, c, p, now+int64(i)+1)
		if it := items(t, st)["acme/widgets#7"]; it.Title != "Fix the widget" || it.State != *p.State {
			t.Errorf("after write %d: item = %+v, want title kept and state %q", i, it, *p.State)
		}
	}
}

func TestUpsertItemLatestWriteWins(t *testing.T) {
	c := wire.Ctx{Machine: "devbox", Repo: "acme/widgets"}
	st := openStore(t)
	steps := []struct {
		name      string
		p         wire.ItemPost
		wantTitle string
		wantState string
	}{
		{"parsed issue create", wire.ItemPost{Repo: "acme/widgets", Number: 7, Title: sp("Parsed title"), State: sp("open")}, "Parsed title", "open"},
		{"explicit title and state", wire.ItemPost{Repo: "acme/widgets", Number: 7, Title: sp("Explicit title"), State: sp("blocked")}, "Explicit title", "blocked"},
		{"parsed pr create", wire.ItemPost{Repo: "acme/widgets", Number: 7, PR: ip(31), State: sp("PR open")}, "Explicit title", "PR open"},
		{"explicit state", wire.ItemPost{Repo: "acme/widgets", Number: 7, State: sp("waiting on review")}, "Explicit title", "waiting on review"},
		{"parsed merge by pr", wire.ItemPost{Repo: "acme/widgets", Number: 0, PR: ip(31), State: sp("merged")}, "Explicit title", "merged"},
		{"explicit after the merge", wire.ItemPost{Repo: "acme/widgets", Number: 7, Title: sp("Final"), State: sp("merged, reviewed")}, "Final", "merged, reviewed"},
	}
	for i, s := range steps {
		upsert(t, st, c, s.p, now+int64(i))
		it := items(t, st)["acme/widgets#7"]
		if it.Title != s.wantTitle || it.State != s.wantState || (i >= 2 && it.PR != 31) {
			t.Errorf("after %s: item = %+v, want title %q state %q", s.name, it, s.wantTitle, s.wantState)
		}
	}
}
