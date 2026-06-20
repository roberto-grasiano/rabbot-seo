// Package mcpsrv exposes the Rabbot-SEO Model Context Protocol (MCP) server
// over STDIO ONLY. It is the full read + safe-actions surface: an MCP host (e.g.
// Claude) reads the monitor's state AND drives a curated set of guarded actions,
// all through the daemon's loopback control API.
//
// # Scope (resources + tools)
//
// Three fixed, read-only RESOURCES stay for @-mention:
//
//   - rabbot://health  — is the daemon reachable on the loopback control API?
//   - rabbot://status  — the daemon's StatusResponse (version, counts, paused…)
//   - rabbot://sites    — the monitored sites, as a read-only SiteView array
//
// Plus a TOOL catalog the model invokes on demand. Five read tools:
//
//   - get_status   — daemon version/uptime/paused + site/URL/queue counts
//   - list_sites   — every monitored site with its id and verification tier
//   - get_site     — per-site detail: tier, cadence, open-issue count, latest SEO fields
//   - list_issues  — detected issues, filterable by site / severity / status
//   - get_history  — a monitored URL's recorded SEO change history
//
// Six write/action tools (each maps 1:1 to an existing control endpoint):
//
//   - add_site          — start monitoring a new site
//   - recheck_site      — force an immediate recheck (empty target = all sites)
//   - pause_monitoring  — turn on the global crawl kill-switch
//   - resume_monitoring — turn off the global crawl kill-switch
//   - ignore_issue      — mark an open issue ignored
//   - send_test_alert   — send a sample alert through a configured notifier
//
// One guarded config-write tool:
//
//   - set_config — set an ALLOW-LISTED config key only; the value is never echoed
//     back, and the allow-list is enforced both in the tool layer (fast rejection)
//     and authoritatively in the daemon.
//
// Two ownership-verification tools:
//
//   - verify_begin — derive the instance-bound proof token + placement
//     instructions (read-only: writes nothing)
//   - verify_check — fetch the placed proof and, on a match, record the site as
//     verified (lifting the unverified throttle); a clean miss leaves it throttled
//
// Each tool carries MCP ToolAnnotations (ReadOnlyHint / DestructiveHint /
// IdempotentHint / OpenWorldHint) so the host can surface the right confirmation
// prompt. None of the write tools is marked destructive.
//
// # All I/O flows through the daemon's loopback control API
//
// Every read AND every write routes through the loopback control client (the
// daemon stays the single DB writer). In particular sites/site/issues/history are
// served over the daemon's control read endpoints (e.g. GET /v1/sites, which
// returns tier-enriched summaries resolved server-side), so the `rabbot mcp`
// child process opens NO SQLite database — it is fully decoupled from the on-disk
// DB path and needs only loopback reachability + the control token.
//
// # Hard constraints
//
//   - STDIO ONLY. There is no HTTP / network MCP endpoint here — no
//     StreamableHTTP, no net.Listen. The only transport is mcp.StdioTransport,
//     whose JSON-RPC channel is stdout, so NOTHING in this package may write to
//     stdout: any framework logging goes to a slog.Logger over os.Stderr (or nil).
//   - The control plane stays loopback + token-auth: all resources and tools reach
//     the daemon over the existing loopback *control.Client; nothing here binds a
//     socket or widens the network surface.
//   - The control token is held only inside the control.Client's Authorization
//     header — it is NEVER logged, printed, or embedded in any snippet/output.
//   - Errors are returned as data where it helps the model self-correct: a
//     down/unauthorized daemon and an unknown id/URL are reported as a clean
//     payload (friendly message / not_found), not a crashed resource. Token leaks
//     are scrubbed by mapBridgeError before any message reaches the client.
//
// # The Bridge seam
//
// Handlers depend on the small Bridge interface (not the concrete *control.Client),
// so they are unit-tested against a mock that needs no live daemon and no store.
// The production implementation (controlBridge) wraps a single loopback control
// client for every method — health, status, the reads, the writes, and verify all
// go through it. This is the same expandable seam introduced by the read-only
// slice: the Connect-Claude launch spec (run `rabbot mcp` over stdio) keeps
// working unchanged as the catalog grows — only the Bridge's production impl moves,
// never its launch contract.
package mcpsrv
