package mcpsrv

import (
	"context"
	"errors"
	"io"
	"strings"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// serverInstructions is the human-readable hint shown to MCP clients about what
// this server exposes. It is intentionally terse. The surface is reads plus a set
// of explicitly annotated mutating actions; every mutating tool runs only when the
// MCP host approves it (the host's per-call permission prompt is the gate).
const serverInstructions = "Rabbot-SEO monitor. Resources: " +
	"rabbot://health (daemon reachable?), rabbot://status (daemon state), " +
	"rabbot://sites (monitored sites). Read tools report status, sites, issues, " +
	"and change history; action tools (add/recheck/pause/resume a site, ignore an " +
	"issue, send a test alert, set an allow-listed config key, verify control) " +
	"mutate state and are gated by your MCP host's approval prompt."

// NewServer builds the read-only MCP server and registers the three fixed
// resources against the given Bridge. version is the rabbot build version,
// surfaced as the MCP server Implementation version.
//
// No Logger is configured (nil): stdout is the JSON-RPC channel under the stdio
// transport, so framework logging must never go there. A caller that wants server
// logs should pass a slog.Logger over os.Stderr — but the default here is silence
// rather than risk a stray stdout write.
//
// AddResource panics on a non-absolute URI; all three URIs are absolute
// rabbot:// URIs (see handlers.go), so construction is panic-free.
func NewServer(b Bridge, version string) *mcp.Server {
	srv := mcp.NewServer(
		&mcp.Implementation{Name: "rabbot-seo", Version: version},
		&mcp.ServerOptions{Instructions: serverInstructions},
	)

	srv.AddResource(&mcp.Resource{
		Name:        "health",
		URI:         uriHealth,
		Description: "Whether the Rabbot-SEO daemon is reachable on the loopback control API.",
		MIMEType:    mimeJSON,
	}, healthHandler(b))

	srv.AddResource(&mcp.Resource{
		Name:        "status",
		URI:         uriStatus,
		Description: "The Rabbot-SEO daemon's status: version, site/URL/queue counts, paused state.",
		MIMEType:    mimeJSON,
	}, statusHandler(b))

	srv.AddResource(&mcp.Resource{
		Name:        "sites",
		URI:         uriSites,
		Description: "The monitored sites as a read-only array (id, url, name, enabled, verification state).",
		MIMEType:    mimeJSON,
	}, sitesHandler(b))

	// Register the read-only action tools (get_status, list_sites, get_site,
	// list_issues, get_history). Tools are the model-driven read surface; the three
	// resources above stay for @-mention (D3).
	registerReadTools(srv, b)

	// Phase 3: register the write/action tools on the same server. The read
	// resources/tools above and these tools share one Bridge.
	registerActionTools(srv, b)

	// Phase 4: write tool — set an allow-listed config key. Registered on the
	// server alongside the read-only resources; the allow-list + secret-safe echo
	// live in the handler (set_config never returns the value).
	setConfigTool(srv, b)

	// Phase 5: verify tools — verify_begin (read-only: derive token + placement
	// instructions, no DB write) and verify_check (action: run the proof fetch and
	// persist the record). Both route through the daemon's POST /v1/verify so the
	// daemon stays the single DB writer.
	registerVerifyTools(srv, b)

	return srv
}

// Serve builds the server for the given Bridge and runs it over the stdio
// transport, blocking until the client disconnects or ctx is cancelled. STDIO is
// the ONLY transport: stdout is the JSON-RPC channel, so the caller must not write
// to stdout. On ctx cancellation, Run closes the connection and returns.
//
// A NORMAL client disconnect is not a failure: when the MCP host closes stdin, the
// transport's read hits io.EOF and Server.Run returns an error wrapping the SDK's
// internal "server is closing" / EOF condition (and a cancelled ctx returns
// context.Canceled). Serve normalizes those routine terminations to a nil error so
// the caller exits cleanly; any other error is returned verbatim.
func Serve(ctx context.Context, b Bridge, version string) error {
	err := NewServer(b, version).Run(ctx, &mcp.StdioTransport{})
	if isGracefulDisconnect(err) {
		return nil
	}
	return err
}

// isGracefulDisconnect reports whether err is a routine end-of-session condition
// (client closed stdin / connection, or ctx cancelled) rather than a real failure.
//
// The SDK builds the disconnect error as fmt.Errorf("%w: %v", ErrServerClosing,
// io.EOF) where ErrServerClosing lives in an unexported internal package and io.EOF
// is formatted with %v (so errors.Is(err, io.EOF) does not match). We therefore
// match the public, importable signals (context cancellation, ErrConnectionClosed,
// io.EOF if a future SDK version wraps it with %w) and fall back to the stable
// "server is closing" / "connection closed" message substrings for the current
// SDK's disconnect shapes. We deliberately do NOT match a bare "EOF" suffix: that
// is already covered by the substring/errors.Is checks above and would wrongly
// swallow genuine failures like "unexpected EOF" (a truncated read).
func isGracefulDisconnect(err error) bool {
	if err == nil {
		return true
	}
	if errors.Is(err, context.Canceled) ||
		errors.Is(err, io.EOF) ||
		errors.Is(err, mcp.ErrConnectionClosed) {
		return true
	}
	msg := err.Error()
	return strings.Contains(msg, "server is closing") ||
		strings.Contains(msg, "connection closed")
}
