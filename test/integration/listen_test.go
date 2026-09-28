package integration

import (
	"context"
	"net"
	"net/http"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/barelyworkingcode/agentboard/internal/server"
)

// serveInProcess runs server.Main with a 200 ms retry interval and returns its
// log and a channel that yields the exit code.
func serveInProcess(t *testing.T, listen ...string) (*syncBuffer, <-chan int) {
	t.Helper()
	old := server.RetryInterval
	server.RetryInterval = 200 * time.Millisecond
	args := []string{"--db", filepath.Join(t.TempDir(), "board.db")}
	for _, l := range listen {
		args = append(args, "--listen", l)
	}
	ctx, cancel := context.WithCancel(context.Background())
	logs := &syncBuffer{}
	done := make(chan int, 1)
	go func() { done <- server.Main(ctx, args, assets, logs) }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Errorf("serve did not stop on cancel")
		}
		server.RetryInterval = old
	})
	return logs, done
}

func serving(addr string) bool {
	c := http.Client{Timeout: 300 * time.Millisecond}
	resp, err := c.Get("http://" + addr + "/api/board")
	if err != nil {
		return false
	}
	resp.Body.Close()
	return resp.StatusCode == 200
}

func TestBusyPortIsBoundOnRetry(t *testing.T) {
	busy, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	busyAddr, other := busy.Addr().String(), freePort(t)
	logs, done := serveInProcess(t, busyAddr, other)

	waitFor(t, 5*time.Second, func() bool { return serving(other) },
		func() string { return "free listener never served while the other was busy; logs:\n" + logs.String() })
	select {
	case code := <-done:
		t.Fatalf("serve exited %d on a busy port; logs:\n%s", code, logs.String())
	default:
	}

	busy.Close()
	freed := time.Now()
	waitFor(t, 2*time.Second, func() bool { return serving(busyAddr) },
		func() string { return "freed port never bound; logs:\n" + logs.String() })
	if took := time.Since(freed); took > time.Second {
		t.Errorf("freed port bound after %v, want within the 200 ms retry interval", took)
	}
	if !serving(other) {
		t.Errorf("the other listener stopped serving")
	}
}

func TestUnavailableAddressIsNotFatal(t *testing.T) {
	const missing = "192.0.2.1:0" // documentation range: never assigned to this host
	t.Run("alongside a working listener", func(t *testing.T) {
		loop := freePort(t)
		logs, done := serveInProcess(t, missing, loop)
		waitFor(t, 5*time.Second, func() bool { return serving(loop) },
			func() string { return "loopback listener never served; logs:\n" + logs.String() })
		time.Sleep(time.Second) // five retry intervals
		select {
		case code := <-done:
			t.Fatalf("serve exited %d; logs:\n%s", code, logs.String())
		default:
		}
		warns := 0
		for _, line := range strings.Split(logs.String(), "\n") {
			if strings.Contains(line, "WARN") && strings.Contains(line, "192.0.2.1") {
				warns++
			}
		}
		if warns != 1 {
			t.Errorf("WARN lines for the missing address = %d, want 1 (first failure only, then DEBUG); logs:\n%s", warns, logs.String())
		}
	})
	t.Run("as the only listener", func(t *testing.T) {
		logs, done := serveInProcess(t, missing)
		select {
		case code := <-done:
			t.Fatalf("serve exited %d when every address failed; logs:\n%s", code, logs.String())
		case <-time.After(time.Second):
		}
	})
}
