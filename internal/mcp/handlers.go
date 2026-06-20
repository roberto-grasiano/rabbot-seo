package mcpsrv

import (
	"context"
	"encoding/json"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// The three fixed, absolute resource URIs. They MUST be absolute (non-empty
// scheme) — Server.AddResource panics otherwise. Spec 2 may add more URIs but
// must not change these three: the Connect-Claude snippet keeps working because
// the URIs are stable.
const (
	uriHealth = "rabbot://health"
	uriStatus = "rabbot://status"
	uriSites  = "rabbot://sites"
)

const mimeJSON = "application/json"

// jsonResult wraps a marshalled JSON payload as the single content block of a
// ReadResourceResult, echoing the requested URI (per the MCP contract). Marshal
// failures fall back to an inline JSON error object so a resource never returns a
// raw Go error to the client for an internal encoding problem.
func jsonResult(uri string, payload any) *mcp.ReadResourceResult {
	b, err := json.Marshal(payload)
	if err != nil {
		// Should be unreachable for our small, static payloads; degrade to a JSON
		// error object rather than panicking or leaking a Go error.
		b = []byte(`{"error":"internal: failed to encode resource payload"}`)
	}
	return &mcp.ReadResourceResult{
		Contents: []*mcp.ResourceContents{{
			URI:      uri,
			MIMEType: mimeJSON,
			Text:     string(b),
		}},
	}
}

// healthHandler reads rabbot://health. A reachable daemon yields
// {"healthy":true}; an unreachable one yields {"healthy":false,"error":<msg>} with
// a NIL Go error — a down daemon is reported as DATA, never as a crashed resource,
// so an MCP client always gets a clean read. The control token is never included.
func healthHandler(b Bridge) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		type payload struct {
			Healthy bool   `json:"healthy"`
			Error   string `json:"error,omitempty"`
		}
		p := payload{Healthy: true}
		if err := b.Health(ctx); err != nil {
			p.Healthy = false
			p.Error = err.Error()
		}
		return jsonResult(req.Params.URI, p), nil
	}
}

// statusHandler reads rabbot://status, returning the daemon's StatusResponse as
// JSON. A fetch failure (e.g. daemon down) reads as {"error":<msg>} with a nil Go
// error, mirroring health — the resource always reads cleanly.
func statusHandler(b Bridge) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		st, err := b.Status(ctx)
		if err != nil {
			return jsonResult(req.Params.URI, errorPayload{Error: err.Error()}), nil
		}
		return jsonResult(req.Params.URI, st), nil
	}
}

// sitesHandler reads rabbot://sites, returning the monitored sites as a JSON
// array of SiteView. An empty list serializes as [] (never null). A fetch failure
// reads as {"error":<msg>} with a nil Go error.
func sitesHandler(b Bridge) mcp.ResourceHandler {
	return func(ctx context.Context, req *mcp.ReadResourceRequest) (*mcp.ReadResourceResult, error) {
		sites, err := b.Sites(ctx)
		if err != nil {
			return jsonResult(req.Params.URI, errorPayload{Error: err.Error()}), nil
		}
		// Normalize nil to an empty slice so the payload is [] not null.
		if sites == nil {
			sites = []SiteView{}
		}
		return jsonResult(req.Params.URI, sites), nil
	}
}

// errorPayload is the small JSON shape used when a read cannot be served as its
// normal payload (daemon down, store error). It never carries secrets.
type errorPayload struct {
	Error string `json:"error"`
}
