package mcpsrv

import (
	"context"
	"errors"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// VerifyInput is the input for both verify tools: the numeric site id and the
// proof method. The token is DERIVED daemon-side — there is no token input, so no
// caller value can become the match target.
type VerifyInput struct {
	SiteID int64  `json:"site_id" jsonschema:"the numeric id of the site to verify"`
	Method string `json:"method" jsonschema:"proof method: well_known, dns, or meta"`
}

// registerVerifyTools registers verify_begin (read-only: derive token + placement
// instructions, no DB write) and verify_check (action: run the proof fetch and
// persist the record). Both go through the daemon's POST /v1/verify (single
// writer). Errors are mapped to a friendly message and returned as a tool error
// (IsError) — never leaking the control token or a raw transport error.
func registerVerifyTools(srv *mcp.Server, b Bridge) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:  "verify_begin",
		Title: "Begin site ownership verification",
		Description: "Returns the instance-bound proof token and placement instructions " +
			"for a site (well-known file / DNS TXT / homepage meta). Writes nothing — " +
			"use verify_check after placing the proof.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:  true,
			OpenWorldHint: ptrBool(true),
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in VerifyInput) (*mcp.CallToolResult, VerifyView, error) {
		v, err := b.VerifyBegin(ctx, in.SiteID, in.Method)
		if err != nil {
			return nil, VerifyView{}, errors.New(mapBridgeError(err))
		}
		return nil, v, nil
	})

	mcp.AddTool(srv, &mcp.Tool{
		Name:  "verify_check",
		Title: "Check site ownership proof and lift the throttle",
		Description: "Fetches the placed proof and, if it matches the instance-bound " +
			"token, records the site as verified (lifting the unverified throttle). " +
			"A clean miss leaves the site throttled — it is never spoofable by a " +
			"caller-supplied value.",
		Annotations: &mcp.ToolAnnotations{
			ReadOnlyHint:    false,
			DestructiveHint: ptrBool(false),
			IdempotentHint:  true,
			OpenWorldHint:   ptrBool(true),
		},
	}, func(ctx context.Context, _ *mcp.CallToolRequest, in VerifyInput) (*mcp.CallToolResult, VerifyView, error) {
		v, err := b.VerifyCheck(ctx, in.SiteID, in.Method)
		if err != nil {
			return nil, VerifyView{}, errors.New(mapBridgeError(err))
		}
		return nil, v, nil
	})
}
