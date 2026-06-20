package notify

import (
	"bufio"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"fmt"
	"math/big"
	"net"
	"net/textproto"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/roberto-grasiano/rabbot-seo/internal/model"
)

// fakeSMTP is a minimal in-process SMTP server built on stdlib net + textproto —
// no external dependency. It speaks just enough of RFC 5321 for net/smtp's
// client: greeting, EHLO (advertising the configured extensions), optional
// STARTTLS (real TLS upgrade with a self-signed cert), AUTH LOGIN/PLAIN, and the
// MAIL/RCPT/DATA transaction. It records what it received so tests can assert the
// envelope and message body.
type fakeSMTP struct {
	t *testing.T

	advertiseStartTLS bool
	advertiseAuth     bool
	tlsConfig         *tls.Config // server cert for the STARTTLS upgrade
	failAuth          bool        // reply 535 to AUTH

	mu       sync.Mutex
	mailFrom string
	rcptTo   []string
	data     string
	authSeen bool
}

// newFakeSMTP starts a fake SMTP server on a loopback port and returns it plus the
// host:port to dial. It self-registers cleanup with t.
func newFakeSMTP(t *testing.T, advertiseStartTLS, advertiseAuth bool) (*fakeSMTP, string) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeSMTP{t: t, advertiseStartTLS: advertiseStartTLS, advertiseAuth: advertiseAuth}
	if advertiseStartTLS {
		f.tlsConfig = &tls.Config{Certificates: []tls.Certificate{selfSignedCert(t)}} //nolint:gosec // test server
	}
	go f.serve(ln)
	t.Cleanup(func() { _ = ln.Close() })
	return f, ln.Addr().String()
}

func (f *fakeSMTP) serve(ln net.Listener) {
	for {
		conn, err := ln.Accept()
		if err != nil {
			return
		}
		go f.handle(conn)
	}
}

func (f *fakeSMTP) handle(conn net.Conn) {
	defer func() { _ = conn.Close() }()
	tp := textproto.NewConn(conn)
	_ = tp.PrintfLine("220 fake ESMTP")

	for {
		line, err := tp.ReadLine()
		if err != nil {
			return
		}
		cmd := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			var ext []string
			if f.advertiseStartTLS {
				ext = append(ext, "STARTTLS")
			}
			if f.advertiseAuth {
				ext = append(ext, "AUTH PLAIN LOGIN")
			}
			f.writeMultiline(tp, "250", append([]string{"fake greets you"}, ext...))
		case strings.HasPrefix(cmd, "STARTTLS"):
			if !f.advertiseStartTLS {
				_ = tp.PrintfLine("502 not supported")
				continue
			}
			_ = tp.PrintfLine("220 ready to start TLS")
			tlsConn := tls.Server(conn, f.tlsConfig)
			if err := tlsConn.Handshake(); err != nil {
				return
			}
			conn = tlsConn
			tp = textproto.NewConn(conn)
		case strings.HasPrefix(cmd, "AUTH"):
			if f.failAuth {
				_ = tp.PrintfLine("535 authentication failed")
				continue
			}
			f.mu.Lock()
			f.authSeen = true
			f.mu.Unlock()
			// Handle the AUTH LOGIN challenge-response handshake if used; PLAIN is
			// a single line. Accept either.
			if strings.Contains(cmd, "LOGIN") {
				_ = tp.PrintfLine("334 VXNlcm5hbWU6") // "Username:"
				_, _ = tp.ReadLine()
				_ = tp.PrintfLine("334 UGFzc3dvcmQ6") // "Password:"
				_, _ = tp.ReadLine()
			}
			_ = tp.PrintfLine("235 authentication succeeded")
		case strings.HasPrefix(cmd, "MAIL FROM"):
			f.mu.Lock()
			f.mailFrom = extractAddr(line)
			f.mu.Unlock()
			_ = tp.PrintfLine("250 ok")
		case strings.HasPrefix(cmd, "RCPT TO"):
			f.mu.Lock()
			f.rcptTo = append(f.rcptTo, extractAddr(line))
			f.mu.Unlock()
			_ = tp.PrintfLine("250 ok")
		case strings.HasPrefix(cmd, "DATA"):
			_ = tp.PrintfLine("354 end with .")
			body, _ := tp.ReadDotLines()
			f.mu.Lock()
			f.data = strings.Join(body, "\r\n")
			f.mu.Unlock()
			_ = tp.PrintfLine("250 ok queued")
		case strings.HasPrefix(cmd, "QUIT"):
			_ = tp.PrintfLine("221 bye")
			return
		default:
			_ = tp.PrintfLine("250 ok")
		}
	}
}

func (f *fakeSMTP) writeMultiline(tp *textproto.Conn, code string, lines []string) {
	w := bufio.NewWriter(tp.W)
	for i, l := range lines {
		sep := "-"
		if i == len(lines)-1 {
			sep = " "
		}
		_, _ = fmt.Fprintf(w, "%s%s%s\r\n", code, sep, l)
	}
	_ = w.Flush()
}

func (f *fakeSMTP) snapshot() (from string, rcpts []string, data string, authSeen bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	r := append([]string(nil), f.rcptTo...)
	return f.mailFrom, r, f.data, f.authSeen
}

// extractAddr pulls the bare address out of a "MAIL FROM:<a@b>" / "RCPT TO:<a@b>".
func extractAddr(line string) string {
	i := strings.Index(line, "<")
	j := strings.Index(line, ">")
	if i >= 0 && j > i {
		return line[i+1 : j]
	}
	return ""
}

// TestEmailNotifierSendsMessage pins acceptance #1: against the in-process fake
// SMTP server, MAIL FROM = from, one RCPT TO per `to` entry, a Subject containing
// severity + site + change_type, and a body containing Before/After.
func TestEmailNotifierSendsMessage(t *testing.T) {
	srv, addr := newFakeSMTP(t, true /*starttls*/, true /*auth*/)
	host, port := splitHostPort(t, addr)

	n, err := NewEmailNotifier(EmailConfig{
		Name:     "ops-mail",
		Host:     host,
		Port:     port,
		Username: "alerts@example.com",
		Password: "smtp-pass",
		From:     "rabbot@example.com",
		To:       []string{"seo-team@example.com", "lead@example.com"},
		// 587-style/explicit STARTTLS path is exercised via the advertised STARTTLS.
		TLSConfig: insecureClientTLS(), // trust the fake's self-signed cert in tests
	})
	if err != nil {
		t.Fatalf("NewEmailNotifier: %v", err)
	}

	if err := n.Notify(context.Background(), Alert{
		Site: "example.com", URL: "https://example.com/p", ChangeType: "title",
		Severity: model.SeverityCritical, Before: "Old Title", After: "New Title",
		DetectedAt: time.Date(2026, 6, 10, 12, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatalf("Notify: %v", err)
	}

	from, rcpts, data, authSeen := srv.snapshot()
	if from != "rabbot@example.com" {
		t.Errorf("MAIL FROM = %q, want rabbot@example.com", from)
	}
	if len(rcpts) != 2 || rcpts[0] != "seo-team@example.com" || rcpts[1] != "lead@example.com" {
		t.Errorf("RCPT TO = %v, want one per `to` entry", rcpts)
	}
	if !authSeen {
		t.Error("expected AUTH to be attempted when a username is configured")
	}
	// Subject must carry severity + site + change_type — and the assertion must be
	// load-bearing: extract the Subject HEADER LINE specifically (the body also
	// contains all three tokens, so a whole-message Contains would pass even with an
	// empty subject) and assert each token appears ON that line.
	subjectLine := headerLine(data, "Subject")
	if subjectLine == "" {
		t.Fatalf("message missing a Subject header:\n%s", data)
	}
	lowSubj := strings.ToLower(subjectLine)
	for _, want := range []string{"critical", "example.com", "title"} {
		if !strings.Contains(lowSubj, want) {
			t.Errorf("Subject line missing %q: %q", want, subjectLine)
		}
	}
	if !strings.Contains(data, "Old Title") || !strings.Contains(data, "New Title") {
		t.Errorf("body missing Before/After content:\n%s", data)
	}
	// CRLF framing: the header/body separator must be a blank CRLF line.
	if !strings.Contains(data, "\r\n\r\n") {
		t.Errorf("message lacks CRLF header/body separation:\n%q", data)
	}
}

// TestEmailDialModeSelection pins acceptance #2: the pure dial-mode function.
// 465 ⇒ implicit TLS; 587/25/other ⇒ STARTTLS; plaintext only when allowPlaintext.
func TestEmailDialModeSelection(t *testing.T) {
	cases := []struct {
		port           int
		allowPlaintext bool
		want           dialMode
	}{
		{465, false, dialImplicitTLS},
		{465, true, dialImplicitTLS}, // implicit TLS on 465 wins even if plaintext allowed
		{587, false, dialStartTLS},
		{25, false, dialStartTLS},
		{2525, false, dialStartTLS},
		{587, true, dialPlaintext}, // explicit opt-in downgrades to plaintext
		{25, true, dialPlaintext},
	}
	for _, tc := range cases {
		if got := selectDialMode(tc.port, tc.allowPlaintext); got != tc.want {
			t.Errorf("selectDialMode(%d, allowPlaintext=%v) = %v, want %v",
				tc.port, tc.allowPlaintext, got, tc.want)
		}
	}
}

// TestEmailNotifierRefusesPlaintextByDefault pins acceptance #3: a server that
// does NOT advertise STARTTLS must make Notify fail closed unless allow_plaintext
// is set.
func TestEmailNotifierRefusesPlaintextByDefault(t *testing.T) {
	srv, addr := newFakeSMTP(t, false /*no starttls*/, false /*no auth*/)
	_ = srv
	host, port := splitHostPort(t, addr) // a non-465 port ⇒ STARTTLS required

	n, err := NewEmailNotifier(EmailConfig{
		Name: "ops-mail", Host: host, Port: port,
		From: "rabbot@example.com", To: []string{"seo-team@example.com"},
	})
	if err != nil {
		t.Fatalf("NewEmailNotifier: %v", err)
	}
	err = n.Notify(context.Background(), Alert{
		Site: "example.com", ChangeType: "title", Severity: model.SeverityWarning,
		DetectedAt: time.Now(),
	})
	if err == nil {
		t.Fatal("expected Notify to fail closed when the server won't upgrade to STARTTLS")
	}

	// With allow_plaintext, the same no-STARTTLS server must succeed.
	n2, err := NewEmailNotifier(EmailConfig{
		Name: "ops-mail", Host: host, Port: port,
		From: "rabbot@example.com", To: []string{"seo-team@example.com"},
		AllowPlaintext: true,
	})
	if err != nil {
		t.Fatalf("NewEmailNotifier(allow_plaintext): %v", err)
	}
	if err := n2.Notify(context.Background(), Alert{
		Site: "example.com", ChangeType: "title", Severity: model.SeverityWarning,
		DetectedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Notify with allow_plaintext should succeed against a plaintext relay: %v", err)
	}
}

// TestEmailErrorNeverContainsPassword pins acceptance #4: a failed AUTH surfaces
// an error whose Error() does NOT contain the SMTP password.
func TestEmailErrorNeverContainsPassword(t *testing.T) {
	const password = "S3CR3T-SMTP-PASSWORD-XYZ"
	srv, addr := newFakeSMTP(t, true /*starttls*/, true /*auth*/)
	srv.failAuth = true
	host, port := splitHostPort(t, addr)

	n, err := NewEmailNotifier(EmailConfig{
		Name: "ops-mail", Host: host, Port: port,
		Username: "alerts@example.com", Password: password,
		From: "rabbot@example.com", To: []string{"seo-team@example.com"},
		TLSConfig: insecureClientTLS(),
	})
	if err != nil {
		t.Fatalf("NewEmailNotifier: %v", err)
	}
	err = n.Notify(context.Background(), Alert{
		Site: "example.com", ChangeType: "title", Severity: model.SeverityWarning,
		DetectedAt: time.Now(),
	})
	if err == nil {
		t.Fatal("expected an auth-failure error")
	}
	if strings.Contains(err.Error(), password) {
		t.Errorf("error leaked the SMTP password: %v", err)
	}

	// Load-bearing-scrub guard: the live 535 path above may not itself echo the
	// password, so prove the scrub actually removes it from an error that DOES
	// contain it (e.g. a future SMTP/TLS lib surfacing credentials in its message).
	en, ok := n.(*emailNotifier)
	if !ok {
		t.Fatalf("expected *emailNotifier, got %T", n)
	}
	leaky := fmt.Errorf("smtp auth failed for user with password %s", password)
	scrubbed := en.scrub(leaky)
	if strings.Contains(scrubbed.Error(), password) {
		t.Errorf("scrub() failed to redact the password: %v", scrubbed)
	}
	if !strings.Contains(scrubbed.Error(), "<redacted>") {
		t.Errorf("scrub() should mark the redaction; got: %v", scrubbed)
	}
}

// TestEmailNotifierImplicitTLS pins the 465 ⇒ implicit-TLS path end to end: the
// listener speaks TLS from the first byte (no STARTTLS), and the notifier must
// dial it over a TLS connection.
func TestEmailNotifierImplicitTLS(t *testing.T) {
	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{ //nolint:gosec // test server
		Certificates: []tls.Certificate{selfSignedCert(t)},
	})
	if err != nil {
		t.Fatalf("tls listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	srv := &fakeSMTP{t: t, advertiseAuth: false}
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		srv.handle(conn)
	}()

	n, err := NewEmailNotifier(EmailConfig{
		Name: "ops-mail", Host: "smtp.example.com", Port: 465, // 465 ⇒ implicit-TLS dial mode
		From: "rabbot@example.com", To: []string{"seo-team@example.com"},
		TLSConfig: insecureClientTLS(),
		dialAddr:  ln.Addr().String(), // test seam: dial the real ephemeral listener
	})
	if err != nil {
		t.Fatalf("NewEmailNotifier: %v", err)
	}
	if err := n.Notify(context.Background(), Alert{
		Site: "example.com", ChangeType: "title", Severity: model.SeverityInfo,
		Before: "x", After: "y", DetectedAt: time.Now(),
	}); err != nil {
		t.Fatalf("Notify over implicit TLS: %v", err)
	}
	_ = ln.Close()
	wg.Wait()

	from, rcpts, _, _ := srv.snapshot()
	if from != "rabbot@example.com" || len(rcpts) != 1 {
		t.Errorf("implicit-TLS envelope wrong: from=%q rcpts=%v", from, rcpts)
	}
}

// TestEmailNotifierHonorsCtxDeadlineDuringTransaction pins the post-dial deadline:
// a server that completes the TCP dial and sends a banner but then STALLS (never
// answering EHLO) must not pin the delivery goroutine. With a ctx deadline, Notify
// must return promptly (well before net/smtp's absent timeout would let the OS TCP
// timeout fire minutes later). This proves ctx is honored for the whole send, not
// just the dial. Uses a plaintext (allow_plaintext) relay so the stall happens on
// the first post-dial command (EHLO), isolating the in-transaction deadline.
func TestEmailNotifierHonorsCtxDeadlineDuringTransaction(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	t.Cleanup(func() { _ = ln.Close() })

	// A server that greets, then goes silent forever (accepts the conn, writes the
	// 220 banner, and never reads/answers another command).
	accepted := make(chan net.Conn, 1)
	go func() {
		conn, aerr := ln.Accept()
		if aerr != nil {
			return
		}
		accepted <- conn
		_, _ = conn.Write([]byte("220 stall ESMTP\r\n"))
		// Then stall: never read EHLO, never reply. Hold the conn open.
		select {}
	}()

	host, port := splitHostPort(t, ln.Addr().String())
	n, err := NewEmailNotifier(EmailConfig{
		Name: "ops-mail", Host: host, Port: port,
		From: "rabbot@example.com", To: []string{"seo-team@example.com"},
		AllowPlaintext: true, // skip STARTTLS so the stall lands on EHLO, not the upgrade
	})
	if err != nil {
		t.Fatalf("NewEmailNotifier: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	start := time.Now()
	err = n.Notify(ctx, Alert{
		Site: "example.com", ChangeType: "title", Severity: model.SeverityWarning,
		DetectedAt: time.Now(),
	})
	elapsed := time.Since(start)
	if err == nil {
		t.Fatal("expected Notify to fail when the SMTP server stalls mid-transaction")
	}
	// A wedged server must not pin the goroutine well past the ctx deadline. Allow a
	// generous margin over the 300ms deadline for scheduling under -race.
	if elapsed > 5*time.Second {
		t.Errorf("Notify did not honor the ctx deadline during the transaction: took %s", elapsed)
	}
	// The password must never leak even on a deadline error.
	select {
	case c := <-accepted:
		_ = c.Close()
	default:
	}
}

func TestEmailNotifierName(t *testing.T) {
	n, err := NewEmailNotifier(EmailConfig{
		Name: "ops-mail", Host: "smtp.example.com", Port: 465,
		From: "a@example.com", To: []string{"b@example.com"},
	})
	if err != nil {
		t.Fatalf("NewEmailNotifier: %v", err)
	}
	if n.Name() != "ops-mail" {
		t.Errorf("Name() = %q, want ops-mail", n.Name())
	}
}

// TestEmailNotifierRejectsIncompleteConfig asserts the constructor validates its
// required fields (host/from/to) up front so a misconfigured notifier fails at
// construction, never at first send — and the error names the notifier without
// echoing any secret.
func TestEmailNotifierRejectsIncompleteConfig(t *testing.T) {
	const password = "S3CR3T-PW"
	cases := []struct {
		name string
		cfg  EmailConfig
	}{
		{"missing host", EmailConfig{Name: "m", Port: 465, From: "a@x", To: []string{"b@x"}, Password: password}},
		{"missing from", EmailConfig{Name: "m", Host: "h", Port: 465, To: []string{"b@x"}, Password: password}},
		{"missing to", EmailConfig{Name: "m", Host: "h", Port: 465, From: "a@x", Password: password}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := NewEmailNotifier(tc.cfg)
			if err == nil {
				t.Fatalf("expected an error for %s", tc.name)
			}
			if !strings.Contains(err.Error(), "m") {
				t.Errorf("error should name the notifier %q: %v", "m", err)
			}
			if strings.Contains(err.Error(), password) {
				t.Errorf("construction error leaked the password: %v", err)
			}
		})
	}
}

// --- test helpers (cert + host/port + insecure client TLS) ---

// headerLine returns the first RFC 5322 header line (everything after "Name: ")
// for the given header name from a CRLF-framed message, or "" if absent. It scans
// only the header block (up to the blank CRLF separator) so a body line that
// happens to start with the name is never mistaken for the header.
func headerLine(msg, name string) string {
	headerBlock := msg
	if i := strings.Index(msg, "\r\n\r\n"); i >= 0 {
		headerBlock = msg[:i]
	}
	prefix := name + ": "
	for _, line := range strings.Split(headerBlock, "\r\n") {
		if strings.HasPrefix(line, prefix) {
			return strings.TrimPrefix(line, prefix)
		}
	}
	return ""
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("SplitHostPort(%q): %v", addr, err)
	}
	var port int
	_, _ = fmt.Sscanf(portStr, "%d", &port)
	return host, port
}

// insecureClientTLS builds a client tls.Config that trusts the fake server's
// self-signed cert. PRODUCTION code never sets InsecureSkipVerify; this is a
// test-only escape so net/smtp will complete the TLS handshake against the fake.
func insecureClientTLS() *tls.Config {
	return &tls.Config{InsecureSkipVerify: true} //nolint:gosec // test-only: fake server cert
}

// selfSignedCert mints a throwaway in-memory cert for the fake SMTP server's TLS.
func selfSignedCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatalf("rsa key: %v", err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatalf("create cert: %v", err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}
