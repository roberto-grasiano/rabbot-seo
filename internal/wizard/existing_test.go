package wizard

import "testing"

// TestExistingConfigChoiceResolves pins the pure mapping from a choice string to
// an ExistingAction enum, with an error for an unknown choice.
func TestExistingConfigChoiceResolves(t *testing.T) {
	cases := []struct {
		in      string
		want    ExistingAction
		wantErr bool
	}{
		{"add", ActionAddSite, false},
		{"reconfigure", ActionReconfigure, false},
		{"cancel", ActionCancel, false},
		{"bogus", 0, true},
		{"", 0, true},
	}
	for _, tc := range cases {
		t.Run(tc.in, func(t *testing.T) {
			got, err := ResolveExistingAction(tc.in)
			if tc.wantErr {
				if err == nil {
					t.Fatalf("ResolveExistingAction(%q) = %v, want error", tc.in, got)
				}
				return
			}
			if err != nil {
				t.Fatalf("ResolveExistingAction(%q) unexpected error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Fatalf("ResolveExistingAction(%q) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
