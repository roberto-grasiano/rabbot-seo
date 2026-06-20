package control

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
)

// Pause turns on the global crawl kill-switch (POST /v1/pause).
func (c *Client) Pause(ctx context.Context) error {
	return c.post(ctx, "/v1/pause", nil, &OKResponse{})
}

// Resume turns off the global crawl kill-switch (POST /v1/resume).
func (c *Client) Resume(ctx context.Context) error {
	return c.post(ctx, "/v1/resume", nil, &OKResponse{})
}

// RemoveSite removes/disables a site (DELETE /v1/sites/{id}); purge=true deletes history.
func (c *Client) RemoveSite(ctx context.Context, id string, purge bool) error {
	path := "/v1/sites/" + url.PathEscape(id)
	if purge {
		path += "?purge=true"
	}
	return c.do(ctx, http.MethodDelete, path, nil, &OKResponse{})
}

// SetConfig applies a config mutation (POST /v1/config).
func (c *Client) SetConfig(ctx context.Context, key, value string) error {
	return c.post(ctx, "/v1/config", ConfigSetRequest{Key: key, Value: value}, &OKResponse{})
}

// Status fetches daemon state (GET /v1/status).
func (c *Client) Status(ctx context.Context) (StatusResponse, error) {
	var resp StatusResponse
	if err := c.do(ctx, http.MethodGet, "/v1/status", nil, &resp); err != nil {
		return StatusResponse{}, err
	}
	return resp, nil
}

// post is the JSON-POST convenience over do.
func (c *Client) post(ctx context.Context, path string, body, out any) error {
	return c.do(ctx, http.MethodPost, path, body, out)
}

// do performs one authenticated JSON round-trip against the control API, using
// the same base URL, bearer-token, and error mapping as M0's inlined methods.
func (c *Client) do(ctx context.Context, method, path string, body, out any) error {
	var rdr *bytes.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return err
		}
		rdr = bytes.NewReader(b)
	} else {
		rdr = bytes.NewReader(nil)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.baseURL+path, rdr)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return ErrDaemonNotRunning // connection refused => daemon down
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		var er ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&er)
		return fmt.Errorf("control %s %s: status %d: %s", method, path, resp.StatusCode, er.Error)
	}
	if out != nil {
		return json.NewDecoder(resp.Body).Decode(out)
	}
	return nil
}
