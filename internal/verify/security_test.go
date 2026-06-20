package verify

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

// A token placed by instance A must NOT verify the same host in instance B:
// B derives a DIFFERENT expected token, so the replayed surface is not_found.
func TestReplayAttackDefeated(t *testing.T) {
	keyA := make([]byte, instanceKeyBytes)
	keyA[0] = 0xA
	keyB := make([]byte, instanceKeyBytes)
	keyB[0] = 0xB

	host := "victim.example"
	placed := DeriveToken(keyA, host) // what victim (instance A) placed

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(placed)) // attacker can read & serve A's token
	}))
	defer srv.Close()

	out, err := Verify(context.Background(),
		Request{Host: host, Method: MethodWellKnown},
		Options{Now: time.Unix(1, 0), AllowPrivate: true, BaseOverride: srv.URL, Key: keyB})
	if err != nil {
		t.Fatal(err)
	}
	if out.Record.State == StateVerified {
		t.Fatal("REPLAY BYPASS: instance B verified a token only instance A could have placed")
	}
	if out.Reason != ReasonMismatch && out.Reason != ReasonNotFound {
		t.Fatalf("reason = %q, want mismatch/not_found", out.Reason)
	}
}

// Losing/changing the key reverts a previously-placed token to throttled.
func TestKeyLossRevertsToThrottled(t *testing.T) {
	oldKey := make([]byte, instanceKeyBytes)
	oldKey[0] = 1
	newKey := make([]byte, instanceKeyBytes)
	newKey[0] = 2

	host := "example.com"
	placed := DeriveToken(oldKey, host)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(placed))
	}))
	defer srv.Close()

	out, _ := Verify(context.Background(),
		Request{Host: host, Method: MethodWellKnown},
		Options{Now: time.Unix(1, 0), AllowPrivate: true, BaseOverride: srv.URL, Key: newKey})
	if out.Record.State == StateVerified {
		t.Fatal("a token placed under the old key must not verify under a new key")
	}
}
