# Examples

Runnable examples for `github.com/NGRsoftlab/mailer`. Each program is
self-contained and can be executed with `go run ./examples/<name>`.

## Configuration

All examples read SMTP settings from environment variables:

| Variable          | Description                          | Example           |
|-------------------|--------------------------------------|-------------------|
| `SMTP_HOST`       | SMTP server hostname                 | smtp.example.com  |
| `SMTP_PORT`       | SMTP server port                     | 587               |
| `SMTP_FROM`       | Sender address                       | sender@example.com|
| `SMTP_USER`       | Auth username (auth example only)    | user@example.com  |
| `SMTP_PASS`       | Auth password (auth example only)    | secret            |
| `SMTP_OAUTH_TOKEN`| OAuth2 access token (XOAuth2 only)   | ya29...           |
| `AUTH_MECH`       | `plain` / `login` / `xoauth2`        | plain             |
| `TLS_MODE`        | `none` / `starttls` / `implicit`     | starttls          |

## Available examples

- [basic](./basic) — plain-text message
- [html](./html) — HTML message with plain-text fallback
- [attachments](./attachments) — multipart/mixed with attachments
- [auth](./auth) — PLAIN, LOGIN and XOAUTH2 authentication
- [tls](./tls) — opportunistic STARTTLS, mandatory STARTTLS and implicit TLS
- [bulk](./bulk) — personalized bulk send with bounded concurrency

Run any example:

    SMTP_HOST=smtp.example.com \
    SMTP_PORT=587 \
    SMTP_FROM=sender@example.com \
    go run ./examples/basic

## Note on duplication

Examples intentionally duplicate a few lines of setup boilerplate so that
each file is self-contained and copy-pasteable. For real projects, extract
shared configuration into your own helper.