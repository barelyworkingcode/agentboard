package hookstate_test

import (
	"testing"
	"time"

	"github.com/barelyworkingcode/agentboard/internal/hookstate"
	"github.com/barelyworkingcode/agentboard/internal/wire"
)

const (
	t0 = int64(1_800_000_000_000)
	at = t0 + 5_000
)

func hp(event string, mod ...func(*wire.HookPost)) wire.HookPost {
	h := wire.HookPost{Ctx: wire.Ctx{Session: "s1"}, Event: event}
	for _, m := range mod {
		m(&h)
	}
	return h
}

func tool(name, id string) func(*wire.HookPost) {
	return func(h *wire.HookPost) { h.Tool, h.ToolUseID = name, id }
}
func notif(kind string) func(*wire.HookPost) {
	return func(h *wire.HookPost) { h.NotificationType = kind }
}

var (
	inBash  = wire.HookState{State: "active", Tool: "Bash", ToolUseID: "tu1", ToolSince: t0, LastSeenAt: t0}
	waiting = wire.HookState{State: "waiting", WaitingOn: "permission_prompt", Tool: "Bash", ToolUseID: "tu1", ToolSince: t0, LastSeenAt: t0}
	idle    = wire.HookState{State: "idle", LastSeenAt: t0}
	ended   = wire.HookState{State: "ended", LastSeenAt: t0, EndedAt: t0}
)

func with(s wire.HookState, f func(*wire.HookState)) wire.HookState { f(&s); return s }

type applyCase struct {
	name   string
	cur    wire.HookState
	exists bool
	h      wire.HookPost
	want   wire.HookState
	note   string // expected ev.Note; checked when non-empty
}

func TestApply(t *testing.T) {
	cases := []applyCase{
		{"SessionStart new session is idle", wire.HookState{}, false,
			hp("SessionStart", func(h *wire.HookPost) { h.Source = "startup" }),
			wire.HookState{State: "idle", LastSeenAt: at}, "startup"},
		{"SessionStart revives ended", ended, true,
			hp("SessionStart", func(h *wire.HookPost) { h.Source = "resume" }),
			wire.HookState{State: "idle", LastSeenAt: at}, "resume"},
		{"SessionStart clears a tool", inBash, true, hp("SessionStart"),
			wire.HookState{State: "idle", LastSeenAt: at}, ""},
		{"UserPromptSubmit clears a stuck tool", inBash, true, hp("UserPromptSubmit"),
			wire.HookState{State: "active", LastSeenAt: at}, ""},
		{"UserPromptSubmit clears waiting", waiting, true, hp("UserPromptSubmit"),
			wire.HookState{State: "active", LastSeenAt: at}, ""},
		{"PreToolUse from idle", idle, true, hp("PreToolUse", tool("Bash", "tu2")),
			wire.HookState{State: "active", Tool: "Bash", ToolUseID: "tu2", ToolSince: at, LastSeenAt: at}, ""},
		{"PreToolUse clears waiting", waiting, true, hp("PreToolUse", tool("Read", "tu2")),
			wire.HookState{State: "active", Tool: "Read", ToolUseID: "tu2", ToolSince: at, LastSeenAt: at}, ""},
		{"PostToolUse matching id", inBash, true, hp("PostToolUse", tool("Bash", "tu1")),
			wire.HookState{State: "active", LastSeenAt: at}, ""},
		{"PostToolUseFailure matching id releases waiting", waiting, true, hp("PostToolUseFailure", tool("Bash", "tu1")),
			wire.HookState{State: "active", LastSeenAt: at}, ""},
		{"PostToolUse matching id keeps idle", with(inBash, func(s *wire.HookState) { s.State = "idle" }), true,
			hp("PostToolUse", tool("Bash", "tu1")),
			wire.HookState{State: "idle", LastSeenAt: at}, ""},
		{"PostToolUse other id changes nothing", inBash, true, hp("PostToolUse", tool("Read", "tu9")),
			with(inBash, func(s *wire.HookState) { s.LastSeenAt = at }), ""},
		{"PostToolUseFailure other id keeps waiting", waiting, true, hp("PostToolUseFailure", tool("Read", "tu9")),
			with(waiting, func(s *wire.HookState) { s.LastSeenAt = at }), ""},
		{"Notification other type changes nothing", inBash, true, hp("Notification", notif("idle_prompt")),
			with(inBash, func(s *wire.HookState) { s.LastSeenAt = at }), ""},
		{"Stop goes idle and clears the tool", inBash, true, hp("Stop"),
			wire.HookState{State: "idle", LastSeenAt: at}, ""},
		{"SessionEnd", inBash, true, hp("SessionEnd", func(h *wire.HookPost) { h.Reason = "logout" }),
			wire.HookState{State: "ended", LastSeenAt: at, EndedAt: at}, "logout"},
	}
	for _, kind := range []string{"permission_prompt", "agent_needs_input", "elicitation_dialog", "elicitation_url_dialog"} {
		cases = append(cases, applyCase{"Notification " + kind + " waits", inBash, true, hp("Notification", notif(kind)),
			with(inBash, func(s *wire.HookState) { s.State, s.WaitingOn, s.LastSeenAt = "waiting", kind, at }), kind})
	}

	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			next, ev, ok := hookstate.Apply(c.cur, c.exists, c.h, at)
			if !ok {
				t.Fatalf("ok = false, want the event accepted")
			}
			if next != c.want {
				t.Errorf("next =\n  %+v\nwant\n  %+v", next, c.want)
			}
			changed := !c.exists || next.State != c.cur.State
			if changed && ev == nil {
				t.Fatalf("ev = nil, want an event when the state changes or the session is new")
			}
			if changed && ev.State != next.State {
				t.Errorf("ev.State = %q, want %q", ev.State, next.State)
			}
			if c.note != "" && (ev == nil || ev.Note != c.note) {
				t.Errorf("ev = %+v, want note %q", ev, c.note)
			}
		})
	}
}

func TestApplyIgnoresEndedSessions(t *testing.T) {
	for _, event := range []string{"UserPromptSubmit", "PreToolUse", "PostToolUse", "PostToolUseFailure", "Notification", "Stop", "SessionEnd"} {
		h := hp(event, tool("Bash", "tu1"), notif("permission_prompt"))
		if _, _, ok := hookstate.Apply(ended, true, h, at); ok {
			t.Errorf("%s on an ended session: ok = true, want ignored", event)
		}
	}
}

func TestDisplay(t *testing.T) {
	quiet := wire.QuietAfter.Milliseconds()
	sess := func(hs wire.HookState) wire.Session { return wire.Session{ID: "s1", HookState: hs} }
	active := wire.HookState{State: "active", LastSeenAt: t0}
	cases := []struct {
		name    string
		s       wire.HookState
		open    bool
		now     int64
		display string
	}{
		{"ended beats an open decision", ended, true, t0 + 10*quiet, "ended"},
		{"waiting state", waiting, false, t0, "waiting"},
		{"open decision on idle", idle, true, t0, "waiting"},
		{"open decision beats quiet", active, true, t0 + 2*quiet, "waiting"},
		{"active just under the quiet line", active, false, t0 + quiet - 1, "active"},
		{"active at the quiet line", active, false, t0 + quiet, "quiet"},
		{"inside a long tool is never quiet", inBash, false, t0 + 10*quiet, "active"},
		{"idle is never quiet", idle, false, t0 + 10*quiet, "idle"},
	}
	for _, c := range cases {
		if got := hookstate.Display(sess(c.s), c.open, c.now); got != c.display {
			t.Errorf("%s: Display = %q, want %q", c.name, got, c.display)
		}
	}
}

func TestDisplayAfterHookSequences(t *testing.T) {
	later := at + time.Hour.Milliseconds()
	cases := []struct {
		name    string
		events  []wire.HookPost
		display string
	}{
		{"SessionStart only never goes quiet", []wire.HookPost{hp("SessionStart")}, "idle"},
		{"UserPromptSubmit after an interrupted tool goes quiet",
			[]wire.HookPost{hp("SessionStart"), hp("PreToolUse", tool("Bash", "tu1")), hp("UserPromptSubmit")}, "quiet"},
		{"long tool call stays active",
			[]wire.HookPost{hp("SessionStart"), hp("UserPromptSubmit"), hp("PreToolUse", tool("Bash", "tu1"))}, "active"},
	}
	for _, c := range cases {
		var cur wire.HookState
		exists := false
		for _, h := range c.events {
			next, _, ok := hookstate.Apply(cur, exists, h, at)
			if !ok {
				t.Fatalf("%s: %s refused", c.name, h.Event)
			}
			cur, exists = next, true
		}
		if got := hookstate.Display(wire.Session{ID: "s1", HookState: cur}, false, later); got != c.display {
			t.Errorf("%s: Display = %q, want %q", c.name, got, c.display)
		}
	}
}
