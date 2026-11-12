// Copyright 2020-2026 NGR Softlab
package mailer

import (
	"context"
	"crypto/tls"
	"fmt"
	"net"
	"net/smtp"
)

type TLSMode int

const (
	TLSNone     TLSMode = iota // plain SMTP, opportunistically STARTTLS
	TLSStart                   // require STARTTLS
	TLSImplicit                // implicit TLS (SMTPS, e.g. port 465)
)

type dialConfig struct {
	addr      string
	host      string
	tlsConfig *tls.Config
	mode      TLSMode
}

// dialContext establishes an SMTP connection honoring TLS mode, mTLS
// certificates (via tlsConfig.Certificates), and context deadline.
func dialContext(ctx context.Context, cfg dialConfig) (smtpClient, error) {
	if cfg.tlsConfig == nil {
		cfg.tlsConfig = &tls.Config{ServerName: cfg.host}
	}

	d := &net.Dialer{}
	conn, err := d.DialContext(ctx, "tcp", cfg.addr)
	if err != nil {
		return nil, err
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = conn.SetDeadline(deadline)
	}

	if cfg.mode == TLSImplicit {
		// tls.DialWithDialer dials without a context and blocks the
		// handshake until the deadline, ignoring context cancellation.
		// Wrapping a DialContext result with HandshakeContext honors both.
		tlsConn := tls.Client(conn, cfg.tlsConfig)
		if err := tlsConn.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("tls handshake: %w", err)
		}
		conn = tlsConn
	}

	client, err := smtp.NewClient(conn, cfg.host)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}

	if cfg.mode != TLSImplicit {
		if ok, _ := client.Extension("STARTTLS"); ok {
			if err := client.StartTLS(cfg.tlsConfig); err != nil {
				_ = client.Close()
				return nil, fmt.Errorf("starttls: %w", err)
			}
		} else if cfg.mode == TLSStart {
			_ = client.Close()
			return nil, fmt.Errorf("server does not support STARTTLS")
		}
	}

	return client, nil
}
