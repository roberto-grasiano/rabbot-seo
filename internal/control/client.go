package control

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"time"
)

// Client is the typed control-API client used by the CLI for mutations.
type Client struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewClient builds a client targeting 127.0.0.1:<port>.
func NewClient(port int, token string) *Client {
	return NewClientWithBaseURL("http://"+net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), token)
}

// NewClientWithBaseURL builds a client against an explicit base URL (tests).
func NewClientWithBaseURL(baseURL, token string) *Client {
	return &Client{
		baseURL: baseURL,
		token:   token,
		http:    &http.Client{Timeout: 10 * time.Second},
	}
}

// Health calls GET /v1/health, mapping any transport error to
// ErrDaemonNotRunning and a 401 to ErrUnauthorized.
func (c *Client) Health(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.baseURL+"/v1/health", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return ErrDaemonNotRunning
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		var er ErrorResponse
		_ = json.NewDecoder(resp.Body).Decode(&er)
		return fmt.Errorf("control: unexpected status %d: %s", resp.StatusCode, er.Error)
	}
	return nil
}

// IgnoreIssue marks an issue ignored (POST /v1/issues/{id}/ignore).
func (c *Client) IgnoreIssue(ctx context.Context, id int64) error {
	return c.post(ctx, "/v1/issues/"+strconv.FormatInt(id, 10)+"/ignore", nil, &OKResponse{})
}

// NotifyTest sends a sample alert through a named notifier (POST /v1/notify/test).
func (c *Client) NotifyTest(ctx context.Context, notifier string) error {
	return c.post(ctx, "/v1/notify/test", NotifyTestRequest{Notifier: notifier}, &OKResponse{})
}

// Reload calls POST /v1/reload.
func (c *Client) Reload(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/reload", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return ErrDaemonNotRunning
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("control: reload failed with status %d", resp.StatusCode)
	}
	return nil
}

// Shutdown calls POST /v1/shutdown, asking the daemon to gracefully stop. The
// server replies 202 Accepted BEFORE it begins teardown, so a nil error means
// the request was accepted (the daemon then drains/checkpoints and exits). A
// transport error maps to ErrDaemonNotRunning and a 401 to ErrUnauthorized,
// mirroring Reload.
func (c *Client) Shutdown(ctx context.Context) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/shutdown", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+c.token)
	resp, err := c.http.Do(req)
	if err != nil {
		return ErrDaemonNotRunning
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusUnauthorized {
		return ErrUnauthorized
	}
	// The server returns 202 Accepted (teardown begins after the response is sent).
	if resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("control: shutdown failed with status %d", resp.StatusCode)
	}
	return nil
}
