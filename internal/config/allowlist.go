package config

import (
	"fmt"
	"sort"
	"strings"
)

// allowedConfigKeys is the EXACT set of config keys settable over the control
// plane (the `config set` control endpoint, reached by both `rabbot config set`
// and the MCP set_config tool). Anything not in this set — and anything matching a
// DenyConfigKey prefix, which always wins — is rejected with NO write.
//
// The set is deliberately tiny: operator-tunable, non-secret, non-floor keys only.
//   - log.level                            — log verbosity (debug|info|warn|error)
//   - defaults.min_interval                — normal (verified-tier) recheck cadence, e.g. "10m"
//   - defaults.max_interval                — normal-tier max recheck cadence, e.g. "24h"
//   - defaults.discovery.max_pages_per_site — per-site page cap (N>0 = cap, 0 = all);
//     the remedy advertised by `status`/`sites`/`init`. Not under the
//     defaults.unverified_throttle.* floor, so the safety floor stays untouched.
//
// NOT in the set (intentionally, see plan §6): a global alerting on/off — there is
// no AlertingConfig.Enabled field today, so advertising "alerting.enabled" would
// write to a struct field nothing reads. Deferred until such a field exists.
//   - crawler.hydration.enabled            — A8 hydration-payload recovery on/off.
//     Non-secret, non-floor; toggling it changes only whether DOM-empty fields are
//     back-filled from embedded framework state. The companion
//     crawler.hydration.max_payload_bytes is deliberately NOT settable here (it
//     bounds resource use — a DoS guard — so it stays file/env-only, mirroring
//     metrics.addr).
//   - graph.enabled                        — A9 link-graph LITE on/off.
//   - graph.sweep_interval                 — A9 click-depth BFS cadence, e.g. "6h".
//     ONLY these two graph keys are settable. The cap knobs
//     graph.max_outlinks_per_page / graph.export_max_nodes / graph.export_max_edges
//     are deliberately NOT here: they are RESOURCE BOUNDS (a DoS surface), so a
//     control-plane caller must not be able to raise them. They stay file/env-only,
//     mirroring crawler.hydration.max_payload_bytes and metrics.addr. This
//     asymmetry — every ADVERTISED-settable key is allow-listed, but the bounds are
//     deliberately not advertised as settable — is intentional, not an oversight.
var allowedConfigKeys = map[string]struct{}{
	"log.level":                             {},
	"defaults.min_interval":                 {},
	"defaults.max_interval":                 {},
	"defaults.discovery.max_pages_per_site": {},
	"crawler.hydration.enabled":             {},
	"graph.enabled":                         {},
	"graph.sweep_interval":                  {},
}

// DenyConfigKey reports why a key is denied over the control plane, or "" if the
// key is not in a denied family. The DENY rule is the authoritative safety floor:
// any key under these prefixes is ALWAYS rejected regardless of the allow-list, so
// no caller (CLI or MCP) can loosen the unverified-throttle floor, set a notifier
// secret, or relocate the database over the control plane.
func DenyConfigKey(key string) string {
	switch {
	case strings.HasPrefix(key, "defaults.unverified_throttle."):
		return "the unverified throttle floor cannot be changed over the control plane"
	case strings.HasPrefix(key, "notifiers."):
		return "notifier settings (which may contain secrets) cannot be set over the control plane"
	case strings.HasPrefix(key, "database.") || key == "data_dir":
		return "the database/data location cannot be changed over the control plane"
	}
	return ""
}

// AllowConfigKey returns nil if key is settable over the control plane, else a
// clear, actionable error. DENY always wins over ALLOW: a denied key is rejected
// with its deny reason; any other non-allow-listed key is rejected with the list
// of keys that ARE settable. The key is never mutated and nothing is written.
func AllowConfigKey(key string) error {
	if reason := DenyConfigKey(key); reason != "" {
		return fmt.Errorf("config: key %q is not settable over the control plane: %s", key, reason)
	}
	if _, ok := allowedConfigKeys[key]; ok {
		return nil
	}
	return fmt.Errorf("config: key %q is not settable over the control plane; allowed keys: %s",
		key, strings.Join(sortedAllowedKeys(), ", "))
}

// sortedAllowedKeys returns the allow-listed keys in deterministic order for a
// stable, testable error message.
func sortedAllowedKeys() []string {
	keys := make([]string, 0, len(allowedConfigKeys))
	for k := range allowedConfigKeys {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}
