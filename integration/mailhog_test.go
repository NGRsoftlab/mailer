//go:build integration

package integration_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/NGRsoftlab/mailer"
)

// smtpConfig builds a mailer.Config from environment variables.
// Skips the test if SMTP_HOST is not set.
func smtpConfig(t *testing.T) mailer.Config {
	t.Helper()
	host := os.Getenv("SMTP_HOST")
	if host == "" {
		t.Skip("SMTP_HOST is not set; skipping integration test")
	}
	port, err := strconv.Atoi(envOr("SMTP_PORT", "1025"))
	if err != nil {
		t.Fatalf("invalid SMTP_PORT: %v", err)
	}
	return mailer.Config{Host: host, Port: port}
}

// envOr returns the value of key or def if key is empty.
func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// mailhogMessage is the subset of MailHog's JSON we care about.
type mailhogMessage struct {
	Content struct {
		Headers map[string][]string `json:"Headers"`
		Body    string              `json:"Body"`
	} `json:"Content"`
}

// mailhogMessages returns all messages collected by MailHog.
// MailHog returns newest first.
func mailhogMessages(t *testing.T) []mailhogMessage {
	t.Helper()
	url := envOr("SMTP_API_URL", "http://localhost:8025") + "/api/v2/messages"
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("fetch mailhog messages: %v", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		t.Fatalf("mailhog API returned %d", resp.StatusCode)
	}

	var payload struct {
		Items []mailhogMessage `json:"items"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatalf("decode mailhog messages: %v", err)
	}
	return payload.Items
}

// header returns the first value of the given header (case-insensitive).
func header(m mailhogMessage, key string) string {
	for k, vs := range m.Content.Headers {
		if strings.EqualFold(k, key) && len(vs) > 0 {
			return vs[0]
		}
	}
	return ""
}

// waitForMessage polls MailHog until at least one message with the given
// subject is present, or the timeout expires.
func waitForMessage(t *testing.T, subject string, timeout time.Duration) mailhogMessage {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		for _, m := range mailhogMessages(t) {
			if header(m, "Subject") == subject {
				return m
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("no message with subject %q within %s", subject, timeout)
	return mailhogMessage{}
}

func TestSendSimpleMessage(t *testing.T) {
	cfg := smtpConfig(t)
	client, err := mailer.New(cfg)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer client.Close()

	subject := "integration: simple"
	msg := mailer.NewMessage().
		From("from@test.local").
		To("to@test.local").
		Subject(subject).
		Text("hello from integration test")

	if err := client.Send(context.Background(), msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	got := waitForMessage(t, subject, 5*time.Second)
	if !strings.Contains(got.Content.Body, "hello from integration test") {
		t.Fatalf("unexpected body: %q", got.Content.Body)
	}
}

func TestSendHTMLMultipart(t *testing.T) {
	cfg := smtpConfig(t)
	client, err := mailer.New(cfg)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer client.Close()

	subject := "integration: html"
	msg := mailer.NewMessage().
		From("from@test.local").
		To("to@test.local").
		Subject(subject).
		Text("plain fallback").
		HTML("<h1>HTML part</h1>")

	if err := client.Send(context.Background(), msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	got := waitForMessage(t, subject, 5*time.Second)
	ct := header(got, "Content-Type")
	if !strings.HasPrefix(strings.ToLower(ct), "multipart/") {
		t.Fatalf("expected multipart Content-Type, got %q", ct)
	}
}

func TestSendAttachment(t *testing.T) {
	cfg := smtpConfig(t)
	client, err := mailer.New(cfg)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer client.Close()

	subject := "integration: attachment"
	msg := mailer.NewMessage().
		From("from@test.local").
		To("to@test.local").
		Subject(subject).
		Text("see attached").
		Attach("hello.txt", "text/plain", []byte("attached content"))

	if err := client.Send(context.Background(), msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	got := waitForMessage(t, subject, 5*time.Second)
	if !strings.Contains(got.Content.Body, "hello.txt") {
		t.Fatalf("attachment filename not found in body")
	}
}

func TestSendStreamingAttachment(t *testing.T) {
	cfg := smtpConfig(t)
	client, err := mailer.New(cfg)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer client.Close()

	subject := "integration: streaming attachment"
	payload := strings.Repeat("streaming-data-", 1024) // ~15 KiB
	msg := mailer.NewMessage().
		From("from@test.local").
		To("to@test.local").
		Subject(subject).
		Text("see attached").
		AttachReader("data.bin", "application/octet-stream", strings.NewReader(payload))

	if err := client.Send(context.Background(), msg); err != nil {
		t.Fatalf("send: %v", err)
	}

	got := waitForMessage(t, subject, 5*time.Second)
	if !strings.Contains(got.Content.Body, "data.bin") {
		t.Fatalf("attachment filename not found in body")
	}
}

func TestSendInvalidRecipient(t *testing.T) {
	cfg := smtpConfig(t)
	client, err := mailer.New(cfg)
	if err != nil {
		t.Fatalf("new client: %v", err)
	}
	defer client.Close()

	msg := mailer.NewMessage().
		From("not-an-address").
		To("also-not-an-address").
		Subject("integration: invalid").
		Text("this should fail")

	// MailHog accepts anything, so this may pass; the point is it must not panic.
	_ = client.Send(context.Background(), msg)
}

var _ = fmt.Sprintf
