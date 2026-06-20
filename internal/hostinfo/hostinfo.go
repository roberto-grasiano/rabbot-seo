// Package hostinfo answers one best-effort question: does this host look like a
// machine that sleeps? It is a pure-Go, build-tagged, dependency-light probe used
// only to decide whether `rabbot init` prints a one-line "this looks like a laptop"
// nudge at go-live. It never blocks, never errors out to a caller, never touches the
// network, runs no goroutines, and execs nothing. On any error or unknown platform
// the answer is a conservative false (unknown ⇒ silent, no nudge).
package hostinfo

// Sleeper reports whether the host appears to be a machine that sleeps — the honest
// ~90% proxy being "the host has a battery". It returns false on any error and on
// any platform Rabbot has no probe for, so callers may treat a false as either
// "definitely not a sleeper" or "we don't know" — both warrant no nudge.
//
// The platform-specific probe lives in the build-tagged sibling files
// (hostinfo_linux.go, hostinfo_darwin.go, hostinfo_windows.go, hostinfo_other.go),
// each exposing an unexported sleeper() the build selects exactly one of.
func Sleeper() bool {
	return sleeper()
}
