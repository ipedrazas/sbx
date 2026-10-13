package server

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/ipedrazas/sbx/internal/cache"
	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/exportdata"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/util"
)

var liveTestRows = []map[string]any{
	{"id": "1", "name": "Alice", "email": "alice@example.com", "amount": 10, "secret": "s1"},
	{"id": "2", "name": "Bob", "email": "bob@example.com", "amount": 30, "secret": "s2"},
	{"id": "3", "name": "Carol", "email": "carol@example.com", "amount": 20, "secret": "s3"},
}

type liveFixture struct {
	e        *testEnv
	ctx      context.Context
	agentID  int64
	token    string
	dsID     int64
	hits     atomic.Int64
	upstream *httptest.Server
	fail     atomic.Bool
}

func newLiveFixture(t *testing.T) *liveFixture {
	t.Helper()
	f := &liveFixture{e: newTestEnv(t), ctx: context.Background()}
	f.upstream = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.hits.Add(1)
		if f.fail.Load() {
			http.Error(w, "boom", http.StatusInternalServerError)
			return
		}
		rows := []map[string]any{}
		q := r.URL.Query().Get("q")
		for _, row := range liveTestRows {
			if q == "" || strings.Contains(strings.ToLower(row["name"].(string)), strings.ToLower(liveLastWord(q))) {
				rows = append(rows, row)
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(rows)
	}))
	t.Cleanup(f.upstream.Close)

	db := f.e.srv.db
	var err error
	f.agentID, err = crud.CreateAgent(f.ctx, db, "bot", nil)
	if err != nil {
		t.Fatal(err)
	}
	f.token = "agent-token-1"
	if _, err := crud.CreateAgentToken(f.ctx, db, f.agentID, util.HashToken(f.token), "t"); err != nil {
		t.Fatal(err)
	}
	cfg := fmt.Sprintf(`{"base_url":%q,"auth":{"kind":"none"}}`, f.upstream.URL)
	f.dsID, err = crud.CreateDatasource(f.ctx, db, "api", "rest", cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func liveLastWord(s string) string {
	parts := strings.Fields(s)
	if len(parts) == 0 {
		return s
	}
	return parts[len(parts)-1]
}

type liveDSOpts struct {
	mode         string
	ttl          *int64
	maxRows      *int64
	maxPageBytes *int64
	searchQuery  string
	noFields     bool
}

// addDataset creates a REST-backed dataset bound under alias, with fields
// id/name/email/amount visible and secret hidden; email masked.
func (f *liveFixture) addDataset(t *testing.T, alias string, o liveDSOpts) int64 {
	t.Helper()
	db := f.e.srv.db
	sc := map[string]any{"path": "/items", "schema": map[string]any{"fields": []any{}}}
	if o.searchQuery != "" {
		sc["search_query"] = o.searchQuery
	}
	raw := util.JSONString(sc)
	desc := "people"
	id, err := crud.CreateDatasetFull(f.ctx, db, alias+"-ds", f.dsID, o.mode, o.ttl, &raw, &desc, nil, o.maxRows, nil, o.maxPageBytes)
	if err != nil {
		t.Fatal(err)
	}
	if !o.noFields {
		mk := func(src, display, typ string, visible bool, policy string) models.DatasetField {
			fl := models.NewDatasetField()
			fl.SourceName, fl.DisplayName, fl.FieldType, fl.Visible, fl.PiiPolicy = src, display, typ, visible, policy
			return fl
		}
		fields := []models.DatasetField{
			mk("id", "id", "string", true, "none"),
			mk("name", "Name", "string", true, "none"),
			mk("email", "email", "string", true, "mask"),
			mk("amount", "Amount", "integer", true, "none"),
			mk("secret", "secret", "string", false, "none"),
		}
		if err := crud.SetDatasetFields(f.ctx, db, id, fields); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := crud.BindAgentDataset(f.ctx, db, f.agentID, id, alias, nil, nil); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *liveFixture) fillCache(t *testing.T, datasetID int64, rows []map[string]any) {
	t.Helper()
	c, err := cache.Open(f.ctx, f.e.srv.dataDir, datasetID)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.StoreItems(f.ctx, rows); err != nil {
		t.Fatal(err)
	}
}

func (f *liveFixture) get(path string) *httptest.ResponseRecorder {
	return f.e.do("GET", fmt.Sprintf("/a/%d%s", f.agentID, path), f.token, nil)
}

func liveItems(t *testing.T, rec *httptest.ResponseRecorder) ([]map[string]any, map[string]any) {
	t.Helper()
	if rec.Code != http.StatusOK {
		t.Fatalf("status %d: %s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	var items []map[string]any
	for _, it := range m["items"].([]any) {
		items = append(items, it.(map[string]any))
	}
	return items, m["meta"].(map[string]any)
}

func TestLiveAuth(t *testing.T) {
	f := newLiveFixture(t)
	f.addDataset(t, "people", liveDSOpts{mode: "cached"})
	path := fmt.Sprintf("/a/%d/people/list", f.agentID)

	cases := []struct {
		token, path string
		want        int
		msg         string
	}{
		{"", path, 401, "missing or invalid Authorization header"},
		{"nope", path, 401, "invalid token"},
		{f.token, "/a/999/people/list", 401, "token does not match agent"},
		{f.token, "/a/abc/people/list", 400, ""},
		{f.token, fmt.Sprintf("/a/%d/missing/list", f.agentID), 404, "dataset alias 'missing' not found for this agent"},
	}
	for _, c := range cases {
		rec := f.e.do("GET", c.path, c.token, nil)
		if rec.Code != c.want {
			t.Fatalf("%s: got %d %s", c.path, rec.Code, rec.Body.String())
		}
		if c.msg != "" {
			if m := decode(t, rec); m["error"] != c.msg {
				t.Fatalf("error = %v", m["error"])
			}
		}
	}
}

func TestLiveListDatasets(t *testing.T) {
	f := newLiveFixture(t)
	mpb := int64(5000)
	rows := int64(5000)
	f.addDataset(t, "people", liveDSOpts{mode: "cached", maxPageBytes: &mpb, maxRows: &rows})
	f.addDataset(t, "raw", liveDSOpts{mode: "pull_through", noFields: true})

	rec := f.get("/datasets")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	ds := decode(t, rec)["datasets"].([]any)
	if len(ds) != 2 {
		t.Fatalf("datasets = %v", ds)
	}
	byAlias := map[string]map[string]any{}
	for _, d := range ds {
		m := d.(map[string]any)
		byAlias[m["alias"].(string)] = m
	}
	p := byAlias["people"]
	if p["mode"] != "cached" || p["description"] != "people" || p["data_kind"] != nil ||
		p["max_rows_per_query"].(float64) != 1000 || p["max_page_bytes"].(float64) != 5000 || p["capabilities"] == nil {
		t.Fatalf("people entry = %v", p)
	}
	schema := p["schema"].([]any)
	if len(schema) != 4 {
		t.Fatalf("schema should only list visible fields: %v", schema)
	}
	first := schema[0].(map[string]any)
	if first["name"] != "id" || first["type"] != "string" || first["searchable"] != true || first["sortable"] != true {
		t.Fatalf("field = %v", first)
	}
	r := byAlias["raw"]
	if _, ok := r["max_page_bytes"]; ok {
		t.Fatal("max_page_bytes must be omitted when unset")
	}
	if len(r["schema"].([]any)) != 0 || r["mode"] != "pull_through" {
		t.Fatalf("raw entry = %v", r)
	}

	// Bad auth.
	if rec := f.e.do("GET", fmt.Sprintf("/a/%d/datasets", f.agentID), "", nil); rec.Code != 401 {
		t.Fatalf("want 401, got %d", rec.Code)
	}
	if rec := f.e.do("GET", "/a/x/datasets", f.token, nil); rec.Code != 400 {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}

func TestLiveCachedList(t *testing.T) {
	f := newLiveFixture(t)
	id := f.addDataset(t, "people", liveDSOpts{mode: "cached"})
	f.fillCache(t, id, liveTestRows)

	items, meta := liveItems(t, f.get("/people/list"))
	if len(items) != 3 || meta["total"].(float64) != 3 || meta["response_bytes"] == nil {
		t.Fatalf("items=%v meta=%v", items, meta)
	}
	for _, it := range items {
		if _, ok := it["secret"]; ok {
			t.Fatal("hidden field leaked")
		}
		if it["email"] != "***" {
			t.Fatalf("email not masked: %v", it["email"])
		}
	}
	if f.hits.Load() != 0 {
		t.Fatal("cached list without TTL must not hit upstream")
	}

	// fields projection + sort by display name.
	items, meta = liveItems(t, f.get("/people/list?fields=name,%20bogus,secret&sort_by=Amount&sort_dir=DESC"))
	if len(items) != 3 || len(items[0]) != 1 || items[0]["name"] != "Bob" {
		t.Fatalf("items = %v", items)
	}
	if meta["sorted_by"] != "Amount" || meta["sorted_dir"] != "desc" {
		t.Fatalf("meta = %v", meta)
	}
	// Unknown sort field is a silent no-op.
	_, meta = liveItems(t, f.get("/people/list?sort_by=amount"))
	if _, ok := meta["sorted_by"]; ok {
		t.Fatalf("sort by source name must be a no-op: %v", meta)
	}

	// Ad-hoc filter, limit, offset.
	items, _ = liveItems(t, f.get("/people/list?filter=name:eq:Carol"))
	if len(items) != 1 || items[0]["id"] != "3" {
		t.Fatalf("filtered = %v", items)
	}
	items, meta = liveItems(t, f.get("/people/list?limit=1&offset=1&limit=2&offset=bad"))
	if len(items) != 2 || meta["limit"].(float64) != 2 || meta["offset"].(float64) != 0 {
		t.Fatalf("items=%v meta=%v", items, meta)
	}

	// Hidden or malformed filters are rejected.
	rec := f.get("/people/list?filter=secret:eq:s1")
	if rec.Code != 400 || !strings.Contains(decode(t, rec)["error"].(string), "cannot filter on 'secret'") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := f.get("/people/list?filter=garbage"); rec.Code != 400 {
		t.Fatalf("want 400, got %d", rec.Code)
	}
}

func TestLiveRowCapAndSearchQuery(t *testing.T) {
	f := newLiveFixture(t)
	one := int64(1)
	id := f.addDataset(t, "people", liveDSOpts{mode: "mirror", maxRows: &one, searchQuery: "bob"})
	f.fillCache(t, id, liveTestRows)
	items, meta := liveItems(t, f.get("/people/list?limit=50"))
	if len(items) != 1 || meta["limit"].(float64) != 1 || items[0]["name"] != "Bob" {
		t.Fatalf("items=%v meta=%v", items, meta)
	}
}

func TestLiveCachedGetSearchSchema(t *testing.T) {
	f := newLiveFixture(t)
	id := f.addDataset(t, "people", liveDSOpts{mode: "cached"})
	f.fillCache(t, id, liveTestRows)

	rec := f.get("/people/get/2?fields=name,email")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["name"] != "Bob" || m["email"] != "***" || len(m) != 2 {
		t.Fatalf("item = %v", m)
	}
	if rec := f.get("/people/get/99"); rec.Code != 404 || decode(t, rec)["error"] != "item not found" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	items, _ := liveItems(t, f.get("/people/search?q=carol"))
	if len(items) != 1 || items[0]["id"] != "3" {
		t.Fatalf("search = %v", items)
	}
	if rec := f.get("/people/search"); rec.Code != 400 || decode(t, rec)["error"] != "missing required query parameter 'q'" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := f.get("/people/search?q=a&filter=secret:eq:x"); rec.Code != 400 {
		t.Fatalf("want 400, got %d", rec.Code)
	}

	rec = f.get("/people/schema")
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	m = decode(t, rec)
	if len(m["fields"].([]any)) != 4 || m["capabilities"] == nil {
		t.Fatalf("schema = %v", m)
	}
}

func TestLiveMaxPageBytes(t *testing.T) {
	f := newLiveFixture(t)
	limit := int64(40)
	id := f.addDataset(t, "people", liveDSOpts{mode: "cached", maxPageBytes: &limit})
	f.fillCache(t, id, liveTestRows)

	rec := f.get("/people/list")
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["status"].(float64) != 413 || m["limit_bytes"].(float64) != 40 || m["item_count"].(float64) != 3 ||
		len(m["available_fields"].([]any)) != 4 || len(m["suggestions"].([]any)) != 3 {
		t.Fatalf("body = %v", m)
	}
	// Narrowing fields fits.
	if rec := f.get("/people/list?fields=id&limit=1"); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	rec = f.get("/people/get/1")
	if rec.Code != 413 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	m = decode(t, rec)
	if len(m["suggestions"].([]any)) != 1 || m["selected_fields"] == nil {
		t.Fatalf("body = %v", m)
	}
	if rec := f.get("/people/get/1?fields=id"); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestLiveGates(t *testing.T) {
	f := newLiveFixture(t)
	db := f.e.srv.db
	id := f.addDataset(t, "people", liveDSOpts{mode: "cached"})

	if _, err := crud.SetDatasetEnabled(f.ctx, db, id, false); err != nil {
		t.Fatal(err)
	}
	if rec := f.get("/people/list"); rec.Code != 503 || decode(t, rec)["error"] != "dataset is paused" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if _, err := crud.SetDatasetEnabled(f.ctx, db, id, true); err != nil {
		t.Fatal(err)
	}

	if _, err := crud.SetDatasetRequirePIIReview(f.ctx, db, id, true); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/people/list", "/people/get/1", "/people/search?q=a"} {
		rec := f.get(p)
		if rec.Code != 403 || !strings.Contains(decode(t, rec)["error"].(string), "requires a PII review") {
			t.Fatalf("%s: %d %s", p, rec.Code, rec.Body.String())
		}
	}
	if err := crud.ApplyPIIScanResults(f.ctx, db, id, nil); err != nil {
		t.Fatal(err)
	}
	if rec := f.get("/people/list"); rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestLivePullThrough(t *testing.T) {
	f := newLiveFixture(t)
	f.addDataset(t, "people", liveDSOpts{mode: "pull_through"})

	items, meta := liveItems(t, f.get("/people/list?filter=name:contains:o&sort_by=Amount"))
	if len(items) != 2 {
		t.Fatalf("items = %v", items)
	}
	// REST can't sort upstream → no-op in pull-through.
	if _, ok := meta["sorted_by"]; ok {
		t.Fatalf("meta = %v", meta)
	}
	if items[0]["email"] != "***" {
		t.Fatalf("policy not applied: %v", items[0])
	}

	rec := f.get("/people/get/3")
	if rec.Code != 200 || decode(t, rec)["name"] != "Carol" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := f.get("/people/get/42"); rec.Code != 404 {
		t.Fatalf("want 404, got %d", rec.Code)
	}

	items, _ = liveItems(t, f.get("/people/search?q=alice"))
	if len(items) != 1 || items[0]["id"] != "1" {
		t.Fatalf("search = %v", items)
	}

	f.fail.Store(true)
	for _, p := range []string{"/people/list", "/people/get/1", "/people/search?q=x"} {
		if rec := f.get(p); rec.Code != http.StatusBadGateway {
			t.Fatalf("%s: want 502, got %d %s", p, rec.Code, rec.Body.String())
		}
	}
}

func TestLiveRefreshIfStale(t *testing.T) {
	f := newLiveFixture(t)
	ttl := int64(3600)
	f.addDataset(t, "people", liveDSOpts{mode: "cached", ttl: &ttl})

	// Empty cache is stale → fetched from upstream once, then served from cache.
	items, _ := liveItems(t, f.get("/people/list"))
	if len(items) != 3 || f.hits.Load() != 1 {
		t.Fatalf("items=%d hits=%d", len(items), f.hits.Load())
	}
	if rec := f.get("/people/get/1"); rec.Code != 200 || f.hits.Load() != 1 {
		t.Fatalf("%d hits=%d", rec.Code, f.hits.Load())
	}

	// Refresh failures surface through the connector error mapping.
	f2 := newLiveFixture(t)
	f2.addDataset(t, "people", liveDSOpts{mode: "cached", ttl: &ttl})
	f2.fail.Store(true)
	if rec := f2.get("/people/search?q=a"); rec.Code != http.StatusBadGateway {
		t.Fatalf("want 502, got %d %s", rec.Code, rec.Body.String())
	}

	// Synthetic datasets never refresh.
	f3 := newLiveFixture(t)
	f3.addDataset(t, "people", liveDSOpts{mode: "synthetic", ttl: &ttl})
	if _, _ = liveItems(t, f3.get("/people/list")); f3.hits.Load() != 0 {
		t.Fatal("synthetic dataset hit upstream")
	}
}

func TestLiveNoFieldsPassThrough(t *testing.T) {
	f := newLiveFixture(t)
	id := f.addDataset(t, "raw", liveDSOpts{mode: "cached", noFields: true})
	f.fillCache(t, id, liveTestRows)
	items, _ := liveItems(t, f.get("/raw/list?filter=secret:eq:s2&fields=name"))
	if len(items) != 1 || len(items[0]) != 1 || items[0]["name"] != "Bob" {
		t.Fatalf("items = %v", items)
	}
	items, _ = liveItems(t, f.get("/raw/list"))
	if len(items[0]) != 5 {
		t.Fatalf("no-schema dataset must pass rows through: %v", items[0])
	}
}

func TestLiveCollectAndStream(t *testing.T) {
	f := newLiveFixture(t)
	cachedID := f.addDataset(t, "people", liveDSOpts{mode: "cached"})
	f.fillCache(t, cachedID, liveTestRows)
	pullID := f.addDataset(t, "pull", liveDSOpts{mode: "pull_through"})
	rawID := f.addDataset(t, "raw", liveDSOpts{mode: "cached", noFields: true})
	f.fillCache(t, rawID, liveTestRows)
	emptyID := f.addDataset(t, "empty", liveDSOpts{mode: "cached"})

	for _, id := range []int64{cachedID, pullID} {
		rows, err := f.e.srv.liveCollectAllServedRows(f.ctx, id)
		if err != nil || len(rows) != 3 || rows[0]["email"] != "***" {
			t.Fatalf("collect %d: %v %v", id, rows, err)
		}
		if _, ok := rows[0]["secret"]; ok {
			t.Fatal("hidden field in download")
		}
	}

	stream := func(id int64, format exportdata.Format) string {
		t.Helper()
		fn, err := f.e.srv.liveStreamServedRows(f.ctx, id, format)
		if err != nil {
			t.Fatal(err)
		}
		var buf bytes.Buffer
		if err := fn(&buf); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	js := stream(cachedID, exportdata.JSON)
	var arr []map[string]any
	if err := json.Unmarshal([]byte(js), &arr); err != nil || len(arr) != 3 {
		t.Fatalf("json export %q: %v", js, err)
	}
	if got := stream(pullID, exportdata.JSONL); strings.Count(got, "\n") != 3 {
		t.Fatalf("jsonl export %q", got)
	}
	csv := stream(cachedID, exportdata.CSV)
	if lines := strings.Split(strings.TrimSpace(csv), "\n"); len(lines) != 4 || !strings.HasPrefix(lines[0], "id,name,email,amount") {
		t.Fatalf("csv export %q", csv)
	}
	if csv := stream(rawID, exportdata.CSV); !strings.Contains(csv, "secret") {
		t.Fatalf("schema-less csv must derive columns: %q", csv)
	}
	if got := stream(emptyID, exportdata.JSON); got != "[]" {
		t.Fatalf("empty json export = %q", got)
	}

	// Pre-flight errors are returned before streaming.
	if _, err := crud.SetDatasetEnabled(f.ctx, f.e.srv.db, cachedID, false); err != nil {
		t.Fatal(err)
	}
	if _, err := f.e.srv.liveStreamServedRows(f.ctx, cachedID, exportdata.JSON); err == nil {
		t.Fatal("want paused error")
	}
	if _, err := f.e.srv.liveCollectAllServedRows(f.ctx, 9999); err == nil {
		t.Fatal("want not found")
	}
	f.fail.Store(true)
	if _, err := f.e.srv.liveCollectAllServedRows(f.ctx, pullID); toAPIError(err).Status != 502 {
		t.Fatalf("want 502, got %v", err)
	}
	fn, err := f.e.srv.liveStreamServedRows(f.ctx, pullID, exportdata.JSON)
	if err != nil {
		t.Fatal(err)
	}
	if err := fn(&bytes.Buffer{}); err == nil {
		t.Fatal("want mid-stream error")
	}
}

// ── Ported sort-resolution tests ──

func liveSortFields() []liveSortField {
	return []liveSortField{{"Created At", "created_at"}, {"Amount", "amount"}}
}

func liveStrPtr(s string) *string { return &s }

func TestLiveResolveSort(t *testing.T) {
	f := liveSortFields()
	all := connector.SortCapability{Kind: connector.SortAll}
	none := connector.SortCapability{Kind: connector.SortNone}

	if liveResolveSortInner(models.ModeCached, f, all, nil, nil) != nil ||
		liveResolveSortInner(models.ModeCached, f, all, liveStrPtr("  "), liveStrPtr("asc")) != nil {
		t.Fatal("absent/blank sort_by must be a no-op")
	}
	if liveResolveSortInner(models.ModeCached, f, all, liveStrPtr("Nope"), liveStrPtr("asc")) != nil {
		t.Fatal("unknown display name must be a no-op")
	}
	r := liveResolveSortInner(models.ModeCached, f, all, liveStrPtr("Created At"), liveStrPtr("desc"))
	if r == nil || r.spec.Field != "created_at" || r.displayName != "Created At" || !r.spec.Descending {
		t.Fatalf("resolved = %+v", r)
	}
	if r := liveResolveSortInner(models.ModeCached, f, all, liveStrPtr("Amount"), nil); r.spec.Descending {
		t.Fatal("default must be asc")
	}
	if r := liveResolveSortInner(models.ModeCached, f, all, liveStrPtr("Amount"), liveStrPtr("sideways")); r.spec.Descending {
		t.Fatal("garbage dir must be asc")
	}
	if r := liveResolveSortInner(models.ModeCached, f, all, liveStrPtr("Amount"), liveStrPtr("DESC")); !r.spec.Descending {
		t.Fatal("DESC must be desc")
	}
	if liveResolveSortInner(models.ModeMirror, f, none, liveStrPtr("Amount"), liveStrPtr("asc")) == nil {
		t.Fatal("cache mode ignores capability")
	}
	if liveResolveSortInner(models.ModePullThrough, f, none, liveStrPtr("Amount"), liveStrPtr("asc")) != nil {
		t.Fatal("pull-through gated by capability")
	}
	allowed := connector.SortCapability{Kind: connector.SortFields, Fields: []string{"amount"}}
	if liveResolveSortInner(models.ModePullThrough, f, allowed, liveStrPtr("Amount"), liveStrPtr("asc")) == nil {
		t.Fatal("allowed field must resolve")
	}
	other := connector.SortCapability{Kind: connector.SortFields, Fields: []string{"created_at"}}
	if liveResolveSortInner(models.ModePullThrough, f, other, liveStrPtr("Amount"), liveStrPtr("asc")) != nil {
		t.Fatal("other field must be a no-op")
	}
}

func TestLiveEchoSort(t *testing.T) {
	var meta models.EnvelopeMeta
	liveEchoSort(&meta, nil)
	if meta.SortedBy != nil || meta.SortedDir != nil {
		t.Fatal("no-op sort must not stamp meta")
	}
	liveEchoSort(&meta, &liveResolvedSort{spec: models.SortSpec{Field: "created_at", Descending: true}, displayName: "Created At"})
	if *meta.SortedBy != "Created At" || *meta.SortedDir != "desc" {
		t.Fatalf("meta = %+v", meta)
	}
}

func TestLiveResolveSelectedFields(t *testing.T) {
	vis := []string{"a", "b", "c"}
	if got := liveResolveSelectedFields(nil, vis); len(got) != 3 {
		t.Fatal(got)
	}
	if got := liveResolveSelectedFields(liveStrPtr(" , "), vis); len(got) != 3 {
		t.Fatal(got)
	}
	if got := liveResolveSelectedFields(liveStrPtr("c, a,zzz"), vis); len(got) != 2 || got[0] != "a" || got[1] != "c" {
		t.Fatal(got)
	}
	if got := liveResolveSelectedFields(liveStrPtr("x,y"), nil); len(got) != 2 {
		t.Fatal(got)
	}
	if got := liveResolveSelectedFields(liveStrPtr("zzz"), vis); got == nil || len(got) != 0 {
		t.Fatal(got)
	}
}

func TestLiveApplyFieldPolicies(t *testing.T) {
	items := []map[string]any{{"a": 1, "b": "x@y.com", "c": "drop"}}
	fl := func(src, pol string) models.DatasetField {
		f := models.NewDatasetField()
		f.SourceName, f.PiiPolicy = src, pol
		return f
	}
	pols := liveBuildFieldPolicies([]models.DatasetField{fl("a", "none"), fl("b", "mask"), fl("c", "drop")})
	if len(pols) != 2 {
		t.Fatalf("policies = %v", pols)
	}
	out := liveApplyFieldPolicies(items, nil, pols)
	if out[0]["b"] != "***" || len(out[0]) != 2 {
		t.Fatalf("out = %v", out)
	}
	if out := liveApplyFieldPolicies(nil, nil, nil); out == nil || len(out) != 0 {
		t.Fatal("nil items must yield empty slice")
	}
	if out := liveApplyFieldPolicies(items, []string{"a"}, nil); len(out[0]) != 1 {
		t.Fatalf("visibility not applied: %v", out)
	}
}
