//go:build fuzz

package fuzzing_tests

import (
	"bytes"
	"net/mail"
	"strings"
	"testing"

	"github.com/NGRsoftlab/mailer"
)

// FuzzMessageRender feeds arbitrary strings into the user-controlled
// fields of mailer.Message.
//
// Invariants checked:
//   - No panic for any input.
//   - If Bytes returns nil error, the rendered output is a well-formed
//     RFC 5322 message parseable by net/mail.
//   - No header other than the ones the library sets itself appears in
//     the parsed output. This guards against header injection via CR/LF
//     in From, To, Subject, Text or HTML.
//
// Assumes the API is:
//
//	func (m *Message) Bytes() ([]byte, error)
//
// If the real signature differs, adjust the call below.
func FuzzMessageRender(f *testing.F) {
	seeds := []struct {
		from, to, subject, text, html string
	}{
		{"sender@example.com", "rcpt@example.com", "hello", "plain body", "<p>html body</p>"},
		{"", "", "", "", ""},
		{"sender@example.com", "rcpt@example.com", "with\r\ninjection", "body", ""},
		{"sender@example.com", "rcpt@example.com", "with\nnewline", "body", ""},
		{"sender@example.com", "rcpt@example.com", "subject", "line1\r\nBcc: victim@example.com", ""},
		{"sender@example.com", "rcpt@example.com", "тема", "тело", ""},
	}
	for _, s := range seeds {
		f.Add(s.from, s.to, s.subject, s.text, s.html)
	}

	f.Fuzz(func(t *testing.T, from, to, subject, text, html string) {
		msg := &mailer.Message{
			From:    from,
			To:      []string{to},
			Subject: subject,
			Text:    text,
			HTML:    html,
		}

		raw, err := msg.Bytes()
		if err != nil {
			// Validation is allowed to reject any input; we only require
			// no panic. The sanity test below guards the happy path.
			return
		}
		if len(raw) == 0 {
			t.Fatal("Bytes returned empty output with nil error")
		}

		parsed, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("rendered output is not a valid RFC 5322 message: %v\n--- output ---\n%s", err, raw)
		}

		// Only headers the library sets itself are allowed. Anything else
		// means user input leaked into the header block.
		allowed := map[string]bool{
			"from": true, "to": true, "subject": true,
			"date": true, "message-id": true,
			"mime-version": true, "content-type": true,
			"content-transfer-encoding": true,
		}
		for name := range parsed.Header {
			if !allowed[strings.ToLower(name)] {
				t.Fatalf(
					"unexpected header %q in rendered output; input likely injected it\n--- output ---\n%s",
					name, raw,
				)
			}
		}
	})
}

// TestMessageBytesSanity protects the fuzzer from passing trivially when
// Bytes starts failing on every input (which would make the early return
// in FuzzMessageRender hide all failures).
func TestMessageBytesSanity(t *testing.T) {
	msg := &mailer.Message{
		From:    "sender@example.com",
		To:      []string{"rcpt@example.com"},
		Subject: "hello",
		Text:    "plain body",
	}
	raw, err := msg.Bytes()
	if err != nil {
		t.Fatalf("Bytes failed on a valid message: %v", err)
	}
	if len(raw) == 0 {
		t.Fatal("Bytes returned empty output for a valid message")
	}
}
