package gmail

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/oauth"
	"github.com/ipedrazas/sbx/internal/secretstore"
)

const dsID = 7

func setup(t *testing.T, h http.HandlerFunc) *Connector {
	t.Helper()
	secretstore.Init(secretstore.NewMemory())
	if err := oauth.StoreTokens(dsID, &oauth.Tokens{AccessToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer: %q", r.Header.Get("Authorization"))
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c := New(dsID, oauth.Credentials{ClientID: "x"})
	c.BaseURL = srv.URL
	return c
}

func params(to, subject, body string) *createDraftParams {
	return &createDraftParams{To: to, Subject: subject, Body: body}
}

func TestRFC2822RequiredHeadersAndBody(t *testing.T) {
	msg := buildRFC2822(params("a@b.c", "Hi", "Hello"))
	if !strings.HasPrefix(msg, "To: a@b.c\r\n") || !strings.Contains(msg, "Subject: Hi\r\n") ||
		!strings.Contains(msg, "MIME-Version: 1.0\r\n") || !strings.HasSuffix(msg, "\r\n\r\nHello") {
		t.Fatalf("bad message %q", msg)
	}
}

func TestRFC2822CcBccFrom(t *testing.T) {
	p := params("a@b.c", "Hi", "Hello")
	p.Cc, p.Bcc, p.From = models.Ptr("cc@b.c"), models.Ptr("bcc@b.c"), models.Ptr("me@b.c")
	msg := buildRFC2822(p)
	for _, want := range []string{"From: me@b.c\r\n", "Cc: cc@b.c\r\n", "Bcc: bcc@b.c\r\n"} {
		if !strings.Contains(msg, want) {
			t.Errorf("missing %q", want)
		}
	}
}

func TestSimulateDeterministic(t *testing.T) {
	c := New(0, oauth.Credentials{ClientID: "x"})
	ctx := context.Background()
	a, err := c.SimulateAction(ctx, "create_draft", map[string]any{"to": "a@b.c", "subject": "Hi", "body": "hello"})
	if err != nil {
		t.Fatal(err)
	}
	b, _ := c.SimulateAction(ctx, "create_draft", map[string]any{"to": "a@b.c", "subject": "Hi", "body": "hello"})
	cc, _ := c.SimulateAction(ctx, "create_draft", map[string]any{"to": "z@b.c", "subject": "Hi", "body": "hello"})
	ma, mb, mc := a.(map[string]any), b.(map[string]any), cc.(map[string]any)
	if ma["draft_id"] != mb["draft_id"] || ma["draft_id"] == mc["draft_id"] {
		t.Fatal("simulation should be deterministic by params")
	}
	if !strings.HasPrefix(ma["draft_id"].(string), "r-SIM-") || !strings.HasSuffix(ma["message_id"].(string), "@sim.gmail") ||
		!strings.HasSuffix(ma["thread_id"].(string), "@sim.gmail") {
		t.Fatalf("bad shape %v", ma)
	}
}

func TestSimulateRejects(t *testing.T) {
	c := New(0, oauth.Credentials{ClientID: "x"})
	var ce *connector.Error
	_, err := c.SimulateAction(context.Background(), "create_draft", map[string]any{"subject": "x", "body": "y"})
	if !errors.As(err, &ce) || ce.Kind != connector.ErrQuery || !strings.Contains(err.Error(), "missing field `to`") {
		t.Fatalf("got %v", err)
	}
	_, err = c.SimulateAction(context.Background(), "create_draft", map[string]any{"to": 1, "subject": "x", "body": "y"})
	if !errors.As(err, &ce) || ce.Kind != connector.ErrQuery {
		t.Fatalf("got %v", err)
	}
	_, err = c.SimulateAction(context.Background(), "create_draft", map[string]any{"to": "a", "subject": "x", "body": "y", "cc": 3})
	if err == nil {
		t.Fatal("bad cc type should fail")
	}
	_, err = c.SimulateAction(context.Background(), "send", nil)
	if !errors.As(err, &ce) || ce.Kind != connector.ErrUnsupported {
		t.Fatalf("got %v", err)
	}
	_, err = c.InvokeAction(context.Background(), "send", nil)
	if !errors.As(err, &ce) || ce.Kind != connector.ErrUnsupported {
		t.Fatalf("got %v", err)
	}
	if _, err = c.InvokeAction(context.Background(), "create_draft", map[string]any{}); err == nil {
		t.Fatal("invalid params should fail")
	}
}

func TestInvokeCreateDraft(t *testing.T) {
	c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/drafts" {
			t.Errorf("unexpected %s %s", r.Method, r.URL.Path)
		}
		var body struct {
			Message struct{ Raw string } `json:"message"`
		}
		_ = json.NewDecoder(r.Body).Decode(&body)
		raw, err := base64.RawURLEncoding.DecodeString(body.Message.Raw)
		if err != nil || !strings.Contains(string(raw), "Cc: c@d\r\n") {
			t.Errorf("bad raw %q %v", raw, err)
		}
		_, _ = w.Write([]byte(`{"id":"r1","message":{"id":"m1","threadId":"t1"}}`))
	})
	out, err := c.InvokeAction(context.Background(), "create_draft", map[string]any{"to": "a@b", "subject": "s", "body": "b", "cc": "c@d", "bcc": nil})
	if err != nil {
		t.Fatal(err)
	}
	m := out.(map[string]any)
	if m["draft_id"] != "r1" || m["message_id"] != "m1" || m["thread_id"] != "t1" {
		t.Fatalf("got %v", m)
	}
}

func TestInvokeCreateDraftNoMessage(t *testing.T) {
	c := setup(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`{"id":"r1"}`)) })
	out, err := c.InvokeAction(context.Background(), "create_draft", map[string]any{"to": "a", "subject": "s", "body": "b"})
	if err != nil {
		t.Fatal(err)
	}
	if m := out.(map[string]any); m["message_id"] != nil || m["thread_id"] != nil {
		t.Fatalf("got %v", m)
	}
}

func TestInvokeCreateDraftUpstreamError(t *testing.T) {
	c := setup(t, func(w http.ResponseWriter, r *http.Request) { http.Error(w, "boom", 500) })
	_, err := c.InvokeAction(context.Background(), "create_draft", map[string]any{"to": "a", "subject": "s", "body": "b"})
	var ce *connector.Error
	if !errors.As(err, &ce) || ce.Kind != connector.ErrUpstreamHTTP || ce.Status != 500 || !strings.Contains(ce.Message, "Gmail API") {
		t.Fatalf("got %v", err)
	}
}

func msgJSON(id string) string {
	return `{"id":"` + id + `","threadId":"t-` + id + `","labelIds":["INBOX"],"snippet":"hi","payload":{"headers":[` +
		`{"name":"from","value":"alice@x.com"},{"name":"To","value":"me"},{"name":"Subject","value":"S ` + id + `"},{"name":"Date","value":"D"}]}}`
}

func TestQueryList(t *testing.T) {
	var listQ atomic.Value
	c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/messages":
			listQ.Store(r.URL.Query())
			_, _ = w.Write([]byte(`{"messages":[{"id":"a"},{"id":"b"},{"id":"c"}],"resultSizeEstimate":42}`))
		case strings.HasPrefix(r.URL.Path, "/messages/"):
			if r.URL.Query().Get("format") != "metadata" {
				t.Error("expected metadata format")
			}
			_, _ = w.Write([]byte(msgJSON(strings.TrimPrefix(r.URL.Path, "/messages/"))))
		default:
			http.NotFound(w, r)
		}
	})
	p := connector.DefaultQueryParams()
	p.Search = models.Ptr("invoice")
	p.Filters = []models.DatasetFilter{
		{FieldName: "from", Operator: models.OpEquals, Value: "alice", Logic: models.LogicOr},
		{FieldName: "to", Operator: models.OpContains, Value: "me", Logic: models.LogicOr},
		{FieldName: "subject", Operator: models.OpNotEquals, Value: "S b"},
	}
	env, err := c.Query(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	q := listQ.Load().(url.Values)
	if q["q"][0] != "{from:alice to:me} invoice" || q["maxResults"][0] != "50" {
		t.Fatalf("query %v", q)
	}
	if len(env.Items) != 2 || env.Items[0]["id"] != "a" || env.Items[1]["id"] != "c" {
		t.Fatalf("items %v", env.Items)
	}
	if env.Meta.Total != 42 || env.Meta.Limit != 50 {
		t.Fatalf("meta %+v", env.Meta)
	}
	it := env.Items[0]
	if it["from"] != "alice@x.com" || it["thread_id"] != "t-a" || it["date"] != "D" || it["snippet"] != "hi" {
		t.Fatalf("item %v", it)
	}
}

func TestQueryANDFiltersAndEmptyList(t *testing.T) {
	var gotQ string
	c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		gotQ = r.URL.Query().Get("q")
		_, _ = w.Write([]byte(`{}`))
	})
	p := connector.DefaultQueryParams()
	p.Filters = []models.DatasetFilter{
		{FieldName: "from", Operator: models.OpContains, Value: "a"},
		{FieldName: "subject", Operator: models.OpContains, Value: "b"},
	}
	env, err := c.Query(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	if gotQ != "from:a subject:b" || len(env.Items) != 0 || env.Meta.Total != 0 || env.Items == nil {
		t.Fatalf("q=%q env=%+v", gotQ, env)
	}
}

func TestQueryItemID(t *testing.T) {
	c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/messages/xyz" {
			t.Errorf("path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":"xyz","threadId":"t"}`))
	})
	p := connector.DefaultQueryParams()
	p.ItemID = models.Ptr("xyz")
	env, err := c.Query(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	if env.Meta.Total != 1 || env.Meta.Limit != 1 || env.Items[0]["from"] != "" || len(env.Items[0]["label_ids"].([]any)) != 0 {
		t.Fatalf("env %+v", env)
	}
}

func TestQueryDetailError(t *testing.T) {
	c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/messages" {
			_, _ = w.Write([]byte(`{"messages":[{"id":"a"},{"id":"b"}]}`))
			return
		}
		http.Error(w, "nope", http.StatusForbidden)
	})
	p := connector.DefaultQueryParams()
	_, err := c.Query(context.Background(), &p)
	var ce *connector.Error
	if !errors.As(err, &ce) || ce.Kind != connector.ErrAuth {
		t.Fatalf("got %v", err)
	}
}

func TestQueryBadJSON(t *testing.T) {
	c := setup(t, func(w http.ResponseWriter, r *http.Request) { _, _ = w.Write([]byte(`not json`)) })
	p := connector.DefaultQueryParams()
	_, err := c.Query(context.Background(), &p)
	var ce *connector.Error
	if !errors.As(err, &ce) || ce.Kind != connector.ErrQuery {
		t.Fatalf("got %v", err)
	}
	p.ItemID = models.Ptr("a")
	if _, err = c.Query(context.Background(), &p); err == nil {
		t.Fatal("expected error")
	}
}

func TestTestConnection(t *testing.T) {
	c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/profile" {
			http.NotFound(w, r)
		}
	})
	if err := c.TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	c.BaseURL = "http://127.0.0.1:1"
	var ce *connector.Error
	if err := c.TestConnection(context.Background()); !errors.As(err, &ce) || ce.Kind != connector.ErrConnection {
		t.Fatalf("got %v", err)
	}
}

func TestNoTokensIsAuthError(t *testing.T) {
	secretstore.Init(secretstore.NewMemory())
	c := New(99, oauth.Credentials{ClientID: "x"})
	var ce *connector.Error
	if err := c.TestConnection(context.Background()); !errors.As(err, &ce) || ce.Kind != connector.ErrAuth {
		t.Fatalf("got %v", err)
	}
	p := connector.DefaultQueryParams()
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Kind != connector.ErrAuth {
		t.Fatalf("got %v", err)
	}
	if _, err := c.InvokeAction(context.Background(), "create_draft", map[string]any{"to": "a", "subject": "s", "body": "b"}); err == nil {
		t.Fatal("expected auth error")
	}
}

func TestIntrospect(t *testing.T) {
	s, _ := New(1, oauth.Credentials{}).Introspect(context.Background())
	if len(s.Fields) != 8 || s.Fields[5].Name != "date" || s.Fields[5].FieldType != connector.FieldDateTime {
		t.Fatalf("schema %+v", s)
	}
}

func TestSynthHashMatchesRust(t *testing.T) {
	for _, c := range []struct {
		row  int64
		salt uint64
		want uint64
	}{{0, 0, 8556445246977061536}, {1, 5, 9386390421034826751}, {7, 0x674D41, 3174228736872868458}, {42, 23, 12981582849169170285}} {
		if got := synthHash(c.row, c.salt); got != c.want {
			t.Errorf("synthHash(%d,%d)=%d want %d", c.row, c.salt, got, c.want)
		}
	}
}

func TestSyntheticRowsMatchRust(t *testing.T) {
	rows, ok := New(1, oauth.Credentials{}).GenerateSyntheticRows(nil, 38)
	if !ok || len(rows) != 38 {
		t.Fatal("expected 38 rows")
	}
	want := map[int][4]string{
		0:  {"b71d6a24ef50eb1c", "2a28415a71bbf2be", "Wed, 30 Apr 2025 08:00:00 +0000", synthSenders[7][1]},
		1:  {"8243285b50e8fbff", "9b1454a6281eb9b8", "Wed, 30 Apr 2025 02:31:15 +0000", synthSenders[0][1]},
		37: {"ea829b8801ec71ff", "997e720552b1aeb4", "Sun, 20 Apr 2025 00:33:08 +0000", synthSenders[3][1]},
	}
	for i, w := range want {
		r := rows[i]
		if r["id"] != w[0] || r["thread_id"] != w[1] || r["date"] != w[2] || !strings.Contains(r["from"].(string), w[3]) || r["to"] != "me" {
			t.Errorf("row %d = %v", i, r)
		}
	}
	if rows, _ := New(1, oauth.Credentials{}).GenerateSyntheticRows(nil, 0); len(rows) != 0 {
		t.Fatal("zero count")
	}
}

func TestChronoLikeFormat(t *testing.T) {
	if got := chronoLikeFormat(0); got != "Thu, 1 Jan 1970 00:00:00 +0000" {
		t.Fatal(got)
	}
	if got := chronoLikeFormat(951782400); got != "Tue, 29 Feb 2000 00:00:00 +0000" {
		t.Fatal(got)
	}
}
