// Package main demonstrates the three built-in auth mechanisms.
//
// Select the mechanism with AUTH_MECH=plain|login|xoauth2.
package main

import (
	"context"
	"log"
	"net/smtp"
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

	host := os.Getenv("SMTP_HOST")
	user := os.Getenv("SMTP_USER")
	pass := os.Getenv("SMTP_PASS")

	cfg := mailer.Config{
		Host: host,
		Port: port,
	}

	switch mech := os.Getenv("AUTH_MECH"); mech {
	case "", "plain":
		// PLAIN is provided by the standard library.
		cfg.Auth = smtp.PlainAuth("", user, pass, host)
	case "login":
		// LOGIN is provided by the mailer package.
		cfg.Auth = mailer.LoginAuth(user, pass)
	case "xoauth2":
		// XOAUTH2 uses an OAuth2 access token instead of a password.
		cfg.Auth = mailer.XOAuth2Auth(user, os.Getenv("SMTP_OAUTH_TOKEN"))
	default:
		log.Fatalf("unknown AUTH_MECH %q", mech)
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
		Subject: "Authenticated send",
		Text:    "Sent with the configured auth mechanism.",
	}

	if err := m.Send(ctx, msg); err != nil {
		log.Fatalf("send: %v", err)
	}
	log.Println("message sent")
}
