package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const maxPagesSeed = `# config.yaml
crawler:
  contact_email: ops@example.com
sites:
  - url: https://a.example
    name: A
  - url: https://b.example
`

func writeMaxPagesSeed(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(maxPagesSeed), 0o600); err != nil {
		t.Fatal(err)
	}
	return p
}

// loadMaxPages loads the config and returns the resolved MaxPages for siteURL.
func loadMaxPages(t *testing.T, path, siteURL string) int {
	t.Helper()
	cfg, err := Load(path, nil)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	for _, s := range cfg.Sites {
		if s.URL == siteURL {
			return cfg.ResolveDiscovery(s).MaxPages
		}
	}
	t.Fatalf("site %q not found in %+v", siteURL, cfg.Sites)
	return 0
}

func TestSetSiteMaxPagesYAMLSetsCap(t *testing.T) {
	p := writeMaxPagesSeed(t)

	if err := SetSiteMaxPagesYAML(p, "https://a.example", 500); err != nil {
		t.Fatalf("SetSiteMaxPagesYAML: %v", err)
	}
	if got := loadMaxPages(t, p, "https://a.example"); got != 500 {
		t.Errorf("MaxPages = %d, want 500", got)
	}
	// The untouched site still inherits the 2000 default.
	if got := loadMaxPages(t, p, "https://b.example"); got != 2000 {
		t.Errorf("sibling MaxPages = %d, want 2000 (untouched default)", got)
	}
}

func TestSetSiteMaxPagesYAMLZeroIsUnlimited(t *testing.T) {
	p := writeMaxPagesSeed(t)

	if err := SetSiteMaxPagesYAML(p, "https://a.example", 0); err != nil {
		t.Fatalf("SetSiteMaxPagesYAML: %v", err)
	}
	// 0 round-trips through koanf as &0 (explicit unlimited), NOT nil/inherit.
	if got := loadMaxPages(t, p, "https://a.example"); got != 0 {
		t.Errorf("MaxPages = %d, want 0 (unlimited)", got)
	}
}

func TestSetSiteMaxPagesYAMLMissingSiteIsNoop(t *testing.T) {
	p := writeMaxPagesSeed(t)

	// A missing site is a nil-error no-op (the LOCKED API returns plain error).
	if err := SetSiteMaxPagesYAML(p, "https://nope.example", 100); err != nil {
		t.Fatalf("SetSiteMaxPagesYAML for a missing site should be a nil-error no-op, got %v", err)
	}
	// The existing sites are untouched.
	if got := loadMaxPages(t, p, "https://a.example"); got != 2000 {
		t.Errorf("existing site MaxPages = %d, want 2000 (untouched)", got)
	}
}

// mergeSeed has a site whose discovery block ALREADY carries a sibling key
// (max_depth), plus a top-of-file comment and an inline comment, so we can prove
// SetSiteMaxPagesYAML MERGES max_pages_per_site into the existing block rather
// than replacing it, and round-trips every comment.
const mergeSeed = `# top-of-file comment — must survive
crawler:
  contact_email: ops@example.com
sites:
  - url: https://a.example
    name: A # inline comment — must survive
    discovery:
      max_depth: 5
`

// TestSetSiteMaxPagesYAMLMergesExistingDiscovery pins that writing the per-site
// cap into a site that ALREADY has a discovery block (a) keeps the sibling
// max_depth key, (b) ADDS max_pages_per_site (block merged, not clobbered),
// (c) round-trips the file/inline comments, and (d) the new cap is loadable via
// config.Load → ResolveDiscovery.
func TestSetSiteMaxPagesYAMLMergesExistingDiscovery(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.yaml")
	if err := os.WriteFile(p, []byte(mergeSeed), 0o600); err != nil {
		t.Fatal(err)
	}

	if err := SetSiteMaxPagesYAML(p, "https://a.example", 750); err != nil {
		t.Fatalf("SetSiteMaxPagesYAML: %v", err)
	}

	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)

	// (a) the sibling key survives (block was merged, not replaced).
	if !strings.Contains(got, "max_depth: 5") {
		t.Errorf("sibling key max_depth dropped — discovery block was clobbered:\n%s", got)
	}
	// (b) max_pages_per_site was ADDED.
	if !strings.Contains(got, "max_pages_per_site: 750") {
		t.Errorf("max_pages_per_site: 750 not added:\n%s", got)
	}
	// (c) comments round-trip (top-of-file + inline).
	if !strings.Contains(got, "# top-of-file comment — must survive") {
		t.Errorf("top-of-file comment dropped:\n%s", got)
	}
	if !strings.Contains(got, "# inline comment — must survive") {
		t.Errorf("inline comment dropped:\n%s", got)
	}

	// (d) the new cap is what ResolveDiscovery returns after a real Load.
	if mp := loadMaxPages(t, p, "https://a.example"); mp != 750 {
		t.Errorf("resolved MaxPages = %d, want 750", mp)
	}
}
