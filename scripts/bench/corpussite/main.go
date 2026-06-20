// Command corpussite is the LOOPBACK-ONLY synthetic origin for the B3 capacity
// harness (docs/PERFORMANCE.md). It serves the deterministic pages from
// internal/benchcorpus over HTTP on 127.0.0.1 so the real rabbot daemon can be
// pointed at a stable, reproducible corpus with ZERO third-party egress.
//
// It is dev tooling, not a shipped artifact: goreleaser builds only
// ./cmd/rabbot (.goreleaser.yaml), so nothing under scripts/bench/ ever enters
// a release binary. It must still compile under `go test ./...` (the CI
// bench-smoke leg covers it).
//
// # Loopback only (responsible-crawler posture)
//
// The server binds 127.0.0.1 ONLY — never 0.0.0.0. A non-loopback -addr is a
// fatal startup error. The capacity harness runs the daemon and this origin on
// the same box; no real site is ever fetched, so the SSRF/robots posture of the
// production crawler is untouched by the benchmark.
//
// # Determinism and churn
//
// Every page's bytes come from benchcorpus.Page (fixed by construction, SHA-
// pinned in TestCorpusIsFixed), so a steady-state page is byte-identical across
// requests and machines. To exercise the diff -> rules -> alert path under load,
// a churn ticker flips a deterministic share of pages (-churn-pct) to a mutated
// variant every -churn-every interval. The mutation is itself deterministic (it
// rewrites the title and injects a revision marker derived from the page index
// and an integer epoch), so a given (page, epoch) always yields the same bytes —
// the churn is reproducible, not random. There is no math/rand anywhere here.
//
// # Conditional requests (-etag)
//
// With -etag the server emits a strong ETag whose value changes only when a
// page's revision changes (steady -> churned -> back). It honors If-None-Match
// by returning 304 Not Modified with no body, so the daemon's conditional
// re-fetches take the cheap not-modified path between churn epochs — exactly the
// common steady-state case the recheck benchmark's not_modified scenario models.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"sync"
	"syscall"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/benchcorpus"
)

func main() {
	cfg := parseFlags(os.Args[1:], os.Stderr)
	if err := run(cfg); err != nil {
		log.Fatalf("corpussite: %v", err)
	}
}

// config holds the parsed command-line configuration.
type config struct {
	addr       string
	pages      int
	churnPct   int
	churnEvery time.Duration
	etag       bool
}

// parseFlags parses argv into a config. It lives apart from main so a test can
// exercise it without spawning a process.
func parseFlags(args []string, out io.Writer) config {
	fs := flag.NewFlagSet("corpussite", flag.ExitOnError)
	fs.SetOutput(out)
	var c config
	fs.StringVar(&c.addr, "addr", "127.0.0.1:0", "loopback listen address (127.0.0.1 or ::1 only); :0 picks a free port")
	fs.IntVar(&c.pages, "pages", 500, "number of corpus pages served (paths /landing|article|listing/<i>, i in [0,pages))")
	fs.IntVar(&c.churnPct, "churn-pct", 0, "percentage of pages mutated each churn interval (0 = static corpus)")
	fs.DurationVar(&c.churnEvery, "churn-every", time.Minute, "interval between churn epochs (only used when churn-pct > 0)")
	fs.BoolVar(&c.etag, "etag", false, "emit ETag and honor If-None-Match with 304 (lets the daemon take the not-modified path)")
	// flag.ExitOnError handles --help / parse errors; the error is never nil-returned.
	_ = fs.Parse(args)
	return c
}

// run validates the config, starts the loopback server, and blocks until a
// signal (SIGINT/SIGTERM) triggers a graceful shutdown.
func run(cfg config) error {
	if cfg.pages <= 0 {
		return fmt.Errorf("-pages must be > 0, got %d", cfg.pages)
	}
	if cfg.churnPct < 0 || cfg.churnPct > 100 {
		return fmt.Errorf("-churn-pct must be in [0,100], got %d", cfg.churnPct)
	}

	// Bind loopback only. Resolve the host and refuse anything that is not a
	// loopback IP (defends the responsible-crawler posture: the benchmark origin
	// is never reachable off-box).
	host, _, err := net.SplitHostPort(cfg.addr)
	if err != nil {
		return fmt.Errorf("invalid -addr %q: %w", cfg.addr, err)
	}
	if err := requireLoopback(host); err != nil {
		return err
	}

	srv := newCorpusServer(cfg)

	ln, err := net.Listen("tcp", cfg.addr)
	if err != nil {
		return fmt.Errorf("listen on %q: %w", cfg.addr, err)
	}

	httpSrv := &http.Server{
		Handler: srv.mux(),
		// Bound the header read so a stuck client cannot pin a connection; the
		// daemon is a well-behaved local client so the values are generous.
		ReadHeaderTimeout: 10 * time.Second,
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	// Start the churn ticker (no-op when churn is disabled). It is cancelled by
	// the same context as the server.
	if cfg.churnPct > 0 {
		go srv.churnLoop(ctx)
	}

	// Print the bound address (host:port) on stdout so capacity.sh can capture
	// the real port when -addr used :0. One line, parseable.
	fmt.Printf("listening %s pages=%d churn_pct=%d etag=%v\n", ln.Addr().String(), cfg.pages, cfg.churnPct, cfg.etag)

	errc := make(chan error, 1)
	go func() {
		err := httpSrv.Serve(ln)
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			errc <- err
			return
		}
		errc <- nil
	}()

	select {
	case <-ctx.Done():
		shutCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		return httpSrv.Shutdown(shutCtx)
	case err := <-errc:
		return err
	}
}

// requireLoopback returns an error unless host resolves to a loopback address.
// An empty host (e.g. ":8080") is rejected because it binds all interfaces.
func requireLoopback(host string) error {
	if host == "" {
		return errors.New("-addr must bind a loopback host (127.0.0.1 or ::1), not all interfaces")
	}
	if ip := net.ParseIP(host); ip != nil {
		if ip.IsLoopback() {
			return nil
		}
		return fmt.Errorf("-addr host %q is not a loopback address; corpussite binds loopback only", host)
	}
	// A non-literal host (e.g. "localhost") must resolve entirely to loopback.
	ips, err := net.LookupIP(host)
	if err != nil {
		return fmt.Errorf("cannot resolve -addr host %q: %w", host, err)
	}
	for _, ip := range ips {
		if !ip.IsLoopback() {
			return fmt.Errorf("-addr host %q resolves to a non-loopback address; corpussite binds loopback only", host)
		}
	}
	return nil
}

// corpusServer holds the serving state. revisions maps a page index to its
// current revision epoch (0 = pristine benchcorpus.Page bytes; n>0 = the n-th
// mutated variant). A page absent from the map is pristine. The churn loop bumps
// revisions under the write lock; handlers read under the read lock.
type corpusServer struct {
	cfg config

	mu        sync.RWMutex
	revisions map[int]int // page index -> revision epoch
}

func newCorpusServer(cfg config) *corpusServer {
	return &corpusServer{cfg: cfg, revisions: make(map[int]int)}
}

// mux builds the HTTP routes: the page handler at "/" and the discard webhook at
// "/webhook".
func (s *corpusServer) mux() *http.ServeMux {
	m := http.NewServeMux()
	m.HandleFunc("/webhook", s.handleWebhook)
	m.HandleFunc("/", s.handlePage)
	return m
}

// handleWebhook is the alert sink: it drains and discards the request body and
// replies 200. The daemon's notifier delivery thus exercises the full alert path
// with zero egress (the body is never inspected, stored, or logged — it may
// contain alert content but no secret, and we keep it off disk/stdout anyway).
func (s *corpusServer) handleWebhook(w http.ResponseWriter, r *http.Request) {
	_, _ = io.Copy(io.Discard, r.Body)
	_ = r.Body.Close()
	w.WriteHeader(http.StatusOK)
}

// handlePage serves one corpus page resolved from the request path. Unknown
// paths and out-of-range indices return 404. With -etag it sets a revision-
// derived ETag and honors If-None-Match (304).
func (s *corpusServer) handlePage(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path == "/" {
		// A small index page so a manual curl of the root is not a 404; it is not
		// part of the corpus and carries no SEO content.
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, "rabbot benchmark corpus origin (loopback only)\n")
		return
	}

	class, index, ok := benchcorpus.ParsePath(r.URL.Path)
	if !ok || index < 0 || index >= s.cfg.pages {
		http.NotFound(w, r)
		return
	}

	rev := s.revisionOf(index)
	body := pageBytes(class, index, rev)

	if s.cfg.etag {
		etag := etagFor(class, index, rev)
		w.Header().Set("ETag", etag)
		// Honor a conditional request: if the client's If-None-Match matches the
		// current ETag, the page is unchanged -> 304 with no body. This is the
		// cheap not-modified path the daemon takes between churn epochs.
		if match := r.Header.Get("If-None-Match"); match != "" && etagMatches(match, etag) {
			w.WriteHeader(http.StatusNotModified)
			return
		}
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	if r.Method != http.MethodHead {
		// gosec G705 (XSS taint) is a false positive here: body is generated
		// entirely from benchcorpus + integer revisions (no request/user input
		// reaches it), and this is a loopback-only benchmark origin, never a
		// real web server. The deterministic corpus bytes are the whole point.
		_, _ = w.Write(body) //nolint:gosec // synthetic, deterministic, loopback-only corpus bytes
	}
}

// revisionOf returns the current revision epoch for a page index (0 = pristine).
func (s *corpusServer) revisionOf(index int) int {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.revisions[index]
}

// churnLoop bumps the revision of a deterministic share of pages every
// churn-every interval until ctx is cancelled. The share and which pages are
// chosen are fully deterministic (no math/rand): epoch e mutates the pages whose
// index falls in a sliding window of size churnPct% of pages, so over successive
// epochs the churned set rotates across the whole corpus at a steady rate. This
// models a site where a steady fraction of pages changes each interval.
func (s *corpusServer) churnLoop(ctx context.Context) {
	perEpoch := s.cfg.pages * s.cfg.churnPct / 100
	if perEpoch < 1 {
		perEpoch = 1 // churn-pct>0 always mutates at least one page
	}
	t := time.NewTicker(s.cfg.churnEvery)
	defer t.Stop()
	epoch := 0
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			epoch++
			start := (epoch - 1) * perEpoch
			s.mu.Lock()
			for i := 0; i < perEpoch; i++ {
				idx := (start + i) % s.cfg.pages
				// Bump this page to a new revision so its bytes (and ETag) change.
				s.revisions[idx]++
			}
			s.mu.Unlock()
		}
	}
}

// pageBytes returns the bytes for (class,index) at the given revision. Revision 0
// is the pristine benchcorpus page; a positive revision returns a deterministic
// mutated variant (see mutate).
func pageBytes(class benchcorpus.Class, index, rev int) []byte {
	page := benchcorpus.Page(class, index)
	if rev == 0 {
		return page
	}
	return mutate(page, index, rev)
}

// mutate returns a deterministically-altered copy of a pristine corpus page for
// a positive revision. It changes exactly two things the diff engine keys on —
// the <title> and the body content — by:
//
//   - appending " (rev N)" inside the existing <title>...</title>, and
//   - injecting a revision-marker <p> just before the closing </main>,
//
// where N is the revision epoch and the marker text is derived from (index, rev)
// via benchcorpus' own vocabulary so the inserted prose is realistic. Because N
// and the marker depend only on (index, rev), a given (page, revision) always
// produces identical bytes — the churn is reproducible. The original page is not
// modified (we build a new byte slice).
func mutate(page []byte, index, rev int) []byte {
	s := string(page)
	revStr := strconv.Itoa(rev)

	// Title: insert " (rev N)" before the first "</title>".
	if i := indexOf(s, "</title>"); i >= 0 {
		s = s[:i] + " (rev " + revStr + ")" + s[i:]
	}

	// Body: inject a revision-marker paragraph before the closing "</main>". The
	// marker text reuses benchcorpus content for the page so the new prose looks
	// like a real edit, plus the revision number so the content hash changes every
	// epoch.
	marker := "<p data-rev=\"" + revStr + "\">Updated revision " + revStr +
		" for " + benchcorpus.Path(benchcorpus.ClassForIndex(index), index) + ".</p>\n"
	if i := indexOf(s, "</main>"); i >= 0 {
		s = s[:i] + marker + s[i:]
	} else {
		// Defensive: no </main> (should not happen for corpus pages) — append.
		s += marker
	}
	return []byte(s)
}

// indexOf returns the first index of sub in s, or -1 (0 when sub is empty). It and
// the small string helpers below are kept inline so this single-file, dev-only
// tool stays self-contained; they are trivial and covered by the tests.
func indexOf(s, sub string) int {
	n, m := len(s), len(sub)
	if m == 0 {
		return 0
	}
	for i := 0; i+m <= n; i++ {
		if s[i:i+m] == sub {
			return i
		}
	}
	return -1
}

// etagFor builds a strong, stable ETag for a (class,index,revision). It changes
// iff the revision changes, so a stable page keeps a stable ETag and the daemon's
// conditional request gets a 304. The value is opaque; we encode the coordinates
// so it is human-debuggable in a capture.
func etagFor(class benchcorpus.Class, index, rev int) string {
	return "\"" + class.String() + "-" + strconv.Itoa(index) + "-r" + strconv.Itoa(rev) + "\""
}

// etagMatches reports whether an If-None-Match header value matches etag. It
// handles the common single-value and "*" cases and tolerates a weak-validator
// "W/" prefix on the client value (RFC 7232 weak comparison is acceptable for a
// not-modified decision). Multiple comma-separated values are checked in turn.
func etagMatches(ifNoneMatch, etag string) bool {
	v := trimSpace(ifNoneMatch)
	if v == "*" {
		return true
	}
	for _, candidate := range splitComma(v) {
		c := trimSpace(candidate)
		c = trimPrefix(c, "W/")
		if c == etag {
			return true
		}
	}
	return false
}

// The three helpers below are the same kind of small inline string helper as
// indexOf above — kept local so this single-file dev-only tool stays
// self-contained. They are trivial and covered by the tests.

func trimSpace(s string) string {
	start, end := 0, len(s)
	for start < end && (s[start] == ' ' || s[start] == '\t') {
		start++
	}
	for end > start && (s[end-1] == ' ' || s[end-1] == '\t') {
		end--
	}
	return s[start:end]
}

func trimPrefix(s, prefix string) string {
	if len(s) >= len(prefix) && s[:len(prefix)] == prefix {
		return s[len(prefix):]
	}
	return s
}

func splitComma(s string) []string {
	var out []string
	start := 0
	for i := 0; i < len(s); i++ {
		if s[i] == ',' {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	out = append(out, s[start:])
	return out
}
