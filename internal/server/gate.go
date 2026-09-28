package server

import (
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
)

// IsIngestListener reports whether a listener bound to bound may accept
// ingest. Wildcard binds are always read-only.
func IsIngestListener(bound netip.Addr, allow []netip.Prefix) bool {
	if !bound.IsValid() || bound.IsUnspecified() {
		return false
	}
	bound = bound.Unmap()
	return bound.IsLoopback() || inAny(bound, allow)
}

// IngestAllowed reports whether a request from remote on a listener with the
// given flag passes the ingest gate.
func IngestAllowed(listenerIngest bool, remote netip.Addr, allow []netip.Prefix) bool {
	return listenerIngest && remote.IsValid() && inAny(remote.Unmap(), allow)
}

func inAny(a netip.Addr, allow []netip.Prefix) bool {
	for _, p := range allow {
		if p.Contains(a) {
			return true
		}
	}
	return false
}

// gate runs before the body is read. The remote comes from the TCP peer
// only; forwarding headers are deliberately never consulted.
func (h *Handler) gate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		ingest := listenerIngest(r.Context())
		var remote netip.Addr
		if ap, err := netip.ParseAddrPort(r.RemoteAddr); err == nil {
			remote = ap.Addr().Unmap()
		}
		if !IngestAllowed(ingest, remote, h.allow) {
			listener := ""
			if la, ok := r.Context().Value(http.LocalAddrContextKey).(net.Addr); ok {
				listener = la.String()
			}
			h.log.Warn("ingest refused", "remote", r.RemoteAddr, "listener", listener, "ingest_listener", ingest, "path", r.URL.Path)
			writeErr(w, http.StatusForbidden, "ingest not allowed from this address")
			return
		}
		next(w, r)
	}
}

// SameOrigin is the browser-action check: an Origin whose host equals the
// request Host, plus the X-Agentboard: 1 header.
func SameOrigin(r *http.Request) bool {
	origin := r.Header.Get("Origin")
	if origin == "" || origin == "null" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" || !strings.EqualFold(u.Host, r.Host) {
		return false
	}
	v := r.Header.Values("X-Agentboard")
	return len(v) == 1 && v[0] == "1"
}

func (h *Handler) sameOrigin(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !SameOrigin(r) {
			writeErr(w, http.StatusForbidden, "cross-origin request refused")
			return
		}
		next(w, r)
	}
}
