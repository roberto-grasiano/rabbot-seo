package cli

import (
	"errors"
	"fmt"
	"strings"
	"testing"

	"github.com/roberto-grasiano/rabbot-seo/internal/control"
	"github.com/roberto-grasiano/rabbot-seo/internal/model"
	"github.com/roberto-grasiano/rabbot-seo/internal/store"
)

// Finding #20.3 (cli half): the production AddSite hook must complete the error
// contract that internal/control.handleAddSite consumes — caller-fault
// rejections wrap control.ErrBadRequest (-> HTTP 400), while genuine internal
// faults pass through unwrapped (-> HTTP 500). The translation lives in small
// pure helpers in run.go so it can be unit-tested without a live daemon.

// TestAddSiteURLError_BadURLIsBadRequest covers leg (a): URL validation
// failures (empty/blank, unparseable, missing scheme/host, disallowed literal)
// are caller faults and must satisfy errors.Is(err, control.ErrBadRequest).
func TestAddSiteURLError_BadURLIsBadRequest(t *testing.T) {
	cases := []struct {
		name string
		url  string
	}{
		{"empty", ""},
		{"blank", "   "},
		{"no scheme", "example.com"},
		{"ftp scheme", "ftp://example.com"},
		{"no host", "https://"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// allowPrivate=false mirrors production (reject internal ranges); these
			// cases fail the scheme/host checks regardless of allowPrivate.
			err := addSiteURLError(tc.url, false)
			if err == nil {
				t.Fatalf("addSiteURLError(%q) = nil, want a bad-request error", tc.url)
			}
			if !errors.Is(err, control.ErrBadRequest) {
				t.Errorf("addSiteURLError(%q) = %v, want errors.Is(err, control.ErrBadRequest)", tc.url, err)
			}
		})
	}
}

// TestAddSiteURLError_ValidURLPasses confirms a well-formed public URL is not
// rejected (no false positive that would wrongly 400 a legitimate add).
func TestAddSiteURLError_ValidURLPasses(t *testing.T) {
	if err := addSiteURLError("https://example.com/", false); err != nil {
		t.Errorf("addSiteURLError(valid) = %v, want nil", err)
	}
}

// TestClassifyAddSiteStoreErr_DuplicateIsBadRequest covers leg (b): a duplicate
// site (store.ErrSiteExists, possibly wrapped) is a client error -> 400.
func TestClassifyAddSiteStoreErr_DuplicateIsBadRequest(t *testing.T) {
	// Directly wrapped and double-wrapped, to prove the classifier uses
	// errors.Is (not ==) so any wrap depth still maps to a bad request.
	for _, in := range []error{
		store.ErrSiteExists,
		fmt.Errorf("add site %q: %w", "https://dup.example", store.ErrSiteExists),
	} {
		out := classifyAddSiteStoreErr(in)
		if !errors.Is(out, control.ErrBadRequest) {
			t.Errorf("classifyAddSiteStoreErr(%v) = %v, want errors.Is(err, control.ErrBadRequest)", in, out)
		}
		// The duplicate sentinel should remain recoverable for any caller that
		// wants the specific cause; wrapping must add ErrBadRequest, not erase it.
		if !errors.Is(out, store.ErrSiteExists) {
			t.Errorf("classifyAddSiteStoreErr(%v) = %v, lost store.ErrSiteExists", in, out)
		}
	}
}

// TestClassifyAddSiteStoreErr_InternalPassesThrough asserts an internal/IO fault
// is NOT reclassified as a bad request, so handleAddSite maps it to 500.
func TestClassifyAddSiteStoreErr_InternalPassesThrough(t *testing.T) {
	internal := errors.New("database is locked")
	out := classifyAddSiteStoreErr(internal)
	if errors.Is(out, control.ErrBadRequest) {
		t.Fatalf("internal error %v was reclassified as control.ErrBadRequest", out)
	}
	if !errors.Is(out, internal) {
		t.Errorf("classifyAddSiteStoreErr(internal) = %v, want it to wrap/return the original", out)
	}
}

// TestClassifyAddSiteStoreErr_NilStaysNil guards the happy path.
func TestClassifyAddSiteStoreErr_NilStaysNil(t *testing.T) {
	if out := classifyAddSiteStoreErr(nil); out != nil {
		t.Errorf("classifyAddSiteStoreErr(nil) = %v, want nil", out)
	}
}

// TestAddSiteDuplicateErr drives the duplicate-detection decision that the
// closure makes from a store lookup, without a real DB:
//   - an existing ENABLED row is a duplicate -> store.ErrSiteExists
//   - a not-found row is a legitimate (re)add -> nil
//   - an existing DISABLED row is a legitimate re-add (reconcile re-enables it) -> nil
//   - any other lookup error is an internal fault -> returned unwrapped
func TestAddSiteDuplicateErr(t *testing.T) {
	ioErr := errors.New("disk failure")
	cases := []struct {
		name        string
		existing    model.Site
		lookupErr   error
		wantDup     bool // expect store.ErrSiteExists
		wantNil     bool
		wantPassErr error // expect this exact error to pass through
	}{
		{name: "enabled duplicate", existing: model.Site{ID: 1, Enabled: true}, lookupErr: nil, wantDup: true},
		{name: "not found", existing: model.Site{}, lookupErr: store.ErrNotFound, wantNil: true},
		{name: "disabled re-add", existing: model.Site{ID: 2, Enabled: false}, lookupErr: nil, wantNil: true},
		{name: "internal lookup error", existing: model.Site{}, lookupErr: ioErr, wantPassErr: ioErr},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := addSiteDuplicateErr(tc.existing, tc.lookupErr)
			switch {
			case tc.wantDup:
				if !errors.Is(err, store.ErrSiteExists) {
					t.Errorf("addSiteDuplicateErr = %v, want store.ErrSiteExists", err)
				}
			case tc.wantNil:
				if err != nil {
					t.Errorf("addSiteDuplicateErr = %v, want nil", err)
				}
			case tc.wantPassErr != nil:
				if !errors.Is(err, tc.wantPassErr) {
					t.Errorf("addSiteDuplicateErr = %v, want it to pass through %v", err, tc.wantPassErr)
				}
				if errors.Is(err, store.ErrSiteExists) {
					t.Errorf("internal lookup error %v wrongly mapped to store.ErrSiteExists", err)
				}
			}
		})
	}
}

// TestAddSiteErrorContract_EndToEnd ties the two legs together the way the
// closure composes them: a bad URL and a duplicate both surface as
// control.ErrBadRequest, while an internal fault does not. This is the
// regression assertion the finding calls for.
func TestAddSiteErrorContract_EndToEnd(t *testing.T) {
	// Bad URL leg.
	if err := addSiteURLError("not-a-url", false); !errors.Is(err, control.ErrBadRequest) {
		t.Errorf("bad URL: got %v, want control.ErrBadRequest", err)
	}
	// Duplicate leg (enabled existing row -> ErrSiteExists -> classified).
	dup := classifyAddSiteStoreErr(addSiteDuplicateErr(model.Site{ID: 9, Enabled: true}, nil))
	if !errors.Is(dup, control.ErrBadRequest) {
		t.Errorf("duplicate: got %v, want control.ErrBadRequest", dup)
	}
	// Internal leg (DB error from the lookup) must remain a 500.
	internal := classifyAddSiteStoreErr(addSiteDuplicateErr(model.Site{}, errors.New("io")))
	if errors.Is(internal, control.ErrBadRequest) {
		t.Errorf("internal: got %v, must NOT be control.ErrBadRequest", internal)
	}
	// Sanity: the bad-request message still carries a human cause for the operator.
	if err := addSiteURLError("ftp://x", false); err != nil && !strings.Contains(err.Error(), "ftp://x") {
		t.Errorf("bad-request error %q should include the offending URL", err.Error())
	}
}
