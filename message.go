// Copyright 2020-2026 NGR Softlab
package mailer

import (
	"bytes"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"mime"
	"mime/multipart"
	"net/textproto"
	"path/filepath"
	"strings"
	"time"
)

// Attachment describes a single file attached to a message.
type Attachment struct {
	Name        string
	Data        []byte
	ContentType string // auto-detected from extension if empty
}

// Message is an SMTP message with optional HTML alternative and attachments.
type Message struct {
	From        string
	To          []string
	Cc          []string
	Bcc         []string
	Subject     string
	Text        string
	HTML        string
	Attachments []Attachment
	Headers     map[string]string
	Date        time.Time // zero → time.Now() at render time
	MessageID   string    // empty → generated at render time
}

// Bytes renders the message as an RFC 5322 MIME document. A nil receiver
// returns an error instead of panicking, so callers can treat Bytes as a
// total function.
func (m *Message) Bytes() ([]byte, error) {
	if m == nil {
		return nil, errors.New("mailer: message is nil")
	}
	var buf bytes.Buffer
	writeCommonHeaders(&buf, m)

	hasAttachments := len(m.Attachments) > 0
	hasAlternative := m.Text != "" && m.HTML != ""

	switch {
	case hasAttachments:
		if err := writeMultipartMixed(&buf, m, hasAlternative); err != nil {
			return nil, err
		}
	case hasAlternative:
		if err := writeMultipartAlternative(&buf, m); err != nil {
			return nil, err
		}
	case m.HTML != "":
		writeBase64Part(&buf, "text/html; charset=UTF-8", []byte(m.HTML))
	case m.Text != "":
		writeBase64Part(&buf, "text/plain; charset=UTF-8", []byte(m.Text))
	default:
		// Empty body but structurally valid message.
		fmt.Fprintf(&buf, "Content-Type: text/plain; charset=UTF-8\r\n")
		fmt.Fprintf(&buf, "Content-Transfer-Encoding: base64\r\n\r\n")
	}
	return buf.Bytes(), nil
}

// envelopeRecipients returns recipients for the SMTP envelope (To + Cc + Bcc).
// A nil receiver yields nil, so callers can safely check for "no recipients".
func (m *Message) envelopeRecipients() []string {
	if m == nil {
		return nil
	}
	r := make([]string, 0, len(m.To)+len(m.Cc)+len(m.Bcc))
	r = append(r, m.To...)
	r = append(r, m.Cc...)
	r = append(r, m.Bcc...)
	return r
}

// forRecipient returns a shallow copy of the message addressed to a single
// recipient. Cc and Bcc are cleared so personalization does not leak.
// MessageID is cleared only when it was not set by the caller: an empty
// MessageID means "let Bytes generate one", which yields a unique ID per
// personalized copy. A caller-supplied MessageID is preserved verbatim —
// the caller is responsible for keeping it unique across copies.
// A nil receiver yields nil; callers are expected to have validated msg.
func (m *Message) forRecipient(rcpt string) *Message {
	if m == nil {
		return nil
	}
	clone := *m
	clone.To = []string{rcpt}
	clone.Cc = nil
	clone.Bcc = nil
	if m.MessageID == "" {
		// Already empty — Bytes will generate a fresh one per copy.
		clone.MessageID = ""
	}
	return &clone
}

func writeCommonHeaders(buf *bytes.Buffer, m *Message) {
	fmt.Fprintf(buf, "From: %s\r\n", sanitizeHeaderValue(m.From))

	if len(m.To) > 0 {
		fmt.Fprintf(buf, "To: %s\r\n", sanitizeAddrList(m.To))
	}

	if len(m.Cc) > 0 {
		fmt.Fprintf(buf, "Cc: %s\r\n", sanitizeAddrList(m.Cc))
	}

	fmt.Fprintf(buf, "Subject: %s\r\n", encodeHeader(m.Subject))

	// Date: RFC 5322 REQUIRES a Date header. A zero field means "now".
	date := m.Date
	if date.IsZero() {
		date = time.Now()
	}
	fmt.Fprintf(buf, "Date: %s\r\n", date.Format(time.RFC1123Z))

	// Message-ID: RFC 5322 RECOMMENDS a Message-ID. Generate one when the
	// caller left it empty; otherwise wrap/sanitize what they provided.
	fmt.Fprintf(buf, "Message-ID: %s\r\n", buildMessageID(m))

	fmt.Fprintf(buf, "MIME-Version: 1.0\r\n")

	reserved := map[string]bool{
		"from": true, "to": true, "cc": true, "bcc": true,
		"subject": true, "mime-version": true,
		"date": true, "message-id": true,
		"content-type": true, "content-transfer-encoding": true,
	}

	for k, v := range m.Headers {
		lk := strings.ToLower(strings.TrimSpace(k))
		if lk == "" || reserved[lk] || strings.ContainsAny(k, ":\r\n") {
			continue
		}
		fmt.Fprintf(buf, "%s: %s\r\n", k, sanitizeHeaderValue(v))
	}
}

// buildMessageID returns a complete "<id@domain>" token, either derived
// from m.MessageID (sanitized, possibly wrapped) or freshly generated.
// All user input is filtered to printable ASCII minus characters that
// would break out of the header value.
func buildMessageID(m *Message) string {
	raw := strings.TrimSpace(m.MessageID)
	raw = strings.Trim(raw, "<>")
	raw = sanitizeMessageIDToken(raw)

	// A user-provided ID may or may not contain "@". If it does, both
	// halves must be non-empty; otherwise regenerate.
	if at := strings.LastIndex(raw, "@"); at >= 0 {
		if at == 0 || at == len(raw)-1 {
			raw = ""
		}
	}

	if raw == "" {
		b := make([]byte, 16)
		_, _ = rand.Read(b)
		raw = hex.EncodeToString(b)
	}
	if !strings.Contains(raw, "@") {
		raw = raw + "@" + messageIDDomain(m.From)
	}
	return "<" + raw + ">"
}

// messageIDDomain derives a domain for a generated Message-ID from the
// From address. Falls back to "mailer" when From is empty, lacks an "@",
// or contains only characters that cannot appear in an addr-spec.
func messageIDDomain(from string) string {
	clean := sanitizeMessageIDToken(from)
	if at := strings.LastIndex(clean, "@"); at >= 0 && at < len(clean)-1 {
		if d := clean[at+1:]; d != "" {
			return d
		}
	}
	return "mailer"
}

// sanitizeMessageIDToken strips everything outside printable ASCII,
// removing characters that would terminate the header line or change its
// structure. "@" is preserved so callers can supply a complete addr-spec.
func sanitizeMessageIDToken(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if r < 0x21 || r > 0x7e {
			continue
		}
		switch r {
		case '<', '>', '(', ')', ',', ';', ':', '\\', '"', ' ', '\t':
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}

func writeMultipartMixed(buf *bytes.Buffer, m *Message, hasAlternative bool) error {
	boundary := generateBoundary()
	fmt.Fprintf(buf, "Content-Type: multipart/mixed; boundary=%q\r\n\r\n", boundary)

	w := multipart.NewWriter(buf)
	if err := w.SetBoundary(boundary); err != nil {
		return err
	}

	if hasAlternative {
		altBoundary := generateBoundary()
		h := textproto.MIMEHeader{}
		h.Set("Content-Type", fmt.Sprintf("multipart/alternative; boundary=%q", altBoundary))
		part, err := w.CreatePart(h)
		if err != nil {
			return err
		}
		altW := multipart.NewWriter(part)
		if err := altW.SetBoundary(altBoundary); err != nil {
			return err
		}
		if err := writeTextPart(altW, "text/plain; charset=UTF-8", m.Text); err != nil {
			return err
		}
		if err := writeTextPart(altW, "text/html; charset=UTF-8", m.HTML); err != nil {
			return err
		}
		if err := altW.Close(); err != nil {
			return err
		}
	} else {
		switch {
		case m.HTML != "":
			if err := writeTextPart(w, "text/html; charset=UTF-8", m.HTML); err != nil {
				return err
			}
		case m.Text != "":
			if err := writeTextPart(w, "text/plain; charset=UTF-8", m.Text); err != nil {
				return err
			}
		}
	}

	for _, a := range m.Attachments {
		if err := writeAttachment(w, a); err != nil {
			return err
		}
	}
	return w.Close()
}

func writeMultipartAlternative(buf *bytes.Buffer, m *Message) error {
	boundary := generateBoundary()
	fmt.Fprintf(buf, "Content-Type: multipart/alternative; boundary=%q\r\n\r\n", boundary)

	w := multipart.NewWriter(buf)
	if err := w.SetBoundary(boundary); err != nil {
		return err
	}
	if err := writeTextPart(w, "text/plain; charset=UTF-8", m.Text); err != nil {
		return err
	}
	if err := writeTextPart(w, "text/html; charset=UTF-8", m.HTML); err != nil {
		return err
	}
	return w.Close()
}

func writeTextPart(w *multipart.Writer, contentType, body string) error {
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", contentType)
	h.Set("Content-Transfer-Encoding", "base64")
	p, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	return writeBase64(p, []byte(body))
}

func writeAttachment(w *multipart.Writer, a Attachment) error {
	ct := a.ContentType
	if ct == "" {
		ct = mime.TypeByExtension(filepath.Ext(a.Name))
		if ct == "" {
			ct = "application/octet-stream"
		}
	}
	h := textproto.MIMEHeader{}
	h.Set("Content-Type", ct)
	h.Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", encodeHeader(a.Name)))
	h.Set("Content-Transfer-Encoding", "base64")
	p, err := w.CreatePart(h)
	if err != nil {
		return err
	}
	return writeBase64(p, a.Data)
}

func writeBase64Part(buf *bytes.Buffer, contentType string, data []byte) {
	fmt.Fprintf(buf, "Content-Type: %s\r\n", contentType)
	fmt.Fprintf(buf, "Content-Transfer-Encoding: base64\r\n\r\n")
	_ = writeBase64(buf, data)
}

func writeBase64(w io.Writer, data []byte) error {
	enc := base64.StdEncoding.EncodeToString(data)
	for len(enc) > 0 {
		n := 76
		if len(enc) < n {
			n = len(enc)
		}
		if _, err := fmt.Fprintf(w, "%s\r\n", enc[:n]); err != nil {
			return err
		}
		enc = enc[n:]
	}
	return nil
}

func generateBoundary() string {
	b := make([]byte, 16)
	_, _ = rand.Read(b)
	return "b-" + hex.EncodeToString(b)
}

func encodeHeader(s string) string {
	return mime.QEncoding.Encode("UTF-8", sanitizeHeaderValue(s))
}

func sanitizeHeaderValue(s string) string {
	return strings.Map(func(r rune) rune {
		if r == '\t' {
			return r // HTAB is allowed in headers
		}
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

func sanitizeAddrList(addrs []string) string {
	cleaned := make([]string, len(addrs))
	for i, a := range addrs {
		cleaned[i] = sanitizeHeaderValue(a)
	}
	return strings.Join(cleaned, ", ")
}
