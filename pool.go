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

// errPoolClosed is returned when a get is attempted on a closed pool, or
// when the pool was closed while a connection was being prepared. Shared
// as a package-level value so callers can match it with errors.Is.
var errPoolClosed = errors.New("pool: closed")

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

type dialFunc func(context.Context) (smtpClient, error)

type connPool struct {
	mu      sync.Mutex
	idle    []*poolConn
	dial    dialFunc
	maxIdle int
	closed  bool
}

func newConnPool(maxIdle int, dial dialFunc) *connPool {
	return &connPool{maxIdle: maxIdle, dial: dial}
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

		// Refresh the I/O deadline from the current context. A pooled
		// connection would otherwise keep the deadline applied by a
		// previous send, which is either already expired (forcing an
		// unnecessary reconnect) or wrong for the current request.
		// A zero time means "no deadline" and clears any previous one.
		deadline, _ := ctx.Deadline()
		if err := c.SetDeadline(deadline); err != nil {
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
	// Close may have run while dial was in flight. Same reasoning as for
	// the Noop branch: never return a connection from a closed pool.
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		_ = raw.Close()
		return nil, errPoolClosed
	}
	p.mu.Unlock()
	return &poolConn{smtpClient: raw}, nil
}

func (p *connPool) put(c *poolConn) {
	if c == nil {
		return
	}
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
