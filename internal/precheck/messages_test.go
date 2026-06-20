package precheck

import (
	"strings"
	"testing"
)

// containsAny reports whether s contains any of the substrings (case-insensitive).
func containsAny(s string, subs ...string) bool {
	ls := strings.ToLower(s)
	for _, sub := range subs {
		if strings.Contains(ls, strings.ToLower(sub)) {
			return true
		}
	}
	return false
}

func TestApplyMessagesPerKind(t *testing.T) {
	tests := []struct {
		kind RenderKind
		// summaryMust: every kind's Summary must contain at least one of these.
		summaryMust []string
		// adviceMust: every kind's Advice must contain at least one of these.
		adviceMust []string
	}{
		{
			kind:        ServerRendered,
			summaryMust: []string{"present in the server", "fully monitor"},
			adviceMust:  []string{"view source"},
		},
		{
			kind:        Hydrated,
			summaryMust: []string{"recoverable", "hydration payload"},
			adviceMust:  []string{"view source"},
		},
		{
			kind:        ClientShell,
			summaryMust: []string{"appears", "likely"},
			// The mandatory honest warning lives in Advice.
			adviceMust: []string{"reads the server", "may not see", "cannot fully verify"},
		},
		{
			kind:        HeadOnlyShell,
			summaryMust: []string{"partial", "head"},
			// The honest "body may not be visible / cannot fully verify" warning.
			adviceMust: []string{"cannot fully verify", "body"},
		},
		{
			kind:        Unknown,
			summaryMust: []string{"couldn't", "could not", "confident"},
			adviceMust:  []string{"view source"},
		},
	}

	for _, tc := range tests {
		t.Run(string(tc.kind), func(t *testing.T) {
			js := JSDependency{Kind: tc.kind}
			applyMessages(&js)

			if strings.TrimSpace(js.Summary) == "" {
				t.Fatalf("Summary empty for %s", tc.kind)
			}
			if !containsAny(js.Summary, tc.summaryMust...) {
				t.Errorf("Summary %q missing all of %v", js.Summary, tc.summaryMust)
			}
			for _, must := range tc.adviceMust {
				if !containsAny(js.Advice, must) {
					t.Errorf("%s Advice %q missing %q", tc.kind, js.Advice, must)
				}
			}
			// No kind may overclaim certainty.
			if containsAny(js.Summary, "definitely", "guaranteed", "certain") {
				t.Errorf("%s Summary overclaims certainty: %q", tc.kind, js.Summary)
			}
			if containsAny(js.Advice, "definitely", "guaranteed") {
				t.Errorf("%s Advice overclaims certainty: %q", tc.kind, js.Advice)
			}
		})
	}
}

// TestClientShellMandatoryWarning makes the user's #1 requirement load-bearing:
// the ClientShell advice MUST carry the full honest warning.
func TestClientShellMandatoryWarning(t *testing.T) {
	js := JSDependency{Kind: ClientShell}
	applyMessages(&js)
	for _, phrase := range []string{
		"reads the server",
		"may not see",
		"cannot fully verify",
	} {
		if !containsAny(js.Advice, phrase) {
			t.Errorf("ClientShell Advice missing mandatory phrase %q; got %q", phrase, js.Advice)
		}
	}
	// And it must say this is a hint, not a verdict.
	if !containsAny(js.Advice, "hint", "calibrated") {
		t.Errorf("ClientShell Advice should frame the call as a hint; got %q", js.Advice)
	}
}
