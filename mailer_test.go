// Copyright 2020-2026 NGR Softlab
package mailer

import (
	"context"
	"errors"
	"strings"
	"testing"
)

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
