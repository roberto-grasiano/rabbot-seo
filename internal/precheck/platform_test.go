package precheck

import "testing"

func TestSniffPlatform_FromGeneratorMeta(t *testing.T) {
	cases := map[string]Platform{
		`<meta name="generator" content="WordPress 6.5">`:           PlatformWordPress,
		`<meta name="generator" content="Squarespace">`:             PlatformSquarespace,
		`<meta name="generator" content="Wix.com Website Builder">`: PlatformWix,
		`<html><body>nothing here</body></html>`:                    PlatformUnknown,
	}
	for html, want := range cases {
		if got := SniffPlatform(html); got != want {
			t.Errorf("SniffPlatform(%q) = %v, want %v", html, got, want)
		}
	}
}
