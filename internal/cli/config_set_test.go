package cli

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

func TestRunConfigSet(t *testing.T) {
	var gotKey, gotValue string
	ts := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/config" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		body, _ := io.ReadAll(r.Body)
		var req control.ConfigSetRequest
		_ = json.Unmarshal(body, &req)
		gotKey, gotValue = req.Key, req.Value
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(control.OKResponse{OK: true})
	}))
	defer ts.Close()

	client := control.NewClientWithBaseURL(ts.URL, "test-token") // M0 httptest-friendly constructor
	if err := runConfigSet(context.Background(), client, "log.level=debug"); err != nil {
		t.Fatalf("runConfigSet: %v", err)
	}
	if gotKey != "log.level" || gotValue != "debug" {
		t.Errorf("ConfigSetRequest = %q=%q, want log.level=debug", gotKey, gotValue)
	}
}
