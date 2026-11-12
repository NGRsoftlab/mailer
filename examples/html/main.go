// Package main sends an HTML body with a plain-text fallback.
package main

import (
	"context"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/NGRsoftlab/mailer"
)

func main() {
	port, _ := strconv.Atoi(os.Getenv("SMTP_PORT"))
	if port == 0 {
		port = 587
	}

	m, err := mailer.New(mailer.Config{
		Host: os.Getenv("SMTP_HOST"),
		Port: port,
	})
	if err != nil {
		log.Fatalf("mailer.New: %v", err)
	}
	defer m.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msg := &mailer.Message{
		From:    os.Getenv("SMTP_FROM"),
		To:      []string{"recipient@example.com"},
		Subject: "Hello from mailer",
		// Both bodies are provided; the library builds multipart/alternative.
		Text: "Hello, this is the plain-text fallback.",
		HTML: `<h1>Hello</h1><p>This is the <strong>HTML</strong> version.</p>`,
	}

	if err := m.Send(ctx, msg); err != nil {
		log.Fatalf("send: %v", err)
	}
	log.Println("message sent")
}
