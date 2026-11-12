// Package main demonstrates the three TLS modes.
//
// Select the mode with TLS_MODE=none|starttls|implicit.
package main

import (
	"context"
	"crypto/tls"
	"log"
	"os"
	"strconv"
	"time"

	"github.com/NGRsoftlab/mailer"
)

func main() {
	port, _ := strconv.Atoi(os.Getenv("SMTP_PORT"))
	if port == 0 {
		port = 465
	}

	host := os.Getenv("SMTP_HOST")

	cfg := mailer.Config{
		Host: host,
		Port: port,
	}

	switch mode := os.Getenv("TLS_MODE"); mode {
	case "", "none":
		// Opportunistic STARTTLS: upgrade if the server offers it.
		// Nothing to set — this is the default behavior.
	case "starttls":
		// Mandatory STARTTLS: fail if the server does not offer it.
		cfg.TLSMode = mailer.TLSStart
		cfg.TLSConfig = &tls.Config{ServerName: host}
	case "implicit":
		// Implicit TLS (SMTPS): TLS handshake starts before any SMTP traffic.
		cfg.TLSMode = mailer.TLSImplicit
		cfg.TLSConfig = &tls.Config{ServerName: host}
	default:
		log.Fatalf("unknown TLS_MODE %q", mode)
	}

	m, err := mailer.New(cfg)
	if err != nil {
		log.Fatalf("mailer.New: %v", err)
	}
	defer m.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	msg := &mailer.Message{
		From:    os.Getenv("SMTP_FROM"),
		To:      []string{"recipient@example.com"},
		Subject: "TLS example",
		Text:    "Sent with the configured TLS mode.",
	}

	if err := m.Send(ctx, msg); err != nil {
		log.Fatalf("send: %v", err)
	}
	log.Println("message sent")
}
