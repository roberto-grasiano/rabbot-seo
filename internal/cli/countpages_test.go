package cli

import (
	"context"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
)

// TestProductionCountPagesPosture builds the production CountPages closure and
// asserts it (a) is non-nil and callable and (b) refuses a loopback target under
// the allowPrivate=false posture (the inner SSRF guard rejects it → !ok), proving
// no private-target bypass reaches production. It exercises the closure shape and
// the SSRF posture, not a live sitemap. config.Defaults() is the factory config
// (NOT config.Default()).
func TestProductionCountPagesPosture(t *testing.T) {
	cfg := config.Defaults()
	count := productionCountPages(&cfg, "9.9.9")
	if count == nil {
		t.Fatal("productionCountPages returned a nil closure")
	}
	if _, ok := count(context.Background(), "http://127.0.0.1:9/"); ok {
		t.Error("production CountPages must refuse a loopback target (allowPrivate=false)")
	}
}
