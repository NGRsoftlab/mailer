// Copyright 2020-2026 NGR Softlab
package mailer

import (
	"fmt"
	"net/smtp"
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

func (a *loginAuth) Next(fromServer []byte, more bool) ([]byte, error) {
	if !more {
		return nil, nil
	}
	switch string(fromServer) {
	case "Username:":
		return []byte(a.username), nil
	case "Password:":
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

func (a *xoauth2Auth) Next(_ []byte, more bool) ([]byte, error) {
	if more {
		return nil, fmt.Errorf("xoauth2: unexpected continuation")
	}
	return nil, nil
}
