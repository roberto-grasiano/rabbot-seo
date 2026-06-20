package wizard

import (
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/precheck"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

func TestRecommendMethod_EditableCMSPrefersMeta(t *testing.T) {
	m, hint := RecommendMethod(precheck.PlatformWordPress)
	if m != verify.MethodMeta {
		t.Fatalf("WordPress should recommend the meta-tag method, got %q", m)
	}
	if hint == "" {
		t.Fatal("expected a plain hint")
	}
}

func TestRecommendMethod_UnknownHasNoForcedChoice(t *testing.T) {
	if _, hint := RecommendMethod(precheck.PlatformUnknown); hint != "" {
		// Unknown ⇒ no recommendation (empty hint); the screen shows plain choices.
		t.Fatalf("unknown platform should not push a recommendation, got hint %q", hint)
	}
}

func TestProviderHint_WordPressMetaHasDeepLink(t *testing.T) {
	label, url := ProviderHint(precheck.PlatformWordPress, verify.MethodMeta)
	if label == "" || url == "" {
		t.Fatal("expected a labeled deep-link for WordPress + meta")
	}
}
