// Command attachments demonstrates sending an in-memory file attachment.
package main

import (
	"context"
	"log"
	"os"

	"github.com/NGRsoftlab/mailer"
)

func main() {
	path := envOr("ATTACHMENT_PATH", "./README.md")
	data, err := os.ReadFile(path)
	if err != nil {
		log.Fatalf("read %s: %v", path, err)
	}

	client, err := mailer.New(mailer.Config{
		Host:     mustEnv("SMTP_HOST"),
		Port:     587,
		Username: mustEnv("SMTP_USER"),
		Password: mustEnv("SMTP_PASS"),
	})
	if err != nil {
		log.Fatalf("new client: %v", err)
	}
	defer client.Close()

	msg := mailer.NewMessage().
		From(mustEnv("SMTP_FROM")).
		To(mustEnv("SMTP_TO")).
		Subject("Attachment demo").
		Text("See the attached file.").
		Attach("README.md", "text/markdown", data)

	if err := client.Send(context.Background(), msg); err != nil {
		log.Fatalf("send: %v", err)
	}
	log.Println("sent OK")
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("env %s is required", key)
	}
	return v
}

func envOr(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
