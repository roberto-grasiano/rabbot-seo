package behavior

import (
	"reflect"
	"sort"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// runScenarios is the shared table-driven runner: for each scenario it drives the
// real diff+rules pipeline and asserts the failing-finding set (and, when the
// scenario pins it, the substantive change-stream field set). A scenario tagged
// with skip is reported via t.Skip (a SUSPECTED DEFECT we refuse to bless).
func runScenarios(t *testing.T, scns []scenario) {
	t.Helper()
	for _, sc := range scns {
		sc := sc
		t.Run(sc.name, func(t *testing.T) {
			if sc.skip != "" {
				t.Skipf("SUSPECTED DEFECT: %s", sc.skip)
			}
			changes, findings := driveFindings(sc.old, sc.nw, sc.truncated)
			got := findingSet(findings)
			want := sc.wantFindings
			if want == nil {
				want = map[string]model.Severity{}
			}
			if !reflect.DeepEqual(got, want) {
				t.Errorf("[%s/%s] finding set mismatch\n  got:  %v\n  want: %v",
					sc.siteType, sc.class, sortedFindings(got), sortedFindings(want))
			}
			if sc.wantSubstantive != nil {
				gotSub := substantiveChangeFields(changes)
				wantSub := append([]string(nil), sc.wantSubstantive...)
				sort.Strings(wantSub)
				if !reflect.DeepEqual(gotSub, wantSub) {
					t.Errorf("[%s] substantive change-stream fields mismatch\n  got:  %v\n  want: %v",
						sc.name, gotSub, wantSub)
				}
			}
		})
	}
}

// sortedFindings renders a finding map deterministically for failure messages.
func sortedFindings(m map[string]model.Severity) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k+"="+string(m[k]))
	}
	sort.Strings(keys)
	return keys
}

func TestPublisherScenarios(t *testing.T)   { runScenarios(t, publisherScenarios()) }
func TestEcommerceScenarios(t *testing.T)   { runScenarios(t, ecommerceScenarios()) }
func TestMarketplaceScenarios(t *testing.T) { runScenarios(t, marketplaceScenarios()) }
func TestBlogScenarios(t *testing.T)        { runScenarios(t, blogScenarios()) }
func TestSaaSScenarios(t *testing.T)        { runScenarios(t, saasScenarios()) }
func TestLocalScenarios(t *testing.T)       { runScenarios(t, localScenarios()) }

// TestScenarioCountIsLogged makes the encoded-scenario total explicit so a dropped
// scenario is never silent (the task forbids a silent cap).
func TestScenarioCountIsLogged(t *testing.T) {
	groups := map[string]int{
		"publisher":   len(publisherScenarios()),
		"ecommerce":   len(ecommerceScenarios()),
		"marketplace": len(marketplaceScenarios()),
		"blog":        len(blogScenarios()),
		"saas":        len(saasScenarios()),
		"local":       len(localScenarios()),
	}
	total := ScenarioCount()
	t.Logf("encoded scenarios = %d  breakdown=%v", total, groups)
	if total == 0 {
		t.Fatal("no scenarios encoded")
	}
}
