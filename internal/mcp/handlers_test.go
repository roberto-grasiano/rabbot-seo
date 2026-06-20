package mcpsrv

import (
	"context"
	"encoding/json"
	"reflect"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/roberto-grasiano/rabbot-seo/internal/control"
)

// readReq builds a minimal ReadResourceRequest carrying the requested URI, which
// is all the read-only handlers consult.
func readReq(uri string) *mcp.ReadResourceRequest {
	return &mcp.ReadResourceRequest{Params: &mcp.ReadResourceParams{URI: uri}}
}

// singleContent asserts the result has exactly one JSON content block echoing the
// requested URI, and returns its Text for further JSON assertions.
func singleContent(t *testing.T, res *mcp.ReadResourceResult, wantURI string) string {
	t.Helper()
	if res == nil {
		t.Fatal("nil ReadResourceResult")
	}
	if len(res.Contents) != 1 {
		t.Fatalf("Contents len = %d, want 1", len(res.Contents))
	}
	c := res.Contents[0]
	if c.MIMEType != "application/json" {
		t.Fatalf("MIMEType = %q, want application/json", c.MIMEType)
	}
	if c.URI != wantURI {
		t.Fatalf("Contents[0].URI = %q, want %q (must echo the requested URI)", c.URI, wantURI)
	}
	return c.Text
}

func TestHandlerHealth_Up(t *testing.T) {
	t.Parallel()

	h := healthHandler(&mockBridge{healthErr: nil})
	res, err := h(context.Background(), readReq(uriHealth))
	if err != nil {
		t.Fatalf("handler returned Go error: %v", err)
	}
	text := singleContent(t, res, uriHealth)

	var got struct {
		Healthy bool   `json:"healthy"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("payload not JSON: %v (%s)", err, text)
	}
	if !got.Healthy {
		t.Fatalf("healthy = false, want true (text=%s)", text)
	}
	if got.Error != "" {
		t.Fatalf("error = %q, want empty when healthy", got.Error)
	}
}

func TestHandlerHealth_Down(t *testing.T) {
	t.Parallel()

	// A down daemon must read as DATA, not crash the resource: the handler returns
	// a non-nil result with healthy:false + the error string, and a NIL Go error.
	h := healthHandler(&mockBridge{healthErr: control.ErrDaemonNotRunning})
	res, err := h(context.Background(), readReq(uriHealth))
	if err != nil {
		t.Fatalf("down daemon must not be a Go error, got: %v", err)
	}
	text := singleContent(t, res, uriHealth)

	var got struct {
		Healthy bool   `json:"healthy"`
		Error   string `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("payload not JSON: %v (%s)", err, text)
	}
	if got.Healthy {
		t.Fatalf("healthy = true, want false when daemon down")
	}
	if got.Error != control.ErrDaemonNotRunning.Error() {
		t.Fatalf("error = %q, want %q", got.Error, control.ErrDaemonNotRunning.Error())
	}
}

func TestHandlerStatus(t *testing.T) {
	t.Parallel()

	want := control.StatusResponse{Version: "9.9.9", SiteCount: 3, Paused: true, URLCount: 12}
	h := statusHandler(&mockBridge{status: want})
	res, err := h(context.Background(), readReq(uriStatus))
	if err != nil {
		t.Fatalf("handler returned Go error: %v", err)
	}
	text := singleContent(t, res, uriStatus)

	var got control.StatusResponse
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("payload not JSON: %v (%s)", err, text)
	}
	if got.Version != "9.9.9" || got.SiteCount != 3 || !got.Paused || got.URLCount != 12 {
		t.Fatalf("status payload = %+v, want %+v", got, want)
	}
}

func TestHandlerStatus_BridgeError(t *testing.T) {
	t.Parallel()

	// A status fetch failure (daemon down) reads as data too, not a crashed
	// resource: handler returns a non-nil result and a nil Go error.
	h := statusHandler(&mockBridge{statusErr: control.ErrDaemonNotRunning})
	res, err := h(context.Background(), readReq(uriStatus))
	if err != nil {
		t.Fatalf("status error must not be a Go error, got: %v", err)
	}
	text := singleContent(t, res, uriStatus)
	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("payload not JSON: %v (%s)", err, text)
	}
	if got.Error != control.ErrDaemonNotRunning.Error() {
		t.Fatalf("error = %q, want %q", got.Error, control.ErrDaemonNotRunning.Error())
	}
}

func TestHandlerSites(t *testing.T) {
	t.Parallel()

	want := []SiteView{
		{ID: 1, URL: "https://a.test", Name: "A", Enabled: true, VerificationState: "verified"},
		{ID: 2, URL: "https://b.test", Name: "B", Enabled: false, VerificationState: ""},
	}
	h := sitesHandler(&mockBridge{sites: want})
	res, err := h(context.Background(), readReq(uriSites))
	if err != nil {
		t.Fatalf("handler returned Go error: %v", err)
	}
	text := singleContent(t, res, uriSites)

	var got []SiteView
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("payload not a JSON array: %v (%s)", err, text)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("sites payload = %+v, want %+v", got, want)
	}
}

func TestHandlerSites_Empty(t *testing.T) {
	t.Parallel()

	h := sitesHandler(&mockBridge{sites: nil})
	res, err := h(context.Background(), readReq(uriSites))
	if err != nil {
		t.Fatalf("handler returned Go error: %v", err)
	}
	text := singleContent(t, res, uriSites)
	// Empty must serialize as an empty array [], never JSON null.
	if text != "[]" {
		t.Fatalf("empty sites payload = %q, want \"[]\"", text)
	}
}

func TestHandlerSites_BridgeError(t *testing.T) {
	t.Parallel()

	h := sitesHandler(&mockBridge{sitesErr: control.ErrDaemonNotRunning})
	res, err := h(context.Background(), readReq(uriSites))
	if err != nil {
		t.Fatalf("sites error must not be a Go error, got: %v", err)
	}
	text := singleContent(t, res, uriSites)
	var got struct {
		Error string `json:"error"`
	}
	if err := json.Unmarshal([]byte(text), &got); err != nil {
		t.Fatalf("payload not JSON: %v (%s)", err, text)
	}
	if got.Error != control.ErrDaemonNotRunning.Error() {
		t.Fatalf("error = %q, want %q", got.Error, control.ErrDaemonNotRunning.Error())
	}
}
