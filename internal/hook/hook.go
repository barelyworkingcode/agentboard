// Package hook adapts a Claude Code hook invocation (JSON on stdin) into a
// POST to /api/hook, plus POSTs to /api/items for gh commands a Bash tool ran.
package hook

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/barelyworkingcode/agentboard/internal/client"
	"github.com/barelyworkingcode/agentboard/internal/ghparse"
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

// input holds the stdin fields posted to /api/hook. Prompts, messages and
// transcript paths are never decoded; the Bash command and stdout are decoded
// only by bashFields, for ghparse, and never posted.
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

// bash holds the Bash fields of a PostToolUse input. A PostToolUse for Bash
// means exit 0; there is no exit-code field.
type bash struct {
	ToolInput struct {
		Command string `json:"command"`
	} `json:"tool_input"`
	ToolResponse struct {
		Stdout      string `json:"stdout"`
		Interrupted bool   `json:"interrupted"`
	} `json:"tool_response"`
}

// bashFields decodes the Bash command, stdout and exit code from stdin. ok is
// false when they are missing or malformed.
func bashFields(stdin []byte) (command, stdout string, exitCode int, ok bool) {
	var b bash
	if err := json.Unmarshal(stdin, &b); err != nil || b.ToolInput.Command == "" {
		return "", "", 0, false
	}
	if b.ToolResponse.Interrupted {
		exitCode = 1
	}
	return b.ToolInput.Command, b.ToolResponse.Stdout, exitCode, true
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
	if h.Event != "PostToolUse" || h.Tool != "Bash" {
		return 0
	}
	command, stdout, exitCode, ok := bashFields(raw)
	if !ok {
		return 0
	}
	for _, item := range ghparse.Parse(command, stdout, exitCode, h.Ctx) {
		client.Report(stderr, client.Post(ctx, env.BaseURL(), "/api/items", item, nil))
	}
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
