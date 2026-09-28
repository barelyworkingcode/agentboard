package integration

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"
)

func gitRepo(t *testing.T) string {
	t.Helper()
	repo := filepath.Join(t.TempDir(), "relay")
	os.Mkdir(repo, 0o755)
	for _, args := range [][]string{
		{"init", "-q", "-b", "main"},
		{"remote", "add", "origin", "git@github.com:acme/relay.git"},
		{"checkout", "-q", "-b", "feat/88-x"},
	} {
		cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
		cmd.Env = childEnv()
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
	return repo
}

func hookJSON(session, cwd, event string) string {
	b, _ := json.Marshal(map[string]string{
		"session_id": session, "cwd": cwd, "hook_event_name": event, "source": "startup",
		"transcript_path": "/tmp/transcript.jsonl", "prompt": "never sent",
	})
	return string(b)
}

func TestHookSessionStartFillsBoard(t *testing.T) {
	base, _ := startServe(t)
	repo := gitRepo(t)
	env := childEnv("AB_URL="+base, "AB_MACHINE=devbox", "AB_NAME=p1")

	r := runBin(t, repo, hookJSON("sess-hook-1", repo, "SessionStart"), env, "hook")
	if r.code != 0 || r.stdout != "" {
		t.Fatalf("hook: exit=%d stdout=%q stderr=%q, want exit 0 and no stdout", r.code, r.stdout, r.stderr)
	}
	b, _ := board(t, base)
	s, ok := session(b, "sess-hook-1")
	if !ok {
		t.Fatalf("session missing from board: %+v", b.Sessions)
	}
	c := s.Ctx
	if c.Machine != "devbox" || c.Project != "relay" || c.Branch != "feat/88-x" || c.Repo != "acme/relay" || c.Issue != 88 || c.Name != "p1" {
		t.Errorf("ctx = %+v, want devbox relay feat/88-x acme/relay #88 p1", c)
	}
	if s.State != "idle" {
		t.Errorf("state = %q, want idle", s.State)
	}
}

func TestCLIAskAnswerExitCodes(t *testing.T) {
	base, _ := startServe(t)
	env := childEnv("AB_URL="+base, "AB_MACHINE=devbox", "CLAUDE_CODE_SESSION_ID=sess-cli-1")
	dir := t.TempDir()

	ask := runBin(t, dir, "", env, "ask", "Ship", "it?", "--rec", "yes, CI is green")
	if ask.code != 0 || !regexp.MustCompile(`^\d+\n$`).MatchString(ask.stdout) {
		t.Fatalf("ask: exit=%d stdout=%q stderr=%q, want the id alone", ask.code, ask.stdout, ask.stderr)
	}
	id := strings.TrimSpace(ask.stdout)
	b, _ := board(t, base)
	if len(b.Decisions) != 1 || b.Decisions[0].Rec != "yes, CI is green" || b.Decisions[0].Question != "Ship it?" {
		t.Fatalf("board decisions = %+v", b.Decisions)
	}

	cases := []struct {
		name   string
		args   []string
		code   int
		stdout string
		stderr string
	}{
		{"answer", []string{"answer", id, "go"}, 0, "answered " + id + "\n", ""},
		{"answer closed", []string{"answer", id, "again"}, 1, "", "(409)"},
		{"dismiss closed", []string{"dismiss", id}, 1, "", "(409)"},
		{"answer unknown", []string{"answer", "999999", "x"}, 1, "", "(404)"},
	}
	for _, c := range cases {
		r := runBin(t, dir, "", env, c.args...)
		if r.code != c.code || r.stdout != c.stdout || !strings.HasSuffix(strings.TrimSpace(r.stderr), c.stderr) {
			t.Errorf("%s: exit=%d stdout=%q stderr=%q, want exit=%d stdout=%q stderr …%s", c.name, r.code, r.stdout, r.stderr, c.code, c.stdout, c.stderr)
		}
	}
	if b, _ := board(t, base); len(b.Decisions) != 0 {
		t.Errorf("answered decision still open: %+v", b.Decisions)
	}
}

func TestFailSoft(t *testing.T) {
	closed := freePort(t)

	hang, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { hang.Close() })
	go func() {
		var held []net.Conn // kept reachable so no finalizer closes them early
		for {
			c, err := hang.Accept()
			if err != nil {
				for _, c := range held {
					c.Close()
				}
				return
			}
			held = append(held, c)
		}
	}()
	status := func(code int) string {
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.WriteHeader(code)
			w.Write([]byte(`{"error":"refused"}`))
		}))
		t.Cleanup(srv.Close)
		return srv.URL
	}
	targets := []struct {
		name  string
		url   string
		timed bool
	}{
		{"closed port", "http://" + closed, true},
		{"never responds", "http://" + hang.Addr().String(), true},
		{"403", status(403), false},
		{"500", status(500), false},
	}
	dir := t.TempDir()
	calls := []struct {
		name, stdin string
		args        []string
	}{
		{"log", "", []string{"log", "hello"}},
		{"hook SessionStart", hookJSON("sess-soft", dir, "SessionStart"), []string{"hook"}},
		{"hook UserPromptSubmit", hookJSON("sess-soft", dir, "UserPromptSubmit"), []string{"hook"}},
	}
	for _, tg := range targets {
		for _, c := range calls {
			env := childEnv("AB_URL="+tg.url, "AB_MACHINE=devbox", "CLAUDE_CODE_SESSION_ID=sess-soft")
			r := runBin(t, dir, c.stdin, env, c.args...)
			if r.code != 0 || r.stdout != "" || strings.Count(r.stderr, "\n") != 1 {
				t.Errorf("%s / %s: exit=%d stdout=%q stderr=%q, want exit 0, no stdout, one stderr line", tg.name, c.name, r.code, r.stdout, r.stderr)
			}
			if tg.timed && r.elapsed >= 300*time.Millisecond {
				t.Errorf("%s / %s took %v, want < 300 ms", tg.name, c.name, r.elapsed)
			}
		}
	}
}

func TestCLIItemBareRepo(t *testing.T) {
	cases := []struct {
		name     string
		dir      string
		code     int
		stdout   string
		stderr   string
		wantRepo string
	}{
		{"outside a git repo", t.TempDir(), 1, "", "repo needs owner/name", ""},
		{"inside a repo with origin acme/relay", gitRepo(t), 0, "acme/eve#5\n", "", "acme/eve"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			base, _ := startServe(t)
			env := childEnv("AB_URL="+base, "AB_MACHINE=devbox")
			r := runBin(t, c.dir, "", env, "item", "eve#5", "--pr", "6")
			if r.code != c.code || r.stdout != c.stdout || !strings.Contains(r.stderr, c.stderr) {
				t.Errorf("exit=%d stdout=%q stderr=%q, want exit=%d stdout=%q stderr containing %q", r.code, r.stdout, r.stderr, c.code, c.stdout, c.stderr)
			}
			b, _ := board(t, base)
			if c.wantRepo == "" {
				if len(b.Items) != 0 {
					t.Errorf("refused item was stored: %+v", b.Items)
				}
				return
			}
			if len(b.Items) != 1 || b.Items[0].Repo != c.wantRepo || b.Items[0].Number != 5 || b.Items[0].PR != 6 {
				t.Errorf("board items = %+v, want one %s#5 with pr 6", b.Items, c.wantRepo)
			}
		})
	}
}
