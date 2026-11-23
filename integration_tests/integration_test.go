//go:build integration

package integration_tests

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
	"sync/atomic"
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
	// failRcpt lists recipients that the backend should reject with a 550
	// at the RCPT stage. Used by partial-failure tests; nil map means
	// "accept everyone".
	failRcpt map[string]bool
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
	s.backend.mu.Lock()
	reject := s.backend.failRcpt[to]
	s.backend.mu.Unlock()
	if reject {
		return &smtp.SMTPError{
			Code:         550,
			EnhancedCode: smtp.EnhancedCode{5, 1, 1},
			Message:      "recipient rejected",
		}
	}
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
		tlsLn := tls.NewListener(ln, &tls.Config{Certificates: []tls.Certificate{cert}})
		go func() {
			if err := s.Serve(tlsLn); err != nil && !strings.Contains(err.Error(), "closed") {
				t.Logf("smtp server stopped: %v", err)
			}
		}()
	} else {
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

func waitForMessageCount(t *testing.T, b *testBackend, n int, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b.mu.Lock()
		got := len(b.messages)
		b.mu.Unlock()
		if got >= n {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	b.mu.Lock()
	got := len(b.messages)
	b.mu.Unlock()
	t.Fatalf("timed out waiting for %d messages, got %d", n, got)
}

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
		return readPart(t, msg.Header.Get("Content-Transfer-Encoding"), msg.Body)
	}

	mr := multipart.NewReader(msg.Body, params["boundary"])
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

func assertBodyContains(t *testing.T, raw []byte, substr string) {
	t.Helper()
	body := extractTextBody(t, raw)
	if !strings.Contains(body, substr) {
		t.Errorf("body = %q, want it to contain %q", body, substr)
	}
}

// ---------------------------------------------------------------------------
// Existing tests.
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

	waitForMessageCount(t, backend, len(recipients), 3*time.Second)

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

func TestContextCancellationWithIdleConn(t *testing.T) {
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

	warmup := &mailer.Message{
		From:    "sender@example.com",
		To:      []string{"rcpt@example.com"},
		Subject: "warmup",
		Text:    "warmup",
	}
	if err := m.Send(context.Background(), warmup); err != nil {
		t.Fatalf("warmup Send: %v", err)
	}
	_ = waitForMessage(t, backend, 2*time.Second)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err = m.Send(ctx, &mailer.Message{
		From:    "sender@example.com",
		To:      []string{"rcpt@example.com"},
		Subject: "cancelled",
		Text:    "should not be sent",
	})
	if err == nil {
		t.Fatal("expected error from cancelled context with idle conn in pool")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled", err)
	}

	time.Sleep(100 * time.Millisecond)
	backend.mu.Lock()
	n := len(backend.messages)
	backend.mu.Unlock()
	if n != 1 {
		t.Errorf("delivered %d messages, want 1 (cancelled send must not deliver)", n)
	}
}

func TestContextCancellationDuringDialogue(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	stop := make(chan struct{})
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				<-stop
				_ = conn.Close()
			}(c)
		}
	}()
	defer close(stop)

	host, portStr, _ := net.SplitHostPort(ln.Addr().String())
	m, err := mailer.New(mailer.Config{
		Host:    host,
		Port:    mustAtoi(portStr),
		Timeout: 30 * time.Second,
	})
	if err != nil {
		t.Fatalf("mailer.New: %v", err)
	}
	defer m.Close()

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()

	msg := &mailer.Message{
		From:    "sender@example.com",
		To:      []string{"rcpt@example.com"},
		Subject: "cancelled during dialogue",
		Text:    "should not be sent",
	}

	start := time.Now()
	err = m.Send(ctx, msg)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("expected error from cancelled context")
	}
	if !errors.Is(err, context.Canceled) {
		t.Errorf("error = %v, want context.Canceled (translated from socket error)", err)
	}
	if elapsed > 2*time.Second {
		t.Fatalf("Send took %v, want prompt return on ctx cancel", elapsed)
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

// ---------------------------------------------------------------------------
// Group E: new coverage.
// ---------------------------------------------------------------------------

// TestSendBulkPartialFailure verifies that one rejected recipient does
// not prevent delivery to the rest: SendBulk returns a joined error for
// the bad address, the good addresses still receive their personalized
// copy, and the backend observes exactly the expected messages.
func TestSendBulkPartialFailure(t *testing.T) {
	backend := &testBackend{
		failRcpt: map[string]bool{
			"bad@example.com": true,
		},
	}
	addr, stop := startPlainServer(t, backend)
	defer stop()

	host, portStr, _ := net.SplitHostPort(addr)
	m, err := mailer.New(mailer.Config{
		Host:           host,
		Port:           mustAtoi(portStr),
		MaxConcurrency: 4,
	})
	if err != nil {
		t.Fatalf("mailer.New: %v", err)
	}
	defer m.Close()

	msg := &mailer.Message{
		From:    "sender@example.com",
		Subject: "Bulk partial",
		Text:    "Bulk body",
	}
	recipients := []string{"a@example.com", "bad@example.com", "c@example.com"}

	err = m.SendBulk(context.Background(), msg, recipients)
	if err == nil {
		t.Fatal("expected error from partial bulk failure")
	}
	// Error must mention the rejected recipient.
	if !strings.Contains(err.Error(), "bad@example.com") {
		t.Errorf("error = %v, want it to mention bad@example.com", err)
	}

	// The two good recipients must have been delivered.
	waitForMessageCount(t, backend, 2, 3*time.Second)
	time.Sleep(100 * time.Millisecond) // allow any spurious third message

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.messages) != 2 {
		t.Fatalf("delivered %d messages, want 2", len(backend.messages))
	}
	seen := make(map[string]bool)
	for _, d := range backend.messages {
		if len(d.To) != 1 {
			t.Errorf("bulk message has %d recipients, want 1", len(d.To))
		}
		seen[d.To[0]] = true
	}
	if !seen["a@example.com"] || !seen["c@example.com"] {
		t.Errorf("delivered set = %v, want {a@example.com, c@example.com}", seen)
	}
}

// TestMaxOpenWithRealServer verifies that MaxOpen caps the number of
// concurrent dials against a real server, and that the second Send
// reuses the idle connection after the first one returns to the pool.
func TestMaxOpenWithRealServer(t *testing.T) {
	backend := &testBackend{}
	addr, stop := startPlainServer(t, backend)
	defer stop()

	host, portStr, _ := net.SplitHostPort(addr)
	var dials atomic.Int32
	m, err := mailer.New(mailer.Config{
		Host:    host,
		Port:    mustAtoi(portStr),
		MaxOpen: 1,
		MaxIdle: 1,
		OnDial: func(_ time.Duration, _ error) {
			dials.Add(1)
		},
	})
	if err != nil {
		t.Fatalf("mailer.New: %v", err)
	}
	defer m.Close()

	msg := &mailer.Message{
		From:    "sender@example.com",
		To:      []string{"rcpt@example.com"},
		Subject: "parallel",
		Text:    "parallel body",
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = m.Send(context.Background(), msg)
		}(i)
	}
	wg.Wait()

	for i, e := range errs {
		if e != nil {
			t.Errorf("Send #%d: %v", i, e)
		}
	}
	if got := dials.Load(); got != 1 {
		t.Errorf("dials = %d, want 1 (MaxOpen=1 must serialise dials and reuse idle)", got)
	}
	waitForMessageCount(t, backend, 2, 3*time.Second)
}

// TestIdleReuseWithRealServer verifies that two sequential Send calls
// share one connection: the second finds the idle connection in the
// pool and reuses it instead of dialing again.
func TestIdleReuseWithRealServer(t *testing.T) {
	backend := &testBackend{}
	addr, stop := startPlainServer(t, backend)
	defer stop()

	host, portStr, _ := net.SplitHostPort(addr)
	var dials atomic.Int32
	m, err := mailer.New(mailer.Config{
		Host: host,
		Port: mustAtoi(portStr),
		OnDial: func(_ time.Duration, _ error) {
			dials.Add(1)
		},
	})
	if err != nil {
		t.Fatalf("mailer.New: %v", err)
	}
	defer m.Close()

	msg := &mailer.Message{
		From:    "sender@example.com",
		To:      []string{"rcpt@example.com"},
		Subject: "reuse",
		Text:    "reuse body",
	}
	if err := m.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send #1: %v", err)
	}
	if err := m.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send #2: %v", err)
	}

	if got := dials.Load(); got != 1 {
		t.Errorf("dials = %d, want 1 (second Send must reuse idle)", got)
	}
	waitForMessageCount(t, backend, 2, 3*time.Second)
}

// TestOnSendHookWithRealServer verifies that Config.OnSend is invoked
// once per Send against a real server, with nil error on success.
func TestOnSendHookWithRealServer(t *testing.T) {
	backend := &testBackend{}
	addr, stop := startPlainServer(t, backend)
	defer stop()

	host, portStr, _ := net.SplitHostPort(addr)
	var (
		calls    atomic.Int32
		sawError atomic.Bool
		sawDurMS atomic.Int64
	)
	m, err := mailer.New(mailer.Config{
		Host: host,
		Port: mustAtoi(portStr),
		OnSend: func(d time.Duration, e error) {
			calls.Add(1)
			if e != nil {
				sawError.Store(true)
			}
			sawDurMS.Store(int64(d / time.Millisecond))
		},
	})
	if err != nil {
		t.Fatalf("mailer.New: %v", err)
	}
	defer m.Close()

	msg := &mailer.Message{
		From:    "sender@example.com",
		To:      []string{"rcpt@example.com"},
		Subject: "hook",
		Text:    "hook body",
	}
	if err := m.Send(context.Background(), msg); err != nil {
		t.Fatalf("Send: %v", err)
	}

	if got := calls.Load(); got != 1 {
		t.Fatalf("OnSend called %d times, want 1", got)
	}
	if sawError.Load() {
		t.Error("OnSend reported non-nil error on success path")
	}
	if got := sawDurMS.Load(); got < 0 {
		t.Errorf("OnSend duration = %d ms, want non-negative", got)
	}
}
