package control

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestVerify_NilHookReturns501(t *testing.T) {
	ts := newTestServer(Hooks{}) // no Verify hook wired
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/verify",
		strings.NewReader(`{"site_id":1,"method":"well_known","action":"begin"}`))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusNotImplemented {
		t.Errorf("status = %d, want 501", resp.StatusCode)
	}
}

func TestVerify_RequiresToken(t *testing.T) {
	called := false
	ts := newTestServer(Hooks{
		Verify: func(context.Context, VerifyRequest) (VerifyResponse, error) {
			called = true
			return VerifyResponse{}, nil
		},
	})
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/verify",
		strings.NewReader(`{"site_id":1,"method":"meta","action":"begin"}`))
	// no Authorization header
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("status = %d, want 401", resp.StatusCode)
	}
	if called {
		t.Error("Verify hook fired on an unauthorized request")
	}
}

func TestVerify_BeginReturnsTokenAndInstructions(t *testing.T) {
	ts := newTestServer(Hooks{
		Verify: func(_ context.Context, req VerifyRequest) (VerifyResponse, error) {
			return VerifyResponse{
				SiteID:       req.SiteID,
				Method:       req.Method,
				Token:        "rab_DERIVED",
				State:        "throttled",
				Instructions: "Place this file: ...",
				Throttled:    true,
			}, nil
		},
	})
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/verify",
		strings.NewReader(`{"site_id":7,"method":"well_known","action":"begin"}`))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		t.Fatalf("status = %d (body=%s), want 200", resp.StatusCode, body)
	}
	var got VerifyResponse
	if err := json.NewDecoder(resp.Body).Decode(&got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.SiteID != 7 || got.Token != "rab_DERIVED" || !got.Throttled {
		t.Fatalf("response = %+v, want site 7 / derived token / throttled", got)
	}
}

func TestVerify_BadRequestMapsTo400(t *testing.T) {
	ts := newTestServer(Hooks{
		Verify: func(context.Context, VerifyRequest) (VerifyResponse, error) {
			// A caller fault (unknown method/action) wraps ErrBadRequest.
			return VerifyResponse{}, ErrBadRequest
		},
	})
	t.Cleanup(ts.Close)

	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/verify",
		strings.NewReader(`{"site_id":1,"method":"bogus","action":"begin"}`))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusBadRequest {
		t.Errorf("status = %d, want 400", resp.StatusCode)
	}
}

func TestVerify_BodyCapEnforced(t *testing.T) {
	ts := newTestServer(Hooks{
		Verify: func(context.Context, VerifyRequest) (VerifyResponse, error) {
			return VerifyResponse{}, nil
		},
	})
	t.Cleanup(ts.Close)

	// > 1 MiB body must be rejected by http.MaxBytesReader at decode time, which
	// decodeBody maps to 413 Request Entity Too Large (finding Low-413).
	big := `{"action":"` + strings.Repeat("a", (1<<20)+16) + `"}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/v1/verify", strings.NewReader(big))
	req.Header.Set("Authorization", "Bearer tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Errorf("oversized body status = %d, want 413", resp.StatusCode)
	}
}
