// Package segments compiles per-site URL classifiers from config and serves
// hot-path, in-memory membership lookups.
//
// A segment is a named slice of a site (e.g. /blog, /product) defined by an
// anchored Go regexp matched against the URL PATH ONLY (query strings are
// excluded in v1). A URL may belong to multiple segments (M:N). Segment names
// are constrained to a route-key-friendly charset (lowercase letters, digits,
// '_' and '-') so they can serve as alert-routing keys.
//
// The Registry holds one SiteMatcher per site and supports an atomic Swap so
// config reload re-classifies without a daemon restart; lookups are
// allocation-light and never touch the database.
package segments

import (
	"fmt"
	"net/url"
	"regexp"
	"sync/atomic"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
)

// nameRE constrains segment names to the route-key charset: lowercase ASCII
// letters, digits, underscore and hyphen, at least one character.
var nameRE = regexp.MustCompile(`^[a-z0-9_-]+$`)

// ValidName reports whether name is an acceptable segment name. Exported so
// other slices (store sync, config validation) reuse the single source of
// truth.
func ValidName(name string) bool {
	return nameRE.MatchString(name)
}

// compiled is one segment's name + its compiled path matcher.
type compiled struct {
	name string
	re   *regexp.Regexp
	// id is the DB segment id, filled by Bind after persistence assigns one.
	// Zero means "not yet bound".
	id int64
}

// SiteMatcher classifies URLs for a single site against that site's ordered
// segment definitions. It is immutable after Compile (aside from Bind, which is
// called once before the matcher is published to a Registry), so concurrent
// Match/MatchIDs calls are safe.
type SiteMatcher struct {
	siteID   int64
	compiled []compiled
}

// Compile builds a SiteMatcher from a site's segment configs. Patterns are Go
// regexps matched against the URL path only. It returns an error naming the
// site and the offending segment when a name is duplicated, a name is outside
// the allowed charset, or a pattern fails to compile.
func Compile(siteID int64, segs []config.SegmentConfig) (*SiteMatcher, error) {
	m := &SiteMatcher{siteID: siteID, compiled: make([]compiled, 0, len(segs))}
	seen := make(map[string]struct{}, len(segs))
	for _, s := range segs {
		if !ValidName(s.Name) {
			return nil, fmt.Errorf("site %d segment %q: invalid name (allowed: lowercase letters, digits, '_', '-')", siteID, s.Name)
		}
		if _, dup := seen[s.Name]; dup {
			return nil, fmt.Errorf("site %d segment %q: duplicate name", siteID, s.Name)
		}
		seen[s.Name] = struct{}{}

		re, err := regexp.Compile(s.Match)
		if err != nil {
			return nil, fmt.Errorf("site %d segment %q: invalid pattern %q: %w", siteID, s.Name, s.Match, err)
		}
		m.compiled = append(m.compiled, compiled{name: s.Name, re: re})
	}
	return m, nil
}

// Bind associates DB segment ids with names so MatchIDs can return ids. Names
// absent from the map keep id 0. Call once before publishing to a Registry.
func (m *SiteMatcher) Bind(nameToID map[string]int64) {
	if m == nil {
		return
	}
	for i := range m.compiled {
		if id, ok := nameToID[m.compiled[i].name]; ok {
			m.compiled[i].id = id
		}
	}
}

// pathOf extracts the path component used as the match input. Query strings are
// excluded by design (v1). A URL that fails to parse yields an empty path, so it
// matches nothing rather than panicking.
func pathOf(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	return u.Path
}

// Match returns the names of every segment whose pattern matches the URL path,
// in config order. nil when none match (or the matcher is empty/nil).
func (m *SiteMatcher) Match(rawURL string) []string {
	if m == nil || len(m.compiled) == 0 {
		return nil
	}
	path := pathOf(rawURL)
	if path == "" {
		return nil
	}
	var out []string
	for i := range m.compiled {
		if m.compiled[i].re.MatchString(path) {
			out = append(out, m.compiled[i].name)
		}
	}
	return out
}

// MatchIDs returns the DB ids of every matching segment (in config order),
// skipping any segment not yet Bound. nil when none match.
func (m *SiteMatcher) MatchIDs(rawURL string) []int64 {
	if m == nil || len(m.compiled) == 0 {
		return nil
	}
	path := pathOf(rawURL)
	if path == "" {
		return nil
	}
	var out []int64
	for i := range m.compiled {
		if m.compiled[i].id != 0 && m.compiled[i].re.MatchString(path) {
			out = append(out, m.compiled[i].id)
		}
	}
	return out
}

// Names returns the segment names this matcher knows, in config order.
func (m *SiteMatcher) Names() []string {
	if m == nil {
		return nil
	}
	out := make([]string, len(m.compiled))
	for i := range m.compiled {
		out[i] = m.compiled[i].name
	}
	return out
}

// Registry maps site ids to their SiteMatcher and supports an atomic Swap so a
// config reload can re-classify with no daemon restart. SegmentsFor is the
// hot-path, in-memory lookup used by the alert pipeline and discovery — it never
// touches the database.
type Registry struct {
	// v holds a map[int64]*SiteMatcher. The whole map is replaced on Swap; it
	// is never mutated in place, so readers that load the pointer see a
	// consistent snapshot without a lock.
	v atomic.Value
}

// NewRegistry returns an empty Registry. Lookups before the first Swap return
// nil.
func NewRegistry() *Registry {
	r := &Registry{}
	r.v.Store(map[int64]*SiteMatcher(nil))
	return r
}

// Swap atomically replaces the entire site→matcher map. The provided map must
// not be mutated by the caller after this call. Passing nil clears the
// registry.
func (r *Registry) Swap(matchers map[int64]*SiteMatcher) {
	if matchers == nil {
		matchers = map[int64]*SiteMatcher{}
	}
	r.v.Store(matchers)
}

// Matcher returns the SiteMatcher for a site, or nil if none is registered.
func (r *Registry) Matcher(siteID int64) *SiteMatcher {
	m, _ := r.v.Load().(map[int64]*SiteMatcher)
	return m[siteID]
}

// SegmentsFor returns the segment names a URL belongs to within a site, in
// config order. nil when the site is unknown or the URL matches nothing. This is
// the in-memory hot-path lookup; it performs no DB access.
func (r *Registry) SegmentsFor(siteID int64, rawURL string) []string {
	return r.Matcher(siteID).Match(rawURL)
}

// SegmentIDsFor mirrors SegmentsFor but returns Bound DB ids — used by the
// discovery classify seam to write url_segments rows.
func (r *Registry) SegmentIDsFor(siteID int64, rawURL string) []int64 {
	return r.Matcher(siteID).MatchIDs(rawURL)
}
