// Package urlx is a tiny, dependency-free helper for host-scoped URL
// comparison. It is the single owner of "are these two URLs the same host?",
// used by discovery (same-host link scope), the fetcher (credential-strip on
// cross-host redirect), and the frontier. Keeping it in one leaf package means
// the normalization rules (lowercasing, default-port stripping, userinfo,
// IPv6) are defined once and tested once.
package urlx

import (
	"errors"
	"net/url"
	"strings"
)

// ErrNoHost is the sentinel wrapped by Normalize when the input carries no host
// (a bare relative path, or a "scheme:opaque" form). Test for it with
// errors.Is(err, urlx.ErrNoHost); Normalize returns it inside a *url.Error.
var ErrNoHost = errors.New("urlx: URL has no host")

// Normalize applies the safe, syntax-based normalizations of RFC 3986 §6.2.2
// and §6.2.3 to rawURL, producing a canonical form suitable as a crawl/link
// identity key. The transformations are exactly the ones that never change
// which resource a URL identifies:
//
//   - §6.2.2.1 case: lowercase the scheme and host (both case-insensitive).
//     The path, query, and userinfo keep their case — they are case-sensitive.
//   - §6.2.2.1 percent-encoding: uppercase the hex digits of every %-escape in
//     the path and query (%2f -> %2F).
//   - §6.2.2.2 percent-encoding: decode escapes of unreserved octets
//     (ALPHA / DIGIT / "-" / "_" / "~") in the path and query (%7E -> ~).
//     Reserved and other octets stay encoded. The dot ("." / %2E) is the one
//     unreserved octet left ENCODED, so an encoded segment ("%2E" / "%2E%2E")
//     can never be turned into a "." / ".." dot-segment — matching net/url's
//     own ResolveReference and keeping the result idempotent.
//   - §6.2.2.3 path segments: remove "." and ".." dot-segments (/a/../b -> /b).
//     This operates on the *escaped* path, so a percent-encoded slash (%2F) is
//     opaque and never treated as a segment separator.
//   - §6.2.3 default port: drop :80 for http and :443 for https.
//   - the fragment is DROPPED — it is not sent to the origin and never
//     identifies a distinct crawlable resource.
//
// Query parameters are treated as identity-significant: their order, presence,
// and duplicates are preserved verbatim (only their %-escapes are
// case/unreserved-normalized). Userinfo is preserved — Normalize is the
// identity key, not a credential-stripper; SameHost owns credential equality.
//
// Normalize is idempotent: Normalize(Normalize(x)) == Normalize(x).
//
// Normalize requires an absolute or scheme-relative URL — one that carries a
// host (e.g. "https://example.com/x" or "//example.com/x"). It returns an error
// for an unparseable input OR a host-less reference (a bare relative path like
// "/a/b" or "a/b", or a "scheme:opaque" form with no authority). This matches
// the package's role as a crawl/link-identity key: links are resolved against
// their base to an absolute URL before they reach Normalize, and a host-less
// relative path has no stable, unambiguous canonical form anyway (RFC 3986 §4.2
// forbids a relative reference whose path begins with "//", which dot-segment
// removal can otherwise produce). Use Host/SameHost for host-only questions.
func Normalize(rawURL string) (string, error) {
	u, err := url.Parse(rawURL)
	if err != nil {
		return "", err
	}
	// Require a real host. u.Hostname() (not u.Host) is the gate: an authority
	// like ":80" or "//:80" parses to a non-empty u.Host but an empty hostname,
	// and stripping its default port would otherwise collapse it to a degenerate
	// "scheme:" form that no longer round-trips.
	if u.Hostname() == "" {
		return "", &url.Error{Op: "Normalize", URL: rawURL, Err: ErrNoHost}
	}

	// §6.2.2.1 — scheme is case-insensitive. net/url already lowercases known
	// schemes on Parse; normalize explicitly so the contract does not depend on
	// that incidental behavior.
	u.Scheme = strings.ToLower(u.Scheme)

	// §6.2.2.1 — host is case-insensitive. u.Host is the "host[:port]" authority
	// component; userinfo lives in u.User and is left untouched. Host and port
	// contain only case-insensitive/numeric characters, so lowercasing the whole
	// authority is safe.
	u.Host = strings.ToLower(u.Host)

	// §6.2.3 — drop the scheme's default port.
	u.Host = stripDefaultPort(u.Scheme, u.Host)

	// §6.2.2.3 then §6.2.2.1/§6.2.2.2 — remove dot-segments on the *escaped* path
	// FIRST, then normalize %-escapes. Operating on the escaped string keeps an
	// encoded "%2F" opaque (never a separator) AND an encoded "%2E" opaque (never
	// a "." or ".." dot-segment), matching net/url's own §5.2.4 ResolveReference,
	// which collapses literal "/a/../b" but leaves "/a/%2E%2E/b" intact. Because
	// the dot is kept encoded by normalizePercentEncoding, this stays idempotent.
	escPath := normalizePercentEncoding(removeDotSegments(u.EscapedPath()))
	if err := setEscapedPath(u, escPath); err != nil {
		return "", err
	}

	// §6.2.2.1/§6.2.2.2 — normalize %-escapes in the query. Order/presence are
	// identity-significant, so the structure is preserved verbatim; net/url emits
	// RawQuery byte-for-byte, so this is already a fixed point.
	u.RawQuery = normalizePercentEncoding(u.RawQuery)

	// Drop the fragment entirely (both the decoded and raw hint).
	u.Fragment = ""
	u.RawFragment = ""

	// Re-parse the rendered string once so net/url's own escaping rules settle the
	// output to a Parse fixed point. net/url's String()/EscapedPath() is slightly
	// more permissive than Parse() for a few escapes (notably in a *rootless*
	// relative path, where "%2A" is re-canonicalized to "*"), so a value produced
	// only by String() can fail to be a fixed point of the Parse-then-render cycle
	// that Normalize itself is. Delegating the final escaping decision to net/url
	// (rather than hardcoding its per-version literal table) makes Normalize a true
	// fixed point. The reparse cannot itself fail — the input is a string net/url
	// just produced.
	//
	// Guard: dot-segment removal can leave a relative (hostless) path beginning
	// with "//", which url.Parse would re-interpret as a network-path reference
	// (RFC 3986 §4.2) — promoting the leading segment to a host and changing the
	// URL's structure. Only accept the reparse when it preserved scheme+host;
	// otherwise the structured form built above is the canonical answer.
	out := u.String()
	if reparsed, err := url.Parse(out); err == nil &&
		reparsed.Scheme == u.Scheme && reparsed.Host == u.Host {
		return reparsed.String(), nil
	}
	return out, nil
}

// stripDefaultPort removes the scheme's default port from a "host[:port]"
// authority: :80 for http, :443 for https. Any other port (or scheme) is kept.
// It is IPv6-safe: a bracketed literal's inner colons are not a port.
func stripDefaultPort(scheme, host string) string {
	var suffix string
	switch scheme {
	case "http":
		suffix = ":80"
	case "https":
		suffix = ":443"
	default:
		return host
	}
	// The port is whatever follows the last colon that is outside any brackets.
	// For "[::1]:80" the bracketed segment is skipped; for "h:80" it is the whole
	// host. strings.HasSuffix on the ":port" token is correct because the only
	// colon that can introduce a port is the final, unbracketed one.
	if !strings.HasSuffix(host, suffix) {
		return host
	}
	// Guard against a bare IPv6 literal ending in the same digits, e.g. an
	// (unbracketed, malformed) host — but Host here always arrives bracketed for
	// IPv6, so a trailing ":80" outside brackets is genuinely the port.
	stripped := host[:len(host)-len(suffix)]
	if strings.Contains(stripped, "]") || !strings.Contains(stripped, ":") {
		return stripped
	}
	// stripped still holds a ":" and no "]" — that is a bare-IPv6-ish authority
	// where the trailing token was part of the address, not a port. Keep it.
	return host
}

// normalizePercentEncoding applies RFC 3986 §6.2.2.1/§6.2.2.2 to a path or
// query string: every "%XX" escape has its hex uppercased, and an escape of an
// unreserved octet is decoded to the literal character. Escapes of reserved or
// other octets keep their encoding (with uppercased hex); non-"%" bytes are
// copied verbatim.
//
// A "%" that does not begin a well-formed two-hex-digit escape is itself an
// illegal URI character; it is re-encoded as "%25" rather than emitted bare.
// This is what keeps the function idempotent: passing a bare "%" through could
// let a later byte close it into a *new* escape (e.g. "%2%44" -> "%2"+"D" would
// read as "%2D" on the next pass). net/url already rejects such malformed
// escapes in a path at Parse time, so in practice this only guards the query
// (where net/url is lenient), but applying it uniformly is correct everywhere.
func normalizePercentEncoding(s string) string {
	// Fast path: nothing to do without a "%".
	if !strings.Contains(s, "%") {
		return s
	}
	var b strings.Builder
	b.Grow(len(s))
	for i := 0; i < len(s); i++ {
		c := s[i]
		if c != '%' {
			b.WriteByte(c)
			continue
		}
		hi, hiOK := hexNibble(s, i+1)
		lo, loOK := hexNibble(s, i+2)
		if !hiOK || !loOK {
			// Malformed escape: encode the literal "%" so it can never combine
			// with following bytes into a spurious escape on a later pass.
			b.WriteString("%25")
			continue
		}
		octet := hi<<4 | lo
		if decodeUnreserved(octet) {
			b.WriteByte(octet)
		} else {
			// Keep encoded, but with uppercase hex digits.
			b.WriteByte('%')
			b.WriteByte(toUpperHex(hi))
			b.WriteByte(toUpperHex(lo))
		}
		i += 2
	}
	return b.String()
}

// hexNibble returns the value of the hex digit at s[idx], or ok=false if idx is
// out of range or the byte is not a hex digit.
func hexNibble(s string, idx int) (byte, bool) {
	if idx >= len(s) {
		return 0, false
	}
	return fromHex(s[idx])
}

// removeDotSegments implements RFC 3986 §5.2.4 on an *escaped* path string,
// where the only segment separator is a literal "/". Encoded separators (%2F)
// are opaque and never split a segment.
func removeDotSegments(path string) string {
	// Fast path: a "." or ".." complete segment requires a literal dot somewhere.
	// Without one there is nothing to remove. (This also short-circuits the
	// common case of a "%2E"-only path, since the encoded form has no literal ".")
	if !strings.Contains(path, ".") {
		return path
	}
	in := path
	var out strings.Builder
	out.Grow(len(path))
	for in != "" {
		switch {
		case strings.HasPrefix(in, "../"):
			in = in[3:]
		case strings.HasPrefix(in, "./"):
			in = in[2:]
		case strings.HasPrefix(in, "/./"):
			in = "/" + in[3:]
		case in == "/.":
			in = "/"
		case strings.HasPrefix(in, "/../"):
			in = "/" + in[4:]
			removeLastSegment(&out)
		case in == "/..":
			in = "/"
			removeLastSegment(&out)
		case in == ".", in == "..":
			in = ""
		default:
			// Move the leading "/" (if any) plus the next segment up to but not
			// including the next "/" to the output buffer.
			start := 0
			if in[0] == '/' {
				start = 1
			}
			next := strings.IndexByte(in[start:], '/')
			var seg string
			if next < 0 {
				seg = in
				in = ""
			} else {
				seg = in[:start+next]
				in = in[start+next:]
			}
			out.WriteString(seg)
		}
	}
	return out.String()
}

// removeLastSegment trims the output buffer back to (but not including) the
// last "/", per the §5.2.4 "/../" and "/.." rules. strings.Builder cannot be
// truncated, so rebuild it from the trimmed string.
func removeLastSegment(out *strings.Builder) {
	s := out.String()
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		s = s[:i]
	} else {
		s = ""
	}
	out.Reset()
	out.WriteString(s)
}

// setEscapedPath stores escaped as u's path such that u.String() emits it
// verbatim: u.Path holds the decoded form and u.RawPath holds the escaped hint.
// net/url echoes RawPath only when it is a valid encoding of Path, which holds
// here because escaped is exactly an encoding of its own unescape.
func setEscapedPath(u *url.URL, escaped string) error {
	dec, err := url.PathUnescape(escaped)
	if err != nil {
		return err
	}
	u.Path = dec
	u.RawPath = escaped
	return nil
}

func isUnreserved(b byte) bool {
	switch {
	case b >= 'A' && b <= 'Z', b >= 'a' && b <= 'z', b >= '0' && b <= '9':
		return true
	case b == '-' || b == '.' || b == '_' || b == '~':
		return true
	default:
		return false
	}
}

// decodeUnreserved reports whether a %-escaped octet should be DECODED to its
// literal during normalization. It is the unreserved set MINUS "." (0x2E): the
// dot is left encoded so that an encoded segment like "%2E"/"%2E%2E" can never
// be turned into a "." / ".." dot-segment after dot-segment removal has already
// run. This mirrors net/url's ResolveReference, which leaves "%2E" encoded, and
// is what keeps Normalize idempotent.
func decodeUnreserved(b byte) bool {
	return b != '.' && isUnreserved(b)
}

func fromHex(c byte) (byte, bool) {
	switch {
	case c >= '0' && c <= '9':
		return c - '0', true
	case c >= 'a' && c <= 'f':
		return c - 'a' + 10, true
	case c >= 'A' && c <= 'F':
		return c - 'A' + 10, true
	default:
		return 0, false
	}
}

func toUpperHex(nibble byte) byte {
	const hex = "0123456789ABCDEF"
	return hex[nibble&0xF]
}

// Host returns the host of rawURL, normalized for comparison:
//
//   - lowercased (hosts are case-insensitive),
//   - userinfo (user:pass@) stripped,
//   - the scheme's default port stripped — :80 for http, :443 for https — so
//     that "http://h/" and "http://h:80/" compare equal; any non-default port
//     is kept (e.g. ":8080"). The default-port rule only applies when the URL
//     carries a scheme: a scheme-relative ("//h:80/") or scheme-less input has
//     no scheme to judge "default" against, so its port is kept verbatim.
//
// It is scheme-agnostic: "//host/x" (scheme-relative) and a bare "host" or
// "host/x" (no scheme) both resolve to their host. IPv6 literals are returned
// without brackets when there is no port ("http://[::1]/" -> "::1"), and
// re-bracketed when a port is kept ("http://[::1]:8080/" -> "[::1]:8080") so
// the result stays unambiguous and re-parseable.
//
// Limitation: a scheme-less host that also carries a port ("host:8080/x") is
// indistinguishable from a "scheme:opaque" URL to net/url, so it yields "".
// In practice every URL this package sees is either absolute (sitemap <loc>,
// link-resolved against a base) or scheme-relative, so the ambiguous shape
// does not arise; prefer "//host:8080/x" if you must express it.
//
// Limitation: net/url decodes one level of %-escapes in the authority on Parse
// ("//ex%2541mple.com" -> host "ex%41mple.com"). A decoded host that still
// carries a literal "%" is not a valid host label and is not re-parseable, so
// Host yields "" rather than emitting a value that fails its own round-trip.
//
// Fallback: Host returns "" when no host can be parsed from rawURL (empty
// string, a relative path with no authority, or a parse error).
func Host(rawURL string) string {
	u, err := url.Parse(rawURL)
	if err != nil {
		return ""
	}
	// A bare "host/path" has no "//" authority, so net/url files the whole
	// thing under Path and leaves Host empty. Retry as a scheme-relative URL
	// to recover the authority. Only do this for genuinely scheme-less input:
	// an explicit scheme with no authority (e.g. "mailto:a@b.com", which parses
	// as opaque) has no host by design and must fall through to "".
	if u.Host == "" {
		if u.Scheme != "" || u.Opaque != "" || rawURL == "" || strings.HasPrefix(rawURL, "/") {
			return ""
		}
		u, err = url.Parse("//" + rawURL)
		if err != nil || u.Host == "" {
			return ""
		}
	}

	host := strings.ToLower(u.Hostname()) // strips brackets + port, keeps the bare host
	if host == "" {
		return ""
	}
	// net/url decodes one level of %-escapes in the authority on Parse, so an
	// input like "//ex%2541mple.com" yields the host "ex%41mple.com". A literal
	// "%" left in the decoded host is not a valid DNS/IP label and is not
	// re-parseable (net/url rejects "%41" as a host escape), so Host's output
	// would not round-trip. No real host carries a "%"; treat it as unparseable
	// to keep Host's output escape-stable and re-referenceable.
	if strings.Contains(host, "%") {
		return ""
	}
	// A legitimate IPv6 literal always arrives bracketed (u.Host contains "["),
	// so u.Hostname() only legitimately contains ":" for a bracketed source.
	// An unbracketed authority whose hostname still holds a ":" (e.g. "//::")
	// is net/url mis-parsing a malformed authority into a stray-colon host that
	// is neither a valid label nor re-referenceable; treat it as unparseable.
	if strings.Contains(host, ":") && !strings.Contains(u.Host, "[") {
		return ""
	}

	port := u.Port()
	if port == "" {
		return host
	}
	// Drop the scheme's default port; keep anything else. Without a scheme we
	// cannot know the default, so we keep the port verbatim.
	switch strings.ToLower(u.Scheme) {
	case "http":
		if port == "80" {
			return host
		}
	case "https":
		if port == "443" {
			return host
		}
	}

	// A kept port on an IPv6 literal needs brackets to stay unambiguous.
	if strings.Contains(host, ":") {
		return "[" + host + "]:" + port
	}
	return host + ":" + port
}

// SameHost reports whether a and b target the same host, applying Host's
// normalization (case, userinfo, default port, IPv6) to both. Two inputs whose
// hosts are both unparseable ("" == "") are NOT considered the same host.
func SameHost(a, b string) bool {
	ha, hb := Host(a), Host(b)
	if ha == "" || hb == "" {
		return false
	}
	return strings.EqualFold(ha, hb)
}

// SameSite reports whether a and b belong to the same crawl *site*. It is a
// superset of SameHost's exact-host rule with ONE extra equivalence: an apex
// host and its "www." sibling are the same site (example.com == www.example.com).
//
// It is deliberately NOT eTLD+1 collapsing: an unrelated subdomain stays a
// distinct site (blog.example.com != example.com, sub.example.com != example.com).
// Only the single leading "www." label is folded, and only when a registrable
// host (one carrying at least one further dot) remains — so "www.com" is left
// intact and is not merged with the bare TLD "com".
//
// As with SameHost, two inputs whose hosts are both unparseable are NOT the
// same site.
func SameSite(a, b string) bool {
	ha, hb := Host(a), Host(b)
	if ha == "" || hb == "" {
		return false
	}
	return strings.EqualFold(stripWWW(ha), stripWWW(hb))
}

// stripWWW removes a single leading "www." label from a normalized host, but
// only when at least one more dot remains afterward (so a registrable host is
// left, e.g. "www.example.com" -> "example.com"). A bare "www.com" keeps its
// label so it is never folded into the TLD "com". The host arrives lowercased
// from Host, so a byte-prefix check is sufficient.
func stripWWW(host string) string {
	const p = "www."
	if strings.HasPrefix(host, p) {
		if rest := host[len(p):]; strings.Contains(rest, ".") {
			return rest
		}
	}
	return host
}
