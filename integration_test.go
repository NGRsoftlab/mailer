//go:build integration

// Copyright 2020-2026 NGR Softlab

package mailer

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/rsa"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"errors"
	"io"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

type capturedMsg struct {
	from  string
	rcpts []string
	body  []byte
}

type testBackend struct {
	mu       sync.Mutex
	msgs     []capturedMsg
	lastAuth string
	authUser string
	authPass string
}

func (b *testBackend) NewSession(_ *smtp.Conn) (smtp.Session, error) {
	return &testSession{backend: b}, nil
}

func (b *testBackend) Login(user, pass string) (smtp.Session, error) {
	b.mu.Lock()
	b.lastAuth = user
	b.mu.Unlock()
	if b.authUser != "" && (user != b.authUser || pass != b.authPass) {
		return nil, smtp.ErrAuthFailed
	}
	return &testSession{backend: b}, nil
}

type testSession struct {
	backend *testBackend
	from    string
	rcpts   []string
	buf     bytes.Buffer
}

func (s *testSession) Mail(from string, _ *smtp.MailOptions) error {
	s.from = from
	return nil
}

func (s *testSession) Rcpt(to string, _ *smtp.RcptOptions) error {
	s.rcpts = append(s.rcpts, to)
	return nil
}

func (s *testSession) Data(r io.Reader) error {
	s.buf.Reset()
	if _, err := s.buf.ReadFrom(r); err != nil {
		return err
	}
	s.backend.mu.Lock()
	s.backend.msgs = append(s.backend.msgs, capturedMsg{
		from:  s.from,
		rcpts: append([]string(nil), s.rcpts...),
		body:  append([]byte(nil), s.buf.Bytes()...),
	})
	s.backend.mu.Unlock()
	return nil
}

func (s *testSession) Reset()        {}
func (s *testSession) Logout() error { return nil }

func startServer(t *testing.T, withTLS bool) (addr string, backend *testBackend, shutdown func()) {
	t.Helper()
	backend = &testBackend{authUser: "user", authPass: "pass"}

	s := smtp.NewServer(backend)
	s.Domain = "localhost"
	s.AllowInsecureAuth = true
	s.ReadTimeout = 5 * time.Second
	s.WriteTimeout = 5 * time.Second

	if withTLS {
		cert := makeCert(t)
		s.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	addr = ln.Addr().String()

	go func() { _ = s.Serve(ln) }()
	return addr, backend, func() { _ = s.Close() }
}

func makeCert(t *testing.T) tls.Certificate {
	t.Helper()
	key, err := rsa.GenerateKey(rand.Reader, 2048)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber: big.NewInt(1),
		Subject:      pkix.Name{CommonName: "localhost"},
		NotBefore:    time.Now().Add(-time.Hour),
		NotAfter:     time.Now().Add(time.Hour),
		KeyUsage:     x509.KeyUsageDigitalSignature | x509.KeyUsageKeyEncipherment,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		DNSNames:     []string{"localhost"},
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func TestIntegration_PlainSend(t *testing.T) {
	addr, backend, stop := startServer(t, false)
	defer stop()

	host, port := splitHostPort(t, addr)
	m, err := New(Config{Host: host, Port: port})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	msg := &Message{
		From:    "sender@example.com",
		To:      []string{"rcpt@example.com"},
		Subject: "Hello",
		Text:    "plain body",
		HTML:    "<p>html body</p>",
	}
	if err := m.Send(context.Background(), msg); err != nil {
		t.Fatal(err)
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(backend.msgs))
	}
	if len(backend.msgs[0].rcpts) != 1 || backend.msgs[0].rcpts[0] != "rcpt@example.com" {
		t.Fatalf("unexpected rcpts: %v", backend.msgs[0].rcpts)
	}
	if !strings.Contains(string(backend.msgs[0].body), "Subject: Hello") {
		t.Fatalf("body missing Subject header")
	}
}

func TestIntegration_PlainAuth(t *testing.T) {
	addr, backend, stop := startServer(t, false)
	defer stop()

	host, port := splitHostPort(t, addr)
	m, err := New(Config{
		Host: host, Port: port,
		Auth: PlainAuth("", "user", "pass", host),
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	if err := m.Send(context.Background(), basicMessage()); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	if backend.lastAuth != "user" {
		t.Fatalf("expected auth as user, got %q", backend.lastAuth)
	}
	backend.mu.Unlock()
}

func TestIntegration_StartTLS(t *testing.T) {
	addr, backend, stop := startServer(t, true)
	defer stop()

	host, port := splitHostPort(t, addr)
	m, err := New(Config{
		Host: host, Port: port,
		TLSMode:   TLSStart,
		TLSConfig: &tls.Config{InsecureSkipVerify: true}, // self-signed test cert
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	if err := m.Send(context.Background(), basicMessage()); err != nil {
		t.Fatal(err)
	}
	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.msgs) != 1 {
		t.Fatalf("want 1 message, got %d", len(backend.msgs))
	}
}

func TestIntegration_ImplicitTLS(t *testing.T) {
	cert := makeCert(t)
	backend := &testBackend{}
	s := smtp.NewServer(backend)
	s.Domain = "localhost"
	s.TLSConfig = &tls.Config{Certificates: []tls.Certificate{cert}}

	ln, err := tls.Listen("tcp", "127.0.0.1:0", &tls.Config{Certificates: []tls.Certificate{cert}})
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	go func() { _ = s.Serve(ln) }()
	defer s.Close()

	host, port := splitHostPort(t, addr)
	m, err := New(Config{
		Host: host, Port: port,
		TLSMode:   TLSImplicit,
		TLSConfig: &tls.Config{InsecureSkipVerify: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	if err := m.Send(context.Background(), basicMessage()); err != nil {
		t.Fatal(err)
	}
}

func TestIntegration_BulkParallel(t *testing.T) {
	addr, backend, stop := startServer(t, false)
	defer stop()

	host, port := splitHostPort(t, addr)
	m, err := New(Config{Host: host, Port: port, MaxConcurrency: 4, MaxIdle: 8})
	if err != nil {
		t.Fatal(err)
	}
	defer m.Close()

	recipients := make([]string, 8)
	for i := range recipients {
		recipients[i] = "u" + string(rune('a'+i)) + "@example.com"
	}
	if err := m.SendBulk(context.Background(), &Message{
		From:    "sender@example.com",
		Subject: "bulk",
		Text:    "body",
	}, recipients); err != nil {
		t.Fatal(err)
	}

	backend.mu.Lock()
	defer backend.mu.Unlock()
	if len(backend.msgs) != len(recipients) {
		t.Fatalf("want %d messages, got %d", len(recipients), len(backend.msgs))
	}
	// Each recipient gets exactly one To header of themselves.
	for _, msg := range backend.msgs {
		body := string(msg.body)
		for _, other := range recipients {
			if other == msg.rcpts[0] {
				continue
			}
			if strings.Contains(body, "To: "+other) {
				t.Fatalf("recipient %s leaked into %s's To header", other, msg.rcpts[0])
			}
		}
	}
}

func TestIntegration_ContextTimeout(t *testing.T) {
	addr, _, stop := startServer(t, false)
	defer stop()

	host, port := splitHostPort(t, addr)
	m, _ := New(Config{Host: host, Port: port})
	defer m.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := m.Send(ctx, basicMessage()); !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func splitHostPort(t *testing.T, addr string) (string, int) {
	t.Helper()
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatal(err)
	}
	var port int
	if _, err := fmt.Sscanf(p, "%d", &port); err != nil {
		t.Fatal(err)
	}
	return h, port
}
