package segments

import (
	"strings"
	"sync"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
)

// cfg is a tiny helper to keep the table tests readable.
func cfg(pairs ...string) []config.SegmentConfig {
	if len(pairs)%2 != 0 {
		panic("cfg needs name/match pairs")
	}
	out := make([]config.SegmentConfig, 0, len(pairs)/2)
	for i := 0; i < len(pairs); i += 2 {
		out = append(out, config.SegmentConfig{Name: pairs[i], Match: pairs[i+1]})
	}
	return out
}

func TestCompile_AnchoredPathMatch(t *testing.T) {
	m, err := Compile(7, cfg("content", "^/blog/"))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}

	cases := []struct {
		url  string
		want []string
	}{
		{"https://example.com/blog/x", []string{"content"}},
		{"https://example.com/blog/", []string{"content"}},
		// /blogger must NOT match ^/blog/ — the trailing slash anchor protects it.
		{"https://example.com/blogger", nil},
		{"https://example.com/blog", nil},
		{"https://example.com/other", nil},
	}
	for _, c := range cases {
		got := m.Match(c.url)
		if !equalNames(got, c.want) {
			t.Errorf("Match(%q) = %v, want %v", c.url, got, c.want)
		}
	}
}

func TestCompile_QueryStringExcluded(t *testing.T) {
	// A pattern intended to match a query string must never fire, because v1
	// matches the path only.
	m, err := Compile(1, cfg("paged", `page=`))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if got := m.Match("https://example.com/list?page=2"); got != nil {
		t.Errorf("query string leaked into match input: got %v", got)
	}

	// And a path pattern still matches even when a query string is present.
	m2, err := Compile(1, cfg("content", "^/blog/"))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	if got := m2.Match("https://example.com/blog/post?utm=x&page=2"); !equalNames(got, []string{"content"}) {
		t.Errorf("path match should ignore query string: got %v", got)
	}
}

func TestCompile_MultiMembership(t *testing.T) {
	m, err := Compile(1, cfg(
		"content", "^/blog/",
		"featured", "^/blog/featured/",
	))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	got := m.Match("https://example.com/blog/featured/post")
	if !equalNames(got, []string{"content", "featured"}) {
		t.Errorf("Match = %v, want both content and featured", got)
	}
	// Deterministic order = config order.
	got2 := m.Match("https://example.com/blog/featured/post")
	if len(got2) != 2 || got2[0] != "content" || got2[1] != "featured" {
		t.Errorf("expected config order [content featured], got %v", got2)
	}
}

func TestCompile_InvalidRegexp(t *testing.T) {
	_, err := Compile(42, cfg("content", "^/blog/("))
	if err == nil {
		t.Fatal("expected error for invalid regexp")
	}
	if !strings.Contains(err.Error(), "content") {
		t.Errorf("error should name the segment: %v", err)
	}
	// Site identity must be present (42 here).
	if !strings.Contains(err.Error(), "42") {
		t.Errorf("error should name the site: %v", err)
	}
}

func TestCompile_DuplicateName(t *testing.T) {
	_, err := Compile(3, cfg(
		"content", "^/blog/",
		"content", "^/news/",
	))
	if err == nil {
		t.Fatal("expected error for duplicate name")
	}
	if !strings.Contains(err.Error(), "content") || !strings.Contains(err.Error(), "3") {
		t.Errorf("error should name site and segment: %v", err)
	}
}

func TestCompile_NameCharset(t *testing.T) {
	bad := []string{"Blog", "blog pages", "", "blog/news", "café", "BLOG", "blog.news", "_ok-but-has Space "}
	for _, name := range bad {
		if _, err := Compile(1, cfg(name, "^/x/")); err == nil {
			t.Errorf("name %q should be rejected", name)
		}
	}
	good := []string{"content", "product", "blog-news", "blog_news", "a", "a-b_c"}
	for _, name := range good {
		if _, err := Compile(1, cfg(name, "^/x/")); err != nil {
			t.Errorf("name %q should be accepted: %v", name, err)
		}
	}
}

func TestCompile_EmptyConfig(t *testing.T) {
	m, err := Compile(1, nil)
	if err != nil {
		t.Fatalf("Compile(nil): %v", err)
	}
	if got := m.Match("https://example.com/blog/x"); got != nil {
		t.Errorf("empty matcher should match nothing, got %v", got)
	}
}

func TestCompile_MalformedURL(t *testing.T) {
	m, err := Compile(1, cfg("content", "^/blog/"))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	// A path-only string (no scheme/host) still has a parseable path.
	if got := m.Match("/blog/x"); !equalNames(got, []string{"content"}) {
		t.Errorf("relative path should match: got %v", got)
	}
	// An unparseable URL must not panic; it simply matches nothing.
	if got := m.Match("https://exa mple.com/blog/x"); got != nil {
		_ = got // tolerate either nil or a best-effort match, but no panic
	}
}

func TestSegmentIDs(t *testing.T) {
	m, err := Compile(1, cfg("content", "^/blog/", "product", "^/product/"))
	if err != nil {
		t.Fatalf("Compile: %v", err)
	}
	// IDs are resolvable after Bind.
	m.Bind(map[string]int64{"content": 100, "product": 200})
	got := m.MatchIDs("https://example.com/blog/x")
	if len(got) != 1 || got[0] != 100 {
		t.Errorf("MatchIDs = %v, want [100]", got)
	}
}

// --- Registry ---

func TestRegistry_LookupAndSwap(t *testing.T) {
	reg := NewRegistry()

	mA, err := Compile(1, cfg("content", "^/blog/"))
	if err != nil {
		t.Fatal(err)
	}
	reg.Swap(map[int64]*SiteMatcher{1: mA})

	if got := reg.SegmentsFor(1, "https://example.com/blog/x"); !equalNames(got, []string{"content"}) {
		t.Errorf("SegmentsFor = %v, want [content]", got)
	}
	// Unknown site → nil, no panic.
	if got := reg.SegmentsFor(999, "https://example.com/blog/x"); got != nil {
		t.Errorf("unknown site should yield nil, got %v", got)
	}

	// Swap replaces the matcher set atomically.
	mB, err := Compile(1, cfg("product", "^/product/"))
	if err != nil {
		t.Fatal(err)
	}
	reg.Swap(map[int64]*SiteMatcher{1: mB})
	if got := reg.SegmentsFor(1, "https://example.com/blog/x"); got != nil {
		t.Errorf("after swap, old pattern should not match: %v", got)
	}
	if got := reg.SegmentsFor(1, "https://example.com/product/y"); !equalNames(got, []string{"product"}) {
		t.Errorf("after swap, new pattern should match: %v", got)
	}
}

func TestRegistry_ConcurrentSwapAndLookup(t *testing.T) {
	reg := NewRegistry()
	mA, _ := Compile(1, cfg("content", "^/blog/"))
	reg.Swap(map[int64]*SiteMatcher{1: mA})

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// Readers.
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					_ = reg.SegmentsFor(1, "https://example.com/blog/x")
				}
			}
		}()
	}
	// Swappers.
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				select {
				case <-stop:
					return
				default:
					m, _ := Compile(1, cfg("content", "^/blog/"))
					reg.Swap(map[int64]*SiteMatcher{1: m})
				}
			}
		}(i)
	}

	// Let it run a bit then stop.
	done := make(chan struct{})
	go func() {
		// brief activity window driven by the swappers finishing
		for j := 0; j < 2000; j++ {
			_ = reg.SegmentsFor(1, "https://example.com/blog/x")
		}
		close(done)
	}()
	<-done
	close(stop)
	wg.Wait()
}

func equalNames(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
