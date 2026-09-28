// Package cli implements the agentboard subcommands other than hook and serve.
package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/barelyworkingcode/agentboard/internal/client"
	"github.com/barelyworkingcode/agentboard/internal/wire"
)

const usage = `usage: agentboard <command> [args]

  ask <question...> --rec <text> [--kind question|review|notify]
                                   post a decision; prints its id (--rec optional for notify)
  answer <id> <text...>            answer a decision
  dismiss <id>                     dismiss a decision
  state <note...>                  set this session's note ("" clears)
  log <text...>                    append a log line
  item <repo>#<N> [--title T] [--pr N] [--state S] [--tier T]
                                   upsert a ledger item
  went well|less <text...>         add a retro note
  run start <name>                 start or reopen a run
  run end [<name>]                 end a run
  meter [--run R] [--open-start N] [--open-now N] [--filed N] [--closed N]
                                   set run meter values
  prune --ended-older DUR | --ended-all | --quiet DUR
                                   clear old sessions
  hook                             Claude Code hook adapter (reads stdin)
  serve [--listen IP:PORT]... [--ingest-from CIDR]... [--db PATH]
                                   run the board server
  help                             show this help

Environment: AB_URL (default http://127.0.0.1:8790), AB_MACHINE, AB_NAME, AB_RUN.
`

// usageError is a local problem found before any network call (exit 2).
type usageError string

func (e usageError) Error() string { return string(e) }

func usagef(format string, a ...any) error { return usageError(fmt.Sprintf(format, a...)) }

var errHelp = errors.New("help")

// command is one parsed subcommand, ready to post.
type command struct {
	path    string
	body    func(c wire.Ctx) any
	out     any
	success func() string
}

// Main runs the CLI for args (args[0] is the subcommand) and returns the exit code.
func Main(ctx context.Context, args []string, env client.Env, stdout, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(ctx, client.Deadline)
	defer cancel()

	cmd, err := parse(args, env)
	if errors.Is(err, errHelp) {
		fmt.Fprint(stdout, usage)
		return 0
	}
	if err != nil {
		return client.Report(stderr, err)
	}

	dir, err := os.Getwd()
	if err != nil {
		dir = "."
	}
	c := client.Gather(ctx, dir, env, "")
	if err := client.Post(ctx, env.BaseURL(), cmd.path, cmd.body(c), cmd.out); err != nil {
		return client.Report(stderr, err)
	}
	fmt.Fprintln(stdout, cmd.success())
	return 0
}

// flagSpec maps a flag name to whether it takes a value.
type flagSpec map[string]bool

type parsedArgs struct {
	pos   []string
	flags map[string]string
}

func (p parsedArgs) text() string { return strings.Join(p.pos, " ") }

func (p parsedArgs) has(name string) bool { _, ok := p.flags[name]; return ok }

// parseArgs accepts --name v, --name=v and bare boolean flags anywhere; "--"
// ends flags; -h and --help request usage. Other tokens are positional.
func parseArgs(args []string, spec flagSpec) (parsedArgs, error) {
	p := parsedArgs{pos: []string{}, flags: map[string]string{}}
	for i := 0; i < len(args); i++ {
		tok := args[i]
		switch {
		case tok == "--":
			p.pos = append(p.pos, args[i+1:]...)
			return p, nil
		case tok == "-h" || tok == "--help":
			return p, errHelp
		case strings.HasPrefix(tok, "--"):
			name, val, hasVal := strings.Cut(tok[2:], "=")
			takesVal, known := spec[name]
			if !known {
				return p, usagef("unknown flag --%s", name)
			}
			switch {
			case !takesVal && hasVal:
				return p, usagef("--%s takes no value", name)
			case takesVal && !hasVal:
				if i+1 >= len(args) {
					return p, usagef("--%s needs a value", name)
				}
				i++
				val = args[i]
			}
			p.flags[name] = val
		default:
			p.pos = append(p.pos, tok)
		}
	}
	return p, nil
}

var specs = map[string]flagSpec{
	"ask":     {"rec": true, "kind": true},
	"answer":  {},
	"dismiss": {},
	"state":   {},
	"log":     {},
	"item":    {"title": true, "pr": true, "state": true, "tier": true},
	"went":    {},
	"run":     {},
	"meter":   {"run": true, "open-start": true, "open-now": true, "filed": true, "closed": true},
	"prune":   {"ended-older": true, "ended-all": false, "quiet": true},
}

func parse(args []string, env client.Env) (command, error) {
	if len(args) == 0 {
		return command{}, usageError("missing command")
	}
	name := args[0]
	if name == "help" || name == "-h" || name == "--help" {
		return command{}, errHelp
	}
	spec, ok := specs[name]
	if !ok {
		return command{}, usagef("unknown command %q", name)
	}
	p, err := parseArgs(args[1:], spec)
	if err != nil {
		return command{}, err
	}
	switch name {
	case "ask":
		return parseAsk(p)
	case "answer":
		return parseAnswer(p)
	case "dismiss":
		return parseDismiss(p)
	case "state":
		return parseState(p, env)
	case "log":
		return parseLog(p)
	case "item":
		return parseItem(p)
	case "went":
		return parseWent(p)
	case "run":
		return parseRun(p)
	case "meter":
		return parseMeter(p)
	default:
		return parsePrune(p)
	}
}

func parseAsk(p parsedArgs) (command, error) {
	q := p.text()
	if q == "" {
		return command{}, usageError("ask needs a question")
	}
	kind := p.flags["kind"]
	switch kind {
	case "", "question", "review", "notify":
	default:
		return command{}, usagef("--kind must be question, review or notify, not %q", kind)
	}
	rec := p.flags["rec"]
	if rec == "" && kind != "notify" {
		return command{}, usageError("ask needs --rec unless --kind notify")
	}
	var resp wire.IDResp
	return command{
		path:    "/api/decisions",
		body:    func(c wire.Ctx) any { return wire.AskPost{Ctx: c, Kind: kind, Question: q, Rec: rec} },
		out:     &resp,
		success: func() string { return strconv.FormatInt(resp.ID, 10) },
	}, nil
}

func parseDecisionID(s string) (int64, error) {
	id, err := strconv.ParseInt(s, 10, 64)
	if err != nil || id < 1 {
		return 0, usagef("bad decision id %q", s)
	}
	return id, nil
}

func parseAnswer(p parsedArgs) (command, error) {
	if len(p.pos) < 2 {
		return command{}, usageError("answer needs <id> <text>")
	}
	id, err := parseDecisionID(p.pos[0])
	if err != nil {
		return command{}, err
	}
	text := strings.Join(p.pos[1:], " ")
	if text == "" {
		return command{}, usageError("answer needs <id> <text>")
	}
	return command{
		path:    fmt.Sprintf("/api/decisions/%d/answer", id),
		body:    func(c wire.Ctx) any { return wire.AnswerPost{Ctx: c, Text: text} },
		out:     &wire.DecisionResp{},
		success: func() string { return fmt.Sprintf("answered %d", id) },
	}, nil
}

func parseDismiss(p parsedArgs) (command, error) {
	if len(p.pos) != 1 {
		return command{}, usageError("dismiss needs exactly one <id>")
	}
	id, err := parseDecisionID(p.pos[0])
	if err != nil {
		return command{}, err
	}
	return command{
		path:    fmt.Sprintf("/api/decisions/%d/dismiss", id),
		body:    func(c wire.Ctx) any { return wire.CtxPost{Ctx: c} },
		out:     &wire.DecisionResp{},
		success: func() string { return fmt.Sprintf("dismissed %d", id) },
	}, nil
}

func parseState(p parsedArgs, env client.Env) (command, error) {
	if env.SessionID == "" {
		return command{}, usageError("state needs CLAUDE_CODE_SESSION_ID")
	}
	note := p.text()
	return command{
		path:    "/api/state",
		body:    func(c wire.Ctx) any { return wire.StatePost{Ctx: c, Note: note} },
		out:     &wire.OKResp{},
		success: func() string { return "ok" },
	}, nil
}

func parseLog(p parsedArgs) (command, error) {
	text := p.text()
	if text == "" {
		return command{}, usageError("log needs text")
	}
	return command{
		path:    "/api/log",
		body:    func(c wire.Ctx) any { return wire.LogPost{Ctx: c, Text: text} },
		out:     &wire.IDResp{},
		success: func() string { return "ok" },
	}, nil
}

var itemRE = regexp.MustCompile(`^(?:[A-Za-z0-9-]+/)?[A-Za-z0-9._-]+#\d+$`)

func parseItem(p parsedArgs) (command, error) {
	if len(p.pos) != 1 || !itemRE.MatchString(p.pos[0]) {
		return command{}, usageError("item needs one <repo>#<N>")
	}
	repo, num, _ := strings.Cut(p.pos[0], "#")
	n, err := strconv.Atoi(num)
	if err != nil {
		return command{}, usagef("bad item number %q", num)
	}
	post := wire.ItemPost{Repo: repo, Number: n}
	if v, ok := p.flags["title"]; ok {
		post.Title = &v
	}
	if v, ok := p.flags["state"]; ok {
		post.State = &v
	}
	if v, ok := p.flags["tier"]; ok {
		post.Tier = &v
	}
	if p.has("pr") {
		pr, err := intFlag(p, "pr")
		if err != nil {
			return command{}, err
		}
		post.PR = &pr
	}
	var resp wire.ItemResp
	return command{
		path:    "/api/items",
		body:    func(c wire.Ctx) any { post.Ctx = c; return post },
		out:     &resp,
		success: func() string { return resp.Key },
	}, nil
}

func parseWent(p parsedArgs) (command, error) {
	if len(p.pos) < 2 || (p.pos[0] != "well" && p.pos[0] != "less") {
		return command{}, usageError("went needs well|less <text>")
	}
	kind := p.pos[0]
	text := strings.Join(p.pos[1:], " ")
	if text == "" {
		return command{}, usageError("went needs well|less <text>")
	}
	return command{
		path:    "/api/notes",
		body:    func(c wire.Ctx) any { return wire.NotePost{Ctx: c, Kind: kind, Text: text} },
		out:     &wire.IDResp{},
		success: func() string { return "ok" },
	}, nil
}

func parseRun(p parsedArgs) (command, error) {
	var path, verb string
	switch {
	case len(p.pos) == 2 && p.pos[0] == "start":
		path, verb = "/api/runs/start", "started"
	case (len(p.pos) == 1 || len(p.pos) == 2) && p.pos[0] == "end":
		path, verb = "/api/runs/end", "ended"
	default:
		return command{}, usageError("run needs start <name> or end [<name>]")
	}
	name := ""
	if len(p.pos) == 2 {
		name = p.pos[1]
	}
	var resp wire.Run
	return command{
		path:    path,
		body:    func(c wire.Ctx) any { return wire.RunPost{Ctx: c, Name: name} },
		out:     &resp,
		success: func() string { return fmt.Sprintf("run %s %s", resp.Name, verb) },
	}, nil
}

func parseMeter(p parsedArgs) (command, error) {
	if len(p.pos) != 0 {
		return command{}, usagef("meter takes no argument %q", p.pos[0])
	}
	post := wire.MeterPost{Run: p.flags["run"]}
	fields := []struct {
		flag string
		dst  **int
	}{
		{"open-start", &post.OpenStart},
		{"open-now", &post.OpenNow},
		{"filed", &post.Filed},
		{"closed", &post.Closed},
	}
	given := false
	for _, f := range fields {
		if !p.has(f.flag) {
			continue
		}
		v, err := intFlag(p, f.flag)
		if err != nil {
			return command{}, err
		}
		*f.dst = &v
		given = true
	}
	if !given {
		return command{}, usageError("meter needs at least one of --open-start, --open-now, --filed, --closed")
	}
	return command{
		path:    "/api/meter",
		body:    func(c wire.Ctx) any { post.Ctx = c; return post },
		out:     &wire.OKResp{},
		success: func() string { return "ok" },
	}, nil
}

func parsePrune(p parsedArgs) (command, error) {
	if len(p.pos) != 0 {
		return command{}, usagef("prune takes no argument %q", p.pos[0])
	}
	var modes []string
	for _, m := range []string{"ended-older", "ended-all", "quiet"} {
		if p.has(m) {
			modes = append(modes, m)
		}
	}
	if len(modes) != 1 {
		return command{}, usageError("prune needs exactly one of --ended-older DUR, --ended-all, --quiet DUR")
	}
	post := wire.PrunePost{Mode: strings.ReplaceAll(modes[0], "-", "_")}
	if modes[0] != "ended-all" {
		d, err := time.ParseDuration(p.flags[modes[0]])
		if err != nil {
			return command{}, usagef("--%s needs a Go duration like 24h", modes[0])
		}
		post.OlderThanS = int64(d / time.Second)
	}
	var resp wire.PruneResp
	return command{
		path:    "/api/prune",
		body:    func(c wire.Ctx) any { post.Ctx = c; return post },
		out:     &resp,
		success: func() string { return fmt.Sprintf("cleared %d", resp.Deleted) },
	}, nil
}

func intFlag(p parsedArgs, name string) (int, error) {
	v, err := strconv.Atoi(p.flags[name])
	if err != nil {
		return 0, usagef("--%s needs an integer", name)
	}
	return v, nil
}
