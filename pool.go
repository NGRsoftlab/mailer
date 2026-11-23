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

// maxProbeAttempts caps the number of consecutive failed Noop probes
// per get() call. Without this cap, a pool holding many dead-but-not-
// closed idle connections (e.g. MaxIdle=1000 against a hung server)
// would burn the caller's entire Timeout on sequential noopTimeout
// waits before falling through to a dial. After this many failures we
// conclude that the idle set is unusable and dial a fresh connection
// instead of probing the rest.
const maxProbeAttempts = 3

// errPoolClosed is returned when a get is attempted on a closed pool.
var errPoolClosed = errors.New("pool: closed")

// ctxBinder is implemented by connection types that support binding to
// a context. Plain smtpClient implementations (mocks) do not have to
// implement it: the pool checks with a type assertion.
type ctxBinder interface {
	BindContext(ctx context.Context)
	ReleaseContext()
}

// aborter is implemented by connection types that can tear the socket
// down without an SMTP QUIT. Used to abort a DATA transaction without
// committing a partial message.
type aborter interface {
	Abort()
}

// smtpClient is the subset of *smtp.Client used by the mailer. It exists
// so tests can substitute a mock without a network connection.
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
//
// onClose is invoked exactly once, when the connection is truly torn
// down. It releases the slot reserved against connPool.maxOpen (if any).
// sync.Once guards against double-release when Close is called from
// multiple sites (mailer.send's defer and connPool.put's overflow path).
type poolConn struct {
	smtpClient
	authed    bool
	closeOnce sync.Once
	onClose   func()
}

func (pc *poolConn) bind(ctx context.Context) {
	if b, ok := pc.smtpClient.(ctxBinder); ok {
		b.BindContext(ctx)
	}
}

func (pc *poolConn) release() {
	if b, ok := pc.smtpClient.(ctxBinder); ok {
		b.ReleaseContext()
	}
}

// abort discards the connection without sending an SMTP QUIT. See
// smtpConn.Abort for the RFC 5321 §4.1.1.4 reasoning.
func (pc *poolConn) abort() {
	pc.release()
	if a, ok := pc.smtpClient.(aborter); ok {
		a.Abort()
	} else {
		_ = pc.smtpClient.Close()
	}
	pc.closeOnce.Do(func() {
		if pc.onClose != nil {
			pc.onClose()
		}
	})
}

// Close releases the context watcher, closes the underlying client, and
// releases the MaxOpen slot (once).
func (pc *poolConn) Close() error {
	pc.release()
	err := pc.smtpClient.Close()
	pc.closeOnce.Do(func() {
		if pc.onClose != nil {
			pc.onClose()
		}
	})
	return err
}

type dialFunc func(context.Context) (smtpClient, error)

// acquireResult enumerates the four outcomes of acquireOpen. The caller
// uses this to decide between dialing, looping back, or returning an
// error — no re-probing of p.closed / ctx.Err() / p.idle is needed.
type acquireResult int

const (
	// acquireOK: a MaxOpen slot has been reserved. Caller must dial
	// (or, upon re-check, take an idle connection it found while
	// waiting — but the slot itself is now owned).
	acquireOK acquireResult = iota

	// acquireIdle: an idle connection became available while the
	// caller was blocked. The caller should loop and pick it up; no
	// slot was reserved.
	acquireIdle

	// acquireClosed: the pool was closed. Caller must return
	// errPoolClosed.
	acquireClosed

	// acquireCtxDone: ctx was cancelled while waiting. Caller must
	// return ctx.Err().
	acquireCtxDone
)

// connPool is a bounded SMTP connection pool.
//
// Concurrency model:
//   - p.mu protects idle and closed. It is never held across a
//     blocking operation (dial, Noop, Close).
//   - p.slots is the MaxOpen semaphore. A token in the channel means
//     "one open slot is taken" (whether idle or active). maxOpen == 0
//     disables the semaphore (unbounded pool).
//   - p.idleReady is a buffered signal that wake one waiter in
//     acquireOpen when put() returns a connection to the idle list.
//     The buffer is 1: a single token is enough because the woken
//     waiter immediately inspects p.idle, and a missed wakeup (buffer
//     already full) is harmless — the idle conn is still visible to
//     the next get() via the fast path.
//   - p.closedCh is closed exactly once in close(). Waiters select on
//     it to unblock immediately when the pool shuts down.
type connPool struct {
	mu          sync.Mutex
	idle        []*poolConn
	dial        dialFunc
	maxIdle     int
	maxOpen     int           // 0 = unbounded
	slots       chan struct{} // nil when maxOpen == 0
	idleReady   chan struct{} // buffered 1
	closedCh    chan struct{} // closed on close()
	noopTimeout time.Duration
	closed      bool
}

func newConnPool(maxIdle, maxOpen int, noopTimeout time.Duration, dial dialFunc) *connPool {
	if noopTimeout <= 0 {
		noopTimeout = defaultNoopTimeout
	}
	p := &connPool{
		maxIdle:     maxIdle,
		maxOpen:     maxOpen,
		noopTimeout: noopTimeout,
		dial:        dial,
		idleReady:   make(chan struct{}, 1),
		closedCh:    make(chan struct{}),
	}
	if maxOpen > 0 {
		p.slots = make(chan struct{}, maxOpen)
	}
	return p
}

// acquireOpen reserves one slot against maxOpen. Blocks until either a
// slot is available, an idle connection appears, ctx is cancelled, or
// the pool is closed.
//
// Unlike a sync.Cond-based implementation, this creates no goroutines:
// both the ctx-cancelled and closed cases are native select branches.
//
// Called and returns with p.mu released.
func (p *connPool) acquireOpen(ctx context.Context) acquireResult {
	if p.maxOpen == 0 {
		return acquireOK
	}
	// Fast path: try to grab a slot without blocking.
	select {
	case p.slots <- struct{}{}:
		return acquireOK
	default:
	}

	// Slow path: wait for any of the four events.
	select {
	case p.slots <- struct{}{}:
		return acquireOK
	case <-p.idleReady:
		return acquireIdle
	case <-p.closedCh:
		return acquireClosed
	case <-ctx.Done():
		return acquireCtxDone
	}
}

// get returns a live connection from the pool or dials a new one.
//
// The idle fast path is bounded by maxProbeAttempts: after that many
// consecutive probe failures the pool stops inspecting idle connections
// and dials fresh, so a large dead-idle set cannot consume the caller's
// entire Timeout.
func (p *connPool) get(ctx context.Context) (*poolConn, error) {
	probeFailures := 0
	for {
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			return nil, errPoolClosed
		}
		if len(p.idle) > 0 && probeFailures < maxProbeAttempts {
			c := p.idle[len(p.idle)-1]
			p.idle = p.idle[:len(p.idle)-1]
			p.mu.Unlock()

			if ok := p.probeAndBind(ctx, c); ok {
				return c, nil
			}
			probeFailures++
			continue
		}
		p.mu.Unlock()

		switch p.acquireOpen(ctx) {
		case acquireOK:
			// Slot acquired; fall through to the post-acquire path.
		case acquireIdle:
			// An idle connection appeared while waiting. Loop back and
			// take it — unless the probe budget is exhausted, in which
			// case the next iteration acquires a slot and dials.
			continue
		case acquireClosed:
			return nil, errPoolClosed
		case acquireCtxDone:
			return nil, ctx.Err()
		}

		// Slot reserved. Re-check idle — another goroutine may have
		// returned a connection while we were waiting. Gated by the
		// probe budget so we don't keep probing dead idles.
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			p.releaseOpen()
			return nil, errPoolClosed
		}
		if len(p.idle) > 0 && probeFailures < maxProbeAttempts {
			c := p.idle[len(p.idle)-1]
			p.idle = p.idle[:len(p.idle)-1]
			p.mu.Unlock()
			p.releaseOpen()

			if ok := p.probeAndBind(ctx, c); ok {
				return c, nil
			}
			probeFailures++
			continue
		}
		p.mu.Unlock()

		// Dial with the slot held.
		raw, err := p.dial(ctx)
		if err != nil {
			p.releaseOpen()
			return nil, err
		}

		pc := &poolConn{
			smtpClient: raw,
			onClose:    p.releaseOpen,
		}
		// Close may have run while dial was in flight.
		p.mu.Lock()
		if p.closed {
			p.mu.Unlock()
			_ = pc.Close()
			return nil, errPoolClosed
		}
		p.mu.Unlock()
		return pc, nil
	}
}

// probeAndBind validates an idle connection and binds it to ctx.
// Returns true when the connection is ready to hand out.
func (p *connPool) probeAndBind(ctx context.Context, c *poolConn) bool {
	if err := ctx.Err(); err != nil {
		_ = c.Close()
		return false
	}
	callerDeadline, hasDeadline := ctx.Deadline()
	probeDeadline := time.Now().Add(p.noopTimeout)
	if hasDeadline && callerDeadline.Before(probeDeadline) {
		probeDeadline = callerDeadline
	}
	if err := c.SetDeadline(probeDeadline); err != nil {
		_ = c.Close()
		return false
	}
	if err := c.Noop(); err != nil {
		_ = c.Close()
		return false
	}
	if err := ctx.Err(); err != nil {
		_ = c.Close()
		return false
	}
	p.mu.Lock()
	closed := p.closed
	p.mu.Unlock()
	if closed {
		_ = c.Close()
		return false
	}
	if err := c.SetDeadline(callerDeadline); err != nil {
		_ = c.Close()
		return false
	}
	c.bind(ctx)
	return true
}

// releaseOpen returns the slot acquired by acquireOpen. Safe to call
// from any goroutine; invoked exactly once per connection via
// poolConn.closeOnce.
func (p *connPool) releaseOpen() {
	if p.maxOpen == 0 {
		return
	}
	<-p.slots
}

// put returns c to the idle pool, or closes it if the pool is closed or
// full. Explicit Lock/Unlock in both branches: c.Close() calls
// releaseOpen (which reads from p.slots) — holding p.mu across Close
// is not required but keeping the critical section tight is.
func (p *connPool) put(c *poolConn) {
	if c == nil {
		return
	}
	c.release()
	p.mu.Lock()
	if p.closed || len(p.idle) >= p.maxIdle {
		p.mu.Unlock()
		_ = c.Close()
		return
	}
	p.idle = append(p.idle, c)
	p.mu.Unlock()
	// Wake one waiter blocked in acquireOpen: an idle connection is
	// just as good as a freshly-acquired slot from their perspective.
	// Non-blocking: if no one is waiting or the buffer is already full,
	// the next get() will find the idle conn via the fast path.
	select {
	case p.idleReady <- struct{}{}:
	default:
	}
}

func (p *connPool) close() {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return
	}
	p.closed = true
	idle := p.idle
	p.idle = nil
	p.mu.Unlock()

	// Unblock everyone waiting in acquireOpen.
	close(p.closedCh)

	for _, c := range idle {
		_ = c.Close()
	}
}
