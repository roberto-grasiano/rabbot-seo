package obs

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	yaml "go.yaml.in/yaml/v3"
)

// bundleFiles is the exact set of files WriteObservabilityBundle must emit. The
// test pins the set so an accidental rename or a dropped provisioning file fails
// loudly rather than producing a stack that silently won't scrape.
var bundleFiles = []string{
	"docker-compose.observability.yml",
	"prometheus.yml",
	"grafana/provisioning/datasources/datasource.yml",
	"grafana/provisioning/dashboards/dashboard.yml",
	"grafana/dashboards/rabbot.json",
}

// Criterion 8: WriteObservabilityBundle writes the full provisioned bundle into a
// fresh dir; every expected file exists.
func TestWriteObservabilityBundle_WritesEveryFile(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := WriteObservabilityBundle(dir); err != nil {
		t.Fatalf("WriteObservabilityBundle: %v", err)
	}
	for _, rel := range bundleFiles {
		p := filepath.Join(dir, rel)
		fi, err := os.Stat(p)
		if err != nil {
			t.Fatalf("bundle file %q missing: %v", rel, err)
		}
		if fi.Size() == 0 {
			t.Fatalf("bundle file %q is empty", rel)
		}
	}
}

// Criterion 8: every YAML/JSON file in the bundle parses.
func TestWriteObservabilityBundle_FilesParse(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := WriteObservabilityBundle(dir); err != nil {
		t.Fatalf("WriteObservabilityBundle: %v", err)
	}

	for _, rel := range bundleFiles {
		raw, err := os.ReadFile(filepath.Join(dir, rel)) //nolint:gosec // test-controlled path under t.TempDir
		if err != nil {
			t.Fatalf("read %q: %v", rel, err)
		}
		switch {
		case strings.HasSuffix(rel, ".json"):
			var v any
			if err := json.Unmarshal(raw, &v); err != nil {
				t.Fatalf("%q is not valid JSON: %v", rel, err)
			}
		case strings.HasSuffix(rel, ".yml") || strings.HasSuffix(rel, ".yaml"):
			var v any
			if err := yaml.Unmarshal(raw, &v); err != nil {
				t.Fatalf("%q is not valid YAML: %v", rel, err)
			}
		default:
			t.Fatalf("unexpected bundle file extension: %q", rel)
		}
	}
}

// Criterion 10 (byte-identical re-run): writing twice into the same dir produces
// byte-identical files — the generator is safe for an agent to retry.
func TestWriteObservabilityBundle_ByteIdenticalRerun(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()

	if err := WriteObservabilityBundle(dir); err != nil {
		t.Fatalf("WriteObservabilityBundle (first): %v", err)
	}
	first := make(map[string][]byte, len(bundleFiles))
	for _, rel := range bundleFiles {
		b, err := os.ReadFile(filepath.Join(dir, rel)) //nolint:gosec // test-controlled path under t.TempDir
		if err != nil {
			t.Fatalf("read %q after first write: %v", rel, err)
		}
		first[rel] = b
	}

	if err := WriteObservabilityBundle(dir); err != nil {
		t.Fatalf("WriteObservabilityBundle (second): %v", err)
	}
	for _, rel := range bundleFiles {
		b, err := os.ReadFile(filepath.Join(dir, rel)) //nolint:gosec // test-controlled path under t.TempDir
		if err != nil {
			t.Fatalf("read %q after second write: %v", rel, err)
		}
		if string(b) != string(first[rel]) {
			t.Fatalf("bundle file %q changed on re-run; generator must be byte-identical", rel)
		}
	}
}

// dashboardMetricNames extracts every rabbot_* token appearing in the committed
// dashboard JSON. The dashboard references metrics by name in panel exprs; this
// pulls each distinct rabbot_-prefixed identifier so the consistency test can
// assert it is actually registered.
func dashboardMetricNames(t *testing.T, dir string) []string {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(dir, "grafana/dashboards/rabbot.json")) //nolint:gosec // test path
	if err != nil {
		t.Fatalf("read dashboard: %v", err)
	}
	return extractRabbotNames(string(raw))
}

// extractRabbotNames scans s for distinct identifiers beginning with "rabbot_"
// (the metric-name prefix), stopping each at the first character that cannot be
// part of a Prometheus metric name ([a-zA-Z0-9_:]). It returns them in first-seen
// order with duplicates removed.
func extractRabbotNames(s string) []string {
	const prefix = "rabbot_"
	seen := map[string]bool{}
	var out []string
	for i := 0; i+len(prefix) <= len(s); {
		if s[i:i+len(prefix)] != prefix {
			i++
			continue
		}
		j := i
		for j < len(s) && isMetricNameByte(s[j]) {
			j++
		}
		name := s[i:j]
		if !seen[name] {
			seen[name] = true
			out = append(out, name)
		}
		i = j
	}
	return out
}

func isMetricNameByte(b byte) bool {
	return b == '_' || b == ':' ||
		(b >= 'a' && b <= 'z') || (b >= 'A' && b <= 'Z') || (b >= '0' && b <= '9')
}

// Criterion 8 (consistency): every rabbot_* metric named in the committed
// dashboard JSON exists in the registered metric set — a dashboard panel cannot
// reference a metric the daemon does not emit (pattern: docs_consistency_test.go).
func TestDashboardMetricsAreRegistered(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	if err := WriteObservabilityBundle(dir); err != nil {
		t.Fatalf("WriteObservabilityBundle: %v", err)
	}

	registered := registeredMetricNameSet(t)

	dashNames := dashboardMetricNames(t, dir)
	if len(dashNames) == 0 {
		t.Fatal("dashboard references no rabbot_* metrics; expected the committed panels to use them")
	}
	for _, name := range dashNames {
		if !registered[familyBaseName(name)] {
			t.Errorf("dashboard references metric %q that is not in the registered metric set", name)
		}
	}
}

// familyBaseName maps an exposition time series name back to its registered
// metric-family name. A histogram registered as rabbot_fetch_duration_seconds is
// exposed as the _bucket/_sum/_count time series; a dashboard naturally refers to
// rabbot_fetch_duration_seconds_bucket, which belongs to the same family. The
// consistency check is about the family being registered, so the suffix is
// stripped before lookup.
func familyBaseName(name string) string {
	for _, suffix := range []string{"_bucket", "_sum", "_count"} {
		if strings.HasSuffix(name, suffix) {
			return strings.TrimSuffix(name, suffix)
		}
	}
	return name
}

// registeredMetricNameSet gathers the names of every rabbot_* metric family the
// daemon registers, the same exposition Prometheus would scrape. Vector families
// must be populated before Gather emits them, so it touches each labelled vector
// once with a representative label value (the closed enums / a config name).
func registeredMetricNameSet(t *testing.T) map[string]bool {
	t.Helper()
	m := NewMetrics("test")
	// Emit every vector family so Gather returns it.
	m.ObserveFetch("ok", 0)
	m.AddChanges("cosmetic", 1)
	m.ObserveDispatch("slack", nil)
	m.AddDigestDropped(1)

	families, err := m.Registry().Gather()
	if err != nil {
		t.Fatalf("Gather: %v", err)
	}
	set := map[string]bool{}
	for _, fam := range families {
		set[fam.GetName()] = true
	}
	return set
}
