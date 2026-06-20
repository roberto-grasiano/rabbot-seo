package fetcher

import (
	"net/http"
	"strings"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// Classify inspects status, headers, and body for WAF/challenge markers and
// returns the FetchClass plus the matched detector name ("" if none).
// Markers: Cloudflare (cf-mitigated, cf-chl-*, "Attention Required"), Akamai,
// DataDome, PerimeterX, hCaptcha/reCAPTCHA.
func Classify(status int, header http.Header, body []byte) (model.FetchClass, string) {
	// soft_block: explicit upstream throttling.
	if status == http.StatusTooManyRequests || status == http.StatusServiceUnavailable {
		return model.FetchSoftBlock, ""
	}

	// Detect challenge/WAF vendors from headers + body regardless of status.
	if detector := detectVendor(header, body); detector != "" {
		return model.FetchHardBlock, detector
	}

	// 403 with no vendor signature is still a hard block (access denied).
	if status == http.StatusForbidden {
		return model.FetchHardBlock, ""
	}

	return model.FetchOK, ""
}

func detectVendor(header http.Header, body []byte) string {
	lowerBody := strings.ToLower(string(body))
	setCookie := strings.ToLower(strings.Join(header.Values("Set-Cookie"), " "))
	server := strings.ToLower(header.Get("Server"))

	// Cloudflare: cf-mitigated header, cf-chl* markers, "Attention Required".
	if header.Get("Cf-Mitigated") != "" ||
		strings.Contains(lowerBody, "cf-chl") ||
		strings.Contains(lowerBody, "attention required") ||
		(strings.Contains(server, "cloudflare") && strings.Contains(lowerBody, "cloudflare") &&
			(strings.Contains(lowerBody, "ray id") || strings.Contains(lowerBody, "attention required"))) {
		return "cloudflare"
	}

	// DataDome: datadome cookie or body marker.
	if strings.Contains(setCookie, "datadome") || strings.Contains(lowerBody, "datadome") {
		return "datadome"
	}

	// PerimeterX / HUMAN: _px markers.
	if strings.Contains(lowerBody, "_pxappid") || strings.Contains(lowerBody, "px-captcha") ||
		strings.Contains(setCookie, "_px") {
		return "perimeterx"
	}

	// Akamai: server header + access-denied reference.
	if strings.Contains(server, "akamai") &&
		(strings.Contains(lowerBody, "access denied") || strings.Contains(lowerBody, "reference #")) {
		return "akamai"
	}

	// hCaptcha.
	if strings.Contains(lowerBody, "h-captcha") || strings.Contains(lowerBody, "hcaptcha.com") {
		return "hcaptcha"
	}

	// reCAPTCHA.
	if strings.Contains(lowerBody, "g-recaptcha") || strings.Contains(lowerBody, "recaptcha/api.js") {
		return "recaptcha"
	}

	return ""
}
