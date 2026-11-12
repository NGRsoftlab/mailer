// Command pool demonstrates concurrent sends through a single client,
// relying on the library's connection pool.
package main

import (
	"context"
	"fmt"
	"log"
	"os"
	"sync"

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

	from, to := mustEnv("SMTP_FROM"), mustEnv("SMTP_TO")
	const workers = 16

	var wg sync.WaitGroup
	errs := make(chan error, workers)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			msg := mailer.NewMessage().
				From(from).
				To(to).
				Subject(fmt.Sprintf("pool message #%d", i)).
				Text(fmt.Sprintf("body #%d", i))
			if err := client.Send(context.Background(), msg); err != nil {
				errs <- fmt.Errorf("worker %d: %w", i, err)
			}
		}(i)
	}
	wg.Wait()
	close(errs)

	var failed int
	for err := range errs {
		log.Printf("send failed: %v", err)
		failed++
	}
	if failed > 0 {
		log.Fatalf("%d of %d sends failed", failed, workers)
	}
	log.Printf("sent %d messages OK", workers)
}

func mustEnv(key string) string {
	v := os.Getenv(key)
	if v == "" {
		log.Fatalf("env %s is required", key)
	}
	return v
}
