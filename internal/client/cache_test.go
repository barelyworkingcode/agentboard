package client_test

import (
	"context"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/barelyworkingcode/agentboard/internal/client"
	"github.com/barelyworkingcode/agentboard/internal/wire"
)

// cacheEnv reads Env the way the hook does, with AB_CACHE_DIR set to cacheDir.
// HOME moves to a temp dir so a missed override can't touch the real user cache.
func cacheEnv(t *testing.T, cacheDir string) client.Env {
	t.Helper()
	t.Setenv("HOME", t.TempDir())
	t.Setenv("XDG_CACHE_HOME", "")
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	for k, v := range map[string]string{
		"AB_CACHE_DIR": cacheDir, "AB_MACHINE": "devbox", "AB_NAME": "p1", "AB_RUN": "", "AB_URL": "",
		"CLAUDE_CODE_SESSION_ID": "sess-a", "TMUX": "", "TMUX_PANE": "",
	} {
		t.Setenv(k, v)
	}
	return client.EnvFromOS()
}

func cacheRepo(t *testing.T) (root, repo string) {
	t.Helper()
	root = t.TempDir()
	repo = filepath.Join(root, "relay")
	if err := os.Mkdir(repo, 0o755); err != nil {
		t.Fatal(err)
	}
	git(t, repo, "init", "-q", "-b", "main")
	git(t, repo, "commit", "-q", "--allow-empty", "-m", "init")
	git(t, repo, "remote", "add", "origin", "git@github.com:acme/relay.git")
	return root, repo
}

func labels(branch, repo string, issue int) wire.Ctx {
	return wire.Ctx{Machine: "devbox", Project: "relay", Branch: branch, Repo: repo, Issue: issue, Session: "sess-a", Name: "p1"}
}

func expectGather(t *testing.T, step, dir string, env client.Env, want wire.Ctx) {
	t.Helper()
	if got := client.Gather(context.Background(), dir, env, ""); got != want {
		t.Errorf("%s: Gather =\n  %+v\nwant\n  %+v", step, got, want)
	}
}

func TestGatherFollowsGitChanges(t *testing.T) {
	_, repo := cacheRepo(t)
	env := cacheEnv(t, t.TempDir())
	steps := []struct {
		name   string
		change []string
		want   wire.Ctx
	}{
		{"first call", nil, labels("main", "acme/relay", 0)},
		{"after git switch -c", []string{"switch", "-q", "-c", "feat/77-x"}, labels("feat/77-x", "acme/relay", 77)},
		{"after origin set-url", []string{"remote", "set-url", "origin", "https://github.com/acme/other.git"}, labels("feat/77-x", "acme/other", 77)},
	}
	for _, s := range steps {
		if s.change != nil {
			git(t, repo, s.change...)
		}
		expectGather(t, s.name, repo, env, s.want)
	}
}

func TestGatherWorktreeFollowsItsOwnHead(t *testing.T) {
	root, repo := cacheRepo(t)
	git(t, repo, "switch", "-q", "-c", "feat/88-x")
	wt := filepath.Join(root, "wt-other")
	git(t, repo, "worktree", "add", "-q", "-b", "fix/5-y", wt)
	env := cacheEnv(t, t.TempDir())

	expectGather(t, "main checkout", repo, env, labels("feat/88-x", "acme/relay", 88))
	expectGather(t, "worktree", wt, env, labels("fix/5-y", "acme/relay", 5))
	git(t, wt, "switch", "-q", "-c", "chore/6-z")
	expectGather(t, "worktree after switch", wt, env, labels("chore/6-z", "acme/relay", 6))
	expectGather(t, "main checkout after worktree switch", repo, env, labels("feat/88-x", "acme/relay", 88))
}

func cacheFiles(t *testing.T, dir string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(dir, func(p string, d fs.DirEntry, err error) error {
		if err == nil && d.Type().IsRegular() {
			files = append(files, p)
		}
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return files
}

func TestGatherCacheFailureFallsBackToGit(t *testing.T) {
	cases := []struct {
		name string
		prep func(t *testing.T, cacheDir, repo string, env client.Env)
	}{
		{"unwritable cache dir", func(t *testing.T, cacheDir, _ string, _ client.Env) {
			if err := os.Chmod(cacheDir, 0o500); err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { os.Chmod(cacheDir, 0o700) })
			if f, err := os.CreateTemp(cacheDir, "probe"); err == nil {
				f.Close()
				t.Skip("chmod 0500 does not stop writes here")
			}
		}},
		{"corrupt cache files", func(t *testing.T, cacheDir, repo string, env client.Env) {
			client.Gather(context.Background(), repo, env, "")
			for _, f := range cacheFiles(t, cacheDir) {
				if err := os.WriteFile(f, []byte("\x00not a cache entry{"), 0o600); err != nil {
					t.Fatal(err)
				}
			}
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			_, repo := cacheRepo(t)
			git(t, repo, "switch", "-q", "-c", "feat/77-x")
			cacheDir := t.TempDir()
			env := cacheEnv(t, cacheDir)
			c.prep(t, cacheDir, repo, env)
			expectGather(t, c.name, repo, env, labels("feat/77-x", "acme/relay", 77))
		})
	}
}

func TestGatherCacheHoldsNoRemoteURL(t *testing.T) {
	_, repo := cacheRepo(t)
	git(t, repo, "remote", "set-url", "origin", "https://x-access-token:SECRET@github.com/acme/relay.git")
	cacheDir := t.TempDir()
	env := cacheEnv(t, cacheDir)

	expectGather(t, "first call", repo, env, labels("main", "acme/relay", 0))
	expectGather(t, "cached call", repo, env, labels("main", "acme/relay", 0))

	files := cacheFiles(t, cacheDir)
	if len(files) == 0 {
		t.Fatal("Gather wrote no cache file under AB_CACHE_DIR")
	}
	for _, f := range files {
		b, err := os.ReadFile(f)
		if err != nil {
			t.Fatal(err)
		}
		for _, leak := range []string{"SECRET", "github.com/"} {
			if strings.Contains(string(b), leak) {
				t.Errorf("cache file %s contains %q", filepath.Base(f), leak)
			}
		}
	}
}
