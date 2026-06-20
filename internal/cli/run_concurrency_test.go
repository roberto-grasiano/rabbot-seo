package cli

import (
	"bytes"
	"context"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

// TestConcurrentAddSiteNoLostUpdates is the regression test for High#1: the
// config-mutating control hooks (add_site / remove_site / set_config) did an
// UNSERIALIZED file read-modify-write (config.AddSiteYAML) + d.Reload(). The std
// http.Server dispatches each request on its own goroutine, and cfgMu only guards
// the in-memory cfg inside Reload(), NOT the on-disk RMW — so N concurrent
// add_site calls interleave their read-modify-write of config.yaml and silently
// lose ~80-95% of updates, diverging config.yaml from the DB.
//
// This test fires N=20 CONCURRENT add_site operations (distinct, admission-valid
// base URLs) against a real daemon driven through the real control client, then
// asserts ALL 20 persisted to BOTH config.yaml and the DB with no divergence and
// no lost updates. Without the cfgWriteMu serialization it fails (only a handful
// of sites survive the lost-update race); with it, all 20 land in both stores.
//
// The 20 base URLs all point at one loopback httptest origin under distinct paths
// (srv.URL + "/site-N/"), so each add is a legitimate distinct add that resolves
// instantly — the daemon runs with AllowPrivate so its SSRF guard admits loopback
// (production never sets that), keeping the reconcile after each add fast instead
// of stalling on real DNS/connect for a fake host.
func TestConcurrentAddSiteNoLostUpdates(t *testing.T) {
	// Loopback origin: answers robots/sitemap/page so each add's reconcile resolves
	// instantly. The handler bodies are irrelevant to this race regression — what
	// matters is that the host resolves and the reconcile returns promptly.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte("<html><head><title>t</title></head><body>ok</body></html>"))
	}))
	defer srv.Close()

	// Grab a free port, then release it so the control server can bind it.
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("net.Listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.yaml")
	// Seed a minimal config file so the daemon's reload path read-modify-writes a
	// real file (an empty ConfigPath would route to defaults and skip the file RMW).
	// Disable sitemap + link discovery so each add's reconcile is a fast DB-only
	// re-sync — this regression isolates the config.yaml WRITE race, not the
	// (Frontier-gated, network-bound) discovery path that reconcile would otherwise
	// run inside the serialized critical section.
	seed := "data_dir: '" + dir + "'\n" + // single-quoted YAML: Windows backslashes are literal, not escapes
		"defaults:\n  discovery:\n    sitemap: false\n    follow_links: false\n" +
		"sites: []\n"
	if werr := os.WriteFile(cfgPath, []byte(seed), 0o600); werr != nil {
		t.Fatalf("seed config.yaml: %v", werr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var out bytes.Buffer

	done := make(chan error, 1)
	go func() {
		done <- runDaemon(ctx, &out, daemonOptions{
			ConfigPath:         cfgPath,
			DataDir:            dir,
			ControlToken:       "tok",
			ControlPort:        port,
			Version:            "0.0.1",
			LogLevel:           "error",
			TickInterval:       5 * time.Millisecond,
			EgressCheckEnabled: false,
			AllowPrivate:       true, // admit loopback httptest base URLs in this test
		})
	}()
	// Always drain the daemon goroutine before the test exits.
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(30 * time.Second):
			t.Error("runDaemon did not exit within 30s of cancel")
		}
	}()

	client := control.NewClient(port, "tok")

	// Wait for the control server to bind and serve (poll Health).
	deadline := time.Now().Add(30 * time.Second) // generous: windows-latest under -race needs >3s to start
	for {
		if herr := client.Health(context.Background()); herr == nil {
			break
		}
		select {
		case derr := <-done:
			done <- derr // re-buffer for the drain path
			t.Fatalf("daemon exited before becoming healthy: %v; logs:\n%s", derr, out.String())
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("control server did not become healthy within 30s; logs:\n%s", out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}

	const n = 20
	urls := make([]string, n)
	for i := range urls {
		urls[i] = fmt.Sprintf("%s/site-%d/", srv.URL, i)
	}

	// Fire N concurrent add_site calls. A shared start gate maximizes the overlap
	// of their config.yaml read-modify-write windows so the lost-update race is
	// reliably triggered without the serialization fix.
	var wg sync.WaitGroup
	start := make(chan struct{})
	errs := make([]error, n)
	for i := range urls {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			cctx, ccancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer ccancel()
			_, errs[i] = client.AddSite(cctx, control.AddSiteRequest{URL: urls[i]})
		}(i)
	}
	close(start)
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("AddSite(%s) returned error: %v", urls[i], e)
		}
	}

	// Assert ALL 20 persisted to config.yaml (the on-disk RMW must not lose any).
	loaded, lerr := config.Load(cfgPath, nil)
	if lerr != nil {
		t.Fatalf("reload config.yaml after concurrent adds: %v", lerr)
	}
	inFile := make(map[string]bool, len(loaded.Sites))
	for _, s := range loaded.Sites {
		inFile[s.URL] = true
	}
	missingFile := 0
	for _, u := range urls {
		if !inFile[u] {
			missingFile++
		}
	}
	if missingFile > 0 {
		t.Errorf("config.yaml lost %d/%d sites to the concurrent-add race (have %d); lost updates not serialized",
			missingFile, n, len(loaded.Sites))
	}

	// Assert ALL 20 persisted to the DB too (no divergence between file and DB).
	// Read through the daemon's own control read path (client.Sites) rather than
	// opening a second connection to the DB file the daemon still holds open.
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	dbSites, derr := client.Sites(sctx)
	if derr != nil {
		t.Fatalf("client.Sites: %v", derr)
	}
	inDB := make(map[string]bool, len(dbSites))
	for _, s := range dbSites {
		inDB[s.URL] = true
	}
	missingDB := 0
	for _, u := range urls {
		if !inDB[u] {
			missingDB++
		}
	}
	if missingDB > 0 {
		t.Errorf("DB lost %d/%d sites to the concurrent-add race (have %d); config.yaml and DB diverged",
			missingDB, n, len(dbSites))
	}
}

// TestLogFileKnobIsHonored is the regression test for the dead log.file knob:
// runDaemon always built obs.NewLogger(out, …) and never consumed cfg.Log.File,
// so the advertised log.file setting did nothing. The fix loads config before
// building the logger and, when log.file is set, routes structured logs to a
// size-rotating file writer. This asserts the configured file is created and
// receives the daemon's structured log lines, while the io.Writer passed to
// runDaemon (stdout in production) stays empty.
func TestLogFileKnobIsHonored(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "rabbot.log")
	cfgPath := filepath.Join(dir, "config.yaml")
	seed := "data_dir: '" + dir + "'\n" + // single-quoted YAML: Windows backslashes are literal, not escapes
		"log:\n  level: info\n  file: '" + logPath + "'\n" +
		"sites: []\n"
	if werr := os.WriteFile(cfgPath, []byte(seed), 0o600); werr != nil {
		t.Fatalf("seed config.yaml: %v", werr)
	}

	ctx, cancel := context.WithCancel(context.Background())
	var out bytes.Buffer
	done := make(chan error, 1)
	go func() {
		done <- runDaemon(ctx, &out, daemonOptions{
			ConfigPath:         cfgPath,
			DataDir:            dir,
			ControlToken:       "tok",
			ControlPort:        0, // no control server needed for this test
			Version:            "0.0.1",
			LogLevel:           "info", // info so "daemon starting" emits
			TickInterval:       5 * time.Millisecond,
			EgressCheckEnabled: false,
		})
	}()

	// Poll for the log file to appear and carry the startup line, then shut down.
	deadline := time.Now().Add(30 * time.Second) // generous: windows-latest under -race needs >3s to start
	var contents []byte
	for {
		b, rerr := os.ReadFile(logPath)
		if rerr == nil && bytes.Contains(b, []byte("daemon starting")) {
			contents = b
			break
		}
		select {
		case derr := <-done:
			done <- derr // re-buffer for the drain path
			t.Fatalf("daemon exited before logging: %v; io.Writer out=%q", derr, out.String())
		default:
		}
		if time.Now().After(deadline) {
			cancel()
			<-done
			t.Fatalf("log.file %q never received the startup line (rerr=%v); io.Writer out=%q", logPath, rerr, out.String())
		}
		time.Sleep(5 * time.Millisecond)
	}

	cancel()
	select {
	case <-done:
	case <-time.After(30 * time.Second):
		t.Fatal("runDaemon did not exit within 30s of cancel")
	}

	if !bytes.Contains(contents, []byte("daemon starting")) {
		t.Errorf("log file did not contain the daemon-starting line; got:\n%s", contents)
	}
	// The structured logs went to the file, NOT to the io.Writer passed to runDaemon.
	if out.Len() != 0 {
		t.Errorf("log.file set but the io.Writer still received output (%d bytes): %q", out.Len(), out.String())
	}
}
