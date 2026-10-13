package imap

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"net"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	goimap "github.com/emersion/go-imap"
	"github.com/emersion/go-imap/backend/memory"
	"github.com/emersion/go-imap/server"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
)

func testConfig() Config {
	return Config{
		ImapHost: "imap.example.com", ImapPort: 993, SMTPHost: "smtp.example.com", SMTPPort: 587,
		Username: "test@example.com", Password: "test", Mailbox: "INBOX",
	}
}

// ── ported Rust unit tests ─────────────────────────────────────

func TestSimulateSendEmailIsDeterministic(t *testing.T) {
	c := New(0, testConfig())
	ctx := context.Background()
	a := map[string]any{"to": "a@b.c", "subject": "Hi", "body": "hello"}
	b := map[string]any{"to": "a@b.c", "subject": "Hi", "body": "hello"}
	cc := map[string]any{"to": "z@b.c", "subject": "Hi", "body": "hello"}
	ra, err := c.SimulateAction(ctx, "send_email", a)
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := c.SimulateAction(ctx, "send_email", b)
	rc, _ := c.SimulateAction(ctx, "send_email", cc)
	ma, mb, mc := ra.(map[string]any), rb.(map[string]any), rc.(map[string]any)
	if ma["message_id"] != mb["message_id"] || ma["message_id"] == mc["message_id"] {
		t.Fatal(ma, mb, mc)
	}
	if !strings.HasSuffix(ma["message_id"].(string), "@sim.imap") || ma["status"] != "sent" {
		t.Fatal(ma)
	}
}

func TestSimulateRejectsInvalidParams(t *testing.T) {
	c := New(0, testConfig())
	_, err := c.SimulateAction(context.Background(), "send_email", map[string]any{"subject": "x", "body": "y"})
	var ce *connector.Error
	if !errors.As(err, &ce) || ce.Kind != connector.ErrQuery || ce.Message != "invalid send_email params: missing field `to`" {
		t.Fatal(err)
	}
	_, err = c.SimulateAction(context.Background(), "mark_read", nil)
	if !errors.As(err, &ce) || ce.Kind != connector.ErrUnsupported {
		t.Fatal(err)
	}
}

func TestSyntheticRows(t *testing.T) {
	c := New(0, testConfig())
	rows, ok := c.GenerateSyntheticRows(nil, 5)
	if !ok || len(rows) != 5 {
		t.Fatal(len(rows))
	}
	for _, r := range rows {
		for _, k := range []string{"id", "from", "to", "subject", "date", "flags", "message_id", "snippet"} {
			if _, ok := r[k]; !ok {
				t.Fatalf("missing %s", k)
			}
		}
	}
}

// Golden values computed with the Rust implementation (DefaultHasher).
func TestSynthHashMatchesRust(t *testing.T) {
	cases := []struct {
		row  int64
		salt uint64
		want uint64
	}{
		{0, 0x494D4150, 6853306257763552612},
		{1, 1, 7912899488978770657},
		{7, 5, 6812789931433920842},
		{3, 4, 11819013553020960951},
		{-1, 0, 6235750831517777511},
	}
	for _, c := range cases {
		if got := synthHash(c.row, c.salt); got != c.want {
			t.Fatalf("synthHash(%d,%d)=%d want %d", c.row, c.salt, got, c.want)
		}
	}
	rows := generateIMAPRows(3)
	want := []struct{ id, from, subject, mid, date string }{
		{"3", "Carol Diaz <carol.diaz@startup.dev>", synthSubjects[0], "<b71d6a24ef50eb1c@mail.example.com>", "2025-04-21T08:00:00+00:00"},
		{"2", "GitHub <notifications@github.com>", synthSubjects[7], "<8243285b50e8fbff@mail.example.com>", "2025-04-21T02:31:00+00:00"},
		{"1", "GitHub <notifications@github.com>", synthSubjects[2], "<ba5c8f01785871a6@mail.example.com>", "2025-04-21T00:13:00+00:00"},
	}
	for i, w := range want {
		r := rows[i]
		if r["id"] != w.id || r["from"] != w.from || r["subject"] != w.subject || r["message_id"] != w.mid || r["date"] != w.date {
			t.Fatalf("row %d: %+v", i, r)
		}
	}
	if rows[0]["snippet"] != synthSnippets[6] || len(rows[0]["flags"].([]any)) != 1 {
		t.Fatalf("%+v", rows[0])
	}
	if len(generateIMAPRows(0)) != 0 || len(generateIMAPRows(-1)) != 0 {
		t.Fatal("empty")
	}
	// tail bytes path of the generic SipHash
	if sipHash13(0, 0, []byte{1, 2, 3}) == sipHash13(0, 0, []byte{1, 2}) {
		t.Fatal("tail")
	}
}

func TestBuildSearch(t *testing.T) {
	q, mem := buildIMAPSearch(nil, nil)
	if q != "ALL" || len(mem) != 0 {
		t.Fatal(q, mem)
	}
	filters := []models.DatasetFilter{
		{FieldName: "from", Operator: models.OpContains, Value: "alice"},
		{FieldName: "to", Operator: models.OpEquals, Value: "bob"},
		{FieldName: "subject", Operator: models.OpContains, Value: "hi"},
		{FieldName: "subject", Operator: models.OpEquals, Value: "exact"},
		{FieldName: "flags", Operator: models.OpContains, Value: "Seen"},
	}
	search := "hello"
	q, mem = buildIMAPSearch(filters, &search)
	if q != `FROM "alice" TO "bob" SUBJECT "hi" TEXT "hello"` || len(mem) != 2 {
		t.Fatal(q, mem)
	}
}

func TestIntrospect(t *testing.T) {
	s, _ := New(0, testConfig()).Introspect(context.Background())
	if len(s.Fields) != 8 || s.Fields[5].FieldType != connector.FieldDateTime || *s.Fields[0].Description != "Message UID" {
		t.Fatalf("%+v", s)
	}
}

func TestDatasourceConfig(t *testing.T) {
	c, err := ParseDatasourceConfig(map[string]any{"imap_host": "imap.x.com", "username": "u", "password_ref": "r"})
	if err != nil || c.ImapPort != 993 || c.SMTPPort != 587 || c.SMTPHost != nil {
		t.Fatal(c, err)
	}
	if c.DefaultSMTPHost() != "smtp.x.com" {
		t.Fatal(c.DefaultSMTPHost())
	}
	h := "mail.x.com"
	c.SMTPHost = &h
	if c.DefaultSMTPHost() != h {
		t.Fatal("explicit smtp host")
	}
	if _, err := ParseDatasourceConfig(map[string]any{"imap_host": "x", "username": "u"}); err == nil || err.Error() != "missing field `password_ref`" {
		t.Fatal(err)
	}
	var d DatasourceConfig
	if err := json.Unmarshal([]byte(`[]`), &d); err == nil {
		t.Fatal("expected error")
	}
}

func TestParseSendEmailParams(t *testing.T) {
	p, err := parseSendEmailParams(map[string]any{"to": "a@b", "subject": "s", "body": "b", "cc": "c@d", "bcc": nil})
	if err != nil || *p.Cc != "c@d" || p.Bcc != nil {
		t.Fatal(p, err)
	}
	for in, want := range map[string]map[string]any{
		"invalid type: number `1`, expected a string":      {"to": json.Number("1"), "subject": "s", "body": "b"},
		"invalid type: boolean `true`, expected a string":  {"to": "a", "subject": true, "body": "b"},
		"invalid type: sequence, expected a string":        {"to": "a", "subject": "s", "body": "b", "cc": []any{}},
		"invalid type: map, expected a string":             {"to": "a", "subject": "s", "body": "b", "bcc": map[string]any{}},
		"missing field `body`":                             {"to": "a", "subject": "s"},
		"invalid type: null, expected a string":            {"to": nil, "subject": "s", "body": "b"},
		"invalid type: value, expected a string":           {"to": struct{}{}, "subject": "s", "body": "b"},
		"invalid type: number `2.5`, expected a string":    {"to": 2.5, "subject": "s", "body": "b"},
		"invalid type: boolean `false`, expected a string": {"to": "a", "subject": "s", "body": false},
		"invalid type: number `3`, expected a string":      {"to": "a", "subject": "s", "body": 3},
		"invalid type: number `4`, expected a string":      {"to": "a", "subject": "s", "body": int64(4)},
	} {
		_, err := parseSendEmailParams(want)
		if err == nil || err.Error() != in {
			t.Fatalf("%v: got %v want %s", want, err, in)
		}
	}
}

// ── fake session ───────────────────────────────────────────────

type fakeSession struct {
	log       []string
	uids      []uint32
	msgs      []*goimap.Message
	failOn    string
	searchArg string
}

func (f *fakeSession) fail(op string) error {
	f.log = append(f.log, op)
	if f.failOn == op {
		return errors.New(op + " failed")
	}
	return nil
}
func (f *fakeSession) Select(string) error { return f.fail("select") }
func (f *fakeSession) UidSearch(q string) ([]uint32, error) {
	f.searchArg = q
	return f.uids, f.fail("search")
}
func (f *fakeSession) UidFetch(set string) ([]*goimap.Message, error) {
	f.log = append(f.log, "fetch "+set)
	if f.failOn == "fetch" {
		return nil, errors.New("fetch failed")
	}
	return f.msgs, nil
}
func (f *fakeSession) UidStore(set string, item goimap.StoreItem, flag string) error {
	return f.fail("store " + set + " " + string(item) + " " + flag)
}
func (f *fakeSession) UidCopy(set, dest string) error { return f.fail("copy " + set + " " + dest) }
func (f *fakeSession) Expunge() error                 { return f.fail("expunge") }
func (f *fakeSession) Logout() error                  { f.log = append(f.log, "logout"); return nil }

func withFake(t *testing.T, f *fakeSession, dialErr error) {
	t.Helper()
	orig := dialIMAP
	dialIMAP = func(context.Context, Config) (session, error) {
		if dialErr != nil {
			return nil, dialErr
		}
		return f, nil
	}
	t.Cleanup(func() { dialIMAP = orig })
}

func msg(uid uint32, from, subject string, flags ...string) *goimap.Message {
	return &goimap.Message{
		Uid:   uid,
		Flags: flags,
		Envelope: &goimap.Envelope{
			Date:      time.Date(2024, 5, 1, 10, 0, 0, 0, time.UTC),
			Subject:   subject,
			From:      []*goimap.Address{{PersonalName: "Al", MailboxName: from, HostName: "x.com"}},
			To:        []*goimap.Address{{MailboxName: "me", HostName: "y.com"}, {MailboxName: "local"}},
			MessageId: "<" + strconv.Itoa(int(uid)) + "@x>",
		},
	}
}

func TestQueryPagingAndFilters(t *testing.T) {
	f := &fakeSession{uids: []uint32{1, 5, 3, 9, 7}, msgs: []*goimap.Message{msg(7, "al", "hello", `\Seen`), msg(5, "bo", "bye")}}
	withFake(t, f, nil)
	c := New(1, testConfig())
	p := connector.DefaultQueryParams()
	p.Limit, p.Offset = 2, 1
	p.Filters = []models.DatasetFilter{{FieldName: "subject", Operator: models.OpEquals, Value: "hello"}}
	env, err := c.Query(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	if f.searchArg != "ALL" || !slicesContains(f.log, "fetch 7,5") {
		t.Fatal(f.searchArg, f.log)
	}
	if env.Meta.Total != 5 || len(env.Items) != 1 {
		t.Fatalf("%+v", env)
	}
	it := env.Items[0]
	if it["id"] != "7" || it["from"] != "Al <al@x.com>" || it["to"] != "me@y.com, local" || it["date"] != "Wed, 01 May 2024 10:00:00 +0000" ||
		it["message_id"] != "<7@x>" || it["snippet"] != "" || it["flags"].([]any)[0] != `\Seen` {
		t.Fatalf("%+v", it)
	}

	// offset past the end → no fetch
	f.log = nil
	p.Offset = 10
	env, err = c.Query(context.Background(), &p)
	if err != nil || len(env.Items) != 0 || slicesContains(f.log, "fetch ") {
		t.Fatal(env, err, f.log)
	}

	// item lookup
	id := "7"
	p = connector.DefaultQueryParams()
	p.ItemID = &id
	env, err = c.Query(context.Background(), &p)
	if err != nil || env.Meta.Limit != 1 || env.Meta.Total != 2 {
		t.Fatal(env, err)
	}
	if m := fetchToMap(&goimap.Message{Uid: 1}); m["from"] != "" || m["date"] != "" {
		t.Fatal(m)
	}
}

func TestQueryErrors(t *testing.T) {
	var ce *connector.Error
	c := New(1, testConfig())
	p := connector.DefaultQueryParams()

	withFake(t, nil, connector.Connection("IMAP connect: nope"))
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Kind != connector.ErrConnection {
		t.Fatal(err)
	}
	if err := c.TestConnection(context.Background()); err == nil {
		t.Fatal("expected")
	}

	for op, kind := range map[string]connector.ErrorKind{"select": connector.ErrQuery, "search": connector.ErrQuery, "fetch": connector.ErrQuery} {
		f := &fakeSession{uids: []uint32{1}, failOn: op}
		withFake(t, f, nil)
		if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Kind != kind {
			t.Fatal(op, err)
		}
	}
	f := &fakeSession{failOn: "select"}
	withFake(t, f, nil)
	if err := c.TestConnection(context.Background()); !errors.As(err, &ce) || ce.Kind != connector.ErrConnection || ce.Message != "SELECT INBOX: select failed" {
		t.Fatal(err)
	}
	id := "1"
	p.ItemID = &id
	f = &fakeSession{failOn: "fetch"}
	withFake(t, f, nil)
	if _, err := c.Query(context.Background(), &p); err == nil {
		t.Fatal("expected")
	}
	f = &fakeSession{}
	withFake(t, f, nil)
	if err := c.TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
}

func TestFlagAndMoveActions(t *testing.T) {
	c := New(1, testConfig())
	ctx := context.Background()
	var ce *connector.Error

	f := &fakeSession{}
	withFake(t, f, nil)
	res, err := c.InvokeAction(ctx, "delete_message", map[string]any{"uid": "4"})
	if err != nil || res.(map[string]any)["success"] != true {
		t.Fatal(res, err)
	}
	if strings.Join(f.log, "|") != `select|store 4 +FLAGS \Deleted|expunge|logout` {
		t.Fatal(f.log)
	}
	f.log = nil
	if _, err := c.InvokeAction(ctx, "mark_unread", map[string]any{"uid": "4"}); err != nil || f.log[1] != `store 4 -FLAGS \Seen` {
		t.Fatal(err, f.log)
	}
	f.log = nil
	if _, err := c.InvokeAction(ctx, "move_message", map[string]any{"uid": "4", "destination": "Archive"}); err != nil {
		t.Fatal(err)
	}
	if strings.Join(f.log, "|") != `select|copy 4 Archive|store 4 +FLAGS \Deleted|expunge|logout` {
		t.Fatal(f.log)
	}

	if _, err := c.InvokeAction(ctx, "mark_read", map[string]any{}); !errors.As(err, &ce) || ce.Message != "missing 'uid' parameter" {
		t.Fatal(err)
	}
	if _, err := c.InvokeAction(ctx, "move_message", map[string]any{"uid": "1"}); !errors.As(err, &ce) || ce.Message != "missing 'destination' parameter" {
		t.Fatal(err)
	}
	if _, err := c.InvokeAction(ctx, "move_message", map[string]any{}); err == nil {
		t.Fatal("expected")
	}
	if _, err := c.InvokeAction(ctx, "archive", nil); !errors.As(err, &ce) || ce.Message != "IMAP connector does not support action 'archive'" {
		t.Fatal(err)
	}
	if _, err := imapStoreFlags(ctx, c.config, "bogus", map[string]any{"uid": "1"}); !errors.As(err, &ce) || ce.Kind != connector.ErrUnsupported {
		t.Fatal(err)
	}

	for _, op := range []string{"select", `store 4 +FLAGS \Flagged`} {
		withFake(t, &fakeSession{failOn: op}, nil)
		if _, err := c.InvokeAction(ctx, "flag_message", map[string]any{"uid": "4"}); err == nil {
			t.Fatal(op)
		}
	}
	withFake(t, &fakeSession{failOn: "expunge"}, nil)
	if _, err := c.InvokeAction(ctx, "delete_message", map[string]any{"uid": "4"}); !errors.As(err, &ce) || !strings.HasPrefix(ce.Message, "EXPUNGE") {
		t.Fatal(err)
	}
	for _, op := range []string{"select", "copy 4 A", `store 4 +FLAGS \Deleted`, "expunge"} {
		withFake(t, &fakeSession{failOn: op}, nil)
		if _, err := c.InvokeAction(ctx, "move_message", map[string]any{"uid": "4", "destination": "A"}); err == nil {
			t.Fatal(op)
		}
	}
	withFake(t, nil, errors.New("dial"))
	if _, err := c.InvokeAction(ctx, "unflag_message", map[string]any{"uid": "4"}); err == nil {
		t.Fatal("dial")
	}
	if _, err := c.InvokeAction(ctx, "move_message", map[string]any{"uid": "4", "destination": "A"}); err == nil {
		t.Fatal("dial")
	}
}

func TestSendEmail(t *testing.T) {
	var gotFrom string
	var gotRcpts []string
	var gotMsg string
	orig := smtpSend
	smtpSend = func(_ context.Context, _ Config, from string, rcpts []string, m []byte) error {
		gotFrom, gotRcpts, gotMsg = from, rcpts, string(m)
		return nil
	}
	t.Cleanup(func() { smtpSend = orig })

	c := New(1, testConfig())
	res, err := c.InvokeAction(context.Background(), "send_email", map[string]any{
		"to": "Bob <bob@x.com>", "subject": "Héllo", "body": "hi there", "cc": "c@x.com", "bcc": "d@x.com",
	})
	if err != nil || res.(map[string]any)["status"] != "sent" {
		t.Fatal(res, err)
	}
	if gotFrom != "test@example.com" || strings.Join(gotRcpts, ",") != "bob@x.com,c@x.com,d@x.com" {
		t.Fatal(gotFrom, gotRcpts)
	}
	if !strings.Contains(gotMsg, "To: \"Bob\" <bob@x.com>") || !strings.Contains(gotMsg, "Cc: <c@x.com>") || strings.Contains(gotMsg, "d@x.com") ||
		!strings.Contains(gotMsg, "Subject: =?utf-8?") || !strings.Contains(gotMsg, "hi there") {
		t.Fatal(gotMsg)
	}

	var ce *connector.Error
	for params, wantMsg := range map[string]string{
		`{"to":"nope","subject":"s","body":"b"}`:              "invalid to address",
		`{"to":"a@b.c","subject":"s","body":"b","cc":"x y"}`:  "invalid cc",
		`{"to":"a@b.c","subject":"s","body":"b","bcc":"x y"}`: "invalid bcc",
		`{"to":"a@b.c","subject":"s"}`:                        "invalid send_email params",
		`{"to":"a@b.c","subject":"s","body":"b","bcc":"d@e"}`: "",
	} {
		var m map[string]any
		_ = json.Unmarshal([]byte(params), &m)
		_, err := c.InvokeAction(context.Background(), "send_email", m)
		if wantMsg == "" {
			if err != nil {
				t.Fatal(err)
			}
			continue
		}
		if !errors.As(err, &ce) || ce.Kind != connector.ErrQuery || !strings.HasPrefix(ce.Message, wantMsg) {
			t.Fatal(params, err)
		}
	}
	bad := testConfig()
	bad.Username = "not an address"
	if _, err := New(1, bad).InvokeAction(context.Background(), "send_email", map[string]any{"to": "a@b.c", "subject": "s", "body": "b"}); !errors.As(err, &ce) || ce.Kind != connector.ErrConfig {
		t.Fatal(err)
	}
	smtpSend = func(context.Context, Config, string, []string, []byte) error { return errors.New("refused") }
	if _, err := c.InvokeAction(context.Background(), "send_email", map[string]any{"to": "a@b.c", "subject": "s", "body": "b"}); !errors.As(err, &ce) || ce.Message != "SMTP send: refused" {
		t.Fatal(err)
	}
}

// ── real go-imap client against an in-process go-imap server ──────

func testTLS(t *testing.T) *tls.Config {
	t.Helper()
	srv := httptest.NewUnstartedServer(nil)
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return &tls.Config{Certificates: srv.TLS.Certificates}
}

func TestRealIMAPServer(t *testing.T) {
	be := memory.New()
	user, _ := be.Login(nil, "username", "password")
	if err := user.CreateMailbox("Archive"); err != nil {
		t.Fatal(err)
	}
	s := server.New(be)
	s.AllowInsecureAuth = true
	l, err := tls.Listen("tcp", "127.0.0.1:0", testTLS(t))
	if err != nil {
		t.Fatal(err)
	}
	go func() { _ = s.Serve(l) }()
	t.Cleanup(func() { _ = s.Close() })

	origTLS := imapTLSConfig
	imapTLSConfig = func(string) *tls.Config { return &tls.Config{InsecureSkipVerify: true} }
	t.Cleanup(func() { imapTLSConfig = origTLS })

	port := l.Addr().(*net.TCPAddr).Port
	cfg := Config{ImapHost: "127.0.0.1", ImapPort: uint16(port), Username: "username", Password: "password", Mailbox: "INBOX"}
	c := New(1, cfg)
	ctx := context.Background()

	if err := c.TestConnection(ctx); err != nil {
		t.Fatal(err)
	}
	p := connector.DefaultQueryParams()
	env, err := c.Query(ctx, &p)
	if err != nil {
		t.Fatal(err)
	}
	if env.Meta.Total != 1 || len(env.Items) != 1 {
		t.Fatalf("%+v", env)
	}
	it := env.Items[0]
	if it["id"] != "6" || it["from"] != "contact@example.org" || it["subject"] != "A little message, just for you" ||
		it["date"] != "Wed, 11 May 2016 14:31:59 +0000" || it["message_id"] != "<0000000@localhost/>" {
		t.Fatalf("%+v", it)
	}
	p.Filters = []models.DatasetFilter{{FieldName: "from", Operator: models.OpContains, Value: "nobody"}}
	env, err = c.Query(ctx, &p)
	if err != nil || env.Meta.Total != 0 {
		t.Fatal(env, err)
	}
	id := "6"
	p = connector.DefaultQueryParams()
	p.ItemID = &id
	if env, err = c.Query(ctx, &p); err != nil || len(env.Items) != 1 {
		t.Fatal(env, err)
	}
	bad := "x:"
	p.ItemID = &bad
	if _, err := c.Query(ctx, &p); err == nil {
		t.Fatal("bad uid set")
	}

	if _, err := c.InvokeAction(ctx, "flag_message", map[string]any{"uid": "6"}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.InvokeAction(ctx, "flag_message", map[string]any{"uid": "bad"}); err == nil {
		t.Fatal("bad uid")
	}
	if _, err := c.InvokeAction(ctx, "move_message", map[string]any{"uid": "bad", "destination": "Archive"}); err == nil {
		t.Fatal("bad uid")
	}
	if _, err := c.InvokeAction(ctx, "move_message", map[string]any{"uid": "6", "destination": "Archive"}); err != nil {
		t.Fatal(err)
	}
	env, err = c.Query(ctx, &connector.QueryParams{Limit: 50})
	if err != nil || env.Meta.Total != 0 {
		t.Fatal(env, err)
	}
	if _, err := c.Query(ctx, &connector.QueryParams{Limit: 50, Search: strPtr(`x" BOGUS "y`)}); err == nil {
		t.Fatal("expected search error")
	}

	cfg.Password = "wrong"
	var ce *connector.Error
	if err := New(1, cfg).TestConnection(ctx); !errors.As(err, &ce) || ce.Kind != connector.ErrAuth {
		t.Fatal(err)
	}
	cfg.ImapPort = 1
	if err := New(1, cfg).TestConnection(ctx); !errors.As(err, &ce) || ce.Kind != connector.ErrConnection {
		t.Fatal(err)
	}
}

// ── real net/smtp path against a tiny STARTTLS server ─────────────

func TestRealSMTPStartTLS(t *testing.T) {
	tlsCfg := testTLS(t)
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	got := make(chan string, 1)
	go fakeSMTPServer(l, tlsCfg, got, true)

	origTLS := smtpTLSConfig
	smtpTLSConfig = func(string) *tls.Config { return &tls.Config{InsecureSkipVerify: true} }
	t.Cleanup(func() { smtpTLSConfig = origTLS })

	port := l.Addr().(*net.TCPAddr).Port
	cfg := testConfig()
	cfg.SMTPHost, cfg.SMTPPort = "127.0.0.1", uint16(port)
	_, err = New(1, cfg).InvokeAction(context.Background(), "send_email", map[string]any{"to": "a@b.c", "subject": "s", "body": "hello body"})
	if err != nil {
		t.Fatal(err)
	}
	data := <-got
	if !strings.Contains(data, "hello body") {
		t.Fatal(data)
	}

	// Server without STARTTLS is rejected.
	l2, _ := net.Listen("tcp", "127.0.0.1:0")
	defer l2.Close()
	go fakeSMTPServer(l2, tlsCfg, make(chan string, 1), false)
	cfg.SMTPPort = uint16(l2.Addr().(*net.TCPAddr).Port)
	_, err = New(1, cfg).InvokeAction(context.Background(), "send_email", map[string]any{"to": "a@b.c", "subject": "s", "body": "b"})
	if err == nil || !strings.Contains(err.Error(), "STARTTLS") {
		t.Fatal(err)
	}
	cfg.SMTPPort = 1
	if _, err := New(1, cfg).InvokeAction(context.Background(), "send_email", map[string]any{"to": "a@b.c", "subject": "s", "body": "b"}); err == nil {
		t.Fatal("expected dial error")
	}
}

func fakeSMTPServer(l net.Listener, tlsCfg *tls.Config, got chan<- string, startTLS bool) {
	conn, err := l.Accept()
	if err != nil {
		return
	}
	defer conn.Close()
	var c net.Conn = conn
	r := bufio.NewReader(c)
	w := func(s string) { _, _ = c.Write([]byte(s + "\r\n")) }
	w("220 fake ESMTP")
	var data strings.Builder
	inData := false
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			return
		}
		line = strings.TrimRight(line, "\r\n")
		if inData {
			if line == "." {
				inData = false
				got <- data.String()
				w("250 OK")
				continue
			}
			data.WriteString(line + "\n")
			continue
		}
		cmd := strings.ToUpper(line)
		switch {
		case strings.HasPrefix(cmd, "EHLO"):
			if startTLS {
				w("250-fake")
				w("250-STARTTLS")
				w("250 AUTH PLAIN")
			} else {
				w("250 fake")
			}
		case cmd == "STARTTLS":
			w("220 go ahead")
			tc := tls.Server(conn, tlsCfg)
			if tc.Handshake() != nil {
				return
			}
			c = tc
			r = bufio.NewReader(c)
		case strings.HasPrefix(cmd, "AUTH"):
			w("235 ok")
		case strings.HasPrefix(cmd, "MAIL"), strings.HasPrefix(cmd, "RCPT"), strings.HasPrefix(cmd, "RSET"), strings.HasPrefix(cmd, "NOOP"):
			w("250 OK")
		case cmd == "DATA":
			inData = true
			w("354 go")
		case cmd == "QUIT":
			w("221 bye")
			return
		default:
			w("502 no")
		}
	}
}

func strPtr(s string) *string { return &s }

func slicesContains(log []string, prefix string) bool {
	for _, l := range log {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}
