package behavior

import "github.com/roberto-grasiano/rabbot-seo/internal/model"

// saasScenarios encodes the SaaS/corporate/brochure rows of the scenario matrix.
// Same pure-path semantics as publisherScenarios (see that file's header).
func saasScenarios() []scenario {
	const st = "saas/corporate"
	// Healthy hydrated money page (Product+BreadcrumbList eligible).
	page := model.Snapshot{
		ID: 42, URLID: 7, HTTPStatus: 200, Indexable: true,
		Title:           "Pricing | Acme",
		MetaDescription: "Acme helps modern software teams plan projects, track work, and automate billing in one place.",
		Canonical:       "https://acme.io/pricing", MetaRobots: "index,follow",
		Headings:   `{"h1":["Pricing"]}`,
		RenderMode: model.RenderHydrated, InternalLinkCount: 60,
		SchemaTypes:   "BreadcrumbList,Product",
		ContentSHA256: "saas-base", ContentSimhash: 0x1111111111111111,
		JSONLD: `[{"@type":"Product","name":"Acme","offers":{"price":"49","priceCurrency":"USD"}},{"@type":"BreadcrumbList","itemListElement":[{"position":1,"name":"Home"}]}]`,
	}

	return []scenario{
		{
			name: "cold_start_hydrated_spa_money_page", siteType: st, class: mustStayQuiet,
			old:          firstCrawlBaseline,
			nw:           func() model.Snapshot { s := page; s.ID = 0; return s }(),
			wantFindings: nil,
		},
		{
			name: "cold_start_client_shell_spa", siteType: st, class: typeNoise,
			old: firstCrawlBaseline,
			nw: model.Snapshot{
				URLID: 7, HTTPStatus: 200, Title: "Acme App", Canonical: "https://acme.io/app/dashboard",
				Indexable: true, MetaRobots: "", MetaDescription: "", Headings: "",
				RenderMode: model.RenderClientShell, ContentSimhash: 0,
			},
			// needs_rendering OPENS (client_shell, no guard); meta absent => meta_description_changed.
			// canonical present (passes). All first-crawl Slack-suppressed.
			// NOISE(overlay-TODO): known-SPA needs_rendering down-tiers to info.
			wantFindings: map[string]model.Severity{
				"needs_rendering":          model.SeverityWarning,
				"meta_description_changed": model.SeverityWarning,
			},
		},
		{
			name: "steady_state_spa_hydrated_no_change", siteType: st, class: mustStayQuiet,
			old:             page,
			nw:              func() model.Snapshot { s := page; s.ID = 0; return s }(),
			wantFindings:    nil,
			wantSubstantive: []string{},
		},
		{
			name: "steady_state_client_shell_recurring", siteType: st, class: typeNoise,
			old: setRender(page, model.RenderClientShell),
			nw:  func() model.Snapshot { s := page; s.ID = 0; s.RenderMode = model.RenderClientShell; return s }(),
			// needs_rendering RE-FAILS every crawl (steady, no guard); render_mode unchanged.
			// NOISE(overlay-TODO): biggest SaaS trust-eroder; overlay down-tiers to info.
			wantFindings:    map[string]model.Severity{"needs_rendering": model.SeverityWarning},
			wantSubstantive: []string{},
		},
		{
			name: "money_page_500_outage", siteType: st, class: mustFire,
			old: page,
			nw: func() model.Snapshot {
				s := page
				s.HTTPStatus = 500
				s.Title = ""
				s.Canonical = ""
				s.MetaDescription = ""
				return s
			}(),
			// 5xx + absent head fields.
			wantFindings: map[string]model.Severity{
				"status_regression":        model.SeverityCritical,
				"canonical_changed":        model.SeverityCritical,
				"title_changed":            model.SeverityWarning,
				"meta_description_changed": model.SeverityWarning,
			},
		},
		{
			name: "money_page_404_from_200", siteType: st, class: mustFire,
			old: page,
			nw: func() model.Snapshot {
				s := page
				s.HTTPStatus = 404
				s.Title = ""
				s.Canonical = ""
				s.MetaDescription = ""
				return s
			}(),
			wantFindings: map[string]model.Severity{
				"status_regression":        model.SeverityCritical,
				"canonical_changed":        model.SeverityCritical,
				"title_changed":            model.SeverityWarning,
				"meta_description_changed": model.SeverityWarning,
			},
		},
		{
			name: "404_was_already_404_steady", siteType: st, class: mustStayQuiet,
			old: func() model.Snapshot {
				s := page
				s.HTTPStatus = 404
				s.Title = ""
				s.Canonical = ""
				s.MetaDescription = ""
				s.Headings = ""
				return s
			}(),
			nw: func() model.Snapshot {
				s := page
				s.ID = 0
				s.HTTPStatus = 404
				s.Title = ""
				s.Canonical = ""
				s.MetaDescription = ""
				s.Headings = ""
				return s
			}(),
			// FIXED (#steady-4xx): a steady 4xx (Old & New both >= 400) now keeps status_regression
			// OPEN at WARNING so the broken page stays an open issue across rechecks and auto-CLOSES
			// via the engine lifecycle when the page recovers (404->200). canonical/title/meta also
			// stay absent => they re-fail but were already open. Marked must_stay_quiet on the
			// NEW-page axis (no NEW alert: 404->404 is no change); the pure path shows the open issues.
			wantFindings: map[string]model.Severity{
				"status_regression":        model.SeverityWarning,
				"canonical_changed":        model.SeverityCritical,
				"title_changed":            model.SeverityWarning,
				"meta_description_changed": model.SeverityWarning,
			},
		},
		{
			name: "first_crawl_4xx_no_baseline", siteType: st, class: edge,
			old: firstCrawlBaseline,
			nw:  model.Snapshot{URLID: 7, HTTPStatus: 404},
			// FIXED (#born-4xx): a born-4xx (Old.ID==0) now OPENS status_regression at WARNING so the
			// broken page is a visible open issue (no 2xx/3xx baseline => the CRITICAL arm can't fire).
			// The missing-field rules also open (canonical critical, title/meta absent). All
			// Slack-suppressed on the first crawl by ProcessFetch's first-crawl guard; the pure path
			// shows the open issues here.
			wantFindings: map[string]model.Severity{
				"status_regression":        model.SeverityWarning,
				"canonical_changed":        model.SeverityCritical,
				"title_changed":            model.SeverityWarning,
				"meta_description_changed": model.SeverityWarning,
			},
		},
		{
			name: "noindex_flip_on_money_page", siteType: st, class: mustFire,
			old: page,
			nw: func() model.Snapshot {
				s := page
				s.Indexable = false
				s.MetaRobots = "noindex,follow"
				s.IndexabilityReason = "meta_robots_noindex"
				return s
			}(),
			wantFindings: map[string]model.Severity{
				"indexability_flip":   model.SeverityCritical,
				"meta_robots_noindex": model.SeverityCritical,
			},
		},
		{
			name: "x_robots_header_noindex", siteType: st, class: mustFire,
			old: page,
			nw: func() model.Snapshot {
				s := page
				s.Indexable = false
				s.XRobotsTag = "noindex"
				s.IndexabilityReason = "x_robots_noindex"
				return s
			}(),
			wantFindings: map[string]model.Severity{
				"indexability_flip":   model.SeverityCritical,
				"meta_robots_noindex": model.SeverityCritical,
			},
		},
		{
			name: "login_page_steady_noindex", siteType: st, class: typeNoise,
			old: func() model.Snapshot {
				s := page
				s.Indexable = false
				s.MetaRobots = "noindex,follow"
				s.IndexabilityReason = "meta_robots_noindex"
				s.Canonical = "https://acme.io/login"
				return s
			}(),
			nw: func() model.Snapshot {
				s := page
				s.ID = 0
				s.Indexable = false
				s.MetaRobots = "noindex,follow"
				s.IndexabilityReason = "meta_robots_noindex"
				s.Canonical = "https://acme.io/login"
				return s
			}(),
			// meta_robots_noindex RE-FAILS (steady, no guard); indexability_flip passes (Old not indexable).
			// NOISE(overlay-TODO): intentional /login noindex; overlay routes to digest.
			wantFindings:    map[string]model.Severity{"meta_robots_noindex": model.SeverityCritical},
			wantSubstantive: []string{},
		},
		{
			name: "noindex_recovery_closes_issue", siteType: st, class: mustStayQuiet,
			old: func() model.Snapshot {
				s := page
				s.Indexable = false
				s.MetaRobots = "noindex,follow"
				s.IndexabilityReason = "meta_robots_noindex"
				return s
			}(),
			nw: page, // indexable true, index,follow
			// Recovery: both rules pass; no FINDING.
			wantFindings: nil,
		},
		{
			name: "canonical_changed_on_consolidation", siteType: st, class: mustFire,
			old:          setCanonical(page, "https://acme.io/lp/trial"),
			nw:           setCanonical(page, "https://acme.io/signup"),
			wantFindings: map[string]model.Severity{"canonical_changed": model.SeverityCritical},
		},
		{
			name: "title_value_change_marketing_edit", siteType: st, class: typeNoise,
			old: setTitle(page, "Features | Acme"),
			nw:  setTitle(page, "Product Features for Modern Teams | Acme"), // 390px fits
			// NOISE(overlay-TODO): marketing title value-change.
			wantFindings: map[string]model.Severity{"title_changed": model.SeverityWarning},
		},
		{
			name: "title_pixel_overflow_on_new_title", siteType: st, class: mustFire,
			old: setTitle(page, "Pricing | Acme"),
			nw:  setTitle(page, "Acme — The All-in-One Project Management, CRM, and Billing Platform for Teams"), // 736.9px
			wantFindings: map[string]model.Severity{
				"title_changed":        model.SeverityWarning,
				"title_pixel_overflow": model.SeverityWarning,
			},
		},
		{
			name: "pre_existing_long_title_no_change", siteType: st, class: mustStayQuiet,
			old: setTitle(withContent(page, "saas-base", 5), "Acme — The All-in-One Project Management, CRM, and Billing Platform for Teams"),
			nw:  setTitle(withContent(page, "saas-base", 5), "Acme — The All-in-One Project Management, CRM, and Billing Platform for Teams"),
			// overflow OPENS but push gate suppresses Slack (title unchanged). No content change.
			wantFindings:    map[string]model.Severity{"title_pixel_overflow": model.SeverityWarning},
			wantSubstantive: []string{},
		},
		{
			name: "meta_description_pixel_overflow_new", siteType: st, class: edge,
			old: setMeta(page, "Acme helps teams ship faster."),
			nw:  setMeta(page, "Acme is the all-in-one platform that helps modern software teams plan projects, track work, manage customer relationships, automate billing, and report on everything in one unified workspace built for scale."), // 1274.6px
			// EDGE: 920px budget calibration caveat (memory 8558). This case is unambiguously over.
			wantFindings: map[string]model.Severity{
				"meta_description_changed":        model.SeverityWarning,
				"meta_description_pixel_overflow": model.SeverityWarning,
			},
		},
		{
			name: "meta_description_borderline_calibration", siteType: st, class: edge,
			old: setMeta(page, "short desc"),
			nw:  setMeta(page, "A roughly one hundred fifty character marketing description that renders somewhere around nine hundred thirty to nine hundred eighty pixels wide overall."),
			// EDGE — calibration over-fire (memory 8558): this description measures 952.5px, OVER Rabbot's
			// conservative 920px reference but UNDER the 990px figure many tools cite — so it would NOT truncate
			// in many real SERPs, yet meta_description_pixel_overflow DOES fire. This deterministically
			// demonstrates the 920px@14px false-positive surface. The meta also CHANGED so meta_description_changed
			// fires too. Both warnings are pinned; whether the OVERFLOW one is a "true" regression is the open
			// calibration question the owner should weigh (F4 threshold work).
			wantFindings: map[string]model.Severity{
				"meta_description_changed":        model.SeverityWarning,
				"meta_description_pixel_overflow": model.SeverityWarning,
			},
		},
		{
			name: "hreflang_introduced_monolingual_to_intl", siteType: st, class: typeNoise,
			old: setHreflang(page, ""),
			nw:  setHreflang(page, `["en","de"]`),
			// hreflang_invalid WARNING; change-stream routes WARNING too (FIXED #16: agrees with the rule).
			// NOISE(overlay-TODO): SaaS gates hreflang OFF.
			wantFindings:    map[string]model.Severity{"hreflang_invalid": model.SeverityWarning},
			wantSubstantive: []string{"hreflang"},
		},
		{
			name: "multiple_h1_component_page", siteType: st, class: typeNoise,
			old: setHeadings(page, `{"h1":["Build faster"],"h2":["x"]}`),
			nw:  setHeadings(page, `{"h1":["Build faster","Ship with confidence"],"h2":["x"]}`),
			// FIXED (#h1-rewrite): the headings SET changed (a 2nd H1 appeared), so the
			// genuine-rewrite WARNING arm fires ABOVE the count switch (no longer silently
			// downgraded to the INFO "multiple" finding that never pages). headings diff emitted.
			wantFindings:    map[string]model.Severity{"h1_issue": model.SeverityWarning},
			wantSubstantive: []string{"headings"},
		},
		{
			name: "h1_disappears_broken_template", siteType: st, class: mustFire,
			old:             setHeadings(page, `{"h1":["Features"],"h2":["Plans"]}`),
			nw:              setHeadings(page, `{"h1":[],"h2":["Plans"]}`),
			wantFindings:    map[string]model.Severity{"h1_issue": model.SeverityWarning},
			wantSubstantive: []string{"headings"},
		},
		{
			name: "h1_text_change_only", siteType: st, class: typeNoise,
			old: setHeadings(withContent(page, "p", 3), `{"h1":["Features"]}`),
			nw:  setHeadings(withContent(page, "q", 4), `{"h1":["Product Features"]}`), // content hamming(3,4)=3 cosmetic
			// h1 changed warning; content cosmetic (suppressed).
			wantFindings:    map[string]model.Severity{"h1_issue": model.SeverityWarning},
			wantSubstantive: []string{"headings"},
		},
		{
			name: "content_rewrite_substantive", siteType: st, class: typeNoise,
			old: withContent(page, "old", 0x00000000000000FF),
			nw:  withContent(page, "new", 0xFFFFFF00000000FF), // hamming 24 substantive
			// NOISE(overlay-TODO): pricing rewrite is intentional.
			wantFindings:    nil,
			wantSubstantive: []string{"content"},
		},
		{
			name: "content_cosmetic_churn", siteType: st, class: mustStayQuiet,
			old:             withContent(page, "old", 0x0F0F0F0F0F0F0F0F),
			nw:              withContent(page, "new", 0x0F0F0F0F0F0F0F0D), // hamming 1 cosmetic
			wantFindings:    nil,
			wantSubstantive: []string{},
		},
		{
			name: "content_change_zero_simhash_forced_substantive", siteType: st, class: edge,
			old:             withContent(page, "old", 0),
			nw:              withContent(page, "new", 12345),
			wantFindings:    nil,
			wantSubstantive: []string{"content"},
		},
		{
			name: "internal_link_collapse_nav_regression", siteType: st, class: mustFire,
			old:             setInternalLinks(withContent(page, "a", 0x0F0F), 120),
			nw:              setInternalLinks(withContent(page, "b", 0xF0F0), 30), // 75% drop + substantive content (hamming 16)
			wantFindings:    map[string]model.Severity{"broken_links_spike": model.SeverityWarning},
			wantSubstantive: []string{"content", "internal_link_count"},
		},
		{
			name: "internal_link_minor_fluctuation", siteType: st, class: mustStayQuiet,
			old: setInternalLinks(page, 100),
			nw:  setInternalLinks(page, 92), // 8% drop
			// internal_link_count substantive change but no standalone alert; broken_links_spike passes.
			wantFindings:    nil,
			wantSubstantive: []string{"internal_link_count"},
		},
		{
			name: "internal_links_zero_to_zero_baseline_guard", siteType: st, class: mustStayQuiet,
			old: setInternalLinks(page, 0),
			nw:  func() model.Snapshot { s := page; s.ID = 0; s.InternalLinkCount = 0; return s }(),
			// div-by-zero guard; no change. Silent.
			wantFindings:    nil,
			wantSubstantive: []string{},
		},
		{
			name: "external_link_injection_hacked", siteType: st, class: mustFire,
			old:             setExternalLinks(withContent(page, "a", 1), 3),
			nw:              setExternalLinks(withContent(page, "z", 0xDEAD), 45), // jump 42>=10 AND 45>=2*3 + substantive content
			wantFindings:    map[string]model.Severity{"external_link_spike": model.SeverityWarning},
			wantSubstantive: []string{"content"},
		},
		{
			name: "external_link_small_increase_below_floor", siteType: st, class: mustStayQuiet,
			old:          setExternalLinks(page, 4),
			nw:           setExternalLinks(page, 9), // jump 5 < abs floor 10
			wantFindings: nil,
		},
		{
			name: "sd_eligibility_loss_breadcrumb", siteType: st, class: mustFire,
			old:          withJSONLD(page, `[{"@type":"BreadcrumbList","itemListElement":[{"position":1}]}]`, "BreadcrumbList"),
			nw:           withJSONLD(page, `[{"@type":"BreadcrumbList"}]`, "BreadcrumbList"),
			wantFindings: map[string]model.Severity{"rich_result_breadcrumb": model.SeverityCritical},
		},
		{
			name: "sd_eligibility_loss_product_offers_dropped", siteType: st, class: mustFire,
			old:          withJSONLD(page, `[{"@type":"Product","name":"Acme","offers":{"price":"49"}}]`, "Product"),
			nw:           withJSONLD(page, `[{"@type":"Product","name":"Acme"}]`, "Product"),
			wantFindings: map[string]model.Severity{"rich_result_product": model.SeverityCritical},
		},
		{
			name: "softwareapplication_eligibility_loss_uncovered_gap", siteType: st, class: edge,
			old: withJSONLD(page, `[{"@type":"SoftwareApplication","name":"Acme","offers":{"price":"49"},"aggregateRating":{"ratingValue":"4.5"}}]`, "SoftwareApplication"),
			nw:  withJSONLD(page, `[{"@type":"SoftwareApplication","name":"Acme"}]`, "SoftwareApplication"),
			// SoftwareApplication UNPROFILED => all rich rules PASS; schema_types unchanged => no generic alert.
			// MISS by design (SoftwareApplication profile is the SaaS fast-follow).
			wantFindings:    nil,
			wantSubstantive: []string{},
		},
		{
			name: "schema_block_fully_removed", siteType: st, class: mustFire,
			old: withJSONLD(page, `[{"@type":"Product","name":"Acme","offers":{"price":"1"}},{"@type":"BreadcrumbList","itemListElement":[{"position":1}]}]`, "BreadcrumbList,Product"),
			nw:  withJSONLD(page, `[{"@type":"BreadcrumbList","itemListElement":[{"position":1}]}]`, "BreadcrumbList"),
			// schema_types warning change-stream; rich_result_product PASSES (absent).
			wantFindings:    nil,
			wantSubstantive: []string{"schema_types"},
		},
		{
			name: "invalid_jsonld_block", siteType: st, class: mustFire,
			old: func() model.Snapshot { s := page; s.JSONLDInvalidCount = 0; return s }(),
			nw:  func() model.Snapshot { s := withContent(page, "b", 50); s.JSONLDInvalidCount = 1; return s }(),
			// structured_data_invalid_json warning + substantive content (the broken markup changed body).
			wantFindings:    map[string]model.Severity{"structured_data_invalid_json": model.SeverityWarning},
			wantSubstantive: []string{"content"},
		},
		{
			name: "invalid_jsonld_but_truncated_body", siteType: st, class: edge,
			old:          func() model.Snapshot { s := page; s.JSONLDInvalidCount = 0; return s }(),
			nw:           func() model.Snapshot { s := page; s.JSONLDInvalidCount = 2; return s }(),
			truncated:    true,
			wantFindings: nil,
		},
		{
			name: "redirect_loop_within_cap", siteType: st, class: mustFire,
			old:          setRedirect(page, `["https://acme.io/pricing"]`),
			nw:           setRedirect(page, `["https://acme.io/pricing","https://acme.io/pricing/","https://acme.io/pricing"]`),
			wantFindings: map[string]model.Severity{"redirect_loop": model.SeverityCritical},
		},
		{
			name: "redirect_chain_growth_no_loop", siteType: st, class: mustFire,
			old:          setRedirect(page, `["http://acme.io/x","https://acme.io/x"]`),
			nw:           setRedirect(page, `["http://acme.io/x","https://acme.io/x","https://www.acme.io/x"]`),
			wantFindings: map[string]model.Severity{"redirect_chain_growth": model.SeverityWarning},
		},
		{
			name: "redirect_chain_unparseable_guard", siteType: st, class: mustStayQuiet,
			old: setRedirect(page, `["https://acme.io/a"]`),
			nw:  setRedirect(page, ``), // empty => ok=false
			// both redirect rules emit nothing; redirect_chain diff has no standalone alert.
			wantFindings: nil,
		},
		{
			name: "image_alt_regression_on_new_gallery", siteType: st, class: mustFire,
			old: setImages(withContent(page, "a", 0x0F0F), 4, 0),
			nw:  setImages(withContent(page, "b", 0xF0F0), 12, 6), // increase; coverage 0.5<0.80 + substantive content (hamming 16)
			wantFindings: map[string]model.Severity{
				"image_alt_regression": model.SeverityWarning,
				"image_alt_missing":    model.SeverityInfo,
			},
			wantSubstantive: []string{"content"},
		},
		{
			name: "image_alt_fix_rebaseline_no_fire", siteType: st, class: mustStayQuiet,
			old:          setImages(page, 12, 6),
			nw:           setImages(page, 12, 0), // decrease; coverage 1.0
			wantFindings: nil,
		},
		{
			name: "hydrated_to_client_shell_render_regression", siteType: st, class: mustFire,
			old: setRender(withContent(page, "a", 30), model.RenderHydrated),
			nw:  setRender(withContent(page, "b", 0), model.RenderClientShell), // new simhash 0 forces substantive
			// needs_rendering warning + forced substantive content (body collapsed). render_mode is ALSO a
			// substantive diff field (server-side it raises no standalone change-stream alert, but diff.Compare
			// emits it substantive — the rule bridge owns the alerting).
			wantFindings:    map[string]model.Severity{"needs_rendering": model.SeverityWarning},
			wantSubstantive: []string{"content", "render_mode"},
		},
		{
			name: "client_shell_to_hydrated_recovery", siteType: st, class: mustStayQuiet,
			old: setRender(withContent(page, "a", 0), model.RenderClientShell),
			nw:  setRender(withContent(page, "b", 30), model.RenderHydrated), // old simhash 0 forces substantive content
			// needs_rendering passes (recovery) => no needs_rendering finding. content substantive (body reappeared).
			// render_mode is also a substantive diff field. Marked quiet on the RENDER axis (no needs_rendering
			// finding); the content/render_mode change-stream events are coherent recovery side effects.
			wantFindings:    nil,
			wantSubstantive: []string{"content", "render_mode"},
		},
		{
			name: "render_mode_unknown_no_fire", siteType: st, class: mustStayQuiet,
			old:          setRender(page, model.RenderUnknown),
			nw:           setRender(page, model.RenderUnknown),
			wantFindings: nil,
		},
		{
			name: "head_only_shell_steady", siteType: st, class: typeNoise,
			old: setRender(page, model.RenderHeadOnlyShell),
			nw:  func() model.Snapshot { s := page; s.ID = 0; s.RenderMode = model.RenderHeadOnlyShell; return s }(),
			// needs_rendering warning (head_only_shell, steady). render_mode unchanged.
			// NOISE(overlay-TODO): SaaS down-tiers to info.
			wantFindings:    map[string]model.Severity{"needs_rendering": model.SeverityWarning},
			wantSubstantive: []string{},
		},
		{
			name: "meta_description_disappears", siteType: st, class: mustFire,
			old:             setMeta(page, "Acme helps teams ship faster. Start free."),
			nw:              setMeta(page, ""),
			wantFindings:    map[string]model.Severity{"meta_description_changed": model.SeverityWarning},
			wantSubstantive: []string{"meta_description"},
		},
		{
			name: "title_disappears", siteType: st, class: mustFire,
			old:             setTitle(page, "Sign Up | Acme"),
			nw:              setTitle(page, ""),
			wantFindings:    map[string]model.Severity{"title_changed": model.SeverityWarning},
			wantSubstantive: []string{"title"},
		},
		{
			name: "canonical_disappears", siteType: st, class: mustFire,
			old:             setCanonical(page, "https://acme.io/features"),
			nw:              setCanonical(page, ""),
			wantFindings:    map[string]model.Severity{"canonical_changed": model.SeverityCritical},
			wantSubstantive: []string{"canonical"},
		},
		{
			name: "oscillation_noindex_flap", siteType: st, class: edge,
			// N+2 recovery crawl: Old = the N+1 noindex snapshot, New = re-indexed.
			old: func() model.Snapshot { s := page; s.Indexable = false; s.MetaRobots = "noindex"; return s }(),
			nw:  page, // indexable true, index,follow
			// On recovery both rules pass; the N+1-opened issues CLOSE. No new alert. (The DOWN-flip on the
			// prior crawl pages — covered by noindex_flip_on_money_page. Oscillation = page-on-down, close-on-up.)
			wantFindings: nil,
		},
		{
			name: "oscillation_title_flap_ab_test", siteType: st, class: typeNoise,
			old: withContent(page, "a", 10),
			nw:  setTitle(withContent(page, "b", 14), "Pricing — Simple Plans for Every Team | Acme"), // content hamming(10,14)=1 cosmetic
			// title_changed warning (each differing crawl). content cosmetic (suppressed).
			// NOISE(overlay-TODO): A/B title flapping.
			wantFindings:    map[string]model.Severity{"title_changed": model.SeverityWarning},
			wantSubstantive: []string{"title"},
		},
		{
			name: "all_clean_money_page_recheck_after_deploy", siteType: st, class: mustStayQuiet,
			old: withContent(page, "a", 0x1111111111111111),
			nw:  withContent(page, "b", 0x1111111111111113), // hamming 1 cosmetic (build-hash churn)
			// only cosmetic content churn; everything else identical. Silent (deploys must not spam).
			wantFindings:    nil,
			wantSubstantive: []string{},
		},
		{
			name: "first_crawl_missing_everything_brochure", siteType: st, class: mustStayQuiet,
			old: firstCrawlBaseline,
			nw: model.Snapshot{
				URLID: 7, HTTPStatus: 200, Title: "Acme", Canonical: "", MetaDescription: "",
				Indexable: true, MetaRobots: "", Headings: `{"h1":["Acme"]}`, RenderMode: model.RenderClientShell,
			},
			// canonical missing + meta absent + needs_rendering all OPEN but first-crawl Slack-suppressed.
			wantFindings: map[string]model.Severity{
				"canonical_changed":        model.SeverityCritical,
				"meta_description_changed": model.SeverityWarning,
				"needs_rendering":          model.SeverityWarning,
			},
		},
	}
}
