// Copyright 2020-2026 NGR Softlab
package mailer

import (
	"fmt"
	"net/smtp"
	"strings"
)

// Auth is an alias to the standard SMTP auth interface, so any custom
// implementation compatible with net/smtp can be plugged in without adaptation.
type Auth = smtp.Auth

// --- PLAIN --- ///

// PlainAuth returns a PLAIN auth mechanism.
func PlainAuth(identity, username, password, host string) Auth {
	return smtp.PlainAuth(identity, username, password, host)
}

// --- LOGIN --- ///

type loginAuth struct {
	username, password string
}

// LoginAuth returns a LOGIN auth mechanism.
func LoginAuth(username, password string) Auth {
	return &loginAuth{username: username, password: password}
}

func (a *loginAuth) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	return "LOGIN", nil, nil
}

// Next answers the server's LOGIN challenges. The comparison is
// case-insensitive and tolerant of surrounding whitespace: the LOGIN
// mechanism is not standardised (it predates RFC 4616), so real servers
// vary widely — Exchange sends "Username:" with a capital U, some Unix
// MTAs send "username:" or "Username:" with a trailing space. Rejecting
// those would make the library unusable against otherwise-valid servers.
func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	challenge := strings.TrimSpace(string(fromServer))
	switch {
	case strings.EqualFold(challenge, "Username:"):
		return []byte(a.username), nil
	case strings.EqualFold(challenge, "Password:"):
		return []byte(a.password), nil
	default:
		return nil, fmt.Errorf("login auth: unexpected challenge %q", fromServer)
	}
}

// --- XOAUTH2 --- ///

type xoauth2Auth struct {
	username, token string
}

// XOAuth2Auth returns an XOAUTH2 auth mechanism.
func XOAuth2Auth(username, token string) Auth {
	return &xoauth2Auth{username: username, token: token}
}

func (a *xoauth2Auth) Start(_ *smtp.ServerInfo) (string, []byte, error) {
	resp := fmt.Sprintf("user=%s\x01auth=Bearer %s\x01\x01", a.username, a.token)
	return "XOAUTH2", []byte(resp), nil
}

// Next handles the server's reply after Start. A "more=true" continuation
// means the server rejected the token: it has sent a base64-encoded JSON
// error body and is waiting for the client to acknowledge it with an
// empty \x01 before closing the session. Without that ACK the server
// keeps the connection half-open and net/smtp.Client.Auth reports the
// failure as a generic I/O error rather than the server's own message.
func (a *xoauth2Auth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return []byte("\x01"), nil
	}
	return nil, nil
}
