package cli

import (
	"bytes"
	"context"
	"strings"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/verify"
)

// TestRunVerifyConfigEditCannotFakeVerified is a load-bearing guard for the
// instance-bound design: a hand-edited config.yaml must NOT be able to mint a
// verified state. We seed the site's config verification block with a BOGUS
// token and a FUTURE verified_at (exactly what an attacker editing the file
// would write), point the verifier at a server that does NOT serve the derived
// token, and assert runVerify records throttled — the engine derives the
// expected token from the instance key and checks reality, never the config.
func TestRunVerifyConfigEditCannotFakeVerified(t *testing.T) {
	key := testInstanceKey()
	// The server serves a token that does NOT equal DeriveToken(key, host), so the
	// fetch succeeds (verr==nil) but the proof is a clean miss.
	srv := wellKnownTokenServer(t, "rab_BOGUSBOGUSBOGUSBOGUSBOGUSBOGUSBO")
	h := newVerifyHarness(t, srv, false)

	// Hand-write a forged verification block into config.yaml: a bogus token and a
	// far-future verified_at, as if an operator edited the file to fake control.
	if found, err := config.SetSiteVerificationYAML(h.configPath, h.siteURL, config.VerificationConfig{
		Method:     string(verify.MethodWellKnown),
		Token:      "rab_FORGEDFORGEDFORGEDFORGEDFORGEDFOR",
		VerifiedAt: "2099-01-01T00:00:00Z",
	}); err != nil || !found {
		t.Fatalf("seed forged config found=%v err=%v", found, err)
	}

	var buf bytes.Buffer
	deps := verifyDeps{
		db:           h.db,
		configPath:   h.configPath,
		cfg:          h.cfg,
		target:       h.siteURL,
		method:       verify.MethodWellKnown,
		key:          key,
		allowPrivate: true,
		baseOverride: srv.URL,
		now:          time.Date(2026, 6, 5, 12, 0, 0, 0, time.UTC),
	}
	if err := runVerify(context.Background(), &buf, deps); err != nil {
		t.Fatalf("runVerify: %v", err)
	}

	// The authoritative DB proof record must NOT be verified — the forged config
	// token is ignored; the engine derives and checks the real surface.
	site, err := h.db.GetSiteByBaseURL(context.Background(), h.siteURL)
	if err != nil {
		t.Fatal(err)
	}
	rec, err := h.db.GetVerification(context.Background(), site.ID)
	if err != nil {
		t.Fatal(err)
	}
	if rec.State == verify.StateVerified {
		t.Fatal("config-edit BYPASS: a hand-edited config token produced a verified state")
	}
	if rec.State != verify.StateThrottled {
		t.Fatalf("DB state = %q, want throttled (forged config must not verify)", rec.State)
	}
	// The DB record carries the DERIVED token, not the forged config one.
	if want := verify.DeriveToken(key, hostFromURL(h.siteURL)); rec.Token != want {
		t.Fatalf("DB record token = %q, want derived %q (not the forged config value)", rec.Token, want)
	}

	// The forged config token does not survive a verify run: the intent block is
	// rewritten with the DERIVED token, so a later read shows the real token (and a
	// hand-edited token never reaches the verifier — the engine derives its own).
	reloaded, err := config.Load(h.configPath, nil)
	if err != nil {
		t.Fatalf("config.Load: %v", err)
	}
	if len(reloaded.Sites) != 1 {
		t.Fatalf("want 1 site, got %d", len(reloaded.Sites))
	}
	v := reloaded.Sites[0].Verification
	if v.Token == "rab_FORGEDFORGEDFORGEDFORGEDFORGEDFOR" {
		t.Fatal("forged config token survived a verify run (it must be replaced by the derived token)")
	}

	// The output must not falsely claim a verified / full-speed state.
	out := strings.ToLower(buf.String())
	if strings.Contains(out, "full speed") {
		t.Fatalf("output falsely claims full speed for a forged config:\n%s", buf.String())
	}
}
