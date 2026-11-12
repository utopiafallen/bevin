package email

import (
	"bufio"
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
	"testing"

	"github.com/utopiafallen/bevin/internal/config"
)

// fakeSMTP is a minimal SMTP server that accepts one message and records the
// DATA payload + the MAIL FROM / RCPT TO. It is just enough to exercise the
// net/smtp client path (no STARTTLS, optional AUTH).
type fakeSMTP struct {
	ln        net.Listener
	addr      string
	mu        sync.Mutex
	from      string
	to        string
	data      string
	authUser  string
	expectAuth bool
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

func (s *fakeSMTP) serve() {
	conn, err := s.ln.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	r := bufio.NewReader(conn)
	fmt.Fprintln(conn, "220 fake ESMTP")
	// Read + reply to commands until QUIT (one message).
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		cmd := strings.TrimSpace(strings.ToUpper(line))
		switch {
		case strings.HasPrefix(cmd, "EHLO"), strings.HasPrefix(cmd, "HELO"):
			caps := "250-fake\n250-AUTH PLAIN\n250 OK\n"
			fmt.Fprint(conn, caps)
		case strings.HasPrefix(cmd, "AUTH"):
			if s.expectAuth {
				s.mu.Lock()
				s.authUser = line
				s.mu.Unlock()
				fmt.Fprintln(conn, "235 2.7.0 Authentication successful")
			} else {
				fmt.Fprintln(conn, "535 5.7.8 Authentication failed")
			}
		case strings.HasPrefix(cmd, "MAIL FROM:"):
			s.mu.Lock()
			s.from = line
			s.mu.Unlock()
			fmt.Fprintln(conn, "250 2.1.0 OK")
		case strings.HasPrefix(cmd, "RCPT TO:"):
			s.mu.Lock()
			s.to = line
			s.mu.Unlock()
			fmt.Fprintln(conn, "250 2.1.5 OK")
		case cmd == "DATA":
			fmt.Fprintln(conn, "354 End data with <CR><LF>.<CR><LF>")
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
			fmt.Fprintln(conn, "250 2.0.0 Accepted")
		case cmd == "QUIT":
			fmt.Fprintln(conn, "221 Bye")
			return
		default:
			fmt.Fprintln(conn, "250 OK")
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
// encoded.
func TestBuildMessageNonASCIISubject(t *testing.T) {
	cfg := config.Email{FromAddress: "bevin@example.com"}
	msg := string(buildMessage(cfg, "user@example.com", "héllo", "body"))
	if !strings.Contains(msg, "Subject: ?UTF-8?B?") {
		t.Fatalf("non-ASCII subject not encoded:\n%s", msg)
	}
}
