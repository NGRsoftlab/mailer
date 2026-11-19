// Copyright 2020-2026 NGR Softlab
package mailer

import (
	"context"
	"crypto/tls"
	"errors"
	"io"
	"strings"
	"sync/atomic"
	"testing"
)

// spyWriteCloser is a DATA body writer that records whether Close was
// called. Used to verify that a failing body write does not send the
// SMTP terminator (which would commit a partial message).
type spyWriteCloser struct {
	writeErr    error
	closeCalled atomic.Bool
}

func (s *spyWriteCloser) Write(p []byte) (int, error) {
	if s.writeErr != nil {
		return 0, s.writeErr
	}
	return len(p), nil
}

func (s *spyWriteCloser) Close() error {
	s.closeCalled.Store(true)
	return nil
}

// dataErrorClient is a fakeClient whose Data returns a spyWriteCloser,
// so tests can inject a failing body write without a real network.
type dataErrorClient struct {
	*fakeClient
	wc *spyWriteCloser
}

func (c *dataErrorClient) Data() (io.WriteCloser, error) {
	return c.wc, nil
}

// noDialFunc is a dial stub that fails the test if invoked. Used to
// prove that argument validation happens before any network activity.
func noDialFunc(t *testing.T) dialFunc {
	t.Helper()
	return func(ctx context.Context) (smtpClient, error) {
		t.Fatal("dial must not be called for an invalid call")
		return nil, nil
	}
}

// TestSendNilMessage verifies that Send does not panic on a nil message
// and returns an error before touching the pool or the network.
func TestSendNilMessage(t *testing.T) {
	m, err := newWithDial(Config{Host: "example.com"}, noDialFunc(t))
	if err != nil {
		t.Fatalf("newWithDial: %v", err)
	}
	defer m.Close()

	err = m.Send(context.Background(), nil)
	if err == nil {
		t.Fatal("expected error for nil message")
	}
	if !strings.Contains(err.Error(), "message is nil") {
		t.Errorf("error = %q, want it to mention nil message", err)
	}
}

// TestSendBulkNilMessage pins the existing contract for SendBulk: nil
// message is rejected before any dial.
func TestSendBulkNilMessage(t *testing.T) {
	m, err := newWithDial(Config{Host: "example.com"}, noDialFunc(t))
	if err != nil {
		t.Fatalf("newWithDial: %v", err)
	}
	defer m.Close()

	err = m.SendBulk(context.Background(), nil, []string{"a@example.com"})
	if err == nil {
		t.Fatal("expected error for nil message")
	}
	if !strings.Contains(err.Error(), "message is nil") {
		t.Errorf("error = %q, want it to mention nil message", err)
	}
}

// TestMessageBytesNilReceiver verifies that Bytes handles a nil receiver
// by returning an error instead of panicking.
func TestMessageBytesNilReceiver(t *testing.T) {
	var msg *Message
	raw, err := msg.Bytes()
	if err == nil {
		t.Fatalf("expected error, got raw=%q", raw)
	}
	if raw != nil {
		t.Errorf("raw = %v, want nil", raw)
	}
}

// TestMessageEnvelopeRecipientsNilReceiver verifies the helper is safe
// on a nil receiver; it is used internally by Send and runDialogue.
func TestMessageEnvelopeRecipientsNilReceiver(t *testing.T) {
	var msg *Message
	if got := msg.envelopeRecipients(); got != nil {
		t.Fatalf("envelopeRecipients = %v, want nil", got)
	}
}

// TestMessageForRecipientNilReceiver verifies the helper is safe on a
// nil receiver.
func TestMessageForRecipientNilReceiver(t *testing.T) {
	var msg *Message
	if got := msg.forRecipient("x@example.com"); got != nil {
		t.Fatalf("forRecipient = %v, want nil", got)
	}
}

// TestSendBulkCancelledContextNoDial verifies that SendBulk with an
// already-cancelled context:
//   - never touches the network (dialFunc would t.Fatal),
//   - returns one sub-error per recipient, each matching
//     errors.Is(err, context.Canceled).
//
// This pins the contract of the ctx.Err() checks inside SendBulk. The
// worker-level check closes the race window between the main-loop check
// and worker start; it cannot be deterministically forced in a test, but
// the already-cancelled path covers the same code branch when the select
// chooses ctx.Done().
func TestSendBulkCancelledContextNoDial(t *testing.T) {
	m, err := newWithDial(Config{Host: "example.com"}, noDialFunc(t))
	if err != nil {
		t.Fatalf("newWithDial: %v", err)
	}
	defer m.Close()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	recipients := []string{"a@example.com", "b@example.com", "c@example.com"}
	msg := &Message{From: "sender@example.com", Subject: "x", Text: "y"}

	err = m.SendBulk(ctx, msg, recipients)
	if err == nil {
		t.Fatal("expected error from cancelled context")
	}

	unwrapper, ok := err.(interface{ Unwrap() []error })
	if !ok {
		t.Fatalf("err = %v (%T), want joined error", err, err)
	}
	subs := unwrapper.Unwrap()
	if len(subs) != len(recipients) {
		t.Fatalf("got %d sub-errors, want %d", len(subs), len(recipients))
	}
	for i, e := range subs {
		if !errors.Is(e, context.Canceled) {
			t.Errorf("sub[%d] = %v, want it to wrap context.Canceled", i, e)
		}
	}
}

// TestNewFillsServerNameWhenNil verifies that when the caller passes no
// TLSConfig, New creates one with ServerName populated from Host. Without
// it, crypto/tls would silently skip hostname verification.
func TestNewFillsServerNameWhenNil(t *testing.T) {
	m, err := newWithDial(Config{Host: "smtp.example.com"}, noDialFunc(t))
	if err != nil {
		t.Fatalf("newWithDial: %v", err)
	}
	defer m.Close()

	if m.cfg.TLSConfig == nil {
		t.Fatal("TLSConfig = nil, want non-nil")
	}
	if got := m.cfg.TLSConfig.ServerName; got != "smtp.example.com" {
		t.Fatalf("ServerName = %q, want smtp.example.com", got)
	}
}

// TestNewFillsServerNameWhenEmpty verifies that when the caller passes a
// TLSConfig without ServerName, New populates it from Host. Otherwise
// crypto/tls would accept any certificate signed by the supplied roots,
// regardless of the hostname being dialed.
func TestNewFillsServerNameWhenEmpty(t *testing.T) {
	m, err := newWithDial(Config{
		Host:      "smtp.example.com",
		TLSConfig: &tls.Config{RootCAs: nil},
	}, noDialFunc(t))
	if err != nil {
		t.Fatalf("newWithDial: %v", err)
	}
	defer m.Close()

	if got := m.cfg.TLSConfig.ServerName; got != "smtp.example.com" {
		t.Fatalf("ServerName = %q, want smtp.example.com", got)
	}
}

// TestNewPreservesExplicitServerName verifies that an explicitly set
// ServerName is not overwritten. This supports the case where the server
// certificate is issued for a name other than the dialed host (e.g. an
// internal alias or IP endpoint behind a load balancer).
func TestNewPreservesExplicitServerName(t *testing.T) {
	m, err := newWithDial(Config{
		Host:      "127.0.0.1",
		TLSConfig: &tls.Config{ServerName: "smtp.internal.example.com"},
	}, noDialFunc(t))
	if err != nil {
		t.Fatalf("newWithDial: %v", err)
	}
	defer m.Close()

	if got := m.cfg.TLSConfig.ServerName; got != "smtp.internal.example.com" {
		t.Fatalf("ServerName = %q, want smtp.internal.example.com", got)
	}
}

// TestNewClonesTLSConfig verifies that mutating the caller's TLSConfig
// after New does not affect the Mailer. Without the clone, a caller's
// later assignment to ServerName (or InsecureSkipVerify) would silently
// change the mailer's security posture.
func TestNewClonesTLSConfig(t *testing.T) {
	original := &tls.Config{ServerName: "smtp.example.com"}
	m, err := newWithDial(Config{Host: "smtp.example.com", TLSConfig: original}, noDialFunc(t))
	if err != nil {
		t.Fatalf("newWithDial: %v", err)
	}
	defer m.Close()

	if m.cfg.TLSConfig == original {
		t.Fatal("Mailer retains caller's *tls.Config pointer; want clone")
	}

	// Mutate the original. The mailer must not observe the change.
	original.ServerName = "attacker.example.com"
	original.InsecureSkipVerify = true

	if got := m.cfg.TLSConfig.ServerName; got != "smtp.example.com" {
		t.Errorf("ServerName = %q, want smtp.example.com (mutation leaked)", got)
	}
	if m.cfg.TLSConfig.InsecureSkipVerify {
		t.Error("InsecureSkipVerify = true, want false (mutation leaked)")
	}
}

// TestSendDataWriteErrorDoesNotCommit verifies that when the DATA body
// write fails, the data writer is NOT closed. Closing it would send the
// SMTP terminator ".\r\n", committing a partial or empty message while
// Send returns an error — the worst of both worlds. The connection is
// aborted instead (via poolConn.abort in runDialogue), so the server
// discards the buffered data per RFC 5321 §4.1.1.4.
func TestSendDataWriteErrorDoesNotCommit(t *testing.T) {
	writeErr := errors.New("simulated write failure")
	spy := &spyWriteCloser{writeErr: writeErr}
	client := &dataErrorClient{
		fakeClient: &fakeClient{conn: &fakeConn{}},
		wc:         spy,
	}

	m, err := newWithDial(Config{Host: "example.com"}, func(ctx context.Context) (smtpClient, error) {
		return client, nil
	})
	if err != nil {
		t.Fatalf("newWithDial: %v", err)
	}
	defer m.Close()

	msg := &Message{
		From:    "sender@example.com",
		To:      []string{"rcpt@example.com"},
		Subject: "test",
		Text:    "body",
	}
	err = m.Send(context.Background(), msg)
	if err == nil {
		t.Fatal("expected error from failing Write")
	}
	if !errors.Is(err, writeErr) {
		t.Errorf("err = %v, want it to wrap %v", err, writeErr)
	}
	if spy.closeCalled.Load() {
		t.Fatal("WriteCloser.Close was called on error; that would commit a partial message")
	}
}
