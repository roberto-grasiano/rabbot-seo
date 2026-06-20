package mcpsrv

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

// ptrBool returns a pointer to b. The MCP ToolAnnotations DestructiveHint and
// OpenWorldHint fields are *bool with a DEFAULT of true, so an action tool MUST
// set them explicitly to advertise the correct (usually closed/non-destructive)
// behaviour — a nil pointer would silently mean "destructive, open-world".
func ptrBool(b bool) *bool { return &b }

// mapBridgeError turns a control-client error into a friendly, actionable message
// for an MCP client. It NEVER leaks the control token or a raw transport error:
// transport failures are normalised by the client to ErrDaemonNotRunning, and a
// 401 to ErrUnauthorized, before they reach here. Any other error is already a
// control-layer message carrying no secret, so it passes through verbatim.
func mapBridgeError(err error) string {
	switch {
	case err == nil:
		return ""
	case errors.Is(err, control.ErrDaemonNotRunning):
		return "daemon not running — it is installed as a service; try restarting it " +
			"(e.g. `rabbot service start`)"
	case errors.Is(err, control.ErrUnauthorized):
		return "token mismatch — the daemon and this client disagree on the data dir/token; " +
			"ensure `rabbot mcp` runs with the same --data-dir/--config as the daemon"
	default:
		return err.Error()
	}
}

// ── Action tool input structs ─────────────────────────────────────────────
// Each field's jsonschema tag becomes the property description the model sees.

// AddSiteInput is the add_site tool input.
type AddSiteInput struct {
	URL         string `json:"url" jsonschema:"the site URL to start monitoring (http or https)"`
	Name        string `json:"name,omitempty" jsonschema:"optional human-friendly name"`
	MinInterval string `json:"min_interval,omitempty" jsonschema:"optional minimum recheck interval, e.g. 10m"`
	MaxInterval string `json:"max_interval,omitempty" jsonschema:"optional maximum recheck interval, e.g. 24h"`
	Speed       int    `json:"speed,omitempty" jsonschema:"optional speed scale (percent)"`
}

// RecheckSiteInput is the recheck_site tool input.
type RecheckSiteInput struct {
	Target string `json:"target,omitempty" jsonschema:"a site base URL or page URL to recheck now; empty rechecks all enabled sites"`
}

// IgnoreIssueInput is the ignore_issue tool input.
type IgnoreIssueInput struct {
	IssueID int64 `json:"issue_id" jsonschema:"the numeric id of the issue to mark ignored"`
}

// SendTestAlertInput is the send_test_alert tool input.
type SendTestAlertInput struct {
	Notifier string `json:"notifier,omitempty" jsonschema:"optional name of the configured notifier to send a sample alert through"`
}

// noArgs is the empty input for zero-argument action tools (pause/resume).
type noArgs struct{}

// OKResult is the structured output of the zero-arg / ack action tools.
type OKResult struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// registerActionTools wires the six Phase-3 write tools onto srv, each backed by
// the Bridge. It is called from NewServer after the read resources/tools are added.
func registerActionTools(srv *mcp.Server, b Bridge) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        "add_site",
		Title:       "Add site",
		Description: "Start monitoring a new site for SEO-relevant changes.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: ptrBool(false),
			IdempotentHint:  false,
			OpenWorldHint:   ptrBool(true),
		},
	}, addSiteHandler(b))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "recheck_site",
		Title:       "Recheck site",
		Description: "Force an immediate recheck of a site or page now; empty target rechecks all enabled sites.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: ptrBool(false),
			IdempotentHint:  true,
			OpenWorldHint:   ptrBool(true),
		},
	}, recheckSiteHandler(b))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "pause_monitoring",
		Title:       "Pause monitoring",
		Description: "Turn on the global crawl kill-switch; no sites are rechecked until resumed.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: ptrBool(false),
			IdempotentHint:  true,
			OpenWorldHint:   ptrBool(false),
		},
	}, pauseHandler(b))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "resume_monitoring",
		Title:       "Resume monitoring",
		Description: "Turn off the global crawl kill-switch; monitoring resumes on the normal schedule.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: ptrBool(false),
			IdempotentHint:  true,
			OpenWorldHint:   ptrBool(false),
		},
	}, resumeHandler(b))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ignore_issue",
		Title:       "Ignore issue",
		Description: "Mark an open issue as ignored so it stops being reported.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: ptrBool(false),
			IdempotentHint:  true,
			OpenWorldHint:   ptrBool(false),
		},
	}, ignoreIssueHandler(b))

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "send_test_alert",
		Title:       "Send test alert",
		Description: "Send a synthetic alert through a configured notifier to verify the alert wiring.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: ptrBool(false),
			IdempotentHint:  true,
			OpenWorldHint:   ptrBool(true),
		},
	}, sendTestAlertHandler(b))
}

func addSiteHandler(b Bridge) mcp.ToolHandlerFor[AddSiteInput, control.AddSiteResponse] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in AddSiteInput) (*mcp.CallToolResult, control.AddSiteResponse, error) {
		resp, err := b.AddSite(ctx, control.AddSiteRequest{
			URL:         in.URL,
			Name:        in.Name,
			MinInterval: in.MinInterval,
			MaxInterval: in.MaxInterval,
			Speed:       in.Speed,
		})
		if err != nil {
			return nil, control.AddSiteResponse{}, errors.New(mapBridgeError(err))
		}
		return nil, resp, nil
	}
}

func recheckSiteHandler(b Bridge) mcp.ToolHandlerFor[RecheckSiteInput, control.CrawlResponse] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in RecheckSiteInput) (*mcp.CallToolResult, control.CrawlResponse, error) {
		resp, err := b.Recheck(ctx, in.Target)
		if err != nil {
			return nil, control.CrawlResponse{}, errors.New(mapBridgeError(err))
		}
		return nil, resp, nil
	}
}

func pauseHandler(b Bridge) mcp.ToolHandlerFor[noArgs, OKResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, OKResult, error) {
		if err := b.Pause(ctx); err != nil {
			return nil, OKResult{}, errors.New(mapBridgeError(err))
		}
		return nil, OKResult{OK: true}, nil
	}
}

func resumeHandler(b Bridge) mcp.ToolHandlerFor[noArgs, OKResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, _ noArgs) (*mcp.CallToolResult, OKResult, error) {
		if err := b.Resume(ctx); err != nil {
			return nil, OKResult{}, errors.New(mapBridgeError(err))
		}
		return nil, OKResult{OK: true}, nil
	}
}

func ignoreIssueHandler(b Bridge) mcp.ToolHandlerFor[IgnoreIssueInput, OKResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in IgnoreIssueInput) (*mcp.CallToolResult, OKResult, error) {
		if err := b.IgnoreIssue(ctx, in.IssueID); err != nil {
			return nil, OKResult{}, errors.New(mapBridgeError(err))
		}
		return nil, OKResult{OK: true}, nil
	}
}

func sendTestAlertHandler(b Bridge) mcp.ToolHandlerFor[SendTestAlertInput, OKResult] {
	return func(ctx context.Context, _ *mcp.CallToolRequest, in SendTestAlertInput) (*mcp.CallToolResult, OKResult, error) {
		if err := b.TestAlert(ctx, in.Notifier); err != nil {
			return nil, OKResult{}, errors.New(mapBridgeError(err))
		}
		return nil, OKResult{OK: true}, nil
	}
}
