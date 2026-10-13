package gcal

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

const dsID = 3

func setup(t *testing.T, cal *string, h http.HandlerFunc) *Connector {
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
	c := New(dsID, oauth.Credentials{ClientID: "x"}, cal)
	c.BaseURL = srv.URL
	return c
}

const eventsJSON = `{"items":[
 {"id":"e1","summary":"Standup","start":{"dateTime":"2026-03-02T09:00:00Z"},"end":{"dateTime":"2026-03-02T09:15:00Z"},
  "status":"confirmed","organizer":{"email":"o@x"},"attendees":[{"email":"a@x","responseStatus":"accepted"},{}],
  "htmlLink":"L","created":"C","updated":"U","description":"d","location":"here"},
 {"id":"e2","summary":"Holiday","start":{"date":"2026-03-05"},"end":{"date":"2026-03-06"},"status":"tentative"}
]}`

func TestQueryListBoundsAndFilters(t *testing.T) {
	var gotQ url.Values
	var rawQuery string
	c := setup(t, models.Ptr("team@group.calendar.google.com"), func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/calendars/team@group.calendar.google.com/events" {
			t.Errorf("path %s", r.URL.Path)
		}
		gotQ, rawQuery = r.URL.Query(), r.URL.RawQuery
		_, _ = w.Write([]byte(eventsJSON))
	})
	p := connector.DefaultQueryParams()
	p.Search = models.Ptr("stand up")
	p.Filters = []models.DatasetFilter{
		{FieldName: "start", Operator: models.OpGreaterThan, Value: "2026-03-01"},
		{FieldName: "end", Operator: models.OpLessThan, Value: "2026-04-01T00:00:00Z"},
		{FieldName: "status", Operator: models.OpEquals, Value: "confirmed"},
	}
	env, err := c.Query(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	if gotQ.Get("timeMin") != "2026-03-01T00:00:00Z" || gotQ.Get("timeMax") != "2026-04-01T00:00:00Z" ||
		gotQ.Get("q") != "stand up" || gotQ.Get("singleEvents") != "true" || gotQ.Get("orderBy") != "startTime" || gotQ.Get("maxResults") != "50" {
		t.Fatalf("query %v", gotQ)
	}
	if !strings.Contains(rawQuery, "q=stand+up") {
		t.Fatalf("expected form encoding: %s", rawQuery)
	}
	if len(env.Items) != 1 || env.Meta.Total != 1 {
		t.Fatalf("items %v", env.Items)
	}
	it := env.Items[0]
	att := it["attendees"].([]any)
	if it["organizer"] != "o@x" || it["start"] != "2026-03-02T09:00:00Z" || len(att) != 2 ||
		att[0].(map[string]any)["response_status"] != "accepted" || att[1].(map[string]any)["email"] != "" ||
		it["html_link"] != "L" || it["location"] != "here" {
		t.Fatalf("item %v", it)
	}
}

func TestQueryDefaultTimeMinIsNow(t *testing.T) {
	old := now
	now = func() int64 { return 1746100800 }
	defer func() { now = old }()
	var gotQ url.Values
	c := setup(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/calendars/primary/events" {
			t.Errorf("path %s", r.URL.Path)
		}
		gotQ = r.URL.Query()
		_, _ = w.Write([]byte(eventsJSON))
	})
	p := connector.DefaultQueryParams()
	env, err := c.Query(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	if gotQ.Get("timeMin") != "2025-05-01T12:00:00Z" || gotQ.Has("timeMax") || gotQ.Has("q") {
		t.Fatalf("query %v", gotQ)
	}
	if len(env.Items) != 2 || env.Items[1]["start"] != "2026-03-05" || env.Items[1]["organizer"] != "" {
		t.Fatalf("items %v", env.Items)
	}
}

func TestQueryItemID(t *testing.T) {
	c := setup(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/calendars/primary/events/e+1" {
			t.Errorf("path %s", r.URL.Path)
		}
		_, _ = w.Write([]byte(`{"id":"e 1"}`))
	})
	p := connector.DefaultQueryParams()
	p.ItemID = models.Ptr("e 1")
	env, err := c.Query(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	if env.Meta.Total != 1 || env.Meta.Limit != 1 || env.Items[0]["id"] != "e 1" || env.Items[0]["start"] != "" {
		t.Fatalf("env %+v", env)
	}
}

func TestQueryErrors(t *testing.T) {
	status := 404
	body := "missing"
	c := setup(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if status != 200 {
			http.Error(w, body, status)
			return
		}
		_, _ = w.Write([]byte(body))
	})
	p := connector.DefaultQueryParams()
	var ce *connector.Error
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Kind != connector.ErrUpstreamHTTP || ce.Status != 404 || !strings.Contains(ce.Message, "Calendar API") {
		t.Fatalf("got %v", err)
	}
	p.ItemID = models.Ptr("x")
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Status != 404 {
		t.Fatalf("got %v", err)
	}
	status, body = 200, "garbage"
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Kind != connector.ErrQuery {
		t.Fatalf("got %v", err)
	}
	p.ItemID = nil
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Kind != connector.ErrQuery {
		t.Fatalf("got %v", err)
	}
	status = 401
	if err := c.TestConnection(context.Background()); !errors.As(err, &ce) || ce.Kind != connector.ErrAuth {
		t.Fatalf("got %v", err)
	}
}

func TestTestConnection(t *testing.T) {
	c := setup(t, nil, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/calendars/primary" {
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
	p := connector.DefaultQueryParams()
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Kind != connector.ErrQuery {
		t.Fatalf("got %v", err)
	}
}

func TestNoTokens(t *testing.T) {
	secretstore.Init(secretstore.NewMemory())
	c := New(99, oauth.Credentials{}, nil)
	var ce *connector.Error
	p := connector.DefaultQueryParams()
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Kind != connector.ErrAuth {
		t.Fatalf("got %v", err)
	}
	if err := c.TestConnection(context.Background()); !errors.As(err, &ce) || ce.Kind != connector.ErrAuth {
		t.Fatalf("got %v", err)
	}
}

func TestIntrospectAndDefaults(t *testing.T) {
	c := New(1, oauth.Credentials{}, nil)
	s, _ := c.Introspect(context.Background())
	if len(s.Fields) != 12 || s.Fields[8].FieldType != connector.FieldJSON {
		t.Fatalf("schema %+v", s)
	}
	if _, err := c.InvokeAction(context.Background(), "x", nil); err == nil {
		t.Fatal("actions unsupported")
	}
}

func TestExtractTimeBounds(t *testing.T) {
	min, max := extractTimeBounds([]models.DatasetFilter{
		{FieldName: "start", Operator: models.OpGreaterThan, Value: "2026-01-01"},
		{FieldName: "end", Operator: models.OpGreaterThan, Value: "2026-02-01T00:00:00+01:00"},
		{FieldName: "start", Operator: models.OpEquals, Value: "x"},
		{FieldName: "summary", Operator: models.OpLessThan, Value: "y"},
	})
	if min == nil || *min != "2026-02-01T00:00:00+01:00" || max != nil {
		t.Fatalf("got %v %v", min, max)
	}
	if normalizeRFC3339("2026/01/01") != "2026/01/01" {
		t.Fatal("normalize")
	}
}

func TestFormatRFC3339(t *testing.T) {
	for epoch, want := range map[int64]string{0: "1970-01-01T00:00:00Z", 951782400: "2000-02-29T00:00:00Z", -1: "1969-12-31T23:59:59Z", 1746100800: "2025-05-01T12:00:00Z"} {
		if got := formatRFC3339(epoch); got != want {
			t.Errorf("%d: %s want %s", epoch, got, want)
		}
	}
}

func TestSyntheticRowsMatchRust(t *testing.T) {
	rows, ok := New(1, oauth.Credentials{}, nil).GenerateSyntheticRows(nil, 38)
	if !ok || len(rows) != 38 {
		t.Fatal("rows")
	}
	want := map[int]struct {
		id, start, created string
		evt, nAtt          int
	}{
		0:  {"00000000d3eed631c35c21cf", "2025-05-01T22:45:00Z", "2025-04-28T21:06:24Z", 16, 3},
		1:  {"00000000e753f69cf0c2391e", "2025-05-02T03:15:00Z", "2025-04-29T17:09:09Z", 6, 1},
		2:  {"00000000a691aaa855dc52c9", "2025-05-02T02:15:00Z", "2025-04-25T12:50:06Z", 19, 4},
		37: {"00000000aea438deab613ffa", "2025-05-13T03:00:00Z", "2025-05-09T08:47:45Z", 3, 1},
	}
	for i, w := range want {
		r := rows[i]
		if r["id"] != w.id || r["start"] != w.start || r["created"] != w.created || r["summary"] != synthEvents[w.evt].title ||
			len(r["attendees"].([]any)) != w.nAtt || r["html_link"] != "https://calendar.google.com/calendar/event?eid="+w.id[:16] {
			t.Errorf("row %d = %v", i, r)
		}
		for _, a := range r["attendees"].([]any) {
			if strings.Contains(r["organizer"].(string), a.(map[string]any)["email"].(string)) {
				t.Errorf("row %d: organizer listed as attendee", i)
			}
		}
	}
}
