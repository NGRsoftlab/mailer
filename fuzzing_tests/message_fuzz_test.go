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

// FuzzMessageHeadersAndAttachment feeds arbitrary strings into
// Message.Headers (as a single key/value pair) and Attachment.Name.
//
// Invariants:
//   - No panic for any input.
//   - If Bytes returns nil error, the output parses as RFC 5322.
//   - No header outside the library's own set appears in the parsed
//     top-level headers, except possibly the exact key the fuzzer
//     supplied (which is how a legitimate custom header manifests).
//   - The raw bytes contain no "\r\nBcc:" or "\r\nCc:" sequence, which
//     would mean a header or attachment name escaped sanitization.
func FuzzMessageHeadersAndAttachment(f *testing.F) {
	seeds := []struct {
		hk, hv, an string
	}{
		{"Reply-To", "support@example.com", "report.pdf"},
		{"X-Custom", "value", "файл.pdf"},
		{"X-Ok", "value with spaces", "data.bin"},
		{"Injected\r\nBcc", "x", "a.txt"},
		{"X-Ok", "value\r\nBcc: victim@example.com", "a.txt"},
		{"X-Ok", "value", "a\r\ntxt.txt"},
		{"X-Ok", "value", "a\x00.txt"},
		{"", "value", "a.txt"},
	}
	for _, s := range seeds {
		f.Add(s.hk, s.hv, s.an)
	}

	f.Fuzz(func(t *testing.T, hk, hv, an string) {
		msg := &mailer.Message{
			From:    "sender@example.com",
			To:      []string{"rcpt@example.com"},
			Subject: "test",
			Text:    "body",
			Headers: map[string]string{hk: hv},
			Attachments: []mailer.Attachment{
				{Name: an, Data: []byte("x")},
			},
		}

		raw, err := msg.Bytes()
		if err != nil {
			return
		}

		parsed, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("parse: %v\n--- raw ---\n%s", err, raw)
		}

		allowed := map[string]bool{
			"from": true, "to": true, "subject": true,
			"date": true, "message-id": true,
			"mime-version": true, "content-type": true,
			"content-transfer-encoding": true,
		}
		// A legitimate custom header is echoed back only if the caller's
		// key was already a valid RFC 5322 field-name. Anything else is
		// dropped, so the only non-standard header we may see is the
		// exact key the fuzzer supplied (lowercased).
		hkLower := strings.ToLower(hk)
		for name := range parsed.Header {
			lk := strings.ToLower(name)
			if allowed[lk] {
				continue
			}
			// A non-standard header is acceptable only if it is exactly
			// the key the fuzzer asked for; anything else means the input
			// injected a new header name.
			if lk != hkLower {
				t.Fatalf("unexpected header %q\n--- raw ---\n%s", name, raw)
			}
		}

		if bytes.Contains(raw, []byte("\r\nBcc:")) {
			t.Fatalf("CRLF+Bcc leaked\n--- raw ---\n%s", raw)
		}
		if bytes.Contains(raw, []byte("\r\nCc:")) {
			t.Fatalf("CRLF+Cc leaked\n--- raw ---\n%s", raw)
		}
	})
}
