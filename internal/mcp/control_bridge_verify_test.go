package mcpsrv

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

func TestControlBridge_VerifyBeginAndCheck(t *testing.T) {
	t.Parallel()

	var lastReq control.VerifyRequest
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/verify" {
			w.WriteHeader(http.StatusNotFound)
			return
		}
		_ = json.NewDecoder(r.Body).Decode(&lastReq)
		w.Header().Set("Content-Type", "application/json")
		resp := control.VerifyResponse{
			SiteID: lastReq.SiteID, Method: lastReq.Method, Token: "rab_TOK",
			State: "throttled", Throttled: true,
		}
		if lastReq.Action == "begin" {
			resp.Instructions = "Place this file: ..."
		} else {
			resp.Reason = "not_found"
		}
		_ = json.NewEncoder(w).Encode(resp)
	}))
	defer ts.Close()

	client := control.NewClientWithBaseURL(ts.URL, "tok")
	br := NewControlBridge(client) // single-arg per Phase 1's D2 re-point

	v, err := br.VerifyBegin(context.Background(), 7, "well_known")
	if err != nil {
		t.Fatalf("VerifyBegin: %v", err)
	}
	if lastReq.Action != "begin" || lastReq.SiteID != 7 || lastReq.Method != "well_known" {
		t.Fatalf("begin request = %+v, want action=begin site=7 method=well_known", lastReq)
	}
	if v.Token != "rab_TOK" || v.Instructions == "" || !v.Throttled {
		t.Fatalf("begin view = %+v, want token + instructions + throttled", v)
	}

	v2, err := br.VerifyCheck(context.Background(), 7, "well_known")
	if err != nil {
		t.Fatalf("VerifyCheck: %v", err)
	}
	if lastReq.Action != "check" {
		t.Fatalf("check request action = %q, want check", lastReq.Action)
	}
	if v2.Reason != "not_found" {
		t.Fatalf("check view Reason = %q, want not_found", v2.Reason)
	}
}
