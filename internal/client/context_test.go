package client_test

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/agentboard/internal/client"
	"github.com/barelyworkingcode/agentboard/internal/wire"
)

func TestParseRemote(t *testing.T) {
	cases := map[string]string{
		"git@github.com:acme/relay.git":                      "acme/relay",
		"ssh://git@github.com/acme/relay.git":                "acme/relay",
		"ssh://git@github.com:22/acme/relay":                 "acme/relay",
		"https://github.com/acme/relay":                      "acme/relay",
		"https://github.com/acme/relay.git/":                 "acme/relay",
		"https://x-access-token:T@github.com/acme/relay.git": "acme/relay",
		"https://GitHub.COM/acme/relay":                      "acme/relay",
		"https://github.com/acme/re.lay_x-1":                 "acme/re.lay_x-1",
		"https://gitlab.com/acme/relay.git":                  "",
		"git@bitbucket.org:acme/relay.git":                   "",
		"https://github.com.example/acme/relay":              "",
		"https://github.com/acme":                            "",
		"https://github.com/acme/relay/tree/main":            "",
		"": "",
	}
	for in, want := range cases {
		if got := client.ParseRemote(in); got != want {
			t.Errorf("ParseRemote(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestIssueFromBranch(t *testing.T) {
	cases := map[string]int{
		"feat/88-x":                   88,
		"fix/7-crash":                 7,
		"chore/12-deps":               12,
		"feat/88":                     0,
		"feature/88-x":                0,
		"acme/feat/88-x":              0,
		"feat/x-88":                   0,
		"main":                        0,
		"":                            0,
		"feat/99999999999999999999-x": 0,
	}
	for in, want := range cases {
		if got := client.IssueFromBranch(in); got != want {
			t.Errorf("IssueFromBranch(%q) = %d, want %d", in, got, want)
		}
	}
}

func git(t *testing.T, dir string, args ...string) {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", dir, "-c", "user.name=p1", "-c", "user.email=p1@example.invalid"}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1")
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("git %v: %v\n%s", args, err, out)
	}
}

func TestGather(t *testing.T) {
	root := t.TempDir()
	repo := filepath.Join(root, "relay")
	if err := os.MkdirAll(filepath.Join(repo, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	git(t, repo, "remote", "add", "origin", "git@github.com:acme/relay.git")
	git(t, repo, "checkout", "-q", "-b", "feat/88-x")
	worktree := filepath.Join(root, "wt-other")
	git(t, repo, "worktree", "add", "-q", "-b", "fix/5-y", worktree)
	detached := filepath.Join(root, "wt-detached")
	git(t, repo, "worktree", "add", "-q", "--detach", detached)
	plain := filepath.Join(root, "plain")
	if err := os.Mkdir(plain, 0o755); err != nil {
		t.Fatal(err)
	}

	env := client.Env{Machine: "devbox", Name: "p1", Run: "r1", SessionID: "sess-env"}
	full := wire.Ctx{Machine: "devbox", Project: "relay", Branch: "feat/88-x", Repo: "acme/relay", Issue: 88, Session: "sess-env", Name: "p1", Run: "r1"}
	with := func(f func(*wire.Ctx)) wire.Ctx { c := full; f(&c); return c }
	host, _ := os.Hostname()
	host, _, _ = strings.Cut(host, ".")

	cases := []struct {
		name    string
		dir     string
		env     client.Env
		session string
		origin  string
		want    wire.Ctx
	}{
		{"ssh origin from a subdirectory", filepath.Join(repo, "sub"), env, "", "", full},
		{"https origin", repo, env, "", "https://github.com/acme/relay.git", full},
		{"hook session overrides env", repo, env, "sess-hook", "", with(func(c *wire.Ctx) { c.Session = "sess-hook" })},
		{"worktree reports the main repo", worktree, env, "", "", with(func(c *wire.Ctx) { c.Branch, c.Issue = "fix/5-y", 5 })},
		{"detached head", detached, env, "", "", with(func(c *wire.Ctx) { c.Branch, c.Issue = "", 0 })},
		{"not a repo", plain, env, "", "", wire.Ctx{Machine: "devbox", Session: "sess-env", Name: "p1", Run: "r1"}},
		{"name and machine fallbacks", plain, client.Env{SessionID: "abcdef123456"}, "", "",
			wire.Ctx{Machine: host, Session: "abcdef123456", Name: "abcdef12"}},
		{"no session, no name", plain, client.Env{Machine: "devbox"}, "", "", wire.Ctx{Machine: "devbox"}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if c.origin != "" {
				git(t, repo, "remote", "set-url", "origin", c.origin)
				t.Cleanup(func() { git(t, repo, "remote", "set-url", "origin", "git@github.com:acme/relay.git") })
			}
			t.Setenv("GIT_CONFIG_GLOBAL", "/dev/null")
			if got := client.Gather(context.Background(), c.dir, c.env, c.session); got != c.want {
				t.Errorf("Gather =\n  %+v\nwant\n  %+v", got, c.want)
			}
		})
	}
}
