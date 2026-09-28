package server

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/barelyworkingcode/agentboard/internal/store"
)

const shutdownGrace = 3 * time.Second

var defaultAllow = []netip.Prefix{
	netip.MustParsePrefix("127.0.0.0/8"),
	netip.MustParsePrefix("::1/128"),
	netip.MustParsePrefix("192.168.64.0/24"),
}

type multiFlag []string

func (m *multiFlag) String() string     { return strings.Join(*m, ",") }
func (m *multiFlag) Set(v string) error { *m = append(*m, v); return nil }

// listenAddr is a parsed --listen value; ip is invalid for a wildcard bind.
type listenAddr struct {
	ip   netip.Addr
	port uint16
}

func (a listenAddr) String() string {
	if !a.ip.IsValid() {
		return ":" + strconv.Itoa(int(a.port))
	}
	return netip.AddrPortFrom(a.ip, a.port).String()
}

func parseListen(s string) (listenAddr, error) {
	host, port, err := net.SplitHostPort(s)
	if err != nil {
		return listenAddr{}, fmt.Errorf("--listen %q: %w", s, err)
	}
	p, err := strconv.ParseUint(port, 10, 16)
	if err != nil {
		return listenAddr{}, fmt.Errorf("--listen %q: bad port", s)
	}
	a := listenAddr{port: uint16(p)}
	if host == "" {
		return a, nil
	}
	if a.ip, err = netip.ParseAddr(host); err != nil {
		return listenAddr{}, fmt.Errorf("--listen %q: host must be an IP literal or empty", s)
	}
	return a, nil
}

func defaultDBPath() (string, error) {
	dir, err := os.UserConfigDir()
	if err != nil {
		return "", fmt.Errorf("find config dir: %w", err)
	}
	return filepath.Join(dir, "agentboard", "agentboard.db"), nil
}

// Main runs `agentboard serve` and returns the process exit code.
func Main(ctx context.Context, args []string, assets fs.FS, stderr io.Writer) int {
	fl := flag.NewFlagSet("serve", flag.ContinueOnError)
	fl.SetOutput(stderr)
	var listens, ingestFrom multiFlag
	fl.Var(&listens, "listen", "IP:PORT to listen on (repeatable; empty host = wildcard, read-only)")
	fl.Var(&ingestFrom, "ingest-from", "CIDR allowed to post (repeatable; replaces the default)")
	dbPath := fl.String("db", "", "database path (default <config dir>/agentboard/agentboard.db)")
	if err := fl.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if fl.NArg() > 0 {
		fmt.Fprintf(stderr, "agentboard: serve: unexpected argument %q; see agentboard help\n", fl.Arg(0))
		return 2
	}
	if len(listens) == 0 {
		listens = multiFlag{"127.0.0.1:8790"}
	}
	addrs := make([]listenAddr, 0, len(listens))
	for _, s := range listens {
		a, err := parseListen(s)
		if err != nil {
			fmt.Fprintf(stderr, "agentboard: serve: %v; see agentboard help\n", err)
			return 2
		}
		addrs = append(addrs, a)
	}
	allow := defaultAllow
	if len(ingestFrom) > 0 {
		allow = nil
		for _, s := range ingestFrom {
			p, err := netip.ParsePrefix(s)
			if err != nil {
				fmt.Fprintf(stderr, "agentboard: serve: --ingest-from %q: %v; see agentboard help\n", s, err)
				return 2
			}
			allow = append(allow, p.Masked())
		}
	}

	log := slog.New(slog.NewTextHandler(stderr, nil))
	if *dbPath == "" {
		p, err := defaultDBPath()
		if err != nil {
			log.Error("resolve database path", "err", err)
			return 1
		}
		*dbPath = p
	}
	if err := os.MkdirAll(filepath.Dir(*dbPath), 0o700); err != nil {
		log.Error("create database directory", "path", filepath.Dir(*dbPath), "err", err)
		return 1
	}
	st, err := store.Open(*dbPath)
	if err != nil {
		log.Error("open database", "path", *dbPath, "err", err)
		return 1
	}
	defer st.Close()

	ctx, stop := signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
	defer stop()

	h := NewHandler(st, Options{Allow: allow, Assets: assets, Logger: log, Now: time.Now})
	go h.Run(ctx)

	listeners := make([]*listener, 0, len(addrs))
	anyIngest := false
	var wg sync.WaitGroup
	for _, a := range addrs {
		l := &listener{addr: a.String(), ingest: IsIngestListener(a.ip, allow), h: h, log: log}
		anyIngest = anyIngest || l.ingest
		listeners = append(listeners, l)
		wg.Go(func() { l.run(ctx) })
	}
	if !anyIngest {
		log.Warn("no listener accepts ingest; bind a loopback address or one inside --ingest-from")
	}
	log.Info("serving", "db", *dbPath, "listeners", len(listeners))

	<-ctx.Done()
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), shutdownGrace)
	defer cancel()
	for _, l := range listeners {
		wg.Go(func() { l.shutdown(sctx) })
	}
	wg.Wait()
	return 0
}

// listener binds one address, retrying forever; a bind failure is never fatal.
type listener struct {
	addr   string
	ingest bool
	h      *Handler
	log    *slog.Logger

	mu     sync.Mutex
	srv    *http.Server
	closed bool
}

func (l *listener) run(ctx context.Context) {
	var lastErr string
	for {
		ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", l.addr)
		if err != nil {
			if msg := err.Error(); msg != lastErr {
				l.log.Warn("listener bind failed", "addr", l.addr, "err", err, "retry", RetryInterval)
				lastErr = msg
			} else {
				l.log.Debug("listener bind failed", "addr", l.addr, "err", err)
			}
			if !sleep(ctx, RetryInterval) {
				return
			}
			continue
		}
		lastErr = ""
		srv := &http.Server{
			Handler:           l.h.ForListener(l.ingest),
			ReadHeaderTimeout: 5 * time.Second,
			IdleTimeout:       60 * time.Second,
			BaseContext:       func(net.Listener) context.Context { return withIngest(context.Background(), l.ingest) },
			ErrorLog:          slog.NewLogLogger(l.log.Handler(), slog.LevelDebug),
		}
		l.mu.Lock()
		if l.closed {
			l.mu.Unlock()
			ln.Close()
			return
		}
		l.srv = srv
		l.mu.Unlock()

		l.log.Info("listener bound", "addr", l.addr, "ingest", l.ingest)
		err = srv.Serve(ln)
		if errors.Is(err, http.ErrServerClosed) {
			return
		}
		l.log.Warn("listener stopped", "addr", l.addr, "err", err, "retry", RetryInterval)
		if !sleep(ctx, RetryInterval) {
			return
		}
	}
}

func (l *listener) shutdown(ctx context.Context) {
	l.mu.Lock()
	l.closed = true
	srv := l.srv
	l.mu.Unlock()
	if srv == nil {
		return
	}
	if err := srv.Shutdown(ctx); err != nil {
		l.log.Warn("graceful shutdown incomplete", "addr", l.addr, "err", err)
		srv.Close()
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
