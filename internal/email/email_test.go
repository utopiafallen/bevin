package email

import (
	"bufio"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"fmt"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/utopiafallen/bevin/internal/config"
)

// fakeSMTP is a minimal SMTP server that accepts one message and records the
// DATA payload + the MAIL FROM / RCPT TO. It is just enough to exercise the
// SMTP client path (optional AUTH; optional STARTTLS with a real TLS
// handshake when startTLS is set + a loopback certificate is provided).
type fakeSMTP struct {
	ln           net.Listener
	addr         string
	mu           sync.Mutex
	from         string
	to           string
	data         string
	authUser     string
	expectAuth   bool
	startTLS     bool // advertise STARTTLS in EHLO + perform the handshake
	cert         *tls.Certificate
	tlsUpgraded  bool
}

func startFakeSMTP(t *testing.T, expectAuth bool) *fakeSMTP {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s := &fakeSMTP{ln: ln, addr: ln.Addr().String(), expectAuth: expectAuth}
	go s.serve()
	t.Cleanup(func() { ln.Close() })
	return s
}

// startFakeSMTPTLS is a fakeSMTP that advertises STARTTLS and completes a real
// TLS handshake when the client asks. The client must trust the certificate
// (a root pool is returned alongside it).
func startFakeSMTPTLS(t *testing.T) (*fakeSMTP, *x509.CertPool) {
	t.Helper()
	cert, pool := selfSignedLoopbackCert(t)
	s := startFakeSMTP(t, false)
	s.startTLS = true
	s.cert = &cert
	return s, pool
}

// selfSignedLoopbackCert builds a self-signed certificate valid for
// 127.0.0.1 (IP SAN), plus a root pool that trusts it — so the client's
// ServerName-verified handshake succeeds against the fake server.
func selfSignedLoopbackCert(t *testing.T) (tls.Certificate, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "127.0.0.1"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		IPAddresses:  []net.IP{net.ParseIP("127.0.0.1")},
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(leaf)
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}, pool
}

func (s *fakeSMTP) serve() {
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	var c net.Conn = conn
	r := bufio.NewReader(c)
	fmt.Fprintln(c, "220 fake ESMTP")
	// Read + reply to commands until QUIT (one message).
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(strings.ToUpper(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			caps := "250-fake\n"
			if s.startTLS {
				caps += "250-STARTTLS\n"
			}
			caps += "250-AUTH PLAIN\n250 OK\n"
			fmt.Fprint(c, caps)
		case cmd == "STARTTLS":
			if s.startTLS && s.cert != nil {
				fmt.Fprintln(c, "220 Ready to start TLS")
				tc := tls.Server(c, &tls.Config{Certificates: []tls.Certificate{*s.cert}})
				if err := tc.Handshake(); err != nil {
					return
				}
				c = tc
				r = bufio.NewReader(c)
				s.mu.Lock()
				s.tlsUpgraded = true
				s.mu.Unlock()
			} else {
				fmt.Fprintln(c, "250 OK")
			}
		case strings.HasPrefix(cmd, "AUTH"):
			if s.expectAuth {
				s.mu.Lock()
				s.authUser = line
				s.mu.Unlock()
				fmt.Fprintln(c, "235 2.7.0 Authentication successful")
			} else {
				fmt.Fprintln(c, "535 5.7.8 Authentication failed")
			}
		case strings.HasPrefix(cmd, "MAIL FROM:"):
			s.mu.Lock()
			s.from = line
			s.mu.Unlock()
			fmt.Fprintln(c, "250 2.1.0 OK")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			s.mu.Lock()
			s.to = line
			s.mu.Unlock()
			fmt.Fprintln(c, "250 2.1.5 OK")
		case cmd == "DATA":
			fmt.Fprintln(c, "354 End data with <CR><LF>.<CR><LF>")
			// Read the message until the lone ".".
			var msg strings.Builder
			for {
				mline, err := r.ReadString('\n')
				if err != nil {
					return
				}
				if mline == ".\r\n" || mline == ".\n" {
					break
				}
				msg.WriteString(mline)
			}
			s.mu.Lock()
			s.data = msg.String()
			s.mu.Unlock()
			fmt.Fprintln(c, "250 2.0.0 Accepted")
		case cmd == "QUIT":
			fmt.Fprintln(c, "221 Bye")
			return
		default:
			fmt.Fprintln(c, "250 OK")
		}
	}
}

func (s *fakeSMTP) hostPort() (string, int) {
	host, portStr, _ := net.SplitHostPort(s.addr)
	var port int
	fmt.Sscanf(portStr, "%d", &port)
	return host, port
}

// TestSendDeliversToFakeSMTP verifies a configured Sender delivers a message
// to the (fake) SMTP server.
func TestSendDeliversToFakeSMTP(t *testing.T) {
	host, port := startFakeSMTP(t, false).hostPort()
	s := New(config.Email{
		Enabled: true, SMTPHost: host, SMTPPort: port,
		FromAddress: "bevin@example.com", FromName: "Bevin",
	})
	if s == nil {
		t.Fatal("sender should be configured")
	}
	if err := s.Send(context.Background(), "user@example.com", "hello", "body line"); err != nil {
		t.Fatalf("Send: %v", err)
	}
}

// TestSendNoAuthWithCredsFails verifies AUTH is attempted when a username is
// set (the fake server rejects it when expectAuth is false).
func TestSendWithAuth(t *testing.T) {
	fake := startFakeSMTP(t, true)
	host, port := fake.hostPort()
	s := New(config.Email{
		Enabled: true, SMTPHost: host, SMTPPort: port,
		Username: "bevin@example.com", Password: "pw",
		FromAddress: "bevin@example.com",
	})
	if err := s.Send(context.Background(), "user@example.com", "hello", "body"); err != nil {
		t.Fatalf("Send with auth: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !strings.Contains(fake.authUser, "AUTH") {
		t.Fatalf("auth was not attempted (server saw %q)", fake.authUser)
	}
}

// TestNilSenderNoop verifies a nil Sender (email not configured) is a no-op.
func TestNilSenderNoop(t *testing.T) {
	var s *Sender
	if err := s.Send(context.Background(), "user@example.com", "hello", "body"); err != nil {
		t.Fatalf("nil sender Send should be a no-op (nil), got %v", err)
	}
	if s.Configured() {
		t.Fatal("nil sender should report Configured=false")
	}
}

// TestNewNilWhenUnconfigured verifies New returns nil when email is not
// configured.
func TestNewNilWhenUnconfigured(t *testing.T) {
	if New(config.Email{}) != nil {
		t.Fatal("empty config -> nil sender")
	}
	if New(config.Email{Enabled: true}) != nil {
		t.Fatal("enabled but no host -> nil sender")
	}
	if New(config.Email{Enabled: true, SMTPHost: "h"}) == nil {
		t.Fatal("enabled + host -> non-nil sender")
	}
}

// TestBuildMessage verifies the message format (From/To/Subject/Content-Type).
func TestBuildMessage(t *testing.T) {
	cfg := config.Email{FromAddress: "bevin@example.com", FromName: "Bevin"}
	msg := string(buildMessage(cfg, "user@example.com", "hello", "body line"))
	for _, want := range []string{
		"From: Bevin <bevin@example.com>",
		"To: user@example.com",
		"Subject: hello",
		"Content-Type: text/plain; charset=UTF-8",
		"\r\n\r\nbody line",
	} {
		if !strings.Contains(msg, want) {
			t.Fatalf("message missing %q:\n%s", want, msg)
		}
	}
}

// TestBuildMessageNonASCIISubject verifies a non-ASCII subject is RFC 2047
// encoded in the FULL encoded-word form (=?UTF-8?B?data?=) and that it
// round-trips: the client must be able to decode it back to the original
// subject (a missing leading/trailing "=" makes clients show the raw
// "?UTF-8?B?..." string).
func TestBuildMessageNonASCIISubject(t *testing.T) {
	cfg := config.Email{FromAddress: "bevin@example.com"}
	msg := string(buildMessage(cfg, "user@example.com", "héllo", "body"))
	const prefix = "Subject: =?UTF-8?B?"
	i := strings.Index(msg, prefix)
	if i < 0 {
		t.Fatalf("subject not encoded as an RFC 2047 encoded-word (=%s...=?)\n%s", "UTF-8?B?", msg)
	}
	rest := msg[i+len(prefix):]
	j := strings.Index(rest, "=?")
	if j < 0 {
		t.Fatalf("encoded-word not terminated with =?:\n%s", msg)
	}
	b64 := rest[:j]
	raw, err := base64.StdEncoding.DecodeString(b64)
	if err != nil {
		t.Fatalf("encoded subject is not valid base64: %v (%q)", err, b64)
	}
	if string(raw) != "héllo" {
		t.Fatalf("encoded subject decodes to %q, want %q", string(raw), "héllo")
	}
}

// TestSendStartTLSUpgrades verifies the client upgrades to STARTTLS when the
// server offers it (required in the config): a real TLS handshake happens and
// the message is delivered over the encrypted session.
func TestSendStartTLSUpgrades(t *testing.T) {
	fake, pool := startFakeSMTPTLS(t)
	host, port := fake.hostPort()
	s := New(config.Email{
		Enabled: true, SMTPHost: host, SMTPPort: port, UseStartTLS: true,
		FromAddress: "bevin@example.com", FromName: "Bevin",
	})
	s.tlsOverride = &tls.Config{RootCAs: pool}
	if err := s.Send(context.Background(), "user@example.com", "hello", "body"); err != nil {
		t.Fatalf("Send over STARTTLS: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !fake.tlsUpgraded {
		t.Fatal("server never saw a TLS upgrade")
	}
	if !strings.Contains(fake.from, "bevin@example.com") {
		t.Fatalf("message not delivered over TLS (server saw from=%q)", fake.from)
	}
}

// TestSendStartTLSOptional verifies bevin upgrades opportunistically even when
// the config does not require it (matching the wizard help text: a plain relay
// that offers STARTTLS gets encrypted traffic anyway).
func TestSendStartTLSOptional(t *testing.T) {
	fake, pool := startFakeSMTPTLS(t)
	host, port := fake.hostPort()
	s := New(config.Email{
		Enabled: true, SMTPHost: host, SMTPPort: port, UseStartTLS: false,
		FromAddress: "bevin@example.com",
	})
	s.tlsOverride = &tls.Config{RootCAs: pool}
	if err := s.Send(context.Background(), "user@example.com", "hello", "body"); err != nil {
		t.Fatalf("Send with offered STARTTLS: %v", err)
	}
	fake.mu.Lock()
	defer fake.mu.Unlock()
	if !fake.tlsUpgraded {
		t.Fatal("offered STARTTLS should have been used even when not required")
	}
}

// TestSendStartTLSRequiredNotOffered verifies a config that requires STARTTLS
// fails (rather than silently sending plaintext) when the server does not
// offer it.
func TestSendStartTLSRequiredNotOffered(t *testing.T) {
	fake := startFakeSMTP(t, false)
	host, port := fake.hostPort()
	s := New(config.Email{
		Enabled: true, SMTPHost: host, SMTPPort: port, UseStartTLS: true,
		FromAddress: "bevin@example.com",
	})
	err := s.Send(context.Background(), "user@example.com", "hello", "body")
	if err == nil {
		t.Fatal("required STARTTLS with a non-offering server must fail")
	}
	if !strings.Contains(err.Error(), "does not offer") {
		t.Fatalf("error = %v (want the STARTTLS-not-offered error)", err)
	}
}
