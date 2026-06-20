// Package alerts is the incident state machine: it dedups, groups, throttles,
// and auto-closes incidents, suppressing SEO alerts when a fetch is not ok and
// instead maintaining operational monitoring_* incidents.
package alerts

import (
	"crypto/sha256"
	"encoding/hex"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// Event is the pipeline input: a single detected change/issue or operational
// state ready to become (or update) an incident.
type Event struct {
	SiteID      int64
	Site        string
	URL         string // empty for site-level / operational events
	URLID       int64
	ChangeType  string
	Severity    model.Severity
	Before      string
	After       string
	Operational bool // monitoring_blocked / monitoring_unreachable
	DeepLink    string
	// Segments is the set of segment names this event's URL belongs to (A7),
	// populated at emit time from the in-memory registry. It annotates the alert
	// and feeds segment-based route matching; it is deliberately NOT part of
	// Fingerprint or GroupKey, so segments never re-group or re-dedup incidents.
	// Site-level events (robots/sitemap, no URL) carry no segments.
	Segments []string
}

// Fingerprint is the dedup key: hash(site, url, change_type, severity).
func Fingerprint(e Event) string {
	h := sha256.New()
	h.Write([]byte(e.Site))
	h.Write([]byte{0})
	h.Write([]byte(e.URL))
	h.Write([]byte{0})
	h.Write([]byte(e.ChangeType))
	h.Write([]byte{0})
	h.Write([]byte(string(e.Severity)))
	return hex.EncodeToString(h.Sum(nil))
}

// GroupKey rolls events for a site + change_type into one incident.
func GroupKey(site, changeType string) string {
	return site + "|" + changeType
}

// groupFingerprint is the incident-identity fingerprint: the same as Fingerprint
// but with the URL elided, so all pages sharing site+change_type+severity map to
// one incident.
func groupFingerprint(e Event) string {
	ge := e
	ge.URL = ""
	return Fingerprint(ge)
}
