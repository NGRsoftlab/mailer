// Copyright 2020-2026 NGR Softlab
package mailer

import (
	"net/smtp"
	"strings"
	"testing"
)

// TestLoginAuthCaseInsensitive verifies that the LOGIN mechanism accepts
// challenges regardless of letter case. Real servers (e.g. Exchange) do
// not all use the "Username:" / "Password:" capitalisation.
func TestLoginAuthCaseInsensitive(t *testing.T) {
	a := LoginAuth("alice", "s3cr3t").(*loginAuth)

	cases := []struct {
		challenge string
		want      string
	}{
		{"Username:", "alice"},
		{"username:", "alice"},
		{"USERNAME:", "alice"},
		{"Password:", "s3cr3t"},
		{"password:", "s3cr3t"},
		{"  Username:  ", "alice"},
	}
	for _, c := range cases {
		got, err := a.Next([]byte(c.challenge), true)
		if err != nil {
			t.Errorf("Next(%q) returned error: %v", c.challenge, err)
			continue
		}
		if string(got) != c.want {
			t.Errorf("Next(%q) = %q, want %q", c.challenge, got, c.want)
		}
	}
}

// TestLoginAuthUnknownChallenge verifies that an unexpected challenge is
// surfaced as an error rather than silently answered with credentials.
func TestLoginAuthUnknownChallenge(t *testing.T) {
	a := LoginAuth("alice", "s3cr3t").(*loginAuth)
	_, err := a.Next([]byte("SomethingElse:"), true)
	if err == nil {
		t.Fatal("expected error for unexpected challenge")
	}
	if !strings.Contains(err.Error(), "unexpected challenge") {
		t.Errorf("error = %q, want it to mention unexpected challenge", err)
	}
}

// TestLoginAuthNoMore verifies that a terminal continuation (more=false)
// returns nil, nil — the dialogue is over.
func TestLoginAuthNoMore(t *testing.T) {
	a := LoginAuth("alice", "s3cr3t").(*loginAuth)
	got, err := a.Next([]byte("irrelevant"), false)
	if err != nil {
		t.Fatalf("Next(more=false) error: %v", err)
	}
	if got != nil {
		t.Errorf("Next(more=false) = %v, want nil", got)
	}
}

// TestXOAuth2ContinuationSendsAck verifies that on a server rejection
// (more=true) the mechanism answers with an empty \x01 ACK. Without that
// ACK the connection stays half-open and the caller gets a generic I/O
// error instead of the server's actual authentication failure.
func TestXOAuth2ContinuationSendsAck(t *testing.T) {
	a := XOAuth2Auth("user@example.com", "token").(*xoauth2Auth)
	// Simulate the server replying with a base64-encoded error body.
	// The bytes themselves don't matter; only the "more" flag does.
	got, err := a.Next([]byte(`{"status":"401"}`), true)
	if err != nil {
		t.Fatalf("Next(more=true) error: %v", err)
	}
	if string(got) != "\x01" {
		t.Errorf("Next(more=true) = %q, want \\x01", got)
	}
}

// TestXOAuth2NoMore verifies that a clean end-of-dialogue (more=false)
// produces no data.
func TestXOAuth2NoMore(t *testing.T) {
	a := XOAuth2Auth("user@example.com", "token").(*xoauth2Auth)
	got, err := a.Next(nil, false)
	if err != nil {
		t.Fatalf("Next(more=false) error: %v", err)
	}
	if got != nil {
		t.Errorf("Next(more=false) = %v, want nil", got)
	}
}

// Compile-time sanity: the returned Auth values really do implement
// smtp.Auth, which the public type alias already guarantees but we pin
// it here so a future refactor cannot silently swap the return type.
var _ smtp.Auth = (*loginAuth)(nil)
var _ smtp.Auth = (*xoauth2Auth)(nil)
