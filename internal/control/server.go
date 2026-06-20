package control

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"net"
	"net/http"
	"strconv"
	"sync"
	"time"
)

// maxControlBody caps each JSON request body. Control payloads are tiny; 1 MiB
// is generous and bounds the memory/CPU a (loopback, authenticated) client can
// force the decoder to buffer (defense-in-depth — see F20).
const maxControlBody = 1 << 20

// ServerOptions configure the control-plane server (amendment §B).
type ServerOptions struct {
	Token   string
	Version string
	Hooks   Hooks
	// Shutdown, when non-nil, triggers the daemon's own graceful teardown (the
	// same ctx-cancel-driven drain/checkpoint the SIGINT/SIGTERM path runs). It
	// backs POST /v1/shutdown. A nil Shutdown makes that route return 501. It is
	// kept on ServerOptions (not Hooks) because it does not return an error and is
	// fired asynchronously after the response is written — calling it tears down
	// THIS server, so the success response must already be on the wire (#43).
	Shutdown func()
}

// Hooks are daemon-supplied callbacks the control API invokes — one per
// contract §4 mutation. A nil hook makes its route return 501 Not Implemented.
// M0 wires Reload + Status; M1 wires Pause/Crawl/AddSite/RemoveSite/SetConfig
// (and a richer Status); M2 wires IgnoreIssue/NotifyTest.
type Hooks struct {
	Reload      func() error
	Pause       func(ctx context.Context, paused bool) error
	Status      func(ctx context.Context) (StatusResponse, error)
	Crawl       func(ctx context.Context, req CrawlRequest) (CrawlResponse, error)
	AddSite     func(ctx context.Context, req AddSiteRequest) (AddSiteResponse, error)
	RemoveSite  func(ctx context.Context, id int64, purge bool) error
	IgnoreIssue func(ctx context.Context, id int64) error
	NotifyTest  func(ctx context.Context, notifier string) error
	SetConfig   func(ctx context.Context, req ConfigSetRequest) error
	ListSites   func(ctx context.Context) ([]SiteSummary, error)
	SiteDetail  func(ctx context.Context, id int64) (SiteDetailResponse, bool, error)
	Issues      func(ctx context.Context, q IssueQuery) ([]IssueView, error)
	History     func(ctx context.Context, url string, since time.Time) (HistoryResponse, error)
	// Report backs GET /v1/report: a windowed activity digest. A non-nil segment
	// scopes every sub-query to URLs that are members of a segment with that name.
	// Nil => 501.
	Report func(ctx context.Context, since time.Time, siteID *int64, top int, segment *string) (ReportResponse, error)
	// Coverage backs GET /v1/coverage: sitemap-coverage drift for one site. The
	// bool return is found (false => unknown site id -> HTTP 404). Nil => 501.
	Coverage func(ctx context.Context, siteID int64) (CoverageResponse, bool, error)
	// RichResults backs GET /v1/rich-results?url=: rich-result eligibility for one
	// monitored URL's latest snapshot (A4). An unknown URL is reported as data on
	// the response (RichResultsResponse.NotFound=true, HTTP 200) — the same
	// not-found-as-data pattern as History, NOT a 404. Nil => 501.
	RichResults func(ctx context.Context, url string) (RichResultsResponse, error)
	// Score backs GET /v1/score: the LIVE health score for one scope (whole site, or
	// the named segment) plus its persisted trend (A6). The bool return is found
	// (false => unknown site id OR segment name -> NotFoundResponse, HTTP 200, the
	// errors-as-data SiteDetail pattern). Nil => 501.
	Score func(ctx context.Context, siteID int64, segment string, since time.Time) (ScoreResponse, bool, error)
	// Links backs GET /v1/links?url=&limit=: the inbound link-graph answers for one
	// URL — its ranked inlinkers (WhatLinksTo) plus the blast-radius summary (A9).
	// Node identity is exact-string (fragment-stripped only); a never-linked URL is
	// reported as data (LinksResponse.NotFound=true, HTTP 200) — the History
	// not-found pattern, NOT a 404. Nil => 501.
	Links func(ctx context.Context, url string, limit int) (LinksResponse, error)
	// Graph backs GET /v1/graph?site_id=&focus=&hops=&mode=: the bounded link-graph
	// export — a focus-URL neighborhood (≤ 2 hops) or a segment/folder overview
	// (A9). The bool return is found (false => unknown site id -> NotFoundResponse,
	// HTTP 200, the errors-as-data SiteDetail pattern). A caller-fault (hops > 2,
	// bad mode) is surfaced as an ErrBadRequest-wrapped error -> HTTP 400. Nil => 501.
	Graph func(ctx context.Context, q GraphQuery) (GraphResponse, bool, error)
	// IndexStatus backs GET /v1/index-status?url=: the latest stored GSC index
	// status for one URL (GSC W2). An un-inspected URL is reported as data
	// (IndexStatusResponse.NotFound=true / HasStatus=false, HTTP 200) — the
	// RichResults not-found pattern, NOT a 404 (the quota-bounded-staleness guard).
	// Nil => 501.
	IndexStatus func(ctx context.Context, url string) (IndexStatusResponse, error)
	// SearchPerformance backs GET /v1/search-performance?url=&since=: the stored
	// GSC search metrics for one URL over a window (GSC W2, dataState=final). A URL
	// with no metrics is reported as data (HasData=false, HTTP 200) — never a 404.
	// The since string is RFC3339 (validated by the handler before the hook runs).
	// Nil => 501.
	SearchPerformance func(ctx context.Context, url, since string) (SearchPerformanceResponse, error)
	// Verify runs the daemon-owned proof flow (begin/check) — the daemon holds the
	// single DB writer and the fetcher. A nil hook makes POST /v1/verify 501.
	Verify func(ctx context.Context, req VerifyRequest) (VerifyResponse, error)
}

// Server is the loopback control-plane HTTP server.
type Server struct {
	token    string
	version  string
	hooks    Hooks
	shutdown func() // daemon graceful-teardown trigger for POST /v1/shutdown; nil => 501

	// mu guards httpSrv and shuttingDown, which are written/read from both the
	// serve goroutine (ListenAndServe) and the teardown goroutine (Shutdown).
	mu           sync.Mutex
	httpSrv      *http.Server
	shuttingDown bool
}

// NewServer constructs a control Server. opts.Token is the bearer token;
// opts.Version is injected at build time; opts.Hooks may have nil fields
// (their routes return 501 Not Implemented). opts.Shutdown backs the
// POST /v1/shutdown route (nil => 501).
func NewServer(opts ServerOptions) *Server {
	return &Server{token: opts.Token, version: opts.Version, hooks: opts.Hooks, shutdown: opts.Shutdown}
}

// Handler builds the routed, auth-wrapped http.Handler. It registers every
// contract §4 route; routes whose hook is nil return 501 Not Implemented.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/health", s.handleHealth)
	mux.HandleFunc("GET /v1/status", s.handleStatus)
	mux.HandleFunc("POST /v1/sites", s.handleAddSite)
	mux.HandleFunc("DELETE /v1/sites/{id}", s.handleRemoveSite)
	mux.HandleFunc("POST /v1/crawl", s.handleCrawl)
	mux.HandleFunc("POST /v1/issues/{id}/ignore", s.handleIgnoreIssue)
	mux.HandleFunc("POST /v1/pause", s.handlePause)
	mux.HandleFunc("POST /v1/resume", s.handleResume)
	mux.HandleFunc("POST /v1/reload", s.handleReload)
	mux.HandleFunc("POST /v1/notify/test", s.handleNotifyTest)
	mux.HandleFunc("POST /v1/config", s.handleConfigSet)
	mux.HandleFunc("POST /v1/verify", s.handleVerify)
	mux.HandleFunc("POST /v1/shutdown", s.handleShutdown)
	mux.HandleFunc("GET /v1/sites", s.handleListSites)
	mux.HandleFunc("GET /v1/sites/{id}/detail", s.handleSiteDetail)
	mux.HandleFunc("GET /v1/issues", s.handleListIssues)
	mux.HandleFunc("GET /v1/history", s.handleHistory)
	mux.HandleFunc("GET /v1/report", s.handleReport)
	mux.HandleFunc("GET /v1/coverage", s.handleCoverage)
	mux.HandleFunc("GET /v1/rich-results", s.handleRichResults)
	mux.HandleFunc("GET /v1/score", s.handleScore)
	mux.HandleFunc("GET /v1/links", s.handleLinks)
	mux.HandleFunc("GET /v1/graph", s.handleGraph)
	mux.HandleFunc("GET /v1/index-status", s.handleIndexStatus)
	mux.HandleFunc("GET /v1/search-performance", s.handleSearchPerformance)
	return s.auth(mux)
}

// auth enforces the Authorization: Bearer <token> header on every request.
//
// The whole header is compared against the expected "Bearer "+token in a single
// subtle.ConstantTimeCompare. This deliberately leaves no variable-time byte
// comparison over the secret region (the old code did h[:len(prefix)] != prefix
// before the constant-time token check, which the reviewer kept re-flagging).
// The only remaining length-dependent branch is the len(h) guard, which reads
// just the public, attacker-controlled header length — ConstantTimeCompare
// itself already short-circuits on a length mismatch — so it leaks nothing about
// the token. The guard requires a non-empty token region after the prefix,
// preserving the exact prior behavior (a bare "Bearer " with an empty token is
// rejected). Missing header, wrong scheme/prefix, and wrong token all 401;
// only a correct "Bearer <token>" passes (finding #17.2).
func (s *Server) auth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		const prefix = "Bearer "
		expected := prefix + s.token
		h := r.Header.Get("Authorization")
		if len(h) <= len(prefix) ||
			subtle.ConstantTimeCompare([]byte(h), []byte(expected)) != 1 {
			writeJSON(w, http.StatusUnauthorized, ErrorResponse{Error: ErrUnauthorized.Error()})
			return
		}
		next.ServeHTTP(w, r)
	})
}

// notImplemented writes a 501 for an unwired hook.
func notImplemented(w http.ResponseWriter, route string) {
	writeJSON(w, http.StatusNotImplemented, ErrorResponse{Error: "not implemented: " + route})
}

// decodeBody caps the request body at maxControlBody, JSON-decodes it into dst,
// and on failure writes the right error response and reports false so the caller
// returns. An oversized body trips http.MaxBytesReader, whose *http.MaxBytesError
// (Go 1.19+) we detect with errors.As and map to 413 Request Entity Too Large;
// any other decode error is a malformed payload -> 400 Bad Request (finding
// Low-413). Callers that need ErrBadRequest-aware status mapping apply that to
// the *hook* error, not the decode error handled here.
func decodeBody(w http.ResponseWriter, r *http.Request, dst any) bool {
	r.Body = http.MaxBytesReader(w, r.Body, maxControlBody)
	if err := json.NewDecoder(r.Body).Decode(dst); err != nil {
		status := http.StatusBadRequest
		var maxErr *http.MaxBytesError
		if errors.As(err, &maxErr) {
			status = http.StatusRequestEntityTooLarge
		}
		writeJSON(w, status, ErrorResponse{Error: err.Error()})
		return false
	}
	return true
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok", "version": s.version})
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	if s.hooks.Status == nil {
		notImplemented(w, "status")
		return
	}
	resp, err := s.hooks.Status(r.Context())
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleAddSite(w http.ResponseWriter, r *http.Request) {
	if s.hooks.AddSite == nil {
		notImplemented(w, "add site")
		return
	}
	var req AddSiteRequest
	if !decodeBody(w, r, &req) {
		return
	}
	resp, err := s.hooks.AddSite(r.Context(), req)
	if err != nil {
		// Caller-validation failures (bad URL/interval) wrap ErrBadRequest and
		// map to 400; everything else is a genuine server fault -> 500. control
		// stays decoupled from internal/store — the cli-layer hook is what wraps
		// client errors with ErrBadRequest (finding #20.3).
		status := http.StatusInternalServerError
		if errors.Is(err, ErrBadRequest) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleRemoveSite(w http.ResponseWriter, r *http.Request) {
	if s.hooks.RemoveSite == nil {
		notImplemented(w, "remove site")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid site id"})
		return
	}
	purge := r.URL.Query().Get("purge") == "true"
	if err := s.hooks.RemoveSite(r.Context(), id, purge); err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, OKResponse{OK: true})
}

func (s *Server) handleCrawl(w http.ResponseWriter, r *http.Request) {
	if s.hooks.Crawl == nil {
		notImplemented(w, "crawl")
		return
	}
	var req CrawlRequest
	if !decodeBody(w, r, &req) {
		return
	}
	resp, err := s.hooks.Crawl(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleIgnoreIssue(w http.ResponseWriter, r *http.Request) {
	if s.hooks.IgnoreIssue == nil {
		notImplemented(w, "ignore issue")
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, ErrorResponse{Error: "invalid issue id"})
		return
	}
	if err := s.hooks.IgnoreIssue(r.Context(), id); err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, OKResponse{OK: true})
}

func (s *Server) handlePause(w http.ResponseWriter, r *http.Request)  { s.setPaused(w, r, true) }
func (s *Server) handleResume(w http.ResponseWriter, r *http.Request) { s.setPaused(w, r, false) }

func (s *Server) setPaused(w http.ResponseWriter, r *http.Request, paused bool) {
	if s.hooks.Pause == nil {
		notImplemented(w, "pause/resume")
		return
	}
	if err := s.hooks.Pause(r.Context(), paused); err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, OKResponse{OK: true})
}

func (s *Server) handleReload(w http.ResponseWriter, r *http.Request) {
	if s.hooks.Reload == nil {
		notImplemented(w, "reload")
		return
	}
	if err := s.hooks.Reload(); err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, OKResponse{OK: true})
}

func (s *Server) handleNotifyTest(w http.ResponseWriter, r *http.Request) {
	if s.hooks.NotifyTest == nil {
		notImplemented(w, "notify test")
		return
	}
	var req NotifyTestRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := s.hooks.NotifyTest(r.Context(), req.Notifier); err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, OKResponse{OK: true})
}

func (s *Server) handleConfigSet(w http.ResponseWriter, r *http.Request) {
	if s.hooks.SetConfig == nil {
		notImplemented(w, "config set")
		return
	}
	var req ConfigSetRequest
	if !decodeBody(w, r, &req) {
		return
	}
	if err := s.hooks.SetConfig(r.Context(), req); err != nil {
		writeJSON(w, http.StatusInternalServerError, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, OKResponse{OK: true})
}

// handleShutdown triggers the daemon's graceful teardown (POST /v1/shutdown).
//
// Ordering is load-bearing. s.shutdown cancels the daemon's root ctx, which tears
// down THIS control server (ctrlSrv.Shutdown) as part of the same drain. So the
// success response MUST be fully written and flushed to the (loopback,
// authenticated) client BEFORE the trigger fires — otherwise the teardown races
// the response and the caller may see a dropped connection instead of 202. We
// therefore write+flush 202 Accepted, then fire s.shutdown asynchronously in its
// own goroutine so this handler can return and release the connection. A nil
// trigger returns 501 like every other unwired route.
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	if s.shutdown == nil {
		notImplemented(w, "shutdown")
		return
	}
	writeJSON(w, http.StatusAccepted, OKResponse{OK: true})
	// Flush the response to the client before triggering teardown. Flush is
	// best-effort: an error (e.g. the writer does not support flushing) is benign
	// because the WriteTimeout-bounded write above has already committed the bytes
	// to the std server's buffered writer; the goroutine below only fires after.
	_ = http.NewResponseController(w).Flush()
	// Fire the trigger in its own goroutine so the handler returns and releases the
	// connection immediately, independent of how long the trigger takes. In production
	// s.shutdown is the daemon ctx cancel (non-blocking); the actual drain/checkpoint
	// — which includes shutting down this very server — runs on the daemon's main
	// goroutine afterward. Keeping it async also future-proofs against a blocking
	// Shutdown impl and guarantees the handler never waits on the server's own teardown.
	go s.shutdown()
}

// ListenAndServe binds 127.0.0.1:<port> only (never 0.0.0.0) and serves until
// the listener is closed via Shutdown. The *http.Server is published under the
// mutex before Serve is entered so a concurrent Shutdown observes it (F4: no
// data race, no lost shutdown). If Shutdown already ran, this returns nil
// without binding a listener — a fast startup-time stop cannot leak a bound
// listener that serves forever.
func (s *Server) ListenAndServe(port int) error {
	srv := &http.Server{
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,  // bound slow-header (Slowloris) clients
		WriteTimeout:      30 * time.Second,  // bound a slow/stuck response writer; control payloads are tiny so 30s is ample
		IdleTimeout:       120 * time.Second, // reap idle keep-alive conns so stale loopback clients can't pin sockets (finding #20.5)
	}

	s.mu.Lock()
	if s.shuttingDown {
		s.mu.Unlock()
		return nil // Shutdown beat us; do not bind a listener.
	}
	s.httpSrv = srv
	s.mu.Unlock()

	addr := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	return srv.Serve(ln)
}

// Shutdown gracefully stops the server. It is safe to call concurrently with
// ListenAndServe and even before it: a Shutdown that arrives first marks the
// server as shutting down so a later ListenAndServe returns without binding.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.shuttingDown = true
	srv := s.httpSrv
	s.mu.Unlock()

	if srv == nil {
		return nil
	}
	return srv.Shutdown(ctx)
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	// The status line and headers are already committed by WriteHeader, so an
	// encode error here cannot change the response. Its only cause is the client —
	// a loopback, authenticated CLI — dropping the connection mid-body, which is
	// not a server fault and nothing the handler can act on. Deliberately discarded:
	// the Server holds no logger, and threading one through every handler for this
	// unactionable, effectively-unreachable case is not worth the coupling (PR #21).
	_ = json.NewEncoder(w).Encode(v)
}
