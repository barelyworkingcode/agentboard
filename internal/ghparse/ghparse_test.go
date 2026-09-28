package ghparse_test

import (
	"fmt"
	"reflect"
	"strings"
	"testing"

	"github.com/barelyworkingcode/agentboard/internal/ghparse"
	"github.com/barelyworkingcode/agentboard/internal/wire"
)

func str(s string) *string { return &s }
func num(n int) *int       { return &n }

// Session contexts. The branch issue decides what pr create and a bare
// pr merge post.
var (
	withIssue     = wire.Ctx{Machine: "devbox", Project: "widgets", Branch: "feat/7-fix", Repo: "acme/widgets", Issue: 7, Session: "sess-1", Name: "p1", Run: "r1"}
	noIssue       = wire.Ctx{Machine: "devbox", Project: "widgets", Branch: "main", Repo: "acme/widgets", Session: "sess-1", Name: "p1"}
	noRepo        = wire.Ctx{Machine: "devbox", Project: "scratch", Branch: "feat/7-fix", Issue: 7, Session: "sess-1"}
	noRepoNoIssue = wire.Ctx{Machine: "devbox", Project: "scratch", Session: "sess-1"}
)

func item(c wire.Ctx, repo string, number int, title *string, pr *int, state string) wire.ItemPost {
	return wire.ItemPost{Ctx: c, Repo: repo, Number: number, Title: title, PR: pr, State: str(state)}
}

func show(items []wire.ItemPost) string {
	if len(items) == 0 {
		return "nothing"
	}
	var parts []string
	for _, p := range items {
		d := func(s *string) string {
			if s == nil {
				return "<nil>"
			}
			return fmt.Sprintf("%q", *s)
		}
		pr := "<nil>"
		if p.PR != nil {
			pr = fmt.Sprint(*p.PR)
		}
		parts = append(parts, fmt.Sprintf("{Repo:%q Number:%d Title:%s PR:%s State:%s Tier:%s Ctx:%+v}",
			p.Repo, p.Number, d(p.Title), pr, d(p.State), d(p.Tier), p.Ctx))
	}
	return "[" + strings.Join(parts, " ") + "]"
}

type parseCase struct {
	name    string
	command string
	stdout  string
	exit    int
	ctx     wire.Ctx
	want    []wire.ItemPost
}

func runCases(t *testing.T, cases []parseCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := ghparse.Parse(c.command, c.stdout, c.exit, c.ctx)
			if len(c.want) == 0 {
				if len(got) != 0 {
					t.Errorf("Parse(%q, %q, %d) = %s, want nothing", c.command, c.stdout, c.exit, show(got))
				}
				return
			}
			if !reflect.DeepEqual(got, c.want) {
				t.Errorf("Parse(%q, %q, %d)\n got  %s\n want %s", c.command, c.stdout, c.exit, show(got), show(c.want))
			}
		})
	}
}

const (
	issue12 = "https://github.com/acme/widgets/issues/12\n"
	pull31  = "https://github.com/acme/widgets/pull/31\n"
)

func TestIssueCreate(t *testing.T) {
	runCases(t, []parseCase{
		{"double-quoted title", `gh issue create --title "Fix the widget" --body "Steps to reproduce"`, issue12, 0, withIssue,
			[]wire.ItemPost{item(withIssue, "acme/widgets", 12, str("Fix the widget"), nil, "open")}},
		{"--title='a b'", `gh issue create --title='a b' --body x`, issue12, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, str("a b"), nil, "open")}},
		{"--title=plain", `gh issue create --title=plain --body x`, issue12, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, str("plain"), nil, "open")}},
		{"-t value", `gh issue create -t "short one" -b body`, issue12, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, str("short one"), nil, "open")}},
		{"single-quoted title", `gh issue create --title 'it''s "odd"' -b x`, issue12, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, str(`its "odd"`), nil, "open")}},
		{"escaped quote inside double quotes", `gh issue create --title "say \"hi\"" -b x`, issue12, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, str(`say "hi"`), nil, "open")}},
		{"backslash-escaped space outside quotes", `gh issue create --title a\ b -b x`, issue12, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, str("a b"), nil, "open")}},
		{"no title leaves Title nil", `gh issue create --body-file notes.md --label bug`, issue12, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, nil, nil, "open")}},
		// R11: present but empty is an empty title, not an absent one.
		{"--title '' is empty, not absent", `gh issue create --title '' -b x`, issue12, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, str(""), nil, "open")}},
		{`-t "" is empty, not absent`, `gh issue create -t "" -b x`, issue12, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, str(""), nil, "open")}},
		// R10: a title built from a shell expansion is absent.
		{`-t "$T"`, `gh issue create -t "$T" -b x`, issue12, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, nil, nil, "open")}},
		{"title with an embedded $VAR", `gh issue create --title "Fix $WIDGET now" -b x`, issue12, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, nil, nil, "open")}},
		{"--title=$(...)", `gh issue create --title="$(head -1 notes.md)" -b x`, issue12, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, nil, nil, "open")}},
		{"backtick title", "gh issue create -t `cat t.txt` -b x", issue12, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, nil, nil, "open")}},
		{"single-quoted $ is literal", `gh issue create -t 'costs $5' -b x`, issue12, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, str("costs $5"), nil, "open")}},
		{"no trailing newline", `gh issue create -t T`, "https://github.com/acme/widgets/issues/12", 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, str("T"), nil, "open")}},
		{"extra stdout lines around the URL", `gh issue create -t T -b x`,
			"Warning: 1 uncommitted change\n\nhttps://github.com/acme/widgets/issues/13\nOpening in browser\n", 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 13, str("T"), nil, "open")}},
		{"first issue URL in stdout wins", `gh issue create -t T -b x`,
			"https://github.com/acme/widgets/issues/13\nhttps://github.com/acme/widgets/issues/14\n", 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 13, str("T"), nil, "open")}},
		{"repo from stdout URL over ctx.Repo", `gh issue create -t T -b x`, "https://github.com/acme/gadgets/issues/4\n", 0, withIssue,
			[]wire.ItemPost{item(withIssue, "acme/gadgets", 4, str("T"), nil, "open")}},
		{"repo from stdout URL with no ctx.Repo", `gh issue create -t T -b x`, "https://github.com/acme/gadgets/issues/4\n", 0, noRepoNoIssue,
			[]wire.ItemPost{item(noRepoNoIssue, "acme/gadgets", 4, str("T"), nil, "open")}},

		{"non-zero exit", `gh issue create --title "Fix the widget" -b x`, issue12, 1, noIssue, nil},
		{"exit 2 with URL", `gh issue create -t T -b x`, issue12, 2, noIssue, nil},
		{"no URL in stdout", `gh issue create -t T -b x`, "", 0, noIssue, nil},
		{"only progress text in stdout", `gh issue create -t T -b x`, "Creating issue in acme/widgets\n", 0, noIssue, nil},
		{"pull URL for issue create", `gh issue create -t T -b x`, pull31, 0, noIssue, nil},
		{"non-GitHub host", `gh issue create -t T -b x`, "https://git.example.com/acme/widgets/issues/12\n", 0, noIssue, nil},
	})
}

func TestRepoPrecedence(t *testing.T) {
	gadgets4 := "https://github.com/acme/gadgets/issues/4\n"
	runCases(t, []parseCase{
		{"--repo value", `gh issue create --repo acme/gadgets -t T -b x`, gadgets4, 0, withIssue,
			[]wire.ItemPost{item(withIssue, "acme/gadgets", 4, str("T"), nil, "open")}},
		{"-R value", `gh issue create -R acme/gadgets -t T -b x`, gadgets4, 0, withIssue,
			[]wire.ItemPost{item(withIssue, "acme/gadgets", 4, str("T"), nil, "open")}},
		{"--repo=value", `gh issue create --repo=acme/gadgets -t T -b x`, gadgets4, 0, withIssue,
			[]wire.ItemPost{item(withIssue, "acme/gadgets", 4, str("T"), nil, "open")}},
		{"--repo with host prefix", `gh issue create --repo github.com/acme/gadgets -t T -b x`, gadgets4, 0, withIssue,
			[]wire.ItemPost{item(withIssue, "acme/gadgets", 4, str("T"), nil, "open")}},
		{"--repo wins over the stdout URL", `gh issue create -R acme/gadgets -t T -b x`, "https://github.com/acme/widgets/issues/4\n", 0, withIssue,
			[]wire.ItemPost{item(withIssue, "acme/gadgets", 4, str("T"), nil, "open")}},
		{"malformed --repo is ignored", `gh issue create --repo gadgets -t T -b x`, "https://github.com/acme/widgets/issues/4\n", 0, noRepoNoIssue,
			[]wire.ItemPost{item(noRepoNoIssue, "acme/widgets", 4, str("T"), nil, "open")}},
		{"pr merge: --repo over ctx.Repo", `gh pr merge -R acme/gadgets 12 --squash`, "", 0, withIssue,
			[]wire.ItemPost{item(withIssue, "acme/gadgets", 0, nil, num(12), "merged")}},
		{"pr merge: --repo=value", `gh pr merge --repo=acme/gadgets 12`, "", 0, withIssue,
			[]wire.ItemPost{item(withIssue, "acme/gadgets", 0, nil, num(12), "merged")}},
		{"pr merge: --repo from an expansion is absent", `gh pr merge 12 --repo "$REPO"`, "", 0, withIssue,
			[]wire.ItemPost{item(withIssue, "acme/widgets", 0, nil, num(12), "merged")}},
		{"pr merge: ctx.Repo when nothing else", `gh pr merge 12 --squash`, "", 0, withIssue,
			[]wire.ItemPost{item(withIssue, "acme/widgets", 0, nil, num(12), "merged")}},
		{"pr merge: no repo anywhere", `gh pr merge 12 --squash`, "", 0, noRepo, nil},
		{"pr merge: no repo anywhere, no arg", `gh pr merge --squash`, "", 0, noRepo, nil},
	})
}

func TestPRCreate(t *testing.T) {
	runCases(t, []parseCase{
		{"branch issue: recorded under the issue, no title", `gh pr create --title "Add the widget" --body "Closes #7"`, pull31, 0, withIssue,
			[]wire.ItemPost{item(withIssue, "acme/widgets", 7, nil, num(31), "PR open")}},
		{"no branch issue: recorded under its own number", `gh pr create --title "Add the widget" --body "Closes #7"`, pull31, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 31, str("Add the widget"), num(31), "PR open")}},
		{"no branch issue, -t", `gh pr create -t 'Add it' -b x --base main`, pull31, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 31, str("Add it"), num(31), "PR open")}},
		{"no branch issue, title from an expansion", `gh pr create -t "$TITLE" -b x`, pull31, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 31, nil, num(31), "PR open")}},
		{"no branch issue, --fill (no title)", `gh pr create --fill`, pull31, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 31, nil, num(31), "PR open")}},
		{"extra stdout lines", `gh pr create -t T -b x`,
			"\nWarning: 2 uncommitted changes\nhttps://github.com/acme/widgets/pull/31\n", 0, withIssue,
			[]wire.ItemPost{item(withIssue, "acme/widgets", 7, nil, num(31), "PR open")}},
		{"repo from the stdout URL", `gh pr create -t T -b x`, "https://github.com/acme/gadgets/pull/9\n", 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/gadgets", 9, str("T"), num(9), "PR open")}},
		{"-R repo", `gh pr create -R acme/gadgets -t T -b x`, "https://github.com/acme/gadgets/pull/9\n", 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/gadgets", 9, str("T"), num(9), "PR open")}},

		{"non-zero exit", `gh pr create -t T -b x`, pull31, 1, withIssue, nil},
		{"no URL", `gh pr create -t T -b x`, "", 0, withIssue, nil},
		{"issue URL for pr create", `gh pr create -t T -b x`, issue12, 0, withIssue, nil},
	})
}

func TestPRMerge(t *testing.T) {
	merged := func(c wire.Ctx, repo string, p int) []wire.ItemPost {
		return []wire.ItemPost{item(c, repo, 0, nil, num(p), "merged")}
	}
	byIssue := []wire.ItemPost{item(withIssue, "acme/widgets", 7, nil, nil, "merged")}
	runCases(t, []parseCase{
		{"number", `gh pr merge 12`, "", 0, withIssue, merged(withIssue, "acme/widgets", 12)},
		{"number, no branch issue", `gh pr merge 12`, "", 0, noIssue, merged(noIssue, "acme/widgets", 12)},
		{"quoted #number", `gh pr merge "#12" --squash`, "", 0, withIssue, merged(withIssue, "acme/widgets", 12)},
		{"single-quoted #number", `gh pr merge '#12'`, "", 0, noIssue, merged(noIssue, "acme/widgets", 12)},
		// R9: an unquoted # starts a shell comment, so there is no argument.
		{"unquoted #12 is a comment, branch issue", `gh pr merge #12`, "", 0, withIssue, byIssue},
		{"unquoted #12 is a comment, no branch issue", `gh pr merge #12`, "", 0, noIssue, nil},
		{"comment after flags", `gh pr merge --squash # merge 12 now`, "", 0, withIssue, byIssue},
		// R10: an argument built from a shell expansion is unknown.
		{"$VAR argument", `gh pr merge $PR --squash`, "", 0, withIssue, nil},
		{"quoted ${VAR} argument", `gh pr merge "${PR}"`, "", 0, withIssue, nil},
		{"$(...) argument", `gh pr merge $(cat pr.txt)`, "", 0, withIssue, nil},
		{"backtick argument", "gh pr merge `cat pr.txt`", "", 0, withIssue, nil},
		{"flags after the number", `gh pr merge 12 --squash --delete-branch`, "", 0, withIssue, merged(withIssue, "acme/widgets", 12)},
		{"flags before the number", `gh pr merge --squash --delete-branch 12`, "", 0, withIssue, merged(withIssue, "acme/widgets", 12)},
		{"short flags", `gh pr merge -s -d 12`, "", 0, withIssue, merged(withIssue, "acme/widgets", 12)},
		{"pull URL sets the repo", `gh pr merge https://github.com/acme/gadgets/pull/12 --merge`, "", 0, withIssue, merged(withIssue, "acme/gadgets", 12)},
		{"pull URL with no ctx.Repo", `gh pr merge https://github.com/acme/gadgets/pull/12`, "", 0, noRepoNoIssue, merged(noRepoNoIssue, "acme/gadgets", 12)},
		{"stdout text is ignored", `gh pr merge 12 --squash`, "✓ Squashed and merged pull request acme/widgets#12\n", 0, withIssue, merged(withIssue, "acme/widgets", 12)},
		{"--body value is not the PR", `gh pr merge --body "see #99" 12`, "", 0, withIssue, merged(withIssue, "acme/widgets", 12)},
		{"-b value is not the PR", `gh pr merge -b 99 12`, "", 0, withIssue, merged(withIssue, "acme/widgets", 12)},
		{"-t value is not the PR", `gh pr merge -t 5 12`, "", 0, withIssue, merged(withIssue, "acme/widgets", 12)},
		{"--subject value is not the PR", `gh pr merge --subject 5 --squash 12`, "", 0, withIssue, merged(withIssue, "acme/widgets", 12)},
		{"-F value is not the PR", `gh pr merge -F 5 12`, "", 0, withIssue, merged(withIssue, "acme/widgets", 12)},
		{"-A value is not the PR", `gh pr merge -A dev@example.com 12`, "", 0, withIssue, merged(withIssue, "acme/widgets", 12)},
		{"--match-head-commit value is not the PR", `gh pr merge --match-head-commit 1234567 12`, "", 0, withIssue, merged(withIssue, "acme/widgets", 12)},
		{"--body=value is not the PR", `gh pr merge --body=99 12`, "", 0, withIssue, merged(withIssue, "acme/widgets", 12)},

		{"no arg, branch issue", `gh pr merge --squash --delete-branch`, "", 0, withIssue, byIssue},
		{"no arg at all, branch issue", `gh pr merge`, "", 0, withIssue, byIssue},
		{"-t 5 only, branch issue", `gh pr merge -t 5 --squash`, "", 0, withIssue, byIssue},
		{"-b 99 only, branch issue", `gh pr merge -b 99`, "", 0, withIssue, byIssue},
		{"body URL is not an argument", `gh pr merge --squash --body "https://github.com/acme/gadgets/pull/99"`, "", 0, withIssue, byIssue},

		{"no arg, no branch issue", `gh pr merge --squash`, "", 0, noIssue, nil},
		{"-t 5 only, no branch issue", `gh pr merge -t 5 --squash`, "", 0, noIssue, nil},
		{"body URL only, no branch issue", `gh pr merge --body "https://github.com/acme/gadgets/pull/99"`, "", 0, noIssue, nil},
		{"branch name argument", `gh pr merge feat/7-fix --squash`, "", 0, withIssue, nil},
		{"zero", `gh pr merge 0`, "", 0, withIssue, nil},
		{"issue URL argument", `gh pr merge https://github.com/acme/widgets/issues/12`, "", 0, withIssue, nil},
		{"non-zero exit", `gh pr merge 12 --squash`, "", 1, withIssue, nil},
		{"non-zero exit, no arg", `gh pr merge`, "", 1, withIssue, nil},
	})
}

func TestUnrecognisedCommands(t *testing.T) {
	runCases(t, []parseCase{
		{"gh issue list", `gh issue list --state open`, issue12, 0, withIssue, nil},
		{"gh issue view", `gh issue view 12`, issue12, 0, withIssue, nil},
		{"gh pr view", `gh pr view 31`, pull31, 0, withIssue, nil},
		{"gh pr checkout", `gh pr checkout 31`, pull31, 0, withIssue, nil},
		{"gh pr list", `gh pr list`, pull31, 0, withIssue, nil},
		{"gh repo view", `gh repo view acme/widgets`, "", 0, withIssue, nil},
		{"gh api", `gh api repos/acme/widgets/issues -f title=x`, issue12, 0, withIssue, nil},
		{"bare gh", `gh`, "", 0, withIssue, nil},
		{"git commit mentioning gh pr create", `git commit -m "run gh pr create next"`, pull31, 0, withIssue, nil},
		{"echo 'gh pr create'", `echo 'gh pr create'`, pull31, 0, withIssue, nil},
		{"echo gh pr merge", `echo gh pr merge 12`, "gh pr merge 12\n", 0, withIssue, nil},
		{"captured echo of an issue URL", `echo https://github.com/acme/widgets/issues/7`, "https://github.com/acme/widgets/issues/7", 0, withIssue, nil},
		{"sudo gh", `sudo gh issue create -t T -b x`, issue12, 0, withIssue, nil},
		{"env gh", `env GH_PAGER= gh issue create -t T -b x`, issue12, 0, withIssue, nil},
		{"absolute gh path", `/usr/bin/gh pr merge 12`, "", 0, withIssue, nil},
		{"ghx is not gh", `ghx pr merge 12`, "", 0, withIssue, nil},
		{"empty command", ``, issue12, 0, withIssue, nil},
	})
}

func TestFirstGhInvocation(t *testing.T) {
	runCases(t, []parseCase{
		{"after cd &&", `cd widgets && gh issue create --title "Fix the widget" -b x`, issue12, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, str("Fix the widget"), nil, "open")}},
		{"after ;", `git push -u origin HEAD; gh pr create -t T -b x`, pull31, 0, withIssue,
			[]wire.ItemPost{item(withIssue, "acme/widgets", 7, nil, num(31), "PR open")}},
		{"after ||", `false || gh pr merge 12`, "", 0, withIssue,
			[]wire.ItemPost{item(withIssue, "acme/widgets", 0, nil, num(12), "merged")}},
		{"after a newline", "cd widgets\ngh pr merge 12 --squash", "", 0, withIssue,
			[]wire.ItemPost{item(withIssue, "acme/widgets", 0, nil, num(12), "merged")}},
		{"piped into tee", `gh issue create -t T -b x | tee out.txt`, issue12, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, str("T"), nil, "open")}},
		{"empty env assignment prefix", `GH_PAGER= gh issue create -t T -b x`, issue12, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, str("T"), nil, "open")}},
		{"several env assignments", `GH_PAGER=cat GH_PROMPT_DISABLED=1 gh pr merge 12`, "", 0, withIssue,
			[]wire.ItemPost{item(withIssue, "acme/widgets", 0, nil, num(12), "merged")}},
		{"cd then env assignment", `cd widgets && NO_COLOR=1 gh pr create -t T -b x`, pull31, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 31, str("T"), num(31), "PR open")}},
		{"&& inside quotes does not split", `gh issue create --title "a && gh pr merge 5" -b x`, issue12, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, str("a && gh pr merge 5"), nil, "open")}},

		{"gh auth status first", `gh auth status && gh pr create --title T -b x`, pull31, 0, withIssue, nil},
		{"gh repo view first", `gh repo view; gh issue create -t T -b x`, issue12, 0, withIssue, nil},
		{"gh pr view first, then merge", `gh pr view 12 && gh pr merge 12`, "", 0, withIssue, nil},
	})
}

func TestBodiesAreNeverParsed(t *testing.T) {
	heredocBody := "gh issue create --title \"Fix the widget\" --body \"$(cat <<'EOF'\n" +
		"See https://github.com/acme/gadgets/issues/99\n" +
		"--repo acme/gadgets -t Wrong\n" +
		"EOF\n)\""
	prHeredoc := "gh pr create --body \"$(cat <<'EOF'\n" +
		"## Summary\n-t Wrong\n--title Wrong\nhttps://github.com/acme/gadgets/pull/99\n" +
		"EOF\n)\" --title \"Add the widget\""
	shellHeredoc := "gh issue create --title T --body-file - <<'EOF'\n" +
		"https://github.com/acme/gadgets/issues/99\n--repo acme/gadgets\n" +
		"EOF"
	dashHeredoc := "gh issue create --title T --body-file - <<-EOF\n" +
		"\t--repo acme/gadgets\n" +
		"\tEOF"
	plainHeredoc := "gh pr merge --body-file - <<EOF\n" +
		"12\nhttps://github.com/acme/gadgets/pull/99\n" +
		"EOF"
	// These bodies hold an apostrophe, an unbalanced paren and a gh line: they
	// parse correctly only when the heredoc body is skipped as a whole.
	prApostrophe := "gh pr create --body \"$(cat <<'EOF'\n" +
		"- Don't crash\n- fixes (part 1\n" +
		"EOF\n)\" --title \"feat: add x\""
	catQuoted := "cat > b.md <<'EOF'\n" +
		"gh pr merge 99\nit's (odd\n" +
		"EOF\n" +
		"gh issue create -t T -F b.md"
	catDash := "cat > b.md <<-EOF\n" +
		"\tgh pr merge 99\n\tit's (odd\n" +
		"\tEOF\n" +
		"gh issue create -t T -F b.md"
	catPlain := "cat > b.md <<EOF\n" +
		"gh pr merge 99\nit's (odd \"half\n" +
		"EOF\n" +
		"gh issue create -t T -F b.md"
	issueT := []wire.ItemPost{item(noIssue, "acme/widgets", 12, str("T"), nil, "open")}
	runCases(t, []parseCase{
		{"$(cat <<'EOF') body with apostrophe and open paren, title after", prApostrophe, pull31, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 31, str("feat: add x"), num(31), "PR open")}},
		{"heredoc <<'EOF' before gh: body gh line is not a command", catQuoted, issue12, 0, noIssue, issueT},
		{"heredoc <<'EOF' before gh, empty stdout", catQuoted, "", 0, noIssue, nil},
		{"heredoc <<-EOF with tab-indented delimiter before gh", catDash, issue12, 0, noIssue, issueT},
		{"heredoc <<-EOF before gh, empty stdout", catDash, "", 0, noIssue, nil},
		{"heredoc <<EOF before gh", catPlain, issue12, 0, noIssue, issueT},
		{"heredoc <<EOF before gh, empty stdout", catPlain, "", 0, noIssue, nil},
		{"$(cat <<'EOF') body with URL and flags", heredocBody, issue12, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, str("Fix the widget"), nil, "open")}},
		{"$(cat <<'EOF') body, empty stdout", heredocBody, "", 0, noIssue, nil},
		{"pr create: title after a heredoc body", prHeredoc, pull31, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 31, str("Add the widget"), num(31), "PR open")}},
		{"pr create: heredoc body, empty stdout", prHeredoc, "", 0, noIssue, nil},
		{"shell heredoc <<'EOF'", shellHeredoc, issue12, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, str("T"), nil, "open")}},
		{"shell heredoc <<'EOF', empty stdout", shellHeredoc, "", 0, noIssue, nil},
		{"shell heredoc <<-EOF", dashHeredoc, issue12, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, str("T"), nil, "open")}},
		{"pr merge: heredoc body holds a number and a URL, no branch issue", plainHeredoc, "", 0, noIssue, nil},
		{"pr merge: heredoc body, branch issue", plainHeredoc, "", 0, withIssue,
			[]wire.ItemPost{item(withIssue, "acme/widgets", 7, nil, nil, "merged")}},
		{"--body URL, empty stdout", `gh issue create -t T --body "https://github.com/acme/widgets/issues/99"`, "", 0, noIssue, nil},
		{"--body=URL, empty stdout", `gh issue create -t T --body=https://github.com/acme/widgets/issues/99`, "", 0, noIssue, nil},
		{"-b URL, empty stdout", `gh pr create -t T -b https://github.com/acme/widgets/pull/99`, "", 0, withIssue, nil},
		{"--body URL does not set the repo", `gh issue create -t T --body "https://github.com/acme/gadgets/issues/99"`, issue12, 0, noIssue,
			[]wire.ItemPost{item(noIssue, "acme/widgets", 12, str("T"), nil, "open")}},
		{"$(...) merge argument is opaque", `gh pr merge $(gh pr list --json number -q '.[0].number')`, "", 0, noIssue, nil},
	})
}

// R1: every field of Ctx is carried through unchanged, whichever command.
func TestCtxIsCarriedThrough(t *testing.T) {
	c := wire.Ctx{Machine: "m9", Project: "pz", Branch: "fix/3-y", Repo: "acme/widgets", Issue: 3, Session: "sess-z", Name: "nz", Run: "rz"}
	for _, in := range []struct{ cmd, out string }{
		{`gh issue create -t T`, issue12},
		{`gh pr create -t T`, pull31},
		{`gh pr merge 12`, ""},
		{`gh pr merge`, ""},
	} {
		got := ghparse.Parse(in.cmd, in.out, 0, c)
		if len(got) == 0 {
			t.Errorf("%s: nothing parsed", in.cmd)
			continue
		}
		for _, p := range got {
			if p.Ctx != c {
				t.Errorf("%s: Ctx = %+v, want %+v", in.cmd, p.Ctx, c)
			}
			if p.Tier != nil {
				t.Errorf("%s: Tier = %q, want nil", in.cmd, *p.Tier)
			}
		}
	}
}
