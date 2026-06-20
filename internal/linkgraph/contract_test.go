package linkgraph

import (
	"context"

	"github.com/roberto-grasiano/rabbot-seo/internal/alerts"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// graphHook mirrors the structural scheduler.Crawler.Graph interface (scheduler
// never imports this package; the contract is structural). *Grapher must satisfy
// it so it can be wired as Crawler.Graph in run.go — a signature drift here fails
// to compile, catching the wiring break in this package's own tests rather than
// at the call site.
type graphHook interface {
	SyncPage(ctx context.Context, site model.Site, u model.URL, links []string) error
}

var _ graphHook = (*Grapher)(nil)

// _ asserts *Grapher.BlastRadius is assignable to the Processor's WithBlastRadius
// func shape (scheduler.WithBlastRadius takes exactly this signature). A drift in
// either the method or the option signature fails to compile here.
var _ func(ctx context.Context, siteID int64, url string) (inlinks, highImportance int, ok bool) = (*Grapher)(nil).BlastRadius

// _ asserts *alerts.Pipeline satisfies the AlertSink the Grapher writes through
// (Ingest + Resolve), so the production wiring (NewGrapher(db,
// WithAlertSink(pipeline))) compiles.
var _ AlertSink = (*alerts.Pipeline)(nil)
