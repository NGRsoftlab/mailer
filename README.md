# mailer

[![Go Reference](https://pkg.go.dev/badge/github.com/NGRsoftlab/mailer.svg)](https://pkg.go.dev/github.com/NGRsoftlab/mailer)
[![Tests](https://github.com/NGRsoftlab/mailer/actions/workflows/tests.yml/badge.svg)](https://github.com/NGRsoftlab/mailer/actions/workflows/tests.yml)
[![Fuzzing](https://github.com/NGRsoftlab/mailer/actions/workflows/fuzz.yml/badge.svg?branch=main&event=schedule)](https://github.com/NGRsoftlab/mailer/actions/workflows/fuzz.yml)
[![Integration](https://github.com/NGRsoftlab/mailer/actions/workflows/integration.yml/badge.svg?branch=main&event=push)](https://github.com/NGRsoftlab/mailer/actions/workflows/integration.yml)
[![Go Version](https://img.shields.io/github/go-mod/go-version/NGRsoftlab/mailer)](go.mod)
[![License](https://img.shields.io/github/license/NGRsoftlab/mailer)](LICENSE)

A context-aware SMTP client for Go with connection pooling, parallel bulk sending, flexible TLS, and pluggable authentication.

## Table of contents

- [Features](#features)
- [Installation](#installation)
- [Quick start](#quick-start)
- [Configuration](#configuration)
- [Sending messages](#sending-messages)
- [Message building](#message-building)
- [Authentication](#authentication)
- [TLS, SSL and mTLS](#tls-ssl-and-mtls)
- [Connection pooling](#connection-pooling)
- [Parallel bulk sending](#parallel-bulk-sending)
- [Observability](#observability)
- [Error handling](#error-handling)
- [Testing](#testing)
- [Notes and non-goals](#notes-and-non-goals)
- [License](#license)

## Features

- **Context-aware**: every send honors `context.Context` end to end. Deadlines become socket deadlines; cancellation tears the underlying TCP socket down, interrupting whichever SMTP operation is currently blocked — greeting read, TLS handshake, AUTH exchange, DATA write, or a `NOOP` liveness probe. `Send` and `SendBulk` return `context.Canceled` / `context.DeadlineExceeded` from every phase, not the raw socket error.
- **Pluggable authentication**: PLAIN, LOGIN and XOAUTH2 are built in. `mailer.Auth` is a type alias for `smtp.Auth`, so any custom mechanism compatible with `net/smtp` plugs in without adaptation.
- **Connection pool**: SMTP connections are reused across sends, probed with `NOOP` before being handed out (bounded by `Config.NoopTimeout`), and transparently replaced on failure. The pool enforces an optional global cap on open connections (`Config.MaxOpen`) on top of the idle limit (`Config.MaxIdle`).
- **TLS everywhere**: plain SMTP with opportunistic STARTTLS, required STARTTLS, implicit TLS (SMTPS), and mutual TLS (mTLS) via `tls.Config.Certificates`. `Config.TLSConfig` is cloned on `New`, and `ServerName` is auto-populated from `Host` when empty so hostname verification is never silently disabled.
- **MIME message builder**: plain text, HTML, `multipart/alternative`, `multipart/mixed` with attachments, custom headers, automatic base64 encoding, RFC 5322 `Date` / `Message-ID`, RFC 2231 filenames, and header-injection protection.
- **Safe partial-send semantics**: if an error occurs after `DATA`, the transaction is aborted via raw socket close (RFC 5321 §4.1.1.4) so the server never commits a partial message.
- **Single and bulk send**: `Send` delivers a message to all envelope recipients in one SMTP transaction. `SendBulk` delivers personalized copies in parallel, bounded by `Config.MaxConcurrency`, and aggregates errors with `errors.Join`.
- **Observability hooks**: `Config.OnDial` and `Config.OnSend` receive per-attempt durations and errors, so callers can wire metrics without forking the library.
- **No third-party runtime dependencies**: everything is built on the Go standard library. The only external dependency (`emersion/go-smtp`) is used in integration tests behind a build tag.

## Installation

```sh
go get github.com/NGRsoftlab/mailer
```

Requires Go 1.26.10 or newer.

## Quick start

```go
package main

import (
    "context"
    "log"

    "github.com/NGRsoftlab/mailer"
)

func main() {
    m, err := mailer.New(mailer.Config{
        Host:    "smtp.example.com",
        Port:    587,
        Auth:    mailer.PlainAuth("", "user", "pass", "smtp.example.com"),
        TLSMode: mailer.TLSStart,
    })
    if err != nil {
        log.Fatal(err)
    }
    defer m.Close()

    msg := &mailer.Message{
        From:    "sender@example.com",
        To:      []string{"rcpt@example.com"},
        Subject: "Hello",
        Text:    "Plain text body",
        HTML:    "<p>HTML body</p>",
    }

    if err := m.Send(context.Background(), msg); err != nil {
        log.Fatal(err)
    }
}
```

## Configuration

`mailer.New` accepts a `Config` struct:

| Field            | Type            | Default | Description                                                                                          |
|------------------|-----------------|---------|------------------------------------------------------------------------------------------------------|
| `Host`           | `string`        | —       | SMTP server hostname. Required.                                                                       |
| `Port`           | `int`           | `587`   | SMTP server port.                                                                                     |
| `Auth`           | `mailer.Auth`   | `nil`   | Auth mechanism. If `nil`, `AUTH` is not attempted.                                                    |
| `TLSMode`        | `TLSMode`       | `TLSNone` | TLS strategy. Unknown values are rejected by `New`.                                                 |
| `TLSConfig`      | `*tls.Config`   | `nil`   | TLS settings. Supply `Certificates` for mTLS. Cloned by `New`; `ServerName` auto-filled if empty.     |
| `MaxIdle`        | `int`           | `5`     | Maximum number of idle pooled connections. Clamped to `MaxOpen` when `MaxOpen > 0`.                   |
| `MaxOpen`        | `int`           | `0`     | Global cap on open connections (idle + active). `0` means unbounded.                                  |
| `MaxConcurrency` | `int`           | `4`     | Maximum parallel deliveries in `SendBulk`.                                                            |
| `Timeout`        | `time.Duration` | `30s`   | Per-send timeout. Applied only if the incoming context has no deadline.                               |
| `NoopTimeout`    | `time.Duration` | `5s`    | Liveness-probe budget per reused connection. Caps the socket deadline during `NOOP`.                  |
| `OnDial`         | `func(time.Duration, error)` | `nil` | Called once per dial attempt. Synchronous — keep it cheap.                              |
| `OnSend`         | `func(time.Duration, error)` | `nil` | Called once per `Send`/`SendBulk` delivery. Synchronous — keep it cheap.                |

`Config` is validated and normalized inside `New`. Invalid input (empty `Host`, unknown `TLSMode`, negative `MaxOpen`) returns an error. Zero values of optional fields are replaced with defaults.

## Sending messages

### Single-transaction send

`Send` delivers a message to **all** of its envelope recipients (`To` + `Cc` + `Bcc`) in one SMTP transaction. This is the right choice when you want recipients to see each other, or when using `Bcc` for a mailing list.

```go
msg := &mailer.Message{
    From:    "sender@example.com",
    To:      []string{"a@example.com"},
    Cc:      []string{"b@example.com"},
    Bcc:     []string{"hidden@example.com"},
    Subject: "Team update",
    Text:    "Hello team",
}
err := m.Send(ctx, msg)
```

`Send` rejects `nil` messages and messages with no recipients before touching the pool or the network.

### Bulk send

`SendBulk` takes a message and a slice of recipients and delivers a **personalized copy** to each of them in parallel:

```go
recipients := []string{"a@example.com", "b@example.com", "c@example.com"}
if err := m.SendBulk(ctx, msg, recipients); err != nil {
    // Joined error; see Error handling below.
    log.Println(err)
}
```

For each recipient, `SendBulk` produces a copy in which:

- `To` contains **only** that recipient,
- `Cc` is cleared,
- `Bcc` is cleared,
- `MessageID` is reset to empty **unless** the caller set it explicitly — this makes each auto-generated `Message-ID` unique per copy,
- all other fields (`From`, `Subject`, `Text`, `HTML`, `Attachments`, `Headers`, `Date`) are preserved.

Deliveries run concurrently, bounded by `Config.MaxConcurrency`. Failures are collected per recipient — one failed delivery does not abort the rest.

## Message building

`Message` describes a MIME email. `Bytes` renders it as a valid RFC 5322 document and automatically chooses the correct MIME structure:

| Input                                   | Output structure                              |
|-----------------------------------------|-----------------------------------------------|
| `Text` only                             | `text/plain`                                  |
| `HTML` only                             | `text/html`                                   |
| `Text` + `HTML`                         | `multipart/alternative`                       |
| Any of the above + attachments          | `multipart/mixed` wrapping the above          |
| Neither `Text` nor `HTML`               | empty `text/plain` body (structurally valid)  |

The following headers are always emitted, in this order:

- `From`, `To`, `Cc` (from the corresponding `Message` fields),
- `Subject` (RFC 2047 encoded when non-ASCII),
- `Date` — from `Message.Date`, or `time.Now()` in `RFC1123Z` form if zero,
- `Message-ID` — `<id@domain>` built from `Message.MessageID` when provided, or generated. The final token is filtered to printable ASCII minus characters that would break out of the header; a caller-supplied ID without brackets gets them added.
- `MIME-Version`, `Content-Type`, `Content-Transfer-Encoding`.

The following headers are reserved and cannot be overridden via `Message.Headers`: `From`, `To`, `Cc`, `Bcc`, `Subject`, `MIME-Version`, `Date`, `Message-ID`, `Content-Type`, `Content-Transfer-Encoding`.

### Text and HTML

```go
msg := &mailer.Message{
    From:    "sender@example.com",
    To:      []string{"rcpt@example.com"},
    Subject: "Order confirmed",
    Text:    "Your order has been confirmed.",
    HTML:    `<h1>Order confirmed</h1><p>Your order has been confirmed.</p>`,
}
```

When both `Text` and `HTML` are present, the resulting MIME is `multipart/alternative`, and mail clients choose the best representation.

**Styling**: there is no separate styling API. HTML email styling is just HTML — inline CSS on the `HTML` field works. For templating, render the template to a string before setting `HTML` and `Text`.

**Signature**: likewise, there is no dedicated signature field. Append the signature to `Text` and/or `HTML` before setting the message.

### Date and Message-ID

Both fields are optional; when zero/empty, `Bytes` fills them in:

```go
msg := &mailer.Message{
    From:      "sender@example.com",
    To:        []string{"rcpt@example.com"},
    Subject:   "Order confirmed",
    Text:      "Your order has been confirmed.",
    Date:      time.Date(2024, 5, 1, 12, 0, 0, 0, time.UTC),
    MessageID: "order-42@example.com",
}
```

- `Date` is rendered in `RFC1123Z` (e.g. `Wed, 01 May 2024 12:00:00 +0000`).
- `MessageID` is wrapped in `<>` if the caller omitted the brackets. Illegal characters are stripped; if the result is empty, a fresh ID is generated. Leave `MessageID` empty to let the library generate a unique value per `Bytes` call — this is what guarantees `SendBulk` copies get distinct IDs.

### Attachments

```go
msg.Attachments = []mailer.Attachment{
    {
        Name: "invoice.pdf",
        Data: pdfBytes,
        // ContentType is optional; if empty it is inferred from Name.
    },
    {
        Name: "data.bin",
        Data: rawBytes,
        ContentType: "application/octet-stream",
    },
}
```

Attachments are base64-encoded and sent as separate MIME parts with `Content-Disposition: attachment; filename=...`. Non-ASCII filenames are encoded per RFC 2231 (`filename*=UTF-8''…`), which modern clients render correctly; the legacy RFC 2047 form is not used because many clients reject it inside a MIME parameter. CR/LF and other control characters in the filename are stripped.

### Custom headers

```go
msg.Headers = map[string]string{
    "Reply-To":      "support@example.com",
    "X-Campaign-ID": "welcome-2024",
}
```

Safety rules enforced by the builder:

- Header names containing `:` or CR/LF are silently dropped.
- Header values have CR, LF, U+0085 (NEL), U+2028 and U+2029 stripped, preventing header injection via any of those line separators.
- Reserved headers (`From`, `To`, `Cc`, `Bcc`, `Subject`, `MIME-Version`, `Date`, `Message-ID`, `Content-Type`, `Content-Transfer-Encoding`) are set from the dedicated `Message` fields and cannot be overridden via `Headers`.

## Authentication

`mailer.Auth` is a type alias for `net/smtp.Auth`. The following work out of the box:

### PLAIN

```go
Auth: mailer.PlainAuth("", "username", "password", "smtp.example.com")
```

### LOGIN

```go
Auth: mailer.LoginAuth("username", "password")
```

The LOGIN mechanism is matched case-insensitively and tolerant of surrounding whitespace, because it predates RFC 4616 and real servers vary (Exchange sends `Username:` with a capital U; some MTAs send `username:` or with a trailing space). An unrecognised challenge is surfaced as an error rather than answered with credentials.

### XOAUTH2

```go
Auth: mailer.XOAuth2Auth("user@example.com", oauth2AccessToken)
```

When the server rejects the token it sends a base64-encoded JSON error body and waits for the client to acknowledge it with an empty `\x01` before closing the session. `XOAuth2Auth` sends that ACK, so `net/smtp.Client.Auth` surfaces the server's own authentication error instead of hanging the connection half-open until the socket deadline expires.

### Custom mechanisms

Any type implementing `smtp.Auth` is accepted:

```go
type mySCRAMAuth struct {
    // ...
}

func (a *mySCRAMAuth) Start(server *smtp.ServerInfo) (string, []byte, error) { ... }
func (a *mySCRAMAuth) Next(fromServer []byte, more bool) ([]byte, error)     { ... }

m, _ := mailer.New(mailer.Config{
    Host: "smtp.example.com",
    Auth: &mySCRAMAuth{ /* ... */ },
})
```

The mailer only issues `AUTH` if the server advertises the `AUTH` extension and the pooled connection has not been authenticated yet. Per RFC 4954, `AUTH` is issued at most once per connection, even across many sends.

## TLS, SSL and mTLS

### Plain SMTP with opportunistic STARTTLS (`TLSNone`)

```go
m, _ := mailer.New(mailer.Config{
    Host:      "smtp.example.com",
    Port:      587,
    TLSMode:   mailer.TLSNone,
    TLSConfig: &tls.Config{ServerName: "smtp.example.com"},
})
```

If the server advertises `STARTTLS`, the connection is upgraded. If not, the session proceeds in plain text. Use this only for servers on trusted networks.

### Required STARTTLS (`TLSStart`)

```go
m, _ := mailer.New(mailer.Config{
    Host:      "smtp.example.com",
    Port:      587,
    TLSMode:   mailer.TLSStart,
    TLSConfig: &tls.Config{ServerName: "smtp.example.com"},
})
```

If the server does not advertise `STARTTLS`, `Send` fails. This is the recommended mode for internet-facing SMTP on port 587.

### Implicit TLS / SMTPS (`TLSImplicit`)

```go
m, _ := mailer.New(mailer.Config{
    Host:      "smtp.example.com",
    Port:      465,
    TLSMode:   mailer.TLSImplicit,
    TLSConfig: &tls.Config{ServerName: "smtp.example.com"},
})
```

The TLS handshake happens immediately after the TCP connection is established (port 465). `STARTTLS` is not used.

### Mutual TLS (mTLS)

mTLS is not a separate mode — it is enabled by populating `tls.Config.Certificates`. All three modes support it.

```go
cert, err := tls.LoadX509KeyPair("client.crt", "client.key")
if err != nil {
    log.Fatal(err)
}

m, _ := mailer.New(mailer.Config{
    Host:    "smtp.internal.example.com",
    Port:    465,
    TLSMode: mailer.TLSImplicit,
    TLSConfig: &tls.Config{
        ServerName:   "smtp.internal.example.com",
        Certificates: []tls.Certificate{cert},
        // Optionally pin the server CA:
        // RootCAs: caPool,
    },
})
```

If `TLSConfig` is `nil`, a minimal one is created automatically with `ServerName` set to `Config.Host`. If you do supply a `TLSConfig`, `New` clones it and fills `ServerName` from `Host` when empty — an empty `ServerName` in Go's `crypto/tls` disables hostname verification entirely, making TLS trivially vulnerable to MITM. An explicitly set `ServerName` is preserved as-is. `TLSMode` is validated: values other than `TLSNone`, `TLSStart`, `TLSImplicit` are rejected by `New`, so an out-of-range mode can never fall through to plaintext.

## Connection pooling

Each `Mailer` owns a pool of SMTP connections. The pool works as follows:

1. On `Send` / `SendBulk`, the mailer asks the pool for a connection.
2. If the pool has an idle connection, its liveness is verified with `NOOP`, bounded by `Config.NoopTimeout` or the caller's deadline, whichever is shorter.
    - If `NOOP` succeeds, the socket deadline is restored to the caller's deadline and the connection is reused.
    - If `NOOP` fails or times out, the connection is closed and the next idle one is tried. After `maxProbeAttempts` consecutive failures (currently 3) the pool stops probing idle connections and dials a fresh one, so a large dead-idle set cannot consume the caller's entire `Timeout`.
3. If no live idle connection is available, a new one is dialed. When `Config.MaxOpen > 0`, the number of concurrently open connections (idle + active) is capped; new `get` calls block until a slot frees up or the caller's context is cancelled.
4. After a successful send, the connection is returned to the pool (or closed if the pool is at `MaxIdle`).
5. On any error during send, the connection is discarded — a connection that has seen an error may be in an undefined state.

This gives transparent reconnect behavior: dead connections are detected and replaced without any background goroutines.

`Close` marks the pool as closed and drops every idle connection. It does **not** wait for in-flight `Send` calls to return — that is the caller's responsibility (typically via `defer m.Close()` in `main` and joining any goroutines that call `Send` before returning). Once `Close` has returned, every subsequent `get` returns `errPoolClosed` (`errors.Is`-checkable via the sentinel). In-flight sends are unaffected; idle connections extracted immediately before `Close` are closed rather than handed out.

Notes:

- `MaxIdle` bounds the number of **idle** connections. `MaxOpen` bounds the **total** number of open connections (idle + active). With `MaxOpen=0`, active connections can exceed `MaxIdle`; they are closed rather than pooled when the send completes.
- `MaxIdle > MaxOpen` is clamped to `MaxOpen` at construction time. Otherwise idle connections would consume the entire open budget and deadlock new sends.
- The pool is safe for concurrent use. No goroutines are created per waiting caller — the pool uses a buffered channel as a semaphore and a `sync.Mutex` for the idle list.
- `NoopTimeout` bounds the liveness probe so a hung peer cannot consume the entire `Send` budget before the SMTP dialogue even starts. Raise it only if you observe spurious evictions of live connections against a high-latency server; lower it if you want faster failure on dead connections.

## Parallel bulk sending

`SendBulk` runs deliveries in parallel with two mechanisms:

- **Semaphore** — bounds the number of in-flight goroutines to `Config.MaxConcurrency`. Defaults to 4. Set it to the value your SMTP provider tolerates (most providers rate-limit aggressively; 4–8 is usually safe).
- **Context cancellation** — before starting each delivery, `SendBulk` checks `ctx.Err()` in the dispatch loop *and* again inside the worker before entering the SMTP dialogue. The second check closes the race window between the dispatch check and the worker running, so a context cancelled in that window still fails per recipient with `context.Canceled` without touching the network. In-flight sends are cancelled via the context propagated to the socket.

Ordering is **not** guaranteed — recipients are delivered in a non-deterministic order. If ordering matters, either sort the recipient list at the caller and process the resulting errors in the corresponding order, or call `Send` sequentially.

Considerations:

- The SMTP server is usually the bottleneck. Increasing `MaxConcurrency` beyond what the server allows will yield rate-limit errors (typically `421` / `450`), not throughput.
- When `MaxConcurrency` is higher than `MaxIdle`, connections are created on demand and closed after each send — that is correct, but produces more TLS handshakes. Match `MaxIdle` to `MaxConcurrency` if you have long-running pools.

## Observability

Two optional hooks let callers wire metrics without forking the library:

```go
m, _ := mailer.New(mailer.Config{
    Host: "smtp.example.com",
    Port: 587,

    OnDial: func(d time.Duration, err error) {
        dialDuration.Observe(d.Seconds())
        if err != nil {
            dialErrors.WithLabelValues(classify(err)).Inc()
        }
    },

    OnSend: func(d time.Duration, err error) {
        sendDuration.Observe(d.Seconds())
        if err != nil {
            sendErrors.WithLabelValues(classify(err)).Inc()
        }
    },
})
```

Both hooks:

- Run **synchronously on the caller's goroutine** — keep them cheap (increment a counter, record a histogram); do I/O asynchronously if needed.
- Receive the resulting error (`nil` on success) and a non-negative duration.
- `OnDial` fires once per dial attempt, including failed ones. `OnSend` fires once per `Send` / `SendBulk` delivery (one copy per recipient in `SendBulk`).
- `OnSend` observes the **translated** error on cancellation — `context.Canceled` / `context.DeadlineExceeded`, not the underlying socket error.

## Error handling

### `Send`

`Send` returns the first error encountered, wrapped with a short message describing the stage:

- `mailer: host is required`, `mailer: unknown TLSMode …`, `mailer: MaxOpen must be non-negative …` — from `New`, at construction time.
- `mailer: closed` — the `Mailer` has been `Close`d.
- `mailer: message is nil`, `mailer: message has no recipients` — argument validation, before touching the pool.
- `auth: …`, `starttls: …`, `server does not support STARTTLS` — SMTP handshake stages.
- raw SMTP errors from `Mail`, `Rcpt`, `Data`, `Write`, `Close`.

Use `errors.Is` / `errors.As` to inspect `context.Canceled`, `context.DeadlineExceeded`, and any wrapped `net.Error`.

If the context is cancelled at **any** point — before `pool.get`, during the dial / greeting read inside `pool.get`, or during the SMTP dialogue — the raw socket error is translated to the caller's `ctx.Err()`, so `errors.Is(err, context.Canceled)` and `errors.Is(err, context.DeadlineExceeded)` work regardless of which phase was interrupted. `Config.OnSend`, if set, observes the translated error.

If an error occurs after the SMTP `DATA` command has been issued — for example, while rendering the body or writing it to the socket — the transaction is **aborted**, not committed: the connection is closed without sending the terminating `.\r\n`, so the server discards the buffered message (RFC 5321 §4.1.1.4). The caller sees the original error, and no partial or empty message is delivered. Closing the connection with a graceful `QUIT` in this state would send `QUIT` into the data stream and then block until the socket deadline expires; the library bypasses `QUIT` by closing the raw socket.

`Message` methods are safe on a nil receiver: `Bytes` returns an error, while `envelopeRecipients` and `forRecipient` return nil, so a nil `*Message` cannot panic the process.

### `SendBulk`

`SendBulk` aggregates per-recipient errors with `errors.Join`:

- If every delivery succeeds, it returns `nil`.
- If some deliveries fail, it returns a non-nil error whose `Error()` is the concatenation of all sub-errors. The order matches the order of the `recipients` slice.
- Use `errors.As(err, &multi)` with `interface{ Unwrap() []error }` (Go 1.20+), or iterate via `err.(interface{ Unwrap() []error }).Unwrap()`.
- Context cancellation is reflected per recipient: if `SendBulk` was interrupted, the remaining recipients have `context.Canceled` (or `context.DeadlineExceeded`) in their sub-errors.
- Each sub-error is wrapped as `send to <rcpt>: <cause>` for easy identification.

Example:

```go
err := m.SendBulk(ctx, msg, recipients)
if err != nil {
    type multiErr interface{ Unwrap() []error }
    if me, ok := err.(multiErr); ok {
        for _, e := range me.Unwrap() {
            log.Printf("delivery failed: %v", e)
        }
    } else {
        log.Printf("bulk send failed: %v", err)
    }
}
```

### Retries

The library does **not** implement retries. Retry semantics are policy decisions — transactional emails, notifications, and marketing campaigns have different requirements for backoff, jitter, and idempotency. Wrap `Send` / `SendBulk` in a retry loop at the caller level if needed.

## Testing

The package ships with three layers of tests.

### Unit tests (default)

```sh
go test ./...
```

Uses a mock `smtpClient`, so no network access is required. Covers:

- Header and MIME structure for all message shapes.
- `Date` and `Message-ID` rendering, sanitization of caller-supplied `MessageID` (CRLF injection), uniqueness across renders.
- RFC 2231 filename encoding and CRLF filename-injection protection.
- Custom header sanitization (including U+2028 / U+2029 / NEL).
- Nil-message and nil-receiver safety across `Send`, `SendBulk`, `Bytes`, `envelopeRecipients`, `forRecipient`.
- `TLSMode` validation, `TLSConfig` cloning, `ServerName` auto-fill and preservation.
- LOGIN case-insensitive challenges, XOAUTH2 `\x01` ACK on rejection.
- Pool: deadline refresh on reuse, idle close during `Noop`, close during dial, `NoopTimeout` cap and deadline restore, `MaxOpen` cap, idle reuse under `MaxOpen`, `MaxIdle` clamp, `probeAttempts` cap, ctx bind/release.
- `OnDial` and `OnSend` hooks for success and error paths.
- Error propagation from every stage of the SMTP dialogue.
- Abort path when `DATA` body write fails.

### Integration tests

```sh
go test -tags=integration ./integration_tests/...
```

Spins up a real in-process SMTP server (`github.com/emersion/go-smtp`) and exercises real TCP/TLS paths, including:

- plain send, STARTTLS, implicit TLS with a self-signed certificate,
- parallel bulk send,
- partial-bulk failure (one recipient rejected, the rest delivered),
- context cancellation before and during the dialogue,
- `MaxOpen` interaction with real sockets,
- idle reuse after a slot frees,
- `OnDial` and `OnSend` against a real server.

The server certificate is generated on the fly, so the test suite is fully self-contained.

### Fuzz tests

Fuzz targets are gated behind the `fuzz` build tag so they don't slow down the regular test suite. Run the seed corpus with:

```sh
go test -tags=fuzz ./fuzzing_tests/...
```

Run an actual fuzzing session for a specific target:

```sh
go test -tags=fuzz -run=^$ -fuzz=FuzzMessageRender -fuzztime=30s ./fuzzing_tests/
```

Two fuzz targets:

- `FuzzMessageRender` — feeds arbitrary `From`, `To`, `Subject`, `Text`, `HTML`. Invariants: no panic; if `Bytes` returns nil error, the output parses as RFC 5322; no unexpected headers appear.
- `FuzzMessageHeadersAndAttachment` — feeds arbitrary `Headers` key/value and `Attachment.Name`. Invariants: no panic; output parses; no unexpected headers beyond the caller-supplied one; no raw `\r\nBcc:` or `\r\nCc:` sequences appear in the rendered bytes.

## Notes and non-goals

- **Attachments are fully buffered in memory.** `Message.Bytes` renders the entire MIME document at once. This is fine for typical transactional emails but not for attachments measured in hundreds of megabytes. Streaming support would require a `WriteTo(io.Writer)` API.
- **No retry, no rate limiting, no bounce handling.** These are deliberately out of scope; the library is a transport, not an email delivery platform.
- **No built-in templating.** Rendered `Text` and `HTML` are expected; use whatever template engine fits your project.