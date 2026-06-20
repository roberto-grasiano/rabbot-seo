package obs

import (
	"context"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// MetricsServer is the daemon's read-only, GET-only, UNAUTHENTICATED metrics
// listener. It is deliberately NOT part of internal/control: the control plane
// is loopback + bearer-token with mutation hooks, whereas this server is
// tokenless and therefore exposes exactly one read surface — GET /metrics —
// from the daemon's private Prometheus registry. Every other path is 404 and
// every non-GET method on /metrics is 405, so no caller can probe it for more
// than the bounded, cardinality-disciplined rabbot_* exposition.
//
// Off by default and loopback-when-enabled: binding it at all is an explicit
// operator action (config metrics.addr), and binding it non-loopback logs a
// startup warning (handled by the daemon, not here). The same timeout hardening
// as control.Server.ListenAndServe bounds slow-header / slow-writer / idle
// keep-alive clients.
type MetricsServer struct {
	handler http.Handler

	// mu guards httpSrv and shuttingDown, written/read from both the serve
	// goroutine (ListenAndServe/Serve) and a concurrent Shutdown — mirroring
	// control.Server so a Shutdown that races startup cannot leak a listener.
	mu           sync.Mutex
	httpSrv      *http.Server
	shuttingDown bool
}

// NewMetricsServer builds a MetricsServer over the registry backing m. A nil
// *Metrics (or a Metrics with a nil registry) yields a server that 404s every
// path — there is nothing to expose — but is still safe to start/stop, so the
// daemon lifecycle need not special-case the off state at the server layer.
func NewMetricsServer(m *Metrics) *MetricsServer {
	mux := http.NewServeMux()
	if reg := m.Registry(); reg != nil {
		// promhttp.HandlerFor serves the private registry's exposition. The
		// pattern "GET /metrics" makes the std mux return 405 for other methods
		// on this exact path and 404 for any other path — exactly the GET-only,
		// /metrics-only contract, with no hand-rolled method/path checks.
		mux.Handle("GET /metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{}))
	}
	return &MetricsServer{handler: mux}
}

// Serve serves on an already-bound listener until Shutdown closes it. It is the
// seam tests use to bind :0 and learn the port; ListenAndServe is the daemon's
// bind-and-serve entry. A Shutdown that arrives first makes this return without
// serving.
func (s *MetricsServer) Serve(ln net.Listener) error {
	srv := s.newHTTPServer()
	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		_ = ln.Close()
		return nil
	}
	s.httpSrv = srv
	s.mu.Unlock()
	return srv.Serve(ln)
}

// ListenAndServe binds addr (host:port) and serves until Shutdown. A bind
// failure is returned to the caller so the daemon can make it a fatal startup
// error (the F18 pattern: a configured-but-unbindable metrics addr must fail
// loudly, not run silently without the surface). If Shutdown already ran this
// returns nil without binding.
func (s *MetricsServer) ListenAndServe(addr string) error {
	srv := s.newHTTPServer()

	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		return nil
	}
	s.httpSrv = srv
	s.mu.Unlock()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return srv.Serve(ln)
}

// newHTTPServer builds the hardened *http.Server. Timeouts mirror
// control.Server.ListenAndServe: bound slow-header (Slowloris) clients, bound a
// slow/stuck writer, and reap idle keep-alive conns so a stale client cannot pin
// a socket. The metrics body is small and the surface read-only, so these are
// generous.
func (s *MetricsServer) newHTTPServer() *http.Server {
	return &http.Server{
		Handler:           s.handler,
		ReadHeaderTimeout: 10 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
}

// Shutdown gracefully stops the server. It is safe to call concurrently with
// Serve/ListenAndServe and even before it: a Shutdown that arrives first marks
// the server shutting down so a later Serve/ListenAndServe returns without
// binding.
func (s *MetricsServer) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.shuttingDown = true
	srv := s.httpSrv
	s.mu.Unlock()

	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}
