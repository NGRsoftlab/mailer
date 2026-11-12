// Command mtls demonstrates sending with mutual TLS (client certificates).
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"log"
	"os"

	"github.com/NGRsoftlab/mailer"
)

func main() {
	cert, err := tls.LoadX509KeyPair(
		mustEnv("TLS_CERT"),
		mustEnv("TLS_KEY"),
	)
	if err != nil {
		log.Fatalf("load key pair: %v", err)
	}

	caPEM, err := os.ReadFile(mustEnv("TLS_CA"))
	if err != nil {
		log.Fatalf("read CA: %v", err)
	}
	pool := x509.NewCertPool()
	if !pool.AppendCertsFromPEM(caPEM) {
		log.Fatal("no certificates parsed from TLS_CA")
	}

	client, err := mailer.New(mailer.Config{
		Host: mustEnv("SMTP_HOST"),
		Port: 465,
		TLS: &tls.Config{
			Certificates: []tls.Certificate{cert},
			RootCAs:      pool,
			ServerName:   mustEnv("SMTP_HOST"),
		},
	})
	if err != nil {
		log.Fatalf("new client: %v", err)
	}
	defer client.Close()

	msg := mailer.NewMessage().
		From(mustEnv("SMTP_FROM")).
		To(mustEnv("SMTP_TO")).
		Subject("mTLS demo").
		Text("Sent with a client certificate.")

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
