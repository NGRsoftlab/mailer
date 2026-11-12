// Package main sends a message with a single attachment.
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

	// In a real application load the bytes from disk, e.g. os.ReadFile.
	reportData := []byte("Monthly report\nrevenue: 42\n")

	msg := &mailer.Message{
		From:    os.Getenv("SMTP_FROM"),
		To:      []string{"recipient@example.com"},
		Subject: "Report attached",
		Text:    "Please find the report attached.",
		Attachments: []mailer.Attachment{
			{
				Name:        "report.txt",
				ContentType: "text/plain",
				Data:        reportData,
			},
		},
	}

	if err := m.Send(ctx, msg); err != nil {
		log.Fatalf("send: %v", err)
	}
	log.Println("message sent")
}
