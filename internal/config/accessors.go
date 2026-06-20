package config

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"golang.org/x/net/publicsuffix"
)

// ResolvedUserAgent returns the configured crawler.user_agent verbatim if set,
// otherwise the host-agnostic default form
// Rabbot-SEO/<version> (+mailto:<contact_email>). The version is supplied by
// the caller (BuildInfo) since config has no build deps. Use UserAgentFor for the
// per-site trust-signalling form when the target host is known.
func (c *Config) ResolvedUserAgent(version string) string {
	if c.Crawler.UserAgent != "" {
		return c.Crawler.UserAgent
	}
	return fmt.Sprintf("Rabbot-SEO/%s (+mailto:%s)", version, c.Crawler.ContactEmail)
}

// UserAgentFor returns the per-site, trust-signalling crawler User-Agent for a
// crawl of host. The crawler.user_agent override, when set, always wins verbatim.
//
// Otherwise it composes a contact + trust suffix, verification first (the strong
// proof) then email-domain match as a fallback hint:
//
//  1. verified (any email):  …; verified for <siteDomain>
//  2. !verified && match:     …; <siteDomain> contact, unverified
//  3. !verified && !match:    …; unverified — confirm or block
//
// Match is a case-insensitive equality of the registrable domain (eTLD+1) of the
// operator's email and of host. When host is empty/unparseable the registrable
// domain is unknown: siteDomain renders as "the site" and the match is false
// (state 3, the most cautious branch — or state 1 if the site is verified).
func (c *Config) UserAgentFor(host, version string, verified bool) string {
	if c.Crawler.UserAgent != "" {
		return c.Crawler.UserAgent
	}
	base := fmt.Sprintf("Rabbot-SEO/%s (+mailto:%s", version, c.Crawler.ContactEmail)

	siteDomain, siteOK := registrableDomain(host)
	switch {
	case verified:
		site := "the site"
		if siteOK {
			site = siteDomain
		}
		return base + fmt.Sprintf("; verified for %s)", site)
	case siteOK && domainsMatch(c.Crawler.ContactEmail, siteDomain):
		return base + fmt.Sprintf("; %s contact, unverified)", siteDomain)
	default:
		return base + "; unverified — confirm or block)"
	}
}

// registrableDomain returns the lowercased eTLD+1 (registrable domain) of host
// and whether it could be derived. publicsuffix preserves the input case, so we
// lowercase for a predictable, comparable result.
//
// A single-target caller (doctor/wizard) may pass a host:port value, so strip any
// explicit port first via url.URL.Hostname() — otherwise publicsuffix sees
// "lottie.org:8080", fails to derive an eTLD+1, and a same-domain site is
// mislabeled "unverified — confirm or block". Hostname() also unwraps an IPv6
// literal's brackets, matching the bare host the rest of the chain compares.
func registrableDomain(host string) (string, bool) {
	if host == "" {
		return "", false
	}
	host = (&url.URL{Host: host}).Hostname()
	if host == "" {
		return "", false
	}
	d, err := publicsuffix.EffectiveTLDPlusOne(host)
	if err != nil || d == "" {
		return "", false
	}
	return strings.ToLower(d), true
}

// domainsMatch reports whether the registrable domain of email's domain part
// equals siteDomain (already a lowercased eTLD+1), case-insensitively.
func domainsMatch(email, siteDomain string) bool {
	at := strings.LastIndexByte(email, '@')
	if at < 0 {
		return false
	}
	emailDomain, ok := registrableDomain(email[at+1:])
	if !ok {
		return false
	}
	return emailDomain == siteDomain
}

// MinIntervalDuration parses Defaults.MinInterval, falling back to 10m on a
// missing or unparseable value.
func (c *Config) MinIntervalDuration() time.Duration {
	return parseDurationOr(c.Defaults.MinInterval, 10*time.Minute)
}

// MaxIntervalDuration parses Defaults.MaxInterval, falling back to 24h on a
// missing or unparseable value.
func (c *Config) MaxIntervalDuration() time.Duration {
	return parseDurationOr(c.Defaults.MaxInterval, 24*time.Hour)
}

// RetentionSweepInterval parses Retention.SweepInterval, falling back to 6h.
func (c *Config) RetentionSweepInterval() time.Duration {
	return parseDurationOr(c.Retention.SweepInterval, 6*time.Hour)
}

// RetentionSnapshotMaxAge parses Retention.SnapshotMaxAge, falling back to 720h
// (30d) on a missing/unparseable value. An explicit "0"/"0s" parses to 0, which
// the sweep treats as "Layer 2 disabled".
func (c *Config) RetentionSnapshotMaxAge() time.Duration {
	return parseDurationOr(c.Retention.SnapshotMaxAge, 720*time.Hour)
}

// GraphSweepInterval parses Graph.SweepInterval (the A9 click-depth BFS cadence),
// falling back to 6h on a missing/unparseable value.
func (c *Config) GraphSweepInterval() time.Duration {
	return parseDurationOr(c.Graph.SweepInterval, 6*time.Hour)
}

// AccessForBaseURL returns the §5A AccessConfig for the SiteConfig whose URL
// matches baseURL, or the zero AccessConfig when no site matches.
func (c *Config) AccessForBaseURL(baseURL string) AccessConfig {
	for _, s := range c.Sites {
		if s.URL == baseURL {
			return s.Access
		}
	}
	return AccessConfig{}
}

func parseDurationOr(s string, fallback time.Duration) time.Duration {
	if s == "" {
		return fallback
	}
	d, err := time.ParseDuration(s)
	if err != nil {
		return fallback
	}
	return d
}
