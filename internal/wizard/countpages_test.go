package wizard

import (
	"context"
	"testing"
)

// TestDepsCountPagesSeam pins the CountPages seam signature on Deps: a fake
// closure must satisfy the field type and be callable with (ctx, url), returning
// (int, bool). Phase 4's TUI step branches on these return values (ok => sitemap
// estimate path, !ok => ranged question), so the 2-return contract is locked here,
// away from any TTY.
func TestDepsCountPagesSeam(t *testing.T) {
	d := Deps{
		CountPages: func(_ context.Context, _ string) (int, bool) {
			return 4200, true
		},
	}
	n, ok := d.CountPages(context.Background(), "https://example.com")
	if n != 4200 || !ok {
		t.Fatalf("CountPages() = (%d, %v), want (4200, true)", n, ok)
	}
}
