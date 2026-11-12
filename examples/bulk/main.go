// Package main demonstrates personalized bulk send with bounded concurrency.
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
		// At most 4 SMTP transactions in flight at any time.
		MaxConcurrency: 4,
	})
	if err != nil {
		log.Fatalf("mailer.New: %v", err)
	}
	defer m.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	msg := &mailer.Message{
		From:    os.Getenv("SMTP_FROM"),
		Subject: "Hello from mailer",
		Text:    "This is a bulk message.",
	}
	recipients := []string{
		"alice@example.com",
		"bob@example.com",
		"carol@example.com",
	}

	// SendBulk delivers an individual copy to each recipient; the To header
	// of each copy is set to the corresponding address.
	if err := m.SendBulk(ctx, msg, recipients); err != nil {
		log.Fatalf("send bulk: %v", err)
	}
	log.Printf("bulk send complete: %d recipients", len(recipients))
}
