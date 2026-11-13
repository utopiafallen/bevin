// Package email is the server-managed SMTP notifier. It is internal to the
// harness (not a tool yet — a per-user email tool is a later revision) and is
// used by the server to send email notifications to users + admin. When the
// Email config is not configured the Sender is nil and Send is a no-op.
package email

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"net"
	"net/smtp"
	"strconv"
	"strings"
	"time"

	"github.com/utopiafallen/bevin/internal/config"
)

// Sender sends emails via SMTP. A nil Sender (from New when email is not
// configured) is a no-op: Send returns nil without doing anything, so call
// sites can hold a nil Sender and call Send unconditionally.
type Sender struct {
	cfg config.Email
	// tlsOverride carries test-only TLS settings for the STARTTLS upgrade
	// (a root pool trusting the test server's self-signed certificate);
	// production Send leaves it nil.
	tlsOverride *tls.Config
}

// New builds a Sender from the email config, or nil when email is not
// configured (disabled or no host).
func New(cfg config.Email) *Sender {
	if !cfg.Configured() {
		return nil
	}
	return &Sender{cfg: cfg}
}

// Configured reports whether the Sender will actually send (it is non-nil).
func (s *Sender) Configured() bool { return s != nil }

// Send sends a plain-text email to a single recipient. It is a no-op (nil)
// when the Sender is nil. The context bounds the dial; the rest of the
// session runs to completion (an SMTP server that stops answering mid-session
// is not recoverable anyway).
//
// The session is driven manually (not net/smtp's SendMail, which has two
// shortcomings: it never upgrades to STARTTLS and its dial ignores the
// context). STARTTLS: bevin always upgrades when the server offers it (so
// even a plain relay's traffic is encrypted in transit, matching the wizard
// help text), and it is an ERROR when the config requires it and the server
// does not offer it.
func (s *Sender) Send(ctx context.Context, to, subject, body string) error {
	if s == nil {
		return nil
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	to = strings.TrimSpace(to)
	if to == "" {
		return fmt.Errorf("email: no recipient")
	}
	host := s.cfg.SMTPHost
	addr := net.JoinHostPort(host, strconv.Itoa(s.cfg.SMTPPort))
	d := net.Dialer{Timeout: 15 * time.Second}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return fmt.Errorf("email: dial %s: %w", addr, err)
	}
	client, err := smtp.NewClient(conn, host)
	if err != nil {
		_ = conn.Close()
		return fmt.Errorf("email: smtp handshake with %s: %w", addr, err)
	}
	defer client.Close()
	if ok, _ := client.Extension("STARTTLS"); ok {
		tlsCfg := &tls.Config{ServerName: host}
		if s.tlsOverride != nil { // test-only: trust the fake server's self-signed cert
			tlsCfg.RootCAs = s.tlsOverride.RootCAs
			tlsCfg.InsecureSkipVerify = s.tlsOverride.InsecureSkipVerify
		}
		if err := client.StartTLS(tlsCfg); err != nil {
			return fmt.Errorf("email: STARTTLS with %s: %w", host, err)
		}
	} else if s.cfg.UseStartTLS {
		return fmt.Errorf("email: STARTTLS is enabled but the server %s does not offer it", host)
	}
	if s.cfg.Username != "" {
		if ok, _ := client.Extension("AUTH"); !ok {
			return fmt.Errorf("email: a username is set but the server %s does not offer AUTH", host)
		}
		// The server name for PlainAuth is the host we're connecting to (it
		// is not used for initial-responders unless the server requires it).
		if err := client.Auth(smtp.PlainAuth("", s.cfg.Username, s.cfg.Password, host)); err != nil {
			return fmt.Errorf("email: auth with %s: %w", host, err)
		}
	}
	if err := client.Mail(s.cfg.FromAddress); err != nil {
		return fmt.Errorf("email: MAIL FROM: %w", err)
	}
	if err := client.Rcpt(to); err != nil {
		return fmt.Errorf("email: RCPT TO: %w", err)
	}
	w, err := client.Data()
	if err != nil {
		return fmt.Errorf("email: DATA: %w", err)
	}
	if _, err := w.Write(buildMessage(s.cfg, to, subject, body)); err != nil {
		return fmt.Errorf("email: writing message: %w", err)
	}
	if err := w.Close(); err != nil {
		return fmt.Errorf("email: end of DATA: %w", err)
	}
	if err := client.Quit(); err != nil {
		return fmt.Errorf("email: QUIT: %w", err)
	}
	return nil
}

// buildMessage assembles a minimal RFC 5322 plain-text message (UTF-8). The
// From header carries an optional display name; the Subject is RFC 2047
// encoded when it is not plain ASCII.
func buildMessage(cfg config.Email, to, subject, body string) []byte {
	var b strings.Builder
	from := cfg.FromAddress
	if cfg.FromName != "" {
		from = fmt.Sprintf("%s <%s>", cfg.FromName, cfg.FromAddress)
	}
	b.WriteString("From: " + from + "\r\n")
	b.WriteString("To: " + to + "\r\n")
	b.WriteString("Subject: " + encodeHeader(subject) + "\r\n")
	b.WriteString("Date: " + time.Now().UTC().Format(time.RFC1123Z) + "\r\n")
	b.WriteString("Message-ID: " + messageID() + "\r\n")
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=UTF-8\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n")
	b.WriteString("\r\n")
	b.WriteString(body)
	return []byte(b.String())
}

// encodeHeader RFC 2047-encodes a header value when it contains non-ASCII
// bytes (plain ASCII is passed through unchanged).
func encodeHeader(s string) string {
	if isASCII(s) {
		return s
	}
	return "?UTF-8?B?" + base64.StdEncoding.EncodeToString([]byte(s)) + "?"
}

func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= 0x80 {
			return false
		}
	}
	return true
}

// messageID builds a simple RFC 5322 Message-ID (best-effort, no collision
// guarantee — it is not used for dedup).
func messageID() string {
	var buf [16]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// crypto/rand does not fail on a well-formed OS, but guard anyway.
		return fmt.Sprintf("<%d@bevin>", time.Now().UnixNano())
	}
	return fmt.Sprintf("<%s@bevin>", hex.EncodeToString(buf[:]))
}
