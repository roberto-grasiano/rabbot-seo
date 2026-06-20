package config

import (
	"fmt"
	"strings"
)

// Notifier type strings are a PUBLIC CONTRACT (decisions-2026-06-10.md #10),
// matching the `slack-webhook` service-transport convention. They are referenced
// by config files, docs, and supervisor.BuildAlertingStack's construction switch.
// Do not rename an existing value; add a new one for a new transport.
const (
	NotifierTypeSlack   = "slack-webhook"
	NotifierTypeEmail   = "email-smtp"
	NotifierTypeWebhook = "generic-webhook"
)

// validNotifierTypes is the ordered set of accepted type strings. It is the single
// source of truth for both the accept check and the "valid types" list in the
// rejection error, so the message can never drift from what is actually accepted.
var validNotifierTypes = []string{
	NotifierTypeSlack,
	NotifierTypeEmail,
	NotifierTypeWebhook,
}

// maxTCPPort is the inclusive upper bound for a TCP port number.
const maxTCPPort = 65535

// ValidateNotifiers checks every notifier's type and per-type required fields. It
// is the early, friendly gate used by Config.Validate (so `rabbot config validate`
// and `rabbot doctor` report a bad type or missing field); supervisor.BuildAlertingStack
// enforces the SAME contract a second time, hard, at daemon startup. Errors name the
// offending notifier and the missing field(s) but NEVER echo a secret value
// (password / URL / header values) — CLAUDE.md hard rule.
func ValidateNotifiers(notifiers []NotifierConfig) error {
	for _, n := range notifiers {
		if err := validateNotifier(n); err != nil {
			return err
		}
	}
	return nil
}

// validateNotifier validates a single notifier.
func validateNotifier(n NotifierConfig) error {
	switch n.Type {
	case NotifierTypeSlack:
		return requireFields(n, requiredField{"url", n.URL == ""})
	case NotifierTypeWebhook:
		return requireFields(n, requiredField{"url", n.URL == ""})
	case NotifierTypeEmail:
		if err := requireFields(n,
			requiredField{"smtp_host", n.SMTPHost == ""},
			requiredField{"from", n.From == ""},
			requiredField{"to", len(n.To) == 0},
		); err != nil {
			return err
		}
		// A real port is required to pick a TLS mode (465 ⇒ implicit TLS; else
		// STARTTLS). 0 is "unset" and is rejected like a missing field; an
		// out-of-range value is a clear typo.
		if n.SMTPPort <= 0 || n.SMTPPort > maxTCPPort {
			return fmt.Errorf("notifier %q (email-smtp): smtp_port must be 1..%d, got %d",
				n.Name, maxTCPPort, n.SMTPPort)
		}
		return nil
	default:
		return fmt.Errorf("notifier %q: unknown type %q; valid types: %s",
			n.Name, n.Type, strings.Join(validNotifierTypes, ", "))
	}
}

// requiredField names a field and whether it is currently missing.
type requiredField struct {
	name    string
	missing bool
}

// requireFields builds a single error listing every missing field for a notifier,
// or nil when all are present. The error carries only field NAMES and the notifier
// name — never the field values (which may be secrets).
func requireFields(n NotifierConfig, fields ...requiredField) error {
	var missing []string
	for _, f := range fields {
		if f.missing {
			missing = append(missing, f.name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	return fmt.Errorf("notifier %q (%s): incomplete config, missing %s",
		n.Name, n.Type, strings.Join(missing, ", "))
}
