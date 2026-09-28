package integration

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"slices"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/barelyworkingcode/agentboard/internal/wire"
)

// bashHook is a Bash hook payload shaped like the captured Claude Code ones:
// PostToolUse carries tool_response (no exit code); PostToolUseFailure carries
// error and no tool_response.
func bashHook(session, cwd, event, command, stdout string, interrupted bool) string {
	m := map[string]any{
		"session_id":      session,
		"transcript_path": "/tmp/transcript.jsonl",
		"cwd":             cwd,
		"prompt_id":       "prompt-1",
		"permission_mode": "default",
		"hook_event_name": event,
		"tool_name":       "Bash",
		"tool_input":      map[string]any{"command": command, "description": "Run gh"},
		"tool_use_id":     "toolu_01",
		"duration_ms":     451,
	}
	switch event {
	case "PostToolUse":
		m["tool_response"] = map[string]any{"stdout": stdout, "stderr": "", "interrupted": interrupted, "isImage": false, "noOutputExpected": false}
	case "PostToolUseFailure":
		m["error"] = "Exit code 1\nrequest failed"
		m["is_interrupt"] = false
	}
	b, _ := json.Marshal(m)
	return string(b)
}

func findItem(b wire.Board, repo string, number int) (wire.Item, bool) {
	for _, it := range b.Items {
		if it.Repo == repo && it.Number == number {
			return it, true
		}
	}
	return wire.Item{}, false
}

func itemKeys(b wire.Board) []string {
	var keys []string
	for _, it := range b.Items {
		keys = append(keys, fmt.Sprintf("%s#%d", it.Repo, it.Number))
	}
	sort.Strings(keys)
	return keys
}

func TestItemsByPR(t *testing.T) {
	s := startHandler(t, true, loopbackAllow)
	pr31 := 31
	mustPost(t, s.url+"/api/items", wire.ItemPost{Repo: "acme/widgets", Number: 7, PR: &pr31, State: ptr("PR open"), Title: ptr("Fix the widget")})
	mustPost(t, s.url+"/api/items", wire.ItemPost{Repo: "acme/gadgets", Number: 40, PR: &pr31, State: ptr("PR open")})

	ok := []struct {
		name    string
		p       wire.ItemPost
		wantKey string
	}{
		{"number 0, pr matches a row", wire.ItemPost{Repo: "acme/widgets", Number: 0, PR: &pr31, State: ptr("merged")}, "acme/widgets#7"},
		{"number 0, pr matches no row", wire.ItemPost{Repo: "acme/widgets", Number: 0, PR: ptrInt(44), State: ptr("merged")}, "acme/widgets#44"},
	}
	for _, c := range ok {
		code, body := post(t, s.url+"/api/items", c.p)
		var got wire.ItemResp
		if code != 200 || json.Unmarshal(body, &got) != nil || got.Key != c.wantKey {
			t.Errorf("%s: POST /api/items = %d %s, want 200 key %q", c.name, code, body, c.wantKey)
		}
	}
	b, _ := board(t, s.url)
	if it, _ := findItem(b, "acme/widgets", 7); it.State != "merged" || it.PR != 31 || it.Title != "Fix the widget" {
		t.Errorf("acme/widgets#7 = %+v, want merged with pr 31 and its title", it)
	}
	if it, _ := findItem(b, "acme/gadgets", 40); it.State != "PR open" {
		t.Errorf("acme/gadgets#40 = %+v, another repo's row must not change", it)
	}
	if it, ok := findItem(b, "acme/widgets", 44); !ok || it.PR != 44 || it.State != "merged" {
		t.Errorf("acme/widgets#44 = %+v (found %v), want created with pr 44", it, ok)
	}
	before := itemKeys(b)

	bad := []struct {
		name string
		p    wire.ItemPost
	}{
		{"number 0 without pr", wire.ItemPost{Repo: "acme/widgets", Number: 0, State: ptr("merged")}},
		{"number 0 with pr 0", wire.ItemPost{Repo: "acme/widgets", Number: 0, PR: ptrInt(0), State: ptr("merged")}},
		{"number 0 with pr -1", wire.ItemPost{Repo: "acme/widgets", Number: 0, PR: ptrInt(-1), State: ptr("merged")}},
		{"number -1", wire.ItemPost{Repo: "acme/widgets", Number: -1, State: ptr("merged")}},
		{"number -1 with pr", wire.ItemPost{Repo: "acme/widgets", Number: -1, PR: &pr31, State: ptr("merged")}},
	}
	for _, c := range bad {
		code, body := post(t, s.url+"/api/items", c.p)
		var e wire.ErrorResp
		if code != 400 || json.Unmarshal(body, &e) != nil || e.Error == "" {
			t.Errorf("%s: POST /api/items = %d %s, want 400 with an error body", c.name, code, body)
		}
	}
	b, _ = board(t, s.url)
	if after := itemKeys(b); !slices.Equal(after, before) {
		t.Errorf("items after refused posts = %v, want %v", after, before)
	}
	if it, _ := findItem(b, "acme/widgets", 7); it.State != "merged" {
		t.Errorf("acme/widgets#7 = %+v, a refused post changed it", it)
	}
}

func TestItemsEmptyTitleKeepsTitle(t *testing.T) {
	s := startHandler(t, true, loopbackAllow)
	mustPost(t, s.url+"/api/items", wire.ItemPost{Repo: "acme/widgets", Number: 7, Title: ptr("Fix the widget"), PR: ptrInt(31)})
	mustPost(t, s.url+"/api/items", wire.ItemPost{Repo: "acme/widgets", Number: 7, Title: ptr(""), State: ptr("in review")})
	mustPost(t, s.url+"/api/items", wire.ItemPost{Repo: "acme/widgets", Number: 0, PR: ptrInt(31), State: ptr("merged")})
	b, _ := board(t, s.url)
	if it, _ := findItem(b, "acme/widgets", 7); it.Title != "Fix the widget" || it.State != "merged" {
		t.Errorf("acme/widgets#7 = %+v, want title kept and state merged", it)
	}
}

func ptr(s string) *string { return &s }
func ptrInt(n int) *int    { return &n }

// hookRun execs `agentboard hook` and checks the invariant every hook call
// keeps: exit 0 and nothing on stdout.
func hookRun(t *testing.T, dir, stdin string, env []string) {
	t.Helper()
	r := runBin(t, dir, stdin, env, "hook")
	if r.code != 0 || r.stdout != "" {
		t.Fatalf("hook: exit=%d stdout=%q stderr=%q, want exit 0 and no stdout", r.code, r.stdout, r.stderr)
	}
}

func waitItem(t *testing.T, base string, deadline time.Time, repo string, number int, ok func(wire.Item) bool) {
	t.Helper()
	var last wire.Board
	waitFor(t, time.Until(deadline), func() bool {
		last, _ = board(t, base)
		it, found := findItem(last, repo, number)
		return found && ok(it)
	}, func() string { return fmt.Sprintf("%s#%d; items = %+v", repo, number, last.Items) })
}

func TestHookUpdatesLedgerFromGh(t *testing.T) {
	base, _ := startServe(t)
	repo := gitRepo(t) // origin acme/relay, branch feat/88-x → issue 88
	env := childEnv("AB_URL="+base, "AB_MACHINE=devbox", "AB_NAME=p1")
	const sess = "sess-ledger-1"

	// gh issue create → acme/relay#5 with the title, state open, within 2 s.
	issueCmd := `gh issue create --title "Fix the widget" --body "The widget spins forever."`
	deadline := time.Now().Add(2 * time.Second)
	hookRun(t, repo, bashHook(sess, repo, "PostToolUse", issueCmd, "https://github.com/acme/relay/issues/5\n", false), env)
	waitItem(t, base, deadline, "acme/relay", 5, func(it wire.Item) bool {
		return it.Title == "Fix the widget" && it.State == "open" && it.PR == 0
	})

	// An explicit item after the parse wins.
	if r := runBin(t, repo, "", env, "item", "acme/relay#5", "--title", "T2"); r.code != 0 || r.stdout != "acme/relay#5\n" {
		t.Fatalf("item: exit=%d stdout=%q stderr=%q", r.code, r.stdout, r.stderr)
	}
	b, _ := board(t, base)
	if it, _ := findItem(b, "acme/relay", 5); it.Title != "T2" || it.State != "open" {
		t.Errorf("after explicit item: acme/relay#5 = %+v, want title T2 state open", it)
	}

	// A failed or interrupted pr create changes nothing.
	prCmd := `gh pr create --title "Add the widget" --body "Closes #88"`
	pull := "https://github.com/acme/relay/pull/90\n"
	hookRun(t, repo, bashHook(sess, repo, "PostToolUseFailure", prCmd, "", false), env)
	hookRun(t, repo, bashHook(sess, repo, "PostToolUse", prCmd, pull, true), env)
	b, _ = board(t, base)
	if keys := itemKeys(b); !slices.Equal(keys, []string{"acme/relay#5"}) {
		t.Fatalf("after failed/interrupted pr create: items = %v, want only acme/relay#5", keys)
	}

	// gh pr create on feat/88-x → the branch issue gets the PR and "PR open".
	deadline = time.Now().Add(2 * time.Second)
	hookRun(t, repo, bashHook(sess, repo, "PostToolUse", prCmd, pull, false), env)
	waitItem(t, base, deadline, "acme/relay", 88, func(it wire.Item) bool {
		return it.PR == 90 && it.State == "PR open"
	})
	b, _ = board(t, base)
	if it, _ := findItem(b, "acme/relay", 88); it.Title != "" {
		t.Errorf("acme/relay#88 title = %q, pr create under a branch issue sets no title", it.Title)
	}
	if _, ok := findItem(b, "acme/relay", 90); ok {
		t.Errorf("pr create under a branch issue also created acme/relay#90: %+v", b.Items)
	}

	// An explicit state, then a failed merge: the explicit state stays.
	if r := runBin(t, repo, "", env, "item", "acme/relay#88", "--state", "in review"); r.code != 0 {
		t.Fatalf("item: exit=%d stderr=%q", r.code, r.stderr)
	}
	mergeCmd := `gh pr merge --squash --delete-branch`
	hookRun(t, repo, bashHook(sess, repo, "PostToolUseFailure", mergeCmd, "", false), env)
	b, _ = board(t, base)
	if it, _ := findItem(b, "acme/relay", 88); it.State != "in review" {
		t.Errorf("after a failed merge: acme/relay#88 = %+v, want state still in review", it)
	}

	// gh pr merge with no argument → the branch issue is merged (latest write).
	deadline = time.Now().Add(2 * time.Second)
	hookRun(t, repo, bashHook(sess, repo, "PostToolUse", mergeCmd, "", false), env)
	waitItem(t, base, deadline, "acme/relay", 88, func(it wire.Item) bool {
		return it.State == "merged" && it.PR == 90
	})
	b, _ = board(t, base)
	if it, _ := findItem(b, "acme/relay", 5); it.Title != "T2" || it.State != "open" {
		t.Errorf("acme/relay#5 = %+v, the merge must not touch it", it)
	}
	if keys := itemKeys(b); !slices.Equal(keys, []string{"acme/relay#5", "acme/relay#88"}) {
		t.Errorf("items = %v, want acme/relay#5 and acme/relay#88", keys)
	}
}

func TestHookFailureEventChangesNothing(t *testing.T) {
	base, _ := startServe(t)
	repo := gitRepo(t)
	env := childEnv("AB_URL="+base, "AB_MACHINE=devbox")
	if r := runBin(t, repo, "", env, "item", "acme/relay#88", "--pr", "90", "--state", "PR open"); r.code != 0 {
		t.Fatalf("seed item: exit=%d stderr=%q", r.code, r.stderr)
	}
	// A no-argument merge needs no stdout: parsed on branch feat/88-x it would
	// mark acme/relay#88 merged. A failure event must not be parsed.
	hookRun(t, repo, bashHook("sess-ledger-2", repo, "PostToolUseFailure", `gh pr merge --squash`, "", false), env)
	b, _ := board(t, base)
	if keys := itemKeys(b); !slices.Equal(keys, []string{"acme/relay#88"}) {
		t.Errorf("items after PostToolUseFailure = %v, want only acme/relay#88", keys)
	}
	if it, _ := findItem(b, "acme/relay", 88); it.State != "PR open" || it.PR != 90 {
		t.Errorf("acme/relay#88 = %+v, want state still PR open with pr 90", it)
	}
	if _, ok := session(b, "sess-ledger-2"); !ok {
		t.Errorf("the /api/hook post must still happen: sessions = %+v", b.Sessions)
	}
}

func TestHookMergeByNumberResolvesThroughPR(t *testing.T) {
	base, _ := startServe(t)
	repo := gitRepo(t)
	env := childEnv("AB_URL="+base, "AB_MACHINE=devbox")
	const sess = "sess-ledger-3"
	hookRun(t, repo, bashHook(sess, repo, "PostToolUse", `gh pr create --fill`, "https://github.com/acme/relay/pull/90\n", false), env)
	waitItem(t, base, time.Now().Add(2*time.Second), "acme/relay", 88, func(it wire.Item) bool { return it.PR == 90 })

	// From a directory with no git repo: the repo comes from --repo, and the
	// PR number finds the branch issue's row.
	elsewhere := t.TempDir()
	deadline := time.Now().Add(2 * time.Second)
	hookRun(t, elsewhere, bashHook(sess, elsewhere, "PostToolUse", `gh pr merge 90 --repo acme/relay --squash`, "", false), env)
	waitItem(t, base, deadline, "acme/relay", 88, func(it wire.Item) bool { return it.State == "merged" })
	b, _ := board(t, base)
	if keys := itemKeys(b); !slices.Equal(keys, []string{"acme/relay#88"}) {
		t.Errorf("items = %v, want only acme/relay#88", keys)
	}
}

type recorded struct {
	path string
	body string
}

// TestHookSendsOnlyParsedLabels checks the privacy rule: the raw command and
// stdout never leave the hook process; only the derived item fields do.
func TestHookSendsOnlyParsedLabels(t *testing.T) {
	var mu sync.Mutex
	var reqs []recorded
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		reqs = append(reqs, recorded{r.URL.Path, string(b)})
		mu.Unlock()
		if r.URL.Path == "/api/items" {
			w.Header().Set("Content-Type", "application/json")
			w.Write([]byte(`{"key":"acme/relay#5"}`))
			return
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	t.Cleanup(srv.Close)

	repo := gitRepo(t)
	env := childEnv("AB_URL="+srv.URL, "AB_MACHINE=devbox")
	const (
		bodyMarker   = "MARKER-BODY-7f3a1c"
		stdoutMarker = "MARKER-STDOUT-91c2e4"
		descMarker   = "MARKER-DESC-5b0d"
	)
	command := `gh issue create --title "Fix the widget" --body "` + bodyMarker + ` see https://github.com/acme/relay/issues/1"`
	stdout := stdoutMarker + "\nhttps://github.com/acme/relay/issues/5\n"
	payload := bashHook("sess-private", repo, "PostToolUse", command, stdout, false)
	payload = strings.Replace(payload, `"Run gh"`, `"`+descMarker+`"`, 1)

	r := runBin(t, repo, payload, env, "hook")
	if r.code != 0 || r.stdout != "" {
		t.Fatalf("hook: exit=%d stdout=%q stderr=%q, want exit 0 and no stdout", r.code, r.stdout, r.stderr)
	}

	mu.Lock()
	got := slices.Clone(reqs)
	mu.Unlock()
	var paths []string
	for _, q := range got {
		paths = append(paths, q.path)
	}
	if !slices.Equal(paths, []string{"/api/hook", "/api/items"}) {
		t.Fatalf("requests = %v, want /api/hook then /api/items", paths)
	}

	forbidden := []string{bodyMarker, stdoutMarker, descMarker, command, stdout, "gh issue create", "github.com", "issues/5", "issues/1",
		"tool_input", "tool_response", "stdout", "transcript", repo}
	for _, q := range got {
		for _, f := range forbidden {
			if strings.Contains(q.body, f) {
				t.Errorf("%s body contains %q: %s", q.path, f, q.body)
			}
		}
	}

	var h wire.HookPost
	if err := json.Unmarshal([]byte(got[0].body), &h); err != nil || h.Event != "PostToolUse" || h.Tool != "Bash" || h.Ctx.Session != "sess-private" {
		t.Errorf("/api/hook body = %s (err %v), want a PostToolUse Bash post for sess-private", got[0].body, err)
	}

	var fields map[string]json.RawMessage
	if err := json.Unmarshal([]byte(got[1].body), &fields); err != nil {
		t.Fatalf("/api/items body %s: %v", got[1].body, err)
	}
	var keys []string
	for k := range fields {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	if !slices.Equal(keys, []string{"ctx", "number", "repo", "state", "title"}) {
		t.Errorf("/api/items fields = %v, want ctx, number, repo, state, title", keys)
	}
	var it wire.ItemPost
	if err := json.Unmarshal([]byte(got[1].body), &it); err != nil {
		t.Fatalf("/api/items body %s: %v", got[1].body, err)
	}
	if it.Repo != "acme/relay" || it.Number != 5 || it.Title == nil || *it.Title != "Fix the widget" ||
		it.State == nil || *it.State != "open" || it.PR != nil || it.Tier != nil {
		t.Errorf("/api/items body = %s, want repo acme/relay, number 5, title \"Fix the widget\", state open", got[1].body)
	}
	if it.Ctx.Session != "sess-private" || it.Ctx.Repo != "acme/relay" || it.Ctx.Issue != 88 || it.Ctx.Machine != "devbox" {
		t.Errorf("/api/items ctx = %+v, want the session's gathered ctx", it.Ctx)
	}
}
