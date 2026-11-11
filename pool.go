// Copyright 2020-2026 NGR Softlab
package mailer

import (
	"context"
	"errors"
	"io"
	"net/smtp"
	"sync"
)

// smtpClient is the subset of *smtp.Client used by the mailer. It exists
// so tests can substitute a mock without a network connection.
type smtpClient interface {
	Noop() error
	Auth(smtp.Auth) error
	Extension(string) (bool, string)
	Mail(string) error
	Rcpt(string) error
	Data() (io.WriteCloser, error)
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
func (p *connPool) get(ctx context.Context) (*poolConn, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, errors.New("pool: closed")
	}
	for len(p.idle) > 0 {
		c := p.idle[len(p.idle)-1]
		p.idle = p.idle[:len(p.idle)-1]
		p.mu.Unlock()

		if err := c.Noop(); err == nil {
			return c, nil
		}
		_ = c.Close()

		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errors.New("pool: closed")
		}
	}
	p.mu.Unlock()

	raw, err := p.dial(ctx)
	if err != nil {
		return nil, err
	}
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
