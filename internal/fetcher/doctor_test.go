package fetcher

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

func TestDoctorBlockedSite(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			_, _ = w.Write([]byte("User-agent: *\nDisallow: /private/\n"))
			return
		}
		w.Header().Set("Cf-Mitigated", "challenge")
		w.WriteHeader(403)
		_, _ = w.Write([]byte("Attention Required! | Cloudflare"))
	}))
	defer site.Close()

	egress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("198.51.100.4"))
	}))
	defer egress.Close()

	rep, err := Doctor(context.Background(), site.URL, Request{URL: site.URL},
		"Rabbot-SEO/test (+https://example.test)", egress.URL, true)
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if rep.FetchClass != model.FetchHardBlock {
		t.Errorf("FetchClass = %q, want hard_block", rep.FetchClass)
	}
	if rep.Detector != "cloudflare" {
		t.Errorf("Detector = %q, want cloudflare", rep.Detector)
	}
	if !rep.Blocked {
		t.Errorf("Blocked = false, want true")
	}
	if rep.HomepageStatus != 403 {
		t.Errorf("HomepageStatus = %d, want 403", rep.HomepageStatus)
	}
	if rep.RobotsStatus != 200 {
		t.Errorf("RobotsStatus = %d, want 200", rep.RobotsStatus)
	}
	if rep.UserAgent != "Rabbot-SEO/test (+https://example.test)" {
		t.Errorf("UserAgent = %q", rep.UserAgent)
	}
	if len(rep.Egress.IPs) != 1 || rep.Egress.IPs[0] != "198.51.100.4" {
		t.Errorf("Egress.IPs = %v", rep.Egress.IPs)
	}
	if rep.RobotsVerdict != "allowed" {
		t.Errorf("RobotsVerdict = %q, want allowed for homepage path", rep.RobotsVerdict)
	}
}

func TestDoctorAllowedSite(t *testing.T) {
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		_, _ = w.Write([]byte("<html><title>ok</title></html>"))
	}))
	defer site.Close()
	egress := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("198.51.100.9"))
	}))
	defer egress.Close()

	rep, err := Doctor(context.Background(), site.URL, Request{URL: site.URL}, "Rabbot-SEO/test", egress.URL, true)
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if rep.Blocked {
		t.Errorf("Blocked = true, want false")
	}
	if rep.FetchClass != model.FetchOK {
		t.Errorf("FetchClass = %q, want ok", rep.FetchClass)
	}
}

// TestDoctorSurfacesHomepageBody asserts Doctor additively surfaces the already-fetched
// homepage body and Content-Type so downstream precheck can run the JS detector without
// issuing a second fetch.
func TestDoctorSurfacesHomepageBody(t *testing.T) {
	const body = `<html><head><title>surfaced</title></head><body><h1>real content here</h1></body></html>`
	site := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/robots.txt" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(body))
	}))
	defer site.Close()

	rep, err := Doctor(context.Background(), site.URL, Request{URL: site.URL}, "Rabbot-SEO/test", "", true)
	if err != nil {
		t.Fatalf("Doctor() error = %v", err)
	}
	if !bytes.Contains(rep.RawHTML, []byte("real content here")) {
		t.Errorf("RawHTML = %q, want it to contain the homepage body", rep.RawHTML)
	}
	if rep.ContentType != "text/html; charset=utf-8" {
		t.Errorf("ContentType = %q, want text/html; charset=utf-8", rep.ContentType)
	}
}
