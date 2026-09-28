package cli_test

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/barelyworkingcode/agentboard/internal/cli"
	"github.com/barelyworkingcode/agentboard/internal/client"
)

// fakeServer answers every request with status and body, and counts hits.
func fakeServer(t *testing.T, status int, body string) (*httptest.Server, *atomic.Int32, *atomic.Value) {
	var hits atomic.Int32
	var path atomic.Value
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		path.Store(r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(status)
		w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv, &hits, &path
}

func run(url string, args ...string) (code int, stdout, stderr string) {
	var out, errb bytes.Buffer
	env := client.Env{URL: url, Machine: "devbox", SessionID: "sess-1"}
	code = cli.Main(context.Background(), args, env, &out, &errb)
	return code, out.String(), errb.String()
}

func oneLine(t *testing.T, stderr string) {
	t.Helper()
	if strings.Count(stderr, "\n") != 1 || !strings.HasSuffix(stderr, "\n") {
		t.Errorf("stderr = %q, want exactly one line", stderr)
	}
}

func TestExitCodeByServerStatus(t *testing.T) {
	cases := []struct {
		status int
		body   string
		code   int
		stderr string // prefix
	}{
		{400, `{"error":"empty text"}`, 1, "agentboard: empty text (400)"},
		{404, `{"error":"not found"}`, 1, "agentboard: not found (404)"},
		{409, `{"error":"decision already closed"}`, 1, "agentboard: decision already closed (409)"},
		{413, `{"error":"body too large"}`, 1, "agentboard: body too large (413)"},
		{403, `{"error":"ingest not allowed from this address"}`, 0, "agentboard: not recorded: "},
		{500, `{"error":"boom"}`, 0, "agentboard: not recorded: "},
		{503, ``, 0, "agentboard: not recorded: "},
	}
	for _, c := range cases {
		srv, _, _ := fakeServer(t, c.status, c.body)
		code, stdout, stderr := run(srv.URL, "log", "hello", "world")
		if code != c.code || stdout != "" || !strings.HasPrefix(stderr, c.stderr) {
			t.Errorf("status %d: exit=%d stdout=%q stderr=%q, want exit=%d, empty stdout, stderr %q…",
				c.status, code, stdout, stderr, c.code, c.stderr)
		}
		oneLine(t, stderr)
	}
}

func TestSuccessOutput(t *testing.T) {
	cases := []struct {
		args         []string
		body         string
		path, stdout string
	}{
		{[]string{"log", "hello"}, `{"id":3}`, "/api/log", "ok\n"},
		{[]string{"ask", "Ship", "it?", "--rec", "yes"}, `{"id":42}`, "/api/decisions", "42\n"},
		{[]string{"answer", "42", "go"}, `{"id":42,"status":"answered"}`, "/api/decisions/42/answer", "answered 42\n"},
		{[]string{"dismiss", "42"}, `{"id":42,"status":"dismissed"}`, "/api/decisions/42/dismiss", "dismissed 42\n"},
	}
	for _, c := range cases {
		srv, _, path := fakeServer(t, 200, c.body)
		code, stdout, stderr := run(srv.URL+"/", c.args...)
		if code != 0 || stdout != c.stdout || path.Load() != c.path {
			t.Errorf("%v: exit=%d stdout=%q path=%v stderr=%q, want 0 %q %s", c.args, code, stdout, path.Load(), stderr, c.stdout, c.path)
		}
	}
}

func TestUsageErrorsExitTwoWithoutNetwork(t *testing.T) {
	cases := [][]string{
		{"ask", "Ship it?"}, // --rec required unless notify
		{"meter", "--run", "r1"},
		{"prune", "--ended-all", "--quiet", "2h"},
		{"prune", "--quiet", "soon"},
		{"item", "not-an-item"},
		{"answer", "abc", "text"},
		{"no-such-command"},
	}
	for _, args := range cases {
		srv, hits, _ := fakeServer(t, 200, `{"ok":true}`)
		code, stdout, stderr := run(srv.URL, args...)
		if code != 2 || hits.Load() != 0 || stdout != "" || !strings.HasSuffix(strings.TrimSpace(stderr), "; see agentboard help") {
			t.Errorf("%v: exit=%d hits=%d stdout=%q stderr=%q, want exit 2, no request, usage line", args, code, hits.Load(), stdout, stderr)
		}
	}
}

func TestStateNeedsSession(t *testing.T) {
	srv, hits, _ := fakeServer(t, 200, `{"ok":true}`)
	var out, errb bytes.Buffer
	code := cli.Main(context.Background(), []string{"state", "review round 1"}, client.Env{URL: srv.URL}, &out, &errb)
	if code != 2 || hits.Load() != 0 {
		t.Errorf("state without CLAUDE_CODE_SESSION_ID: exit=%d hits=%d stderr=%q, want exit 2 and no request", code, hits.Load(), errb.String())
	}
}
