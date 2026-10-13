package imap

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"net"
	"net/mail"
	"net/smtp"
	"strconv"
	"time"

	gomail "github.com/emersion/go-message/mail"

	"github.com/ipedrazas/sbx/internal/connector"
)

// smtpSend delivers a message (overridable in tests). Port 465 uses implicit
// TLS; any other port requires STARTTLS — matching lettre's relay /
// starttls_relay transports.
var smtpSend = func(ctx context.Context, cfg Config, from string, rcpts []string, msg []byte) error {
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	tlsCfg := smtpTLSConfig(cfg.SMTPHost)
	var conn net.Conn
	var err error
	if cfg.SMTPPort == 465 {
		conn, err = (&tls.Dialer{NetDialer: dialer, Config: tlsCfg}).DialContext(ctx, "tcp", net.JoinHostPort(cfg.SMTPHost, "465"))
	} else {
		conn, err = dialer.DialContext(ctx, "tcp", net.JoinHostPort(cfg.SMTPHost, strconv.Itoa(int(cfg.SMTPPort))))
	}
	if err != nil {
		return err
	}
	c, err := smtp.NewClient(conn, cfg.SMTPHost)
	if err != nil {
		conn.Close()
		return err
	}
	defer c.Close()
	if cfg.SMTPPort != 465 {
		if ok, _ := c.Extension("STARTTLS"); !ok {
			return errors.New("server does not support STARTTLS")
		}
		if err := c.StartTLS(tlsCfg); err != nil {
			return err
		}
	}
	if err := c.Auth(smtp.PlainAuth("", cfg.Username, cfg.Password, cfg.SMTPHost)); err != nil {
		return err
	}
	if err := c.Mail(from); err != nil {
		return err
	}
	for _, r := range rcpts {
		if err := c.Rcpt(r); err != nil {
			return err
		}
	}
	w, err := c.Data()
	if err != nil {
		return err
	}
	if _, err := w.Write(msg); err != nil {
		return err
	}
	if err := w.Close(); err != nil {
		return err
	}
	return c.Quit()
}

var smtpTLSConfig = func(host string) *tls.Config { return &tls.Config{ServerName: host} }

func toGoMail(a *mail.Address) *gomail.Address {
	return &gomail.Address{Name: a.Name, Address: a.Address}
}

// buildEmail validates addresses and renders the RFC 5322 message. Bcc
// recipients are envelope-only (no header), like lettre.
func buildEmail(cfg Config, req *sendEmailParams) (from string, rcpts []string, msg []byte, err error) {
	fromAddr, perr := mail.ParseAddress(cfg.Username)
	if perr != nil {
		return "", nil, nil, connector.Config("invalid from address: %v", perr)
	}
	toAddr, perr := mail.ParseAddress(req.To)
	if perr != nil {
		return "", nil, nil, connector.Query("invalid to address: %v", perr)
	}
	var h gomail.Header
	h.SetDate(time.Now())
	h.SetAddressList("From", []*gomail.Address{toGoMail(fromAddr)})
	h.SetAddressList("To", []*gomail.Address{toGoMail(toAddr)})
	rcpts = []string{toAddr.Address}
	if req.Cc != nil {
		cc, perr := mail.ParseAddress(*req.Cc)
		if perr != nil {
			return "", nil, nil, connector.Query("invalid cc: %v", perr)
		}
		h.SetAddressList("Cc", []*gomail.Address{toGoMail(cc)})
		rcpts = append(rcpts, cc.Address)
	}
	if req.Bcc != nil {
		bcc, perr := mail.ParseAddress(*req.Bcc)
		if perr != nil {
			return "", nil, nil, connector.Query("invalid bcc: %v", perr)
		}
		rcpts = append(rcpts, bcc.Address)
	}
	h.SetSubject(req.Subject)
	if perr := h.GenerateMessageID(); perr != nil {
		return "", nil, nil, connector.Query("build email: %v", perr)
	}
	h.SetContentType("text/plain", map[string]string{"charset": "utf-8"})
	h.Set("Content-Transfer-Encoding", "quoted-printable")

	var buf bytes.Buffer
	w, perr := gomail.CreateSingleInlineWriter(&buf, h)
	if perr == nil {
		_, perr = io.WriteString(w, req.Body)
		if cerr := w.Close(); perr == nil {
			perr = cerr
		}
	}
	if perr != nil {
		return "", nil, nil, connector.Query("build email: %v", perr)
	}
	return fromAddr.Address, rcpts, buf.Bytes(), nil
}

func sendEmailSMTP(ctx context.Context, cfg Config, req *sendEmailParams) (any, error) {
	from, rcpts, msg, err := buildEmail(cfg, req)
	if err != nil {
		return nil, err
	}
	if err := smtpSend(ctx, cfg, from, rcpts, msg); err != nil {
		return nil, connector.Query("SMTP send: %v", err)
	}
	return map[string]any{"status": "sent"}, nil
}
