// Package humanize holds tiny display-formatting helpers shared by leaf packages
// (cli, wizard) that must agree byte-for-byte on operator-facing strings. It is a
// LEAF — it imports only the standard library — so wizard can depend on it without
// pulling in cli (which would be an import cycle).
package humanize

import (
	"fmt"
	"net/url"
	"time"
)

// Duration renders a Duration as a compact "Xh Ym" / "Ym Zs" / "Zs" string for
// operator-facing coverage/cap lines. time.Duration.String() (e.g. "5h33m20s") is
// fine but noisy; this trims to the two most significant units. A non-positive
// duration renders "0s"; a sub-second positive duration renders "<1s" (a tiny site
// at the fastest verified rate yields a sub-second full pass that integer-second
// math would otherwise truncate to "0s").
func Duration(d time.Duration) string {
	if d <= 0 {
		return "0s"
	}
	h := int(d / time.Hour)
	m := int((d % time.Hour) / time.Minute)
	s := int((d % time.Minute) / time.Second)
	switch {
	case h > 0:
		return fmt.Sprintf("%dh %dm", h, m)
	case m > 0:
		return fmt.Sprintf("%dm %ds", m, s)
	case d < time.Second:
		return "<1s"
	default:
		return fmt.Sprintf("%ds", s)
	}
}

// DisplayHost extracts the host[:port] from a site base URL for DISPLAY (proof
// placement hints, coverage UA host). On a parse failure — or a URL with no host
// component — it returns the raw value (callers have already gated the input with
// fetcher.ValidateSiteURL). This is DISTINCT from urlx.Host, which strips the port
// for host-equality comparison; here the port is kept because the operator needs to
// see the exact host[:port] they typed.
func DisplayHost(raw string) string {
	if u, err := url.Parse(raw); err == nil && u.Host != "" {
		return u.Host
	}
	return raw
}
