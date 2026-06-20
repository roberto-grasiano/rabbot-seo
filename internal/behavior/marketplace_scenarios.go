package behavior

import "github.com/roberto-grasiano/rabbot-seo/internal/model"

// marketplaceScenarios encodes the marketplace rows of the scenario matrix.
// Same pure-path semantics as publisherScenarios (see that file's header).
func marketplaceScenarios() []scenario {
	const st = "marketplace"
	// Healthy listing baseline: eligible Product (physical good), self-canonical.
	listing := model.Snapshot{
		ID: 42, URLID: 7, HTTPStatus: 200, Indexable: true,
		Title:           "Vintage Eames Lounge Chair - $1,200 | Marketplace",
		MetaDescription: "Vintage Eames lounge chair in excellent condition, ships nationwide from a trusted seller.",
		Canonical:       "https://mkt.example/listing/eames-chair", MetaRobots: "index,follow",
		Headings:   `{"h1":["Vintage Eames Lounge Chair"],"h2":["Details"]}`,
		RenderMode: model.RenderServerRendered, InternalLinkCount: 55,
		SchemaTypes:   "Product",
		ContentSHA256: "lst-base", ContentSimhash: 0x1111111111111111,
		JSONLD: `[{"@type":"Product","name":"Eames Chair","offers":{"@type":"Offer","price":"1200","priceCurrency":"USD"}}]`,
	}

	return []scenario{
		{
			name: "live_listing_200_to_410_gone", siteType: st, class: typeNoise,
			old: listing,
			nw:  setStatus(listing, 410),
			// NOISE(overlay-TODO): 404/410 is Google-endorsed lifecycle for marketplaces.
			wantFindings: map[string]model.Severity{"status_regression": model.SeverityCritical},
		},
		{
			name: "expired_listing_404", siteType: st, class: typeNoise,
			old: listing,
			nw:  setStatus(listing, 404),
			// NOISE(overlay-TODO): leaf-listing expiry churn.
			wantFindings: map[string]model.Severity{"status_regression": model.SeverityCritical},
		},
		{
			name: "listing_200_to_503_flash_sale", siteType: st, class: mustFire,
			old:          listing,
			nw:           setStatus(listing, 503),
			wantFindings: map[string]model.Severity{"status_regression": model.SeverityCritical},
		},
		{
			name: "new_listing_first_crawl_500", siteType: st, class: mustFire,
			old: firstCrawlBaseline,
			nw:  model.Snapshot{URLID: 7, HTTPStatus: 500, Indexable: false, Title: ""},
			// 5xx pages on first crawl; canonical/title/meta open (Slack-suppressed).
			wantFindings: map[string]model.Severity{
				"status_regression":        model.SeverityCritical,
				"canonical_changed":        model.SeverityCritical,
				"title_changed":            model.SeverityWarning,
				"meta_description_changed": model.SeverityWarning,
			},
		},
		{
			name: "listing_recovers_410_to_200", siteType: st, class: mustStayQuiet,
			old: func() model.Snapshot { s := listing; s.HTTPStatus = 410; s.Indexable = false; return s }(),
			nw:  listing, // 200, indexable true
			// status passes (200); indexability_flip passes (Old was non-indexable). Recovery. Silent.
			wantFindings: nil,
		},
		{
			name: "facet_url_self_canonical_to_category", siteType: st, class: typeNoise,
			old: setCanonical(listing, "https://mkt.example/cat/chairs?color=blue&size=l"),
			nw:  setCanonical(listing, "https://mkt.example/cat/chairs"),
			// NOISE(overlay-TODO): facet canonical consolidation.
			wantFindings: map[string]model.Severity{"canonical_changed": model.SeverityCritical},
		},
		{
			name: "search_results_gets_noindex_follow", siteType: st, class: typeNoise,
			old: listing,
			nw: func() model.Snapshot {
				s := listing
				s.MetaRobots = "noindex,follow"
				s.Indexable = false
				s.IndexabilityReason = "meta noindex"
				return s
			}(),
			// NOISE(overlay-TODO): intentional search/facet noindex hygiene.
			wantFindings: map[string]model.Severity{
				"indexability_flip":   model.SeverityCritical,
				"meta_robots_noindex": model.SeverityCritical,
			},
		},
		{
			name: "money_page_listing_accidentally_noindexed", siteType: st, class: mustFire,
			old: func() model.Snapshot { s := listing; s.MetaRobots = "index,follow"; return s }(),
			nw: func() model.Snapshot {
				s := listing
				s.MetaRobots = "noindex,follow"
				s.Indexable = false
				s.IndexabilityReason = "meta noindex"
				return s
			}(),
			wantFindings: map[string]model.Severity{
				"indexability_flip":   model.SeverityCritical,
				"meta_robots_noindex": model.SeverityCritical,
			},
		},
		{
			name: "seller_dashboard_steady_state_noindex", siteType: st, class: typeNoise,
			old: func() model.Snapshot {
				s := listing
				s.MetaRobots = "noindex,nofollow"
				s.Indexable = false
				s.IndexabilityReason = "meta noindex"
				return s
			}(),
			nw: func() model.Snapshot {
				s := listing
				s.ID = 0
				s.MetaRobots = "noindex,nofollow"
				s.Indexable = false
				s.IndexabilityReason = "meta noindex"
				return s
			}(),
			// meta_robots_noindex RE-FAILS every crawl (steady, no guard); indexability_flip passes (Old not indexable).
			// NOISE(overlay-TODO): account areas page critical forever; overlay suppresses on dashboard URLs.
			wantFindings:    map[string]model.Severity{"meta_robots_noindex": model.SeverityCritical},
			wantSubstantive: []string{},
		},
		{
			name: "first_crawl_already_noindexed_dashboard", siteType: st, class: mustStayQuiet,
			old: firstCrawlBaseline,
			nw: model.Snapshot{
				URLID: 7, HTTPStatus: 200, MetaRobots: "noindex,nofollow", Indexable: false,
				IndexabilityReason: "meta noindex", Title: "Dashboard", MetaDescription: "Your orders",
				Canonical: "https://mkt.example/dashboard", Headings: `{"h1":["Dashboard"]}`,
				RenderMode: model.RenderServerRendered,
			},
			// meta_robots_noindex OPENS but first-crawl Slack-suppressed; indexability_flip passes (Old.ID==0).
			wantFindings: map[string]model.Severity{"meta_robots_noindex": model.SeverityCritical},
		},
		{
			name: "listing_title_edited_by_seller", siteType: st, class: typeNoise,
			old: setTitle(listing, "Vintage Eames Lounge Chair - $1,200 | Marketplace"),
			nw:  setTitle(listing, "Vintage Eames Lounge Chair - $950 | Marketplace"), // 448.8px, fits
			// NOISE(overlay-TODO): seller UGC title value-change churn (fits SERP budget).
			wantFindings: map[string]model.Severity{"title_changed": model.SeverityWarning},
		},
		{
			name: "listing_body_substantive_rewrite", siteType: st, class: typeNoise,
			old: withContent(listing, "lst-base", 0x1111111111111111),
			nw:  withContent(listing, "lst-rewrite", 0xEEEEEEEEEEEEEEEE), // hamming large
			// NOISE(overlay-TODO): seller body rewrites on listings.
			wantFindings:    nil,
			wantSubstantive: []string{"content"},
		},
		{
			name: "seller_fixes_typo_cosmetic_content", siteType: st, class: mustStayQuiet,
			old:             withContent(listing, "lst-base", 0x00000000000000FF),
			nw:              withContent(listing, "lst-typo", 0x00000000000000FE), // hamming 1 cosmetic
			wantFindings:    nil,
			wantSubstantive: []string{},
		},
		{
			name: "listing_empty_body_now_has_content_zero_hash", siteType: st, class: edge,
			old: withContent(listing, "empty-hash", 0), // zero = unknown
			nw:  withContent(listing, "real-hash", 0xABCD),
			// zero on either side => forced substantive content.
			wantFindings:    nil,
			wantSubstantive: []string{"content"},
		},
		{
			name: "injected_spam_links_hacked_seller", siteType: st, class: mustFire,
			old: setExternalLinks(listing, 3),
			nw:  setExternalLinks(listing, 43), // jump 40>=10 AND 43>=2*3
			// FEATURED marquee for marketplace (UGC guard).
			wantFindings: map[string]model.Severity{"external_link_spike": model.SeverityWarning},
		},
		{
			name: "listing_adds_few_outbound_links_under_floor", siteType: st, class: mustStayQuiet,
			old:          setExternalLinks(listing, 2),
			nw:           setExternalLinks(listing, 6), // jump 4 < abs floor 10
			wantFindings: nil,
		},
		{
			name: "listing_external_links_double_below_floor", siteType: st, class: mustStayQuiet,
			old:          setExternalLinks(listing, 2),
			nw:           setExternalLinks(listing, 5), // jump 3 < 10
			wantFindings: nil,
		},
		{
			name: "listing_internal_links_collapse_related_break", siteType: st, class: mustFire,
			old:          setInternalLinks(listing, 60),
			nw:           setInternalLinks(listing, 8), // 87% drop
			wantFindings: map[string]model.Severity{"broken_links_spike": model.SeverityWarning},
		},
		{
			name: "listing_index_internal_links_normal_churn", siteType: st, class: mustStayQuiet,
			old:          setInternalLinks(listing, 50),
			nw:           setInternalLinks(listing, 48), // 4% drop
			wantFindings: nil,
		},
		{
			name: "product_listing_drops_offers_rich_lost", siteType: st, class: mustFire,
			old:          withJSONLD(listing, `[{"@type":"Product","name":"Eames Chair","offers":{"@type":"Offer","price":"1200","priceCurrency":"USD"}}]`, "Product"),
			nw:           withJSONLD(listing, `[{"@type":"Product","name":"Eames Chair"}]`, "Product"),
			wantFindings: map[string]model.Severity{"rich_result_product": model.SeverityCritical},
		},
		{
			name: "listing_always_missing_offers_steady", siteType: st, class: typeNoise,
			old: withJSONLD(listing, `[{"@type":"Product","name":"Dog walking service"}]`, "Product"),
			nw:  withJSONLD(listing, `[{"@type":"Product","name":"Dog walking service"}]`, "Product"),
			// rich_result_product WARNING (steady-state ineligible; oldEligible==0 so not a flip).
			// NOISE(overlay-TODO): thin UGC Product markup never qualified.
			wantFindings: map[string]model.Severity{"rich_result_product": model.SeverityWarning},
		},
		{
			name: "jobposting_loses_validthrough_unprofiled_miss", siteType: st, class: edge,
			old: withJSONLD(listing, `[{"@type":"JobPosting","title":"Engineer","validThrough":"2026-07-01"}]`, "JobPosting"),
			nw:  withJSONLD(listing, `[{"@type":"JobPosting","title":"Engineer"}]`, "JobPosting"),
			// JobPosting UNPROFILED => no rich rule fires. schema_types unchanged. JSON-LD not value-diffed. MISS by design.
			wantFindings:    nil,
			wantSubstantive: []string{},
		},
		{
			name: "listing_jsonld_becomes_malformed", siteType: st, class: mustFire,
			old:          func() model.Snapshot { s := listing; s.JSONLDInvalidCount = 0; return s }(),
			nw:           func() model.Snapshot { s := listing; s.JSONLDInvalidCount = 1; return s }(),
			wantFindings: map[string]model.Severity{"structured_data_invalid_json": model.SeverityWarning},
		},
		{
			name: "huge_listing_body_truncated", siteType: st, class: edge,
			old:       func() model.Snapshot { s := listing; s.JSONLDInvalidCount = 0; return s }(),
			nw:        func() model.Snapshot { s := listing; s.JSONLDInvalidCount = 1; s.ContentSHA256 = "lst-cut"; return s }(),
			truncated: true,
			// Truncated => JSON-LD rules suppress + content change dropped (we model via truncated; the
			// pure path here still computes the content diff, BUT the scenario asserts the RULE set only).
			// To honor dropContentChanges, we assert no FINDINGS (rules don't read content) and don't pin substantive.
			wantFindings: nil,
		},
		{
			name: "leaf_listing_orphaned_incoming_only", siteType: st, class: edge,
			old: func() model.Snapshot { s := listing; s.InternalLinkCount = 55; s.IncomingCanonicalCount = 12; return s }(),
			nw:  func() model.Snapshot { s := listing; s.InternalLinkCount = 55; s.IncomingCanonicalCount = 0; return s }(),
			// Incoming* not diffed, no rule. Outbound InternalLinkCount unchanged => broken_links_spike passes. Silent (MISS-by-design).
			wantFindings:    nil,
			wantSubstantive: []string{},
		},
		{
			name: "listing_h1_changes_seller_headline", siteType: st, class: typeNoise,
			old: setHeadings(listing, `{"h1":["Vintage Eames Lounge Chair"],"h2":["Details"]}`),
			nw:  setHeadings(listing, `{"h1":["Vintage Eames Lounge Chair - REDUCED"],"h2":["Details"]}`),
			// h1 changed (1 h1 + headings change) warning.
			// NOISE(overlay-TODO): seller headline UGC churn.
			wantFindings:    map[string]model.Severity{"h1_issue": model.SeverityWarning},
			wantSubstantive: []string{"headings"},
		},
		{
			name: "listing_multiple_h1_template_quirk", siteType: st, class: typeNoise,
			old: setHeadings(listing, `{"h1":["Eames Chair","Featured Sellers"]}`),
			nw:  setHeadings(listing, `{"h1":["Eames Chair","Featured Sellers"]}`), // unchanged, 2 h1
			// 'multiple' INFO; no headings change (identical). Never pages.
			wantFindings: map[string]model.Severity{"h1_issue": model.SeverityInfo},
		},
		{
			name: "listing_client_shell_spa", siteType: st, class: typeNoise,
			old: setRender(listing, model.RenderClientShell),
			nw:  func() model.Snapshot { s := listing; s.ID = 0; s.RenderMode = model.RenderClientShell; return s }(),
			// needs_rendering warning (steady client_shell, no guard); render_mode unchanged.
			// NOISE(overlay-TODO): SPA listing detail down-tiers to info.
			wantFindings: map[string]model.Severity{"needs_rendering": model.SeverityWarning},
		},
		{
			name: "listing_recovers_client_shell_to_server", siteType: st, class: mustStayQuiet,
			old:          setRender(listing, model.RenderClientShell),
			nw:           setRender(listing, model.RenderServerRendered),
			wantFindings: nil,
		},
		{
			name: "listing_canonical_disappears", siteType: st, class: mustFire,
			old:          setCanonical(listing, "https://mkt.example/listing/eames-chair"),
			nw:           setCanonical(listing, ""),
			wantFindings: map[string]model.Severity{"canonical_changed": model.SeverityCritical},
		},
		{
			name: "marketplace_adds_hreflang_multi_region", siteType: st, class: typeNoise,
			old: setHreflang(listing, ""),
			nw:  setHreflang(listing, "en-us,es-es,fr-fr,de-de"),
			// hreflang_invalid WARNING; change-stream hreflang routes WARNING too (FIXED #16: agrees with the rule).
			// NOISE(overlay-TODO): intentional internationalization.
			wantFindings:    map[string]model.Severity{"hreflang_invalid": model.SeverityWarning},
			wantSubstantive: []string{"hreflang"},
		},
		{
			name: "listing_meta_description_edited_into_overflow", siteType: st, class: typeNoise,
			old: setMeta(listing, "Vintage Eames lounge chair, excellent condition."),
			nw:  setMeta(listing, "Vintage Eames lounge chair in pristine museum-grade condition with original Herman Miller documentation, certificate of authenticity, premium rosewood veneer, refurbished leather cushions, white-glove nationwide delivery, and a thirty-day satisfaction guarantee from a top-rated verified seller."), // long >920px
			wantFindings: map[string]model.Severity{
				"meta_description_changed":        model.SeverityWarning,
				"meta_description_pixel_overflow": model.SeverityWarning,
			},
		},
		{
			name: "preexisting_long_title_upgrade_no_change", siteType: st, class: mustStayQuiet,
			old: setTitle(listing, "Vintage Eames Lounge Chair in Pristine Museum-Grade Condition with Original Herman Miller Documentation and Certificate | Marketplace"),
			nw:  setTitle(listing, "Vintage Eames Lounge Chair in Pristine Museum-Grade Condition with Original Herman Miller Documentation and Certificate | Marketplace"),
			// overflow OPENS but push gate suppresses Slack (title unchanged).
			wantFindings:    map[string]model.Severity{"title_pixel_overflow": model.SeverityWarning},
			wantSubstantive: []string{},
		},
		{
			name: "listing_redirect_loop", siteType: st, class: mustFire,
			old:          setRedirect(listing, `["https://mkt.example/listing/x","https://mkt.example/Listing/x"]`),
			nw:           setRedirect(listing, `["https://mkt.example/listing/x","https://mkt.example/Listing/x","https://mkt.example/listing/x"]`),
			wantFindings: map[string]model.Severity{"redirect_loop": model.SeverityCritical},
		},
		{
			name: "listing_redirect_chain_grows_no_loop", siteType: st, class: mustFire,
			old:          setRedirect(listing, `["http://mkt.example/x","https://mkt.example/x"]`),
			nw:           setRedirect(listing, `["http://mkt.example/x","https://mkt.example/x","https://www.mkt.example/x","https://www.mkt.example/listing/x"]`),
			wantFindings: map[string]model.Severity{"redirect_chain_growth": model.SeverityWarning},
		},
		{
			name: "listing_image_count_grows_alt_fine", siteType: st, class: mustStayQuiet,
			old:          setImages(listing, 6, 0),
			nw:           setImages(listing, 10, 0), // coverage 1.0
			wantFindings: nil,
		},
		{
			name: "seller_adds_photos_no_alt_regression", siteType: st, class: typeNoise,
			old: setImages(listing, 7, 1),
			nw:  setImages(listing, 12, 6), // increase; coverage 0.5<0.80
			wantFindings: map[string]model.Severity{
				"image_alt_regression": model.SeverityWarning,
				"image_alt_missing":    model.SeverityInfo,
			},
		},
		{
			name: "listing_title_vanishes_template_break", siteType: st, class: mustFire,
			old: setTitle(listing, "Vintage Eames Lounge Chair - $1,200 | Marketplace"),
			nw:  setTitle(listing, ""),
			// missing arm (finding:absent) warning. Also a title diff emitted.
			wantFindings:    map[string]model.Severity{"title_changed": model.SeverityWarning},
			wantSubstantive: []string{"title"},
		},
		{
			name: "schema_types_set_loses_product_block", siteType: st, class: mustFire,
			old: withJSONLD(listing, `[{"@type":"Product","name":"Eames","offers":{"price":"1"}}]`, "Product"),
			nw:  withJSONLD(listing, ``, ""),
			// schema_types warning change-stream; rich_result_product PASSES (absent).
			wantFindings:    nil,
			wantSubstantive: []string{"schema_types"},
		},
		{
			name: "steady_state_healthy_listing_recrawl", siteType: st, class: mustStayQuiet,
			old:             listing,
			nw:              func() model.Snapshot { s := listing; s.ID = 0; return s }(),
			wantFindings:    nil,
			wantSubstantive: []string{},
		},
		{
			name: "listing_200_to_301_redirect_to_category", siteType: st, class: typeNoise,
			old: listing,
			nw: func() model.Snapshot {
				s := listing
				s.HTTPStatus = 301
				s.RedirectChain = `["https://mkt.example/listing/x","https://mkt.example/cat/chairs"]`
				return s
			}(),
			// status_regression does NOT fire (3xx). http_status routes CRITICAL.
			// redirect_chain_growth: old chain empty => ok=false => no finding.
			// NOISE(overlay-TODO): expiring-listing redirect lifecycle.
			wantFindings:    nil,
			wantSubstantive: []string{"http_status", "redirect_chain"},
		},
		{
			name: "listing_meta_robots_gains_nofollow_only", siteType: st, class: mustFire,
			old: func() model.Snapshot { s := listing; s.MetaRobots = "index,follow"; return s }(),
			nw:  setMetaRobots(listing, "index,nofollow"),
			// meta_robots_noindex PASSES (nofollow != noindex). meta_robots change-stream routes CRITICAL.
			wantFindings:    nil,
			wantSubstantive: []string{"meta_robots"},
		},
		{
			name: "listing_multitype_array_keeps_eligibility", siteType: st, class: typeNoise,
			old: withJSONLD(listing, `[{"@type":"Product","name":"Eames","offers":{"price":"1200","priceCurrency":"USD"}}]`, "Product"),
			nw:  withJSONLD(listing, `[{"@type":["Product","IndividualProduct"],"name":"Eames","offers":{"price":"1200","priceCurrency":"USD"}}]`, "Product,IndividualProduct"),
			// rich_result_product PASSES (multi-type, Product eligible). schema_types warning change-stream.
			// NOISE(overlay-TODO): harmless secondary-type addition.
			wantFindings:    nil,
			wantSubstantive: []string{"schema_types"},
		},
	}
}
