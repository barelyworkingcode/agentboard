package integration

import (
	"bytes"
	"net/netip"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

var (
	ipv4Pattern = regexp.MustCompile(`\b\d{1,3}\.\d{1,3}\.\d{1,3}\.\d{1,3}\b`)
	homePattern = regexp.MustCompile(`/(Users|home)/[A-Za-z0-9._-]+`)
	allowedNets = []netip.Prefix{
		netip.MustParsePrefix("127.0.0.0/8"),
		netip.MustParsePrefix("0.0.0.0/32"),
		netip.MustParsePrefix("192.168.64.0/24"),
		netip.MustParsePrefix("192.0.2.0/24"),
		netip.MustParsePrefix("198.51.100.0/24"),
	}
)

func allowedIP(s string) bool {
	a, err := netip.ParseAddr(s)
	if err != nil {
		return true // an octet above 255: a version string, not an address
	}
	for _, p := range allowedNets {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// Tracked and not-yet-committed files are both checked, so a leak is caught
// before the commit that would publish it.
func TestRepoHasNoRealAddressesOrPaths(t *testing.T) {
	root, err := filepath.Abs("../..")
	if err != nil {
		t.Fatal(err)
	}
	out, err := exec.Command("git", "-C", root, "ls-files", "-z", "--cached", "--others", "--exclude-standard").Output()
	if err != nil {
		t.Fatalf("git ls-files: %v", err)
	}
	files := strings.Split(strings.TrimRight(string(out), "\x00"), "\x00")
	if len(files) < 5 {
		t.Fatalf("git ls-files returned %d files; wrong root %s?", len(files), root)
	}
	for _, f := range files {
		b, err := os.ReadFile(filepath.Join(root, f))
		if err != nil || bytes.IndexByte(b, 0) >= 0 {
			continue // deleted in the worktree, or binary
		}
		for i, line := range strings.Split(string(b), "\n") {
			for _, ip := range ipv4Pattern.FindAllString(line, -1) {
				if !allowedIP(ip) {
					t.Errorf("%s:%d: address %s is outside the documentation and loopback ranges", f, i+1, ip)
				}
			}
			if m := homePattern.FindString(line); m != "" {
				t.Errorf("%s:%d: real home path %q", f, i+1, m)
			}
		}
	}
}
