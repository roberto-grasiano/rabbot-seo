package verify

import "testing"

func TestReasonConstants(t *testing.T) {
	// Pin the load-bearing string values (persist nowhere, but the UX maps them).
	cases := map[Reason]string{
		ReasonVerified:    "verified",
		ReasonNotFound:    "not_found",
		ReasonMismatch:    "mismatch",
		ReasonRedirected:  "redirected",
		ReasonUnreachable: "unreachable",
	}
	for r, want := range cases {
		if string(r) != want {
			t.Errorf("reason %v = %q, want %q", r, string(r), want)
		}
	}
}
