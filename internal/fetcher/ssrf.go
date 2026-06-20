package fetcher

import (
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"net/url"
	"strings"
	"syscall"
	"time"
)

// errDisallowedDestination is returned when a dial or redirect targets an
// internal/loopback/link-local/metadata address that the SSRF guard rejects.
var errDisallowedDestination = errors.New("fetcher: destination address is not allowed (SSRF guard)")

// errBadProxyURL is returned when a non-empty proxy_url cannot be parsed. The
// raw value (which may embed credentials) is never echoed in the message.
var errBadProxyURL = errors.New("fetcher: proxy_url is set but could not be parsed")

// errTooManyRedirects is returned when the redirect chain exhausts maxRedirects
// (a redirect loop or an over-long chain), so the fetch is reported unreachable
// rather than as a successful terminal 3xx.
var errTooManyRedirects = errors.New("fetcher: too many redirects (chain cap exhausted)")

// metadataIPv4 is the cloud instance-metadata endpoint (link-local already
// covers it, but it is checked explicitly for defense in depth / clarity).
var metadataIPv4 = net.IPv4(169, 254, 169, 254)

// deniedPrefixes are reachable IP ranges that the stdlib net.IP predicates
// (IsPrivate/IsLinkLocal*/IsUnspecified/…) do NOT classify but that an SSRF
// attempt can still use to reach a metadata service or an internal host:
//
//   - 0.0.0.0/8        "this network" (RFC 1122 §3.2.1.3); IsUnspecified only
//     matches the single 0.0.0.0, so the rest of the block is covered here.
//   - 100.64.0.0/10    CGNAT / shared address space (RFC 6598). Alibaba Cloud's
//     metadata endpoint lives at 100.100.100.200.
//   - 192.0.0.0/24     IETF protocol assignments (RFC 6890), incl. the NAT64
//     well-known-prefix discovery address 192.0.0.171.
//   - 198.18.0.0/15    benchmarking (RFC 2544).
//   - 64:ff9b::/96     NAT64 well-known prefix (RFC 6052): on an IPv6-only host
//     64:ff9b::a9fe:a9fe translates to the IPv4 metadata IP 169.254.169.254.
//
// These complement (do not replace) the IsLoopback/IsPrivate/… predicates in
// ipDisallowed; each address is tested against this list via Prefix.Contains.
var deniedPrefixes = []netip.Prefix{
	netip.MustParsePrefix("0.0.0.0/8"),
	netip.MustParsePrefix("100.64.0.0/10"),
	netip.MustParsePrefix("192.0.0.0/24"),
	netip.MustParsePrefix("198.18.0.0/15"),
	netip.MustParsePrefix("64:ff9b::/96"),
}

// ipDisallowed reports whether dialing ip would reach a loopback, private,
// link-local, unspecified, multicast, the cloud metadata address, or one of the
// extra reachable ranges in deniedPrefixes (CGNAT/NAT64/benchmarking/etc.).
// These are the destinations an SSRF attempt against a cloud host would target.
func ipDisallowed(ip net.IP) bool {
	if ip == nil {
		return true
	}
	if ip.Equal(metadataIPv4) {
		return true
	}
	if ip.IsLoopback() || ip.IsPrivate() || ip.IsUnspecified() ||
		ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() || ip.IsMulticast() {
		return true
	}
	// Check the explicit deny-list. netip.AddrFromSlice expects a 4- or 16-byte
	// slice; normalize to 4 bytes for IPv4 (ip is often a 16-byte v4-in-v6) so an
	// IPv4 address is matched by the IPv4 prefixes via Contains, and unmap so a
	// v4-mapped IPv6 literal compares against IPv4 prefixes too.
	if addr, ok := netip.AddrFromSlice(ip); ok {
		addr = addr.Unmap()
		for _, p := range deniedPrefixes {
			if p.Contains(addr) {
				return true
			}
		}
	}
	return false
}

// dialControl is the net.Dialer.Control hook. It runs after DNS resolution with
// the concrete IP:port the OS is about to connect to, so it catches both the
// initial dial and every redirect dial, and defeats DNS-rebinding tricks.
func dialControl(_, address string, _ syscall.RawConn) error {
	host, _, err := net.SplitHostPort(address)
	if err != nil {
		host = address
	}
	ip := net.ParseIP(host)
	if ip == nil {
		// Control always receives a resolved IP literal; refuse anything else.
		return errDisallowedDestination
	}
	if ipDisallowed(ip) {
		return errDisallowedDestination
	}
	return nil
}

// GuardedClient returns an *http.Client whose dialer installs the same SSRF
// Control hook the page fetcher uses, so callers outside the fetcher (e.g. the
// robots.txt cache) enforce the identical deny-list. timeout bounds the whole
// request; a zero timeout means no client-level timeout (callers should pass a
// non-zero value).
func GuardedClient(timeout time.Duration) *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second, Control: dialControl}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:     dialer.DialContext,
			MaxIdleConns:    100,
			IdleConnTimeout: 90 * time.Second,
			// Bound TLS handshake and header read so a slow-header origin cannot pin
			// the caller for the full client.Timeout (matches the page fetcher and the
			// robots client's explicit 10s header-timeout protection).
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
		},
	}
}

// GuardedNoRedirectClient returns an *http.Client that (a) installs the same
// post-DNS SSRF Control hook the page fetcher uses (unless allowPrivate, which
// the test suite sets to hit loopback httptest servers) and (b) NEVER follows a
// redirect: CheckRedirect returns http.ErrUseLastResponse so the caller always
// sees the first response. This is the client the proof-of-control verifiers
// need: a 30x to an attacker-controlled host (off-host OR same-host) must never
// satisfy a proof, so the token must sit at the EXACT path on the literal host,
// returning 200. timeout bounds the whole request; handshake/header timeouts
// bound a slow origin.
func GuardedNoRedirectClient(timeout time.Duration, allowPrivate bool) *http.Client {
	dialer := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
	if !allowPrivate {
		dialer.Control = dialControl
	}
	return &http.Client{
		Timeout: timeout,
		Transport: &http.Transport{
			DialContext:           dialer.DialContext,
			MaxIdleConns:          100,
			IdleConnTimeout:       90 * time.Second,
			TLSHandshakeTimeout:   10 * time.Second,
			ResponseHeaderTimeout: 10 * time.Second,
		},
		// Refuse every redirect. ErrUseLastResponse makes Do() return the 3xx
		// itself with no error, so the verifier sees a non-200 and reports the
		// proof unsatisfied rather than chasing the redirect to an attacker host.
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

// ValidateSiteURL reports an error if rawURL is not a safe outbound target: the
// scheme must be http or https and a host must be present. Unless allowPrivate
// is set, an IP-literal host must also not fall in a disallowed (loopback/
// private/link-local/metadata) range; name-based hosts are validated at dial
// time by the Control hook (defeating DNS rebinding). allowPrivate mirrors the
// fetcher's AllowPrivate option so test wiring that targets loopback httptest
// servers admits those sites while production rejects internal ranges. This
// catches obvious misconfigurations and non-http schemes at the point a site is
// admitted (the scheme check applies even under allowPrivate).
func ValidateSiteURL(rawURL string, allowPrivate bool) error {
	u, err := url.Parse(rawURL)
	if err != nil {
		return fmt.Errorf("fetcher: site url %q is not parseable: %w", rawURL, err)
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return fmt.Errorf("fetcher: site url %q scheme must be http or https", rawURL)
	}
	host := u.Hostname()
	if host == "" {
		return fmt.Errorf("fetcher: site url %q has no host", rawURL)
	}
	if !allowPrivate {
		if ip := net.ParseIP(host); ip != nil && ipDisallowed(ip) {
			return errDisallowedDestination
		}
	}
	return nil
}

// redirectAllowed reports whether a redirect target URL is permitted: the scheme
// must be http/https and the host must not resolve to a disallowed range. It is
// used inside CheckRedirect so an attacker-controlled 302 cannot send the client
// into an internal address even though the Dialer.Control hook also guards dials.
func redirectAllowed(u *url.URL) bool {
	if u == nil {
		return false
	}
	switch strings.ToLower(u.Scheme) {
	case "http", "https":
	default:
		return false
	}
	host := u.Hostname()
	// If the host is an IP literal, validate it directly. If it is a name, the
	// Dialer.Control hook validates the resolved IP at dial time; here we only
	// pre-empt obvious literal targets to abort the chain early.
	if ip := net.ParseIP(host); ip != nil && ipDisallowed(ip) {
		return false
	}
	return true
}
