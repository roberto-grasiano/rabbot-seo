package verify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestVerifyWellKnown_Reasons(t *testing.T) {
	const token = "rab_ABC"
	tests := []struct {
		name string
		body string
		code int
		want Reason
	}{
		{"match", token, 200, ReasonVerified},
		{"mismatch", "rab_OTHER", 200, ReasonMismatch},
		{"empty body", "", 200, ReasonNotFound},
		{"missing", "", 404, ReasonNotFound},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.WriteHeader(tc.code)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			got, err := VerifyWellKnown(context.Background(), "example.com", token,
				Options{AllowPrivate: true, BaseOverride: srv.URL})
			if err != nil {
				t.Fatalf("err: %v", err)
			}
			if got != tc.want {
				t.Fatalf("reason = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestVerifyWellKnown_Redirected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Redirect(w, &http.Request{}, "https://evil.example/", http.StatusFound)
	}))
	defer srv.Close()
	got, err := VerifyWellKnown(context.Background(), "example.com", "rab_ABC",
		Options{AllowPrivate: true, BaseOverride: srv.URL})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if got != ReasonRedirected {
		t.Fatalf("reason = %q, want %q (a redirect must never verify)", got, ReasonRedirected)
	}
}
