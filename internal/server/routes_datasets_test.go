package server

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ipedrazas/sbx/internal/agentdir"
	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/secretstore"
)

// newDatasetsEnv is newTestEnv plus a cleanup that waits for background seed
// goroutines before the temp data dir is removed.
func newDatasetsEnv(t *testing.T) *testEnv {
	t.Helper()
	e := newTestEnv(t)
	t.Cleanup(datasetsBackground.Wait)
	return e
}

func dsExpect(t *testing.T, rec *httptest.ResponseRecorder, code int) map[string]any {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("want %d, got %d: %s", code, rec.Code, rec.Body.String())
	}
	if strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		return decode(t, rec)
	}
	return nil
}

func dsNum(t *testing.T, v any) int64 {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("not a number: %#v", v)
	}
	return int64(f)
}

func (e *testEnv) dsCreateDatasource(name, dsType, config string) int64 {
	e.t.Helper()
	id, err := crud.CreateDatasource(context.Background(), e.srv.db, name, dsType, config, nil)
	if err != nil {
		e.t.Fatal(err)
	}
	return id
}

func (e *testEnv) dsCreateDataset(body map[string]any) int64 {
	e.t.Helper()
	m := dsExpect(e.t, e.manage("POST", "/manage/datasets", body), http.StatusCreated)
	// Let the initial seed finish: a concurrent first open of a fresh cache
	// file can hit SQLITE_BUSY while it switches to WAL.
	datasetsBackground.Wait()
	return dsNum(e.t, m["id"])
}

// dsUploadDataset creates a file_upload dataset and uploads the given CSV.
func (e *testEnv) dsUploadDataset(name, csv string) int64 {
	e.t.Helper()
	dsID := e.dsCreateDatasource("files-"+name, "file_upload", "{}")
	id := e.dsCreateDataset(map[string]any{"name": name, "datasource_id": dsID, "mode": "mirror"})
	m := dsExpect(e.t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/upload", id), map[string]any{
		"filename": "data.csv", "content": base64.StdEncoding.EncodeToString([]byte(csv)),
	}), http.StatusOK)
	_ = m
	return id
}

const dsSampleCSV = "id,name,email,age\n1,Alice,alice@example.com,30\n2,Bob,bob@example.com,25\n3,Carol,carol@example.com,41\n"

func TestDatasetsCRUD(t *testing.T) {
	e := newDatasetsEnv(t)
	dsExpect(t, e.manage("POST", "/manage/datasets", map[string]any{"name": "x", "datasource_id": 999}), http.StatusBadRequest)
	dsExpect(t, e.manage("POST", "/manage/datasets", map[string]any{"datasource_id": 1}), http.StatusUnprocessableEntity)
	dsExpect(t, e.manage("POST", "/manage/datasets", map[string]any{"name": "x"}), http.StatusUnprocessableEntity)

	dsID := e.dsCreateDatasource("files", "file_upload", "{}")
	id := e.dsCreateDataset(map[string]any{"name": "People", "datasource_id": dsID, "description": "d", "source_config": map[string]any{"a": 1}})

	m := dsExpect(t, e.manage("GET", "/manage/datasets/", nil), http.StatusOK)
	list := m["datasets"].([]any)
	if len(list) != 1 || list[0].(map[string]any)["datasource_name"] != "files" {
		t.Fatalf("list: %v", m)
	}
	m = dsExpect(t, e.manage("GET", fmt.Sprintf("/manage/datasets/%d", id), nil), http.StatusOK)
	if m["datasource_type"] != "file_upload" || m["dataset"].(map[string]any)["mode"] != "pull_through" {
		t.Fatalf("get: %v", m)
	}
	if _, ok := m["fields"].([]any); !ok {
		t.Fatalf("fields should be an array: %v", m)
	}
	dsExpect(t, e.manage("GET", "/manage/datasets/999", nil), http.StatusNotFound)
	dsExpect(t, e.manage("GET", "/manage/datasets/abc", nil), http.StatusBadRequest)

	// Update (PUT and POST alias).
	dsExpect(t, e.manage("PUT", fmt.Sprintf("/manage/datasets/%d", id), map[string]any{"name": "P2", "mode": "pull_through"}), http.StatusOK)
	dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/update", id), map[string]any{"name": "P3", "mode": "synthetic", "max_page_bytes": 1000}), http.StatusOK)
	dsExpect(t, e.manage("PUT", "/manage/datasets/999", map[string]any{"name": "P2", "mode": "cached"}), http.StatusNotFound)
	dsExpect(t, e.manage("PUT", fmt.Sprintf("/manage/datasets/%d", id), map[string]any{"name": "P2"}), http.StatusUnprocessableEntity)
	dsExpect(t, e.manage("PUT", fmt.Sprintf("/manage/datasets/%d", id), map[string]any{"mode": "cached"}), http.StatusUnprocessableEntity)
	dsExpect(t, e.manage("PUT", fmt.Sprintf("/manage/datasets/%d", id), "{bad"), http.StatusBadRequest)
	m = dsExpect(t, e.manage("GET", fmt.Sprintf("/manage/datasets/%d", id), nil), http.StatusOK)
	if d := m["dataset"].(map[string]any); d["name"] != "P3" || d["mode"] != "synthetic" {
		t.Fatalf("after update: %v", d)
	}

	// Fields.
	fields := []map[string]any{{"source_name": "a", "display_name": "A", "field_type": "string", "visible": true, "pii_policy": "mask"}}
	dsExpect(t, e.manage("PUT", fmt.Sprintf("/manage/datasets/%d/fields", id), fields), http.StatusOK)
	m = dsExpect(t, e.manage("GET", fmt.Sprintf("/manage/datasets/%d/fields", id), nil), http.StatusOK)
	if fl := m["fields"].([]any); len(fl) != 1 || fl[0].(map[string]any)["pii_policy"] != "mask" {
		t.Fatalf("fields: %v", m)
	}
	bad := []map[string]any{{"source_name": "a", "display_name": "A", "field_type": "string", "pii_policy": "bogus"}}
	m = dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/fields", id), bad), http.StatusBadRequest)
	if !strings.Contains(m["error"].(string), "field 'a' has invalid pii_policy 'bogus' — must be one of: none, mask, drop, generalize") {
		t.Fatalf("error: %v", m)
	}

	// Filters.
	filters := []map[string]any{{"field_name": "a", "operator": "equals", "value": "x", "logic": "and", "group_index": 0}}
	dsExpect(t, e.manage("PUT", fmt.Sprintf("/manage/datasets/%d/filters", id), filters), http.StatusOK)
	m = dsExpect(t, e.manage("GET", fmt.Sprintf("/manage/datasets/%d/filters", id), nil), http.StatusOK)
	if len(m["filters"].([]any)) != 1 {
		t.Fatalf("filters: %v", m)
	}
	dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/filters", id), "nope"), http.StatusBadRequest)

	// Pause / resume / metrics.
	m = dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/pause", id), nil), http.StatusOK)
	if m["enabled"] != false {
		t.Fatalf("pause: %v", m)
	}
	dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/resume", id), nil), http.StatusOK)
	m = dsExpect(t, e.manage("POST", "/manage/datasets/999/pause", nil), http.StatusNotFound)
	if m["error"] != "not found" {
		t.Fatalf("pause 404: %v", m)
	}
	dsExpect(t, e.manage("GET", fmt.Sprintf("/manage/datasets/%d/metrics", id), nil), http.StatusOK)

	// Require PII review.
	m = dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/require-pii-review", id), map[string]any{"require": true}), http.StatusOK)
	if m["require_pii_review"] != true {
		t.Fatalf("require: %v", m)
	}
	dsExpect(t, e.manage("POST", "/manage/datasets/999/require-pii-review", map[string]any{"require": true}), http.StatusNotFound)
	dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/require-pii-review", id), map[string]any{}), http.StatusUnprocessableEntity)

	// Duplicate: with and without a body.
	m = dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/duplicate", id), map[string]any{"name": "Copy"}), http.StatusCreated)
	dupID := dsNum(t, m["id"])
	dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/duplicate", id), nil), http.StatusCreated)
	dsExpect(t, e.manage("POST", "/manage/datasets/999/duplicate", nil), http.StatusNotFound)
	dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/duplicate", id), "{bad"), http.StatusBadRequest)

	// Delete removes the cache file and cancels any tracked sync.
	cachePath := filepath.Join(e.srv.dataDir, fmt.Sprintf("cache_%d.db", dupID))
	if err := os.WriteFile(cachePath, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	cancelled := false
	e.srv.syncMu.Lock()
	e.srv.syncCancels[dupID] = func() { cancelled = true }
	e.srv.syncMu.Unlock()
	dsExpect(t, e.manage("DELETE", fmt.Sprintf("/manage/datasets/%d", dupID), nil), http.StatusOK)
	if _, err := os.Stat(cachePath); !os.IsNotExist(err) || !cancelled {
		t.Fatalf("cache file should be removed (%v) and sync cancelled (%v)", err, cancelled)
	}
	dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/delete", dupID), nil), http.StatusNotFound)
}

func TestDatasetsUploadPreviewStats(t *testing.T) {
	e := newDatasetsEnv(t)
	id := e.dsUploadDataset("people", dsSampleCSV)

	m := dsExpect(t, e.manage("GET", fmt.Sprintf("/manage/datasets/%d/fields", id), nil), http.StatusOK)
	if len(m["fields"].([]any)) != 4 {
		t.Fatalf("fields: %v", m)
	}

	m = dsExpect(t, e.manage("GET", fmt.Sprintf("/manage/datasets/%d/preview?sort_by=name&sort_dir=DESC&limit=2", id), nil), http.StatusOK)
	items := m["items"].([]any)
	if len(items) != 2 || items[0].(map[string]any)["name"] != "Carol" {
		t.Fatalf("preview sorted: %v", m)
	}
	m = dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/preview", id), map[string]any{"q": "bob"}), http.StatusOK)
	if items := m["items"].([]any); len(items) != 1 || items[0].(map[string]any)["name"] != "Bob" {
		t.Fatalf("preview search: %v", m)
	}
	dsExpect(t, e.manage("GET", fmt.Sprintf("/manage/datasets/%d/preview?limit=x", id), nil), http.StatusBadRequest)
	dsExpect(t, e.manage("GET", "/manage/datasets/999/preview", nil), http.StatusNotFound)

	// Field policies apply to preview: hide age, drop email.
	m = dsExpect(t, e.manage("GET", fmt.Sprintf("/manage/datasets/%d/fields", id), nil), http.StatusOK)
	var fields []map[string]any
	for _, f := range m["fields"].([]any) {
		fm := f.(map[string]any)
		switch fm["source_name"] {
		case "age":
			fm["visible"] = false
		case "email":
			fm["pii_policy"] = "drop"
		}
		fields = append(fields, fm)
	}
	dsExpect(t, e.manage("PUT", fmt.Sprintf("/manage/datasets/%d/fields", id), fields), http.StatusOK)
	m = dsExpect(t, e.manage("GET", fmt.Sprintf("/manage/datasets/%d/preview", id), nil), http.StatusOK)
	row := m["items"].([]any)[0].(map[string]any)
	if _, ok := row["age"]; ok {
		t.Fatalf("age should be hidden: %v", row)
	}
	if _, ok := row["email"]; ok {
		t.Fatalf("email should be dropped: %v", row)
	}

	m = dsExpect(t, e.manage("GET", fmt.Sprintf("/manage/datasets/%d/stats", id), nil), http.StatusOK)
	if dsNum(t, m["total_rows"]) != 3 || dsNum(t, m["visible_fields"]) != 3 || m["estimated_page_bytes"] == nil {
		t.Fatalf("stats: %v", m)
	}
	dsExpect(t, e.manage("GET", "/manage/datasets/999/stats", nil), http.StatusNotFound)

	// Stats on an empty cache.
	dsID := e.dsCreateDatasource("empty", "file_upload", "{}")
	emptyID := e.dsCreateDataset(map[string]any{"name": "E", "datasource_id": dsID, "auto_introspect": false})
	m = dsExpect(t, e.manage("GET", fmt.Sprintf("/manage/datasets/%d/stats", emptyID), nil), http.StatusOK)
	if dsNum(t, m["total_rows"]) != 0 || m["estimated_pages"] == nil {
		t.Fatalf("empty stats: %v", m)
	}
	// Pull-through preview against file_upload is unsupported → 502.
	dsExpect(t, e.manage("GET", fmt.Sprintf("/manage/datasets/%d/preview", emptyID), nil), http.StatusBadGateway)
}

func TestDatasetsUploadErrors(t *testing.T) {
	e := newDatasetsEnv(t)
	dsID := e.dsCreateDatasource("files", "file_upload", "{}")
	id := e.dsCreateDataset(map[string]any{"name": "F", "datasource_id": dsID})
	path := fmt.Sprintf("/manage/datasets/%d/upload", id)
	enc := func(s string) string { return base64.StdEncoding.EncodeToString([]byte(s)) }

	m := dsExpect(t, e.manage("POST", path, map[string]any{"filename": "x.csv", "content": "!!!"}), http.StatusBadRequest)
	if !strings.HasPrefix(m["error"].(string), "invalid base64:") {
		t.Fatalf("%v", m)
	}
	m = dsExpect(t, e.manage("POST", path, map[string]any{"filename": "x.xml", "content": enc("a")}), http.StatusBadRequest)
	if m["error"] != "unsupported file format: .xml (expected .csv, .jsonl, .ndjson, or .json)" {
		t.Fatalf("%v", m)
	}
	dsExpect(t, e.manage("POST", path, map[string]any{"filename": "x.json", "content": enc("{}")}), http.StatusBadRequest)
	dsExpect(t, e.manage("POST", path, map[string]any{"filename": "x.csv"}), http.StatusUnprocessableEntity)
	dsExpect(t, e.manage("POST", path, map[string]any{"content": "x"}), http.StatusUnprocessableEntity)
	dsExpect(t, e.manage("POST", "/manage/datasets/999/upload", map[string]any{"filename": "x.csv", "content": enc("a")}), http.StatusNotFound)

	// JSONL with mixed ids.
	m = dsExpect(t, e.manage("POST", path, map[string]any{"filename": "X.NDJSON", "content": enc(`{"id":5,"v":1}` + "\n" + `{"v":2}` + "\n" + `{"id":"","v":3}`)}), http.StatusOK)
	if dsNum(t, m["rows_loaded"]) != 3 {
		t.Fatalf("%v", m)
	}

	restID := e.dsCreateDatasource("api", "rest", `{"base_url":"http://127.0.0.1:1","auth":{"kind":"none"}}`)
	rid := e.dsCreateDataset(map[string]any{"name": "R", "datasource_id": restID, "auto_introspect": false})
	m = dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/upload", rid), map[string]any{"filename": "x.csv", "content": enc("a")}), http.StatusBadRequest)
	if m["error"] != "upload is only available for file_upload datasources" {
		t.Fatalf("%v", m)
	}
}

func TestDatasetsEnsureRowIDs(t *testing.T) {
	rows := []any{
		map[string]any{"id": "a"},
		map[string]any{"id": float64(7)},
		map[string]any{"id": nil},
		map[string]any{},
		"not-an-object",
	}
	got := datasetsEnsureRowIDs(rows)
	want := []string{"a", "7", "row_2", "row_3"}
	if len(got) != len(want) {
		t.Fatalf("got %v", got)
	}
	for i, w := range want {
		if got[i]["id"] != w {
			t.Fatalf("row %d: got %v want %s", i, got[i]["id"], w)
		}
	}
}

func TestDatasetsSlug(t *testing.T) {
	cases := map[string]string{
		"My Dataset!":   "my-dataset",
		"  --Foo__Bar ": "foo-bar",
		"":              "dataset",
		"ñññ":           "dataset",
		"ABC123":        "abc123",
	}
	for in, want := range cases {
		if got := datasetsSlug(in); got != want {
			t.Errorf("datasetsSlug(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestDatasetsDownload(t *testing.T) {
	e := newDatasetsEnv(t)
	id := e.dsUploadDataset("My People", dsSampleCSV)
	path := fmt.Sprintf("/manage/datasets/%d/download", id)

	rec := e.manage("POST", path, map[string]any{})
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Disposition") != `attachment; filename="my-people.csv"` {
		t.Fatalf("csv: %d %v %s", rec.Code, rec.Header(), rec.Body.String())
	}
	if lines := strings.Split(strings.TrimSpace(rec.Body.String()), "\n"); len(lines) != 4 {
		t.Fatalf("csv body: %q", rec.Body.String())
	}

	rec = e.manage("POST", path, map[string]any{"format": "json"})
	var arr []map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &arr); err != nil || len(arr) != 3 {
		t.Fatalf("json body: %v %s", err, rec.Body.String())
	}
	rec = e.manage("POST", path, map[string]any{"format": "jsonl"})
	if n := strings.Count(rec.Body.String(), "\n"); n != 3 {
		t.Fatalf("jsonl body: %q", rec.Body.String())
	}

	dir := t.TempDir()
	m := dsExpect(t, e.manage("POST", path, map[string]any{"format": "json", "output_dir": dir}), http.StatusOK)
	if dsNum(t, m["rows"]) != 3 || m["format"] != "json" || m["path"] != filepath.Join(dir, "my-people.json") {
		t.Fatalf("output_dir: %v", m)
	}
	if _, err := os.Stat(filepath.Join(dir, "my-people.json")); err != nil {
		t.Fatal(err)
	}
	m = dsExpect(t, e.manage("POST", path, map[string]any{"output_dir": filepath.Join(dir, "missing", "x")}), http.StatusInternalServerError)
	if !strings.HasPrefix(m["error"].(string), "failed to write my-people.csv") {
		t.Fatalf("%v", m)
	}

	m = dsExpect(t, e.manage("POST", path, map[string]any{"format": "xml"}), http.StatusBadRequest)
	if m["error"] != "unsupported format: 'xml' (expected csv, json, or jsonl)" || dsNum(t, m["status"]) != 400 {
		t.Fatalf("%v", m)
	}
	dsExpect(t, e.manage("POST", "/manage/datasets/999/download", map[string]any{}), http.StatusNotFound)

	// PII review gate and paused datasets block exports.
	dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/require-pii-review", id), map[string]any{"require": true}), http.StatusOK)
	m = dsExpect(t, e.manage("POST", path, map[string]any{}), http.StatusForbidden)
	if !strings.HasPrefix(m["error"].(string), "dataset requires a PII review") {
		t.Fatalf("%v", m)
	}
	dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/require-pii-review", id), map[string]any{"require": false}), http.StatusOK)
	dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/pause", id), nil), http.StatusOK)
	m = dsExpect(t, e.manage("POST", path, map[string]any{"output_dir": dir}), http.StatusServiceUnavailable)
	if m["error"] != "dataset is paused" {
		t.Fatalf("%v", m)
	}
}

// dsRestUpstream serves a small users API; /fail returns the given status.
func dsRestUpstream(t *testing.T, n int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/users":
			var users []map[string]any
			for i := 1; i <= n; i++ {
				users = append(users, map[string]any{"id": fmt.Sprint(i), "name": fmt.Sprintf("user%d", i), "email": fmt.Sprintf("u%d@example.com", i)})
			}
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(users)
		case "/notfound":
			http.Error(w, `{"message":"nope"}`, http.StatusNotFound)
		case "/unauth":
			http.Error(w, `{"message":"bad token"}`, http.StatusUnauthorized)
		default:
			http.Error(w, "boom", http.StatusInternalServerError)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

func dsRestSourceConfig(path string) map[string]any {
	return map[string]any{
		"path": path,
		"schema": map[string]any{"fields": []any{
			map[string]any{"name": "id", "field_type": "string"},
			map[string]any{"name": "name", "field_type": "string"},
			map[string]any{"name": "email", "field_type": "string"},
		}},
	}
}

func TestDatasetsRestConnector(t *testing.T) {
	e := newDatasetsEnv(t)
	up := dsRestUpstream(t, 3)
	restID := e.dsCreateDatasource("api", "rest", fmt.Sprintf(`{"base_url":%q,"auth":{"kind":"none"}}`, up.URL))

	// Pull-through: auto-introspect populates fields from the declared schema.
	id := e.dsCreateDataset(map[string]any{"name": "Users", "datasource_id": restID, "source_config": dsRestSourceConfig("/users")})
	m := dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/introspect", id), nil), http.StatusOK)
	if len(m["fields"].([]any)) != 3 {
		t.Fatalf("introspect: %v", m)
	}
	dsExpect(t, e.manage("POST", "/manage/datasets/999/introspect", nil), http.StatusNotFound)
	m = dsExpect(t, e.manage("GET", fmt.Sprintf("/manage/datasets/%d/preview?q=user", id), nil), http.StatusOK)
	if len(m["items"].([]any)) != 3 {
		t.Fatalf("preview: %v", m)
	}

	// PII scan flags the email column and records scan results.
	m = dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/pii-scan", id), nil), http.StatusOK)
	if dsNum(t, m["rows_scanned"]) != 3 {
		t.Fatalf("pii: %v", m)
	}
	email := m["fields"].(map[string]any)["email"].(map[string]any)
	if email["pii_class"] == nil || dsNum(t, email["samples_considered"]) != 3 {
		t.Fatalf("pii email: %v", email)
	}
	dsExpect(t, e.manage("POST", "/manage/datasets/999/pii-scan", nil), http.StatusNotFound)

	// Pull-through download streams from the connector.
	rec := e.manage("POST", fmt.Sprintf("/manage/datasets/%d/download", id), map[string]any{"format": "jsonl"})
	if rec.Code != http.StatusOK || strings.Count(rec.Body.String(), "\n") != 3 {
		t.Fatalf("download: %d %q", rec.Code, rec.Body.String())
	}
	dir := t.TempDir()
	m = dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/download", id), map[string]any{"output_dir": dir}), http.StatusOK)
	if dsNum(t, m["rows"]) != 3 {
		t.Fatalf("download dir: %v", m)
	}

	// Snapshot only on mirror datasets.
	dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/snapshot", id), nil), http.StatusBadRequest)
	mirror := e.dsCreateDataset(map[string]any{"name": "M", "datasource_id": restID, "mode": "mirror", "auto_introspect": false, "source_config": dsRestSourceConfig("/users")})
	m = dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/snapshot", mirror), nil), http.StatusOK)
	if dsNum(t, m["rows_captured"]) != 3 {
		t.Fatalf("snapshot: %v", m)
	}
	m = dsExpect(t, e.manage("GET", fmt.Sprintf("/manage/datasets/%d", mirror), nil), http.StatusOK)
	if m["dataset"].(map[string]any)["snapshot_at"] == nil {
		t.Fatalf("snapshot_at not set: %v", m)
	}
	m = dsExpect(t, e.manage("GET", fmt.Sprintf("/manage/datasets/%d/preview", mirror), nil), http.StatusOK)
	if dsNum(t, m["meta"].(map[string]any)["total"]) != 3 {
		t.Fatalf("mirror preview: %v", m)
	}
	dsExpect(t, e.manage("POST", "/manage/datasets/999/snapshot", nil), http.StatusNotFound)

	// Mirror stays mirror on update → no reseed; switching pull-through → cached seeds.
	dsExpect(t, e.manage("PUT", fmt.Sprintf("/manage/datasets/%d", id), map[string]any{"name": "Users", "mode": "cached", "cache_ttl_seconds": 3600}), http.StatusOK)
	e.srv.syncMu.Lock()
	_, tracked := e.srv.syncCancels[id]
	e.srv.syncMu.Unlock()
	if !tracked {
		t.Fatal("seed goroutine should be tracked in syncCancels")
	}

	// Synthetic generation.
	dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/generate", mirror), nil), http.StatusBadRequest)
	syn := e.dsCreateDataset(map[string]any{"name": "S", "datasource_id": restID, "mode": "synthetic", "auto_introspect": false, "synthetic_row_count": 7, "source_config": dsRestSourceConfig("/users")})
	dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/generate", syn), nil), http.StatusBadRequest) // no fields
	dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/introspect", syn), nil), http.StatusOK)
	m = dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/generate", syn), nil), http.StatusOK)
	if dsNum(t, m["rows_generated"]) != 7 {
		t.Fatalf("generate default: %v", m)
	}
	m = dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/generate", syn), map[string]any{"row_count": 4}), http.StatusOK)
	if dsNum(t, m["rows_generated"]) != 4 {
		t.Fatalf("generate: %v", m)
	}
	dsExpect(t, e.manage("POST", "/manage/datasets/999/generate", nil), http.StatusNotFound)
}

func TestDatasetsRestUpstreamErrors(t *testing.T) {
	e := newDatasetsEnv(t)
	up := dsRestUpstream(t, 1)
	restID := e.dsCreateDatasource("api", "rest", fmt.Sprintf(`{"base_url":%q,"auth":{"kind":"none"}}`, up.URL))
	mk := func(name, path, mode string) int64 {
		return e.dsCreateDataset(map[string]any{"name": name, "datasource_id": restID, "mode": mode, "source_config": dsRestSourceConfig(path)})
	}

	nf := mk("nf", "/notfound", "pull_through")
	dsExpect(t, e.manage("GET", fmt.Sprintf("/manage/datasets/%d/preview", nf), nil), http.StatusNotFound)
	dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/pii-scan", nf), nil), http.StatusNotFound)

	ua := mk("ua", "/unauth", "pull_through")
	rec := e.manage("GET", fmt.Sprintf("/manage/datasets/%d/preview", ua), nil)
	if rec.Code == http.StatusUnauthorized {
		if m := decode(t, rec); m["error_code"] != "auth_expired" {
			t.Fatalf("auth: %v", m)
		}
	} else if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unauth upstream: %d %s", rec.Code, rec.Body.String())
	}

	boom := mk("boom", "/boom", "pull_through")
	dsExpect(t, e.manage("GET", fmt.Sprintf("/manage/datasets/%d/preview", boom), nil), http.StatusBadGateway)
	dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/download", boom), map[string]any{"output_dir": t.TempDir()}), http.StatusBadGateway)

	mirror := mk("mb", "/boom", "mirror")
	dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/snapshot", mirror), nil), http.StatusBadGateway)

	// PII scan without fields.
	dsID := e.dsCreateDatasource("files", "file_upload", "{}")
	empty := e.dsCreateDataset(map[string]any{"name": "E", "datasource_id": dsID})
	m := dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/pii-scan", empty), nil), http.StatusBadRequest)
	if m["error"] != "dataset has no fields — run introspection first" {
		t.Fatalf("%v", m)
	}

	// Broken datasource config → connector creation fails.
	badDS := e.dsCreateDatasource("bad", "rest", `{}`)
	bad := e.dsCreateDataset(map[string]any{"name": "B", "datasource_id": badDS, "mode": "mirror"})
	dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/introspect", bad), nil), http.StatusBadGateway)
	dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/snapshot", bad), nil), http.StatusBadGateway)
}

func TestDatasetsSeedCache(t *testing.T) {
	e := newDatasetsEnv(t)
	up := dsRestUpstream(t, 5)
	restID := e.dsCreateDatasource("api", "rest", fmt.Sprintf(`{"base_url":%q,"auth":{"kind":"none"}}`, up.URL))
	id := e.dsCreateDataset(map[string]any{"name": "C", "datasource_id": restID, "mode": "pull_through", "source_config": dsRestSourceConfig("/users")})
	if err := e.srv.seedCache(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err := e.srv.seedCache(context.Background(), 999); err == nil {
		t.Fatal("expected error for missing dataset")
	}
	dsExpect(t, e.manage("PUT", fmt.Sprintf("/manage/datasets/%d", id), map[string]any{"name": "C", "mode": "mirror"}), http.StatusOK)
	datasetsBackground.Wait()
	m := dsExpect(t, e.manage("GET", fmt.Sprintf("/manage/datasets/%d/stats", id), nil), http.StatusOK)
	if dsNum(t, m["total_rows"]) != 5 {
		t.Fatalf("stats after seed: %v", m)
	}
}

func TestDatasetsDraftDescription(t *testing.T) {
	e := newDatasetsEnv(t)
	body := map[string]any{"datasource_id": 1}
	m := dsExpect(t, e.manage("POST", "/manage/datasets/draft-description", body), http.StatusPreconditionFailed)
	if m["error"] != "AI assist is disabled — enable it in Settings first" {
		t.Fatalf("%v", m)
	}
	e.srv.aiMu.Lock()
	e.srv.ai.Enabled = true
	e.srv.aiMu.Unlock()
	m = dsExpect(t, e.manage("POST", "/manage/datasets/draft-description", body), http.StatusPreconditionFailed)
	if !strings.HasPrefix(m["error"].(string), "AI assist is enabled but no Anthropic API key") {
		t.Fatalf("%v", m)
	}
	if err := secretstore.StoreAnthropicKey("sk-test"); err != nil {
		t.Fatal(err)
	}
	m = dsExpect(t, e.manage("POST", "/manage/datasets/draft-description", body), http.StatusBadRequest)
	if m["error"] != "datasource not found" {
		t.Fatalf("%v", m)
	}
	dsExpect(t, e.manage("POST", "/manage/datasets/draft-description", map[string]any{}), http.StatusUnprocessableEntity)

	badDS := e.dsCreateDatasource("bad", "rest", `{}`)
	dsExpect(t, e.manage("POST", "/manage/datasets/draft-description", map[string]any{"datasource_id": badDS}), http.StatusBadGateway)
}

func TestDatasetsResyncAndStaleRefresh(t *testing.T) {
	e := newDatasetsEnv(t)
	up := dsRestUpstream(t, 2)
	restID := e.dsCreateDatasource("api", "rest", fmt.Sprintf(`{"base_url":%q,"auth":{"kind":"none"}}`, up.URL))
	id := e.dsCreateDataset(map[string]any{"name": "C", "datasource_id": restID, "source_config": dsRestSourceConfig("/users")})

	// Bind an agent; mutations resync its directory, preserving its token.
	ctx := context.Background()
	agentID, err := crud.CreateAgent(ctx, e.srv.db, "bot", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crud.BindAgentDataset(ctx, e.srv.db, agentID, id, "users", nil, nil); err != nil {
		t.Fatal(err)
	}
	tok := "ds_agent_tok"
	dir, err := agentdir.SyncAgentDir(ctx, e.srv.db, e.srv.dataDir, agentID, &tok, e.srv.daemonURL)
	if err != nil {
		t.Fatal(err)
	}
	if got := e.srv.datasetsReadAgentToken(ctx, agentID); got == nil || *got != tok {
		t.Fatalf("token: %v", got)
	}
	dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/pause", id), nil), http.StatusOK)
	raw, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil || !strings.Contains(string(raw), tok) {
		t.Fatalf("token lost on resync: %v %s", err, raw)
	}
	dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/resume", id), nil), http.StatusOK)
	if got := e.srv.datasetsReadAgentToken(ctx, 999); got != nil {
		t.Fatal("unknown agent should have no token")
	}

	// Switch to cached with a TTL without seeding: the export refreshes inline.
	ttl := int64(3600)
	if _, err := crud.UpdateDataset(ctx, e.srv.db, id, "C", "cached", &ttl, nil, nil, nil, nil, nil); err != nil {
		t.Fatal(err)
	}
	m := dsExpect(t, e.manage("POST", fmt.Sprintf("/manage/datasets/%d/download", id), map[string]any{"output_dir": t.TempDir()}), http.StatusOK)
	if dsNum(t, m["rows"]) != 2 {
		t.Fatalf("refresh: %v", m)
	}
}
