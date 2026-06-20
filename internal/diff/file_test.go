package diff

import (
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

func TestCompareFileSnapshot(t *testing.T) {
	now := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name      string
		old       model.FileSnapshot
		new       model.FileSnapshot
		wantField string
		wantEmit  bool
	}{
		{
			name:     "baseline robots emits nothing",
			old:      model.FileSnapshot{},
			new:      model.FileSnapshot{Kind: model.FileKindRobots, ContentSHA256: "a", HTTPStatus: 200},
			wantEmit: false,
		},
		{
			name:      "robots content change",
			old:       model.FileSnapshot{ID: 1, Kind: model.FileKindRobots, ContentSHA256: "a", HTTPStatus: 200},
			new:       model.FileSnapshot{ID: 2, SiteID: 9, Kind: model.FileKindRobots, ContentSHA256: "b", HTTPStatus: 200},
			wantField: "robots_txt",
			wantEmit:  true,
		},
		{
			name:      "sitemap content change",
			old:       model.FileSnapshot{ID: 1, Kind: model.FileKindSitemap, ContentSHA256: "x", HTTPStatus: 200},
			new:       model.FileSnapshot{ID: 2, SiteID: 9, Kind: model.FileKindSitemap, ContentSHA256: "y", HTTPStatus: 200},
			wantField: "sitemap_xml",
			wantEmit:  true,
		},
		{
			name:      "robots became inaccessible",
			old:       model.FileSnapshot{ID: 1, Kind: model.FileKindRobots, ContentSHA256: "a", HTTPStatus: 200},
			new:       model.FileSnapshot{ID: 2, SiteID: 9, Kind: model.FileKindRobots, ContentSHA256: "a", HTTPStatus: 503},
			wantField: "robots_txt_status",
			wantEmit:  true,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			changes := CompareFile(tc.new, tc.old, now)
			if !tc.wantEmit {
				if len(changes) != 0 {
					t.Fatalf("want no changes, got %+v", changes)
				}
				return
			}
			found := false
			for _, c := range changes {
				if c.Field == tc.wantField {
					found = true
					if c.SiteID != 9 {
						t.Errorf("expected SiteID 9, got %d", c.SiteID)
					}
					if !c.DetectedAt.Equal(now) {
						t.Errorf("expected DetectedAt %v, got %v", now, c.DetectedAt)
					}
				}
			}
			if !found {
				t.Errorf("expected a %q change, got %+v", tc.wantField, changes)
			}
		})
	}
}
