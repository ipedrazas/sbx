package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/secretstore"
)

// ── rate limiter ──

func TestRateLimitAllowsUpToCapacity(t *testing.T) {
	rl := NewRateLimiter()
	for i := 0; i < 60; i++ {
		if !rl.Check(1, BindingDataset, 1, 60, 60).Allowed {
			t.Fatalf("call %d should be allowed", i)
		}
	}
	d := rl.Check(1, BindingDataset, 1, 60, 60)
	if d.Allowed || d.RetryAfter < 1 {
		t.Fatalf("61st should deny: %+v", d)
	}
}

func TestRateLimitLongPeriod(t *testing.T) {
	rl := NewRateLimiter()
	for i := 0; i < 3; i++ {
		if !rl.Check(1, BindingAction, 7, 3, 3600).Allowed {
			t.Fatal("burst should be allowed")
		}
	}
	d := rl.Check(1, BindingAction, 7, 3, 3600)
	if d.Allowed || d.RetryAfter < 1100 || d.RetryAfter > 1300 {
		t.Fatalf("got %+v", d)
	}
}

func TestRateLimitIndependentKeys(t *testing.T) {
	rl := NewRateLimiter()
	for i := 0; i < 10; i++ {
		rl.Check(1, BindingDataset, 7, 10, 60)
	}
	if rl.Check(1, BindingDataset, 7, 10, 60).Allowed {
		t.Fatal("dataset 7 should be exhausted")
	}
	if !rl.Check(1, BindingAction, 7, 10, 60).Allowed {
		t.Fatal("action 7 is a separate bucket")
	}
	if !rl.Check(1, BindingDataset, 8, 10, 60).Allowed {
		t.Fatal("dataset 8 is a separate bucket")
	}
	rl.Forget(1, BindingDataset, 7)
	if !rl.Check(1, BindingDataset, 7, 10, 60).Allowed {
		t.Fatal("forgotten bucket should be fresh")
	}
}

func TestRateLimitResizeAndRefill(t *testing.T) {
	rl := NewRateLimiter()
	now := time.Unix(1000, 0)
	rl.now = func() time.Time { return now }
	for i := 0; i < 5; i++ {
		rl.Check(1, BindingDataset, 1, 10, 60)
	}
	d := rl.Check(1, BindingDataset, 1, 100, 60)
	if !d.Allowed || d.Limit != 100 || d.Remaining < 45 || d.Remaining > 55 {
		t.Fatalf("resize: %+v", d)
	}
	// Exhaust a 1/sec bucket then advance the clock to refill.
	for rl.Check(2, BindingDataset, 1, 2, 2).Allowed {
	}
	now = now.Add(2 * time.Second)
	if !rl.Check(2, BindingDataset, 1, 2, 2).Allowed {
		t.Fatal("should refill after time passes")
	}
}

// ── metrics ──

func TestMetricsSnapshot(t *testing.T) {
	m := NewMetrics()
	empty := m.Snapshot(1)
	if empty.TotalRequests != 0 || len(empty.QPS60s) != qpsWindowSecs || len(empty.Recent) != 0 {
		t.Fatalf("empty: %+v", empty)
	}

	now := time.Now().Unix()
	for lat := uint32(1); lat <= 100; lat++ {
		m.Record(2, RequestSample{Timestamp: now, LatencyMS: lat, Status: 200, BytesOut: 100})
	}
	s := m.Snapshot(2)
	if s.LatencyP50MS != 50 || s.LatencyP95MS != 95 || s.LatencyP99MS != 99 || s.LatencyMaxMS != 100 {
		t.Fatalf("percentiles: %+v", s)
	}
	if len(s.Recent) != recentSamples || s.Recent[0].LatencyMS != 100 || s.BytesOut != 10000 {
		t.Fatalf("recent/bytes: %+v", s)
	}

	m.Record(3, RequestSample{Timestamp: now, Status: 404})
	m.Record(3, RequestSample{Timestamp: now, Status: 500})
	m.Record(3, RequestSample{Timestamp: now - 5, Status: 502})
	m.Record(3, RequestSample{Timestamp: now - 120, Status: 200})
	s = m.Snapshot(3)
	if s.ClientErrorCount != 1 || s.ServerErrorCount != 2 {
		t.Fatalf("errors: %+v", s)
	}
	var windowed uint32
	for _, c := range s.QPS60s {
		windowed += c
	}
	if windowed != 3 || s.QPS60s[qpsWindowSecs-1] != 2 || s.QPS60s[qpsWindowSecs-6] != 1 {
		t.Fatalf("qps: %v", s.QPS60s)
	}
}

func TestMetricsRingEvicts(t *testing.T) {
	m := NewMetrics()
	for i := 0; i < ringCapacity+50; i++ {
		m.Record(1, RequestSample{Timestamp: int64(i)})
	}
	if got := m.Snapshot(1).TotalRequests; got != ringCapacity {
		t.Fatalf("got %d", got)
	}
}

// ── config ──

func TestConfigLoadOrInit(t *testing.T) {
	t.Setenv("DSTUDIO_MANAGE_TOKEN", "")
	os.Unsetenv("VAULT_ADDR")
	dir := t.TempDir()
	cfg, err := LoadOrInitConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(cfg.ManageToken, "ds_") || *cfg.AuditRetentionDays != 30 || cfg.AnthropicModel != defaultAnthropicModel {
		t.Fatalf("defaults: %+v", cfg)
	}
	st, _ := os.Stat(ConfigPath(dir))
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("perms %v", st.Mode())
	}
	again, err := LoadOrInitConfig(dir)
	if err != nil || again.ManageToken != cfg.ManageToken {
		t.Fatal("token should be stable across loads")
	}

	t.Setenv("DSTUDIO_MANAGE_TOKEN", "override")
	if c, _ := LoadOrInitConfig(dir); c.ManageToken != "override" {
		t.Fatal("env override ignored")
	}

	// Corrupt file is regenerated.
	_ = os.WriteFile(ConfigPath(dir), []byte("not = [valid"), 0o600)
	t.Setenv("DSTUDIO_MANAGE_TOKEN", "")
	if c, err := LoadOrInitConfig(dir); err != nil || c.ManageToken == cfg.ManageToken {
		t.Fatalf("expected regeneration: %v", err)
	}

	// Defaults applied to partial files.
	_ = os.WriteFile(ConfigPath(dir), []byte(`manage_token = "x"`), 0o600)
	c, err := ReadConfig(dir)
	if err != nil || c.AnthropicModel != defaultAnthropicModel || *c.AuditRetentionDays != 30 || c.AIEnabled {
		t.Fatalf("partial: %+v %v", c, err)
	}
}

func TestConfigVaultSync(t *testing.T) {
	secretstore.Init(secretstore.NewMemory())
	t.Setenv("VAULT_ADDR", "http://vault")
	t.Setenv("DSTUDIO_MANAGE_TOKEN", "")
	dir := t.TempDir()
	cfg, err := LoadOrInitConfig(dir)
	if err != nil {
		t.Fatal(err)
	}
	if b, _ := secretstore.Get("manage_token"); string(b) != cfg.ManageToken {
		t.Fatal("token should be seeded into vault")
	}
	_ = secretstore.Put("manage_token", []byte("from-vault"))
	if c, _ := LoadOrInitConfig(dir); c.ManageToken != "from-vault" {
		t.Fatal("vault token should win")
	}
}

// ── errors / helpers ──

func TestToAPIError(t *testing.T) {
	cases := []struct {
		err  error
		want int
	}{
		{connector.Auth("x"), 401},
		{connector.UpstreamHTTP(404, "x"), 404},
		{connector.UpstreamHTTP(503, "x"), 502},
		{connector.BadInput("x"), 422},
		{connector.Connection("x"), 502},
		{connector.Query("x"), 502},
		{connector.Config("x"), 500},
		{connector.Unsupported("x"), 501},
		{errNotFound("x"), 404},
		{errors.New("boom"), 500},
	}
	for _, c := range cases {
		if got := toAPIError(c.err).Status; got != c.want {
			t.Errorf("%v: got %d want %d", c.err, got, c.want)
		}
	}
	for _, e := range []*APIError{errBadRequest("x"), errInternal("x"), errConflict("x"), errUnauthorized("x"),
		errForbidden("x"), errBadGateway("x"), errUnavailable("x"), errNotImplemented("x")} {
		if e.Status < 400 || e.Error() != "x" {
			t.Errorf("bad helper %+v", e)
		}
	}
}

func TestDecodeJSON(t *testing.T) {
	var v struct {
		N int `json:"n"`
	}
	req := func(body string) *http.Request {
		return httptest.NewRequest("POST", "/", strings.NewReader(body))
	}
	if err := decodeJSON(req(`{"n":1}`), &v); err != nil || v.N != 1 {
		t.Fatal(err)
	}
	if err := decodeJSON(req(`{bad`), &v); toAPIError(err).Status != 400 {
		t.Fatalf("syntax: %v", err)
	}
	if err := decodeJSON(req(`{"n":"str"}`), &v); toAPIError(err).Status != 422 {
		t.Fatalf("type: %v", err)
	}
	if err := decodeJSON(req(``), &v); toAPIError(err).Status != 400 {
		t.Fatalf("empty: %v", err)
	}
}

func TestQueryHelpers(t *testing.T) {
	r := httptest.NewRequest("GET", "/?a=5&b=true&c=x&bad=zz", nil)
	if v, err := queryInt64(r, "a"); err != nil || *v != 5 {
		t.Fatal("int")
	}
	if v, _ := queryInt64(r, "missing"); v != nil {
		t.Fatal("missing int")
	}
	if _, err := queryInt64(r, "bad"); err == nil {
		t.Fatal("bad int")
	}
	if v, err := queryBool(r, "b"); err != nil || !*v {
		t.Fatal("bool")
	}
	if _, err := queryBool(r, "bad"); err == nil {
		t.Fatal("bad bool")
	}
	if v, _ := queryBool(r, "missing"); v != nil {
		t.Fatal("missing bool")
	}
	if v := queryString(r, "c"); v == nil || *v != "x" {
		t.Fatal("string")
	}
	if queryString(r, "missing") != nil {
		t.Fatal("missing string")
	}
}

func TestPathParsers(t *testing.T) {
	if id, err := extractAgentAlias("/12/inbox/list"); *id != 12 || *err != "inbox" {
		t.Fatal("alias")
	}
	if id, alias := extractAgentAlias("/12/datasets"); *id != 12 || alias != nil {
		t.Fatal("discovery")
	}
	if id, _ := extractAgentAlias("/abc/x"); id != nil {
		t.Fatal("non-numeric agent")
	}
	for path, want := range map[string]string{
		"/1/datasets":       "datasets",
		"/1/inbox/list":     "list",
		"/1/inbox/get/a/b":  "get",
		"/1/inbox/whatever": "whatever",
		"/1":                "unknown",
	} {
		if got := extractEndpoint(path); got != want {
			t.Errorf("%s: got %s want %s", path, got, want)
		}
	}
}

func TestOpenAPISpecVersion(t *testing.T) {
	b, err := OpenAPISpec()
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]any
	if err := json.Unmarshal(b, &doc); err != nil {
		t.Fatal(err)
	}
	if doc["info"].(map[string]any)["version"] == "" {
		t.Fatal("missing version")
	}
}

func TestHealthStatsAndAuth(t *testing.T) {
	e := newTestEnv(t)
	if rec := e.do("GET", "/manage/health", "", nil); rec.Code != 200 || decode(t, rec)["status"] != "ok" {
		t.Fatalf("health: %d %s", rec.Code, rec.Body)
	}
	if rec := e.do("GET", "/manage/stats", "", nil); rec.Code != 401 {
		t.Fatalf("stats without token: %d", rec.Code)
	}
	if rec := e.do("GET", "/manage/stats", "wrong", nil); rec.Code != 401 {
		t.Fatalf("stats bad token: %d", rec.Code)
	}
	rec := e.manage("GET", "/manage/stats", nil)
	if m := decode(t, rec); rec.Code != 200 || m["datasources"] != float64(0) {
		t.Fatalf("stats: %d %v", rec.Code, m)
	}
	if rec := e.do("GET", "/api-doc/openapi.json", "", nil); rec.Code != 200 {
		t.Fatalf("openapi: %d", rec.Code)
	}
	if rec := e.do("GET", "/swagger-ui/", "", nil); rec.Code != 200 || !strings.Contains(rec.Body.String(), "swagger") {
		t.Fatalf("swagger: %d", rec.Code)
	}
	e.srv.db.Close()
	if rec := e.do("GET", "/manage/health", "", nil); rec.Code != 503 {
		t.Fatalf("degraded health: %d", rec.Code)
	}
}

func TestSettingsRoutes(t *testing.T) {
	e := newTestEnv(t)
	rec := e.manage("GET", "/manage/settings/ai", nil)
	if m := decode(t, rec); m["enabled"] != false || m["api_key_configured"] != false {
		t.Fatalf("get: %v", m)
	}
	rec = e.manage("PUT", "/manage/settings/ai", map[string]any{"enabled": true, "model": " claude-x ", "api_key": "sk-1"})
	if m := decode(t, rec); rec.Code != 200 || m["enabled"] != true || m["model"] != "claude-x" || m["api_key_configured"] != true {
		t.Fatalf("put: %d %v", rec.Code, m)
	}
	cfg, err := ReadConfig(e.srv.dataDir)
	if err != nil || !cfg.AIEnabled || cfg.AnthropicModel != "claude-x" {
		t.Fatalf("not persisted: %+v %v", cfg, err)
	}
	// Absent api_key leaves it alone; null clears it.
	if m := decode(t, e.manage("POST", "/manage/settings/ai", map[string]any{})); m["api_key_configured"] != true {
		t.Fatal("absent key should be kept")
	}
	if m := decode(t, e.manage("POST", "/manage/settings/ai", `{"api_key": null}`)); m["api_key_configured"] != false {
		t.Fatal("null key should clear")
	}
	_ = secretstore.StoreAnthropicKey("k")
	if m := decode(t, e.manage("DELETE", "/manage/settings/ai/api-key", nil)); m["deleted"] != true {
		t.Fatal("delete key")
	}
	if rec := e.manage("PUT", "/manage/settings/ai", "{bad"); rec.Code != 400 {
		t.Fatalf("bad json: %d", rec.Code)
	}
}

func TestAuditRoute(t *testing.T) {
	e := newTestEnv(t)
	_, err := e.srv.db.Exec("INSERT INTO audit_log (agent_id, dataset_id, endpoint, status) VALUES (1, 2, 'list', 200), (3, 2, 'get', 404)")
	if err != nil {
		t.Fatal(err)
	}
	m := decode(t, e.manage("GET", "/manage/audit?dataset_id=2&limit=9999", nil))
	meta := m["meta"].(map[string]any)
	if meta["total"] != float64(2) || meta["limit"] != float64(500) || len(m["events"].([]any)) != 2 {
		t.Fatalf("got %v", m)
	}
	m = decode(t, e.manage("GET", "/manage/audit/?agent_id=3&offset=-4", nil))
	if m["meta"].(map[string]any)["offset"] != float64(0) || len(m["events"].([]any)) != 1 {
		t.Fatalf("got %v", m)
	}
	if rec := e.manage("GET", "/manage/audit?limit=abc", nil); rec.Code != 400 {
		t.Fatalf("bad limit: %d", rec.Code)
	}
}
