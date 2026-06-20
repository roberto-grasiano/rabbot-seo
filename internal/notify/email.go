package notify

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"strings"
	"time"
)

// implicitTLSPort is the well-known SMTPS port: a connection is TLS from the
// first byte (no STARTTLS handshake). Every other port uses STARTTLS (RFC 3207)
// unless plaintext is explicitly opted in.
const implicitTLSPort = 465

// emailDialTimeout bounds the TCP+TLS dial so a black-holed SMTP host cannot pin
// a delivery goroutine indefinitely (net/smtp itself has no dial timeout).
const emailDialTimeout = 30 * time.Second

// emailTxnTimeout bounds the whole post-dial SMTP transaction when the caller's
// ctx carries no deadline (e.g. the daemon's long-lived ctx). The frozen
// net/smtp.Client is context-unaware, so without an absolute conn deadline a
// server that dials successfully then stalls mid-transaction (slowloris) would
// pin the delivery goroutine until the OS TCP timeout (minutes). Set generously —
// it is a backstop, not the common path; a caller-supplied deadline takes
// precedence.
const emailTxnTimeout = 60 * time.Second

// dialMode is how the SMTP connection is secured.
type dialMode int

const (
	// dialImplicitTLS dials TLS directly (port 465 / SMTPS).
	dialImplicitTLS dialMode = iota
	// dialStartTLS dials plaintext then REQUIRES a STARTTLS upgrade (fail closed
	// if the server does not advertise it).
	dialStartTLS
	// dialPlaintext dials plaintext with no encryption — only when the operator
	// explicitly opts in (localhost relays / Mailpit).
	dialPlaintext
)

func (m dialMode) String() string {
	switch m {
	case dialImplicitTLS:
		return "implicit-tls"
	case dialStartTLS:
		return "starttls"
	case dialPlaintext:
		return "plaintext"
	default:
		return "unknown"
	}
}

// selectDialMode is the pure dial-mode decision (table-tested without a socket):
// port 465 ⇒ implicit TLS (always, even if plaintext is allowed); any other port
// ⇒ STARTTLS, downgraded to plaintext only when allowPlaintext is set.
func selectDialMode(port int, allowPlaintext bool) dialMode {
	if port == implicitTLSPort {
		return dialImplicitTLS
	}
	if allowPlaintext {
		return dialPlaintext
	}
	return dialStartTLS
}

// EmailConfig configures an email-smtp notifier. It is constructed from the
// per-type fields on config.NotifierConfig by the supervisor wiring; the
// notify package does not import config.
type EmailConfig struct {
	Name           string
	Host           string
	Port           int
	Username       string // when set, smtp.PlainAuth is attempted
	Password       string // secret: never logged or echoed into errors
	From           string
	To             []string
	AllowPlaintext bool // opt out of STARTTLS for localhost-style relays only

	// TLSConfig overrides the TLS settings for the implicit-TLS dial and the
	// STARTTLS upgrade. Production leaves it nil (ServerName defaults to Host,
	// full verification). Tests inject a config that trusts a self-signed cert.
	TLSConfig *tls.Config

	// dialAddr is a test-only seam: when set, the connection is dialed to this
	// address instead of Host:Port, while the dial MODE is still derived from
	// Port. It lets a test exercise the 465 implicit-TLS path against an
	// ephemeral listener. Unexported ⇒ not reachable from config.
	dialAddr string
}

// emailNotifier delivers one plain-text RFC 5322 message per alert over SMTP.
type emailNotifier struct {
	cfg  EmailConfig
	mode dialMode
}

// NewEmailNotifier validates the config and builds an email-smtp notifier. A
// misconfigured notifier (missing host/from/to) fails HERE, at construction —
// never at first send — and the error names the notifier without echoing the
// password.
func NewEmailNotifier(cfg EmailConfig) (Notifier, error) {
	var missing []string
	if cfg.Host == "" {
		missing = append(missing, "smtp_host")
	}
	if cfg.From == "" {
		missing = append(missing, "from")
	}
	if len(cfg.To) == 0 {
		missing = append(missing, "to")
	}
	if len(missing) > 0 {
		return nil, fmt.Errorf("email-smtp %q: incomplete config, missing %s",
			cfg.Name, strings.Join(missing, ", "))
	}
	return &emailNotifier{cfg: cfg, mode: selectDialMode(cfg.Port, cfg.AllowPlaintext)}, nil
}

func (e *emailNotifier) Name() string { return e.cfg.Name }

// Notify renders the alert to a plain-text message and delivers it in one SMTP
// transaction (one message, all recipients). On a non-465 port it REQUIRES
// STARTTLS and fails closed if the server will not upgrade. Any error is scrubbed
// of the password before it leaves the package.
func (e *emailNotifier) Notify(ctx context.Context, a Alert) error {
	if err := e.deliver(ctx, a); err != nil {
		return e.scrub(err)
	}
	return nil
}

// deliver runs the full SMTP transaction. It honors ctx at the dial step AND for
// the whole in-flight transaction: because the frozen net/smtp Client has no
// context support, we set an absolute deadline derived from ctx on the underlying
// net.Conn before any command runs, so a server that completes the dial then
// stalls mid-transaction (slowloris: banner, then silence) cannot pin the delivery
// goroutine until the OS TCP timeout. It closes the client on exit.
func (e *emailNotifier) deliver(ctx context.Context, a Alert) error {
	addr := e.cfg.dialAddr
	if addr == "" {
		addr = net.JoinHostPort(e.cfg.Host, fmt.Sprintf("%d", e.cfg.Port))
	}

	client, conn, err := e.dial(ctx, addr)
	if err != nil {
		return err
	}
	defer func() { _ = client.Close() }()

	// Bound the whole post-dial transaction. Prefer the caller's ctx deadline; if
	// the caller passed none (e.g. the daemon's long-lived ctx), fall back to a
	// wall-clock cap so a wedged SMTP server can never hold the goroutine forever.
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(emailTxnTimeout)
	}
	_ = conn.SetDeadline(deadline)
	// Cancel the in-flight transaction promptly if ctx is cancelled before the
	// deadline (daemon shutdown / onboarding abort): tripping the conn deadline
	// unblocks any pending read/write in net/smtp. The goroutine exits when deliver
	// returns and closes done.
	done := make(chan struct{})
	defer close(done)
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetDeadline(time.Now())
		case <-done:
		}
	}()

	if err := client.Hello(heloName(e.cfg.From)); err != nil {
		return fmt.Errorf("smtp hello: %w", err)
	}

	switch e.mode {
	case dialStartTLS:
		// STARTTLS is REQUIRED on non-465 ports: fail closed if the server does
		// not advertise it rather than silently sending credentials in clear.
		ok, _ := client.Extension("STARTTLS")
		if !ok {
			return fmt.Errorf("smtp server does not support STARTTLS (set allow_plaintext for localhost relays only)")
		}
		if err := client.StartTLS(e.tlsConfig()); err != nil {
			return fmt.Errorf("smtp starttls: %w", err)
		}
	case dialImplicitTLS, dialPlaintext:
		// implicit TLS already wrapped the conn at dial; plaintext stays clear.
	}

	if e.cfg.Username != "" {
		// net/smtp.PlainAuth refuses PLAIN over an unencrypted, non-localhost
		// connection — we rely on that posture rather than re-implementing it.
		auth := smtp.PlainAuth("", e.cfg.Username, e.cfg.Password, e.cfg.Host)
		if err := client.Auth(auth); err != nil {
			return fmt.Errorf("smtp auth: %w", err)
		}
	}

	if err := client.Mail(e.cfg.From); err != nil {
		return fmt.Errorf("smtp mail from: %w", err)
	}
	for _, rcpt := range e.cfg.To {
		if err := client.Rcpt(rcpt); err != nil {
			return fmt.Errorf("smtp rcpt: %w", err)
		}
	}
	wc, err := client.Data()
	if err != nil {
		return fmt.Errorf("smtp data: %w", err)
	}
	if _, err := wc.Write(buildEmailMessage(e.cfg.From, e.cfg.To, a)); err != nil {
		_ = wc.Close()
		return fmt.Errorf("smtp write: %w", err)
	}
	if err := wc.Close(); err != nil {
		return fmt.Errorf("smtp data close: %w", err)
	}
	return client.Quit()
}

// dial opens the SMTP client connection per the selected mode, honoring ctx at
// the TCP/TLS dial. host (for TLS ServerName / smtp.NewClient) is the configured
// Host even when a test dialAddr overrides the dialed address. It returns the
// underlying net.Conn alongside the client so deliver can set a transaction-wide
// deadline on it (the conn is the TLS conn under implicit-TLS / STARTTLS, so the
// deadline still bounds reads/writes after the upgrade).
func (e *emailNotifier) dial(ctx context.Context, addr string) (*smtp.Client, net.Conn, error) {
	d := &net.Dialer{Timeout: emailDialTimeout}
	switch e.mode {
	case dialImplicitTLS:
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, nil, fmt.Errorf("smtp dial: %w", err)
		}
		tlsConn := tls.Client(conn, e.tlsConfig())
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, nil, fmt.Errorf("smtp tls handshake: %w", err)
		}
		client, err := smtp.NewClient(tlsConn, e.cfg.Host)
		if err != nil {
			_ = tlsConn.Close()
			return nil, nil, fmt.Errorf("smtp client: %w", err)
		}
		return client, tlsConn, nil
	default: // dialStartTLS, dialPlaintext
		conn, err := d.DialContext(ctx, "tcp", addr)
		if err != nil {
			return nil, nil, fmt.Errorf("smtp dial: %w", err)
		}
		client, err := smtp.NewClient(conn, e.cfg.Host)
		if err != nil {
			_ = conn.Close()
			return nil, nil, fmt.Errorf("smtp client: %w", err)
		}
		return client, conn, nil
	}
}

// tlsConfig returns the TLS settings for the implicit-TLS dial and STARTTLS
// upgrade: the operator-supplied config when present, else a default that
// verifies against the configured Host (ServerName). PRODUCTION never sets
// InsecureSkipVerify — gosec enforces this.
func (e *emailNotifier) tlsConfig() *tls.Config {
	if e.cfg.TLSConfig != nil {
		return e.cfg.TLSConfig
	}
	return &tls.Config{ServerName: e.cfg.Host, MinVersion: tls.VersionTLS12}
}

// scrub strips the SMTP password from any error before it leaves the package
// (CLAUDE.md hard rule: passwords never appear in errors/logs). Returning %s (not
// %w) also severs the error chain so no downstream Unwrap can recover it.
func (e *emailNotifier) scrub(err error) error {
	if err == nil {
		return nil
	}
	msg := replaceAllNonEmpty(err.Error(), e.cfg.Password, "<redacted>")
	return fmt.Errorf("email-smtp %q: %s", e.cfg.Name, msg)
}

// heloName derives a HELO/EHLO domain from the From address, falling back to
// "localhost". The remote server only uses it for its Received header.
func heloName(from string) string {
	if i := strings.LastIndex(from, "@"); i >= 0 && i+1 < len(from) {
		return from[i+1:]
	}
	return "localhost"
}
