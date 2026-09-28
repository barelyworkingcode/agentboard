package integration

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"testing/fstest"
	"time"

	"github.com/barelyworkingcode/agentboard/internal/server"
	"github.com/barelyworkingcode/agentboard/internal/store"
	"github.com/barelyworkingcode/agentboard/internal/wire"
)

var (
	binPath  string
	homeDir  string
	buildErr error
)

func TestMain(m *testing.M) {
	dir, err := os.MkdirTemp("", "agentboard-it-")
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	binPath, homeDir = filepath.Join(dir, "agentboard"), filepath.Join(dir, "home")
	os.Mkdir(homeDir, 0o700)
	if runtime.GOOS == "windows" {
		binPath += ".exe"
	}
	cmd := exec.Command("go", "build", "-trimpath", "-ldflags", "-s -w", "-o", binPath, "github.com/barelyworkingcode/agentboard")
	cmd.Env = append(os.Environ(), "CGO_ENABLED=0")
	if out, err := cmd.CombinedOutput(); err != nil {
		buildErr = fmt.Errorf("%v\n%s", err, out)
	} else {
		// Deliberate: the first exec of a fresh binary pays for OS malware
		// scanning. Timing assertions are about agentboard, not that.
		exec.Command(binPath, "help").Run()
	}
	code := m.Run()
	os.RemoveAll(dir)
	os.Exit(code)
}

func binary(t testing.TB) string {
	t.Helper()
	if buildErr != nil {
		t.Fatalf("build agentboard: %v", buildErr)
	}
	return binPath
}

var assets = fstest.MapFS{
	"index.html": {Data: []byte("<!doctype html><title>agentboard</title>")},
	"app.js":     {Data: []byte("export {}")},
	"app.css":    {Data: []byte("body{}")},
}

func freePort(t testing.TB) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	return l.Addr().String()
}

// childEnv is a clean environment: the test's own session, tmux and proxy
// variables must not leak into the binary.
func childEnv(extra ...string) []string {
	return append([]string{"PATH=" + os.Getenv("PATH"), "HOME=" + homeDir, "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=" + os.DevNull}, extra...)
}

type result struct {
	code           int
	stdout, stderr string
	elapsed        time.Duration
}

func runBin(t testing.TB, dir, stdin string, env []string, args ...string) result {
	t.Helper()
	cmd := exec.Command(binary(t), args...)
	cmd.Dir, cmd.Env = dir, env
	cmd.Stdin = strings.NewReader(stdin)
	var out, errb bytes.Buffer
	cmd.Stdout, cmd.Stderr = &out, &errb
	start := time.Now()
	err := cmd.Run()
	r := result{stdout: out.String(), stderr: errb.String(), elapsed: time.Since(start)}
	if ee, ok := err.(*exec.ExitError); ok {
		r.code = ee.ExitCode()
	} else if err != nil {
		t.Fatalf("exec %v: %v", args, err)
	}
	return r
}

// startServe runs the real binary on a free loopback port and returns its base URL.
func startServe(t testing.TB, extra ...string) (string, *exec.Cmd) {
	t.Helper()
	addr := freePort(t)
	args := append([]string{"serve", "--listen", addr, "--db", filepath.Join(t.TempDir(), "board.db")}, extra...)
	cmd := exec.Command(binary(t), args...)
	cmd.Env = childEnv()
	var logs syncBuffer
	cmd.Stderr = &logs
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cmd.Process.Signal(syscall.SIGTERM)
		done := make(chan struct{})
		go func() { cmd.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			cmd.Process.Kill()
		}
	})
	base := "http://" + addr
	waitFor(t, 10*time.Second, func() bool {
		resp, err := http.Get(base + "/api/board")
		if err != nil {
			return false
		}
		resp.Body.Close()
		return resp.StatusCode == 200
	}, func() string { return "serve never answered; logs:\n" + logs.String() })
	return base, cmd
}

// inproc is a handler served by httptest, with an injectable clock.
type inproc struct {
	url string
	now *atomic.Int64 // unix ms
}

func startHandler(t *testing.T, ingest bool, allow []netip.Prefix) *inproc {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), "board.db"))
	if err != nil {
		t.Fatal(err)
	}
	now := &atomic.Int64{}
	now.Store(time.Now().UnixMilli())
	h := server.NewHandler(st, server.Options{
		Allow:  allow,
		Assets: assets,
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:    func() time.Time { return time.UnixMilli(now.Load()) },
	})
	ctx, cancel := context.WithCancel(context.Background())
	ran := make(chan struct{})
	go func() { h.Run(ctx); close(ran) }()
	srv := httptest.NewServer(h.ForListener(ingest))
	// Joining Run keeps its read of server.QuietTick ordered before the next
	// test's write.
	t.Cleanup(func() { srv.Close(); cancel(); <-ran; st.Close() })
	return &inproc{url: srv.URL, now: now}
}

var loopbackAllow = []netip.Prefix{netip.MustParsePrefix("127.0.0.0/8"), netip.MustParsePrefix("::1/128")}

func post(t testing.TB, url string, body any, header ...string) (int, []byte) {
	t.Helper()
	var rd io.Reader
	if body != nil {
		b, _ := json.Marshal(body)
		rd = bytes.NewReader(b)
	}
	req, _ := http.NewRequest("POST", url, rd)
	req.Header.Set("Content-Type", "application/json")
	for i := 0; i+1 < len(header); i += 2 {
		req.Header.Set(header[i], header[i+1])
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b
}

func mustPost(t testing.TB, url string, body any) []byte {
	t.Helper()
	code, b := post(t, url, body)
	if code/100 != 2 {
		t.Fatalf("POST %s = %d %s", url, code, b)
	}
	return b
}

func hook(session, event string, mod ...func(*wire.HookPost)) wire.HookPost {
	h := wire.HookPost{Ctx: wire.Ctx{Session: session, Machine: "devbox"}, Event: event}
	for _, m := range mod {
		m(&h)
	}
	return h
}

func board(t testing.TB, base string) (wire.Board, string) {
	t.Helper()
	resp, err := http.Get(base + "/api/board")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var b wire.Board
	if err := json.NewDecoder(resp.Body).Decode(&b); err != nil {
		t.Fatalf("decode board: %v", err)
	}
	return b, resp.Header.Get("ETag")
}

func session(b wire.Board, id string) (wire.Session, bool) {
	for _, s := range b.Sessions {
		if s.ID == id {
			return s, true
		}
	}
	return wire.Session{}, false
}

func waitFor(t testing.TB, within time.Duration, ok func() bool, msg func() string) {
	t.Helper()
	deadline := time.Now().Add(within)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %v: %s", within, msg())
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// sseVersions streams the version carried by each `board` event.
func sseVersions(t testing.TB, url string) (<-chan string, *http.Response) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	ch := make(chan string, 16)
	go func() {
		defer close(ch)
		sc := bufio.NewScanner(resp.Body)
		event, data := "", ""
		for sc.Scan() {
			line := sc.Text()
			switch {
			case strings.HasPrefix(line, "event:"):
				event = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data = strings.TrimSpace(strings.TrimPrefix(line, "data:"))
			case line == "":
				var v struct{ Version string }
				if event == "board" && json.Unmarshal([]byte(data), &v) == nil {
					ch <- v.Version
				}
				event, data = "", ""
			}
		}
	}()
	return ch, resp
}

type syncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *syncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}
