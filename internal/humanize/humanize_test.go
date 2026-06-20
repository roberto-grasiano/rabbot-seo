package humanize

import (
	"testing"
	"time"
)

func TestDuration(t *testing.T) {
	tests := []struct {
		name string
		in   time.Duration
		want string
	}{
		{"zero", 0, "0s"},
		{"negative", -5 * time.Second, "0s"},
		{"sub-second", 250 * time.Millisecond, "<1s"},
		{"seconds", 42 * time.Second, "42s"},
		{"minutes-seconds", 5*time.Minute + 33*time.Second, "5m 33s"},
		{"hours-minutes", 5*time.Hour + 33*time.Minute + 20*time.Second, "5h 33m"},
		{"exact-hour", time.Hour, "1h 0m"},
		{"exact-minute", time.Minute, "1m 0s"},
		{"one-second", time.Second, "1s"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Duration(tt.in); got != tt.want {
				t.Errorf("Duration(%v) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}

func TestDisplayHost(t *testing.T) {
	tests := []struct {
		name string
		in   string
		want string
	}{
		{"https with path", "https://example.com/foo/bar", "example.com"},
		{"http with port", "http://example.com:8443/", "example.com:8443"},
		{"bare host fallback", "example.com", "example.com"},
		{"no scheme path-only fallback", "/just/a/path", "/just/a/path"},
		{"https root", "https://sub.example.org", "sub.example.org"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := DisplayHost(tt.in); got != tt.want {
				t.Errorf("DisplayHost(%q) = %q, want %q", tt.in, got, tt.want)
			}
		})
	}
}
