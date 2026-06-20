package cli

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/go-co-op/gocron/v2"

	"github.com/roberto-grasiano/rabbot-seo/internal/linkgraph"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// TestRegisterGraphSweep_NilGrapherRegistersNothing pins the disabled arm: a nil
// Grapher (graph feature off) registers NO job and reports registered=false.
func TestRegisterGraphSweep_NilGrapherRegistersNothing(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := openCLITestStore(t)

	s, err := gocron.NewScheduler()
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })

	registered, err := registerGraphSweep(context.Background(), logger, s, db, nil, time.Hour)
	if err != nil {
		t.Fatalf("registerGraphSweep(nil): %v", err)
	}
	if registered {
		t.Fatal("nil grapher must report registered=false")
	}
	if len(s.Jobs()) != 0 {
		t.Fatalf("nil grapher must register no job; got %d jobs", len(s.Jobs()))
	}
}

// TestRegisterGraphSweep_EnabledRegistersOneJob pins the enabled arm: a non-nil
// Grapher registers exactly one job and reports registered=true.
func TestRegisterGraphSweep_EnabledRegistersOneJob(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	db := openCLITestStore(t)
	g := linkgraph.NewGrapher(db)

	s, err := gocron.NewScheduler()
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })

	registered, err := registerGraphSweep(context.Background(), logger, s, db, g, time.Hour)
	if err != nil {
		t.Fatalf("registerGraphSweep: %v", err)
	}
	if !registered {
		t.Fatal("non-nil grapher must report registered=true")
	}
	if len(s.Jobs()) != 1 {
		t.Fatalf("enabled: want exactly 1 job, got %d", len(s.Jobs()))
	}
}

// TestRegisterGraphSweep_TaskBodyRunsSweep proves the registered job's TASK BODY
// actually runs the per-site sweep (not just that a job exists). The sweep writes
// urls.graph_depth via SweepGraphDepths; we observe that side effect through a focus
// export (GraphDepth becomes non-nil once the BFS has run). WithStartImmediately
// fires the job on Start(), so we poll the export until the root's depth is written.
//
// A regression that left the task closure empty, or that skipped enabled sites,
// would never populate graph_depth and this test would time out.
func TestRegisterGraphSweep_TaskBodyRunsSweep(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, site := seedCLIGraphStoreDirect(t)
	g := linkgraph.NewGrapher(db)

	s, err := gocron.NewScheduler()
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })

	registered, err := registerGraphSweep(ctx, logger, s, db, g, time.Hour)
	if err != nil {
		t.Fatalf("registerGraphSweep: %v", err)
	}
	if !registered {
		t.Fatal("expected the sweep to register")
	}
	s.Start()

	// Poll the focus export: once the BFS sweep has written graph_depth, the
	// homepage (root, depth 0) carries a non-nil GraphDepth.
	deadline := time.Now().Add(5 * time.Second)
	for {
		exp, eerr := g.Export(ctx, linkgraph.Query{SiteID: site.ID, Focus: "https://cli.test/", Hops: 2})
		if eerr != nil {
			t.Fatalf("Export: %v", eerr)
		}
		var rootDepthWritten bool
		for _, n := range exp.Nodes {
			if n.URL == "https://cli.test/" && n.GraphDepth != nil {
				if *n.GraphDepth != 0 {
					t.Fatalf("root graph_depth = %d, want 0 (BFS root)", *n.GraphDepth)
				}
				rootDepthWritten = true
			}
		}
		if rootDepthWritten {
			return // the task body ran the sweep and wrote depths
		}
		if time.Now().After(deadline) {
			t.Fatal("graph sweep task body never wrote graph_depth within 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// TestRegisterGraphSweep_SkipsDisabledSites proves the task body honors the
// per-site Enabled gate: a DISABLED site is never swept, so its pages keep a NULL
// graph_depth even after the sweep fires. We seed one disabled site, run the sweep,
// and confirm its root depth stays unwritten while a sibling enabled site IS swept.
func TestRegisterGraphSweep_SkipsDisabledSites(t *testing.T) {
	t.Parallel()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	db, err := store.Open(ctx, filepath.Join(t.TempDir(), "sweepgate.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Date(2026, 6, 13, 12, 0, 0, 0, time.UTC)
	// base CARRIES the trailing slash and equals the admitted root url so the BFS
	// sweep's siteRootURL anchor (url == base_url) resolves.
	mkSite := func(base string, enabled bool) model.Site {
		id, aerr := db.AddSite(ctx, model.Site{
			BaseURL: base, Name: base, Enabled: enabled,
			MinInterval: 600, MaxInterval: 86400, MaxConcurrency: 1, SpeedScale: 100,
		})
		if aerr != nil {
			t.Fatalf("AddSite(%q): %v", base, aerr)
		}
		if _, uerr := db.UpsertURL(ctx, model.URL{
			SiteID: id, URL: base, FirstSeen: now, NextCheckAt: now,
			Interval: 600, Importance: 1.0, StatusType: model.StatusPage,
		}); uerr != nil {
			t.Fatalf("UpsertURL(%q root): %v", base, uerr)
		}
		st, gerr := db.GetSite(ctx, id)
		if gerr != nil {
			t.Fatalf("GetSite: %v", gerr)
		}
		return st
	}
	enabled := mkSite("https://on.test/", true)
	disabled := mkSite("https://off.test/", false)

	g := linkgraph.NewGrapher(db)
	s, err := gocron.NewScheduler()
	if err != nil {
		t.Fatalf("scheduler: %v", err)
	}
	t.Cleanup(func() { _ = s.Shutdown() })
	if _, rerr := registerGraphSweep(ctx, logger, s, db, g, time.Hour); rerr != nil {
		t.Fatalf("registerGraphSweep: %v", rerr)
	}
	s.Start()

	rootDepthWritten := func(st model.Site) bool {
		exp, eerr := g.Export(ctx, linkgraph.Query{SiteID: st.ID, Focus: st.BaseURL, Hops: 2})
		if eerr != nil {
			t.Fatalf("Export(%q): %v", st.BaseURL, eerr)
		}
		for _, n := range exp.Nodes {
			if n.URL == st.BaseURL && n.GraphDepth != nil {
				return true
			}
		}
		return false
	}

	// Wait until the enabled site has been swept (its root depth is written).
	deadline := time.Now().Add(5 * time.Second)
	for !rootDepthWritten(enabled) {
		if time.Now().After(deadline) {
			t.Fatal("enabled site never swept within 5s")
		}
		time.Sleep(20 * time.Millisecond)
	}
	// The disabled site must NOT have been swept.
	if rootDepthWritten(disabled) {
		t.Fatal("disabled site was swept; the Enabled gate in the task body was not honored")
	}
}
