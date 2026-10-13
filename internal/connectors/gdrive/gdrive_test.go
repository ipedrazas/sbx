package gdrive

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/oauth"
	"github.com/ipedrazas/sbx/internal/secretstore"
)

const dsID = 5

func setup(t *testing.T, h http.HandlerFunc) *Connector {
	t.Helper()
	secretstore.Init(secretstore.NewMemory())
	if err := oauth.StoreTokens(dsID, &oauth.Tokens{AccessToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" {
			t.Errorf("missing bearer")
		}
		h(w, r)
	}))
	t.Cleanup(srv.Close)
	c := New(dsID, oauth.Credentials{ClientID: "x"})
	c.BaseURL = srv.URL
	return c
}

const filesJSON = `{"files":[
 {"id":"f1","name":"Roadmap","mimeType":"application/vnd.google-apps.document","owners":[{"emailAddress":"a@x"}],"shared":true,"size":"10"},
 {"id":"f2","name":"Logo","mimeType":"image/png","starred":true}
]}`

func TestBuildQuery(t *testing.T) {
	p := connector.DefaultQueryParams()
	q, mem := buildQuery(&p)
	if q != "trashed = false" || len(mem) != 0 {
		t.Fatalf("got %q", q)
	}
	p.Search = models.Ptr("it's")
	p.Filters = []models.DatasetFilter{
		{FieldName: "name", Operator: models.OpContains, Value: `a\b`},
		{FieldName: "mime_type", Operator: models.OpEquals, Value: "text/csv"},
		{FieldName: "owner", Operator: models.OpEquals, Value: "a@x"},
	}
	q, mem = buildQuery(&p)
	if q != `trashed = false and name contains 'a\\b' and mimeType = 'text/csv' and fullText contains 'it\'s'` || len(mem) != 1 {
		t.Fatalf("got %q", q)
	}
	p.Search = nil
	p.Filters = []models.DatasetFilter{
		{FieldName: "name", Operator: models.OpEquals, Value: "x", Logic: models.LogicOr},
		{FieldName: "mime_type", Operator: models.OpContains, Value: "pdf", Logic: models.LogicOr},
		{FieldName: "starred", Operator: models.OpEquals, Value: true, Logic: models.LogicOr},
		{FieldName: "trashed", Operator: models.OpEquals, Value: false, Logic: models.LogicOr},
	}
	q, _ = buildQuery(&p)
	if q != "(name = 'x' or mimeType contains 'pdf' or starred = true or trashed = false)" {
		t.Fatalf("got %q", q)
	}
	p.Filters = []models.DatasetFilter{{FieldName: "trashed", Operator: models.OpNotEquals, Value: true}}
	q, mem = buildQuery(&p)
	if q != "" || len(mem) != 1 {
		t.Fatalf("got %q", q)
	}
}

func TestQueryList(t *testing.T) {
	var gotQ url.Values
	c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/files" {
			t.Errorf("path %s", r.URL.Path)
		}
		gotQ = r.URL.Query()
		_, _ = w.Write([]byte(filesJSON))
	})
	p := connector.DefaultQueryParams()
	p.Filters = []models.DatasetFilter{{FieldName: "owner", Operator: models.OpEquals, Value: "a@x"}}
	env, err := c.Query(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	if gotQ.Get("q") != "trashed = false" || gotQ.Get("pageSize") != "50" || gotQ.Get("fields") != "files("+fileFields+"),nextPageToken" {
		t.Fatalf("query %v", gotQ)
	}
	if len(env.Items) != 1 || env.Meta.Total != 1 || env.Items[0]["id"] != "f1" || env.Items[0]["shared"] != true || env.Items[0]["starred"] != false || env.Items[0]["size"] != "10" {
		t.Fatalf("items %v", env.Items)
	}
}

func TestQueryNoQ(t *testing.T) {
	c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Has("q") {
			t.Error("q should be absent")
		}
		_, _ = w.Write([]byte(`{}`))
	})
	p := connector.DefaultQueryParams()
	p.Filters = []models.DatasetFilter{{FieldName: "trashed", Operator: models.OpNotEquals, Value: true}}
	env, err := c.Query(context.Background(), &p)
	if err != nil || len(env.Items) != 0 || env.Items == nil {
		t.Fatalf("%v %v", env, err)
	}
}

func TestQueryItemID(t *testing.T) {
	c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/files/f1" || r.URL.Query().Get("fields") != fileFields {
			t.Errorf("unexpected %s", r.URL)
		}
		_, _ = w.Write([]byte(`{"id":"f1","trashed":true}`))
	})
	p := connector.DefaultQueryParams()
	p.ItemID = models.Ptr("f1")
	env, err := c.Query(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	if env.Meta.Total != 1 || env.Meta.Limit != 1 || env.Items[0]["trashed"] != true || env.Items[0]["owner"] != "" {
		t.Fatalf("env %+v", env)
	}
}

func TestErrors(t *testing.T) {
	status, body := 500, "boom"
	c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if status != 200 {
			http.Error(w, body, status)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	p := connector.DefaultQueryParams()
	var ce *connector.Error
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Status != 500 || !strings.Contains(ce.Message, "Drive API") {
		t.Fatalf("got %v", err)
	}
	p.ItemID = models.Ptr("x")
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Status != 500 {
		t.Fatalf("got %v", err)
	}
	status, body = 200, "nope"
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Kind != connector.ErrQuery {
		t.Fatalf("got %v", err)
	}
	p.ItemID = nil
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Kind != connector.ErrQuery {
		t.Fatalf("got %v", err)
	}
	status = 403
	if err := c.TestConnection(context.Background()); !errors.As(err, &ce) || ce.Kind != connector.ErrAuth {
		t.Fatalf("got %v", err)
	}
}

func TestTestConnection(t *testing.T) {
	c := setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/about" || r.URL.Query().Get("fields") != "user" {
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

func TestNoTokens(t *testing.T) {
	secretstore.Init(secretstore.NewMemory())
	c := New(99, oauth.Credentials{})
	var ce *connector.Error
	p := connector.DefaultQueryParams()
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Kind != connector.ErrAuth {
		t.Fatalf("got %v", err)
	}
	if err := c.TestConnection(context.Background()); !errors.As(err, &ce) || ce.Kind != connector.ErrAuth {
		t.Fatalf("got %v", err)
	}
	if _, err := c.fetchFile(context.Background(), "x"); err == nil {
		t.Fatal("expected error")
	}
}

func TestIntrospect(t *testing.T) {
	s, _ := New(1, oauth.Credentials{}).Introspect(context.Background())
	if len(s.Fields) != 11 || s.Fields[8].FieldType != connector.FieldBoolean {
		t.Fatalf("schema %+v", s)
	}
}

func TestSyntheticRowsMatchRust(t *testing.T) {
	rows, ok := New(1, oauth.Credentials{}).GenerateSyntheticRows(nil, 38)
	if !ok || len(rows) != 38 {
		t.Fatal("rows")
	}
	want := map[int]struct {
		id, created, modified string
		file                  int
		shared, starred       bool
	}{
		0:  {"0000000000006881f435bc0ca85f", "2025-02-24T06:34:44Z", "2025-04-21T16:21:25Z", 0, false, false},
		1:  {"0000000000000afdd02499b8e2cd", "2025-04-16T07:12:56Z", "2025-04-23T10:11:58Z", 18, false, true},
		2:  {"000000000000a7af9f593fdc4675", "2025-02-22T13:04:08Z", "2025-03-30T02:21:25Z", 3, false, false},
		37: {"0000000000000f9f259a2198b94c", "2025-03-24T04:11:10Z", "2025-03-28T11:21:00Z", 19, false, false},
	}
	for i, w := range want {
		r := rows[i]
		if r["id"] != w.id || r["created_time"] != w.created || r["modified_time"] != w.modified || r["name"] != synthFiles[w.file].name ||
			r["shared"] != w.shared || r["starred"] != w.starred || r["trashed"] != false {
			t.Errorf("row %d = %v", i, r)
		}
	}
	if rows[0]["web_view_link"] != "https://docs.google.com/document/d/0000000000006881f435/edit" {
		t.Errorf("link %v", rows[0]["web_view_link"])
	}
	if rows[1]["web_view_link"] != "https://drive.google.com/file/d/0000000000000afdd024/view" {
		t.Errorf("link %v", rows[1]["web_view_link"])
	}
	seen := map[string]bool{}
	for _, r := range rows {
		seen[r["web_view_link"].(string)[:30]] = true
	}
	if len(seen) < 2 {
		t.Error("expected varied links")
	}
}

func TestSynthRFC3339(t *testing.T) {
	if synthRFC3339(951782400) != "2000-02-29T00:00:00Z" || synthRFC3339(0) != "1970-01-01T00:00:00Z" {
		t.Fatal("format")
	}
}
