package fetcher

import (
	"net/http"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

func TestClassify(t *testing.T) {
	tests := []struct {
		name         string
		status       int
		header       http.Header
		body         []byte
		wantClass    model.FetchClass
		wantDetector string
	}{
		{
			name:      "plain 200 ok",
			status:    200,
			header:    http.Header{},
			body:      []byte("<html><title>hi</title></html>"),
			wantClass: model.FetchOK,
		},
		{
			name:      "429 soft block",
			status:    429,
			header:    http.Header{},
			body:      nil,
			wantClass: model.FetchSoftBlock,
		},
		{
			name:      "503 soft block",
			status:    503,
			header:    http.Header{},
			body:      nil,
			wantClass: model.FetchSoftBlock,
		},
		{
			name:      "403 hard block",
			status:    403,
			header:    http.Header{},
			body:      nil,
			wantClass: model.FetchHardBlock,
		},
		{
			name:         "cloudflare cf-mitigated header on 200",
			status:       200,
			header:       http.Header{"Cf-Mitigated": []string{"challenge"}},
			body:         []byte("<html></html>"),
			wantClass:    model.FetchHardBlock,
			wantDetector: "cloudflare",
		},
		{
			name:         "cloudflare attention required body",
			status:       403,
			header:       http.Header{"Server": []string{"cloudflare"}},
			body:         []byte("<html><title>Attention Required! | Cloudflare</title></html>"),
			wantClass:    model.FetchHardBlock,
			wantDetector: "cloudflare",
		},
		{
			name:         "cloudflare cf-chl marker in body",
			status:       200,
			header:       http.Header{},
			body:         []byte(`<div class="cf-chl-widget"></div>`),
			wantClass:    model.FetchHardBlock,
			wantDetector: "cloudflare",
		},
		{
			name:         "datadome cookie header",
			status:       200,
			header:       http.Header{"Set-Cookie": []string{"datadome=abc; Path=/"}},
			body:         []byte("<html></html>"),
			wantClass:    model.FetchHardBlock,
			wantDetector: "datadome",
		},
		{
			name:         "perimeterx body marker",
			status:       200,
			header:       http.Header{},
			body:         []byte(`<html>_pxAppId blocked</html>`),
			wantClass:    model.FetchHardBlock,
			wantDetector: "perimeterx",
		},
		{
			name:         "akamai reference error body",
			status:       403,
			header:       http.Header{"Server": []string{"AkamaiGHost"}},
			body:         []byte("Access Denied Reference #18.abcd"),
			wantClass:    model.FetchHardBlock,
			wantDetector: "akamai",
		},
		{
			name:         "hcaptcha body marker on 200",
			status:       200,
			header:       http.Header{},
			body:         []byte(`<div class="h-captcha" data-sitekey="x"></div>`),
			wantClass:    model.FetchHardBlock,
			wantDetector: "hcaptcha",
		},
		{
			name:         "recaptcha body marker on 200",
			status:       200,
			header:       http.Header{},
			body:         []byte(`<script src="https://www.google.com/recaptcha/api.js"></script>`),
			wantClass:    model.FetchHardBlock,
			wantDetector: "recaptcha",
		},
		{
			name:      "404 not a block",
			status:    404,
			header:    http.Header{},
			body:      []byte("<html><title>Not Found</title></html>"),
			wantClass: model.FetchOK,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotClass, gotDetector := Classify(tc.status, tc.header, tc.body)
			if gotClass != tc.wantClass {
				t.Errorf("Classify() class = %q, want %q", gotClass, tc.wantClass)
			}
			if gotDetector != tc.wantDetector {
				t.Errorf("Classify() detector = %q, want %q", gotDetector, tc.wantDetector)
			}
		})
	}
}
