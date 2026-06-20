//go:build !linux && !darwin && !windows

package hostinfo

// sleeper is the fallback for any platform Rabbot has no battery probe for. Unknown
// ⇒ false (no nudge), by design.
func sleeper() bool {
	return false
}
