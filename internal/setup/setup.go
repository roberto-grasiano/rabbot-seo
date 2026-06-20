// Package setup builds and applies a first-run configuration plan. It is the
// single, UI-agnostic source of truth for onboarding: the TUI wizard and the
// headless flags path both construct a Plan and call Apply. Apply validates the
// inputs and writes them into config.yaml using the comment-preserving config
// writers; it performs NO network I/O (precheck and proof-of-control are layered
// on in later phases).
package setup

import (
	"errors"
	"fmt"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/config"
	"github.com/roberto-grasiano/rabbot-seo/internal/fetcher"
)

var (
	// ErrContactEmailRequired = the contact email is empty; ErrContactEmailInvalid =
	// present but not a valid email address. The contact is a published email (not a
	// URL) carried in the crawler User-Agent so a site owner can reach the operator.
	ErrContactEmailRequired = errors.New("setup: contact email is required")
	ErrContactEmailInvalid  = errors.New("setup: contact must be a valid email address")
	ErrNotAuthorized        = errors.New("setup: authorization attestation is required (confirm you are authorized to monitor the sites you add)")
	ErrNoSites              = errors.New("setup: at least one site is required")
	ErrIntervalInvalid      = errors.New("setup: interval must be a Go duration string (e.g. 10m, 24h)")
	ErrIntervalOrder        = errors.New("setup: max_interval must be greater than or equal to min_interval")
	ErrSpeedOutOfRange      = errors.New("setup: speed must be between 1 and 100")
)

// speedMin and speedMax bound a site's speed-scale percent override. The runtime
// treats Speed as a percent of the global speed_scale (default 100); a value
// outside 1..100 is almost certainly a typo, so setup rejects it rather than
// letting it silently distort the crawl rate.
const (
	speedMin = 1
	speedMax = 100
)

// Plan is the full set of first-run inputs. Later phases extend it (proof-of-
// control, alerts, MCP, run/service); Phase 1 covers identity + sites + the
// authorization attestation.
type Plan struct {
	ContactEmail string
	Authorized   bool
	Sites        []SiteInput
}

// SiteInput is one monitored site and its optional per-site overrides.
type SiteInput struct {
	URL         string
	Name        string
	MinInterval string
	MaxInterval string
	Speed       int
}

// Options carries the environment Apply needs.
type Options struct {
	ConfigPath string    // absolute path to config.yaml
	Version    string    // build version, for the User-Agent preview
	Now        time.Time // attestation timestamp (inject for deterministic tests)
}

// Result reports what Apply did.
type Result struct {
	ConfigPath   string
	UserAgent    string
	SitesAdded   []string
	SitesSkipped []string // already present in config; left untouched
}

// Validate checks the plan's inputs without touching disk or the network.
func (p Plan) Validate() error {
	if p.ContactEmail == "" {
		return ErrContactEmailRequired
	}
	if err := validateContactEmail(p.ContactEmail); err != nil {
		return err
	}
	if !p.Authorized {
		return ErrNotAuthorized
	}
	if len(p.Sites) == 0 {
		return ErrNoSites
	}
	for _, s := range p.Sites {
		if err := fetcher.ValidateSiteURL(s.URL, false); err != nil {
			return fmt.Errorf("setup: site %q: %w", s.URL, err)
		}
		if err := validateSiteOverrides(s); err != nil {
			return fmt.Errorf("setup: site %q: %w", s.URL, err)
		}
	}
	return nil
}

// validateSiteOverrides checks the optional per-site overrides so a typo (a
// unit-less interval like "15", or a nonsense speed) is caught at onboarding
// instead of being silently swallowed by the runtime accessors (which fall back
// to defaults on an unparseable value). The runtime accessors apply the actual
// fallbacks; this only rejects obviously-wrong input the operator likely meant
// to take effect.
func validateSiteOverrides(s SiteInput) error {
	var minD, maxD time.Duration
	if s.MinInterval != "" {
		d, err := time.ParseDuration(s.MinInterval)
		if err != nil {
			return fmt.Errorf("%w: min_interval %q: %w", ErrIntervalInvalid, s.MinInterval, err)
		}
		minD = d
	}
	if s.MaxInterval != "" {
		d, err := time.ParseDuration(s.MaxInterval)
		if err != nil {
			return fmt.Errorf("%w: max_interval %q: %w", ErrIntervalInvalid, s.MaxInterval, err)
		}
		maxD = d
	}
	if s.MinInterval != "" && s.MaxInterval != "" && maxD < minD {
		return fmt.Errorf("%w: min=%s max=%s", ErrIntervalOrder, s.MinInterval, s.MaxInterval)
	}
	if s.Speed != 0 && (s.Speed < speedMin || s.Speed > speedMax) {
		return fmt.Errorf("%w: got %d", ErrSpeedOutOfRange, s.Speed)
	}
	return nil
}

func validateContactEmail(raw string) error {
	// The contact is an email address (it is published in the crawler User-Agent
	// so a site owner can reach the operator). Defer to config.ValidateEmail so
	// the accepted form never drifts from what config.Validate enforces on load.
	if err := config.ValidateEmail(raw); err != nil {
		return fmt.Errorf("%w: %w", ErrContactEmailInvalid, err)
	}
	return nil
}

// Apply validates the plan and writes it into config.yaml at opts.ConfigPath,
// preserving any existing keys and comments. Sites already present are skipped
// (Apply is safe to re-run). It re-loads and validates the written file on
// success. A mid-sequence write failure can leave a partial but re-runnable
// config (contact_email written, sites or attestation not yet applied); a
// subsequent Apply completes it.
func (p Plan) Apply(opts Options) (Result, error) {
	if err := p.Validate(); err != nil {
		return Result{}, err
	}
	// Default a zero Now (a caller using Options{} without setting it) so the
	// attestation timestamp isn't written as the bogus "0001-01-01T00:00:00Z".
	if opts.Now.IsZero() {
		opts.Now = time.Now()
	}

	if err := config.SetKeyYAML(opts.ConfigPath, "crawler.contact_email", p.ContactEmail); err != nil {
		return Result{}, fmt.Errorf("setup: write contact_email: %w", err)
	}

	// Existing sites (for dedup). A missing file loads as defaults (no sites).
	existing, err := config.Load(opts.ConfigPath, nil)
	if err != nil {
		return Result{}, fmt.Errorf("setup: load config: %w", err)
	}
	have := make(map[string]bool, len(existing.Sites))
	for _, s := range existing.Sites {
		have[s.URL] = true
	}

	res := Result{ConfigPath: opts.ConfigPath}
	for _, s := range p.Sites {
		if have[s.URL] {
			res.SitesSkipped = append(res.SitesSkipped, s.URL)
			continue
		}
		if err := config.AddSiteYAML(opts.ConfigPath, config.SiteConfig{
			URL:         s.URL,
			Name:        s.Name,
			MinInterval: s.MinInterval,
			MaxInterval: s.MaxInterval,
			Speed:       s.Speed,
		}); err != nil {
			return Result{}, fmt.Errorf("setup: add site %q: %w", s.URL, err)
		}
		have[s.URL] = true
		res.SitesAdded = append(res.SitesAdded, s.URL)
	}

	if err := config.SetKeyYAML(opts.ConfigPath, "setup.attested_at", opts.Now.UTC().Format(time.RFC3339)); err != nil {
		return Result{}, fmt.Errorf("setup: write attestation: %w", err)
	}

	final, err := config.Load(opts.ConfigPath, nil)
	if err != nil {
		return Result{}, fmt.Errorf("setup: reload config: %w", err)
	}
	if err := final.Validate(); err != nil {
		return Result{}, fmt.Errorf("setup: written config invalid: %w", err)
	}
	res.UserAgent = final.ResolvedUserAgent(opts.Version)
	return res, nil
}
