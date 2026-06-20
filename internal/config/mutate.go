package config

import (
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"

	yaml "go.yaml.in/yaml/v3"

	"github.com/roberto-grasiano/rabbot-seo/internal/fsatomic"
)

// This file implements structure- and comment-preserving edits to a
// config.yaml. Every mutation round-trips through a *yaml.Node tree rather than
// a Go struct, so keys and comments the caller did not touch survive verbatim.
//
// The on-disk layout of a YAML file is a document node wrapping a single root
// node (usually a mapping). We always normalize the parsed result to that root
// mapping, mutate it in place, then re-marshal the document.

// yamlFileMode is owner-only (0600): config.yaml may carry inline secrets
// (notifier webhook URLs, access.basic_pass, access.cookies) when the operator
// does not use ${ENV} interpolation, so it must not be world-readable — same
// posture as control.token.
const yamlFileMode = 0o600

// yamlDirMode is the mode used only for a parent directory fsatomic.Write must
// CREATE (a no-op when the config dir already exists, which it normally does).
// It matches dirs.go's 0o750 so a defensively-created parent is consistent.
const yamlDirMode = 0o750

// loadDocRoot reads path and returns the document's root node, normalized to a
// mapping node. A missing file yields a fresh empty mapping (missing=true). An
// empty/whitespace-only file is treated the same as a fresh mapping. The
// returned node is always a *yaml.MappingNode ready for key insertion.
//
// A file that contains only comments unmarshals to an empty document and the
// comments are dropped by the parser; loadDocRoot recovers the leading comment
// block from the raw bytes and re-attaches it as the fresh mapping's
// HeadComment so it survives the round-trip.
func loadDocRoot(path string) (root *yaml.Node, missing bool, err error) {
	raw, readErr := os.ReadFile(path)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return newMappingNode(), true, nil
		}
		return nil, false, readErr
	}

	var doc yaml.Node
	if err := yaml.Unmarshal(raw, &doc); err != nil {
		return nil, false, fmt.Errorf("config: parse %s: %w", path, err)
	}

	// An empty document (e.g. only comments or whitespace) unmarshals with no
	// content; start from a fresh mapping, salvaging any leading comment.
	if doc.Kind == 0 || len(doc.Content) == 0 {
		m := newMappingNode()
		if c := leadingComment(raw); c != "" {
			m.HeadComment = c
		}
		return m, false, nil
	}

	r := doc.Content[0]
	// A null/empty scalar root (e.g. a file that is just "---") becomes a map.
	if r.Kind == yaml.ScalarNode && (r.Tag == "!!null" || r.Value == "") {
		*r = *newMappingNode()
	}
	if r.Kind != yaml.MappingNode {
		return nil, false, fmt.Errorf("config: %s root is not a mapping", path)
	}
	return r, false, nil
}

// writeDocRoot marshals root as the sole content of a YAML document and writes
// it to path with 0o600 perms (config.yaml may contain inline secrets).
func writeDocRoot(path string, root *yaml.Node) error {
	doc := &yaml.Node{Kind: yaml.DocumentNode, Content: []*yaml.Node{root}}
	out, err := yaml.Marshal(doc)
	if err != nil {
		return fmt.Errorf("config: marshal %s: %w", path, err)
	}
	// Crash-atomic, power-loss-durable write: neither a SIGKILL nor a power loss
	// mid-write may leave a truncated config.yaml (which would fail config.Load on
	// the next start and brick the daemon). fsatomic.Write does the temp+fsync+
	// rename+dir-fsync dance and Chmods the final file to 0600 even if a looser
	// file already existed (it may hold inline secrets: notifier URLs,
	// access.basic_pass/cookies). See internal/fsatomic.
	if err := fsatomic.Write(path, out, yamlFileMode, yamlDirMode); err != nil {
		return fmt.Errorf("config: write %s: %w", path, err)
	}
	return nil
}

func newMappingNode() *yaml.Node {
	return &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
}

// leadingComment extracts the contiguous block of leading comment/blank lines
// from raw and returns it joined with newlines, suitable for a node's
// HeadComment. Trailing blank lines are trimmed. Returns "" if the file does
// not start with a comment line.
func leadingComment(raw []byte) string {
	lines := strings.Split(string(raw), "\n")
	var head []string
	for _, ln := range lines {
		t := strings.TrimSpace(ln)
		if t == "" {
			// Allow blank lines interleaved before the content begins, but only
			// once a comment has already started.
			if len(head) == 0 {
				continue
			}
			head = append(head, ln)
			continue
		}
		if t[0] == '#' {
			head = append(head, ln)
			continue
		}
		break
	}
	// Trim trailing blank lines.
	for len(head) > 0 && strings.TrimSpace(head[len(head)-1]) == "" {
		head = head[:len(head)-1]
	}
	if len(head) == 0 {
		return ""
	}
	return strings.Join(head, "\n")
}

func scalarNode(tag, value string) *yaml.Node {
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: value}
}

// mapValue returns the value node for key in a mapping node, or nil if absent.
// Mapping content alternates key, value, key, value, ...
func mapValue(m *yaml.Node, key string) *yaml.Node {
	if m.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			return m.Content[i+1]
		}
	}
	return nil
}

// setMapValue inserts or replaces the value node for key in mapping m. If the
// key already exists its value node is replaced (so the key node — and any
// comment attached to it — is preserved). Returns the value node.
func setMapValue(m *yaml.Node, key string, value *yaml.Node) *yaml.Node {
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content[i+1] = value
			return value
		}
	}
	m.Content = append(m.Content,
		scalarNode("!!str", key),
		value,
	)
	return value
}

// deleteMapKey removes key (and its value node) from mapping m, filtering the
// key/value pair out of m.Content. It reports whether a pair was removed. Other
// keys — and their attached comments — are preserved verbatim. A no-op if the
// key is absent or m is not a mapping. Used to actively clear a key whose intent
// is now empty (e.g. verification.verified_at on a non-verified write) rather
// than leaving a stale value behind.
func deleteMapKey(m *yaml.Node, key string) bool {
	if m.Kind != yaml.MappingNode {
		return false
	}
	for i := 0; i+1 < len(m.Content); i += 2 {
		if m.Content[i].Value == key {
			m.Content = append(m.Content[:i], m.Content[i+2:]...)
			return true
		}
	}
	return false
}

// AddSiteYAML appends a site mapping to the top-level "sites" sequence of the
// YAML file at path, preserving all unrelated keys and comments. The sites key
// is created (as an empty sequence) if it is absent or null. Only non-zero
// SiteConfig fields are written, in the order url, name, min_interval,
// max_interval, speed. If path does not exist it is created with just the new
// site under "sites".
func AddSiteYAML(path string, s SiteConfig) error {
	root, _, err := loadDocRoot(path)
	if err != nil {
		return err
	}

	seq := mapValue(root, "sites")
	switch {
	case seq == nil:
		seq = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		setMapValue(root, "sites", seq)
	case seq.Kind == yaml.ScalarNode && (seq.Tag == "!!null" || seq.Value == ""):
		// sites: (null) — promote to an empty sequence in place so any comment
		// attached to the value node is retained.
		seq.Kind = yaml.SequenceNode
		seq.Tag = "!!seq"
		seq.Value = ""
		seq.Content = nil
	case seq.Kind != yaml.SequenceNode:
		return fmt.Errorf("config: %s: sites is not a sequence", path)
	}
	// An empty "sites: []" parses as a flow-style sequence; appending to it would
	// keep the whole list inline. Force block style so each site renders as a
	// readable, line-per-key mapping. (A non-empty block sequence already has
	// Style 0, so this is a no-op there.)
	seq.Style = 0

	site := newMappingNode()
	// url and name are always written when non-empty; the optional fields mirror
	// the omitempty yaml tags on SiteConfig.
	if s.URL != "" {
		setMapValue(site, "url", scalarNode("!!str", s.URL))
	}
	if s.Name != "" {
		setMapValue(site, "name", scalarNode("!!str", s.Name))
	}
	if s.MinInterval != "" {
		setMapValue(site, "min_interval", scalarNode("!!str", s.MinInterval))
	}
	if s.MaxInterval != "" {
		setMapValue(site, "max_interval", scalarNode("!!str", s.MaxInterval))
	}
	if s.Speed != 0 {
		setMapValue(site, "speed", scalarNode("!!int", strconv.Itoa(s.Speed)))
	}

	seq.Content = append(seq.Content, site)
	return writeDocRoot(path, root)
}

// AddNotifierYAML appends a notifier mapping to the top-level "notifiers"
// sequence of the YAML file at path, preserving all unrelated keys and comments.
// It mirrors AddSiteYAML exactly but operates on "notifiers" rather than
// "sites": the sequence is created (as an empty block sequence) if it is absent
// or null, and an empty flow sequence ("notifiers: []") is promoted to block
// style. Only non-empty NotifierConfig fields are written, in the yaml-tag order
// name, type, url.
//
// SECRET-SAFETY: the URL value is written as a plain !!str scalar EXACTLY as
// passed. This is deliberate so a ${RABBOT_SLACK_WEBHOOK} interpolation token
// survives to disk LITERALLY and koanf expands it from the environment at Load
// (NotifierConfig.URL is interpolated in load.go) — the secret never has to be
// committed to the file. This function NEVER logs, prints, or transforms the URL
// (CLAUDE.md: Slack webhook URLs must never be logged); writeDocRoot already
// writes 0600 because config.yaml may carry inline secrets. The never-logged
// guarantee is regression-tested by TestAddNotifierYAMLDoesNotLogWebhook.
func AddNotifierYAML(path string, n NotifierConfig) error {
	root, _, err := loadDocRoot(path)
	if err != nil {
		return err
	}

	seq := mapValue(root, "notifiers")
	switch {
	case seq == nil:
		seq = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		setMapValue(root, "notifiers", seq)
	case seq.Kind == yaml.ScalarNode && (seq.Tag == "!!null" || seq.Value == ""):
		// notifiers: (null) — promote to an empty sequence in place so any comment
		// attached to the value node is retained.
		seq.Kind = yaml.SequenceNode
		seq.Tag = "!!seq"
		seq.Value = ""
		seq.Content = nil
	case seq.Kind != yaml.SequenceNode:
		return fmt.Errorf("config: %s: notifiers is not a sequence", path)
	}
	// "notifiers: []" parses as a flow-style sequence; force block style so each
	// notifier renders as a readable, line-per-key mapping. (A non-empty block
	// sequence already has Style 0, so this is a no-op there.)
	seq.Style = 0

	notifier := newMappingNode()
	// Write fields in NotifierConfig's yaml-tag order (name, type, url, then the
	// email-smtp fields, then the generic-webhook headers), omitting empties.
	// SECRET-SAFETY: every secret-bearing value (url, password, header values) is
	// written verbatim as a plain !!str scalar EXACTLY as passed, so a ${ENV}
	// interpolation token survives to disk LITERALLY (koanf expands it at Load — see
	// interpolateSecrets); the secret never has to be committed to the file, and this
	// function NEVER logs, prints, or transforms it (CLAUDE.md hard rule). writeDocRoot
	// writes 0600 because config.yaml may carry inline secrets.
	if n.Name != "" {
		setMapValue(notifier, "name", scalarNode("!!str", n.Name))
	}
	if n.Type != "" {
		setMapValue(notifier, "type", scalarNode("!!str", n.Type))
	}
	if n.URL != "" {
		setMapValue(notifier, "url", scalarNode("!!str", n.URL))
	}
	// email-smtp fields. SMTPPort is written when > 0 (0 means "unset" — see
	// ValidateNotifiers). AllowPlaintext is written only when true (the safe default
	// is STARTTLS-required, so an absent key reads as false).
	if n.SMTPHost != "" {
		setMapValue(notifier, "smtp_host", scalarNode("!!str", n.SMTPHost))
	}
	if n.SMTPPort > 0 {
		setMapValue(notifier, "smtp_port", scalarNode("!!int", strconv.Itoa(n.SMTPPort)))
	}
	if n.Username != "" {
		setMapValue(notifier, "username", scalarNode("!!str", n.Username))
	}
	if n.Password != "" {
		setMapValue(notifier, "password", scalarNode("!!str", n.Password))
	}
	if n.From != "" {
		setMapValue(notifier, "from", scalarNode("!!str", n.From))
	}
	if len(n.To) > 0 {
		toSeq := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		for _, addr := range n.To {
			toSeq.Content = append(toSeq.Content, scalarNode("!!str", addr))
		}
		setMapValue(notifier, "to", toSeq)
	}
	if n.AllowPlaintext {
		setMapValue(notifier, "allow_plaintext", scalarNode("!!bool", "true"))
	}
	// generic-webhook headers. Written as a nested mapping; values are verbatim so an
	// ${ENV} token in e.g. Authorization survives to disk. Keys are sorted for a
	// deterministic, diff-stable render (map iteration order is otherwise random).
	if len(n.Headers) > 0 {
		hdr := newMappingNode()
		keys := make([]string, 0, len(n.Headers))
		for k := range n.Headers {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			setMapValue(hdr, k, scalarNode("!!str", n.Headers[k]))
		}
		setMapValue(notifier, "headers", hdr)
	}

	// Idempotent by name: a re-run of onboarding (e.g. `rabbot init
	// --slack-webhook X` twice, or the interactive Add-a-site flow that re-collects
	// a webhook) must NOT append a duplicate notifier — that would bloat the config
	// and contradict the "re-running never corrupts the config" guarantee. If an
	// element with the same name already exists, replace it in place (picking up an
	// updated url/type) so exactly one notifier per name survives.
	if n.Name != "" {
		for i, item := range seq.Content {
			if item.Kind == yaml.MappingNode {
				if nm := mapValue(item, "name"); nm != nil && nm.Value == n.Name {
					seq.Content[i] = notifier
					return writeDocRoot(path, root)
				}
			}
		}
	}

	seq.Content = append(seq.Content, notifier)
	return writeDocRoot(path, root)
}

// AddRouteYAML appends a route mapping to the top-level "routes" sequence of the
// YAML file at path, preserving all unrelated keys and comments. It mirrors
// AddNotifierYAML: the sequence is created (as an empty block sequence) if absent
// or null, and an empty flow sequence ("routes: []") is promoted to block style.
// The match map is written only when non-empty, so a fallback route (empty Match)
// renders as just "- notifier: <name>".
//
// A notifier with no route is unreachable — the dispatcher iterates cfg.Routes
// and there is no implicit fallback to all notifiers when the list is empty, so
// the onboarding alerts step must write a route alongside the notifier or real
// change alerts are never dispatched.
//
// Idempotent by notifier: re-adding a route for a notifier that already has one
// (matching on Notifier AND an equal Match map) replaces it in place rather than
// appending a duplicate, so a re-run keeps exactly one route per (notifier,match).
func AddRouteYAML(path string, r RouteConfig) error {
	root, _, err := loadDocRoot(path)
	if err != nil {
		return err
	}

	seq := mapValue(root, "routes")
	switch {
	case seq == nil:
		seq = &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
		setMapValue(root, "routes", seq)
	case seq.Kind == yaml.ScalarNode && (seq.Tag == "!!null" || seq.Value == ""):
		// routes: (null) — promote to an empty sequence in place so any comment
		// attached to the value node is retained.
		seq.Kind = yaml.SequenceNode
		seq.Tag = "!!seq"
		seq.Value = ""
		seq.Content = nil
	case seq.Kind != yaml.SequenceNode:
		return fmt.Errorf("config: %s: routes is not a sequence", path)
	}
	// "routes: []" parses as a flow-style sequence; force block style so each route
	// renders as a readable, line-per-key mapping.
	seq.Style = 0

	route := newMappingNode()
	// Write fields in RouteConfig's yaml-tag order (match, notifier). The match map
	// is omitted when empty so a fallback route stays compact.
	if len(r.Match) > 0 {
		match := newMappingNode()
		for _, k := range sortedKeys(r.Match) {
			setMapValue(match, k, scalarNode("!!str", r.Match[k]))
		}
		setMapValue(route, "match", match)
	}
	if r.Notifier != "" {
		setMapValue(route, "notifier", scalarNode("!!str", r.Notifier))
	}

	// Idempotent by (notifier, match): replace an existing route with the same
	// destination and identical match in place rather than appending a duplicate.
	if r.Notifier != "" {
		for i, item := range seq.Content {
			if item.Kind != yaml.MappingNode {
				continue
			}
			nt := mapValue(item, "notifier")
			if nt == nil || nt.Value != r.Notifier {
				continue
			}
			if matchEquals(mapValue(item, "match"), r.Match) {
				seq.Content[i] = route
				return writeDocRoot(path, root)
			}
		}
	}

	seq.Content = append(seq.Content, route)
	return writeDocRoot(path, root)
}

// matchEquals reports whether a route's existing "match" mapping node equals the
// desired match map. A nil/absent node equals an empty map (the fallback route).
func matchEquals(node *yaml.Node, want map[string]string) bool {
	if node == nil || node.Kind != yaml.MappingNode {
		return len(want) == 0
	}
	got := make(map[string]string, len(node.Content)/2)
	for i := 0; i+1 < len(node.Content); i += 2 {
		got[node.Content[i].Value] = node.Content[i+1].Value
	}
	if len(got) != len(want) {
		return false
	}
	for k, v := range want {
		if got[k] != v {
			return false
		}
	}
	return true
}

// sortedKeys returns the keys of m in lexical order so a match mapping renders
// deterministically across runs (map iteration order is randomized in Go).
func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// RemoveSiteYAML removes the element of the top-level "sites" sequence whose
// "url" scalar equals url, preserving everything else in the file. It reports
// found=false (and a nil error) when no element matches or when sites is absent
// or empty.
func RemoveSiteYAML(path string, url string) (found bool, err error) {
	root, missing, err := loadDocRoot(path)
	if err != nil {
		return false, err
	}
	if missing {
		return false, nil
	}

	seq := mapValue(root, "sites")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return false, nil
	}

	kept := make([]*yaml.Node, 0, len(seq.Content))
	for _, item := range seq.Content {
		if item.Kind == yaml.MappingNode {
			if u := mapValue(item, "url"); u != nil && u.Value == url {
				found = true
				continue
			}
		}
		kept = append(kept, item)
	}
	if !found {
		return false, nil
	}
	seq.Content = kept

	if err := writeDocRoot(path, root); err != nil {
		return false, err
	}
	return true, nil
}

// SetSiteVerificationYAML sets the verification block (method/token/verified_at)
// on the element of the top-level "sites" sequence whose "url" scalar equals
// siteURL, preserving everything else in the file (sibling sites, comments, and
// secrets such as notifier URLs / access creds). It reports found=false (and a
// nil error) when no site matches or when sites is absent/empty. Only non-empty
// VerificationConfig fields are written.
//
// This block records INTENT only. The daemon re-verifies (Phase 4) and never
// trusts it as proof on its own; the authoritative living state is the DB proof
// record (store.SaveVerification). The token written here is public.
func SetSiteVerificationYAML(path string, siteURL string, v VerificationConfig) (found bool, err error) {
	root, missing, err := loadDocRoot(path)
	if err != nil {
		return false, err
	}
	if missing {
		return false, nil
	}

	seq := mapValue(root, "sites")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return false, nil
	}

	for _, item := range seq.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		u := mapValue(item, "url")
		if u == nil || u.Value != siteURL {
			continue
		}
		// Build (or reuse) the verification mapping on this site, replacing only
		// the child scalars that are non-empty so an unrelated existing child is
		// left intact, and any comment on the key node is preserved.
		ver := mapValue(item, "verification")
		if ver == nil || ver.Kind != yaml.MappingNode {
			ver = newMappingNode()
			setMapValue(item, "verification", ver)
		}
		if v.Method != "" {
			setMapValue(ver, "method", scalarNode("!!str", v.Method))
		}
		if v.Token != "" {
			setMapValue(ver, "token", scalarNode("!!str", v.Token))
		}
		// verified_at tracks a VERIFIED intent. On a non-verified write (e.g. a
		// re-verify that throttled, or any attested-but-unverified intent) the
		// caller passes VerifiedAt == ""; actively REMOVE the key so config.yaml
		// does not keep a stale timestamp that contradicts the authoritative DB
		// state (issue #35). When set, persist it as before.
		if v.VerifiedAt != "" {
			setMapValue(ver, "verified_at", scalarNode("!!str", v.VerifiedAt))
		} else {
			deleteMapKey(ver, "verified_at")
		}
		if err := writeDocRoot(path, root); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// SetSiteMaxPagesYAML sets the per-site discovery page cap
// (sites.<match>.discovery.max_pages_per_site) for the site whose url equals
// siteURL, preserving all unrelated keys and comments. A missing site (or an
// absent/empty sites sequence) is a nil-error no-op.
//
// maxPages is written as an integer: 0 means "unlimited" (an explicit cap of 0,
// which ResolveDiscovery treats as no cap), N>0 caps at N. It walks the sites
// SEQUENCE node by url — SetKeyYAML cannot do this (it only descends mapping
// nodes), so this mirrors SetSiteVerificationYAML's per-site write pattern.
func SetSiteMaxPagesYAML(path string, siteURL string, maxPages int) error {
	root, missing, err := loadDocRoot(path)
	if err != nil {
		return err
	}
	if missing {
		return nil
	}

	seq := mapValue(root, "sites")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return nil
	}

	for _, item := range seq.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		u := mapValue(item, "url")
		if u == nil || u.Value != siteURL {
			continue
		}
		disc := mapValue(item, "discovery")
		if disc == nil || disc.Kind != yaml.MappingNode {
			disc = newMappingNode()
			setMapValue(item, "discovery", disc)
		}
		setMapValue(disc, "max_pages_per_site", scalarNode("!!int", strconv.Itoa(maxPages)))
		return writeDocRoot(path, root)
	}
	return nil
}

// SetSiteGSCYAML sets the per-site Google Search Console block
// (sites.<match>.gsc.{property,auth,service_account_key_file|oauth_token_file}) for
// the site whose url equals siteURL, preserving all unrelated keys, sibling sites,
// and comments. It reports found=false (and a nil error) when no site matches or when
// sites is absent/empty. It mirrors SetSiteVerificationYAML's per-site SEQUENCE walk
// (SetKeyYAML cannot do this — it only descends mapping nodes).
//
// SECRET DISCIPLINE: the credential fields hold a PATH to a 0600 file (mirroring
// control.token), never a credential body — the path string is written verbatim
// (so an ${ENV} reference survives) and is the only secret-adjacent value here. To
// keep the block valid under config.ValidateGSC's mode↔credential mutual exclusion,
// exactly ONE credential key is written and the OTHER is actively REMOVED, so a
// re-write that switches mode (service_account → oauth2 or back) never leaves a stale
// key that would fail validation. Re-writing the same site REPLACES the block in
// place (no duplicate gsc mapping), so re-running the wizard is idempotent.
func SetSiteGSCYAML(path string, siteURL string, g GSCConfig) (found bool, err error) {
	root, missing, err := loadDocRoot(path)
	if err != nil {
		return false, err
	}
	if missing {
		return false, nil
	}

	seq := mapValue(root, "sites")
	if seq == nil || seq.Kind != yaml.SequenceNode {
		return false, nil
	}

	for _, item := range seq.Content {
		if item.Kind != yaml.MappingNode {
			continue
		}
		u := mapValue(item, "url")
		if u == nil || u.Value != siteURL {
			continue
		}
		// Build (or reuse) the gsc mapping on this site so an unrelated existing child
		// and any key comment survive.
		gsc := mapValue(item, "gsc")
		if gsc == nil || gsc.Kind != yaml.MappingNode {
			gsc = newMappingNode()
			setMapValue(item, "gsc", gsc)
		}
		setMapValue(gsc, "property", scalarNode("!!str", g.Property))
		setMapValue(gsc, "auth", scalarNode("!!str", g.Auth))
		// Write exactly the credential key for the chosen mode and REMOVE the other,
		// so a mode switch never leaves a stale key (mutual exclusion, ValidateGSC).
		if g.ServiceAccountKeyFile != "" {
			setMapValue(gsc, "service_account_key_file", scalarNode("!!str", g.ServiceAccountKeyFile))
		} else {
			deleteMapKey(gsc, "service_account_key_file")
		}
		if g.OAuthTokenFile != "" {
			setMapValue(gsc, "oauth_token_file", scalarNode("!!str", g.OAuthTokenFile))
		} else {
			deleteMapKey(gsc, "oauth_token_file")
		}
		if err := writeDocRoot(path, root); err != nil {
			return false, err
		}
		return true, nil
	}
	return false, nil
}

// SetKeyYAML sets a dotted key (e.g. "log.level", "control.port") to value in
// the YAML file at path, creating intermediate mapping nodes as needed and
// preserving unrelated keys and comments. The leaf scalar's type is inferred:
// an integer if value parses as a base-10 int, a bool for "true"/"false",
// otherwise a plain string.
func SetKeyYAML(path string, key string, value string) error {
	parts := splitDotted(key)
	if len(parts) == 0 {
		return fmt.Errorf("config: empty key")
	}

	root, _, err := loadDocRoot(path)
	if err != nil {
		return err
	}

	// Walk/create intermediate mappings for all but the last segment.
	cur := root
	for _, seg := range parts[:len(parts)-1] {
		next := mapValue(cur, seg)
		if next == nil || next.Kind != yaml.MappingNode {
			// Absent, or present but not a mapping (e.g. null) — (re)create it.
			m := newMappingNode()
			setMapValue(cur, seg, m)
			next = m
		}
		cur = next
	}

	leaf := parts[len(parts)-1]
	setMapValue(cur, leaf, inferScalar(value))

	return writeDocRoot(path, root)
}

// splitDotted splits a dotted key into its segments. It avoids a strings.Split
// dependency note: empty segments (from a leading/trailing/double dot) are
// dropped so a malformed key degrades gracefully rather than creating "" keys.
func splitDotted(key string) []string {
	var parts []string
	start := 0
	for i := 0; i < len(key); i++ {
		if key[i] == '.' {
			if i > start {
				parts = append(parts, key[start:i])
			}
			start = i + 1
		}
	}
	if start < len(key) {
		parts = append(parts, key[start:])
	}
	return parts
}

// inferScalar builds a scalar node, choosing the YAML tag by the value's shape:
// integers and the bools true/false are emitted untagged/typed (so they
// round-trip as int/bool), everything else as a plain string.
func inferScalar(value string) *yaml.Node {
	if _, err := strconv.ParseInt(value, 10, 64); err == nil {
		return scalarNode("!!int", value)
	}
	if value == "true" || value == "false" {
		return scalarNode("!!bool", value)
	}
	return scalarNode("!!str", value)
}
