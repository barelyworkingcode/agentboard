//go:build budget

package integration

import (
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// Run with: go test -tags budget ./test/integration -run TestBudget -v
func TestBudget(t *testing.T) {
	const mib = 1 << 20
	info, err := os.Stat(binary(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("binary size: %.2f MiB", float64(info.Size())/mib)
	if info.Size() >= 20*mib {
		t.Errorf("binary is %d bytes, want < 20 MiB", info.Size())
	}

	base, serve := startServe(t)
	events, _ := sseVersions(t, base+"/events")
	go func() {
		for range events {
		}
	}()
	repo := gitRepo(t)

	var (
		mu    sync.Mutex
		calls []time.Duration
		fails []string
	)
	call := func(env []string, stdin string, args ...string) {
		r := runBin(t, repo, stdin, env, args...)
		mu.Lock()
		defer mu.Unlock()
		calls = append(calls, r.elapsed)
		if r.code != 0 || r.stderr != "" {
			fails = append(fails, fmt.Sprintf("%v: exit=%d stderr=%q", args, r.code, r.stderr))
		}
	}
	tool := func(session, event, id string) string {
		return fmt.Sprintf(`{"session_id":%q,"cwd":%q,"hook_event_name":%q,"tool_name":"Bash","tool_use_id":%q}`, session, repo, event, id)
	}

	var wg sync.WaitGroup
	end := time.Now().Add(60 * time.Second)
	for i := range 10 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			session := fmt.Sprintf("budget-%d", i)
			env := childEnv("AB_URL="+base, "AB_MACHINE=devbox", "CLAUDE_CODE_SESSION_ID="+session)
			call(env, hookJSON(session, repo, "SessionStart"), "hook")
			call(env, hookJSON(session, repo, "UserPromptSubmit"), "hook")
			for tick := 0; time.Now().Before(end); tick++ {
				next := time.Now().Add(time.Second)
				id := fmt.Sprintf("tu-%d", tick)
				call(env, tool(session, "PreToolUse", id), "hook")
				call(env, tool(session, "PostToolUse", id), "hook")
				if tick%10 == 0 {
					call(env, "", "log", "budget tick", strconv.Itoa(tick))
				}
				time.Sleep(time.Until(next))
			}
		}()
	}
	wg.Wait()

	if len(fails) > 0 {
		t.Errorf("%d calls were not recorded, first: %s", len(fails), fails[0])
	}
	slices.Sort(calls)
	p95, worst := calls[len(calls)*95/100], calls[len(calls)-1]
	t.Logf("calls: %d, p95 %v, max %v", len(calls), p95, worst)
	if p95 >= 50*time.Millisecond {
		t.Errorf("per-call p95 = %v, want < 50 ms", p95)
	}

	time.Sleep(30 * time.Second)
	pid := serve.Process.Pid
	rssKiB := psInt(t, pid, "rss=")
	t.Logf("idle RSS: %.1f MiB", float64(rssKiB)/1024)
	if rssKiB*1024 >= 25*mib {
		t.Errorf("idle RSS = %d KiB, want < 25 MiB", rssKiB)
	}
	cpu0 := cpuTime(t, pid)
	time.Sleep(60 * time.Second)
	cpu := cpuTime(t, pid) - cpu0
	t.Logf("idle CPU over 60 s: %v (%.2f%%)", cpu, 100*cpu.Seconds()/60)
	if cpu >= 600*time.Millisecond {
		t.Errorf("idle CPU over 60 s = %v, want < 0.6 s", cpu)
	}
}

func psInt(t *testing.T, pid int, field string) int64 {
	out, err := exec.Command("ps", "-o", field, "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("ps -o %s: %v", field, err)
	}
	n, err := strconv.ParseInt(strings.TrimSpace(string(out)), 10, 64)
	if err != nil {
		t.Fatalf("ps -o %s = %q: %v", field, out, err)
	}
	return n
}

// cpuTime is user+system time. Linux ps reports whole seconds only, so it
// reads /proc there; macOS ps reports centiseconds.
func cpuTime(t *testing.T, pid int) time.Duration {
	if b, err := os.ReadFile(fmt.Sprintf("/proc/%d/stat", pid)); err == nil {
		f := strings.Fields(string(b[strings.LastIndexByte(string(b), ')')+2:]))
		ut, _ := strconv.ParseInt(f[11], 10, 64)
		st, _ := strconv.ParseInt(f[12], 10, 64)
		return time.Duration(ut+st) * 10 * time.Millisecond // USER_HZ is 100
	}
	out, err := exec.Command("ps", "-o", "time=", "-p", strconv.Itoa(pid)).Output()
	if err != nil {
		t.Fatalf("ps -o time: %v", err)
	}
	// [[dd-]hh:]mm:ss.cc
	var total float64
	for _, part := range strings.Split(strings.TrimSpace(string(out)), ":") {
		v, err := strconv.ParseFloat(part, 64)
		if err != nil {
			t.Fatalf("ps -o time = %q", out)
		}
		total = total*60 + v
	}
	return time.Duration(total * float64(time.Second))
}
