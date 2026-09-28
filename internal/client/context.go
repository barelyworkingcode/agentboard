// Package client gathers the context envelope and posts it to agentboard.
package client

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/barelyworkingcode/agentboard/internal/wire"
)

// DefaultURL is used when AB_URL is unset.
const DefaultURL = "http://127.0.0.1:8790"

// gitDeadline bounds the concurrent git and tmux lookups inside Gather.
const gitDeadline = 150 * time.Millisecond

// Env is the process environment the client reads.
type Env struct {
	URL       string // AB_URL
	Machine   string // AB_MACHINE
	Name      string // AB_NAME
	Run       string // AB_RUN
	SessionID string // CLAUDE_CODE_SESSION_ID
	TMUX      string // TMUX
	TMUXPane  string // TMUX_PANE
	CacheDir  string // AB_CACHE_DIR; "" means <user cache dir>/agentboard/ctx
}

// EnvFromOS reads Env from the process environment.
func EnvFromOS() Env {
	return Env{
		URL:       os.Getenv("AB_URL"),
		Machine:   os.Getenv("AB_MACHINE"),
		Name:      os.Getenv("AB_NAME"),
		Run:       os.Getenv("AB_RUN"),
		SessionID: os.Getenv("CLAUDE_CODE_SESSION_ID"),
		TMUX:      os.Getenv("TMUX"),
		TMUXPane:  os.Getenv("TMUX_PANE"),
		CacheDir:  os.Getenv("AB_CACHE_DIR"),
	}
}

// BaseURL is AB_URL (or DefaultURL) without a trailing slash.
func (e Env) BaseURL() string {
	u := e.URL
	if u == "" {
		u = DefaultURL
	}
	return strings.TrimRight(u, "/")
}

// Gather builds the context envelope for a post from dir. An empty session
// falls back to env.SessionID. Every lookup fails soft to "" or 0.
func Gather(ctx context.Context, dir string, env Env, session string) wire.Ctx {
	if session == "" {
		session = env.SessionID
	}
	c := wire.Ctx{
		Machine: machineName(env),
		Session: session,
		Name:    env.Name,
		Run:     env.Run,
	}

	sub, cancel := context.WithTimeout(ctx, gitDeadline)
	defer cancel()

	var (
		wg       sync.WaitGroup
		tmuxName string
	)
	if c.Name == "" && env.TMUX != "" {
		wg.Add(1)
		go func() { defer wg.Done(); tmuxName = tmuxSession(sub, env.TMUXPane) }()
	}
	labels := cachedGitLabels(sub, dir, env)
	wg.Wait()

	c.Project = labels.Project
	c.Branch = labels.Branch
	c.Repo = labels.Repo
	c.Issue = labels.Issue
	if c.Name == "" {
		c.Name = tmuxName
	}
	if c.Name == "" {
		c.Name = firstN(session, 8)
	}
	return c
}

// cachedGitLabels serves the labels from the per-directory cache while its
// stamp holds, else runs git and refreshes the cache.
func cachedGitLabels(ctx context.Context, dir string, env Env) gitLabels {
	cache := openGitCache(dir, env)
	if l, ok := cache.load(); ok {
		return l
	}
	l := gitLabelsFromGit(ctx, dir)
	// A lookup cut short by the deadline would pin blank labels until HEAD
	// next changes.
	if ctx.Err() == nil {
		cache.store(l)
	}
	return l
}

func gitLabelsFromGit(ctx context.Context, dir string) gitLabels {
	var (
		wg                      sync.WaitGroup
		project, branch, remote string
	)
	wg.Add(3)
	go func() { defer wg.Done(); project = projectName(ctx, dir) }()
	go func() { defer wg.Done(); branch = runGit(ctx, dir, "symbolic-ref", "--short", "-q", "HEAD") }()
	go func() { defer wg.Done(); remote = runGit(ctx, dir, "remote", "get-url", "origin") }()
	wg.Wait()
	return gitLabels{Project: project, Branch: branch, Repo: ParseRemote(remote), Issue: IssueFromBranch(branch)}
}

func machineName(env Env) string {
	if env.Machine != "" {
		return env.Machine
	}
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	name, _, _ := strings.Cut(h, ".")
	return name
}

// projectName reports the main repository's name, so a worktree reports the
// repo it belongs to rather than its own directory.
func projectName(ctx context.Context, dir string) string {
	out := runGit(ctx, dir, "rev-parse", "--path-format=absolute", "--show-toplevel", "--git-common-dir")
	lines := strings.Split(out, "\n")
	if len(lines) != 2 {
		return ""
	}
	toplevel, common := strings.TrimSpace(lines[0]), strings.TrimSpace(lines[1])
	if filepath.Base(common) == ".git" {
		return filepath.Base(filepath.Dir(common))
	}
	if toplevel == "" {
		return ""
	}
	return filepath.Base(toplevel)
}

func runGit(ctx context.Context, dir string, args ...string) string {
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", dir}, args...)...)
	cmd.Env = append(os.Environ(), "GIT_TERMINAL_PROMPT=0")
	return output(cmd)
}

func tmuxSession(ctx context.Context, pane string) string {
	args := []string{"display-message", "-p"}
	if pane != "" {
		args = append(args, "-t", pane)
	}
	args = append(args, "#S")
	return output(exec.CommandContext(ctx, "tmux", args...))
}

// output returns trimmed stdout, or "" on any failure.
func output(cmd *exec.Cmd) string {
	var stdout bytes.Buffer
	cmd.Stdout = &stdout
	// A killed child can leave a grandchild holding the pipe open; don't
	// let that outlive the deadline.
	cmd.WaitDelay = 20 * time.Millisecond
	if err := cmd.Run(); err != nil {
		return ""
	}
	return strings.TrimSpace(stdout.String())
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n]
}

var remoteRE = regexp.MustCompile(`^(?:(?:https?|ssh|git)://(?:[^@/]+@)?(?i:github\.com)(?::\d+)?/|(?:[^@/]+@)?(?i:github\.com):)([A-Za-z0-9-]+)/([A-Za-z0-9._-]+?)(?:\.git)?/?$`)

// ParseRemote returns "owner/name" for a GitHub remote URL, else "". Only the
// slug is kept, because the URL can carry a token.
func ParseRemote(url string) string {
	m := remoteRE.FindStringSubmatch(strings.TrimSpace(url))
	if m == nil {
		return ""
	}
	return m[1] + "/" + m[2]
}

var branchRE = regexp.MustCompile(`^(fix|feat|chore)/(\d+)-`)

// IssueFromBranch returns N from a fix/N-, feat/N- or chore/N- branch, else 0.
func IssueFromBranch(branch string) int {
	m := branchRE.FindStringSubmatch(branch)
	if m == nil {
		return 0
	}
	n, err := strconv.Atoi(m[2])
	if err != nil {
		return 0
	}
	return n
}
