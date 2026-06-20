package behavior

import "github.com/roberto-grasiano/rabbot-seo/internal/model"

// localScenarios encodes the local/service-business rows of the scenario matrix.
// Same pure-path semantics as publisherScenarios (see that file's header).
func localScenarios() []scenario {
	const st = "local/service"
	// Healthy location page: LocalBusiness (UNPROFILED) + eligible BreadcrumbList.
	loc := model.Snapshot{
		ID: 432, URLID: 7, HTTPStatus: 200, Indexable: true,
		Title:           "Plumbing in Austin, TX | Acme Plumbing",
		MetaDescription: "24/7 emergency plumbers serving Austin and the surrounding metro area, licensed and insured.",
		Canonical:       "https://acme.com/locations/austin", MetaRobots: "index,follow",
		Headings:   `{"h1":["Austin Plumbing Services"],"h2":["Service Area","Hours"]}`,
		RenderMode: model.RenderServerRendered, InternalLinkCount: 42,
		SchemaTypes:   "BreadcrumbList,LocalBusiness",
		ContentSHA256: "loc-base", ContentSimhash: 0x1122334455667788,
		ImageCount: 3, MissingAltCount: 0,
		JSONLD: `[{"@type":"LocalBusiness","name":"Acme Plumbing","address":{"streetAddress":"100 Main St"},"telephone":"+1-512-555-0100"},{"@type":"BreadcrumbList","itemListElement":[{"position":1,"name":"Home"}]}]`,
	}

	return []scenario{
		{
			name: "first_crawl_complete_location_page", siteType: st, class: mustStayQuiet,
			old:          firstCrawlBaseline,
			nw:           func() model.Snapshot { s := loc; s.ID = 0; return s }(),
			wantFindings: nil,
		},
		{
			name: "first_crawl_location_page_missing_canonical_and_h1", siteType: st, class: mustStayQuiet,
			old: firstCrawlBaseline,
			nw: model.Snapshot{
				URLID: 7, HTTPStatus: 200, Title: "Dallas Roofing", Canonical: "", Indexable: true,
				Headings: `{"h1":[],"h2":["Quote"]}`, RenderMode: model.RenderServerRendered,
				MetaDescription: "Roofing in Dallas, fast quotes.",
			},
			// canonical missing critical + h1 missing warning OPEN; first-crawl Slack-suppressed.
			wantFindings: map[string]model.Severity{
				"canonical_changed": model.SeverityCritical,
				"h1_issue":          model.SeverityWarning,
			},
		},
		{
			name: "location_page_500_first_crawl", siteType: st, class: mustFire,
			old: firstCrawlBaseline,
			nw:  model.Snapshot{URLID: 7, HTTPStatus: 503, Title: "", Canonical: "", Indexable: false},
			wantFindings: map[string]model.Severity{
				"status_regression":        model.SeverityCritical,
				"canonical_changed":        model.SeverityCritical,
				"title_changed":            model.SeverityWarning,
				"meta_description_changed": model.SeverityWarning,
			},
		},
		{
			name: "location_page_410_gone_after_live", siteType: st, class: mustFire,
			old: loc,
			nw: func() model.Snapshot {
				s := loc
				s.HTTPStatus = 410
				s.Title = ""
				s.Canonical = ""
				s.MetaDescription = ""
				s.Indexable = false
				return s
			}(),
			// status 4xx arm fires (prior 200). canonical/title/meta vanish; indexability_flip fires (was indexable).
			wantFindings: map[string]model.Severity{
				"status_regression":        model.SeverityCritical,
				"indexability_flip":        model.SeverityCritical,
				"canonical_changed":        model.SeverityCritical,
				"title_changed":            model.SeverityWarning,
				"meta_description_changed": model.SeverityWarning,
			},
		},
		{
			name: "wordpress_discourage_search_engines_noindex", siteType: st, class: mustFire,
			old: func() model.Snapshot { s := loc; s.IndexabilityReason = "indexable"; return s }(),
			nw: func() model.Snapshot {
				s := loc
				s.Indexable = false
				s.MetaRobots = "noindex, follow"
				s.IndexabilityReason = "meta-noindex"
				return s
			}(),
			wantFindings: map[string]model.Severity{
				"indexability_flip":   model.SeverityCritical,
				"meta_robots_noindex": model.SeverityCritical,
			},
		},
		{
			name: "location_page_noindex_steady_state_first_crawl", siteType: st, class: mustStayQuiet,
			old: firstCrawlBaseline,
			nw: model.Snapshot{
				URLID: 7, HTTPStatus: 200, Indexable: false, MetaRobots: "noindex,follow",
				IndexabilityReason: "meta-noindex", Title: "Duplicate Austin Stub",
				MetaDescription: "stub", Canonical: "https://acme.com/locations/austin",
				Headings: `{"h1":["Austin"]}`, RenderMode: model.RenderServerRendered,
			},
			// meta_robots_noindex OPENS (steady, no guard) but first-crawl Slack-suppressed; flip passes (Old.ID==0).
			wantFindings: map[string]model.Severity{"meta_robots_noindex": model.SeverityCritical},
		},
		{
			name: "nap_phone_number_drift_in_body", siteType: st, class: typeNoise,
			old: withJSONLD(withContent(loc, "loc-base", 0x1122334455667788),
				`[{"@type":"LocalBusiness","name":"Acme Plumbing","telephone":"+1-512-555-0100"}]`, "LocalBusiness"),
			nw: withJSONLD(withContent(loc, "loc-phone", 0x1122334455667789), // hamming 1 cosmetic
				`[{"@type":"LocalBusiness","name":"Acme Plumbing","telephone":"+1-512-555-0199"}]`, "LocalBusiness"),
			// LocalBusiness UNPROFILED (no rich rule). schema_types unchanged. content cosmetic (small footer edit).
			// NOISE(overlay-TODO): NAP-drift WARNING is a fast-follow; today a small footer edit is silently cosmetic.
			wantFindings:    nil,
			wantSubstantive: []string{},
		},
		{
			name: "business_hours_edit_body_content", siteType: st, class: typeNoise,
			old: withContent(loc, "loc-hours-a", 0xAABBCCDDEEFF0011),
			nw:  withContent(loc, "loc-hours-b", 0xAABBCCDDEEFF0013), // hamming 1 cosmetic
			// content cosmetic => suppressed. NOISE(overlay-TODO): hours->info-only is the overlay goal.
			wantFindings:    nil,
			wantSubstantive: []string{},
		},
		{
			name: "localbusiness_address_street_changes_jsonld_only", siteType: st, class: typeNoise,
			old: withJSONLD(withContent(loc, "loc-addr-a", 0x1122334455667788),
				`[{"@type":"LocalBusiness","address":{"streetAddress":"100 Main St"}}]`, "LocalBusiness"),
			nw: withJSONLD(withContent(loc, "loc-addr-b", 0x112233445566AABB), // hamming 10 substantive
				`[{"@type":"LocalBusiness","address":{"streetAddress":"250 Oak Ave"}}]`, "LocalBusiness"),
			// LocalBusiness unprofiled (no rich). schema_types unchanged. content substantive => warning change-stream.
			// NOISE(overlay-TODO): a blunt content warning; the NAP-diff fast-follow would label it precisely.
			wantFindings:    nil,
			wantSubstantive: []string{"content"},
		},
		{
			name: "location_page_canonical_swapped_to_homepage", siteType: st, class: mustFire,
			old:          setCanonical(loc, "https://acme.com/locations/austin"),
			nw:           setCanonical(loc, "https://acme.com/"),
			wantFindings: map[string]model.Severity{"canonical_changed": model.SeverityCritical},
		},
		{
			name: "location_page_title_rewrite_value_change", siteType: st, class: typeNoise,
			old: setTitle(loc, "Plumbing Services | Acme"),
			nw:  setTitle(loc, "Emergency Plumbers in Austin TX | Acme Plumbing"), // 461px fits
			// NOISE(overlay-TODO): local overlay KEEPS title/meta as-is, but value-change is the tunable class.
			wantFindings: map[string]model.Severity{"title_changed": model.SeverityWarning},
		},
		{
			name: "location_page_title_edited_into_overflow", siteType: st, class: mustFire,
			old: setTitle(loc, "Acme Plumbing"),
			nw:  setTitle(loc, "Affordable 24/7 Emergency Plumbing & Drain Cleaning Services in Greater Austin Texas Metro Area | Acme"), // 957.9px
			wantFindings: map[string]model.Severity{
				"title_changed":        model.SeverityWarning,
				"title_pixel_overflow": model.SeverityWarning,
			},
		},
		{
			name: "preexisting_long_title_no_change_overflow_upgrade", siteType: st, class: mustStayQuiet,
			old: setTitle(loc, "Affordable 24/7 Emergency Plumbing & Drain Cleaning Services in Greater Austin Texas Metro Area | Acme"),
			nw:  setTitle(loc, "Affordable 24/7 Emergency Plumbing & Drain Cleaning Services in Greater Austin Texas Metro Area | Acme"),
			// overflow OPENS but push gate suppresses Slack (title unchanged).
			wantFindings:    map[string]model.Severity{"title_pixel_overflow": model.SeverityWarning},
			wantSubstantive: []string{},
		},
		{
			name: "meta_description_steady_overflow_no_change", siteType: st, class: edge,
			old: setMeta(loc, "Acme Plumbing provides fast, affordable, fully licensed and insured 24/7 emergency plumbing and drain-cleaning services across Austin and every surrounding suburb, with upfront pricing, same-day appointments, and a satisfaction guarantee on every single job we take on for you."),
			nw:  setMeta(loc, "Acme Plumbing provides fast, affordable, fully licensed and insured 24/7 emergency plumbing and drain-cleaning services across Austin and every surrounding suburb, with upfront pricing, same-day appointments, and a satisfaction guarantee on every single job we take on for you."),
			// overflow OPENS (>920px) but push gate suppresses Slack (meta unchanged). EDGE: 920px calibration caveat (8558).
			wantFindings:    map[string]model.Severity{"meta_description_pixel_overflow": model.SeverityWarning},
			wantSubstantive: []string{},
		},
		{
			name: "hreflang_added_to_monolingual_local_site", siteType: st, class: typeNoise,
			old: setHreflang(loc, ""),
			nw:  setHreflang(loc, `["en-us"]`),
			// hreflang_invalid WARNING; change-stream routes WARNING too (FIXED #16: agrees with the rule). NOISE(overlay-TODO): gate hreflang OFF for local.
			wantFindings:    map[string]model.Severity{"hreflang_invalid": model.SeverityWarning},
			wantSubstantive: []string{"hreflang"},
		},
		{
			name: "location_page_orphaned_inlink_loss", siteType: st, class: mustFire,
			old:          setInternalLinks(loc, 38),
			nw:           setInternalLinks(loc, 5), // 86.8% drop
			wantFindings: map[string]model.Severity{"broken_links_spike": model.SeverityWarning},
		},
		{
			name: "location_page_minor_inlink_churn_under_threshold", siteType: st, class: mustStayQuiet,
			old:          setInternalLinks(loc, 40),
			nw:           setInternalLinks(loc, 38), // 5% drop
			wantFindings: nil,
		},
		{
			name: "breadcrumb_eligibility_lost_on_location_pages", siteType: st, class: mustFire,
			old: withJSONLD(loc, `[{"@type":"LocalBusiness","name":"Acme"},{"@type":"BreadcrumbList","itemListElement":[{"position":1,"name":"Home"}]}]`, "BreadcrumbList,LocalBusiness"),
			nw:  withJSONLD(loc, `[{"@type":"LocalBusiness","name":"Acme"},{"@type":"BreadcrumbList"}]`, "BreadcrumbList,LocalBusiness"),
			// Breadcrumb lost-eligibility flip; schema_types unchanged.
			wantFindings: map[string]model.Severity{"rich_result_breadcrumb": model.SeverityCritical},
		},
		{
			name: "localbusiness_jsonld_becomes_malformed", siteType: st, class: mustFire,
			old: func() model.Snapshot { s := loc; s.JSONLDInvalidCount = 0; return s }(),
			nw:  func() model.Snapshot { s := loc; s.JSONLDInvalidCount = 1; s.SchemaTypes = ""; return s }(),
			// structured_data_invalid_json warning + schema_types diff (LocalBusiness,BreadcrumbList -> "").
			wantFindings:    map[string]model.Severity{"structured_data_invalid_json": model.SeverityWarning},
			wantSubstantive: []string{"schema_types"},
		},
		{
			name: "localbusiness_jsonld_malformed_but_body_truncated", siteType: st, class: edge,
			old:          func() model.Snapshot { s := loc; s.JSONLDInvalidCount = 0; return s }(),
			nw:           func() model.Snapshot { s := loc; s.JSONLDInvalidCount = 1; return s }(),
			truncated:    true,
			wantFindings: nil,
		},
		{
			name: "location_page_redirect_loop", siteType: st, class: mustFire,
			old:          setRedirect(loc, `["https://acme.com/locations/austin"]`),
			nw:           setRedirect(loc, `["https://acme.com/Locations/Austin","https://acme.com/locations/austin","https://acme.com/Locations/Austin"]`),
			wantFindings: map[string]model.Severity{"redirect_loop": model.SeverityCritical},
		},
		{
			name: "location_page_redirect_chain_lengthens", siteType: st, class: mustFire,
			old:          setRedirect(loc, `["https://acme.com/austin","https://acme.com/locations/austin"]`),
			nw:           setRedirect(loc, `["https://acme.com/austin","https://track.acme.com/x","https://acme.com/loc?id=1","https://acme.com/locations/austin"]`),
			wantFindings: map[string]model.Severity{"redirect_chain_growth": model.SeverityWarning},
		},
		{
			name: "booking_widget_dynamic_content_churn", siteType: st, class: typeNoise,
			old: withContent(loc, "loc-book-a", 0x0F0F0F0F0F0F0F0F),
			nw:  withContent(loc, "loc-book-b", 0x0F0F0F0F0F0FF0F0), // hamming 16 substantive
			// NOISE(overlay-TODO): booking-widget slot churn; overlay relaxes content on booking segments.
			wantFindings:    nil,
			wantSubstantive: []string{"content"},
		},
		{
			name: "review_widget_word_count_only_change", siteType: st, class: mustStayQuiet,
			old: func() model.Snapshot {
				s := withContent(loc, "loc-rev-a", 0x1234567890ABCDEF)
				s.WordCount = 520
				return s
			}(),
			nw: func() model.Snapshot {
				s := withContent(loc, "loc-rev-b", 0x1234567890ABCDEE)
				s.WordCount = 535
				return s
			}(), // content hamming 1 cosmetic
			// word_count cosmetic + content cosmetic => both suppressed.
			wantFindings:    nil,
			wantSubstantive: []string{},
		},
		{
			name: "image_alt_regression_on_gallery_page", siteType: st, class: typeNoise,
			old: setImages(loc, 12, 1),
			nw:  setImages(loc, 20, 9), // increase; coverage 0.55<0.80
			wantFindings: map[string]model.Severity{
				"image_alt_regression": model.SeverityWarning,
				"image_alt_missing":    model.SeverityInfo,
			},
		},
		{
			name: "image_alt_fix_rebaseline_no_false_fire", siteType: st, class: mustStayQuiet,
			old:          setImages(loc, 20, 9),
			nw:           setImages(loc, 20, 2), // decrease; coverage 0.9>=0.8
			wantFindings: nil,
		},
		{
			name: "location_page_404_then_recovers_oscillation", siteType: st, class: mustFire,
			// Crawl N down-edge: Old 200 -> New 404.
			old: loc,
			nw:  func() model.Snapshot { s := loc; s.HTTPStatus = 404; s.Indexable = false; return s }(),
			wantFindings: map[string]model.Severity{
				"status_regression": model.SeverityCritical,
				"indexability_flip": model.SeverityCritical,
			},
		},
		{
			name: "indexable_flip_back_recovery_closes_incident", siteType: st, class: mustStayQuiet,
			old: func() model.Snapshot {
				s := loc
				s.Indexable = false
				s.MetaRobots = "noindex, follow"
				s.IndexabilityReason = "meta-noindex"
				return s
			}(),
			nw: func() model.Snapshot { s := loc; s.IndexabilityReason = "indexable"; return s }(),
			// Recovery: both rules pass; no FINDING.
			wantFindings: nil,
		},
		{
			name: "location_redirect_to_consolidated_page_200", siteType: st, class: edge,
			old: setCanonical(setRedirect(loc, `["https://acme.com/locations/austin-downtown"]`), "https://acme.com/locations/austin-downtown"),
			nw: func() model.Snapshot {
				s := loc
				s.HTTPStatus = 200
				s.RedirectChain = `["https://acme.com/locations/austin-downtown","https://acme.com/locations/austin"]`
				s.Canonical = "https://acme.com/locations/austin"
				return s
			}(),
			// redirect_chain_growth (depth 0->1) warning + canonical_changed critical (canonical moved).
			wantFindings: map[string]model.Severity{
				"redirect_chain_growth": model.SeverityWarning,
				"canonical_changed":     model.SeverityCritical,
			},
		},
		{
			name: "empty_snapshot_both_sides_panic_safety", siteType: st, class: edge,
			old: model.Snapshot{
				ID: 439, URLID: 7, HTTPStatus: 200, Title: "", Canonical: "", MetaRobots: "",
				Headings: "", RedirectChain: "", JSONLD: "", ContentSHA256: "emptyhash",
				ContentSimhash: 0, Indexable: true,
			},
			nw: model.Snapshot{
				ID: 0, URLID: 7, HTTPStatus: 200, Title: "", Canonical: "", MetaRobots: "",
				Headings: "", RedirectChain: "", JSONLD: "", ContentSHA256: "emptyhash",
				ContentSimhash: 0, Indexable: true,
			},
			// No panic on degenerate input. Identical empty fields => no diff. Snapshot-reading rules on empties:
			// canonical missing critical + title/meta absent warning. h1_issue/redirect/rich emit nothing (empty).
			wantFindings: map[string]model.Severity{
				"canonical_changed":        model.SeverityCritical,
				"title_changed":            model.SeverityWarning,
				"meta_description_changed": model.SeverityWarning,
			},
			wantSubstantive: []string{}, // identical content hash => no content change
		},
		{
			name: "nap_phone_removed_from_jsonld_telephone", siteType: st, class: typeNoise,
			old: withJSONLD(withContent(loc, "loc-nap", 0xCAFEBABEDEADBEEF),
				`[{"@type":"LocalBusiness","name":"Acme","address":{"streetAddress":"100 Main St"},"telephone":"+1"}]`, "LocalBusiness"),
			nw: withJSONLD(withContent(loc, "loc-nap", 0xCAFEBABEDEADBEEF), // identical content hash (phone still in footer)
				`[{"@type":"LocalBusiness","name":"Acme","address":{"streetAddress":"100 Main St"}}]`, "LocalBusiness"),
			// LocalBusiness unprofiled => dropping telephone fires NOTHING. schema_types unchanged. body unchanged.
			// MISS the fast-follow LocalBusiness profile will fix. Silent today.
			wantFindings:    nil,
			wantSubstantive: []string{},
		},
		{
			name: "schema_types_set_loses_localbusiness_entirely", siteType: st, class: typeNoise,
			old: withJSONLD(loc, `[{"@type":"LocalBusiness","name":"Acme"},{"@type":"BreadcrumbList","itemListElement":[{"position":1}]}]`, "BreadcrumbList,LocalBusiness"),
			nw:  withJSONLD(loc, `[{"@type":"BreadcrumbList","itemListElement":[{"position":1}]}]`, "BreadcrumbList"),
			// schema_types warning change-stream (full removal surfaces); rich rules pass (LocalBusiness unprofiled, Breadcrumb eligible).
			// NOISE(overlay-TODO): schema set-churn is type-correlated.
			wantFindings:    nil,
			wantSubstantive: []string{"schema_types"},
		},
		{
			name: "location_page_render_mode_becomes_client_shell", siteType: st, class: mustFire,
			old: setRender(loc, model.RenderServerRendered),
			nw:  setRender(loc, model.RenderClientShell),
			// For LOCAL (not SaaS) a location page needing JS is a real monitoring regression.
			wantFindings: map[string]model.Severity{"needs_rendering": model.SeverityWarning},
		},
		{
			name: "location_page_render_mode_recovers", siteType: st, class: mustStayQuiet,
			old:          setRender(loc, model.RenderClientShell),
			nw:           setRender(loc, model.RenderServerRendered),
			wantFindings: nil,
		},
		{
			name: "title_disappears_template_break", siteType: st, class: mustFire,
			old:             setTitle(loc, "Austin Plumbing | Acme"),
			nw:              setTitle(loc, ""),
			wantFindings:    map[string]model.Severity{"title_changed": model.SeverityWarning},
			wantSubstantive: []string{"title"},
		},
		{
			name: "multiple_h1_on_location_page", siteType: st, class: typeNoise,
			old: setHeadings(loc, `{"h1":["Austin Plumbing"],"h2":["Hours"]}`),
			nw:  setHeadings(loc, `{"h1":["Austin Plumbing","Get a Free Quote"],"h2":["Hours"]}`),
			// FIXED (#h1-rewrite): the headings SET changed (a "Get a Free Quote" H1 appeared),
			// so the genuine-rewrite WARNING arm fires ABOVE the count switch (no longer silently
			// downgraded to the INFO "multiple" finding that never pages). headings diff emitted.
			wantFindings:    map[string]model.Severity{"h1_issue": model.SeverityWarning},
			wantSubstantive: []string{"headings"},
		},
		{
			name: "location_page_h1_text_edited", siteType: st, class: typeNoise,
			old: setHeadings(loc, `{"h1":["Plumbing Services"],"h2":["Hours"]}`),
			nw:  setHeadings(loc, `{"h1":["Austin Emergency Plumbing"],"h2":["Hours"]}`),
			// 1 h1 + headings change => changed warning.
			wantFindings:    map[string]model.Severity{"h1_issue": model.SeverityWarning},
			wantSubstantive: []string{"headings"},
		},
		{
			name: "external_link_spike_injected_spam", siteType: st, class: mustFire,
			old: setExternalLinks(loc, 4),
			nw:  setExternalLinks(loc, 38), // jump 34>=10 AND 38>=2*4
			// FEATURED for local/service (hacked-site guard).
			wantFindings: map[string]model.Severity{"external_link_spike": model.SeverityWarning},
		},
		{
			name: "external_link_spike_normal_editorial_below_threshold", siteType: st, class: mustStayQuiet,
			old:          setExternalLinks(loc, 5),
			nw:           setExternalLinks(loc, 11), // jump 6 < abs floor 10
			wantFindings: nil,
		},
		{
			name: "meta_robots_nofollow_only_added", siteType: st, class: edge,
			old: loc,
			nw:  setMetaRobots(loc, "index, nofollow"), // still indexable
			// meta_robots_noindex PASSES (nofollow != noindex). meta_robots change-stream routes CRITICAL.
			wantFindings:    nil,
			wantSubstantive: []string{"meta_robots"},
		},
		{
			name: "x_robots_tag_noindex_via_header", siteType: st, class: mustFire,
			old: func() model.Snapshot { s := loc; s.XRobotsTag = ""; s.IndexabilityReason = "indexable"; return s }(),
			nw: func() model.Snapshot {
				s := loc
				s.XRobotsTag = "noindex"
				s.Indexable = false
				s.IndexabilityReason = "x-robots-noindex"
				return s
			}(),
			wantFindings: map[string]model.Severity{
				"indexability_flip":   model.SeverityCritical,
				"meta_robots_noindex": model.SeverityCritical,
			},
		},
		{
			name: "identical_recrawl_no_changes", siteType: st, class: mustStayQuiet,
			old:             loc,
			nw:              func() model.Snapshot { s := loc; s.ID = 0; return s }(),
			wantFindings:    nil,
			wantSubstantive: []string{},
		},
		{
			name: "canonical_whitespace_only_value", siteType: st, class: mustFire,
			old:             setCanonical(loc, "https://acme.com/locations/austin"),
			nw:              setCanonical(loc, "   "), // whitespace-only trims to empty => missing arm
			wantFindings:    map[string]model.Severity{"canonical_changed": model.SeverityCritical},
			wantSubstantive: []string{"canonical"},
		},
		{
			name: "hostile_malformed_headings_json", siteType: st, class: edge,
			old: setHeadings(loc, `{"h1":["Austin Plumbing"]}`),
			nw:  setHeadings(loc, `{"h1":["Austin`), // truncated/invalid JSON
			// h1_issue emits NOTHING on invalid JSON (don't-guess) — no panic, no false h1 finding.
			// headings STRING differs => substantive headings change-stream. RULE correctly silent.
			wantFindings:    nil,
			wantSubstantive: []string{"headings"},
		},
		{
			name: "zero_simhash_content_forces_substantive", siteType: st, class: edge,
			old:             withContent(loc, "loc-m", 0x9876543210FEDCBA),
			nw:              withContent(loc, "loc-n", 0), // new simhash 0 forces substantive
			wantFindings:    nil,
			wantSubstantive: []string{"content"},
		},
		{
			name: "meta_description_disappears", siteType: st, class: mustFire,
			old:             setMeta(loc, "24/7 emergency plumbers serving Austin and surrounding areas."),
			nw:              setMeta(loc, ""),
			wantFindings:    map[string]model.Severity{"meta_description_changed": model.SeverityWarning},
			wantSubstantive: []string{"meta_description"},
		},
		{
			name: "location_page_5xx_then_steady_5xx", siteType: st, class: edge,
			// N+1 steady-down crawl: Old 500 -> New 500 (no change).
			old: func() model.Snapshot { s := loc; s.HTTPStatus = 500; return s }(),
			nw:  func() model.Snapshot { s := loc; s.ID = 0; s.HTTPStatus = 500; return s }(),
			// status_regression RE-FAILS (5xx, no Old dep) — pure path shows the FINDING. But http_status is
			// UNCHANGED (500->500) so diff emits NO http_status change => no new change-stream event; the
			// scheduler bridges only newly-opened findings, so no RE-PAGE. Quiet on the NEW-page axis.
			wantFindings:    map[string]model.Severity{"status_regression": model.SeverityCritical},
			wantSubstantive: []string{},
		},
	}
}
