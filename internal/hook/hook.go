// Package hook adapts a Claude Code hook invocation (JSON on stdin) into a
// POST to /api/hook.
package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/barelyworkingcode/agentboard/internal/client"
	"github.com/barelyworkingcode/agentboard/internal/wire"
)

// posted is the set of hook events that reach the server.
var posted = map[string]bool{
	"SessionStart":       true,
	"UserPromptSubmit":   true,
	"PreToolUse":         true,
	"PostToolUse":        true,
	"PostToolUseFailure": true,
	"Notification":       true,
	"Stop":               true,
	"SessionEnd":         true,
}

// input holds only the stdin fields the hook uses. Prompts, tool input and
// output, messages and transcript paths are never decoded.
type input struct {
	SessionID        string `json:"session_id"`
	Cwd              string `json:"cwd"`
	HookEventName    string `json:"hook_event_name"`
	ToolName         string `json:"tool_name"`
	ToolUseID        string `json:"tool_use_id"`
	NotificationType string `json:"notification_type"`
	Source           string `json:"source"`
	Reason           string `json:"reason"`
}

// Parse decodes a hook's stdin. h.Ctx is left empty except for Session.
func Parse(stdin []byte) (h wire.HookPost, cwd string, err error) {
	var in input
	if err := json.Unmarshal(stdin, &in); err != nil {
		return wire.HookPost{}, "", fmt.Errorf("decode hook input: %w", err)
	}
	h = wire.HookPost{
		Ctx:              wire.Ctx{Session: in.SessionID},
		Event:            in.HookEventName,
		Tool:             in.ToolName,
		ToolUseID:        in.ToolUseID,
		NotificationType: in.NotificationType,
		Source:           in.Source,
		Reason:           in.Reason,
	}
	return h, in.Cwd, nil
}

// Main runs `agentboard hook`. It always returns 0 and never writes stdout,
// because Claude Code adds a hook's stdout to the model's context.
func Main(ctx context.Context, stdin io.Reader, env client.Env, stderr io.Writer) int {
	ctx, cancel := context.WithTimeout(ctx, client.Deadline)
	defer cancel()

	raw, err := readAll(ctx, stdin)
	if err != nil {
		fmt.Fprintf(stderr, "agentboard: %v\n", err)
		return 0
	}
	h, cwd, err := Parse(raw)
	if err != nil {
		fmt.Fprintf(stderr, "agentboard: %v\n", err)
		return 0
	}
	if h.Ctx.Session == "" || !posted[h.Event] {
		return 0
	}
	h.Ctx = client.Gather(ctx, cwd, env, h.Ctx.Session)
	client.Report(stderr, client.Post(ctx, env.BaseURL(), "/api/hook", h, nil))
	return 0
}

// readAll reads r to EOF or until ctx ends. On timeout the reading goroutine
// is abandoned; the process exits right after.
func readAll(ctx context.Context, r io.Reader) ([]byte, error) {
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() {
		b, err := io.ReadAll(r)
		done <- result{b, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			return nil, fmt.Errorf("read hook input: %w", res.err)
		}
		return res.b, nil
	case <-ctx.Done():
		return nil, fmt.Errorf("read hook input: %w", ctx.Err())
	}
}
