package behavior

import "github.com/roberto-grasiano/rabbot-seo/internal/model"

// Value-copy snapshot mutators. Each takes a base Snapshot by value and returns a
// modified COPY (Go struct copy), so a shared `healthy` baseline can spawn many
// scenario variants without aliasing. These keep the scenario tables terse and
// make the single mutated field obvious.

func setTitle(s model.Snapshot, v string) model.Snapshot            { s.Title = v; return s }
func setMeta(s model.Snapshot, v string) model.Snapshot             { s.MetaDescription = v; return s }
func setCanonical(s model.Snapshot, v string) model.Snapshot        { s.Canonical = v; return s }
func setMetaRobots(s model.Snapshot, v string) model.Snapshot       { s.MetaRobots = v; return s }
func setHeadings(s model.Snapshot, v string) model.Snapshot         { s.Headings = v; return s }
func setRedirect(s model.Snapshot, v string) model.Snapshot         { s.RedirectChain = v; return s }
func setHreflang(s model.Snapshot, v string) model.Snapshot         { s.Hreflang = v; return s }
func setStatus(s model.Snapshot, code int) model.Snapshot           { s.HTTPStatus = code; return s }
func setInternalLinks(s model.Snapshot, n int) model.Snapshot       { s.InternalLinkCount = n; return s }
func setExternalLinks(s model.Snapshot, n int) model.Snapshot       { s.ExternalLinkCount = n; return s }
func setRender(s model.Snapshot, m model.RenderMode) model.Snapshot { s.RenderMode = m; return s }

// setImages sets both the image count and the missing-alt count.
func setImages(s model.Snapshot, count, missing int) model.Snapshot {
	s.ImageCount = count
	s.MissingAltCount = missing
	return s
}

// withContent sets the content hash and SimHash together (the pair diff.Compare
// reads to classify a content change cosmetic vs substantive).
func withContent(s model.Snapshot, sha string, simhash uint64) model.Snapshot {
	s.ContentSHA256 = sha
	s.ContentSimhash = simhash
	return s
}

// withJSONLD sets the raw JSON-LD and the derived schema_types SET together (the
// rich-result rules read JSONLD; diff.Compare diffs only the schema_types set).
func withJSONLD(s model.Snapshot, jsonld, schemaTypes string) model.Snapshot {
	s.JSONLD = jsonld
	s.SchemaTypes = schemaTypes
	return s
}
