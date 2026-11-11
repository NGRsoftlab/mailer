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

func FuzzMessageBytes(f *testing.F) {
	f.Add("from@x.com", "subject", "text body", "<p>html</p>", "file.txt", []byte("data"))
	f.Add("", "", "", "", "", []byte{})
	f.Add("a@b.c", strings.Repeat("x", 500), "\r\n.", "\x00\x01", "..hidden", []byte{0, 1, 2})
	f.Add("a@b.c", "subj\nInjected: yes", "b", "", "a\r\nb", []byte("x"))

	f.Fuzz(func(t *testing.T, from, subject, text, html, attName string, attData []byte) {
		msg := &Message{
			From:    from,
			To:      []string{"to@example.com"},
			Subject: subject,
			Text:    text,
			HTML:    html,
		}
		if attName != "" || len(attData) > 0 {
			msg.Attachments = []Attachment{{Name: attName, Data: attData}}
		}

		raw, err := msg.Bytes()
		if err != nil {
			// Some structural failures are legitimate; skip.
			return
		}

		parsed, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil {
			t.Fatalf("invalid MIME structure: %v", err)
		}

		// Sanity: no header injection.
		for k := range parsed.Header {
			if k == "" || strings.ContainsAny(k, " :\r\n") {
				t.Fatalf("malformed header name %q", k)
			}
		}

		// If multipart, ensure the body reads cleanly.
		if ct := parsed.Header.Get("Content-Type"); strings.HasPrefix(ct, "multipart/") {
			mt, params, err := mime.ParseMediaType(ct)
			if err != nil {
				t.Fatalf("bad Content-Type: %v", err)
			}
			if !strings.HasPrefix(mt, "multipart/") {
				return
			}
			r := multipart.NewReader(parsed.Body, params["boundary"])
			for {
				if _, err := r.NextPart(); err == io.EOF {
					break
				} else if err != nil {
					t.Fatalf("multipart parse: %v", err)
				}
			}
		}
	})
}
