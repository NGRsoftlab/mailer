// Copyright 2020-2026 NGR Softlab
package mailer

import (
	"bytes"
	"io"
	"mime"
	"mime/multipart"
	"net/mail"
	"strings"
	"testing"
)

func TestMessage_PlainText(t *testing.T) {
	m := &Message{From: "a@x.com", To: []string{"b@x.com"}, Subject: "Hi", Text: "hello"}
	raw, err := m.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if ct := parsed.Header.Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("want text/plain, got %q", ct)
	}
	body, _ := io.ReadAll(parsed.Body)
	if !strings.Contains(string(body), "aGVsbG8=") { // base64("hello")
		t.Fatalf("body missing base64 content: %q", body)
	}
}

func TestMessage_Alternative(t *testing.T) {
	m := &Message{From: "a@x.com", To: []string{"b@x.com"}, Subject: "Hi",
		Text: "hello", HTML: "<p>hello</p>"}
	raw, _ := m.Bytes()

	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	mt, params, err := mime.ParseMediaType(parsed.Header.Get("Content-Type"))
	if err != nil || mt != "multipart/alternative" {
		t.Fatalf("want multipart/alternative, got %q (%v)", mt, err)
	}
	r := multipart.NewReader(parsed.Body, params["boundary"])
	n := 0
	for {
		_, err := r.NextPart()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		n++
	}
	if n != 2 {
		t.Fatalf("want 2 parts, got %d", n)
	}
}

func TestMessage_Attachment(t *testing.T) {
	m := &Message{From: "a@x.com", To: []string{"b@x.com"}, Subject: "Hi", Text: "hello",
		Attachments: []Attachment{{Name: "report.txt", Data: []byte("data")}}}
	raw, _ := m.Bytes()

	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	if mt, _, _ := mime.ParseMediaType(parsed.Header.Get("Content-Type")); mt != "multipart/mixed" {
		t.Fatalf("want multipart/mixed, got %q", mt)
	}
}

func TestMessage_HeaderInjectionPrevented(t *testing.T) {
	m := &Message{
		From:    "a@x.com\r\nBcc: evil@x.com",
		To:      []string{"b@x.com\r\nX-Injected: 1"},
		Subject: "Hi\r\nBcc: evil@x.com",
		Text:    "hi",
	}
	raw, _ := m.Bytes()
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		t.Fatal(err)
	}
	if msg.Header.Get("Bcc") != "" {
		t.Fatal("header injection: Bcc header present")
	}
	if msg.Header.Get("X-Injected") != "" {
		t.Fatal("header injection: X-Injected header present")
	}
}

func TestMessage_NonASCII(t *testing.T) {
	m := &Message{From: "a@x.com", To: []string{"b@x.com"},
		Subject: "Привет мир", Text: "тело",
		Attachments: []Attachment{{Name: "файл.txt", Data: []byte("x")}}}
	raw, err := m.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mail.ReadMessage(bytes.NewReader(raw)); err != nil {
		t.Fatalf("non-ASCII message must parse: %v", err)
	}
}

func TestMessage_EmptyBody(t *testing.T) {
	m := &Message{From: "a@x.com", To: []string{"b@x.com"}}
	raw, err := m.Bytes()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := mail.ReadMessage(bytes.NewReader(raw)); err != nil {
		t.Fatalf("empty body must parse: %v", err)
	}
}
