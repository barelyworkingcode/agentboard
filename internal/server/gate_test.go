package server_test

import (
	"net/http/httptest"
	"net/netip"
	"testing"

	"github.com/barelyworkingcode/agentboard/internal/server"
)

func prefixes(s ...string) []netip.Prefix {
	out := make([]netip.Prefix, len(s))
	for i, p := range s {
		out[i] = netip.MustParsePrefix(p)
	}
	return out
}

var defaultAllow = prefixes("127.0.0.0/8", "::1/128", "192.168.64.0/24")

func TestIsIngestListener(t *testing.T) {
	lan := prefixes("192.0.2.0/24")
	cases := []struct {
		bound string
		allow []netip.Prefix
		want  bool
	}{
		{"127.0.0.1", defaultAllow, true},
		{"::1", defaultAllow, true},
		{"127.0.0.1", lan, true}, // loopback qualifies even outside the allow list
		{"192.168.64.1", defaultAllow, true},
		{"192.0.2.50", defaultAllow, false},
		{"192.0.2.50", lan, true},
		{"198.51.100.7", defaultAllow, false},
		{"0.0.0.0", defaultAllow, false},
		{"::", defaultAllow, false},
		{"0.0.0.0", prefixes("0.0.0.0/0"), false},
		{"::", prefixes("::/0"), false},
	}
	for _, c := range cases {
		if got := server.IsIngestListener(netip.MustParseAddr(c.bound), c.allow); got != c.want {
			t.Errorf("IsIngestListener(%s, %v) = %v, want %v", c.bound, c.allow, got, c.want)
		}
	}
}

func TestIngestAllowed(t *testing.T) {
	cases := []struct {
		listener bool
		remote   string
		allow    []netip.Prefix
		want     bool
	}{
		{true, "127.0.0.1", defaultAllow, true},
		{true, "::1", defaultAllow, true},
		{true, "::ffff:127.0.0.1", defaultAllow, true},
		{true, "192.168.64.5", defaultAllow, true},
		{true, "::ffff:192.168.64.5", defaultAllow, true},
		{true, "192.0.2.50", defaultAllow, false},
		{true, "198.51.100.7", defaultAllow, false},
		{true, "192.0.2.50", prefixes("192.0.2.0/24"), true},
		{true, "127.0.0.1", prefixes("192.0.2.0/24"), false},
		{false, "127.0.0.1", defaultAllow, false},
		{false, "192.168.64.5", defaultAllow, false},
	}
	for _, c := range cases {
		if got := server.IngestAllowed(c.listener, netip.MustParseAddr(c.remote), c.allow); got != c.want {
			t.Errorf("IngestAllowed(%v, %s, %v) = %v, want %v", c.listener, c.remote, c.allow, got, c.want)
		}
	}
}

func TestSameOrigin(t *testing.T) {
	cases := []struct {
		name, host, origin, header string
		setOrigin                  bool
		want                       bool
	}{
		{"same origin with header", "127.0.0.1:8790", "http://127.0.0.1:8790", "1", true, true},
		{"host compared case-insensitively", "DEVBOX:8790", "http://devbox:8790", "1", true, true},
		{"no Origin", "127.0.0.1:8790", "", "1", false, false},
		{"Origin null", "127.0.0.1:8790", "null", "1", true, false},
		{"other host", "127.0.0.1:8790", "http://198.51.100.7:8790", "1", true, false},
		{"other port", "127.0.0.1:8790", "http://127.0.0.1:9999", "1", true, false},
		{"no header", "127.0.0.1:8790", "http://127.0.0.1:8790", "", true, false},
		{"header not exactly 1", "127.0.0.1:8790", "http://127.0.0.1:8790", "true", true, false},
	}
	for _, c := range cases {
		r := httptest.NewRequest("POST", "http://"+c.host+"/ui/clear", nil)
		r.Host = c.host
		if c.setOrigin {
			r.Header.Set("Origin", c.origin)
		}
		if c.header != "" {
			r.Header.Set("X-Agentboard", c.header)
		}
		if got := server.SameOrigin(r); got != c.want {
			t.Errorf("%s: SameOrigin = %v, want %v", c.name, got, c.want)
		}
	}
}
