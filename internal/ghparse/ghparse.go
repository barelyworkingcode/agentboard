// Package ghparse derives ledger updates from finished gh commands, so the
// hook can post them without the command or its output leaving the machine.
package ghparse

import (
	"regexp"
	"strconv"
	"strings"

	"github.com/barelyworkingcode/agentboard/internal/wire"
)

var (
	issueURL  = regexp.MustCompile(`https://github\.com/([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)/issues/(\d+)\b`)
	pullURL   = regexp.MustCompile(`https://github\.com/([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)/pull/(\d+)\b`)
	pullArg   = regexp.MustCompile(`^https://github\.com/([A-Za-z0-9-]+)/([A-Za-z0-9._-]+)/pull/(\d+)$`)
	numArg    = regexp.MustCompile(`^#?(\d+)$`)
	repoFlag  = regexp.MustCompile(`^(?:(?i:github\.com)/)?([A-Za-z0-9-]+/[A-Za-z0-9._-]+)$`)
	assignRE  = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*=`)
	createVal = valueFlags("title:t body:b body-file:F repo:R base:B head:H label:l assignee:a reviewer:r milestone:m project:p template:T recover")
	mergeVal  = valueFlags("body:b body-file:F subject:t author-email:A match-head-commit repo:R")
)

// Parse returns the ledger updates a finished Bash command implies. ctx is the session's gathered context.
func Parse(command, stdout string, exitCode int, ctx wire.Ctx) []wire.ItemPost {
	if exitCode != 0 {
		return nil
	}
	args := ghArgs(command)
	if len(args) < 2 {
		return nil
	}
	var p *wire.ItemPost
	switch args[0] + " " + args[1] {
	case "issue create":
		p = create(args[2:], stdout, issueURL, ctx)
	case "pr create":
		p = create(args[2:], stdout, pullURL, ctx)
	case "pr merge":
		p = merge(args[2:], stdout, ctx)
	}
	if p == nil {
		return nil
	}
	return []wire.ItemPost{*p}
}

// ghArgs returns the words after gh in the first simple command that runs gh,
// skipping leading NAME=value assignments.
func ghArgs(command string) []string {
	l := &lexer{s: command}
	for _, words := range l.commands(false) {
		for len(words) > 0 && assignRE.MatchString(words[0]) {
			words = words[1:]
		}
		if len(words) > 0 && words[0] == "gh" {
			return words[1:]
		}
	}
	return nil
}

// create handles gh issue create and gh pr create, whose output URL matches re.
func create(args []string, stdout string, re *regexp.Regexp, ctx wire.Ctx) *wire.ItemPost {
	flags, _ := parseArgs(args, createVal)
	m := re.FindStringSubmatch(stdout)
	if m == nil {
		return nil
	}
	n := positive(m[3])
	if n == 0 {
		return nil
	}
	p := &wire.ItemPost{Ctx: ctx, Repo: pickRepo(flags, "", m[1]+"/"+m[2], ctx), Number: n}
	if re == issueURL {
		p.State = ptr("open")
	} else {
		p.PR, p.State = ptr(n), ptr("PR open")
		if ctx.Issue > 0 {
			p.Number = ctx.Issue
			return p
		}
	}
	if t, ok := flags["title"]; ok && !strings.ContainsRune(t, opaque) {
		p.Title = &t
	}
	return p
}

// merge handles gh pr merge [<number>|<url>].
func merge(args []string, stdout string, ctx wire.Ctx) *wire.ItemPost {
	flags, pos := parseArgs(args, mergeVal)
	var pr int
	var argRepo string
	if len(pos) > 0 {
		if m := numArg.FindStringSubmatch(pos[0]); m != nil {
			pr = positive(m[1])
		} else if m := pullArg.FindStringSubmatch(pos[0]); m != nil {
			pr, argRepo = positive(m[3]), m[1]+"/"+m[2]
		}
		if pr == 0 {
			return nil
		}
	}
	var outRepo string
	if m := pullURL.FindStringSubmatch(stdout); m != nil {
		outRepo = m[1] + "/" + m[2]
	}
	repo := pickRepo(flags, argRepo, outRepo, ctx)
	switch {
	case repo == "":
		return nil
	case pr > 0:
		return &wire.ItemPost{Ctx: ctx, Repo: repo, PR: ptr(pr), State: ptr("merged")}
	case ctx.Issue > 0:
		return &wire.ItemPost{Ctx: ctx, Repo: repo, Number: ctx.Issue, State: ptr("merged")}
	}
	return nil
}

// pickRepo applies the repo precedence: --repo, the merge URL argument, the
// URL in stdout, then ctx.Repo.
func pickRepo(flags map[string]string, argRepo, outRepo string, ctx wire.Ctx) string {
	if m := repoFlag.FindStringSubmatch(flags["repo"]); m != nil {
		return m[1]
	}
	for _, r := range []string{argRepo, outRepo, ctx.Repo} {
		if r != "" {
			return r
		}
	}
	return ""
}

// valueFlags builds a name→long-name map of value-taking flags from
// "long:short long ..." pairs.
func valueFlags(spec string) map[string]string {
	m := map[string]string{}
	for _, f := range strings.Fields(spec) {
		long, short, _ := strings.Cut(f, ":")
		m["--"+long] = long
		if short != "" {
			m["-"+short] = long
		}
	}
	return m
}

// parseArgs splits args the way gh's flag parser does. Flags not in valued are
// taken as booleans. A repeated flag keeps its last value.
func parseArgs(args []string, valued map[string]string) (flags map[string]string, pos []string) {
	flags = map[string]string{}
	for i := 0; i < len(args); i++ {
		a := args[i]
		switch {
		case a == "--":
			return flags, append(pos, args[i+1:]...)
		case strings.HasPrefix(a, "--"):
			name, val, eq := strings.Cut(a, "=")
			long, ok := valued[name]
			if !ok {
				continue
			}
			if !eq {
				if i+1 >= len(args) {
					continue
				}
				i++
				val = args[i]
			}
			flags[long] = val
		case len(a) > 1 && a[0] == '-':
			// A shorthand cluster such as -dt v, -tv or -t=v.
			for j := 1; j < len(a); j++ {
				long, ok := valued["-"+a[j:j+1]]
				if !ok {
					continue
				}
				val := strings.TrimPrefix(a[j+1:], "=")
				if j+1 == len(a) {
					if i+1 >= len(args) {
						break
					}
					i++
					val = args[i]
				}
				flags[long] = val
				break
			}
		default:
			pos = append(pos, a)
		}
	}
	return flags, pos
}

// positive parses a decimal number, returning 0 unless it is > 0.
func positive(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil || n < 0 {
		return 0
	}
	return n
}

func ptr[T any](v T) *T { return &v }
