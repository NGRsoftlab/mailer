# mailer

[![Go Reference](https://pkg.go.dev/badge/github.com/NGRsoftlab/mailer.svg)](https://pkg.go.dev/github.com/NGRsoftlab/mailer)
[![Tests](https://github.com/NGRsoftlab/mailer/actions/workflows/tests.yml/badge.svg)](https://github.com/NGRsoftlab/mailer/actions/workflows/tests.yml)
![Fuzzing](https://github.com/NGRsoftlab/mailer/actions/workflows/fuzz.yml/badge.svg)
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
- [Error handling](#error-handling)
- [Testing](#testing)
- [Notes and non-goals](#notes-and-non-goals)
- [License](#license)

## Features

- **Context-aware**: every send honors `context.Context` — timeouts, deadlines and cancellation propagate to the underlying network I/O.
- **Pluggable authentication**: PLAIN, LOGIN and XOAUTH2 are built in. The public `Auth` type is an alias for `smtp.Auth`, so any custom mechanism compatible with `net/smtp` works without adaptation.
- **Connection pool**: SMTP connections are reused across sends, checked for liveness via `NOOP` before being handed out, and transparently replaced on failure.
- **TLS everywhere**: plain SMTP with opportunistic STARTTLS, required STARTTLS, implicit TLS (SMTPS), and mutual TLS (mTLS) via `tls.Config.Certificates`.
- **MIME message builder**: plain text, HTML, `multipart/alternative`, `multipart/mixed` with attachments, custom headers, automatic base64 encoding, and header-injection protection.
- **Single and bulk send**: `Send` delivers a message to all recipients in a single SMTP transaction. `SendBulk` delivers personalized copies in parallel with a configurable concurrency limit.
- **No third-party runtime dependencies**: everything is built on the Go standard library. The only external dependency (`emersion/go-smtp`) is used in integration tests behind a build tag.

## Installation

```sh
go get github.com/NGRsoftlab/mailer
```

Requires Go 1.26.8 or newer.

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

| Field            | Type            | Default | Description                                                                 |
|------------------|-----------------|---------|-----------------------------------------------------------------------------|
| `Host`           | `string`        | —       | SMTP server hostname. Required.                                             |
| `Port`           | `int`           | `587`   | SMTP server port.                                                           |
| `Auth`           | `mailer.Auth`   | `nil`   | Auth mechanism. If `nil`, `AUTH` is not attempted.                          |
| `TLSMode`        | `TLSMode`       | `TLSNone` | TLS strategy (see [TLS, SSL and mTLS](#tls-ssl-and-mtls)).                 |
| `TLSConfig`      | `*tls.Config`   | `nil`   | TLS settings. Supply `Certificates` for mTLS.                               |
| `MaxIdle`        | `int`           | `5`     | Maximum number of idle pooled connections.                                  |
| `MaxConcurrency` | `int`           | `4`     | Maximum parallel deliveries in `SendBulk`.                                  |
| `Timeout`        | `time.Duration` | `30s`   | Per-send timeout. Applied only if the incoming context has no deadline.     |

`Config` is validated and normalized inside `New`. Invalid input (empty `Host`) returns an error; zero values of optional fields are replaced with defaults.

## Sending messages

### Single-transaction send

`Send` delivers a message to *all* of its envelope recipients (`To` + `Cc` + `Bcc`) in one SMTP transaction. This is the right choice when you want recipients to see each other, or when using `Bcc` for a mailing list.

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
- all other fields (`From`, `Subject`, `Text`, `HTML`, `Attachments`, `Headers`) are preserved.

This guarantees that recipients do not see each other, which is the common requirement for transactional and notification emails. If you need recipients to see each other, use `Send` with the full recipient list.

Deliveries run concurrently, bounded by `Config.MaxConcurrency`. Failures are collected for every recipient — one failed delivery does not abort the rest.

## Message building

`Message` describes a MIME email. The `Bytes` method renders it as a valid RFC 5322 document. The builder automatically chooses the correct MIME structure:

| Input                                   | Output structure                            |
|-----------------------------------------|---------------------------------------------|
| `Text` only                             | `text/plain`                                |
| `HTML` only                             | `text/html`                                 |
| `Text` + `HTML`                         | `multipart/alternative`                     |
| Any of the above + attachments          | `multipart/mixed` wrapping the above         |
| Neither `Text` nor `HTML`               | empty `text/plain` body (structurally valid) |

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

**Styling**: there is no separate styling API. HTML email styling is just HTML — inline CSS on the `HTML` field works. If you need templating, render the template to a string before setting `HTML` and `Text`.

**Signature**: likewise, there is no dedicated signature field. Append the signature to `Text` and/or `HTML` before setting the message. This keeps the API minimal and avoids forcing a particular signature format on callers.

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

Attachments are base64-encoded and sent as separate MIME parts with `Content-Disposition: attachment; filename=...`. Non-ASCII filenames are RFC 2047 encoded.

### Custom headers

```go
msg.Headers = map[string]string{
    "Reply-To":     "support@example.com",
    "X-Campaign-ID": "welcome-2024",
}
```

Safety rules enforced by the builder:
- Header names containing `:` or CR/LF are silently dropped.
- Header values have CR and LF stripped, preventing header injection.
- Well-known headers (`From`, `To`, `Cc`, `Subject`, `MIME-Version`) are set from the dedicated `Message` fields and should not be overridden via `Headers`.

## Authentication

`mailer.Auth` is a type alias for `net/smtp.Auth`. That means the following work out of the box:

### PLAIN

```go
Auth: mailer.PlainAuth("", "username", "password", "smtp.example.com")
```

### LOGIN

```go
Auth: mailer.LoginAuth("username", "password")
```

### XOAUTH2

```go
Auth: mailer.XOAuth2Auth("user@example.com", oauth2AccessToken)
```

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
    Host:    "smtp.example.com",
    Port:    587,
    TLSMode: mailer.TLSNone,
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

If the server does not advertise `STARTTLS`, `New`/`Send` fails. This is the recommended mode for internet-facing SMTP on port 587.

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

If `TLSConfig` is `nil`, a minimal one is created automatically with `ServerName` set to `Config.Host`. Always supply your own config in production — in particular, do not set `InsecureSkipVerify: true` outside of tests.

## Connection pooling

Each `Mailer` owns a pool of SMTP connections. The pool works as follows:

1. On `Send`/`SendBulk`, the mailer asks the pool for a connection.
2. If the pool has an idle connection, its liveness is verified with `NOOP`.
  - If `NOOP` succeeds, the connection is reused.
  - If `NOOP` fails, the connection is closed and the next idle one is tried.
3. If no live idle connection is available, a new one is dialed.
4. After a successful send, the connection is returned to the pool (or closed if the pool is at `MaxIdle`).
5. On any error during send, the connection is discarded — a connection that has seen an error may be in an undefined state.

This gives transparent reconnect behavior: dead connections are detected and replaced without any background goroutines.

Notes:
- `MaxIdle` bounds the number of *idle* connections, not the total. Under high concurrency, active connections can exceed `MaxIdle`; they are closed rather than pooled when the send completes.
- The pool is safe for concurrent use.
- `Close` on the `Mailer` drains the pool. Call it after all in-flight sends have returned — typically via `defer` in `main`.

## Parallel bulk sending

`SendBulk` runs deliveries in parallel with two mechanisms:

- **Semaphore** — bounds the number of in-flight goroutines to `Config.MaxConcurrency`. Defaults to 4. Set it to the value your SMTP provider tolerates (most providers rate-limit aggressively; 4–8 is usually safe).
- **Context cancellation** — before starting each delivery, `SendBulk` checks `ctx.Err()`. If the context is already canceled, the remaining recipients are immediately marked as failed without attempting a network round-trip. In-flight sends are cancelled via the context deadline propagated to the socket.

Ordering is **not** guaranteed — recipients are delivered in a non-deterministic order. If ordering matters, either sort the recipient list at the caller and process the resulting errors in the corresponding order, or call `Send` sequentially.

Considerations:
- The SMTP server is usually the bottleneck. Increasing `MaxConcurrency` beyond what the server allows will yield rate-limit errors (typically `421` / `450`), not throughput.
- When `MaxConcurrency` is higher than `MaxIdle`, connections are created on demand and closed after each send — that is correct, but produces more TLS handshakes. Match `MaxIdle` to `MaxConcurrency` if you have long-running pools.

## Error handling

### `Send`

`Send` returns the first error encountered, wrapped with a short message describing the stage:

- `auth: ...`
- `starttls: ...`
- `server does not support STARTTLS`
- raw SMTP errors from `Mail`, `Rcpt`, `Data`, `Write`, `Close`

Use `errors.Is` / `errors.As` to inspect `context.Canceled`, `context.DeadlineExceeded`, and any wrapped `net.Error`.

### `SendBulk`

`SendBulk` aggregates per-recipient errors with `errors.Join`, so:

- If every delivery succeeds, it returns `nil`.
- If some deliveries fail, it returns a non-nil error whose `Error()` is the concatenation of all sub-errors. The order matches the order of the `recipients` slice.
- Use `errors.As(err, &multi)` with `interface{ Unwrap() []error }` (Go 1.20+) or iterate via `err.(interface{ Unwrap() []error }).Unwrap()` to inspect individual failures.
- Context cancellation is reflected per recipient: if `SendBulk` was interrupted, the remaining recipients have `context.Canceled` (or `context.DeadlineExceeded`) in their sub-errors.

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
- Header injection protection.
- Correct handling of `AUTH` across connection reuse.
- Pool reconnect logic on dead connections.
- `SendBulk` parallelism and concurrency limiting.
- Error propagation from every stage of the SMTP dialogue.

### Integration tests

```sh
go test -tags=integration ./...
```

Spins up a real in-process SMTP server (`github.com/emersion/go-smtp`) and exercises real TCP/TLS paths, including:

- plain send,
- PLAIN auth,
- STARTTLS,
- implicit TLS with a self-signed certificate,
- parallel bulk send,
- context cancellation.

The server certificate is generated on the fly, so the test suite is fully self-contained.

### Fuzz tests

```sh
go test -fuzz=FuzzMessageBytes -fuzztime=30s .
```

`FuzzMessageBytes` feeds arbitrary `from`, `subject`, `text`, `html`, attachment names, and attachment bodies into `Message.Bytes`, then parses the result with `net/mail` and, for multipart messages, with `mime/multipart`. The invariant is: whatever goes in, the output must parse as valid MIME and must not contain injected headers.

## Notes and non-goals

- **Attachments are fully buffered in memory.** `Message.Bytes` renders the entire MIME document at once. This is fine for typical transactional emails but not for attachments measured in hundreds of megabytes. Streaming support would require a `WriteTo(io.Writer)` API.
- **No retry, no rate limiting, no bounce handling.** These are deliberately out of scope; the library is a transport, not an email delivery platform.
- **No built-in templating.** Rendered `Text` and `HTML` are expected; use whatever template engine fits your project.
- **No background health checks.** Connection liveness is verified lazily via `NOOP` at the moment a connection is reused. This keeps the pool simple and avoids long-lived goroutines.
- **`Close` must be called.** The mailer does not leak goroutines on its own, but it holds SMTP connections; `defer m.Close()` is the expected pattern.
- **Recipients are not validated.** Neither `Send` nor `SendBulk` parses addresses — the SMTP server is the authority. Invalid addresses surface as `Rcpt` errors and are reflected in the returned error.

## License

MIT.