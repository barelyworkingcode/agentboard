// Package hookstate is the pure state machine that turns Claude Code hook
// events into a session's state, and the rule that picks what the board shows.
package hookstate

import "github.com/barelyworkingcode/agentboard/internal/wire"

const (
	Active  = "active"
	Waiting = "waiting"
	Idle    = "idle"
	Ended   = "ended"
	Quiet   = "quiet"
)

// waitingTypes are the notification types that mean the session waits on the user.
var waitingTypes = map[string]bool{
	"permission_prompt":      true,
	"agent_needs_input":      true,
	"elicitation_dialog":     true,
	"elicitation_url_dialog": true,
}

// Apply advances cur by one hook event received at at (unix ms). ok=false
// means the event is ignored entirely: nothing is written, not even a heartbeat.
// ev is non-nil for a new session and whenever the state changes.
func Apply(cur wire.HookState, exists bool, h wire.HookPost, at int64) (next wire.HookState, ev *wire.Event, ok bool) {
	if !exists {
		cur = wire.HookState{State: Idle}
	}
	if cur.State == Ended && h.Event != "SessionStart" {
		return cur, nil, false
	}

	next = cur
	note := ""
	switch h.Event {
	case "SessionStart":
		next.State = Idle
		next.EndedAt = 0
		clearTool(&next)
		next.WaitingOn = ""
		note = h.Source
	case "UserPromptSubmit":
		next.State = Active
		clearTool(&next)
		next.WaitingOn = ""
	case "PreToolUse":
		next.State = Active
		next.Tool = h.Tool
		next.ToolUseID = h.ToolUseID
		next.ToolSince = at
		next.WaitingOn = ""
	case "PostToolUse", "PostToolUseFailure":
		// An empty id matches nothing, so a Post without an id can't clear
		// state or end a wait it has no part in.
		if h.ToolUseID != "" && h.ToolUseID == cur.ToolUseID {
			if next.State == Waiting {
				next.State = Active
			}
			clearTool(&next)
			next.WaitingOn = ""
		}
	case "Notification":
		if waitingTypes[h.NotificationType] {
			next.State = Waiting
			next.WaitingOn = h.NotificationType
			note = h.NotificationType
		}
	case "Stop":
		next.State = Idle
		clearTool(&next)
		next.WaitingOn = ""
	case "SessionEnd":
		next.State = Ended
		next.EndedAt = at
		clearTool(&next)
		next.WaitingOn = ""
		note = h.Reason
	default:
		return cur, nil, false
	}

	next.LastSeenAt = at
	if !exists || next.State != cur.State {
		ev = &wire.Event{At: at, Kind: "state", State: next.State, Note: note}
	}
	return next, ev, true
}

func clearTool(s *wire.HookState) {
	s.Tool = ""
	s.ToolUseID = ""
	s.ToolSince = 0
}

// Display is what the board shows for s at now (unix ms).
func Display(s wire.Session, hasOpenDecision bool, now int64) string {
	switch {
	case s.State == Ended:
		return Ended
	case s.State == Waiting || hasOpenDecision:
		return Waiting
	case s.State == Active && s.Tool == "" && now-s.LastSeenAt >= wire.QuietAfter.Milliseconds():
		return Quiet
	default:
		return s.State
	}
}
