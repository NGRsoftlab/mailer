// Package mailer provides a context-aware SMTP client with connection
// pooling, parallel bulk sending, flexible TLS modes, and pluggable
// authentication.
//
// The entry point is New, which returns a Mailer. Use Send for a
// single-transaction delivery to all envelope recipients, or SendBulk
// for personalized parallel delivery. Close must be called to drain
// the connection pool.
// Copyright 2020-2026 NGR Softlab
package mailer

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"net"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
)

type Config struct {
	Host           string
	Port           int
	Auth           Auth
	TLSMode        TLSMode
	TLSConfig      *tls.Config // supply Certificates for mTLS
	MaxIdle        int         // max pooled connections; default 5
	MaxConcurrency int         // parallel sends in SendBulk; default 4
	Timeout        time.Duration
}

type Mailer struct {
	cfg    Config
	pool   *connPool
	closed atomic.Bool
}

// New validates config and constructs a Mailer with a real SMTP dialer.
func New(cfg Config) (*Mailer, error) {
	if err := normalizeConfig(&cfg); err != nil {
		return nil, err
	}
	addr := net.JoinHostPort(cfg.Host, strconv.Itoa(cfg.Port))
	dial := func(ctx context.Context) (smtpClient, error) {
		return dialContext(ctx, dialConfig{
			addr:      addr,
			host:      cfg.Host,
			tlsConfig: cfg.TLSConfig,
			mode:      cfg.TLSMode,
		})
	}
	return newWithDial(cfg, dial)
}

// newWithDial is the internal constructor used by tests.
func newWithDial(cfg Config, dial dialFunc) (*Mailer, error) {
	if err := normalizeConfig(&cfg); err != nil {
		return nil, err
	}
	return &Mailer{cfg: cfg, pool: newConnPool(cfg.MaxIdle, dial)}, nil
}

// Send delivers msg to all its envelope recipients (To + Cc + Bcc) in one
// SMTP transaction.
func (m *Mailer) Send(ctx context.Context, msg *Message) error {
	if len(msg.envelopeRecipients()) == 0 {
		return errors.New("mailer: message has no recipients")
	}

	return m.send(ctx, msg)
}

// SendBulk delivers one copy of msg to each recipient in the list,
// executing deliveries concurrently up to Config.MaxConcurrency. Each copy
// is personalized: To is set to that recipient, Cc and Bcc are cleared.
// The returned error is a joined error containing per-recipient failures.
func (m *Mailer) SendBulk(ctx context.Context, msg *Message, recipients []string) error {
	if len(recipients) == 0 {
		return nil
	}

	if msg == nil {
		return errors.New("mailer: message is nil")
	}

	concurrency := m.cfg.MaxConcurrency
	if concurrency < 1 {
		concurrency = 1
	}
	sem := make(chan struct{}, concurrency)
	errs := make([]error, len(recipients))
	var wg sync.WaitGroup

	for i, rcpt := range recipients {
		if rcpt == "" {
			errs[i] = errors.New("empty recipient")
			continue
		}

		if ctx.Err() != nil {
			errs[i] = ctx.Err()
			continue
		}

		select {
		case <-ctx.Done():
			errs[i] = ctx.Err()
			continue
		case sem <- struct{}{}:
		}

		wg.Add(1)
		go func(i int, rcpt string) {
			defer wg.Done()
			defer func() { <-sem }()
			if err := m.send(ctx, msg.forRecipient(rcpt)); err != nil {
				errs[i] = fmt.Errorf("send to %s: %w", rcpt, err)
			}
		}(i, rcpt)
	}

	wg.Wait()
	return errors.Join(errs...)
}

// Close drains the pool. Call after all in-flight sends have returned.
func (m *Mailer) Close() {
	if m.closed.Swap(true) {
		return
	}
	m.pool.close()
}

func (m *Mailer) send(ctx context.Context, msg *Message) (err error) {
	if m.closed.Load() {
		return errors.New("mailer: closed")
	}
	if _, ok := ctx.Deadline(); !ok && m.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, m.cfg.Timeout)
		defer cancel()
	}

	c, err := m.pool.get(ctx)
	if err != nil {
		return err
	}
	defer func() {
		if err == nil {
			m.pool.put(c)
		} else {
			_ = c.Close()
		}
	}()

	if m.cfg.Auth != nil && !c.authed {
		if ok, _ := c.Extension("AUTH"); ok {
			if err := c.Auth(m.cfg.Auth); err != nil {
				return fmt.Errorf("auth: %w", err)
			}
			c.authed = true
		}
	}

	if err := c.Mail(msg.From); err != nil {
		return err
	}
	for _, rcpt := range msg.envelopeRecipients() {
		if err := c.Rcpt(rcpt); err != nil {
			return err
		}
	}

	w, err := c.Data()
	if err != nil {
		return err
	}

	body, err := msg.Bytes()
	if err != nil {
		_ = w.Close()
		return err
	}
	if _, err := w.Write(body); err != nil {
		_ = w.Close()
		return err
	}
	return w.Close()
}

func normalizeConfig(cfg *Config) error {
	if cfg.Host == "" {
		return errors.New("mailer: host is required")
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	if cfg.MaxIdle < 1 {
		cfg.MaxIdle = 5
	}
	if cfg.MaxConcurrency < 1 {
		cfg.MaxConcurrency = 4
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	return nil
}
