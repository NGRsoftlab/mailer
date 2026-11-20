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
	TLSConfig      *tls.Config // supply Certificates for mTLS; cloned by New
	MaxIdle        int         // max pooled connections; default 5
	MaxOpen        int         // total open connection cap; 0 = unbounded
	MaxConcurrency int         // parallel sends in SendBulk; default 4
	Timeout        time.Duration
	NoopTimeout    time.Duration // liveness-probe budget; default 5s

	// OnDial, if set, is invoked once per dial attempt with the wall-clock
	// duration and the resulting error (nil on success). It is called
	// synchronously on the caller's goroutine; keep it cheap.
	OnDial func(duration time.Duration, err error)

	// OnSend, if set, is invoked once per Send/SendBulk delivery (one copy
	// per recipient in SendBulk) with the total wall-clock duration and
	// the resulting error (nil on success). Called synchronously; keep it
	// cheap.
	OnSend func(duration time.Duration, err error)
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

// newWithDial is the internal constructor used by tests. It is the sole
// place where Config.OnDial is wired into the pool's dial function:
// wrapping in New would make every test constructor silently bypass the
// hook. New() and newWithDial() must produce a Mailer whose hooks fire
// identically; that contract is what lets unit tests assert on Config
// behavior without a real socket.
func newWithDial(cfg Config, dial dialFunc) (*Mailer, error) {
	if err := normalizeConfig(&cfg); err != nil {
		return nil, err
	}
	if cfg.OnDial != nil {
		inner := dial
		hook := cfg.OnDial
		dial = func(ctx context.Context) (smtpClient, error) {
			start := time.Now()
			c, err := inner(ctx)
			hook(time.Since(start), err)
			return c, err
		}
	}
	return &Mailer{
		cfg:  cfg,
		pool: newConnPool(cfg.MaxIdle, cfg.MaxOpen, cfg.NoopTimeout, dial),
	}, nil
}

// Send delivers msg to all its envelope recipients (To + Cc + Bcc) in one
// SMTP transaction.
func (m *Mailer) Send(ctx context.Context, msg *Message) error {
	if msg == nil {
		return errors.New("mailer: message is nil")
	}
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
			if err := ctx.Err(); err != nil {
				errs[i] = fmt.Errorf("send to %s: %w", rcpt, err)
				return
			}
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
	if m.cfg.OnSend != nil {
		start := time.Now()
		defer func() { m.cfg.OnSend(time.Since(start), err) }()
	}
	if m.closed.Load() {
		return errors.New("mailer: closed")
	}
	if msg == nil {
		return errors.New("mailer: message is nil")
	}
	if _, ok := ctx.Deadline(); !ok && m.cfg.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, m.cfg.Timeout)
		defer cancel()
	}
	if err := ctx.Err(); err != nil {
		return err
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

	err = m.runDialogue(c, msg)
	if err != nil && ctx.Err() != nil {
		return ctx.Err()
	}
	return err
}

// runDialogue performs the SMTP transaction on c.
func (m *Mailer) runDialogue(c *poolConn, msg *Message) error {
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
		c.abort()
		return err
	}
	if _, err := w.Write(body); err != nil {
		c.abort()
		return err
	}
	return w.Close()
}

func normalizeConfig(cfg *Config) error {
	if cfg.Host == "" {
		return errors.New("mailer: host is required")
	}
	switch cfg.TLSMode {
	case TLSNone, TLSStart, TLSImplicit:
		// valid
	default:
		return fmt.Errorf("mailer: unknown TLSMode %d", cfg.TLSMode)
	}
	if cfg.Port == 0 {
		cfg.Port = 587
	}
	if cfg.MaxIdle < 1 {
		cfg.MaxIdle = 5
	}
	if cfg.MaxOpen < 0 {
		return fmt.Errorf("mailer: MaxOpen must be non-negative, got %d", cfg.MaxOpen)
	}
	if cfg.MaxOpen > 0 && cfg.MaxIdle > cfg.MaxOpen {
		cfg.MaxIdle = cfg.MaxOpen
	}
	if cfg.MaxConcurrency < 1 {
		cfg.MaxConcurrency = 4
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 30 * time.Second
	}
	if cfg.NoopTimeout == 0 {
		cfg.NoopTimeout = defaultNoopTimeout
	}
	if cfg.TLSConfig == nil {
		cfg.TLSConfig = &tls.Config{ServerName: cfg.Host}
	} else {
		cfg.TLSConfig = cfg.TLSConfig.Clone()
		if cfg.TLSConfig.ServerName == "" {
			cfg.TLSConfig.ServerName = cfg.Host
		}
	}
	return nil
}
