// Command simple demonstrates sending a plain-text email over STARTTLS.
package main

import (
	"context"
	"log"
	"os"
	"strconv"

	"github.com/NGRsoftlab/mailer"
)

func main() {
	port, err := strconv.Atoi(envOr("SMTP_PORT", "587"))
	if err != nil {
		log.Fatalf("invalid SMTP_PORT: %v", err)
	}

	client, err := mailer.New(mailer.Config{
		Host:     mustEnv("SMTP_HOST"),
		Port:     port,
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
		Subject("Hello from NGRsoftlab/mailer").
		Text("This message was sent by examples/simple.")

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
