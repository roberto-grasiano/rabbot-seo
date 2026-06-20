package fetcher

import (
	"context"
	"net/url"
	"time"

	"github.com/temoto/robotstxt"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// DoctorReport is the rabbot doctor preflight diagnostic result.
type DoctorReport struct {
	Target         string
	HomepageStatus int
	RobotsStatus   int
	RedirectChain  []string
	FetchClass     model.FetchClass
	Detector       string
	Blocked        bool
	UserAgent      string
	Egress         EgressInfo
	RobotsVerdict  string // allowed/disallowed for the target path

	// RawHTML is the homepage response body already fetched during the preflight,
	// surfaced (not re-fetched) so downstream callers — e.g. internal/precheck's JS
	// detector — can inspect the served HTML without issuing a second request. It is
	// only populated for ok-class fetches (the fetcher returns a body only then); a
	// blocked/unreachable homepage leaves it nil.
	RawHTML []byte
	// ContentType is the homepage response's Content-Type header, surfaced alongside
	// RawHTML so callers can gate HTML parsing on the served media type.
	ContentType string
}

// Doctor runs the §5A preflight: fetches the homepage + robots.txt, classifies the
// response, computes the robots verdict for the target path, and looks up egress IP.
// allowPrivate disables the SSRF guard (test-only; production targets are public).
func Doctor(ctx context.Context, target string, req Request, userAgent, egressEndpoint string, allowPrivate bool) (DoctorReport, error) {
	rep := DoctorReport{Target: target, UserAgent: userAgent}

	f := New(Options{UserAgent: userAgent, Timeout: 15 * time.Second, MaxBodyBytes: 1 << 20, AllowPrivate: allowPrivate})

	// Homepage fetch (carry per-site access settings; classify regardless of class).
	hreq := req
	hreq.URL = target
	hreq.ETag = ""
	hreq.LastMod = ""
	hres, _ := f.Fetch(ctx, hreq)
	rep.HomepageStatus = hres.HTTPStatus
	rep.RedirectChain = hres.RedirectChain
	rep.FetchClass = hres.FetchClass
	rep.Detector = hres.Detector
	rep.Blocked = hres.FetchClass == model.FetchHardBlock || hres.FetchClass == model.FetchSoftBlock
	// Surface the already-fetched homepage body + Content-Type (additive; no second
	// fetch, no change to body limits or SSRF/redirect guards). hres.Body is only
	// populated for ok-class fetches, so blocked/unreachable homepages leave RawHTML nil.
	rep.RawHTML = hres.Body
	rep.ContentType = hres.Header.Get("Content-Type")

	// robots.txt fetch + verdict for the target path.
	origin, u, err := splitOrigin(target)
	if err == nil {
		rreq := req
		rreq.URL = origin + "/robots.txt"
		rreq.ETag = ""
		rreq.LastMod = ""
		rres, _ := f.Fetch(ctx, rreq)
		rep.RobotsStatus = rres.HTTPStatus
		rep.RobotsVerdict = robotsVerdict(rres, userAgent, u)
	}

	// Egress IP.
	if egressEndpoint != "" {
		if info, err := EgressIP(ctx, egressEndpoint, req.ProxyURL, allowPrivate); err == nil {
			rep.Egress = info
		}
	}
	return rep, nil
}

func splitOrigin(rawURL string) (string, *url.URL, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", nil, err
	}
	return u.Scheme + "://" + u.Host, u, nil
}

func robotsVerdict(rres Result, userAgent string, target *url.URL) string {
	// Missing/unreadable robots.txt => allowed.
	if rres.HTTPStatus == 404 || rres.HTTPStatus == 410 || len(rres.Body) == 0 {
		return "allowed"
	}
	data, err := robotstxt.FromBytes(rres.Body)
	if err != nil {
		return "allowed"
	}
	group := data.FindGroup(userAgent)
	path := target.EscapedPath()
	if path == "" {
		path = "/"
	}
	if group.Test(path) {
		return "allowed"
	}
	return "disallowed"
}
