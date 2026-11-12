//go:build integration

package integration_test

import (
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/base64"
	"encoding/pem"
	"errors"
	"fmt"
	"io"
	"math/big"
	"mime"
	"mime/multipart"
	"mime/quotedprintable"
	"net"
	"net/mail"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/emersion/go-smtp"

	"github.com/NGRsoftlab/mailer"
)

// ---------------------------------------------------------------------------
// Test SMTP backend: collects delivered messages so tests can assert on them.
// ---------------------------------------------------------------------------

type testBackend struct {
	mu       sync.Mutex
	messages []*testMessage
}

type testMessage struct {
	From string
	To   []string
	Data []byte
}

func (b *testBackend) NewSession(c *smtp.Conn) (smtp.Session, error) {
	return &testSession{backend: b}, nil
}

type testSession struct {
	backend *testBackend
	from    string
	to      []string
}

func (s *testSession) Mail(from string, opts *smtp.MailOptions) error {
	s.from = from
	return nil
}

func (s *testSession) Rcpt(to string, opts *smtp.RcptOptions) error {
	s.to = append(s.to, to)
	return nil
}

func (s *testSession) Data(r io.Reader) error {
	data, err := io.ReadAll(r)
	if err != nil {
		return err
	}
	s.backend.mu.Lock()
	s.backend.messages = append(s.backend.messages, &testMessage{
		From: s.from,
		To:   s.to,
		Data: data,
	})
	s.backend.mu.Unlock()
	return nil
}

func (s *testSession) Reset()        { s.from, s.to = "", nil }
func (s *testSession) Logout() error { return nil }

// ---------------------------------------------------------------------------
// Helpers to start a local SMTP server.
// ---------------------------------------------------------------------------

func startPlainServer(t *testing.T, b *testBackend) (addr string, stop func()) {
	t.Helper()

	s := smtp.NewServer(b)
	s.Addr = "127.0.0.1:0"
	s.Domain = "localhost"
	s.AllowInsecureAuth = true
	s.ErrorLog = nil

	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s.Addr = ln.Addr().String()

	go func() {
		if err := s.Serve(ln); err != nil && !strings.Contains(err.Error(), "closed") {
			t.Logf("smtp server stopped: %v", err)
		}
	}()

	return s.Addr, func() { _ = s.Close() }
}

func startTLSServer(t *testing.T, b *testBackend, implicit bool) (addr string, stop func()) {
	t.Helper()

	cert, err := generateSelfSignedCert()
	if err != nil {
		t.Fatalf("generate cert: %v", err)
	}

	s := smtp.NewServer(b)
	s.Addr = "127.0.0.1:0"
	s.Domain = "localhost"
	s.AllowInsecureAuth = true
	s.ErrorLog = nil

	ln, err := net.Listen("tcp", s.Addr)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	s.Addr = ln.Addr().String()

	if implicit {
		// Implicit TLS: wrap the listener so every accepted connection
		// starts with a TLS handshake before any SMTP traffic.
		tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}})
		go func() {
			if err := s.Serve(tlsLn); err != nil && !strings.Contains(err.Error(), "closed") {
				t.Logf("smtp server stopped: %v", err)
			}
		}()
	} else {
		// STARTTLS: plaintext listener, server advertises STARTTLS
		// because TLSConfig is set.
		s.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
		go func() {
			if err := s.Serve(ln); err != nil && !strings.Contains(err.Error(), "closed") {
				t.Logf("smtp server stopped: %v", err)
			}
		}()
	}

	return s.Addr, func() { _ = s.Close() }
}

func generateSelfSignedCert() (tls.Certificate, error) {
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		return tls.Certificate{}, err
	}

	template := x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(24 * time.Hour),
		KeyUsage:     x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}

	derBytes, err := x509.CreateCertificate(rand.Reader, &template, &template, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: derBytes})
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "RSA PRIVATE KEY", Bytes: x509.MarshalPKCS1PrivateKey(key)})
	return tls.X509KeyPair(certPEM, keyPEM)
}

// ---------------------------------------------------------------------------
// Assertions.
// ---------------------------------------------------------------------------

func waitForMessage(t *testing.T, b *testBackend, timeout time.Duration) *testMessage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		if len(b.messages) > 0 {
			msg := b.messages[0]
			b.mu.Unlock()
			return msg
		}
		b.mu.Unlock()
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("timed out waiting for message")
	return nil
}

// extractTextBody decodes the text/plain part of a raw MIME message,
// handling both quoted-printable and base64 transfer encodings.
func extractTextBody(t *testing.T, raw []byte) string {
	t.Helper()

	msg, err := mail.ReadMessage(strings.NewReader(string(raw)))
	if err != nil {
		t.Fatalf("parse message: %v", err)
	}

	mediaType, params, err := mime.ParseMediaType(msg.Header.Get("Content-Type"))
	if err != nil {
		t.Fatalf("parse content type: %v", err)
	}
	if !strings.HasPrefix(mediaType, "multipart/") {
		// Single-part message.
		return readPart(t, msg.Header.Get("Content-Transfer-Encoding"), msg.Body)
	}

	// Multipart: iterate parts and return the first text/plain body.
	mr := multipartReader(t, msg.Body, params["boundary"])
	for {
		part, err := mr.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("read part: %v", err)
		}
		partType, _, err := mime.ParseMediaType(part.Header.Get("Content-Type"))
		if err != nil {
			continue
		}
		if partType == "text/plain" {
			return readPart(t, part.Header.Get("Content-Transfer-Encoding"), part)
		}
	}
	t.Fatal("no text/plain part found")
	return ""
}

func readPart(t *testing.T, transferEncoding string, r io.Reader) string {
	t.Helper()
	var reader io.Reader = r
	switch strings.ToLower(strings.TrimSpace(transferEncoding)) {
	case "base64":
		reader = base64.NewDecoder(base64.StdEncoding, r)
	case "quoted-printable":
		reader = quotedprintable.NewReader(r)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return string(data)
}

// multipartReader is a tiny helper to avoid importing mime/multipart
// at the top level with an alias.
func multipartReader(t *testing.T, r io.Reader, boundary string) *multipart.Reader {
	t.Helper()
	return multipart.NewReader(r, boundary)
}

func assertBodyContains(t *testing.T, raw []byte, substr string) {
	t.Helper()
	body := extractTextBody(t, raw)
	if !strings.Contains(body, substr) {
		t.Errorf("body = %q, want it to contain %q", body, substr)
	}
}

// ---------------------------------------------------------------------------
// Tests.
// ---------------------------------------------------------------------------

func TestPlainSend(t *testing.T) {
	backend := &testBackend{}
	addr, stop := startPlainServer(t, backend)
	defer stop()

	host, portStr, _ := net.SplitHostPort(addr)
	m, err := mailer.New(mailer.Config{
		Host: host,
		Port: mustAtoi(portStr),
	})
	if err != nil {
		t.Fatalf("mailer.New: %v", err)
	}
	defer m.Close()

	msg := &mailer.Message{
		From:    "sender@example.com",
		To:      []string{"rcpt@example.com"},
		Subject: "Plain send",
		Text:    "Hello from integration test",
	}

	if err := m.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	delivered := waitForMessage(t, backend, 2*time.Second)
	if delivered.From != "sender@example.com" {
		t.Errorf("From = %q, want sender@example.com", delivered.From)
	}
	if len(delivered.To) != 1 || delivered.To[0] != "rcpt@example.com" {
		t.Errorf("To = %v, want [rcpt@example.com]", delivered.To)
	}
	assertBodyContains(t, delivered.Data, "Hello from integration test")
}

func TestSendBulk(t *testing.T) {
	backend := &testBackend{}
	addr, stop := startPlainServer(t, backend)
	defer stop()

	host, portStr, _ := net.SplitHostPort(addr)
	m, err := mailer.New(mailer.Config{
		Host:           host,
		Port:           mustAtoi(portStr),
		MaxConcurrency: 2,
	})
	if err != nil {
		t.Fatalf("mailer.New: %v", err)
	}
	defer m.Close()

	msg := &mailer.Message{
		From:    "sender@example.com",
		Subject: "Bulk",
		Text:    "Bulk body",
	}
	recipients := []string{"a@example.com", "b@example.com", "c@example.com"}

	if err := m.SendBulk(context.Background(), msg, recipients); err != nil {
		t.Fatalf("SendBulk: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		backend.mu.Lock()
		n := len(backend.messages)
		backend.mu.Unlock()
		if n >= len(recipients) {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.messages) != len(recipients) {
		t.Fatalf("got %d messages, want %d", len(backend.messages), len(recipients))
	}

	seen := make(map[string]bool)
	for _, delivered := range backend.messages {
		if len(delivered.To) != 1 {
			t.Errorf("bulk message has %d recipients, want 1", len(delivered.To))
		}
		seen[delivered.To[0]] = true
		assertBodyContains(t, delivered.Data, "Bulk body")
	}
	for _, want := range recipients {
		if !seen[want] {
			t.Errorf("missing personalized message for %s", want)
		}
	}
}

func TestStartTLS(t *testing.T) {
	backend := &testBackend{}
	addr, stop := startTLSServer(t, backend, false)
	defer stop()

	host, portStr, _ := net.SplitHostPort(addr)
	m, err := mailer.New(mailer.Config{
		Host:    host,
		Port:    mustAtoi(portStr),
		TLSMode: mailer.TLSStart,
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	})
	if err != nil {
		t.Fatalf("mailer.New: %v", err)
	}
	defer m.Close()

	msg := &mailer.Message{
		From:    "sender@example.com",
		To:      []string{"rcpt@example.com"},
		Subject: "STARTTLS",
		Text:    "Encrypted hello",
	}
	if err := m.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send over STARTTLS: %v", err)
	}

	delivered := waitForMessage(t, backend, 2*time.Second)
	assertBodyContains(t, delivered.Data, "Encrypted hello")
}

func TestImplicitTLS(t *testing.T) {
	backend := &testBackend{}
	addr, stop := startTLSServer(t, backend, true)
	defer stop()

	host, portStr, _ := net.SplitHostPort(addr)
	m, err := mailer.New(mailer.Config{
		Host:    host,
		Port:    mustAtoi(portStr),
		TLSMode: mailer.TLSImplicit,
		TLSConfig: &tls.Config{
			InsecureSkipVerify: true,
		},
	})
	if err != nil {
		t.Fatalf("mailer.New: %v", err)
	}
	defer m.Close()

	msg := &mailer.Message{
		From:    "sender@example.com",
		To:      []string{"rcpt@example.com"},
		Subject: "Implicit TLS",
		Text:    "SMTPS hello",
	}
	if err := m.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send over implicit TLS: %v", err)
	}

	delivered := waitForMessage(t, backend, 2*time.Second)
	assertBodyContains(t, delivered.Data, "SMTPS hello")
}

func TestContextCancellation(t *testing.T) {
	backend := &testBackend{}
	addr, stop := startPlainServer(t, backend)
	defer stop()

	host, portStr, _ := net.SplitHostPort(addr)
	m, err := mailer.New(mailer.Config{
		Host: host,
		Port: mustAtoi(portStr),
	})
	if err != nil {
		t.Fatalf("mailer.New: %v", err)
	}
	defer m.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	msg := &mailer.Message{
		From:    "sender@example.com",
		To:      []string{"rcpt@example.com"},
		Subject: "Cancelled",
		Text:    "Should not be sent",
	}
	err = m.Send(ctx, msg)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}
}

func mustAtoi(s string) int {
	n := 0
	for _, r := range s {
		if r < '0' || r > '9' {
			panic(fmt.Sprintf("not a number: %q", s))
		}
		n = n*10 + int(r-'0')
	}
	return n
}
