package config

import (
	"testing"
	"time"
)

func TestResolvedUserAgent(t *testing.T) {
	tests := []struct {
		name     string
		uaConfig string
		contact  string
		version  string
		want     string
	}{
		{"explicit ua wins", "MyBot/1.0", "ops@x.example", "9.9.9", "MyBot/1.0"},
		{"default ua from version+email", "", "ops@x.example", "1.2.3",
			"Rabbot-SEO/1.2.3 (+mailto:ops@x.example)"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			c.Crawler.UserAgent = tc.uaConfig
			c.Crawler.ContactEmail = tc.contact
			if got := c.ResolvedUserAgent(tc.version); got != tc.want {
				t.Errorf("ResolvedUserAgent(%q) = %q, want %q", tc.version, got, tc.want)
			}
		})
	}
}

func TestUserAgentFor(t *testing.T) {
	const email = "ops@lottie.org"
	tests := []struct {
		name     string
		uaConfig string
		contact  string
		host     string
		version  string
		verified bool
		want     string
	}{
		{
			name: "override wins", uaConfig: "MyBot/1.0", contact: email,
			host: "lottie.org", version: "9.9.9", verified: true, want: "MyBot/1.0",
		},
		{
			name: "verified any email", contact: "ops@other.example",
			host: "lottie.org", version: "1.2.3", verified: true,
			want: "Rabbot-SEO/1.2.3 (+mailto:ops@other.example; verified for lottie.org)",
		},
		{
			name: "verified subdomain host", contact: "ops@other.example",
			host: "app.lottie.org", version: "1.2.3", verified: true,
			want: "Rabbot-SEO/1.2.3 (+mailto:ops@other.example; verified for lottie.org)",
		},
		{
			name: "unverified match", contact: email,
			host: "lottie.org", version: "1.2.3", verified: false,
			want: "Rabbot-SEO/1.2.3 (+mailto:ops@lottie.org; lottie.org contact, unverified)",
		},
		{
			name: "unverified match subdomain host", contact: email,
			host: "app.lottie.org", version: "1.2.3", verified: false,
			want: "Rabbot-SEO/1.2.3 (+mailto:ops@lottie.org; lottie.org contact, unverified)",
		},
		{
			name: "unverified match multipart tld", contact: "ops@brand.co.uk",
			host: "shop.brand.co.uk", version: "1.2.3", verified: false,
			want: "Rabbot-SEO/1.2.3 (+mailto:ops@brand.co.uk; brand.co.uk contact, unverified)",
		},
		{
			// A single-target caller (doctor/wizard) may pass host:port. The port
			// must be stripped before publicsuffix so a same-domain site is matched,
			// not mislabeled "unverified — confirm or block".
			name: "unverified match host with port", contact: email,
			host: "lottie.org:8080", version: "1.2.3", verified: false,
			want: "Rabbot-SEO/1.2.3 (+mailto:ops@lottie.org; lottie.org contact, unverified)",
		},
		{
			name: "unverified mismatch", contact: email,
			host: "example.com", version: "1.2.3", verified: false,
			want: "Rabbot-SEO/1.2.3 (+mailto:ops@lottie.org; unverified — confirm or block)",
		},
		{
			name: "unverified empty host", contact: email,
			host: "", version: "1.2.3", verified: false,
			want: "Rabbot-SEO/1.2.3 (+mailto:ops@lottie.org; unverified — confirm or block)",
		},
		{
			name: "unverified bad host", contact: email,
			host: "localhost", version: "1.2.3", verified: false,
			want: "Rabbot-SEO/1.2.3 (+mailto:ops@lottie.org; unverified — confirm or block)",
		},
		{
			name: "case-insensitive match", contact: "ops@Lottie.ORG",
			host: "APP.Lottie.org", version: "1.2.3", verified: false,
			want: "Rabbot-SEO/1.2.3 (+mailto:ops@Lottie.ORG; lottie.org contact, unverified)",
		},
		{
			name: "verified bad host falls back to the site", contact: email,
			host: "localhost", version: "1.2.3", verified: true,
			want: "Rabbot-SEO/1.2.3 (+mailto:ops@lottie.org; verified for the site)",
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			c := Defaults()
			c.Crawler.UserAgent = tc.uaConfig
			c.Crawler.ContactEmail = tc.contact
			if got := c.UserAgentFor(tc.host, tc.version, tc.verified); got != tc.want {
				t.Errorf("UserAgentFor(%q, %q, %v) = %q, want %q",
					tc.host, tc.version, tc.verified, got, tc.want)
			}
		})
	}
}

func TestIntervalDurations(t *testing.T) {
	c := Defaults()
	if got := c.MinIntervalDuration(); got != 10*time.Minute {
		t.Errorf("MinIntervalDuration = %v, want 10m", got)
	}
	if got := c.MaxIntervalDuration(); got != 24*time.Hour {
		t.Errorf("MaxIntervalDuration = %v, want 24h", got)
	}
}

func TestIntervalDurationsFallBackOnBadValue(t *testing.T) {
	c := Defaults()
	c.Defaults.MinInterval = "not-a-duration"
	c.Defaults.MaxInterval = ""
	if got := c.MinIntervalDuration(); got != 10*time.Minute {
		t.Errorf("MinIntervalDuration fallback = %v, want 10m", got)
	}
	if got := c.MaxIntervalDuration(); got != 24*time.Hour {
		t.Errorf("MaxIntervalDuration fallback = %v, want 24h", got)
	}
}

func TestAccessForBaseURL(t *testing.T) {
	c := Defaults()
	c.Sites = []SiteConfig{
		{URL: "https://example.com", Access: AccessConfig{BasicUser: "alice"}},
		{URL: "https://other.example", Access: AccessConfig{ProxyURL: "http://proxy:8080"}},
	}
	tests := []struct {
		name     string
		baseURL  string
		wantUser string
		wantPxy  string
	}{
		{"match first", "https://example.com", "alice", ""},
		{"match second", "https://other.example", "", "http://proxy:8080"},
		{"no match returns zero", "https://nope.example", "", ""},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := c.AccessForBaseURL(tc.baseURL)
			if got.BasicUser != tc.wantUser {
				t.Errorf("BasicUser = %q, want %q", got.BasicUser, tc.wantUser)
			}
			if got.ProxyURL != tc.wantPxy {
				t.Errorf("ProxyURL = %q, want %q", got.ProxyURL, tc.wantPxy)
			}
		})
	}
}
