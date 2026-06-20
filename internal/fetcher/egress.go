package fetcher

import (
	"context"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// EgressInfo is produced by the doctor/status egress-IP lookup.
type EgressInfo struct {
	IPs       []string
	Dynamic   bool
	Endpoint  string
	CheckedAt time.Time
}

// EgressIP performs a one-shot outbound-IP lookup against a configurable echo
// endpoint. Unless allowPrivate is set, it installs the same post-DNS SSRF dial
// guard the crawl/robots clients use, so a misconfigured egress_check_endpoint —
// e.g. pointed at 169.254.169.254 or an internal host — cannot reach
// loopback/private/link-local/metadata addresses. allowPrivate is for tests that
// hit an httptest (loopback) server; production passes false.
func EgressIP(ctx context.Context, endpoint string, proxyURL string, allowPrivate bool) (EgressInfo, error) {
	info := EgressInfo{Endpoint: endpoint, CheckedAt: time.Now()}

	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	if !allowPrivate {
		dialer.Control = dialControl
	}
	tr := &http.Transport{DialContext: dialer.DialContext, TLSHandshakeTimeout: 10 * time.Second}
	if proxyURL != "" {
		// Fail closed on a malformed proxy_url, mirroring newClient: a typo must not
		// silently bypass the proxy and reveal the daemon's real outbound IP.
		// errBadProxyURL never echoes the raw value, so no credential leak.
		pu, perr := url.Parse(proxyURL)
		if perr != nil {
			return info, errBadProxyURL
		}
		tr.Proxy = http.ProxyURL(pu)
	}
	client := &http.Client{Transport: tr, Timeout: 10 * time.Second}

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return info, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return info, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return info, fmt.Errorf("egress lookup: status %d", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if err != nil {
		return info, err
	}

	raw := strings.TrimSpace(string(body))
	for _, candidate := range strings.FieldsFunc(raw, func(r rune) bool {
		return r == ',' || r == '\n' || r == ' '
	}) {
		candidate = strings.TrimSpace(candidate)
		if ip := net.ParseIP(candidate); ip != nil {
			info.IPs = append(info.IPs, candidate)
		}
	}
	if len(info.IPs) == 0 {
		return info, fmt.Errorf("egress lookup: no IP found in %q", raw)
	}
	return info, nil
}
