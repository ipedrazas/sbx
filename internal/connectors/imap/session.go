package imap

import (
	"context"
	"crypto/tls"
	"net"
	"strconv"
	"time"

	goimap "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/client"
	"github.com/emersion/go-imap/commands"
	"github.com/emersion/go-imap/responses"
	"github.com/emersion/go-message/charset"

	"github.com/ipedrazas/sbx/internal/connector"
)

func init() {
	// Decode RFC 2047 encoded-words in envelope fields for any charset.
	goimap.CharsetReader = charset.Reader
}

// session is the IMAP surface the connector needs (faked in tests).
type session interface {
	Select(mailbox string) error
	UidSearch(query string) ([]uint32, error)
	UidFetch(uidSet string) ([]*goimap.Message, error)
	UidStore(uidSet string, item goimap.StoreItem, flag string) error
	UidCopy(uidSet, dest string) error
	Expunge() error
	Logout() error
}

// imapTLSConfig builds the TLS config for a host (overridable in tests).
var imapTLSConfig = func(host string) *tls.Config { return &tls.Config{ServerName: host} }

// dialIMAP connects over implicit TLS and logs in.
var dialIMAP = func(ctx context.Context, cfg Config) (session, error) {
	addr := net.JoinHostPort(cfg.ImapHost, strconv.Itoa(int(cfg.ImapPort)))
	dialer := &net.Dialer{Timeout: 30 * time.Second}
	if dl, ok := ctx.Deadline(); ok {
		dialer.Deadline = dl
	}
	c, err := client.DialWithDialerTLS(dialer, addr, imapTLSConfig(cfg.ImapHost))
	if err != nil {
		return nil, connector.Connection("IMAP connect: %v", err)
	}
	if err := c.Login(cfg.Username, cfg.Password); err != nil {
		_ = c.Terminate()
		return nil, connector.Auth("IMAP login: %v", err)
	}
	return &clientSession{c: c}, nil
}

type clientSession struct{ c *client.Client }

func (s *clientSession) Select(mailbox string) error {
	_, err := s.c.Select(mailbox, false)
	return err
}

// rawCommand sends a pre-built argument string verbatim (the Rust connector
// builds SEARCH criteria as text).
type rawCommand struct {
	name string
	args string
}

func (r rawCommand) Command() *goimap.Command {
	return &goimap.Command{Name: r.name, Arguments: []interface{}{goimap.RawString(r.args)}}
}

func (s *clientSession) UidSearch(query string) ([]uint32, error) {
	res := &responses.Search{}
	status, err := s.c.Execute(&commands.Uid{Cmd: rawCommand{"SEARCH", query}}, res)
	if err != nil {
		return nil, err
	}
	if err := status.Err(); err != nil {
		return nil, err
	}
	return res.Ids, nil
}

func (s *clientSession) UidFetch(uidSet string) ([]*goimap.Message, error) {
	seq, err := goimap.ParseSeqSet(uidSet)
	if err != nil {
		return nil, err
	}
	ch := make(chan *goimap.Message, 16)
	done := make(chan error, 1)
	go func() {
		done <- s.c.UidFetch(seq, []goimap.FetchItem{goimap.FetchUid, goimap.FetchEnvelope, goimap.FetchFlags}, ch)
	}()
	var msgs []*goimap.Message
	for m := range ch {
		msgs = append(msgs, m)
	}
	return msgs, <-done
}

func (s *clientSession) UidStore(uidSet string, item goimap.StoreItem, flag string) error {
	seq, err := goimap.ParseSeqSet(uidSet)
	if err != nil {
		return err
	}
	return s.c.UidStore(seq, item, []interface{}{flag}, nil)
}

func (s *clientSession) UidCopy(uidSet, dest string) error {
	seq, err := goimap.ParseSeqSet(uidSet)
	if err != nil {
		return err
	}
	return s.c.UidCopy(seq, dest)
}

func (s *clientSession) Expunge() error { return s.c.Expunge(nil) }

func (s *clientSession) Logout() error { return s.c.Logout() }
