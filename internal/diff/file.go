package diff

import (
	"strconv"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// FileChange is a site-scoped change record for robots.txt / sitemap.xml file
// entities (which have no url_id), intended to feed the alerts pipeline alongside
// per-URL model.Change records.
type FileChange struct {
	SiteID         int64
	FileSnapshotID int64
	Kind           model.FileSnapshotKind
	Field          string
	OldValue       string
	NewValue       string
	DetectedAt     time.Time
}

// CompareFile diffs a new file snapshot against the prior one. A zero old (ID 0
// and empty hash) is a baseline and emits nothing. It reports content hash
// changes (robots_txt / sitemap_xml) and accessibility regressions
// (robots_txt_status / sitemap_xml_status) when the HTTP status changes.
//
// Both file entities are wired end-to-end: SideTimers.RefreshRobots diffs
// consecutive robots snapshots (robots_txt / robots_txt_status, #8) and
// SideTimers.RefreshSitemap diffs consecutive sitemap snapshots (sitemap_xml /
// sitemap_xml_status, A2), ingesting each change into the alert pipeline. The
// sitemap watch swaps the raw-hash sitemap_xml Before/After for a set-diff summary
// (counts + added/dropped samples) before ingest; this function reports the bare
// hash change that triggers it.
func CompareFile(new, old model.FileSnapshot, now time.Time) []FileChange {
	if old.ID == 0 && old.ContentSHA256 == "" {
		return nil
	}
	contentField, statusField := "sitemap_xml", "sitemap_xml_status"
	if new.Kind == model.FileKindRobots {
		contentField, statusField = "robots_txt", "robots_txt_status"
	}

	var out []FileChange
	add := func(field, oldV, newV string) {
		out = append(out, FileChange{
			SiteID:         new.SiteID,
			FileSnapshotID: new.ID,
			Kind:           new.Kind,
			Field:          field,
			OldValue:       oldV,
			NewValue:       newV,
			DetectedAt:     now,
		})
	}

	if old.ContentSHA256 != new.ContentSHA256 {
		add(contentField, old.ContentSHA256, new.ContentSHA256)
	}
	if old.HTTPStatus != new.HTTPStatus {
		add(statusField, strconv.Itoa(old.HTTPStatus), strconv.Itoa(new.HTTPStatus))
	}
	return out
}
