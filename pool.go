// Copyright 2020-2026 NGR Softlab
package mailer

import (
	"context"
	"errors"
	"io"
	"net/smtp"
	"sync"
	"time"
)

// defaultNoopTimeout is the liveness-probe budget applied when the
// caller does not override it via Config.NoopTimeout. It must be much
// shorter than a typical Send budget, otherwise a hung peer could tie
// up an entire send on a dead connection.
const defaultNoopTimeout = 5 * time.Second

// errPoolClosed is returned when a get is attempted on a closed pool, or
// when the pool was closed while a connection was being prepared. Shared
// as a package-level value so callers can match it with errors.Is.
var errPoolClosed = errors.New("pool: closed")

// ctxBinder is implemented by connection types that support binding to
// a context. Plain smtpClient implementations (mocks) do not have to
// implement it: the pool checks with a type assertion.
type ctxBinder interface {
	BindContext(ctx context.Context)
	ReleaseContext()
}

// smtpClient is the subset of *smtp.Client used by the mailer. It exists
// so tests can substitute a mock without a network connection.
//
// SetDeadline is required so the pool can refresh the socket deadline
// each time a connection is checked out; otherwise a deadline applied
// during a previous Send would silently govern the current one.
type smtpClient interface {
	Noop() error
	Auth(smtp.Auth) error
	Extension(string) (bool, string)
	Mail(string) error
	Rcpt(string) error
	Data() (io.WriteCloser, error)
	SetDeadline(time.Time) error
	Close() error
}

// poolConn tracks per-connection state that outlives one send.
type poolConn struct {
	smtpClient
	authed bool
}

// bind associates the connection with ctx so a later cancellation of
// that ctx closes the socket and unblocks any in-flight Read/Write.
// No-op for clients that do not implement ctxBinder (test mocks).
func (pc *poolConn) bind(ctx context.Context) {
	if b, ok := pc.smtpClient.(ctxBinder); ok {
		b.BindContext(ctx)
	}
}

// release stops any context watcher previously installed via bind.
// No-op for clients that do not implement ctxBinder.
func (pc *poolConn) release() {
	if b, ok := pc.smtpClient.(ctxBinder); ok {
		b.ReleaseContext()
	}
}

// Close releases the context watcher and closes the underlying client.
// Overriding Close is essential: a watcher left running against a stale
// (or long-lived) context would leak a goroutine and, worse, kill the
// socket after the connection had been returned to the pool.
func (pc *poolConn) Close() error {
	pc.release()
	return pc.smtpClient.Close()
}

type dialFunc func(context.Context) (smtpClient, error)

type connPool struct {
	mu          sync.Mutex
	idle        []*poolConn
	dial        dialFunc
	maxIdle     int
	noopTimeout time.Duration
	closed      bool
}

func newConnPool(maxIdle int, noopTimeout time.Duration, dial dialFunc) *connPool {
	if noopTimeout <= 0 {
		noopTimeout = defaultNoopTimeout
	}
	return &connPool{maxIdle: maxIdle, noopTimeout: noopTimeout, dial: dial}
}

// get returns a live connection from the pool or dials a new one.
// Dead connections are discarded and replaced.
//
// Every branch respects ctx: an already-cancelled context must not be
// served from the idle list, since that would silently turn a cancelled
// send into a successful one.
//
// Every branch also re-checks p.closed under p.mu immediately before a
// connection is returned to the caller. Close may run while the pool is
// probing an idle connection (Noop) or dialing a new one; without the
// re-check, get would hand out a connection from a closed pool, and a
// subsequent Write would deliver a message after Close was called.
func (p *connPool) get(ctx context.Context) (*poolConn, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errPoolClosed
	}
	for len(p.idle) > 0 {
		if err := ctx.Err(); err != nil {
			p.mu.Unlock()
			return nil, err
		}
		c := p.idle[len(p.idle)-1]
		p.idle = p.idle[:len(p.idle)-1]
		p.mu.Unlock()

		// Compute the probe deadline: the caller's deadline, capped by
		// noopTimeout. Noop is not context-aware, so without an explicit
		// cap a slow peer could consume the entire send budget before we
		// even start the SMTP dialogue. When the caller supplied no
		// deadline at all, use now+noopTimeout as the sole bound.
		callerDeadline, hasDeadline := ctx.Deadline()
		probeDeadline := time.Now().Add(p.noopTimeout)
		if hasDeadline && callerDeadline.Before(probeDeadline) {
			probeDeadline = callerDeadline
		}
		if err := c.SetDeadline(probeDeadline); err != nil {
			_ = c.Close()
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				return nil, errPoolClosed
			}
			continue
		}

		if err := c.Noop(); err == nil {
			// Noop is not context-aware, so re-check before handing the
			// connection back to the caller: it may have taken long enough
			// for the context to become done. The connection is discarded
			// because a liveness probe that raced with cancellation leaves
			// its state unknown.
			if err := ctx.Err(); err != nil {
				_ = c.Close()
				return nil, err
			}
			// Close may have been called while Noop was running. Re-check
			// under the lock and refuse to hand out a connection from a
			// closed pool.
			p.mu.Lock()
			if p.closed {
				p.mu.Unlock()
				_ = c.Close()
				return nil, errPoolClosed
			}
			p.mu.Unlock()
			// Restore the caller's deadline so the SMTP dialogue that
			// follows runs under its own budget, not the short probe one.
			// A zero callerDeadline (no ctx deadline) clears the socket
			// deadline, matching the caller's intent.
			if err := c.SetDeadline(callerDeadline); err != nil {
				_ = c.Close()
				return nil, err
			}
			// Attach the caller's context so a later cancellation tears
			// the socket down mid-dialogue. Done after all checks so the
			// watcher never fires on a connection still being validated.
			c.bind(ctx)
			return c, nil
		}
		_ = c.Close()

		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errPoolClosed
		}
	}
	p.mu.Unlock()

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	raw, err := p.dial(ctx)
	if err != nil {
		return nil, err
	}
	pc := &poolConn{smtpClient: raw}
	// Close may have run while dial was in flight. Same reasoning as for
	// the Noop branch: never return a connection from a closed pool.
	// Use poolConn so the context watcher installed by dialContext is
	// released before the socket is closed.
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = pc.Close()
		return nil, errPoolClosed
	}
	p.mu.Unlock()
	return pc, nil
}

func (p *connPool) put(c *poolConn) {
	if c == nil {
		return
	}
	// Detach the caller's context before parking the connection. If the
	// watcher were still bound, a later cancel of that ctx would close
	// the socket while it sits idle in the pool.
	c.release()
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed || len(p.idle) >= p.maxIdle {
		_ = c.Close()
		return
	}
	p.idle = append(p.idle, c)
}

func (p *connPool) close() {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed = true
	for _, c := range p.idle {
		_ = c.Close()
	}
	p.idle = nil
}
