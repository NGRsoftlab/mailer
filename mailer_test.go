// Copyright 2020-2026 NGR Softlab
package mailer

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/smtp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// mockSMTP implements smtpClient for tests.
type mockSMTP struct {
	mu sync.Mutex

	noopErr, authErr, mailErr, rcptErr, dataErr, writeErr, closeErr error
	mailDelay                                                       time.Duration

	mails     []string
	rcpts     []string
	authCalls int
	noopCalls int
	dataBody  []byte
	closed    bool

	harness *testHarness
}

func (m *mockSMTP) Noop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.noopCalls++
	return m.noopErr
}

func (m *mockSMTP) Auth(_ smtp.Auth) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.authCalls++
	return m.authErr
}

func (m *mockSMTP) Extension(_ string) (bool, string) { return true, "" }

func (m *mockSMTP) Mail(from string) error {
	m.mu.Lock()
	if m.mailErr != nil {
		err := m.mailErr
		m.mu.Unlock()
		return err
	}
	m.mails = append(m.mails, from)
	delay := m.mailDelay
	h := m.harness
	m.mu.Unlock()

	if h != nil {
		n := atomic.AddInt32(&h.active, 1)
		for {
			old := atomic.LoadInt32(&h.maxConcurrent)
			if n <= old || atomic.CompareAndSwapInt32(&h.maxConcurrent, old, n) {
				break
			}
		}
		defer atomic.AddInt32(&h.active, -1)
	}

	if delay > 0 {
		time.Sleep(delay)
	}
	return nil
}

func (m *mockSMTP) Rcpt(to string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rcptErr != nil {
		return m.rcptErr
	}
	m.rcpts = append(m.rcpts, to)
	return nil
}

func (m *mockSMTP) Data() (io.WriteCloser, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.dataErr != nil {
		return nil, m.dataErr
	}
	return &mockData{parent: m}, nil
}

func (m *mockSMTP) Close() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.closed = true
	return m.closeErr
}

type mockData struct {
	parent *mockSMTP
	buf    []byte
}

func (d *mockData) Write(p []byte) (int, error) {
	d.parent.mu.Lock()
	defer d.parent.mu.Unlock()
	if d.parent.writeErr != nil {
		return 0, d.parent.writeErr
	}
	d.buf = append(d.buf, p...)
	return len(p), nil
}

func (d *mockData) Close() error {
	d.parent.mu.Lock()
	defer d.parent.mu.Unlock()
	d.parent.dataBody = append(d.parent.dataBody, d.buf...)
	return nil
}

// testHarness tracks connections and concurrency across a test.
type testHarness struct {
	mu            sync.Mutex
	conns         []*mockSMTP
	mailDelay     time.Duration
	active        int32
	maxConcurrent int32
}

func (h *testHarness) dial(_ context.Context) (smtpClient, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	m := &mockSMTP{harness: h, mailDelay: h.mailDelay}
	h.conns = append(h.conns, m)
	return m, nil
}

func (h *testHarness) snapshot() []*mockSMTP {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*mockSMTP, len(h.conns))
	copy(out, h.conns)
	return out
}

func basicMessage() *Message {
	return &Message{
		From:    "from@example.com",
		To:      []string{"to@example.com"},
		Subject: "Hello",
		Text:    "body",
	}
}

func TestSend_Success(t *testing.T) {
	h := &testHarness{}
	m, _ := newWithDial(Config{Host: "test"}, h.dial)
	defer m.Close()

	if err := m.Send(context.Background(), basicMessage()); err != nil {
		t.Fatal(err)
	}

	conns := h.snapshot()
	if len(conns) != 1 {
		t.Fatalf("want 1 connection, got %d", len(conns))
	}
	c := conns[0]
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.mails) != 1 || len(c.rcpts) != 1 {
		t.Fatalf("want 1 mail+rcpt, got %d/%d", len(c.mails), len(c.rcpts))
	}
	if len(c.dataBody) == 0 {
		t.Fatal("expected data to be written")
	}
}

func TestSend_AuthOnceAcrossReuse(t *testing.T) {
	h := &testHarness{}
	auth := PlainAuth("", "u", "p", "test")
	m, _ := newWithDial(Config{Host: "test", Auth: auth}, h.dial)
	defer m.Close()

	for i := 0; i < 3; i++ {
		if err := m.Send(context.Background(), basicMessage()); err != nil {
			t.Fatal(err)
		}
	}

	conns := h.snapshot()
	if len(conns) != 1 {
		t.Fatalf("want 1 reused connection, got %d", len(conns))
	}
	conns[0].mu.Lock()
	defer conns[0].mu.Unlock()
	if conns[0].authCalls != 1 {
		t.Fatalf("AUTH must be issued exactly once, got %d", conns[0].authCalls)
	}
}

func TestPool_ReconnectsOnDeadConnection(t *testing.T) {
	h := &testHarness{}
	m, _ := newWithDial(Config{Host: "test"}, h.dial)
	defer m.Close()

	if err := m.Send(context.Background(), basicMessage()); err != nil {
		t.Fatal(err)
	}

	conns := h.snapshot()
	if len(conns) != 1 {
		t.Fatalf("want 1 connection after first send, got %d", len(conns))
	}
	dead := conns[0]
	dead.mu.Lock()
	dead.noopErr = errors.New("dead")
	dead.mu.Unlock()

	if err := m.Send(context.Background(), basicMessage()); err != nil {
		t.Fatal(err)
	}

	if got := len(h.snapshot()); got != 2 {
		t.Fatalf("want 2 connections (1 dead + 1 new), got %d", got)
	}
}

func TestSend_PropagatesErrors(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*mockSMTP)
	}{
		{"auth", func(m *mockSMTP) { m.authErr = errors.New("auth failed") }},
		{"mail", func(m *mockSMTP) { m.mailErr = errors.New("mail failed") }},
		{"rcpt", func(m *mockSMTP) { m.rcptErr = errors.New("rcpt failed") }},
		{"data", func(m *mockSMTP) { m.dataErr = errors.New("data failed") }},
		{"write", func(m *mockSMTP) { m.writeErr = errors.New("write failed") }},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h := &testHarness{}
			// Wrap the harness dialer so the freshly dialed mock is
			// pre-configured with the failure we want to simulate.
			dial := func(ctx context.Context) (smtpClient, error) {
				c, err := h.dial(ctx)
				if err != nil {
					return nil, err
				}
				tc.mutate(c.(*mockSMTP))
				return c, nil
			}
			auth := PlainAuth("", "u", "p", "test")
			m, _ := newWithDial(Config{Host: "test", Auth: auth}, dial)
			defer m.Close()

			if err := m.Send(context.Background(), basicMessage()); err == nil {
				t.Fatal("expected error")
			}
		})
	}
}

func TestSendBulk_DeliversToEachRecipient(t *testing.T) {
	h := &testHarness{}
	m, _ := newWithDial(Config{Host: "test", MaxIdle: 10}, h.dial)
	defer m.Close()

	recipients := []string{"a@x.com", "b@x.com", "c@x.com"}
	if err := m.SendBulk(context.Background(), basicMessage(), recipients); err != nil {
		t.Fatal(err)
	}

	totalRcpts := 0
	for _, c := range h.snapshot() {
		c.mu.Lock()
		totalRcpts += len(c.rcpts)
		for _, r := range c.rcpts {
			if !contains(recipients, r) {
				t.Errorf("unexpected rcpt %q", r)
			}
		}
		c.mu.Unlock()
	}
	if totalRcpts != len(recipients) {
		t.Fatalf("want %d sends, got %d", len(recipients), totalRcpts)
	}
}

func TestSendBulk_RunsInParallel(t *testing.T) {
	h := &testHarness{mailDelay: 30 * time.Millisecond}
	m, _ := newWithDial(Config{Host: "test", MaxConcurrency: 6, MaxIdle: 10}, h.dial)
	defer m.Close()

	recipients := make([]string, 12)
	for i := range recipients {
		recipients[i] = fmt.Sprintf("u%d@x.com", i)
	}

	start := time.Now()
	if err := m.SendBulk(context.Background(), basicMessage(), recipients); err != nil {
		t.Fatal(err)
	}
	elapsed := time.Since(start)

	// Sequential: 12 × 30ms = 360ms. Parallel (6x): ~60ms. Allow slack.
	if elapsed > 200*time.Millisecond {
		t.Fatalf("bulk send took %v, looks sequential", elapsed)
	}
}

func TestSendBulk_RespectsMaxConcurrency(t *testing.T) {
	h := &testHarness{mailDelay: 30 * time.Millisecond}
	m, _ := newWithDial(Config{Host: "test", MaxConcurrency: 2, MaxIdle: 10}, h.dial)
	defer m.Close()

	recipients := make([]string, 6)
	for i := range recipients {
		recipients[i] = fmt.Sprintf("u%d@x.com", i)
	}
	if err := m.SendBulk(context.Background(), basicMessage(), recipients); err != nil {
		t.Fatal(err)
	}

	if got := atomic.LoadInt32(&h.maxConcurrent); got > 2 {
		t.Fatalf("max concurrent %d exceeded limit 2", got)
	}
}

func TestSendBulk_ReportsFailures(t *testing.T) {
	h := &testHarness{}
	dial := func(ctx context.Context) (smtpClient, error) {
		c, err := h.dial(ctx)
		if err != nil {
			return nil, err
		}
		c.(*mockSMTP).rcptErr = errors.New("rcpt rejected")
		return c, nil
	}
	m, _ := newWithDial(Config{Host: "test"}, dial)
	defer m.Close()

	err := m.SendBulk(context.Background(), basicMessage(), []string{"a@x.com", "b@x.com"})
	if err == nil {
		t.Fatal("expected joined error")
	}
	if !strings.Contains(err.Error(), "a@x.com") && !strings.Contains(err.Error(), "b@x.com") {
		t.Fatalf("error does not mention recipient: %v", err)
	}
}

func TestSendBulk_EmptyRecipients(t *testing.T) {
	h := &testHarness{}
	m, _ := newWithDial(Config{Host: "test"}, h.dial)
	defer m.Close()
	if err := m.SendBulk(context.Background(), basicMessage(), nil); err != nil {
		t.Fatal(err)
	}
}

func TestSendBulk_CancelledContext(t *testing.T) {
	h := &testHarness{}
	m, _ := newWithDial(Config{Host: "test"}, h.dial)
	defer m.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := m.SendBulk(ctx, basicMessage(), []string{"a@x.com", "b@x.com"})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("want context.Canceled, got %v", err)
	}
}

func TestSend_ClosedMailer(t *testing.T) {
	h := &testHarness{}
	m, _ := newWithDial(Config{Host: "test"}, h.dial)
	m.Close()
	if err := m.Send(context.Background(), basicMessage()); err == nil {
		t.Fatal("expected error on closed mailer")
	}
}

func contains(xs []string, v string) bool {
	for _, x := range xs {
		if x == v {
			return true
		}
	}
	return false
}
