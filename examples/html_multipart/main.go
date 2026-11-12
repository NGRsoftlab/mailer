// Command html_multipart demonstrates text + HTML alternative parts.
package main

import (
	"context"
	"log"
	"os"

	"github.com/NGRsoftlab/mailer"
)

func main() {
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
		Subject("HTML + plain text").
		Text("Fallback for clients without HTML.").
		HTML(`<h1>Hello</h1><p>This is the <b>HTML</b> part.</p>`)

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
