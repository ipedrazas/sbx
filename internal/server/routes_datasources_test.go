package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/ipedrazas/sbx/internal/actions"
	"github.com/ipedrazas/sbx/internal/agentdir"
	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/secretstore"
)

// dsCreate creates a datasource via the API and returns its id.
func dsCreate(t *testing.T, e *testEnv, body map[string]any) int64 {
	t.Helper()
	rec := e.manage("POST", "/manage/datasources", body)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", rec.Code, rec.Body.String())
	}
	return int64(decode(t, rec)["id"].(float64))
}

func TestDatasourcesNeedsLinearKeyMigration(t *testing.T) {
	cases := []struct {
		t    models.DatasourceType
		cfg  map[string]any
		want bool
	}{
		{models.DatasourceLinear, map[string]any{"api_key": "lin_api_xyz"}, true},
		{models.DatasourceLinear, map[string]any{"api_key_ref": "dstudio-linear-x-apikey"}, false},
		{models.DatasourceLinear, map[string]any{"api_key": "lin_api_xyz", "api_key_ref": "r"}, false},
		{models.DatasourceLinear, map[string]any{}, false},
		{models.DatasourceLinear, map[string]any{"api_key": ""}, false},
		{models.DatasourceRest, map[string]any{"api_key": "lin_api_xyz"}, false},
	}
	for i, c := range cases {
		if got := needsLinearKeyMigration(c.t, c.cfg); got != c.want {
			t.Errorf("case %d: got %v", i, got)
		}
	}
}

func TestDatasourcesSecretSlug(t *testing.T) {
	if got := secretSlug("My Linear!"); got != "my-linear" {
		t.Fatal(got)
	}
	if got := secretSlug("ACME-Prod"); got != "acme-prod" {
		t.Fatal(got)
	}
}

func TestDatasourcesCRUD(t *testing.T) {
	e := newTestEnv(t)
	rec := e.manage("GET", "/manage/datasources/", nil)
	if rec.Code != 200 || rec.Body.String() != `{"datasources":[]}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	id := dsCreate(t, e, map[string]any{"name": "pg", "ds_type": "postgres",
		"config": map[string]any{"connection_string": "postgres://u@127.0.0.1:1/db"}})

	rec = e.manage("GET", fmt.Sprintf("/manage/datasources/%d", id), nil)
	if m := decode(t, rec); rec.Code != 200 || m["name"] != "pg" || m["ds_type"] != "postgres" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := e.manage("GET", "/manage/datasources/999", nil); rec.Code != 404 ||
		decode(t, rec)["error"] != "datasource not found" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := e.manage("GET", "/manage/datasources/abc", nil); rec.Code != 400 {
		t.Fatal(rec.Code)
	}

	upd := map[string]any{"name": "pg2", "config": map[string]any{"connection_string": "postgres://x"}}
	if rec := e.manage("PUT", fmt.Sprintf("/manage/datasources/%d", id), upd); rec.Code != 200 ||
		rec.Body.String() != `{"updated":true}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := e.manage("POST", fmt.Sprintf("/manage/datasources/%d/update", id), upd); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if rec := e.manage("PUT", "/manage/datasources/999", upd); rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	if rec := e.manage("PUT", fmt.Sprintf("/manage/datasources/%d", id), map[string]any{"name": "x"}); rec.Code != 422 {
		t.Fatal(rec.Code)
	}

	rec = e.manage("GET", "/manage/datasources", nil)
	if l := decode(t, rec)["datasources"].([]any); len(l) != 1 || l[0].(map[string]any)["name"] != "pg2" {
		t.Fatal(rec.Body.String())
	}

	if rec := e.manage("POST", fmt.Sprintf("/manage/datasources/%d/delete", id), nil); rec.Code != 200 ||
		rec.Body.String() != `{"deleted":true}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	// Deleting a missing row reports the dependent-datasets conflict (Rust parity).
	if rec := e.manage("DELETE", fmt.Sprintf("/manage/datasources/%d", id), nil); rec.Code != 409 {
		t.Fatal(rec.Code)
	}
}

func TestDatasourcesCreateValidation(t *testing.T) {
	e := newTestEnv(t)
	for _, body := range []map[string]any{
		{"ds_type": "postgres", "config": map[string]any{}},
		{"name": "x", "config": map[string]any{}},
		{"name": "x", "ds_type": "postgres"},
	} {
		if rec := e.manage("POST", "/manage/datasources", body); rec.Code != 422 {
			t.Fatalf("%v: %d", body, rec.Code)
		}
	}
	if rec := e.manage("POST", "/manage/datasources", "{bad"); rec.Code != 400 {
		t.Fatal(rec.Code)
	}
	// REST config must parse.
	rec := e.manage("POST", "/manage/datasources", map[string]any{"name": "r", "ds_type": "rest", "config": "nope"})
	if rec.Code != 400 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rec = e.manage("POST", "/manage/datasources", map[string]any{"name": "r", "ds_type": "rest",
		"config": map[string]any{"base_url": "x", "auth": map[string]any{"kind": "oauth"}}})
	if rec.Code != 400 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestDatasourcesRestSecretsNormalizedAndScrubbed(t *testing.T) {
	e := newTestEnv(t)
	id := dsCreate(t, e, map[string]any{"name": "My API", "ds_type": "rest", "config": map[string]any{
		"base_url": "https://x", "auth": map[string]any{"kind": "bearer", "token": "tok"}}})
	ds, _ := crud.GetDatasource(context.Background(), e.srv.db, id)
	auth := ds.Config["auth"].(map[string]any)
	if _, ok := auth["token"]; ok || auth["token_ref"] != "dstudio-rest-my-api-bearer" {
		t.Fatalf("%v", auth)
	}
	if b, _ := secretstore.Get("dstudio-rest-my-api-bearer"); string(b) != "tok" {
		t.Fatal("secret not stored")
	}
	if rec := e.manage("DELETE", fmt.Sprintf("/manage/datasources/%d", id), nil); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if _, err := secretstore.Get("dstudio-rest-my-api-bearer"); err == nil {
		t.Fatal("secret not scrubbed")
	}
}

func TestDatasourcesLinearKeyNormalizedSeededAndScrubbed(t *testing.T) {
	e := newTestEnv(t)
	id := dsCreate(t, e, map[string]any{"name": "Lin Prod", "ds_type": "linear",
		"config": map[string]any{"api_key": "lin_api_1"}})
	ds, _ := crud.GetDatasource(context.Background(), e.srv.db, id)
	if _, ok := ds.Config["api_key"]; ok || ds.Config["api_key_ref"] != "dstudio-linear-lin-prod-apikey" {
		t.Fatalf("%v", ds.Config)
	}
	if b, _ := secretstore.Get("dstudio-linear-lin-prod-apikey"); string(b) != "lin_api_1" {
		t.Fatal("secret not stored")
	}
	acts, _ := actions.ListActions(context.Background(), e.srv.db, &id)
	if len(acts) == 0 {
		t.Fatal("expected predefined actions to be seeded")
	}

	// Update re-normalizes; an empty api_key is dropped without storing.
	rec := e.manage("PUT", fmt.Sprintf("/manage/datasources/%d", id), map[string]any{"name": "Lin Prod",
		"config": map[string]any{"api_key": "", "api_key_ref": "dstudio-linear-lin-prod-apikey"}})
	if rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	ds, _ = crud.GetDatasource(context.Background(), e.srv.db, id)
	if _, ok := ds.Config["api_key"]; ok {
		t.Fatalf("%v", ds.Config)
	}

	if rec := e.manage("DELETE", fmt.Sprintf("/manage/datasources/%d", id), nil); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if _, err := secretstore.Get("dstudio-linear-lin-prod-apikey"); err == nil {
		t.Fatal("secret not scrubbed")
	}
}

func TestDatasourcesMigrateLinearAPIKeys(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	id, _ := crud.CreateDatasource(ctx, e.srv.db, "Old Lin", "linear", `{"api_key":"lin_old"}`, nil)
	_, _ = crud.CreateDatasource(ctx, e.srv.db, "Done", "linear", `{"api_key_ref":"r"}`, nil)
	_, _ = crud.CreateDatasource(ctx, e.srv.db, "R", "rest", `{"api_key":"x"}`, nil)
	if n := MigrateLinearAPIKeys(ctx, e.srv.db); n != 1 {
		t.Fatalf("migrated %d", n)
	}
	ds, _ := crud.GetDatasource(ctx, e.srv.db, id)
	if ds.Config["api_key_ref"] != "dstudio-linear-old-lin-apikey" || ds.Config["api_key"] != nil {
		t.Fatalf("%v", ds.Config)
	}
	if n := MigrateLinearAPIKeys(ctx, e.srv.db); n != 0 {
		t.Fatalf("second run migrated %d", n)
	}
}

func TestDatasourcesTestConnection(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer up.Close()
	e := newTestEnv(t)
	id := dsCreate(t, e, map[string]any{"name": "r", "ds_type": "rest", "config": map[string]any{"base_url": up.URL}})
	rec := e.manage("POST", fmt.Sprintf("/manage/datasources/%d/test", id), nil)
	if rec.Code != 200 || rec.Body.String() != `{"status":"ok"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := e.manage("POST", "/manage/datasources/999/test", nil); rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	// Linear without credentials → connector config error (500).
	lid := dsCreate(t, e, map[string]any{"name": "l", "ds_type": "linear", "config": map[string]any{}})
	if rec := e.manage("POST", fmt.Sprintf("/manage/datasources/%d/test", lid), nil); rec.Code != 500 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestDatasourcesPostgresListing(t *testing.T) {
	e := newTestEnv(t)
	rid := dsCreate(t, e, map[string]any{"name": "r", "ds_type": "rest", "config": map[string]any{"base_url": "x"}})
	rec := e.manage("GET", fmt.Sprintf("/manage/datasources/%d/tables", rid), nil)
	if rec.Code != 400 || decode(t, rec)["error"] != "table listing is only available for Postgres datasources" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rec = e.manage("GET", fmt.Sprintf("/manage/datasources/%d/tables/t/columns", rid), nil)
	if rec.Code != 400 || decode(t, rec)["error"] != "column listing is only available for Postgres datasources" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := e.manage("GET", "/manage/datasources/999/tables", nil); rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	if rec := e.manage("GET", fmt.Sprintf("/manage/datasources/%d/tables?limit=x", rid), nil); rec.Code != 400 {
		t.Fatal(rec.Code)
	}

	bad := dsCreate(t, e, map[string]any{"name": "pgbad", "ds_type": "postgres", "config": map[string]any{}})
	if rec := e.manage("GET", fmt.Sprintf("/manage/datasources/%d/tables", bad), nil); rec.Code != 400 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	pg := dsCreate(t, e, map[string]any{"name": "pg", "ds_type": "postgres",
		"config": map[string]any{"connection_string": "postgres://u:p@127.0.0.1:1/db?connect_timeout=1"}})
	if rec := e.manage("GET", fmt.Sprintf("/manage/datasources/%d/tables?limit=9999&offset=-3", pg), nil); rec.Code != 502 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := e.manage("GET", fmt.Sprintf("/manage/datasources/%d/tables/users/columns", pg), nil); rec.Code != 502 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestDatasourcesPagination(t *testing.T) {
	r := httptest.NewRequest("GET", "/?limit=9999&offset=-3", nil)
	l, o, err := dsPagination(r)
	if err != nil || l != 500 || o != 0 {
		t.Fatal(l, o, err)
	}
	r = httptest.NewRequest("GET", "/?limit=0", nil)
	if l, _, _ := dsPagination(r); l != 1 {
		t.Fatal(l)
	}
	r = httptest.NewRequest("GET", "/", nil)
	if l, o, _ := dsPagination(r); l != 100 || o != 0 {
		t.Fatal(l, o)
	}
	r = httptest.NewRequest("GET", "/?offset=z", nil)
	if _, _, err := dsPagination(r); err == nil {
		t.Fatal("expected error")
	}
}

func TestDatasourcesEntities(t *testing.T) {
	e := newTestEnv(t)
	lid := dsCreate(t, e, map[string]any{"name": "l", "ds_type": "linear", "config": map[string]any{}})
	rec := e.manage("GET", fmt.Sprintf("/manage/datasources/%d/entities", lid), nil)
	if ents := decode(t, rec)["entities"].([]any); rec.Code != 200 || len(ents) == 0 {
		t.Fatal(rec.Body.String())
	}
	pid := dsCreate(t, e, map[string]any{"name": "p", "ds_type": "postgres", "config": map[string]any{}})
	rec = e.manage("GET", fmt.Sprintf("/manage/datasources/%d/entities", pid), nil)
	if rec.Body.String() != `{"entities":[]}` {
		t.Fatal(rec.Body.String())
	}
	if rec := e.manage("GET", "/manage/datasources/999/entities", nil); rec.Code != 404 {
		t.Fatal(rec.Code)
	}
}

func TestDatasourcesRestSample(t *testing.T) {
	up := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[{"id":1,"name":"a"},{"id":2,"name":"b"}]`))
	}))
	defer up.Close()
	e := newTestEnv(t)
	id := dsCreate(t, e, map[string]any{"name": "r", "ds_type": "rest", "config": map[string]any{"base_url": up.URL}})
	rec := e.manage("POST", fmt.Sprintf("/manage/datasources/%d/rest/sample", id),
		map[string]any{"path": "/users", "sample_limit": 1})
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if fields := m["schema"].(map[string]any)["fields"].([]any); len(fields) != 2 {
		t.Fatalf("%v", m)
	}
	if len(m["sample"].([]any)) == 0 {
		t.Fatalf("%v", m)
	}

	// Empty response → empty schema.
	up2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`[]`))
	}))
	defer up2.Close()
	id2 := dsCreate(t, e, map[string]any{"name": "r2", "ds_type": "rest", "config": map[string]any{"base_url": up2.URL}})
	rec = e.manage("POST", fmt.Sprintf("/manage/datasources/%d/rest/sample", id2), map[string]any{"path": "/x"})
	if rec.Code != 200 || !strings.Contains(rec.Body.String(), `"schema":{"fields":[]}`) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	if rec := e.manage("POST", fmt.Sprintf("/manage/datasources/%d/rest/sample", id),
		map[string]any{"path": "/x", "sample_limit": "many"}); rec.Code != 422 {
		t.Fatal(rec.Code)
	}
	if rec := e.manage("POST", fmt.Sprintf("/manage/datasources/%d/rest/sample", id),
		map[string]any{"method": 5}); rec.Code != 400 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := e.manage("POST", "/manage/datasources/999/rest/sample", map[string]any{"path": "/x"}); rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	pid := dsCreate(t, e, map[string]any{"name": "p", "ds_type": "postgres", "config": map[string]any{}})
	rec = e.manage("POST", fmt.Sprintf("/manage/datasources/%d/rest/sample", pid), map[string]any{"path": "/x"})
	if rec.Code != 400 || decode(t, rec)["error"] != "sample is only available for REST datasources" {
		t.Fatal(rec.Body.String())
	}
}

func TestDatasourcesRestSpecEndpoints(t *testing.T) {
	e := newTestEnv(t)
	spec := `{"openapi":"3.0.0","info":{"title":"t","version":"1"},"paths":{"/users":{"get":{"responses":{"200":{}}}}}}`
	id := dsCreate(t, e, map[string]any{"name": "r", "ds_type": "rest", "config": map[string]any{
		"base_url": "https://x", "spec": map[string]any{"kind": "openapi", "inline": spec}}})
	rec := e.manage("GET", fmt.Sprintf("/manage/datasources/%d/rest/spec/endpoints", id), nil)
	if m := decode(t, rec); rec.Code != 200 || m["total"].(float64) != 1 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	noSpec := dsCreate(t, e, map[string]any{"name": "r2", "ds_type": "rest", "config": map[string]any{"base_url": "x"}})
	rec = e.manage("GET", fmt.Sprintf("/manage/datasources/%d/rest/spec/endpoints", noSpec), nil)
	if rec.Code != 400 || decode(t, rec)["error"] != "datasource has no OpenAPI spec configured" {
		t.Fatal(rec.Body.String())
	}
	badSpec := dsCreate(t, e, map[string]any{"name": "r3", "ds_type": "rest", "config": map[string]any{
		"base_url": "x", "spec": map[string]any{"kind": "openapi", "inline": "  "}}})
	if rec := e.manage("GET", fmt.Sprintf("/manage/datasources/%d/rest/spec/endpoints", badSpec), nil); rec.Code != 502 {
		t.Fatal(rec.Code)
	}
	pid := dsCreate(t, e, map[string]any{"name": "p", "ds_type": "postgres", "config": map[string]any{}})
	if rec := e.manage("GET", fmt.Sprintf("/manage/datasources/%d/rest/spec/endpoints", pid), nil); rec.Code != 400 {
		t.Fatal(rec.Code)
	}
	if rec := e.manage("GET", "/manage/datasources/999/rest/spec/endpoints", nil); rec.Code != 404 {
		t.Fatal(rec.Code)
	}
}

func TestDatasourcesReadOnlyToggleResyncsAgents(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	id := dsCreate(t, e, map[string]any{"name": "r", "ds_type": "rest", "config": map[string]any{"base_url": "x"}})
	other, _ := crud.CreateDataset(ctx, e.srv.db, "other", id, "pull_through", nil, nil, nil, nil, nil)
	_ = other
	dsID, err := crud.CreateDataset(ctx, e.srv.db, "users", id, "pull_through", nil, nil, nil, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	agentID, _ := crud.CreateAgent(ctx, e.srv.db, "Bot", nil)
	if _, err := crud.BindAgentDataset(ctx, e.srv.db, agentID, dsID, "users", nil, nil); err != nil {
		t.Fatal(err)
	}

	rec := e.manage("POST", fmt.Sprintf("/manage/datasources/%d/read-only", id), nil)
	if rec.Code != 200 || rec.Body.String() != `{"read_only":true,"updated":true}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if _, err := os.Stat(agentdir.AgentPath(e.srv.dataDir, "Bot")); err != nil {
		t.Fatalf("agent dir not synced: %v", err)
	}
	ds, _ := crud.GetDatasource(ctx, e.srv.db, id)
	if !ds.ReadOnly {
		t.Fatal("not read-only")
	}
	rec = e.manage("POST", fmt.Sprintf("/manage/datasources/%d/read-write", id), nil)
	if rec.Code != 200 || rec.Body.String() != `{"read_only":false,"updated":true}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rec = e.manage("POST", "/manage/datasources/999/read-only", nil)
	if rec.Code != 404 || rec.Body.String() != `{"error":"not found"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := e.manage("POST", "/manage/datasources/x/read-only", nil); rec.Code != 400 {
		t.Fatal(rec.Code)
	}
}
