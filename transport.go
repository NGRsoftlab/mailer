// Copyright 2020-2026 NGR Softlab
package mailer

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
	"sync"
	"time"
)

type TLSMode int

const (
	TLSNone     TLSMode = iota // plain SMTP, opportunistically STARTTLS
	TLSStart                   // require STARTTLS
	TLSImplicit                // implicit TLS (SMTPS, e.g. port 465)
)

type dialConfig struct {
	addr      string
	host      string
	tlsConfig *tls.Config
	mode      TLSMode
}

// ctxWatcherImpl closes the underlying TCP socket when the context is
// cancelled. It is deliberately built on the raw TCP conn, below any TLS
// layer, so a single Close interrupts whichever Read/Write is currently
// blocked — the initial 220 greeting read, a TLS handshake, an AUTH
// exchange, or a DATA body write.
//
// bind and release are idempotent. The watcher goroutine is created per
// bound context and lives only until either ctx.Done fires or release is
// called; that keeps the pool free of long-lived goroutines while an
// idle connection waits in the pool.
type ctxWatcherImpl struct {
	conn net.Conn

	mu   sync.Mutex
	stop chan struct{}
}

func (w *ctxWatcherImpl) bind(ctx context.Context) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stop != nil {
		close(w.stop)
		w.stop = nil
	}
	if ctx.Done() == nil {
		// Background / TODO: nothing to watch.
		return
	}
	stop := make(chan struct{})
	w.stop = stop
	conn := w.conn
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-stop:
		}
	}()
}

func (w *ctxWatcherImpl) release() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.stop != nil {
		close(w.stop)
		w.stop = nil
	}
}

// smtpConn couples *smtp.Client with the net.Conn underneath it so a
// per-request deadline can be re-applied on every checkout of a pooled
// connection, and so a cancelled context can tear the socket down
// regardless of which SMTP operation is currently blocked.
//
// SetDeadline targets the final conn (which for implicit TLS is the
// tls.Conn). tls.Conn propagates deadlines to its inner net.Conn, so the
// underlying socket is still the one being governed.
//
// BindContext / ReleaseContext are optional from the pool's point of
// view — the pool discovers them via type assertion so that mocks
// without context support keep working.
type smtpConn struct {
	*smtp.Client
	conn    net.Conn
	watcher *ctxWatcherImpl
}

func (c *smtpConn) SetDeadline(t time.Time) error {
	return c.conn.SetDeadline(t)
}

// BindContext (re)installs a watcher for ctx. If the previous binding
// existed, it is cancelled first; this is the expected flow when a
// pooled connection is handed to a new caller.
func (c *smtpConn) BindContext(ctx context.Context) {
	c.watcher.bind(ctx)
}

// ReleaseContext stops the watcher without closing the socket. Called
// when the connection is returned to the pool or discarded.
func (c *smtpConn) ReleaseContext() {
	c.watcher.release()
}

// dialContext establishes an SMTP connection honoring TLS mode, mTLS
// certificates (via tlsConfig.Certificates), and context cancellation.
func dialContext(ctx context.Context, cfg dialConfig) (smtpClient, error) {
	if cfg.tlsConfig == nil {
		cfg.tlsConfig = &tls.Config{ServerName: cfg.host}
	}

	d := &net.Dialer{}
	rawConn, err := d.DialContext(ctx, "tcp", cfg.addr)
	if err != nil {
		return nil, err
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = rawConn.SetDeadline(deadline)
	}

	// Install the watcher on the raw TCP conn before any SMTP/TLS reads
	// so a cancel during the greeting read or handshake also interrupts.
	watcher := &ctxWatcherImpl{conn: rawConn}
	watcher.bind(ctx)
	success := false
	defer func() {
		if !success {
			watcher.release()
		}
	}()

	conn := rawConn
	if cfg.mode == TLSImplicit {
		// tls.DialWithDialer dials without a context and blocks the
		// handshake until the deadline, ignoring context cancellation.
		// Wrapping a DialContext result with HandshakeContext honors both.
		tlsConn := tls.Client(rawConn, cfg.tlsConfig)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = rawConn.Close()
			return nil, fmt.Errorf("tls handshake: %w", err)
		}
		conn = tlsConn
	}

	client, err := smtp.NewClient(conn, cfg.host)
	if err != nil {
		_ = rawConn.Close()
		return nil, err
	}

	if cfg.mode != TLSImplicit {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(cfg.tlsConfig); err != nil {
				_ = client.Close()
				return nil, fmt.Errorf("starttls: %w", err)
			}
		} else if cfg.mode == TLSStart {
			_ = client.Close()
			return nil, fmt.Errorf("server does not support STARTTLS")
		}
	}

	success = true
	return &smtpConn{Client: client, conn: conn, watcher: watcher}, nil
}
