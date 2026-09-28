package integration

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/agentboard/internal/server"
	"github.com/barelyworkingcode/agentboard/internal/wire"
)

func TestIngestGate(t *testing.T) {
	lan := []netip.Prefix{netip.MustParsePrefix("192.0.2.0/24")}
	spoof := []string{"X-Forwarded-For", "192.0.2.50", "X-Real-IP", "192.0.2.50", "Forwarded", "for=192.0.2.50"}
	cases := []struct {
		name   string
		ingest bool
		allow  []netip.Prefix
		header []string
		want   int
	}{
		{"ingest listener, loopback source", true, loopbackAllow, nil, 200},
		{"read-only listener", false, loopbackAllow, nil, 403},
		{"read-only listener, forwarded loopback", false, loopbackAllow, []string{"X-Forwarded-For", "127.0.0.1"}, 403},
		{"ingest listener, source outside allow", true, lan, nil, 403},
		{"ingest listener, spoofed allowed source", true, lan, spoof, 403},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			s := startHandler(t, c.ingest, c.allow)
			code, body := post(t, s.url+"/api/log", wire.LogPost{Text: "hello"}, c.header...)
			if code != c.want {
				t.Fatalf("POST /api/log = %d %s, want %d", code, body, c.want)
			}
			if c.want == 403 && !strings.Contains(string(body), "ingest not allowed from this address") {
				t.Errorf("refusal body = %s", body)
			}
			for _, path := range []string{"/", "/api/board"} {
				resp, err := http.Get(s.url + path)
				if err != nil || resp.StatusCode != 200 {
					t.Errorf("GET %s = %v %v, want 200 on every listener", path, resp.StatusCode, err)
				}
				resp.Body.Close()
			}
		})
	}
}

func TestHookSequenceSetsAndClearsWaiting(t *testing.T) {
	s := startHandler(t, true, loopbackAllow)
	for _, h := range []wire.HookPost{
		hook("s1", "SessionStart"),
		hook("s1", "PreToolUse", func(h *wire.HookPost) { h.Tool, h.ToolUseID = "Bash", "tu1" }),
		hook("s1", "Notification", func(h *wire.HookPost) { h.NotificationType = "agent_needs_input" }),
	} {
		if code, b := post(t, s.url+"/api/hook", h); code != 204 {
			t.Fatalf("%s = %d %s, want 204", h.Event, code, b)
		}
	}
	b, _ := board(t, s.url)
	if got, _ := session(b, "s1"); got.Display != "waiting" || got.WaitingOn != "agent_needs_input" {
		t.Fatalf("after Notification: %+v, want waiting on agent_needs_input", got)
	}
	mustPost(t, s.url+"/api/hook", hook("s1", "PreToolUse", func(h *wire.HookPost) { h.Tool, h.ToolUseID = "Read", "tu2" }))
	b, _ = board(t, s.url)
	if got, _ := session(b, "s1"); got.Display != "active" || got.WaitingOn != "" || got.Tool != "Read" {
		t.Errorf("after the next PreToolUse: %+v, want active in Read", got)
	}

	for _, bad := range []wire.HookPost{hook("s1", "Bogus"), hook("", "SessionStart")} {
		if code, _ := post(t, s.url+"/api/hook", bad); code != 400 {
			t.Errorf("hook %+v = %d, want 400", bad, code)
		}
	}
}

func TestQuietFlipBumpsVersion(t *testing.T) {
	defer func(d time.Duration) { server.QuietTick = d }(server.QuietTick)
	server.QuietTick = 50 * time.Millisecond
	s := startHandler(t, true, loopbackAllow)
	mustPost(t, s.url+"/api/hook", hook("idle-in-turn", "SessionStart"))
	mustPost(t, s.url+"/api/hook", hook("idle-in-turn", "UserPromptSubmit"))
	mustPost(t, s.url+"/api/hook", hook("in-tool", "SessionStart"))
	mustPost(t, s.url+"/api/hook", hook("in-tool", "PreToolUse", func(h *wire.HookPost) { h.Tool, h.ToolUseID = "Bash", "tu1" }))

	before, _ := board(t, s.url)
	if got, _ := session(before, "idle-in-turn"); got.Display != "active" {
		t.Fatalf("fresh session display = %q, want active", got.Display)
	}
	s.now.Add((wire.QuietAfter + time.Minute).Milliseconds())

	var after wire.Board
	waitFor(t, 2*time.Second, func() bool {
		after, _ = board(t, s.url)
		return after.Version != before.Version
	}, func() string { return "version never bumped after the quiet set changed: " + before.Version })
	if got, _ := session(after, "idle-in-turn"); got.Display != "quiet" {
		t.Errorf("idle-in-turn display = %q, want quiet", got.Display)
	}
	if got, _ := session(after, "in-tool"); got.Display != "active" {
		t.Errorf("in-tool display = %q, want active (inside a long tool call)", got.Display)
	}
}

func TestBoardETag(t *testing.T) {
	s := startHandler(t, true, loopbackAllow)
	_, etag := board(t, s.url)
	if !strings.HasPrefix(etag, `"`) || len(etag) < 3 {
		t.Fatalf("ETag = %q, want a quoted version", etag)
	}
	get := func(tag string) *http.Response {
		req, _ := http.NewRequest("GET", s.url+"/api/board", nil)
		req.Header.Set("If-None-Match", tag)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		return resp
	}
	if resp := get(etag); resp.StatusCode != 304 {
		t.Errorf("matching If-None-Match = %d, want 304", resp.StatusCode)
	}
	mustPost(t, s.url+"/api/log", wire.LogPost{Text: "changed"})
	if resp := get(etag); resp.StatusCode != 200 || resp.Header.Get("ETag") == etag {
		t.Errorf("stale If-None-Match = %d ETag %q, want 200 with a new ETag", resp.StatusCode, resp.Header.Get("ETag"))
	}
}

func TestSSEEventWithinOneSecondOfPost(t *testing.T) {
	s := startHandler(t, true, loopbackAllow)
	events, resp := sseVersions(t, s.url+"/events")
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/event-stream") {
		t.Fatalf("Content-Type = %q", ct)
	}
	var first string
	select {
	case first = <-events:
	case <-time.After(time.Second):
		t.Fatal("no board event on connect")
	}
	mustPost(t, s.url+"/api/log", wire.LogPost{Text: "hello"})
	start := time.Now()
	for {
		select {
		case v, ok := <-events:
			if !ok {
				t.Fatal("stream closed")
			}
			if v == first {
				continue
			}
			if _, etag := board(t, s.url); etag != `"`+v+`"` {
				t.Errorf("event version %q, board ETag %s: the latest version was dropped", v, etag)
			}
			return
		case <-time.After(time.Second - time.Since(start)):
			t.Fatal("no board event within 1 s of a POST")
		}
	}
}

func TestBrowserActions(t *testing.T) {
	s := startHandler(t, true, loopbackAllow)
	origin := s.url // httptest serves on http://127.0.0.1:<port>
	good := []string{"Origin", origin, "X-Agentboard", "1"}

	for _, c := range []struct {
		name   string
		header []string
	}{
		{"no Origin, no header", nil},
		{"no header", []string{"Origin", origin}},
		{"no Origin", []string{"X-Agentboard", "1"}},
		{"foreign Origin", []string{"Origin", "http://198.51.100.7:8790", "X-Agentboard", "1"}},
	} {
		code, body := post(t, s.url+"/ui/clear", wire.ClearPost{Mode: "ended_all"}, c.header...)
		if code != 403 || !strings.Contains(string(body), "cross-origin request refused") {
			t.Errorf("/ui/clear, %s = %d %s, want 403", c.name, code, body)
		}
	}
	if code, body := post(t, s.url+"/ui/clear", wire.ClearPost{Mode: "ended_all"}, good...); code != 200 {
		t.Fatalf("/ui/clear same-origin = %d %s, want 200", code, body)
	}

	req, _ := http.NewRequest("OPTIONS", s.url+"/ui/clear", nil)
	req.Header.Set("Origin", "http://198.51.100.7:8790")
	req.Header.Set("Access-Control-Request-Method", "POST")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != 405 || resp.Header.Get("Access-Control-Allow-Origin") != "" {
		t.Errorf("OPTIONS preflight = %d %v, want 405 without CORS headers", resp.StatusCode, resp.Header)
	}

	mustPost(t, s.url+"/api/hook", hook("s1", "SessionStart"))
	var ask wire.IDResp
	json.Unmarshal(mustPost(t, s.url+"/api/decisions", wire.AskPost{Ctx: wire.Ctx{Session: "s1"}, Question: "Merge?", Rec: "yes"}), &ask)

	del := fmt.Sprintf("%s/ui/sessions/%s/delete", s.url, "s1")
	if code, body := post(t, del, nil, good...); code != 409 || !strings.Contains(string(body), "session has an open decision") {
		t.Fatalf("delete with an open decision = %d %s, want 409", code, body)
	}
	if code, body := post(t, del, nil); code != 403 {
		t.Errorf("delete without Origin = %d %s, want 403", code, body)
	}
	if code, _ := post(t, s.url+"/ui/sessions/nope/delete", nil, good...); code != 404 {
		t.Errorf("delete unknown session = %d, want 404", code)
	}
	var dr wire.DecisionResp
	code, body := post(t, fmt.Sprintf("%s/ui/decisions/%d/dismiss", s.url, ask.ID), nil, good...)
	if json.Unmarshal(body, &dr); code != 200 || dr.Status != "dismissed" {
		t.Fatalf("dismiss from the page = %d %s", code, body)
	}
	if code, body := post(t, del, nil, good...); code != 200 || !strings.Contains(string(body), `"deleted":1`) {
		t.Errorf("delete after dismiss = %d %s, want 200 {\"deleted\":1}", code, body)
	}
	if b, _ := board(t, s.url); len(b.Sessions) != 0 {
		t.Errorf("sessions after delete = %+v", b.Sessions)
	}
}
