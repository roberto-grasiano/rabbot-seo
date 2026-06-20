package behavior

import "github.com/roberto-grasiano/rabbot-seo/internal/model"

// blogScenarios encodes the blog/personal/WordPress rows of the scenario matrix.
// Same pure-path semantics as publisherScenarios (see that file's header).
func blogScenarios() []scenario {
	const st = "blog"
	post := model.Snapshot{
		ID: 103, URLID: 7, HTTPStatus: 200, Indexable: true,
		Title:           "How I Built My Cabin — My Blog",
		MetaDescription: "A 1200-word build log of the cabin from foundation to finished roof with photos.",
		Canonical:       "https://blog.example/cabin", MetaRobots: "index,follow",
		Headings:   `{"h1":["How I Built My Cabin"],"h2":["Foundation","Walls"]}`,
		RenderMode: model.RenderServerRendered, InternalLinkCount: 24,
		SchemaTypes:   "BlogPosting",
		ContentSHA256: "blog-base", ContentSimhash: 0x00FF00FF00FF00FF,
		JSONLD: `[{"@type":"BlogPosting","headline":"How I Built My Cabin"}]`,
	}

	return []scenario{
		{
			name: "cold_start_healthy_wordpress_post", siteType: st, class: mustStayQuiet,
			old:          firstCrawlBaseline,
			nw:           func() model.Snapshot { s := post; s.ID = 0; return s }(),
			wantFindings: nil,
		},
		{
			name: "cold_start_missing_canonical_and_meta", siteType: st, class: mustStayQuiet,
			old: firstCrawlBaseline,
			nw: model.Snapshot{
				URLID: 7, HTTPStatus: 200, Title: "Welcome", MetaDescription: "", Canonical: "",
				Indexable: true, Headings: `{"h1":["Welcome"]}`, RenderMode: model.RenderServerRendered,
			},
			// canonical missing + meta absent OPEN but first-crawl Slack-suppressed.
			wantFindings: map[string]model.Severity{
				"canonical_changed":        model.SeverityCritical,
				"meta_description_changed": model.SeverityWarning,
			},
		},
		{
			name: "wordpress_discourage_search_engines_flip", siteType: st, class: mustFire,
			old: post,
			nw: func() model.Snapshot {
				s := post
				s.Indexable = false
				s.MetaRobots = "noindex,nofollow"
				s.IndexabilityReason = "meta noindex"
				return s
			}(),
			// THE marquee blog catch. Both critical findings (collapse to one page).
			wantFindings: map[string]model.Severity{
				"indexability_flip":   model.SeverityCritical,
				"meta_robots_noindex": model.SeverityCritical,
			},
		},
		{
			name: "plugin_update_injects_noindex_no_indexable_flip", siteType: st, class: mustFire,
			old: func() model.Snapshot {
				s := post
				s.Indexable = false
				s.MetaRobots = "nofollow"
				s.IndexabilityReason = "canonicalized"
				return s
			}(),
			nw: func() model.Snapshot {
				s := post
				s.Indexable = false
				s.MetaRobots = "noindex,nofollow"
				s.IndexabilityReason = "canonicalized"
				return s
			}(),
			// meta_robots_noindex fires; indexability_flip passes (Old already non-indexable).
			// meta_robots change-stream routes critical (no indexable flip => no collapse).
			wantFindings:    map[string]model.Severity{"meta_robots_noindex": model.SeverityCritical},
			wantSubstantive: []string{"meta_robots"},
		},
		{
			name: "steady_state_noindex_tag_archive", siteType: st, class: typeNoise,
			old: func() model.Snapshot {
				s := post
				s.Indexable = false
				s.MetaRobots = "noindex,follow"
				s.IndexabilityReason = "meta noindex"
				return s
			}(),
			nw: func() model.Snapshot {
				s := post
				s.ID = 0
				s.Indexable = false
				s.MetaRobots = "noindex,follow"
				s.IndexabilityReason = "meta noindex"
				return s
			}(),
			// meta_robots_noindex RE-FAILS (steady, no guard). indexability_flip passes (Old not indexable).
			// NOISE(overlay-TODO): intentional Yoast archive noindex.
			wantFindings:    map[string]model.Severity{"meta_robots_noindex": model.SeverityCritical},
			wantSubstantive: []string{},
		},
		{
			name: "post_title_edited_value_change", siteType: st, class: typeNoise,
			old: setTitle(post, "My 2024 Garden"),
			nw:  setTitle(post, "My 2024 Garden — Lessons Learned"),
			// NOISE(overlay-TODO): owner title edits.
			wantFindings: map[string]model.Severity{"title_changed": model.SeverityWarning},
		},
		{
			name: "post_body_substantive_rewrite", siteType: st, class: typeNoise,
			old:             withContent(post, "blog-a", 0x00FF00FF00FF00FF),
			nw:              withContent(post, "blog-b", 0xFF00FF00FF00FF00), // hamming large
			wantFindings:    nil,
			wantSubstantive: []string{"content"},
		},
		{
			name: "cosmetic_footer_year_bump", siteType: st, class: mustStayQuiet,
			old:             withContent(post, "blog-c", 0x1122334455667788),
			nw:              withContent(post, "blog-d", 0x1122334455667789), // hamming 1 cosmetic
			wantFindings:    nil,
			wantSubstantive: []string{},
		},
		{
			name: "h1_changed_with_title", siteType: st, class: typeNoise,
			old: setTitle(setHeadings(post, `{"h1":["Old Title"]}`), "Old Title"),
			nw:  setTitle(setHeadings(post, `{"h1":["New Title"]}`), "New Title"),
			wantFindings: map[string]model.Severity{
				"title_changed": model.SeverityWarning,
				"h1_issue":      model.SeverityWarning,
			},
			wantSubstantive: []string{"headings", "title"},
		},
		{
			name: "page_builder_renders_multiple_h1", siteType: st, class: typeNoise,
			old: setHeadings(post, `{"h1":["Intro"]}`),
			nw:  setHeadings(post, `{"h1":["Intro","Features","Pricing","FAQ"]}`),
			// FIXED (#h1-rewrite): the headings SET changed (1->4 h1), so the genuine-rewrite
			// WARNING arm fires ABOVE the count switch — a real heading rewrite on a multi-H1
			// page is no longer silently downgraded to the INFO "multiple" steady-state finding
			// (which never pages). headings diff also emitted (warning change-stream).
			wantFindings:    map[string]model.Severity{"h1_issue": model.SeverityWarning},
			wantSubstantive: []string{"headings"},
		},
		{
			name: "steady_multiple_h1_no_change", siteType: st, class: typeNoise,
			old: setHeadings(post, `{"h1":["Intro","Features"]}`),
			nw:  setHeadings(post, `{"h1":["Intro","Features"]}`),
			// 'multiple' INFO, no headings change. Never pages.
			wantFindings: map[string]model.Severity{"h1_issue": model.SeverityInfo},
		},
		{
			name: "post_returns_404_unpublished", siteType: st, class: mustFire,
			old: post,
			nw:  func() model.Snapshot { s := post; s.HTTPStatus = 404; s.Indexable = false; return s }(),
			wantFindings: map[string]model.Severity{
				"status_regression": model.SeverityCritical,
				"indexability_flip": model.SeverityCritical,
			},
		},
		{
			name: "hard_404_to_soft_redirect_home", siteType: st, class: edge,
			old: func() model.Snapshot { s := post; s.HTTPStatus = 404; s.Indexable = false; return s }(),
			nw: func() model.Snapshot {
				s := post
				s.HTTPStatus = 301
				s.RedirectChain = `["https://blog.example/dead-post","https://blog.example/"]`
				return s
			}(),
			// soft-404 GAP: 404->301 is "improvement" to status_regression (3xx passes). indexability_flip
			// passes (Old not indexable, so the false->true reappearance is NOT a flip). No redirect loop;
			// old chain empty => growth ok=false. NO RULE fires — a real SEO problem (soft-404) the engine
			// does not detect (un-built detector). Change-stream emits http_status (404->301), indexable
			// (false->true reappearance), and redirect_chain — none gated by a rule here.
			wantFindings:    nil,
			wantSubstantive: []string{"http_status", "indexable", "redirect_chain"},
		},
		{
			name: "canonical_swapped_to_wrong_url", siteType: st, class: mustFire,
			old:          setCanonical(post, "https://blog.example/post-a"),
			nw:           setCanonical(post, "https://blog.example/"),
			wantFindings: map[string]model.Severity{"canonical_changed": model.SeverityCritical},
		},
		{
			name: "title_disappears_template_break", siteType: st, class: mustFire,
			old:             setTitle(post, "A Good Post Title"),
			nw:              setTitle(post, ""),
			wantFindings:    map[string]model.Severity{"title_changed": model.SeverityWarning},
			wantSubstantive: []string{"title"},
		},
		{
			name: "article_jsonld_loses_headline", siteType: st, class: mustFire,
			old: withJSONLD(post, `[{"@type":"BlogPosting","headline":"My Post"}]`, "BlogPosting"),
			nw:  withJSONLD(post, `[{"@type":"BlogPosting"}]`, "BlogPosting"),
			// BlogPosting aliases to Article; lost-eligibility flip. schema_types unchanged.
			wantFindings: map[string]model.Severity{"rich_result_article": model.SeverityCritical},
		},
		{
			name: "article_jsonld_steady_missing_headline", siteType: st, class: mustStayQuiet,
			old: firstCrawlBaseline,
			nw:  func() model.Snapshot { s := post; s.ID = 0; s.JSONLD = `[{"@type":"BlogPosting"}]`; return s }(),
			// rich_result_article WARNING (Old.ID==0 so not a critical flip). First-crawl Slack-suppressed.
			wantFindings: map[string]model.Severity{"rich_result_article": model.SeverityWarning},
		},
		{
			name: "breadcrumb_jsonld_loses_itemlistelement", siteType: st, class: mustFire,
			old:          withJSONLD(post, `[{"@type":"BreadcrumbList","itemListElement":[{"position":1}]}]`, "BreadcrumbList"),
			nw:           withJSONLD(post, `[{"@type":"BreadcrumbList"}]`, "BreadcrumbList"),
			wantFindings: map[string]model.Severity{"rich_result_breadcrumb": model.SeverityCritical},
		},
		{
			name: "malformed_jsonld_block_after_plugin", siteType: st, class: mustFire,
			old:          func() model.Snapshot { s := post; s.JSONLDInvalidCount = 0; return s }(),
			nw:           func() model.Snapshot { s := post; s.JSONLDInvalidCount = 1; return s }(),
			wantFindings: map[string]model.Severity{"structured_data_invalid_json": model.SeverityWarning},
		},
		{
			name: "truncated_body_suppresses_jsonld_rules", siteType: st, class: edge,
			old:          func() model.Snapshot { s := post; s.JSONLDInvalidCount = 0; return s }(),
			nw:           func() model.Snapshot { s := post; s.JSONLDInvalidCount = 1; return s }(),
			truncated:    true,
			wantFindings: nil,
		},
		{
			name: "redirect_loop_within_cap", siteType: st, class: mustFire,
			old:          setRedirect(post, `["https://blog.example/x"]`),
			nw:           setRedirect(post, `["https://blog.example/x","https://blog.example/y","https://blog.example/x"]`),
			wantFindings: map[string]model.Severity{"redirect_loop": model.SeverityCritical},
		},
		{
			name: "redirect_chain_grows_no_loop", siteType: st, class: mustFire,
			old:          setRedirect(post, `["http://blog.example/p","https://blog.example/p"]`),
			nw:           setRedirect(post, `["http://blog.example/p","https://blog.example/p","https://blog.example/p/"]`),
			wantFindings: map[string]model.Severity{"redirect_chain_growth": model.SeverityWarning},
		},
		{
			name: "internal_links_collapse_nav_break", siteType: st, class: mustFire,
			old:          setInternalLinks(post, 40),
			nw:           setInternalLinks(post, 10), // 75% drop
			wantFindings: map[string]model.Severity{"broken_links_spike": model.SeverityWarning},
		},
		{
			name: "internal_links_minor_churn", siteType: st, class: mustStayQuiet,
			old:          setInternalLinks(post, 30),
			nw:           setInternalLinks(post, 33), // increase, no drop
			wantFindings: nil,
		},
		{
			name: "external_link_injection_hacked", siteType: st, class: mustFire,
			old:          setExternalLinks(post, 3),
			nw:           setExternalLinks(post, 48), // jump 45>=10 AND 48>=2*3
			wantFindings: map[string]model.Severity{"external_link_spike": model.SeverityWarning},
		},
		{
			name: "editorial_external_links_small_add", siteType: st, class: mustStayQuiet,
			old:          setExternalLinks(post, 6),
			nw:           setExternalLinks(post, 10), // jump 4 < abs floor 10
			wantFindings: nil,
		},
		{
			name: "image_alt_regression_after_media_reimport", siteType: st, class: typeNoise,
			old: setImages(post, 12, 1),
			nw:  setImages(post, 12, 7), // increase; coverage 0.42<0.80
			wantFindings: map[string]model.Severity{
				"image_alt_regression": model.SeverityWarning,
				"image_alt_missing":    model.SeverityInfo,
			},
		},
		{
			name: "image_alt_fix_rebaseline", siteType: st, class: mustStayQuiet,
			old:          setImages(post, 12, 8),
			nw:           setImages(post, 12, 2), // decrease; coverage 0.83>=0.8
			wantFindings: nil,
		},
		{
			name: "page_builder_flips_to_client_shell", siteType: st, class: mustFire,
			old:          setRender(post, model.RenderServerRendered),
			nw:           setRender(post, model.RenderClientShell),
			wantFindings: map[string]model.Severity{"needs_rendering": model.SeverityWarning},
		},
		{
			name: "render_mode_recovers_to_server", siteType: st, class: mustStayQuiet,
			old:          setRender(post, model.RenderClientShell),
			nw:           setRender(post, model.RenderServerRendered),
			wantFindings: nil,
		},
		{
			name: "steady_unknown_render_mode_pre_a8", siteType: st, class: mustStayQuiet,
			old:          setRender(post, model.RenderUnknown),
			nw:           setRender(post, model.RenderUnknown),
			wantFindings: nil,
		},
		{
			name: "oscillating_title_ab_test", siteType: st, class: typeNoise,
			old: setTitle(post, "Variant A — Blog"),
			nw:  setTitle(post, "Variant B — Blog"),
			// NOISE(overlay-TODO): A/B title flapping pages each flip.
			wantFindings: map[string]model.Severity{"title_changed": model.SeverityWarning},
		},
		{
			name: "http_to_https_migration_canonical_and_url", siteType: st, class: typeNoise,
			old: setCanonical(post, "http://blog.example/post"),
			nw:  setCanonical(post, "https://blog.example/post"),
			// NOISE(overlay-TODO): one-time HTTPS migration; canonical change pages.
			wantFindings: map[string]model.Severity{"canonical_changed": model.SeverityCritical},
		},
		{
			name: "meta_description_pixel_overflow_steady", siteType: st, class: mustStayQuiet,
			old: setMeta(post, "Discover the complete step-by-step build log of my off-grid cabin project covering the foundation, framing, insulation, electrical, plumbing, and the finished interior, with hundreds of progress photos and a full materials cost breakdown for anyone planning their own build today."),
			nw:  setMeta(post, "Discover the complete step-by-step build log of my off-grid cabin project covering the foundation, framing, insulation, electrical, plumbing, and the finished interior, with hundreds of progress photos and a full materials cost breakdown for anyone planning their own build today."),
			// overflow OPENS but push gate suppresses Slack (meta unchanged).
			wantFindings:    map[string]model.Severity{"meta_description_pixel_overflow": model.SeverityWarning},
			wantSubstantive: []string{},
		},
		{
			name: "meta_description_edited_into_overflow", siteType: st, class: typeNoise,
			old: setMeta(post, "Short desc."),
			nw:  setMeta(post, "Discover the complete step-by-step build log of my off-grid cabin project covering the foundation, framing, insulation, electrical, plumbing, and the finished interior, with hundreds of progress photos and a full materials cost breakdown for anyone planning their own build today."),
			wantFindings: map[string]model.Severity{
				"meta_description_changed":        model.SeverityWarning,
				"meta_description_pixel_overflow": model.SeverityWarning,
			},
		},
		{
			name: "title_pixel_overflow_with_emoji", siteType: st, class: edge,
			old: setTitle(post, "My Great Recipe Post"),
			nw:  setTitle(post, "My Great Recipe Post 🍕🔥😋👨‍🍳"), // measured 311px UNDER 580 (memory 8562 emoji undercount)
			// title_changed fires. title_pixel_overflow does NOT fire — emoji width is UNDER-measured (311px<580),
			// so a visually-overflowing emoji title is MISSED. The miss is the documented measurement weakness.
			wantFindings: map[string]model.Severity{"title_changed": model.SeverityWarning},
		},
		{
			name: "unparseable_headings_json", siteType: st, class: mustStayQuiet,
			old: setHeadings(post, `{"h1":["Title"]}`),
			nw:  setHeadings(post, "not json"),
			// h1_issue emits NOTHING on invalid JSON (don't-guess) — no false h1 finding. BUT the headings
			// STRING differs => substantive headings change-stream event (warning). RULE is correctly silent.
			wantFindings:    nil,
			wantSubstantive: []string{"headings"},
		},
		{
			name: "everything_stable_steady_state", siteType: st, class: mustStayQuiet,
			old:             post,
			nw:              func() model.Snapshot { s := post; s.ID = 0; return s }(),
			wantFindings:    nil,
			wantSubstantive: []string{},
		},
		{
			name: "noindex_recovers_indexable_restored", siteType: st, class: mustStayQuiet,
			old: func() model.Snapshot {
				s := post
				s.Indexable = false
				s.MetaRobots = "noindex,follow"
				s.IndexabilityReason = "meta noindex"
				return s
			}(),
			nw: post, // indexable true, index,follow
			// Recovery: both rules pass; no FINDING. (Recovery direction; scheduler emits a resolve, not a page.)
			wantFindings: nil,
		},
		{
			name: "x_robots_tag_header_noindex", siteType: st, class: mustFire,
			old: func() model.Snapshot {
				s := post
				s.Indexable = true
				s.MetaRobots = "index,follow"
				s.XRobotsTag = ""
				return s
			}(),
			nw: func() model.Snapshot {
				s := post
				s.Indexable = false
				s.MetaRobots = "index,follow"
				s.XRobotsTag = "noindex"
				s.IndexabilityReason = "x-robots-tag noindex"
				return s
			}(),
			// indexability_flip + meta_robots_noindex (via XRobotsTag). Both critical findings.
			// NOTE: triad collapse suppresses only meta_robots+indexability_reason; x_robots_tag is NOT
			// in the collapse list => the x_robots_tag change-stream event can DOUBLE-ALERT alongside the
			// collapsed 'indexable' event (a possible collapse-coverage hole — see suspected_mismatches).
			wantFindings: map[string]model.Severity{
				"indexability_flip":   model.SeverityCritical,
				"meta_robots_noindex": model.SeverityCritical,
			},
			wantSubstantive: []string{"indexability_reason", "indexable", "x_robots_tag"},
		},
		{
			name: "hreflang_added_on_monolingual_blog", siteType: st, class: typeNoise,
			old: setHreflang(post, ""),
			nw:  setHreflang(post, `["en","en-gb"]`),
			// hreflang_invalid WARNING; change-stream hreflang routes WARNING (FIXED #16: agrees with the rule).
			// NOISE(overlay-TODO): monolingual hreflang dead weight.
			wantFindings:    map[string]model.Severity{"hreflang_invalid": model.SeverityWarning},
			wantSubstantive: []string{"hreflang"},
		},
		{
			name: "server_error_first_crawl", siteType: st, class: mustFire,
			old: firstCrawlBaseline,
			nw:  model.Snapshot{URLID: 7, HTTPStatus: 503},
			// 5xx pages on first crawl (lone exception). canonical/title/meta open (suppressed).
			wantFindings: map[string]model.Severity{
				"status_regression":        model.SeverityCritical,
				"canonical_changed":        model.SeverityCritical,
				"title_changed":            model.SeverityWarning,
				"meta_description_changed": model.SeverityWarning,
			},
		},
		{
			name: "first_crawl_404_no_baseline", siteType: st, class: mustStayQuiet,
			old: firstCrawlBaseline,
			nw:  model.Snapshot{URLID: 7, HTTPStatus: 404, Title: "Not Found", Canonical: "https://blog.example/x", MetaDescription: "x", Headings: `{"h1":["x"]}`, RenderMode: model.RenderServerRendered},
			// FIXED (#born-4xx): a born-4xx (4xx on the very FIRST crawl, Old.ID==0) now OPENS
			// status_regression at WARNING — it has no 2xx/3xx baseline to regress from, so the
			// CRITICAL arm can't fire, but the broken page must still surface as an open issue
			// (it was previously invisible forever). mustStayQuiet is on the SLACK axis: the
			// pure path shows the open warning; ProcessFetch's first-crawl guard suppresses the page.
			wantFindings: map[string]model.Severity{"status_regression": model.SeverityWarning},
		},
		{
			name: "draft_to_publish_first_real_content", siteType: st, class: typeNoise,
			old: setTitle(withContent(post, "thin", 0xAAAA), "Coming Soon"),
			nw:  setTitle(withContent(post, "full", 0x5555), "The Full Post Title"), // hamming(0xAAAA,0x5555)=16 substantive
			// title_changed + substantive content.
			// NOISE(overlay-TODO): publishing is normal lifecycle.
			wantFindings:    map[string]model.Severity{"title_changed": model.SeverityWarning},
			wantSubstantive: []string{"content", "title"},
		},
	}
}
