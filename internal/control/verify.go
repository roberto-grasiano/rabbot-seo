package control

import (
	"context"
	"errors"
	"net/http"
)

// handleVerify runs the daemon-owned verify flow (POST /v1/verify). It mirrors
// handleAddSite: 1 MiB body cap via decodeBody (a malformed body -> 400, an
// oversized body -> 413), then the hook. A hook error wrapping ErrBadRequest
// (unknown method/action, unknown site) maps to 400; any other error is a server
// fault -> 500. The derived token in the response is PUBLIC (placement is the
// proof), so it is safe to emit.
func (s *Server) handleVerify(w http.ResponseWriter, r *http.Request) {
	if s.hooks.Verify == nil {
		notImplemented(w, "verify")
		return
	}
	var req VerifyRequest
	if !decodeBody(w, r, &req) {
		return
	}
	resp, err := s.hooks.Verify(r.Context(), req)
	if err != nil {
		status := http.StatusInternalServerError
		if errors.Is(err, ErrBadRequest) {
			status = http.StatusBadRequest
		}
		writeJSON(w, status, ErrorResponse{Error: err.Error()})
		return
	}
	writeJSON(w, http.StatusOK, resp)
}

// Verify calls POST /v1/verify with the begin/check request, decoding the response.
func (c *Client) Verify(ctx context.Context, req VerifyRequest) (VerifyResponse, error) {
	var resp VerifyResponse
	if err := c.post(ctx, "/v1/verify", req, &resp); err != nil {
		return VerifyResponse{}, err
	}
	return resp, nil
}
