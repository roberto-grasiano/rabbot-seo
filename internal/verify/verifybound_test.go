package verify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestVerifyBound_DerivesAndVerifies(t *testing.T) {
	key := make([]byte, instanceKeyBytes)
	key[0] = 7
	host := "example.com"
	token := DeriveToken(key, host) // what the owner would place

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(token))
	}))
	defer srv.Close()

	out, err := Verify(context.Background(),
		Request{Host: host, Method: MethodWellKnown},
		Options{Now: time.Unix(100, 0), AllowPrivate: true, BaseOverride: srv.URL, Key: key})
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Reason != ReasonVerified || out.Record.State != StateVerified {
		t.Fatalf("got reason=%q state=%q, want verified", out.Reason, out.Record.State)
	}
}

func TestVerifyBound_EmptyKeyFailsSafe(t *testing.T) {
	out, err := Verify(context.Background(),
		Request{Host: "example.com", Method: MethodWellKnown},
		Options{Now: time.Unix(100, 0)}) // no Key
	if err != nil {
		t.Fatalf("err: %v", err)
	}
	if out.Record.State == StateVerified {
		t.Fatal("empty key must never verify")
	}
	if out.Reason != ReasonUnreachable {
		t.Fatalf("reason = %q, want unreachable", out.Reason)
	}
}
