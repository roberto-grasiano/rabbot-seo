package behavior

import "github.com/roberto-grasiano/rabbot-seo/internal/model"

// publisherScenarios encodes the publisher/news rows of the scenario matrix.
//
// IMPORTANT — pure-path semantics: driveFindings returns the FAILED-rule set from
// rules.DefaultRuleSet().Eval (the engine's open-issue set), BEFORE ProcessFetch's
// first-crawl Slack gate, noindex-triad collapse, and bridge dedup (those live in
// internal/scheduler, which this package may not import). So:
//   - A noindex flip shows BOTH indexability_flip AND meta_robots_noindex as
//     critical FINDINGS here; the scheduler collapses them to ONE Slack page.
//   - A first-crawl "must_stay_quiet" scenario still OPENS issues (canonical/title/
//     meta) here — quiet is scoped to SLACK (the scheduler suppresses all but
//     http_status on first crawl). The comment on each such scenario says so.
//   - A scenario whose alert is a change-stream field (content/meta_robots/schema_
//     types/hreflang change) with no rule asserts wantSubstantive instead.
func publisherScenarios() []scenario {
	const st = "publisher/news"
	// A healthy steady article baseline reused across several scenarios.
	healthy := model.Snapshot{
		ID: 1, URLID: 7, HTTPStatus: 200,
		Title: "Mayor Resigns", MetaDescription: "The mayor stepped down today.",
		Canonical: "https://news.example/mayor", Indexable: true, MetaRobots: "",
		Headings: `{"h1":["Mayor Resigns"]}`, RenderMode: model.RenderServerRendered,
		ContentSHA256: "h-base", ContentSimhash: 0x1111111111111111,
		SchemaTypes: "Article", JSONLD: `[{"@type":"Article","headline":"Mayor Resigns"}]`,
	}

	return []scenario{
		{
			name: "cold_start_well_formed_article", siteType: st, class: mustStayQuiet,
			old: firstCrawlBaseline,
			nw: model.Snapshot{
				URLID: 7, HTTPStatus: 200, Title: "Senate Passes Bill — CityNews",
				MetaDescription: "The vote came after a long debate in the chamber over the weekend session details.",
				Canonical:       "https://news.example/senate-bill", Indexable: true,
				Headings: `{"h1":["Senate Passes Bill"]}`, RenderMode: model.RenderServerRendered,
				JSONLD: `[{"@type":"Article","headline":"Senate Passes Bill"}]`, SchemaTypes: "Article",
			},
			// baseline sentinel => diff nil; all snapshot rules pass. Truly silent.
			wantFindings: nil,
		},
		{
			name: "cold_start_article_missing_canonical_and_meta", siteType: st, class: mustStayQuiet,
			old: firstCrawlBaseline,
			nw: model.Snapshot{
				URLID: 7, HTTPStatus: 200, Title: "Breaking: Mayor Resigns",
				MetaDescription: "", Canonical: "", Indexable: true,
				Headings: `{"h1":["Mayor Resigns"]}`, RenderMode: model.RenderServerRendered,
			},
			// Issues OPEN (canonical missing critical, meta absent warning) but the
			// scheduler suppresses both from Slack on first crawl (Old.ID==0). Quiet=Slack.
			wantFindings: map[string]model.Severity{
				"canonical_changed":        model.SeverityCritical,
				"meta_description_changed": model.SeverityWarning,
			},
		},
		{
			name: "cold_start_5xx_origin_down", siteType: st, class: mustFire,
			old: firstCrawlBaseline,
			nw: model.Snapshot{
				URLID: 7, HTTPStatus: 503, Title: "", Canonical: "", Indexable: false,
				RenderMode: model.RenderUnknown,
			},
			// 5xx fires regardless of baseline (the lone first-crawl Slack exception).
			// canonical/title also open (suppressed on first crawl).
			wantFindings: map[string]model.Severity{
				"status_regression":        model.SeverityCritical,
				"canonical_changed":        model.SeverityCritical,
				"title_changed":            model.SeverityWarning,
				"meta_description_changed": model.SeverityWarning,
			},
		},
		{
			name: "article_headline_edited_title_changed", siteType: st, class: typeNoise,
			old: healthy,
			nw:  setTitle(healthy, "Mayor Resigns Following Corruption Probe — CityNews"),
			// NOISE(overlay-TODO): title VALUE-change pages today; overlay routes /article to digest.
			wantFindings: map[string]model.Severity{"title_changed": model.SeverityWarning},
		},
		{
			name: "article_meta_description_edited", siteType: st, class: typeNoise,
			old: healthy,
			nw:  setMeta(healthy, "Mayor steps down after a six-month corruption investigation, sources say."),
			// NOISE(overlay-TODO): meta value-change pages today; overlay routes to digest.
			wantFindings: map[string]model.Severity{"meta_description_changed": model.SeverityWarning},
		},
		{
			name: "article_body_substantive_rewrite", siteType: st, class: typeNoise,
			old: withContent(healthy, "h-base", 0x1111111111111111),
			nw:  withContent(healthy, "h-rewrite", 0xFFFF0000FFFF0000), // hamming 32 > 4
			// NOISE(overlay-TODO): substantive content pages (warning) today; overlay relaxes field:content on /article.
			wantFindings:    nil,
			wantSubstantive: []string{"content"},
		},
		{
			name: "article_cosmetic_byline_timestamp_churn", siteType: st, class: mustStayQuiet,
			old: withContent(healthy, "h-base", 0x00000000000000FF),
			nw:  withContent(healthy, "h-tick", 0x00000000000000F7), // hamming 1 <= 4 => cosmetic
			// content change is cosmetic => suppressed; no rule. Silent.
			wantFindings:    nil,
			wantSubstantive: []string{}, // no SUBSTANTIVE change (content is cosmetic)
		},
		{
			name: "article_goes_noindex_triad", siteType: st, class: mustFire,
			old: healthy,
			nw: func() model.Snapshot {
				s := healthy
				s.Indexable = false
				s.MetaRobots = "noindex"
				s.IndexabilityReason = "meta_robots_noindex"
				return s
			}(),
			// Pure path: BOTH critical findings. Scheduler collapses to one 'indexable' page.
			wantFindings: map[string]model.Severity{
				"indexability_flip":   model.SeverityCritical,
				"meta_robots_noindex": model.SeverityCritical,
			},
		},
		{
			name: "article_x_robots_noindex_header", siteType: st, class: mustFire,
			old: healthy,
			nw: func() model.Snapshot {
				s := healthy
				s.XRobotsTag = "noindex"
				s.Indexable = false
				s.IndexabilityReason = "x_robots_tag_noindex"
				return s
			}(),
			wantFindings: map[string]model.Severity{
				"indexability_flip":   model.SeverityCritical,
				"meta_robots_noindex": model.SeverityCritical, // IsNoindex(XRobotsTag) arm
			},
		},
		{
			name: "article_noindex_recovers", siteType: st, class: mustStayQuiet,
			old: func() model.Snapshot {
				s := healthy
				s.ID = 1
				s.Indexable = false
				s.MetaRobots = "noindex"
				s.IndexabilityReason = "meta_robots_noindex"
				return s
			}(),
			nw: healthy, // indexable true, meta_robots empty
			// Recovery: both rules pass. The meta_robots/indexable diffs are recovery-
			// direction; pure path emits no FINDING. (Change-stream events exist but
			// represent resolution; not asserted here.)
			wantFindings: nil,
		},
		{
			name: "article_404_after_unpublish", siteType: st, class: mustFire,
			old: healthy,
			nw: func() model.Snapshot {
				s := healthy
				s.HTTPStatus = 404
				s.Indexable = false
				return s
			}(),
			// 4xx arm fires (prior 200 baseline). indexability_flip ALSO fires (was indexable).
			wantFindings: map[string]model.Severity{
				"status_regression": model.SeverityCritical,
				"indexability_flip": model.SeverityCritical,
			},
		},
		{
			name: "article_500_origin_error_steady_baseline", siteType: st, class: mustFire,
			old: healthy,
			nw:  setStatus(healthy, 500),
			// 5xx always fires. indexable stays true; only status.
			wantFindings: map[string]model.Severity{"status_regression": model.SeverityCritical},
		},
		{
			name: "article_canonical_repointed_to_amp_or_hub", siteType: st, class: mustFire,
			old: setCanonical(healthy, "https://news.example/story-123"),
			nw:  setCanonical(healthy, "https://news.example/amp/story-123"),
			// changed arm (non-empty, canonical Change present) => critical.
			wantFindings: map[string]model.Severity{"canonical_changed": model.SeverityCritical},
		},
		{
			name: "article_canonical_tag_dropped", siteType: st, class: mustFire,
			old: setCanonical(healthy, "https://news.example/story-123"),
			nw:  setCanonical(healthy, ""),
			// missing arm => critical (Old.ID!=0 so pages).
			wantFindings: map[string]model.Severity{"canonical_changed": model.SeverityCritical},
		},
		{
			name: "article_loses_headline_richresult_eligibility", siteType: st, class: mustFire,
			old: withJSONLD(healthy, `[{"@type":"Article","headline":"Mayor Resigns","datePublished":"2026-01-01"}]`, "Article"),
			nw:  withJSONLD(healthy, `[{"@type":"Article","datePublished":"2026-01-01"}]`, "Article"),
			// Lost-eligibility flip: Old had headline, New has none, schema_types unchanged.
			wantFindings: map[string]model.Severity{"rich_result_article": model.SeverityCritical},
		},
		{
			name: "article_schema_block_fully_removed", siteType: st, class: typeNoise,
			old: withJSONLD(healthy, `[{"@type":"Article","headline":"X"}]`, "Article"),
			nw:  withJSONLD(healthy, ``, ""),
			// rich_result_article PASSES (type absent). schema_types diff => warning change-stream.
			// NOISE(overlay-TODO): schema set-churn can be noisy on publishers.
			wantFindings:    nil,
			wantSubstantive: []string{"schema_types"},
		},
		{
			name: "article_invalid_jsonld_block", siteType: st, class: mustFire,
			old:          func() model.Snapshot { s := healthy; s.JSONLDInvalidCount = 0; return s }(),
			nw:           func() model.Snapshot { s := healthy; s.JSONLDInvalidCount = 1; return s }(),
			wantFindings: map[string]model.Severity{"structured_data_invalid_json": model.SeverityWarning},
		},
		{
			name: "article_truncated_body_suppresses_jsonld_rules", siteType: st, class: mustStayQuiet,
			old:       func() model.Snapshot { s := healthy; s.JSONLDInvalidCount = 0; return s }(),
			nw:        func() model.Snapshot { s := healthy; s.JSONLDInvalidCount = 1; return s }(),
			truncated: true,
			// Truncated => the 4 JSON-LD rules self-suppress. Head-derived fields unchanged. Silent.
			wantFindings: nil,
		},
		{
			name: "article_multiple_h1_from_widget", siteType: st, class: typeNoise,
			old: setHeadings(healthy, `{"h1":["Mayor Resigns"]}`),
			nw:  setHeadings(healthy, `{"h1":["Mayor Resigns","You may also like"]}`),
			// FIXED (#h1-rewrite): the headings SET changed (a "You may also like" H1 appeared),
			// so the genuine-rewrite WARNING arm fires ABOVE the count switch. Previously the
			// 'multiple' branch returned BEFORE the changed branch and a real rewrite on a
			// multi-H1 page was silently downgraded to INFO (never paged).
			// NOTE: the headings diff itself emits a substantive change-stream event.
			wantFindings:    map[string]model.Severity{"h1_issue": model.SeverityWarning},
			wantSubstantive: []string{"headings"},
		},
		{
			name: "article_h1_missing_template_break", siteType: st, class: mustFire,
			old: setHeadings(healthy, `{"h1":["Mayor Resigns"],"h2":["Subhead"]}`),
			nw:  setHeadings(healthy, `{"h2":["Subhead"]}`),
			// 0 h1 => missing warning. headings diff also emitted.
			wantFindings:    map[string]model.Severity{"h1_issue": model.SeverityWarning},
			wantSubstantive: []string{"headings"},
		},
		{
			name: "article_h1_text_edited_only", siteType: st, class: typeNoise,
			old: setHeadings(healthy, `{"h1":["Mayor Resigns"]}`),
			nw:  setHeadings(healthy, `{"h1":["Mayor Resigns After Probe"]}`),
			// 1 h1 + headings change => changed warning.
			// NOISE(overlay-TODO): rides every headline edit on a publisher.
			wantFindings:    map[string]model.Severity{"h1_issue": model.SeverityWarning},
			wantSubstantive: []string{"headings"},
		},
		{
			name: "article_empty_headings_first_extract", siteType: st, class: mustStayQuiet,
			old: setHeadings(healthy, ""),
			nw:  setHeadings(healthy, ""),
			// Empty Headings => h1_issue emits nothing (don't-guess). No diff. Silent.
			wantFindings: nil,
		},
		{
			name: "article_title_edited_into_pixel_overflow", siteType: st, class: mustFire,
			old: setTitle(healthy, "Mayor Resigns"),
			nw:  setTitle(healthy, "City Mayor Abruptly Resigns Following a Lengthy Corruption Investigation by Federal Authorities"), // 852.6px > 580
			// title_changed (warning) + title_pixel_overflow (warning). Push gate pages overflow because title changed.
			wantFindings: map[string]model.Severity{
				"title_changed":        model.SeverityWarning,
				"title_pixel_overflow": model.SeverityWarning,
			},
		},
		{
			name: "article_preexisting_long_title_unchanged", siteType: st, class: mustStayQuiet,
			old: setTitle(withContent(healthy, "h-base", 0x55), "City Mayor Abruptly Resigns Following a Lengthy Corruption Investigation by Federal Authorities"),
			nw:  setTitle(withContent(healthy, "h-base2", 0x57), "City Mayor Abruptly Resigns Following a Lengthy Corruption Investigation by Federal Authorities"),
			// title_pixel_overflow OPENS (width>580) but A3 push gate suppresses Slack (title unchanged).
			// Only a cosmetic content tweak (hamming 1). So pure path shows the overflow FINDING.
			wantFindings:    map[string]model.Severity{"title_pixel_overflow": model.SeverityWarning},
			wantSubstantive: []string{}, // content cosmetic
		},
		{
			name: "article_internal_links_collapse_nav_break", siteType: st, class: mustFire,
			old: setInternalLinks(healthy, 120),
			nw:  setInternalLinks(healthy, 30), // 75% drop
			// broken_links_spike warning. internal_link_count change is substantive but
			// raises no standalone change-stream alert (the rule bridge owns it).
			wantFindings: map[string]model.Severity{"broken_links_spike": model.SeverityWarning},
		},
		{
			name: "article_internal_links_minor_fluctuation", siteType: st, class: mustStayQuiet,
			old:          setInternalLinks(healthy, 120),
			nw:           setInternalLinks(healthy, 108), // 10% drop < 30%
			wantFindings: nil,
		},
		{
			name: "article_external_links_editorial_jump", siteType: st, class: typeNoise,
			old: setExternalLinks(healthy, 8),
			nw:  setExternalLinks(healthy, 40), // jump 32>=10 AND 40>=2*8
			// NOISE(overlay-TODO): editorial citation jumps relax for publishers.
			wantFindings: map[string]model.Severity{"external_link_spike": model.SeverityWarning},
		},
		{
			name: "article_external_links_small_change", siteType: st, class: mustStayQuiet,
			old:          setExternalLinks(healthy, 1),
			nw:           setExternalLinks(healthy, 3), // jump 2 < abs floor 10
			wantFindings: nil,
		},
		{
			name: "article_image_gallery_alt_volume", siteType: st, class: typeNoise,
			old: setImages(healthy, 20, 9),
			nw:  setImages(healthy, 20, 9), // coverage 0.55 < 0.80
			// image_alt_missing INFO (steady, no first-crawl guard); never pages. No regression (count equal).
			// NOISE(overlay-TODO): low alt coverage endemic on photo galleries.
			wantFindings: map[string]model.Severity{"image_alt_missing": model.SeverityInfo},
		},
		{
			name: "article_alt_coverage_regression", siteType: st, class: typeNoise,
			old: setImages(healthy, 20, 3),
			nw:  setImages(healthy, 20, 11), // increase 3->11; coverage 0.45<0.80
			// image_alt_regression warning (increase) + image_alt_missing info (coverage<0.80).
			// NOISE(overlay-TODO): image-heavy publisher alt churn.
			wantFindings: map[string]model.Severity{
				"image_alt_regression": model.SeverityWarning,
				"image_alt_missing":    model.SeverityInfo,
			},
		},
		{
			name: "article_alt_fix_lowers_missing_count", siteType: st, class: mustStayQuiet,
			old: setImages(healthy, 20, 11),
			nw:  setImages(healthy, 20, 2), // decrease; coverage 0.9>=0.8
			// regression is increase-only (no fire on fix); missing passes (coverage ok).
			wantFindings: nil,
		},
		{
			name: "article_url_redirect_chain_grows", siteType: st, class: mustFire,
			old: setRedirect(healthy, `["http://news.example/x","https://news.example/x"]`),
			nw:  setRedirect(healthy, `["http://news.example/x","https://news.example/x","https://news.example/articles/x"]`),
			// depth 1 -> 2, no loop.
			wantFindings: map[string]model.Severity{"redirect_chain_growth": model.SeverityWarning},
		},
		{
			name: "article_redirect_loop", siteType: st, class: mustFire,
			old: setRedirect(healthy, `["https://news.example/a","https://news.example/b"]`),
			nw:  setRedirect(healthy, `["https://news.example/a","https://news.example/b","https://news.example/a"]`),
			// redirect_loop critical; redirect_chain_growth yields (silent).
			wantFindings: map[string]model.Severity{"redirect_loop": model.SeverityCritical},
		},
		{
			name: "article_garbage_redirect_chain_field", siteType: st, class: mustStayQuiet,
			old: setRedirect(healthy, `["https://news.example/a"]`),
			nw:  setRedirect(healthy, `{"x":1}`), // not an array
			// RedirectChainInfo ok=false on both rules => no finding. redirect_chain diff
			// emits a substantive change but raises no standalone change-stream alert.
			wantFindings: nil,
		},
		{
			name: "article_renders_as_client_shell_spa_migration", siteType: st, class: mustFire,
			old: setRender(healthy, model.RenderServerRendered),
			nw:  setRender(healthy, model.RenderClientShell),
			// needs_rendering warning. render_mode diff is ingest-skipped (no standalone alert).
			wantFindings: map[string]model.Severity{"needs_rendering": model.SeverityWarning},
		},
		{
			name: "article_render_mode_recovers_to_hydrated", siteType: st, class: mustStayQuiet,
			old: setRender(healthy, model.RenderClientShell),
			nw:  setRender(healthy, model.RenderHydrated),
			// needs_rendering passes on hydrated => issue closes; no new finding.
			wantFindings: nil,
		},
		{
			name: "article_render_mode_unknown_steady", siteType: st, class: mustStayQuiet,
			old:          setRender(healthy, model.RenderUnknown),
			nw:           setRender(healthy, model.RenderUnknown),
			wantFindings: nil,
		},
		{
			name: "article_hreflang_set_changes_monolingual_noise", siteType: st, class: typeNoise,
			old: setHreflang(healthy, `["en"]`),
			nw:  setHreflang(healthy, `["en","x-default"]`),
			// hreflang_invalid WARNING (rule). Change-stream event for hreflang routes WARNING too (FIXED #16: agrees with the rule).
			// NOISE(overlay-TODO): monolingual hreflang dead weight; overlay gates OFF.
			wantFindings:    map[string]model.Severity{"hreflang_invalid": model.SeverityWarning},
			wantSubstantive: []string{"hreflang"},
		},
		{
			name: "article_title_whitespace_only_to_real", siteType: st, class: edge,
			old: setTitle(healthy, "   "),
			nw:  setTitle(healthy, "Mayor Resigns — CityNews"),
			// Recovery from blank: changed arm fires (whitespace->real is a 'title' Change).
			wantFindings: map[string]model.Severity{"title_changed": model.SeverityWarning},
		},
		{
			name: "article_title_none_robots_token", siteType: st, class: mustFire,
			old: healthy,
			nw: func() model.Snapshot {
				s := healthy
				s.MetaRobots = "none"
				s.Indexable = false
				s.IndexabilityReason = "meta_robots_noindex"
				return s
			}(),
			// 'none' treated as noindex by IsNoindex.
			wantFindings: map[string]model.Severity{
				"indexability_flip":   model.SeverityCritical,
				"meta_robots_noindex": model.SeverityCritical,
			},
		},
		{
			name: "article_nofollow_only_not_noindex", siteType: st, class: edge,
			old: healthy,
			nw:  setMetaRobots(healthy, "index,nofollow"), // still indexable
			// meta_robots_noindex PASSES (nofollow != noindex). No rule fires.
			// BUT meta_robots change-stream event routes CRITICAL (no indexable flip => no triad collapse).
			wantFindings:    nil,
			wantSubstantive: []string{"meta_robots"},
		},
		{
			name: "article_schema_types_set_grows_recipe_widget", siteType: st, class: typeNoise,
			old: withJSONLD(healthy, `[{"@type":"Article","headline":"Mayor Resigns"}]`, "Article"),
			nw:  withJSONLD(healthy, `[{"@type":"Article","headline":"Mayor Resigns"},{"@type":"VideoObject"}]`, "Article,VideoObject"),
			// schema_types set grows => warning change-stream. Article still eligible => rich passes.
			// NOISE(overlay-TODO): benign schema enrichment.
			wantFindings:    nil,
			wantSubstantive: []string{"schema_types"},
		},
		{
			name: "article_word_count_only_change", siteType: st, class: mustStayQuiet,
			old: func() model.Snapshot { s := withContent(healthy, "h-a", 0x0F0F); s.WordCount = 800; return s }(),
			nw:  func() model.Snapshot { s := withContent(healthy, "h-b", 0x0F0E); s.WordCount = 780; return s }(), // content hamming 1 cosmetic
			// word_count cosmetic + cosmetic content => both suppressed. Silent.
			wantFindings:    nil,
			wantSubstantive: []string{},
		},
		{
			name: "article_content_simhash_zero_forces_substantive", siteType: st, class: edge,
			old: withContent(healthy, "old", 0),      // unknown simhash
			nw:  withContent(healthy, "new", 0xABCD), // real
			// zero on either side => forced substantive content change-stream (warning).
			wantFindings:    nil,
			wantSubstantive: []string{"content"},
		},
		{
			name: "article_steady_state_no_change", siteType: st, class: mustStayQuiet,
			old: healthy,
			nw:  func() model.Snapshot { s := healthy; s.ID = 0; s.URLID = 7; return s }(), // identical diffed fields, new not yet persisted (ID 0 ok since old.ID!=0)
			// No diffs; all rules pass. Silent.
			wantFindings:    nil,
			wantSubstantive: []string{},
		},
		{
			name: "article_simultaneous_real_regression_plus_noise", siteType: st, class: mustFire,
			old: withContent(healthy, "h-base", 0x1111111111111111),
			nw: func() model.Snapshot {
				s := withContent(healthy, "h-new", 0xFFFF0000FFFF0000) // hamming 32 substantive
				s.Indexable = false
				s.MetaRobots = "noindex"
				s.IndexabilityReason = "meta_robots_noindex"
				s.Title = "New"
				return s
			}(),
			// critical noindex (2 findings, collapse to 1 page) + title_changed warning + content substantive.
			wantFindings: map[string]model.Severity{
				"indexability_flip":   model.SeverityCritical,
				"meta_robots_noindex": model.SeverityCritical,
				"title_changed":       model.SeverityWarning,
			},
			wantSubstantive: []string{"content", "indexability_reason", "indexable", "meta_robots", "title"},
		},
	}
}
