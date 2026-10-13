package rest

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/secretstore"
)

func strp(s string) *string { return &s }

func mustJSON(t *testing.T, v any) string {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

func TestAuthJSONShape(t *testing.T) {
	cases := []struct {
		in   Auth
		want string
	}{
		{Auth{}, `{"kind":"none"}`},
		{Auth{Kind: AuthBearer, TokenRef: strp("r")}, `{"kind":"bearer","token_ref":"r"}`},
		{Auth{Kind: AuthAPIKey, Header: strp("X-Key"), ValueRef: strp("v")}, `{"header":"X-Key","kind":"api_key","value_ref":"v"}`},
		{Auth{Kind: AuthBasic, Username: "u"}, `{"kind":"basic","username":"u"}`},
	}
	for _, c := range cases {
		if got := mustJSON(t, c.in); got != c.want {
			t.Fatalf("got %s want %s", got, c.want)
		}
		var back Auth
		if err := json.Unmarshal([]byte(c.want), &back); err != nil {
			t.Fatal(err)
		}
		if mustJSON(t, back) != c.want {
			t.Fatal("roundtrip", c.want)
		}
	}
	var a Auth
	for in, wantErr := range map[string]string{
		`{}`:                 "missing field `kind`",
		`{"kind":"basic"}`:   "missing field `username`",
		`{"kind":"oauth"}`:   "unknown variant `oauth`, expected one of `none`, `bearer`, `api_key`, `basic`",
		`null`:               "invalid type: null",
		`{"kind":"bearer"}`:  "",
		`{"kind":"api_key"}`: "",
	} {
		err := json.Unmarshal([]byte(in), &a)
		if wantErr == "" && err != nil || wantErr != "" && (err == nil || !strings.Contains(err.Error(), wantErr)) {
			t.Fatalf("%s: %v", in, err)
		}
	}
	if _, err := json.Marshal(Auth{Kind: "weird"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestDatasourceConfigJSON(t *testing.T) {
	c, err := ParseDatasourceConfig(map[string]any{"base_url": "https://x"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Auth.Kind != AuthNone {
		t.Fatal(c.Auth.Kind)
	}
	if got := mustJSON(t, c); got != `{"base_url":"https://x","auth":{"kind":"none"},"default_headers":{},"spec":null}` {
		t.Fatal(got)
	}
	if _, err := ParseDatasourceConfig(map[string]any{}); err == nil || err.Error() != "missing field `base_url`" {
		t.Fatal(err)
	}
	c, err = ParseDatasourceConfig(map[string]any{"base_url": "x", "spec": map[string]any{"kind": "openapi", "url": "http://s"}})
	if err != nil || c.Spec == nil || *c.Spec.URL != "http://s" || c.Spec.Inline != nil {
		t.Fatal(c, err)
	}
	if _, err := ParseDatasourceConfig(map[string]any{"base_url": "x", "spec": map[string]any{"kind": "raml"}}); err == nil {
		t.Fatal("expected spec kind error")
	}
	if _, err := ParseDatasourceConfig(map[string]any{"base_url": "x", "spec": map[string]any{}}); err == nil {
		t.Fatal("expected spec missing kind")
	}
}

func TestDatasetConfigDefaultsAndShape(t *testing.T) {
	c, err := ParseDatasetConfig(map[string]any{"path": "/users"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Method != "GET" || c.ItemIDField != "id" || c.Pagination.Kind != PaginationNone || c.Schema.Fields == nil {
		t.Fatalf("%+v", c)
	}
	want := `{"path":"/users","method":"GET","query_params":{},"headers":{},"list_path":null,"item_id_field":"id","pagination":{"kind":"none"},"filter_mapping":{},"schema":{"fields":[]}}`
	if got := mustJSON(t, c); got != want {
		t.Fatalf("\n got %s\nwant %s", got, want)
	}
	if _, err := ParseDatasetConfig(map[string]any{}); err == nil || err.Error() != "missing field `path`" {
		t.Fatal(err)
	}

	in := `{"path":"/p","pagination":{"kind":"page","page_param":"p","size_param":"s"},"filter_mapping":{"status":{"param":"state"}}}`
	var d DatasetConfig
	if err := json.Unmarshal([]byte(in), &d); err != nil {
		t.Fatal(err)
	}
	if d.Pagination.StartPage != 1 || d.FilterMapping["status"].Param != "state" || d.FilterMapping["status"].Operators == nil {
		t.Fatalf("%+v", d)
	}
	for _, p := range []Pagination{
		{Kind: PaginationOffset, LimitParam: "l", OffsetParam: "o"},
		{Kind: PaginationPage, PageParam: "p", SizeParam: "s", StartPage: 0},
		{Kind: PaginationCursor, CursorParam: "c", CursorPath: "/n"},
		{Kind: PaginationNone},
	} {
		var back Pagination
		if err := json.Unmarshal([]byte(mustJSON(t, p)), &back); err != nil || back != p {
			t.Fatal(p, back, err)
		}
	}
	var p Pagination
	for _, bad := range []string{`{}`, `{"kind":"offset"}`, `{"kind":"page","page_param":"p"}`, `{"kind":"cursor"}`, `{"kind":"x"}`, `{"kind":"offset","limit_param":"l"}`, `{"kind":"cursor","cursor_param":"c"}`} {
		if err := json.Unmarshal([]byte(bad), &p); err == nil {
			t.Fatal("expected error for", bad)
		}
	}
	var fp FilterParam
	if err := json.Unmarshal([]byte(`{}`), &fp); err == nil {
		t.Fatal("param required")
	}
	if mustJSON(t, FilterParam{Param: "x"}) != `{"param":"x","operators":[]}` {
		t.Fatal("filter param shape")
	}
}

func TestPersistInlineSecrets(t *testing.T) {
	secretstore.Init(secretstore.NewMemory())
	c := DatasourceConfig{BaseURL: "x", Auth: Auth{Kind: AuthBearer, Token: strp("tok")}}
	if err := c.PersistInlineSecrets("My API!"); err != nil {
		t.Fatal(err)
	}
	if c.Auth.Token != nil || *c.Auth.TokenRef != "dstudio-rest-my-api-bearer" {
		t.Fatalf("%+v", c.Auth)
	}
	if b, _ := secretstore.Get("dstudio-rest-my-api-bearer"); string(b) != "tok" {
		t.Fatal(string(b))
	}
	s, err := LoadAuthSecret(c.Auth)
	if err != nil || *s != "tok" {
		t.Fatal(s, err)
	}

	a := Auth{Kind: AuthAPIKey, Header: strp("X"), Value: strp("k")}
	if err := a.PersistInlineSecrets("ds"); err != nil || *a.ValueRef != "dstudio-rest-ds-apikey" || a.Value != nil {
		t.Fatal(a, err)
	}
	a = Auth{Kind: AuthBasic, Username: "u", Password: strp("p")}
	if err := a.PersistInlineSecrets("ds"); err != nil || *a.PasswordRef != "dstudio-rest-ds-basic" {
		t.Fatal(a, err)
	}
	if *a.SecretRef() != "dstudio-rest-ds-basic" {
		t.Fatal("secretref")
	}

	for _, bad := range []struct {
		a   Auth
		msg string
	}{
		{Auth{Kind: AuthBearer}, "bearer auth needs either `token` or `token_ref`"},
		{Auth{Kind: AuthAPIKey, Value: strp("v")}, "api_key auth needs either `header` or `query_param`"},
		{Auth{Kind: AuthAPIKey, QueryParam: strp("k")}, "api_key auth needs either `value` or `value_ref`"},
		{Auth{Kind: AuthBasic, Username: "u"}, "basic auth needs either `password` or `password_ref`"},
	} {
		if err := bad.a.PersistInlineSecrets("x"); err == nil || err.Error() != bad.msg {
			t.Fatal(err)
		}
	}
	none := Auth{Kind: AuthNone}
	if err := none.PersistInlineSecrets("x"); err != nil {
		t.Fatal(err)
	}
	if s, err := LoadAuthSecret(none); s != nil || err != nil {
		t.Fatal(s, err)
	}
	var ce *connector.Error
	if _, err := LoadAuthSecret(Auth{Kind: AuthAPIKey}); !errors.As(err, &ce) || ce.Message != "api_key value_ref missing" {
		t.Fatal(err)
	}
	if _, err := LoadAuthSecret(Auth{Kind: AuthBasic, PasswordRef: strp("nope")}); !errors.As(err, &ce) || ce.Kind != connector.ErrAuth {
		t.Fatal(err)
	}
	if _, err := LoadAuthSecret(Auth{Kind: AuthBearer}); err == nil {
		t.Fatal("expected")
	}
}

func TestSlug(t *testing.T) {
	for in, want := range map[string]string{"My API!": "my-api", "--a_b--": "a-b", "Ünï": "Ünï", "": ""} {
		if got := slug(in); got != want {
			t.Fatalf("%q → %q want %q", in, got, want)
		}
	}
}

type captured struct {
	method string
	path   string
	query  string
	header http.Header
}

func server(t *testing.T, status int, body string, cap *captured) *httptest.Server {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if cap != nil {
			*cap = captured{r.Method, r.URL.Path, r.URL.RawQuery, r.Header.Clone()}
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
	t.Cleanup(srv.Close)
	return srv
}

func TestQueryPushdownPaginationAndAuth(t *testing.T) {
	var cap captured
	srv := server(t, 200, `{"data":{"items":[{"id":1,"name":"a","tag":"x"},{"id":2,"name":"b","tag":"y"},"junk"]}}`, &cap)
	ds := DatasourceConfig{
		BaseURL:        srv.URL + "/v1/",
		Auth:           Auth{Kind: AuthAPIKey, Header: strp("X-Api-Key"), QueryParam: strp("api_key")},
		DefaultHeaders: map[string]string{"Accept": "application/json"},
	}
	dsc := DatasetConfig{
		Path:          "/users",
		Method:        "GET",
		QueryParams:   map[string]string{"expand": "full"},
		Headers:       map[string]string{"X-Extra": "1"},
		ListPath:      strp("/data/items"),
		ItemIDField:   "id",
		Pagination:    Pagination{Kind: PaginationPage, PageParam: "page", SizeParam: "size", StartPage: 1},
		FilterMapping: map[string]FilterParam{"name": {Param: "n", Operators: []models.FilterOperator{models.OpEquals}}},
	}
	c, _ := New(ds, dsc, strp("sekret"))
	params := connector.DefaultQueryParams()
	params.Limit = 10
	params.Offset = 20
	params.Search = strp("hello world")
	params.Filters = []models.DatasetFilter{
		{FieldName: "name", Operator: models.OpEquals, Value: "a"},
		{FieldName: "tag", Operator: models.OpEquals, Value: "x"},
	}
	env, err := c.Query(context.Background(), &params)
	if err != nil {
		t.Fatal(err)
	}
	if cap.path != "/v1/users" {
		t.Fatal(cap.path)
	}
	if cap.query != "api_key=sekret&expand=full&n=a&page=3&size=10&q=hello+world" {
		t.Fatal(cap.query)
	}
	if cap.header.Get("X-Api-Key") != "sekret" || cap.header.Get("Accept") != "application/json" || cap.header.Get("X-Extra") != "1" {
		t.Fatal(cap.header)
	}
	// "junk" dropped; tag filter applied in memory
	if len(env.Items) != 1 || env.Items[0]["name"] != "a" || env.Meta.Total != 1 || env.Meta.Offset != 20 {
		t.Fatalf("%+v", env)
	}

	params = connector.DefaultQueryParams()
	params.ItemID = strp("2")
	env, err = c.Query(context.Background(), &params)
	if err != nil || len(env.Items) != 1 || env.Items[0]["name"] != "b" {
		t.Fatal(env, err)
	}
}

func TestQueryBearerBasicOffset(t *testing.T) {
	var cap captured
	srv := server(t, 200, `[{"id":"1"}]`, &cap)
	c, _ := New(DatasourceConfig{BaseURL: srv.URL, Auth: Auth{Kind: AuthBearer}},
		DatasetConfig{Path: "items?fixed=1", Method: "POST", Pagination: Pagination{Kind: PaginationOffset, LimitParam: "limit", OffsetParam: "offset"}}, strp("t"))
	p := connector.DefaultQueryParams()
	env, err := c.Query(context.Background(), &p)
	if err != nil || len(env.Items) != 1 {
		t.Fatal(env, err)
	}
	if cap.method != "POST" || cap.header.Get("Authorization") != "Bearer t" || cap.query != "fixed=1&limit=50&offset=0" {
		t.Fatalf("%+v", cap)
	}

	c, _ = New(DatasourceConfig{BaseURL: srv.URL, Auth: Auth{Kind: AuthBasic, Username: "u"}},
		DatasetConfig{Path: "", Method: "GET", Pagination: Pagination{Kind: PaginationCursor}}, nil)
	if _, err := c.Query(context.Background(), &p); err != nil {
		t.Fatal(err)
	}
	if cap.header.Get("Authorization") != "Basic dTo=" {
		t.Fatal(cap.header.Get("Authorization"))
	}

	// single object body → one item; null list_path target → no items
	srv2 := server(t, 200, `{"id":"x","nested":null}`, nil)
	c, _ = New(DatasourceConfig{BaseURL: srv2.URL}, DatasetConfig{Path: "/", Method: "GET"}, nil)
	env, _ = c.Query(context.Background(), &p)
	if len(env.Items) != 1 {
		t.Fatal(env)
	}
	c, _ = New(DatasourceConfig{BaseURL: srv2.URL}, DatasetConfig{Path: "/", Method: "GET", ListPath: strp("/nested")}, nil)
	env, _ = c.Query(context.Background(), &p)
	if len(env.Items) != 0 {
		t.Fatal(env)
	}
}

func TestQueryErrors(t *testing.T) {
	p := connector.DefaultQueryParams()
	var ce *connector.Error

	srv := server(t, 401, "nope", nil)
	c, _ := New(DatasourceConfig{BaseURL: srv.URL}, DatasetConfig{Path: "/x", Method: "GET"}, nil)
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Kind != connector.ErrAuth || !strings.Contains(ce.Message, "REST "+srv.URL+"/x") {
		t.Fatal(err)
	}
	srv = server(t, 500, "boom", nil)
	c, _ = New(DatasourceConfig{BaseURL: srv.URL}, DatasetConfig{Path: "/x", Method: "GET"}, nil)
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Kind != connector.ErrUpstreamHTTP || ce.Status != 500 {
		t.Fatal(err)
	}
	srv = server(t, 200, "not json", nil)
	c, _ = New(DatasourceConfig{BaseURL: srv.URL}, DatasetConfig{Path: "/x", Method: "GET"}, nil)
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Kind != connector.ErrQuery {
		t.Fatal(err)
	}
	c, _ = New(DatasourceConfig{BaseURL: "not-a-url"}, DatasetConfig{Path: "/x", Method: "GET"}, nil)
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Kind != connector.ErrConfig {
		t.Fatal(err)
	}
	if err := c.TestConnection(context.Background()); !errors.As(err, &ce) || ce.Kind != connector.ErrConfig {
		t.Fatal(err)
	}
	c, _ = New(DatasourceConfig{BaseURL: srv.URL, Auth: Auth{Kind: AuthBearer}}, DatasetConfig{Path: "/x", Method: "GET"}, nil)
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Kind != connector.ErrAuth {
		t.Fatal(err)
	}
	if err := c.TestConnection(context.Background()); !errors.As(err, &ce) || ce.Kind != connector.ErrAuth {
		t.Fatal(err)
	}
	c, _ = New(DatasourceConfig{BaseURL: srv.URL, Auth: Auth{Kind: AuthAPIKey, Header: strp("X")}}, DatasetConfig{Path: "/x", Method: "GET"}, nil)
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Message != "api-key secret not loaded" {
		t.Fatal(err)
	}
	c, _ = New(DatasourceConfig{BaseURL: srv.URL, DefaultHeaders: map[string]string{"bad header": "x"}}, DatasetConfig{Path: "/x", Method: "GET"}, nil)
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Kind != connector.ErrConfig {
		t.Fatal(err)
	}
	c, _ = New(DatasourceConfig{BaseURL: srv.URL, DefaultHeaders: map[string]string{"X": "a\nb"}}, DatasetConfig{Path: "/x", Method: "GET"}, nil)
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Kind != connector.ErrConfig {
		t.Fatal(err)
	}
	c, _ = New(DatasourceConfig{BaseURL: srv.URL}, DatasetConfig{Path: "/x", Method: "BAD METHOD"}, nil)
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Kind != connector.ErrConfig {
		t.Fatal(err)
	}
	c, _ = New(DatasourceConfig{BaseURL: "http://127.0.0.1:1"}, DatasetConfig{Path: "/x", Method: "GET"}, nil)
	if _, err := c.Query(context.Background(), &p); !errors.As(err, &ce) || ce.Kind != connector.ErrQuery {
		t.Fatal(err)
	}
	if err := c.TestConnection(context.Background()); !errors.As(err, &ce) || ce.Kind != connector.ErrConnection {
		t.Fatal(err)
	}
}

func TestTestConnection(t *testing.T) {
	var methods []string
	status := map[string]int{"HEAD": 405, "GET": 200}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		methods = append(methods, r.Method+" "+r.URL.RawQuery)
		w.WriteHeader(status[r.Method])
	}))
	defer srv.Close()
	c, _ := New(DatasourceConfig{BaseURL: srv.URL, Auth: Auth{Kind: AuthAPIKey, QueryParam: strp("k")}}, DatasetConfig{Path: "", Method: "GET"}, strp("v"))
	if err := c.TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if strings.Join(methods, ",") != "HEAD k=v,GET k=v" {
		t.Fatal(methods)
	}
	status["HEAD"] = 200
	methods = nil
	if err := c.TestConnection(context.Background()); err != nil || len(methods) != 1 {
		t.Fatal(err, methods)
	}
	status["HEAD"], status["GET"] = 404, 404
	err := c.TestConnection(context.Background())
	if err == nil || err.Error() != "connection failed: HTTP 404 Not Found" {
		t.Fatal(err)
	}
	s, _ := c.Introspect(context.Background())
	if s.Fields == nil {
		t.Fatal("nil fields")
	}
}

func TestJSONPointer(t *testing.T) {
	doc := map[string]any{"a/b": map[string]any{"~": []any{"x", "y"}}}
	if v, ok := jsonPointer(doc, "/a~1b/~0/1"); !ok || v != "y" {
		t.Fatal(v)
	}
	for _, bad := range []string{"a", "/missing", "/a~1b/~0/01", "/a~1b/~0/5", "/a~1b/~0/+1", "/a~1b/~0/1/x"} {
		if _, ok := jsonPointer(doc, bad); ok {
			t.Fatal(bad)
		}
	}
	if v, ok := jsonPointer(doc, ""); !ok || v == nil {
		t.Fatal("root")
	}
}

func TestInferSchemaFromSample(t *testing.T) {
	s := InferSchemaFromSample(map[string]any{
		"id": json.Number("1"), "big": json.Number("18446744073709551615"), "score": json.Number("1.5"),
		"ok": true, "when": "2024-01-01T00:00:00Z", "name": "x", "tags": []any{}, "meta": map[string]any{}, "nil": nil,
		"f": 2.0, "g": 2.5,
	})
	got := map[string]connector.FieldType{}
	var names []string
	for _, f := range s.Fields {
		got[f.Name] = f.FieldType
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "big,f,g,id,meta,name,nil,ok,score,tags,when" {
		t.Fatal(names)
	}
	want := map[string]connector.FieldType{
		"id": connector.FieldInteger, "big": connector.FieldInteger, "score": connector.FieldFloat, "ok": connector.FieldBoolean,
		"when": connector.FieldDateTime, "name": connector.FieldString, "tags": connector.FieldJSON, "meta": connector.FieldJSON,
		"nil": connector.FieldString, "f": connector.FieldInteger, "g": connector.FieldFloat,
	}
	for k, w := range want {
		if got[k] != w {
			t.Fatalf("%s: %s want %s", k, got[k], w)
		}
	}
	if s := InferSchemaFromSample([]any{}); len(s.Fields) != 0 || s.Fields == nil {
		t.Fatal(s)
	}
	if looksLikeDatetime("abcd-ef-gh") {
		t.Fatal("datetime")
	}
}
