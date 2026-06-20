package behavior

import "github.com/roberto-grasiano/rabbot-seo/internal/model"

// ecommerceScenarios encodes the ecommerce rows of the scenario matrix (PDP/PLP).
// Same pure-path semantics as publisherScenarios (see that file's header):
// findings = failing-rule set; triad collapse / first-crawl Slack gate / dedup are
// scheduler-only and asserted via comments + the substantive change-stream mirror.
func ecommerceScenarios() []scenario {
	const st = "ecommerce"
	// Healthy PDP baseline: self-canonical, eligible Product+BreadcrumbList.
	pdp := model.Snapshot{
		ID: 201, URLID: 7, HTTPStatus: 200, Indexable: true,
		Title:           "Acme Wireless Earbuds X3 | Acme",
		MetaDescription: "Shop the Acme Wireless Earbuds X3 with 36-hour battery and fast charging today.",
		Canonical:       "https://shop.example/p/earbuds-x3", MetaRobots: "index,follow",
		Headings:   `{"h1":["Acme Wireless Earbuds X3"]}`,
		RenderMode: model.RenderServerRendered, InternalLinkCount: 85,
		SchemaTypes:   "BreadcrumbList,Product",
		ContentSHA256: "pdp-base", ContentSimhash: 0x1111111111111111,
		JSONLD: `[{"@type":"Product","name":"Earbuds X3","offers":{"@type":"Offer","price":"79.99","priceCurrency":"USD"}},{"@type":"BreadcrumbList","itemListElement":[{"position":1,"name":"Home"}]}]`,
	}

	return []scenario{
		{
			name: "pdp_first_crawl_healthy_product", siteType: st, class: mustStayQuiet,
			old:          firstCrawlBaseline,
			nw:           func() model.Snapshot { s := pdp; s.ID = 0; return s }(),
			wantFindings: nil,
		},
		{
			name: "pdp_first_crawl_missing_self_canonical", siteType: st, class: mustStayQuiet,
			old: firstCrawlBaseline,
			nw:  func() model.Snapshot { s := pdp; s.ID = 0; s.Canonical = ""; return s }(),
			// canonical missing OPENS but is Slack-suppressed on first crawl.
			wantFindings: map[string]model.Severity{"canonical_changed": model.SeverityCritical},
		},
		{
			name: "pdp_in_stock_to_out_of_stock_availability", siteType: st, class: mustStayQuiet,
			old: withContent(pdp, "pdp-base", 0x00FF00FF00FF00FF),
			nw: withJSONLD(
				withContent(pdp, "pdp-oos", 0x00FF00FF00FF01FF), // hamming 1 cosmetic
				`[{"@type":"Product","name":"Earbuds X3","offers":{"@type":"Offer","price":"79.99","priceCurrency":"USD","availability":"OutOfStock"}},{"@type":"BreadcrumbList","itemListElement":[{"position":1,"name":"Home"}]}]`,
				"BreadcrumbList,Product"),
			// availability not validated; offers still present => rich passes. content cosmetic. Silent.
			wantFindings:    nil,
			wantSubstantive: []string{},
		},
		{
			name: "pdp_out_of_stock_content_crosses_simhash", siteType: st, class: typeNoise,
			old: withContent(pdp, "pdp-base", 0x1111111111111111),
			nw:  withContent(pdp, "pdp-buybox-swap", 0xFFFF1111FFFF1111), // hamming > 4 substantive
			// NOISE(overlay-TODO): benign stock-driven body swap pages as substantive content.
			wantFindings:    nil,
			wantSubstantive: []string{"content"},
		},
		{
			name: "pdp_out_of_stock_goes_noindex_triad", siteType: st, class: mustFire,
			old: pdp,
			nw: func() model.Snapshot {
				s := pdp
				s.Indexable = false
				s.MetaRobots = "noindex,follow"
				s.IndexabilityReason = "meta noindex"
				return s
			}(),
			wantFindings: map[string]model.Severity{
				"indexability_flip":   model.SeverityCritical,
				"meta_robots_noindex": model.SeverityCritical,
			},
		},
		{
			name: "retired_sku_returns_404", siteType: st, class: mustFire,
			old: pdp,
			nw:  func() model.Snapshot { s := pdp; s.HTTPStatus = 404; s.Indexable = false; return s }(),
			wantFindings: map[string]model.Severity{
				"status_regression": model.SeverityCritical,
				"indexability_flip": model.SeverityCritical,
			},
		},
		{
			name: "retired_sku_returns_410_gone", siteType: st, class: mustFire,
			old: pdp,
			nw:  func() model.Snapshot { s := pdp; s.HTTPStatus = 410; s.Indexable = false; return s }(),
			wantFindings: map[string]model.Severity{
				"status_regression": model.SeverityCritical,
				"indexability_flip": model.SeverityCritical,
			},
		},
		{
			name: "pdp_500_on_first_crawl", siteType: st, class: mustFire,
			old: firstCrawlBaseline,
			nw: model.Snapshot{
				URLID: 7, HTTPStatus: 500, Indexable: false,
			},
			// 5xx pages on first crawl. canonical/title/meta also open (Slack-suppressed).
			wantFindings: map[string]model.Severity{
				"status_regression":        model.SeverityCritical,
				"canonical_changed":        model.SeverityCritical,
				"title_changed":            model.SeverityWarning,
				"meta_description_changed": model.SeverityWarning,
			},
		},
		{
			name: "plp_facet_url_gets_noindex_follow", siteType: st, class: typeNoise,
			old: pdp,
			nw: func() model.Snapshot {
				s := pdp
				s.Indexable = false
				s.MetaRobots = "noindex,follow"
				s.IndexabilityReason = "meta noindex"
				return s
			}(),
			// Mechanically identical to OOS-noindex; for a FACET this is intentional hygiene.
			// NOISE(overlay-TODO): overlay suppresses noindex on facet URL classes.
			wantFindings: map[string]model.Severity{
				"indexability_flip":   model.SeverityCritical,
				"meta_robots_noindex": model.SeverityCritical,
			},
		},
		{
			name: "plp_facet_canonical_points_to_clean_plp", siteType: st, class: typeNoise,
			old: setCanonical(pdp, "https://shop.example/shoes?color=red"),
			nw:  setCanonical(pdp, "https://shop.example/shoes"),
			// NOISE(overlay-TODO): facet canonicalization is benign; overlay suppresses changed-arm on facets.
			wantFindings: map[string]model.Severity{"canonical_changed": model.SeverityCritical},
		},
		{
			name: "pdp_canonical_points_to_different_product", siteType: st, class: mustFire,
			old:          setCanonical(pdp, "https://shop.example/p/earbuds-x3"),
			nw:           setCanonical(pdp, "https://shop.example/p/earbuds-x1"),
			wantFindings: map[string]model.Severity{"canonical_changed": model.SeverityCritical},
		},
		{
			name: "pdp_self_canonical_dropped", siteType: st, class: mustFire,
			old:          setCanonical(pdp, "https://shop.example/p/earbuds-x3"),
			nw:           setCanonical(pdp, ""),
			wantFindings: map[string]model.Severity{"canonical_changed": model.SeverityCritical},
		},
		{
			name: "pdp_loses_offers_rich_eligibility_lost", siteType: st, class: mustFire,
			old: withJSONLD(pdp, `[{"@type":"Product","name":"Earbuds X3","offers":{"price":"79.99","priceCurrency":"USD"}}]`, "Product"),
			nw:  withJSONLD(pdp, `[{"@type":"Product","name":"Earbuds X3"}]`, "Product"),
			// Lost-eligibility flip; schema_types unchanged so rich rule is the sole catch.
			wantFindings: map[string]model.Severity{"rich_result_product": model.SeverityCritical},
		},
		{
			name: "pdp_price_changes_value_only", siteType: st, class: mustStayQuiet,
			old: withJSONLD(withContent(pdp, "pdp-base", 0x2222), `[{"@type":"Product","name":"Earbuds X3","offers":{"price":"79.99","priceCurrency":"USD"}}]`, "Product"),
			nw:  withJSONLD(withContent(pdp, "pdp-price", 0x2223), `[{"@type":"Product","name":"Earbuds X3","offers":{"price":"59.99","priceCurrency":"USD"}}]`, "Product"), // hamming 1 cosmetic
			// JSON-LD not value-diffed; offers still present => rich passes. content cosmetic. Silent.
			wantFindings:    nil,
			wantSubstantive: []string{},
		},
		{
			name: "pdp_offers_present_but_price_zero_engine_miss", siteType: st, class: edge,
			old: withJSONLD(pdp, `[{"@type":"Product","name":"Earbuds X3","offers":{"price":"79.99","priceCurrency":"USD"}}]`, "Product"),
			nw:  withJSONLD(pdp, `[{"@type":"Product","name":"Earbuds X3","offers":{"price":"0"}}]`, "Product"),
			// DOCUMENTED LIMITATION (not a bug, not a defect to skip): present() counts price:0 as present,
			// AnyOf only needs the offers KEY => still ELIGIBLE => rich passes. Quiet is correct-but-incomplete.
			wantFindings:    nil,
			wantSubstantive: []string{},
		},
		{
			name: "pdp_adds_aggregaterating_drops_offers_still_eligible", siteType: st, class: mustStayQuiet,
			old: withJSONLD(pdp, `[{"@type":"Product","name":"Earbuds X3","offers":{"price":"79.99"}}]`, "Product"),
			nw:  withJSONLD(pdp, `[{"@type":"Product","name":"Earbuds X3","aggregateRating":{"ratingValue":"4.5","reviewCount":"212"}}]`, "Product"),
			// AnyOf satisfied by aggregateRating => rich passes (no flip). Silent.
			wantFindings: nil,
		},
		{
			name: "breadcrumb_structure_breaks_itemlistelement_emptied", siteType: st, class: mustFire,
			old: withJSONLD(pdp, `[{"@type":"BreadcrumbList","itemListElement":[{"position":1,"name":"Home"},{"position":2,"name":"Shoes"}]},{"@type":"Product","name":"X","offers":{"price":"1"}}]`, "BreadcrumbList,Product"),
			nw:  withJSONLD(pdp, `[{"@type":"BreadcrumbList","itemListElement":[]},{"@type":"Product","name":"X","offers":{"price":"1"}}]`, "BreadcrumbList,Product"),
			// Breadcrumb lost-eligibility flip; Product still eligible (passes). schema_types unchanged.
			wantFindings: map[string]model.Severity{"rich_result_breadcrumb": model.SeverityCritical},
		},
		{
			name: "pdp_jsonld_becomes_malformed", siteType: st, class: mustFire,
			old:          func() model.Snapshot { s := pdp; s.JSONLDInvalidCount = 0; return s }(),
			nw:           func() model.Snapshot { s := pdp; s.JSONLDInvalidCount = 1; return s }(),
			wantFindings: map[string]model.Severity{"structured_data_invalid_json": model.SeverityWarning},
		},
		{
			name: "pdp_body_truncated_jsonld_rules_suppress", siteType: st, class: edge,
			old:       func() model.Snapshot { s := pdp; s.JSONLDInvalidCount = 0; return s }(),
			nw:        func() model.Snapshot { s := pdp; s.JSONLDInvalidCount = 1; return s }(),
			truncated: true,
			// All 4 JSON-LD rules self-suppress; head fields unchanged. Silent.
			wantFindings: nil,
		},
		{
			name: "plp_loses_most_product_tiles_internal_links_collapse", siteType: st, class: mustFire,
			old:          setInternalLinks(pdp, 120),
			nw:           setInternalLinks(pdp, 30), // 75% drop
			wantFindings: map[string]model.Severity{"broken_links_spike": model.SeverityWarning},
		},
		{
			name: "plp_pagination_churn_small_delta", siteType: st, class: mustStayQuiet,
			old:          setInternalLinks(pdp, 120),
			nw:           setInternalLinks(pdp, 110), // 8% drop
			wantFindings: nil,
		},
		{
			name: "plp_title_rewritten_merchandising", siteType: st, class: typeNoise,
			old: setTitle(pdp, "Men's Running Shoes"),
			nw:  setTitle(pdp, "Men's Running Shoes — Free Shipping | Acme"), // ~390px under 580
			// NOISE(overlay-TODO): merchandising title value-change.
			wantFindings: map[string]model.Severity{"title_changed": model.SeverityWarning},
		},
		{
			name: "pdp_title_edited_into_serp_overflow", siteType: st, class: mustFire,
			old: setTitle(pdp, "Acme Earbuds X3"),
			nw:  setTitle(pdp, "Acme Wireless Noise-Cancelling Bluetooth Earbuds X3 — Premium Sound, 36h Battery, USB-C Fast Charge | Acme Official Store"), // 1154px
			wantFindings: map[string]model.Severity{
				"title_changed":        model.SeverityWarning,
				"title_pixel_overflow": model.SeverityWarning,
			},
		},
		{
			name: "upgrade_preexisting_long_pdp_title_unchanged", siteType: st, class: mustStayQuiet,
			old: setTitle(pdp, "Acme Wireless Noise-Cancelling Bluetooth Earbuds X3 — Premium Sound, 36h Battery, USB-C Fast Charge | Acme Official Store"),
			nw:  setTitle(pdp, "Acme Wireless Noise-Cancelling Bluetooth Earbuds X3 — Premium Sound, 36h Battery, USB-C Fast Charge | Acme Official Store"),
			// overflow OPENS but A3 push gate suppresses Slack (title unchanged). Pure path shows the finding.
			wantFindings:    map[string]model.Severity{"title_pixel_overflow": model.SeverityWarning},
			wantSubstantive: []string{},
		},
		{
			name: "pdp_h1_changes_with_title_single_h1", siteType: st, class: typeNoise,
			old: setTitle(setHeadings(pdp, `{"h1":["Acme Earbuds X3"]}`), "Acme Earbuds X3"),
			nw:  setTitle(setHeadings(pdp, `{"h1":["Acme Earbuds X3 (2026 Model)"]}`), "Acme Earbuds X3 (2026 Model)"),
			// h1 changed (1 h1 + headings change) warning + title_changed warning.
			// NOISE(overlay-TODO): routine PDP copy refresh.
			wantFindings: map[string]model.Severity{
				"h1_issue":      model.SeverityWarning,
				"title_changed": model.SeverityWarning,
			},
			wantSubstantive: []string{"headings", "title"},
		},
		{
			name: "pdp_template_breaks_h1_missing", siteType: st, class: mustFire,
			old:             setHeadings(pdp, `{"h1":["Acme Earbuds X3"],"h2":["Specs","Reviews"]}`),
			nw:              setHeadings(pdp, `{"h2":["Specs","Reviews"]}`),
			wantFindings:    map[string]model.Severity{"h1_issue": model.SeverityWarning},
			wantSubstantive: []string{"headings"},
		},
		{
			name: "pdp_renders_multiple_h1s_theme_quirk", siteType: st, class: typeNoise,
			old: setHeadings(pdp, `{"h1":["Acme Earbuds X3"]}`),
			nw:  setHeadings(pdp, `{"h1":["Summer Sale","Acme Earbuds X3"]}`),
			// FIXED (#h1-rewrite): the headings SET changed (a "Summer Sale" H1 appeared), so the
			// genuine-rewrite WARNING arm fires ABOVE the count switch (no longer downgraded to the
			// INFO "multiple" finding that never pages). headings diff also emitted.
			wantFindings:    map[string]model.Severity{"h1_issue": model.SeverityWarning},
			wantSubstantive: []string{"headings"},
		},
		{
			name: "pdp_gallery_images_lose_alt_regression", siteType: st, class: typeNoise,
			old: setImages(pdp, 12, 2),
			nw:  setImages(pdp, 12, 9), // increase; coverage 0.25<0.80
			// image_alt_regression warning + image_alt_missing info.
			// NOISE(overlay-TODO): PDP gallery alt churn.
			wantFindings: map[string]model.Severity{
				"image_alt_regression": model.SeverityWarning,
				"image_alt_missing":    model.SeverityInfo,
			},
		},
		{
			name: "pdp_alt_fix_rebaseline", siteType: st, class: mustStayQuiet,
			old:          setImages(pdp, 12, 9),
			nw:           setImages(pdp, 12, 1), // decrease; coverage 0.917>=0.8
			wantFindings: nil,
		},
		{
			name: "pdp_injected_external_links_hacked", siteType: st, class: mustFire,
			old:          setExternalLinks(pdp, 3),
			nw:           setExternalLinks(pdp, 40), // jump 37>=10 AND 40>=2*3
			wantFindings: map[string]model.Severity{"external_link_spike": model.SeverityWarning},
		},
		{
			name: "pdp_redirect_chain_grows", siteType: st, class: mustFire,
			old:          setRedirect(pdp, `["http://shop.example/p/x3","https://shop.example/p/x3"]`),
			nw:           setRedirect(pdp, `["http://shop.example/p/x3","https://shop.example/p/x3/","https://shop.example/us/p/x3/","https://shop.example/us/p/x3"]`),
			wantFindings: map[string]model.Severity{"redirect_chain_growth": model.SeverityWarning},
		},
		{
			name: "pdp_redirect_loop_introduced", siteType: st, class: mustFire,
			old:          setRedirect(pdp, `["https://shop.example/p/x3"]`),
			nw:           setRedirect(pdp, `["https://shop.example/p/x3","https://shop.example/product/x3","https://shop.example/p/x3"]`),
			wantFindings: map[string]model.Severity{"redirect_loop": model.SeverityCritical},
		},
		{
			name: "pdp_steady_state_noindex_no_reflip", siteType: st, class: mustStayQuiet,
			old: func() model.Snapshot { s := pdp; s.Indexable = false; s.MetaRobots = "noindex"; return s }(),
			nw:  func() model.Snapshot { s := pdp; s.ID = 0; s.Indexable = false; s.MetaRobots = "noindex"; return s }(),
			// meta_robots_noindex RE-FAILS every crawl (steady, no guard). Pure path shows the FINDING;
			// the scheduler only bridges NEWLY-opened findings, so it does not re-page. indexability_flip
			// passes (Old not indexable). Quiet=Slack (no NEW page).
			wantFindings:    map[string]model.Severity{"meta_robots_noindex": model.SeverityCritical},
			wantSubstantive: []string{},
		},
		{
			name: "plp_search_results_first_observed_noindex", siteType: st, class: mustStayQuiet,
			old: firstCrawlBaseline,
			nw: model.Snapshot{
				URLID: 7, HTTPStatus: 200, Indexable: false, MetaRobots: "noindex,follow",
				IndexabilityReason: "meta noindex", Canonical: "https://shop.example/search",
				Title: "Search", MetaDescription: "Results", Headings: `{"h1":["Search"]}`,
				RenderMode: model.RenderServerRendered,
			},
			// meta_robots_noindex OPENS (steady, no guard) but first-crawl Slack-suppressed.
			// indexability_flip passes (Old.ID==0). Quiet=Slack.
			wantFindings: map[string]model.Severity{"meta_robots_noindex": model.SeverityCritical},
		},
		{
			name: "pdp_meta_robots_gains_nofollow_only", siteType: st, class: mustFire,
			old: pdp,
			nw:  setMetaRobots(pdp, "index,nofollow"), // still indexable
			// meta_robots_noindex PASSES (nofollow != noindex). No rule fires.
			// meta_robots change-stream event routes CRITICAL (no indexable flip => no collapse).
			wantFindings:    nil,
			wantSubstantive: []string{"meta_robots"},
		},
		{
			name: "pdp_renders_as_client_shell_headless", siteType: st, class: typeNoise,
			old: setRender(pdp, model.RenderServerRendered),
			nw:  setRender(pdp, model.RenderClientShell),
			// needs_rendering warning (flip). NOISE(overlay-TODO): known-SPA storefront down-tiers to info.
			wantFindings: map[string]model.Severity{"needs_rendering": model.SeverityWarning},
		},
		{
			name: "pdp_recovers_client_shell_to_server", siteType: st, class: mustStayQuiet,
			old:          setRender(pdp, model.RenderClientShell),
			nw:           setRender(pdp, model.RenderServerRendered),
			wantFindings: nil,
		},
		{
			name: "pdp_meta_description_rewritten_overflows", siteType: st, class: typeNoise,
			old: setMeta(pdp, "Shop the Acme Earbuds X3."),
			nw:  setMeta(pdp, "Discover the all-new Acme Wireless Earbuds X3 featuring 36-hour battery, active noise cancellation, USB-C fast charging, IPX5 water resistance, and premium balanced sound — the best true-wireless earbuds for music, calls, and workouts, now with free shipping and a 2-year warranty."), // 1775px
			// meta_description_changed warning + meta_description_pixel_overflow warning.
			// NOISE(overlay-TODO): the CHANGE is type-noise; the overflow is a (920px-calibration-caveat) signal.
			wantFindings: map[string]model.Severity{
				"meta_description_changed":        model.SeverityWarning,
				"meta_description_pixel_overflow": model.SeverityWarning,
			},
		},
		{
			name: "pdp_schema_types_set_shrinks_product_removed", siteType: st, class: mustFire,
			old: withJSONLD(pdp, `[{"@type":"Product","name":"X","offers":{"price":"1"}},{"@type":"BreadcrumbList","itemListElement":[{"position":1}]}]`, "BreadcrumbList,Product"),
			nw:  withJSONLD(pdp, `[{"@type":"BreadcrumbList","itemListElement":[{"position":1}]}]`, "BreadcrumbList"),
			// schema_types warning change-stream; rich_result_product PASSES (Product absent). No double-fire.
			wantFindings:    nil,
			wantSubstantive: []string{"schema_types"},
		},
		{
			name: "pdp_hreflang_set_changes_added_locale", siteType: st, class: typeNoise,
			old: setHreflang(pdp, "en-US,en-GB"),
			nw:  setHreflang(pdp, "en-US,en-GB,en-CA"),
			// hreflang_invalid WARNING (rule); change-stream hreflang event routes WARNING too (FIXED #16:
			// agrees with the rule, so the bridge dedup no longer swallows the hreflang_invalid finding).
			// NOISE(overlay-TODO): expanding locales; overlay gates hreflang.
			wantFindings:    map[string]model.Severity{"hreflang_invalid": model.SeverityWarning},
			wantSubstantive: []string{"hreflang"},
		},
		{
			name: "pdp_unchanged_steady_state", siteType: st, class: mustStayQuiet,
			old:             pdp,
			nw:              func() model.Snapshot { s := pdp; s.ID = 0; return s }(),
			wantFindings:    nil,
			wantSubstantive: []string{},
		},
		{
			name: "plp_200_to_301_redirect_to_category", siteType: st, class: typeNoise,
			old: pdp, // canonical unchanged; only status + redirect_chain move
			nw: func() model.Snapshot {
				s := pdp
				s.HTTPStatus = 301
				s.RedirectChain = `["https://shop.example/listing/x","https://shop.example/cat/chairs"]`
				return s
			}(),
			// status_regression does NOT fire (3xx). http_status change-stream event routes CRITICAL.
			// redirect_chain_growth: old chain empty ("") => ok=false => no finding.
			// NOISE(overlay-TODO): expiring-listing redirect lifecycle.
			wantFindings:    nil,
			wantSubstantive: []string{"http_status", "redirect_chain"},
		},
		{
			name: "pdp_indexability_reason_changes_no_flip", siteType: st, class: mustFire,
			old: func() model.Snapshot {
				s := pdp
				s.Indexable = false
				s.MetaRobots = "noindex"
				s.IndexabilityReason = "meta noindex"
				return s
			}(),
			nw: func() model.Snapshot {
				s := pdp
				s.Indexable = false
				s.MetaRobots = ""
				s.IndexabilityReason = "canonicalized"
				return s
			}(),
			// indexable false->false: NO triad collapse (guarded on indexable in change set).
			// meta_robots_noindex PASSES now (closes prior). indexability_flip passes (Old not indexable).
			// indexability_reason + meta_robots change-stream events route CRITICAL.
			wantFindings:    nil,
			wantSubstantive: []string{"indexability_reason", "meta_robots"},
		},
	}
}
