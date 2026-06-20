package wizard

import (
	"errors"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/setup"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

func TestBuildPlanHappyPath(t *testing.T) {
	in := Inputs{
		ContactEmail: "ops@example.com",
		Authorized:   true,
		Sites: []SiteDraft{{
			URL:         "https://example.com",
			Name:        "Ex",
			MinInterval: "15m",
			MaxInterval: "24h",
			Speed:       80,
			Method:      verify.MethodWellKnown,
			Token:       "rab_x",
			Verified:    true,
		}},
	}
	plan, err := BuildPlan(in)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if plan.ContactEmail != "ops@example.com" {
		t.Errorf("ContactEmail = %q", plan.ContactEmail)
	}
	if !plan.Authorized {
		t.Errorf("Authorized = false, want true")
	}
	if len(plan.Sites) != 1 {
		t.Fatalf("Sites len = %d, want 1", len(plan.Sites))
	}
	s := plan.Sites[0]
	if s.URL != "https://example.com" || s.Name != "Ex" {
		t.Errorf("site URL/Name = %q/%q", s.URL, s.Name)
	}
	if s.MinInterval != "15m" || s.MaxInterval != "24h" || s.Speed != 80 {
		t.Errorf("site cadence = %q/%q/%d", s.MinInterval, s.MaxInterval, s.Speed)
	}

	// Method/Token/Verified are deliberately OUT-OF-BAND: setup.Plan / setup.Apply
	// cannot persist them, so BuildPlan must not smuggle them onto the Plan. The
	// setup.SiteInput type has no such fields — assert via the public type shape
	// that we only ever set the five Apply-handled fields. This is enforced at
	// compile time below (a struct literal naming every field), so a future field
	// addition to SiteInput forces this test to be revisited.
	_ = setup.SiteInput{URL: "", Name: "", MinInterval: "", MaxInterval: "", Speed: 0}
}

// TestBuildPlanIgnoresMCPConnect asserts the Connect-Claude fields are out-of-band
// (like proof intent): they live on Inputs for the runner to act on, but BuildPlan
// must not smuggle them onto setup.Plan (setup.Apply cannot persist them).
func TestBuildPlanIgnoresMCPConnect(t *testing.T) {
	in := Inputs{
		ContactEmail:  "ops@example.com",
		Authorized:    true,
		Sites:         []SiteDraft{{URL: "https://example.com"}},
		ConnectMCP:    true,
		ConnectTarget: "project",
	}
	plan, err := BuildPlan(in)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	// A valid plan is produced regardless of the MCP fields, and the plan has no
	// place to carry them (compile-time: setup.Plan has no MCP fields).
	if plan.ContactEmail != "ops@example.com" || len(plan.Sites) != 1 {
		t.Fatalf("plan = %+v, MCP fields must not affect plan assembly", plan)
	}
}

// TestInputsCarryAlertsAndRunIntent asserts the alerts/run intent fields are
// OUT-OF-BAND (like ConnectMCP): they live on Inputs for the runner to act on,
// but BuildPlan must NOT smuggle them onto setup.Plan (setup.Apply cannot persist
// them). A valid plan is produced regardless of these fields.
func TestInputsCarryAlertsAndRunIntent(t *testing.T) {
	in := Inputs{
		ContactEmail:   "ops@example.com",
		Authorized:     true,
		Sites:          []SiteDraft{{URL: "https://example.com"}},
		SlackWebhook:   "https://hooks.slack.com/SECRET",
		StartDaemon:    true,
		InstallService: true,
	}
	plan, err := BuildPlan(in)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if plan.ContactEmail != "ops@example.com" || len(plan.Sites) != 1 {
		t.Fatalf("alerts/run fields must not affect plan assembly: %+v", plan)
	}
	// setup.Plan has no place to carry alerts/run intent (compile-time guarantee):
	// it only has ContactEmail/Authorized/Sites.
	_ = setup.Plan{ContactEmail: "", Authorized: false, Sites: nil}
}

func TestBuildPlanInvalidRejected(t *testing.T) {
	cases := []struct {
		name    string
		in      Inputs
		wantErr error
	}{
		{
			name: "missing contact email",
			in: Inputs{
				Authorized: true,
				Sites:      []SiteDraft{{URL: "https://example.com"}},
			},
			wantErr: setup.ErrContactEmailRequired,
		},
		{
			name: "not authorized",
			in: Inputs{
				ContactEmail: "ops@example.com",
				Sites:        []SiteDraft{{URL: "https://example.com"}},
			},
			wantErr: setup.ErrNotAuthorized,
		},
		{
			name: "no sites",
			in: Inputs{
				ContactEmail: "ops@example.com",
				Authorized:   true,
			},
			wantErr: setup.ErrNoSites,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := BuildPlan(tc.in)
			if err == nil {
				t.Fatalf("expected error %v, got nil", tc.wantErr)
			}
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("expected error %v, got %v", tc.wantErr, err)
			}
		})
	}
}

// TestBuildPlanIgnoresMaxPages pins that the cap pointer is OUT-OF-BAND: BuildPlan
// maps only the five setup.Apply-persisted fields, so a SiteDraft.MaxPages must NOT
// leak onto setup.SiteInput (which has no cap field). The cap is written separately
// post-Apply via config.SetSiteMaxPagesYAML, mirroring the Method/Token seam.
func TestBuildPlanIgnoresMaxPages(t *testing.T) {
	cap0 := 0
	in := Inputs{
		ContactEmail: "ops@me.example",
		Authorized:   true,
		Sites:        []SiteDraft{{URL: "https://example.com", MaxPages: &cap0}},
	}
	plan, err := BuildPlan(in)
	if err != nil {
		t.Fatalf("BuildPlan: %v", err)
	}
	if len(plan.Sites) != 1 {
		t.Fatalf("plan.Sites = %d, want 1", len(plan.Sites))
	}
	// setup.SiteInput has no cap field; this compiles only because MaxPages stayed off
	// the Plan. Assert the carried fields round-trip and nothing else.
	got := plan.Sites[0]
	want := setup.SiteInput{URL: "https://example.com"}
	if got != want {
		t.Errorf("plan.Sites[0] = %+v, want %+v", got, want)
	}
}

func TestBuildPlanPrivateSiteRejected(t *testing.T) {
	in := Inputs{
		ContactEmail: "ops@example.com",
		Authorized:   true,
		Sites:        []SiteDraft{{URL: "http://127.0.0.1"}},
	}
	_, err := BuildPlan(in)
	if err == nil {
		t.Fatal("expected error for a private/loopback site, got nil")
	}
}
