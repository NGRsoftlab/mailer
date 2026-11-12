// Command streaming_attachment demonstrates AttachReader + WriteTo:
// a large file is streamed through the SMTP DATA phase without being
// buffered in memory.
package main

import (
	"context"
	"log"
	"os"

	"github.com/NGRsoftlab/mailer"
)

func main() {
	path := mustEnv("ATTACHMENT_PATH")
	f, err := os.Open(path)
	if err != nil {
		log.Fatalf("open %s: %v", path, err)
	}
	defer f.Close()

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
		Subject("Streaming attachment demo").
		Text("The attachment is streamed from disk.").
		AttachReader("big.bin", "application/octet-stream", f)

	// Send uses WriteTo internally: the file is copied through base64
	// directly to the SMTP socket. Do not reuse this Message afterwards.
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
