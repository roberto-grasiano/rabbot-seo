package fetcher

import (
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

func TestStatusTypeFor(t *testing.T) {
	tests := []struct {
		name       string
		status     int
		redirected bool
		want       model.StatusType
	}{
		{"200 page", 200, false, model.StatusPage},
		{"301 redirect via flag", 200, true, model.StatusRedirect},
		{"301 raw redirect", 301, false, model.StatusRedirect},
		{"302 raw redirect", 302, false, model.StatusRedirect},
		{"404 missing", 404, false, model.StatusMissing},
		{"410 missing", 410, false, model.StatusMissing},
		{"500 server error", 500, false, model.StatusServerError},
		{"503 server error", 503, false, model.StatusServerError},
		{"zero status unreachable", 0, false, model.StatusUnreachable},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := StatusTypeFor(tc.status, tc.redirected)
			if got != tc.want {
				t.Errorf("StatusTypeFor(%d,%v) = %q, want %q", tc.status, tc.redirected, got, tc.want)
			}
		})
	}
}
