package hydration

import "time"

// timeoutAfter returns a channel that fires after a generous wall-clock budget.
// It guards the cyclic-reference termination tests: if the resolver fails to
// terminate (an unbounded recursion / index cycle), the select in the test
// falls through to this channel and fails loudly rather than hanging the suite.
// The budget is deliberately generous so a slow CI box never produces a false
// timeout — a correct, bounded decoder finishes in microseconds.
func timeoutAfter() <-chan time.Time {
	return time.After(5 * time.Second)
}
