// Copyright 2020-2026 NGR Softlab
package mailer

import (
	"context"
	"errors"
	"io"
	"net"
	"net/smtp"
	"sync"
	"sync/atomic"
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

// noopFailClient fails Noop — used to simulate a dead idle connection.
type noopFailClient struct {
	*fakeClient
}

func (c *noopFailClient) Noop() error {
	return errors.New("simulated NOOP failure")
}

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

// peekNoopClient captures the socket deadline in effect at the moment
// Noop runs, so tests can verify the probe is capped by noopTimeout.
type peekNoopClient struct {
	*fakeClient
	fc     *fakeConn
	onNoop func(deadline time.Time)
}

func (c *peekNoopClient) Noop() error {
	if c.onNoop != nil {
		c.onNoop(c.fc.deadline)
	}
	return nil
}

type nopWriteCloser struct{}

func (nopWriteCloser) Write(p []byte) (int, error) { return len(p), nil }
func (nopWriteCloser) Close() error                { return nil }

// spyCtxClient implements both smtpClient and ctxBinder so the test can
// verify the pool binds a context on checkout of an idle connection and
// releases it on put.
type spyCtxClient struct {
	*fakeClient
	mu       sync.Mutex
	binds    []context.Context
	releases int
}

func (s *spyCtxClient) BindContext(ctx context.Context) {
	s.mu.Lock()
	s.binds = append(s.binds, ctx)
	s.mu.Unlock()
}

func (s *spyCtxClient) ReleaseContext() {
	s.mu.Lock()
	s.releases++
	s.mu.Unlock()
}

// TestPoolRefreshesDeadlineOnReuse verifies that a pooled connection
// receives a fresh deadline derived from the current context each time
// it is checked out.
func TestPoolRefreshesDeadlineOnReuse(t *testing.T) {
	var fc *fakeConn

	p := newConnPool(1, 0, defaultNoopTimeout, func(ctx context.Context) (smtpClient, error) {
		fc = &fakeConn{}
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
// connections.
func TestPoolGetAfterClose(t *testing.T) {
	p := newConnPool(1, 0, defaultNoopTimeout, func(ctx context.Context) (smtpClient, error) {
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
// from inside Noop.
func TestPoolGetHonorsCloseDuringNoop(t *testing.T) {
	fc := &fakeConn{}
	var p *connPool
	c := &callbackClient{
		fakeClient: &fakeClient{conn: fc},
		onNoop:     func() { p.close() },
	}
	p = newConnPool(1, 0, defaultNoopTimeout, func(ctx context.Context) (smtpClient, error) {
		return c, nil
	})

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
// the dial and returning the fresh connection.
func TestPoolGetHonorsCloseDuringDial(t *testing.T) {
	var p *connPool
	p = newConnPool(1, 0, defaultNoopTimeout, func(ctx context.Context) (smtpClient, error) {
		p.close()
		return &fakeClient{conn: &fakeConn{}}, nil
	})

	_, err := p.get(context.Background())
	if !errors.Is(err, errPoolClosed) {
		t.Fatalf("err = %v, want errPoolClosed", err)
	}
}

// TestPoolNoopDeadlineCappedByNoopTimeout verifies that a Noop on an
// idle connection is bounded by noopTimeout, not by the caller's full
// send budget.
func TestPoolNoopDeadlineCappedByNoopTimeout(t *testing.T) {
	fc := &fakeConn{}
	noopTimeout := 100 * time.Millisecond

	var sawDeadline time.Time
	c := &peekNoopClient{
		fakeClient: &fakeClient{conn: fc},
		fc:         fc,
		onNoop:     func(d time.Time) { sawDeadline = d },
	}
	p := newConnPool(1, 0, noopTimeout, func(ctx context.Context) (smtpClient, error) {
		return c, nil
	})
	p.put(&poolConn{smtpClient: c})

	d := time.Now().Add(time.Hour)
	ctx, cancel := context.WithDeadline(context.Background(), d)
	defer cancel()

	got, err := p.get(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	p.put(got)

	if sawDeadline.IsZero() {
		t.Fatal("Noop saw no deadline; expected now+noopTimeout")
	}
	if !sawDeadline.Before(d) {
		t.Fatalf(
			"Noop saw deadline %v, want < %v (should be capped by noopTimeout %v)",
			sawDeadline, d, noopTimeout,
		)
	}
	if sawDeadline.After(time.Now().Add(3 * noopTimeout)) {
		t.Fatalf("Noop saw deadline %v, want roughly now+%v", sawDeadline, noopTimeout)
	}
	if !fc.deadline.Equal(d) {
		t.Fatalf("post-get deadline = %v, want %v", fc.deadline, d)
	}
}

// TestPoolNoopBoundedWithoutCtxDeadline verifies that a Noop is bounded
// by noopTimeout even when the caller supplied a context without any
// deadline.
func TestPoolNoopBoundedWithoutCtxDeadline(t *testing.T) {
	fc := &fakeConn{}
	noopTimeout := 100 * time.Millisecond

	var sawDeadline time.Time
	c := &peekNoopClient{
		fakeClient: &fakeClient{conn: fc},
		fc:         fc,
		onNoop:     func(d time.Time) { sawDeadline = d },
	}
	p := newConnPool(1, 0, noopTimeout, func(ctx context.Context) (smtpClient, error) {
		return c, nil
	})
	p.put(&poolConn{smtpClient: c})

	got, err := p.get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	p.put(got)

	if sawDeadline.IsZero() {
		t.Fatal("Noop saw no deadline; expected now+noopTimeout")
	}
	if sawDeadline.After(time.Now().Add(3 * noopTimeout)) {
		t.Fatalf("Noop saw deadline %v, want roughly now+%v", sawDeadline, noopTimeout)
	}
	if !fc.deadline.IsZero() {
		t.Fatalf("post-get deadline = %v, want zero (no ctx deadline)", fc.deadline)
	}
}

// TestPoolBindsAndReleasesContextOnReuse verifies the checkout lifecycle
// contract: an idle connection gets bound to the caller's context on
// get and released on put.
func TestPoolBindsAndReleasesContextOnReuse(t *testing.T) {
	spy := &spyCtxClient{fakeClient: &fakeClient{conn: &fakeConn{}}}
	p := newConnPool(1, 0, defaultNoopTimeout, func(ctx context.Context) (smtpClient, error) {
		return spy, nil
	})
	p.put(&poolConn{smtpClient: spy})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	c, err := p.get(ctx)
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	spy.mu.Lock()
	if len(spy.binds) != 1 || spy.binds[0] != ctx {
		spy.mu.Unlock()
		t.Fatalf("binds = %v, want exactly [ctx]", spy.binds)
	}
	spy.mu.Unlock()

	p.put(c)
	spy.mu.Lock()
	releasesAfterPut := spy.releases
	spy.mu.Unlock()
	if releasesAfterPut < 1 {
		t.Fatalf("releases after put = %d, want >= 1", releasesAfterPut)
	}
}

// TestPoolMaxOpenBlocksExtraDials verifies that MaxOpen caps concurrent
// dials.
func TestPoolMaxOpenBlocksExtraDials(t *testing.T) {
	const maxOpen = 2

	var dials atomic.Int32
	var inDial atomic.Int32
	var peakInDial atomic.Int32

	release := make(chan struct{})

	p := newConnPool(maxOpen, maxOpen, defaultNoopTimeout, func(ctx context.Context) (smtpClient, error) {
		dials.Add(1)
		cur := inDial.Add(1)
		for {
			peak := peakInDial.Load()
			if cur <= peak || peakInDial.CompareAndSwap(peak, cur) {
				break
			}
		}
		select {
		case <-release:
		case <-ctx.Done():
			inDial.Add(-1)
			return nil, ctx.Err()
		}
		inDial.Add(-1)
		return &fakeClient{conn: &fakeConn{}}, nil
	})

	var wg sync.WaitGroup
	for i := 0; i < maxOpen+1; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			c, err := p.get(context.Background())
			if err != nil {
				return
			}
			_ = c.Close()
		}()
	}

	deadline := time.Now().Add(2 * time.Second)
	for dials.Load() < maxOpen && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if got := dials.Load(); got < maxOpen {
		t.Fatalf("dials = %d, want at least %d", got, maxOpen)
	}

	time.Sleep(100 * time.Millisecond)
	if got := dials.Load(); got > maxOpen {
		t.Fatalf("dials = %d, want <= %d (MaxOpen must cap concurrent dials)", got, maxOpen)
	}

	close(release)

	done := make(chan struct{})
	go func() {
		wg.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("timeout waiting for all gets to complete")
	}

	if got := peakInDial.Load(); got > maxOpen {
		t.Fatalf("peak concurrent dials = %d, want <= %d", got, maxOpen)
	}
	if got := dials.Load(); got != maxOpen+1 {
		t.Fatalf("total dials = %d, want %d", got, maxOpen+1)
	}
}

// TestPoolMaxOpenReusesIdle verifies that when an idle connection is
// available, the pool hands it out instead of dialing a fresh one even
// under MaxOpen=1.
func TestPoolMaxOpenReusesIdle(t *testing.T) {
	var dials atomic.Int32
	p := newConnPool(1, 1, defaultNoopTimeout, func(ctx context.Context) (smtpClient, error) {
		dials.Add(1)
		return &fakeClient{conn: &fakeConn{}}, nil
	})

	c1, err := p.get(context.Background())
	if err != nil {
		t.Fatalf("get #1: %v", err)
	}
	p.put(c1)

	c2, err := p.get(context.Background())
	if err != nil {
		t.Fatalf("get #2: %v", err)
	}
	p.put(c2)

	if got := dials.Load(); got != 1 {
		t.Fatalf("dials = %d, want 1 (idle reuse should not dial)", got)
	}
}

// TestNewCapsMaxIdleAtMaxOpen verifies that MaxIdle > MaxOpen is clamped
// to MaxOpen.
func TestNewCapsMaxIdleAtMaxOpen(t *testing.T) {
	m, err := newWithDial(Config{
		Host:    "example.com",
		MaxIdle: 10,
		MaxOpen: 3,
	}, func(ctx context.Context) (smtpClient, error) {
		return &fakeClient{conn: &fakeConn{}}, nil
	})
	if err != nil {
		t.Fatalf("newWithDial: %v", err)
	}
	defer m.Close()

	if m.cfg.MaxIdle != 3 {
		t.Fatalf("MaxIdle = %d, want 3 (clamped to MaxOpen)", m.cfg.MaxIdle)
	}
}

// TestPoolProbeAttemptsBounded verifies that get() gives up probing
// after maxProbeAttempts consecutive Noop failures and falls through to
// a fresh dial, rather than probing every idle connection in a large
// dead pool. Without this cap, a pool holding hundreds of stale idle
// connections would burn the caller's entire Timeout before dialing.
//
// The 5 dead idle connections are seeded via put. get tries up to 3,
// each failing Noop, then dials. The remaining 2 dead idles stay in the
// pool (they will be probed by subsequent gets, also bounded by the
// attempt cap).
func TestPoolProbeAttemptsBounded(t *testing.T) {
	var dials atomic.Int32

	deadConn := func() smtpClient {
		return &noopFailClient{fakeClient: &fakeClient{conn: &fakeConn{}}}
	}

	p := newConnPool(10, 0, defaultNoopTimeout, func(ctx context.Context) (smtpClient, error) {
		dials.Add(1)
		return &fakeClient{conn: &fakeConn{}}, nil
	})

	for i := 0; i < 5; i++ {
		p.put(&poolConn{smtpClient: deadConn()})
	}

	c, err := p.get(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got := dials.Load(); got != 1 {
		t.Fatalf("dials = %d, want 1 (after %d probe failures)", got, maxProbeAttempts)
	}
	if _, isDead := c.smtpClient.(*noopFailClient); isDead {
		t.Fatal("get returned a dead connection instead of the fresh dial result")
	}
	p.put(c)
}
