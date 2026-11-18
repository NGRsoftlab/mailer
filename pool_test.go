// Copyright 2020-2026 NGR Softlab
package mailer

import (
	"context"
	"errors"
	"io"
	"net"
	"net/smtp"
	"testing"
	"time"
)

// fakeAddr is a placeholder net.Addr used by fakeConn.
type fakeAddr struct{}

func (fakeAddr) Network() string { return "fake" }
func (fakeAddr) String() string  { return "fake" }

// fakeConn captures the last deadline applied to it so the test can
// assert the pool refreshes it on each checkout.
type fakeConn struct {
	deadline time.Time
	closed   bool
}

func (c *fakeConn) Read(_ []byte) (int, error)  { return 0, io.EOF }
func (c *fakeConn) Write(p []byte) (int, error) { return len(p), nil }

func (c *fakeConn) Close() error {
	c.closed = true
	return nil
}

func (c *fakeConn) LocalAddr() net.Addr  { return fakeAddr{} }
func (c *fakeConn) RemoteAddr() net.Addr { return fakeAddr{} }

func (c *fakeConn) SetDeadline(t time.Time) error {
	c.deadline = t
	return nil
}

func (c *fakeConn) SetReadDeadline(_ time.Time) error  { return nil }
func (c *fakeConn) SetWriteDeadline(_ time.Time) error { return nil }

// fakeClient implements smtpClient; Noop always succeeds.
type fakeClient struct {
	conn net.Conn
}

func (f *fakeClient) Noop() error                       { return nil }
func (f *fakeClient) Auth(_ smtp.Auth) error            { return nil }
func (f *fakeClient) Extension(_ string) (bool, string) { return false, "" }
func (f *fakeClient) Mail(_ string) error               { return nil }
func (f *fakeClient) Rcpt(_ string) error               { return nil }
func (f *fakeClient) Data() (io.WriteCloser, error)     { return nopWriteCloser{}, nil }
func (f *fakeClient) SetDeadline(t time.Time) error     { return f.conn.SetDeadline(t) }
func (f *fakeClient) Close() error                      { return f.conn.Close() }

// callbackClient lets a test run arbitrary code at the beginning of Noop.
type callbackClient struct {
	*fakeClient
	onNoop func()
}

func (c *callbackClient) Noop() error {
	if c.onNoop != nil {
		c.onNoop()
	}
	return nil
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// TestPoolRefreshesDeadlineOnReuse verifies that a pooled connection
// receives a fresh deadline derived from the current context each time
// it is checked out, rather than carrying over the deadline applied
// during the previous send. Without the fix, the deadline from the
// first ctx persists after the connection is put back and reused.
func TestPoolRefreshesDeadlineOnReuse(t *testing.T) {
	var fc *fakeConn

	p := newConnPool(1, func(ctx context.Context) (smtpClient, error) {
		fc = &fakeConn{}
		// Mimic what dialContext does: apply the caller's deadline on
		// the freshly created connection.
		if deadline, ok := ctx.Deadline(); ok {
			_ = fc.SetDeadline(deadline)
		}
		return &fakeClient{conn: fc}, nil
	})

	d1 := time.Now().Add(10 * time.Second)
	ctx1, cancel1 := context.WithDeadline(context.Background(), d1)
	defer cancel1()

	c1, err := p.get(ctx1)
	if err != nil {
		t.Fatalf("get #1: %v", err)
	}
	if got := fc.deadline; !got.Equal(d1) {
		t.Fatalf("deadline after get #1 = %v, want %v", got, d1)
	}
	p.put(c1)

	d2 := time.Now().Add(20 * time.Second)
	ctx2, cancel2 := context.WithDeadline(context.Background(), d2)
	defer cancel2()

	c2, err := p.get(ctx2)
	if err != nil {
		t.Fatalf("get #2: %v", err)
	}
	if got := fc.deadline; !got.Equal(d2) {
		t.Fatalf(
			"deadline after get #2 = %v, want %v (deadline from previous send was not refreshed)",
			got, d2,
		)
	}
	p.put(c2)
}

// TestPoolGetAfterClose ensures a closed pool never hands out new
// connections. Guards the pre-loop check from regressing.
func TestPoolGetAfterClose(t *testing.T) {
	p := newConnPool(1, func(ctx context.Context) (smtpClient, error) {
		return &fakeClient{conn: &fakeConn{}}, nil
	})
	p.close()

	_, err := p.get(context.Background())
	if !errors.Is(err, errPoolClosed) {
		t.Fatalf("err = %v, want errPoolClosed", err)
	}
}

// TestPoolGetHonorsCloseDuringNoop exercises the window between
// extracting an idle connection and returning it: Close is invoked
// from inside Noop. get must not return the connection, and it must be
// closed rather than leaked.
func TestPoolGetHonorsCloseDuringNoop(t *testing.T) {
	fc := &fakeConn{}
	var p *connPool
	c := &callbackClient{
		fakeClient: &fakeClient{conn: fc},
		onNoop:     func() { p.close() },
	}
	p = newConnPool(1, func(ctx context.Context) (smtpClient, error) {
		return c, nil
	})

	// Seed the pool with the callback client so get takes the idle path.
	p.put(&poolConn{smtpClient: c})

	_, err := p.get(context.Background())
	if !errors.Is(err, errPoolClosed) {
		t.Fatalf("err = %v, want errPoolClosed", err)
	}
	if !fc.closed {
		t.Fatal("connection returned from closed pool must be closed")
	}
}

// TestPoolGetHonorsCloseDuringDial exercises the window between finishing
// the dial and returning the fresh connection: Close is invoked from
// inside dial. get must not return the connection.
func TestPoolGetHonorsCloseDuringDial(t *testing.T) {
	var p *connPool
	p = newConnPool(1, func(ctx context.Context) (smtpClient, error) {
		p.close()
		return &fakeClient{conn: &fakeConn{}}, nil
	})

	_, err := p.get(context.Background())
	if !errors.Is(err, errPoolClosed) {
		t.Fatalf("err = %v, want errPoolClosed", err)
	}
}
