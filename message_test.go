// Copyright 2020-2026 NGR Softlab
package mailer

import (
	"bytes"
	"net/mail"
	"strings"
	"testing"
	"time"
)

// parseMessage is a helper used by the tests below to assert on rendered
// headers with net/mail's canonical view.
func parseMessage(t *testing.T, raw []byte) *mail.Message {
	t.Helper()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse: %v\n--- raw ---\n%s", err, raw)
	}
	return msg
}

// TestMessageBytesHasDateAndMessageID verifies that Bytes emits both a
// Date and a Message-ID header even when neither was supplied by the
// caller. Without them, RFC 5322 messages are malformed and receiving
// clients cannot deduplicate.
func TestMessageBytesHasDateAndMessageID(t *testing.T) {
	msg := &Message{
		From:    "sender@example.com",
		To:      []string{"rcpt@example.com"},
		Subject: "hello",
		Text:    "body",
	}
	raw, err := msg.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	parsed := parseMessage(t, raw)

	if parsed.Header.Get("Date") == "" {
		t.Error("Date header is missing")
	}
	mid := parsed.Header.Get("Message-Id")
	if mid == "" {
		t.Fatal("Message-Id header is missing")
	}
	if !strings.HasPrefix(mid, "<") || !strings.HasSuffix(mid, ">") {
		t.Errorf("Message-Id = %q, want <...>", mid)
	}
	if !strings.Contains(mid, "@") {
		t.Errorf("Message-Id = %q, want it to contain @", mid)
	}
}

// TestMessageBytesRespectsUserDate verifies that a caller-supplied Date
// is rendered verbatim in RFC 5322 form.
func TestMessageBytesRespectsUserDate(t *testing.T) {
	fixed := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	msg := &Message{
		From:    "sender@example.com",
		To:      []string{"rcpt@example.com"},
		Subject: "hello",
		Text:    "body",
		Date:    fixed,
	}
	raw, err := msg.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	parsed := parseMessage(t, raw)
	want := "Thu, 02 Jan 2020 03:04:05 +0000"
	if got := parsed.Header.Get("Date"); got != want {
		t.Errorf("Date = %q, want %q", got, want)
	}
}

// TestMessageBytesRespectsUserMessageID verifies that a caller-supplied
// MessageID is preserved. Brackets are added if the caller omitted them.
func TestMessageBytesRespectsUserMessageID(t *testing.T) {
	msg := &Message{
		From:      "sender@example.com",
		To:        []string{"rcpt@example.com"},
		Subject:   "hello",
		Text:      "body",
		MessageID: "custom-id@example.org",
	}
	raw, err := msg.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	parsed := parseMessage(t, raw)
	if got := parsed.Header.Get("Message-Id"); got != "<custom-id@example.org>" {
		t.Errorf("Message-Id = %q, want <custom-id@example.org>", got)
	}
}

// TestMessageBytesGeneratesUniqueMessageID verifies that two independent
// renders produce different Message-IDs. Without uniqueness, receiving
// clients may deduplicate or thread unrelated messages.
func TestMessageBytesGeneratesUniqueMessageID(t *testing.T) {
	msg := &Message{
		From:    "sender@example.com",
		To:      []string{"rcpt@example.com"},
		Subject: "hello",
		Text:    "body",
	}
	raw1, err := msg.Bytes()
	if err != nil {
		t.Fatalf("Bytes #1: %v", err)
	}
	raw2, err := msg.Bytes()
	if err != nil {
		t.Fatalf("Bytes #2: %v", err)
	}
	id1 := parseMessage(t, raw1).Header.Get("Message-Id")
	id2 := parseMessage(t, raw2).Header.Get("Message-Id")
	if id1 == "" || id2 == "" {
		t.Fatalf("Message-Id missing: %q / %q", id1, id2)
	}
	if id1 == id2 {
		t.Errorf("Message-Id collided across renders: %q", id1)
	}
}

// TestMessageBytesSanitizesMessageID verifies that CRLF and other
// illegal characters in a caller-supplied MessageID cannot break out of
// the header and inject new headers.
func TestMessageBytesSanitizesMessageID(t *testing.T) {
	msg := &Message{
		From:      "sender@example.com",
		To:        []string{"rcpt@example.com"},
		Subject:   "hello",
		Text:      "body",
		MessageID: "abc\r\nInjected: yes",
	}
	raw, err := msg.Bytes()
	if err != nil {
		t.Fatalf("Bytes: %v", err)
	}
	parsed := parseMessage(t, raw)
	if got := parsed.Header.Get("Injected"); got != "" {
		t.Errorf("Injected header leaked into output: %q", got)
	}
	mid := parsed.Header.Get("Message-Id")
	if !strings.HasPrefix(mid, "<") || !strings.HasSuffix(mid, ">") {
		t.Errorf("Message-Id = %q, want well-formed <...>", mid)
	}
	// The sanitized ID must not contain the injected prefix's CR/LF.
	if strings.ContainsAny(mid, "\r\n") {
		t.Errorf("Message-Id contains CR/LF: %q", mid)
	}
}
