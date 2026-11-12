# Examples

Runnable programs that demonstrate the main use cases of
[`github.com/NGRsoftlab/mailer`](https://pkg.go.dev/github.com/NGRsoftlab/mailer).

All examples live in the library module and are built by CI, so they stay in
sync with the public API.

## Prerequisites

- Go 1.26+
- An SMTP endpoint (real server, MailHog, GreenMail, etc.)

## Run

Each example reads its configuration from environment variables. From the
repository root:

    export SMTP_HOST=localhost SMTP_PORT=1025 \
           SMTP_USER= SMTP_PASS= \
           SMTP_FROM=from@test.local SMTP_TO=to@test.local
    go run ./examples/simple

| Example | What it shows |
|---|---|
| [simple](simple) | Plain-text message over STARTTLS |
| [html_multipart](html_multipart) | `multipart/alternative` with text + HTML |
| [attachments](attachments) | In-memory attachment via `Attach` |
| [streaming_attachment](streaming_attachment) | `AttachReader` + `WriteTo` streaming |
| [mtls](mtls) | Mutual TLS with client certificates |
| [pool](pool) | Concurrent sends through one client |