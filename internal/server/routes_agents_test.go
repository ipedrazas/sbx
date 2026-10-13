package server

import (
	"archive/zip"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ipedrazas/sbx/internal/actions"
	"github.com/ipedrazas/sbx/internal/agentdir"
	"github.com/ipedrazas/sbx/internal/clidist"
	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/models"
)

type agentsFixture struct {
	e                        *testEnv
	agentID, dsID, datasetID int64
	actionID                 int64
}

func newAgentsFixture(t *testing.T) *agentsFixture {
	t.Helper()
	e := newTestEnv(t)
	ctx := context.Background()
	f := &agentsFixture{e: e}
	rec := e.manage("POST", "/manage/agents", map[string]any{
		"name": "Mail Bot", "description": "Reads mail", "agent_type": "", "owner": "ivan", "mission": "triage",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create agent: %d %s", rec.Code, rec.Body)
	}
	f.agentID = agentsInt(t, decode(t, rec)["id"])
	var err error
	if f.dsID, err = crud.CreateDatasource(ctx, e.srv.db, "My Gmail", "gmail", "{}", nil); err != nil {
		t.Fatal(err)
	}
	if f.datasetID, err = crud.CreateDataset(ctx, e.srv.db, "Inbox", f.dsID, "pull_through", nil, nil,
		models.Ptr("Recent mail"), models.Ptr("email"), nil); err != nil {
		t.Fatal(err)
	}
	schema := map[string]any{"type": "object", "properties": map[string]any{"to": map[string]any{"type": "string"}}}
	if f.actionID, err = actions.CreateAction(ctx, e.srv.db, f.dsID, "Send", "send_email", nil,
		schema, map[string]any{}, models.PolicyAlwaysApprove, map[string]any{}, nil); err != nil {
		t.Fatal(err)
	}
	return f
}

func agentsInt(t *testing.T, v any) int64 {
	t.Helper()
	f, ok := v.(float64)
	if !ok {
		t.Fatalf("not a number: %#v", v)
	}
	return int64(f)
}

func (f *agentsFixture) path(format string, a ...any) string {
	return fmt.Sprintf("/manage/agents/%d", f.agentID) + fmt.Sprintf(format, a...)
}

func agentsExpect(t *testing.T, rec interface {
	Result() *http.Response
}, code int) {
	t.Helper()
	if got := rec.Result().StatusCode; got != code {
		t.Fatalf("status %d, want %d", got, code)
	}
}

func TestAgentsCRUD(t *testing.T) {
	f := newAgentsFixture(t)
	e := f.e

	rec := e.manage("GET", "/manage/agents/", nil)
	agentsExpect(t, rec, 200)
	agents := decode(t, rec)["agents"].([]any)
	if len(agents) != 1 {
		t.Fatalf("agents: %v", agents)
	}
	a := agents[0].(map[string]any)
	if a["owner"] != "ivan" || a["agent_type"] != nil {
		t.Fatalf("meta: %v", a)
	}

	rec = e.manage("GET", f.path(""), nil)
	agentsExpect(t, rec, 200)
	body := decode(t, rec)
	if body["agent"].(map[string]any)["name"] != "Mail Bot" || len(body["datasets"].([]any)) != 0 {
		t.Fatalf("get: %v", body)
	}

	agentsExpect(t, e.manage("GET", "/manage/agents/999", nil), 404)
	agentsExpect(t, e.manage("GET", "/manage/agents/abc", nil), 400)

	// Update (PUT and POST alias) resyncs the agent dir.
	rec = e.manage("PUT", f.path(""), map[string]any{"name": "Mail Bot", "status": "active"})
	agentsExpect(t, rec, 200)
	if decode(t, rec)["updated"] != true {
		t.Fatal(rec.Body)
	}
	cfgPath := filepath.Join(agentdir.AgentPath(e.srv.dataDir, "Mail Bot"), "config.json")
	if _, err := os.Stat(cfgPath); err != nil {
		t.Fatalf("agent dir not synced: %v", err)
	}
	agentsExpect(t, e.manage("POST", f.path("/update"), map[string]any{"name": "Mail Bot"}), 200)
	agentsExpect(t, e.manage("PUT", "/manage/agents/999", map[string]any{"name": "x"}), 404)
	agentsExpect(t, e.manage("PUT", f.path(""), map[string]any{"description": "x"}), 422)
	agentsExpect(t, e.manage("PUT", f.path(""), "{bad"), 400)
	agentsExpect(t, e.manage("POST", "/manage/agents", map[string]any{}), 422)

	// Delete removes the dir.
	agentsExpect(t, e.manage("POST", f.path("/delete"), nil), 200)
	if _, err := os.Stat(filepath.Dir(cfgPath)); !os.IsNotExist(err) {
		t.Fatalf("agent dir not removed: %v", err)
	}
	rec = e.manage("DELETE", f.path(""), nil)
	agentsExpect(t, rec, 404)
	if decode(t, rec)["error"] != "not found" {
		t.Fatal(rec.Body)
	}
}

func TestAgentsDatasetBindings(t *testing.T) {
	f := newAgentsFixture(t)
	e := f.e

	agentsExpect(t, e.manage("POST", "/manage/agents/999/datasets", map[string]any{"dataset_id": f.datasetID, "alias": "x"}), 404)
	rec := e.manage("POST", f.path("/datasets"), map[string]any{"dataset_id": 999, "alias": "x"})
	agentsExpect(t, rec, 400)
	if decode(t, rec)["error"] != "dataset not found" {
		t.Fatal(rec.Body)
	}
	agentsExpect(t, e.manage("POST", f.path("/datasets"), map[string]any{"alias": "x"}), 422)
	agentsExpect(t, e.manage("POST", f.path("/datasets"), map[string]any{"dataset_id": 1}), 422)

	rec = e.manage("POST", f.path("/datasets"), map[string]any{
		"dataset_id": f.datasetID, "alias": "inbox", "rate_limit_count": 5, "rate_limit_period_seconds": 60,
	})
	agentsExpect(t, rec, 201)
	// Duplicate alias → 409.
	agentsExpect(t, e.manage("POST", f.path("/datasets"), map[string]any{"dataset_id": f.datasetID, "alias": "inbox"}), 409)

	rec = e.manage("GET", f.path("/datasets"), nil)
	agentsExpect(t, rec, 200)
	ds := decode(t, rec)["datasets"].([]any)
	if len(ds) != 1 || ds[0].(map[string]any)["rate_limit_count"].(float64) != 5 {
		t.Fatalf("bindings: %v", ds)
	}

	// Prime a rate-limit bucket, then update: Forget must drop it.
	e.srv.rateLimiter.Check(f.agentID, BindingDataset, f.datasetID, 1, 60)
	if d := e.srv.rateLimiter.Check(f.agentID, BindingDataset, f.datasetID, 1, 60); d.Allowed {
		t.Fatal("bucket should be exhausted")
	}
	rec = e.manage("PUT", f.path("/datasets/%d", f.datasetID), map[string]any{
		"alias": "mail", "rate_limit_count": nil, "rate_limit_period_seconds": nil,
	})
	agentsExpect(t, rec, 200)
	if d := e.srv.rateLimiter.Check(f.agentID, BindingDataset, f.datasetID, 1, 60); !d.Allowed {
		t.Fatal("bucket not forgotten")
	}
	b, _ := crud.GetAgentBinding(context.Background(), e.srv.db, f.agentID, "mail")
	if b == nil || b.RateLimitCount != nil || b.RateLimitPeriodSeconds != nil {
		t.Fatalf("binding after update: %+v", b)
	}
	// Alias-only update leaves the limit alone.
	agentsExpect(t, e.manage("PUT", f.path("/datasets/%d", f.datasetID), map[string]any{"rate_limit_count": 3, "rate_limit_period_seconds": 10}), 200)
	agentsExpect(t, e.manage("PUT", f.path("/datasets/%d", f.datasetID), map[string]any{"alias": "mail2"}), 200)
	b, _ = crud.GetAgentBinding(context.Background(), e.srv.db, f.agentID, "mail2")
	if b == nil || b.RateLimitCount == nil || *b.RateLimitCount != 3 {
		t.Fatalf("binding after alias update: %+v", b)
	}
	agentsExpect(t, e.manage("PUT", f.path("/datasets/%d", f.datasetID), map[string]any{"rate_limit_count": "x"}), 422)

	// Metrics keyed by alias.
	rec = e.manage("GET", f.path("/metrics"), nil)
	agentsExpect(t, rec, 200)
	m := decode(t, rec)
	if agentsInt(t, m["agent_id"]) != f.agentID {
		t.Fatal(m)
	}
	if _, ok := m["datasets"].(map[string]any)["mail2"]; !ok {
		t.Fatalf("metrics: %v", m)
	}

	e.srv.rateLimiter.Check(f.agentID, BindingDataset, f.datasetID, 1, 60)
	agentsExpect(t, e.manage("POST", f.path("/datasets/%d/unbind", f.datasetID), nil), 200)
	if d := e.srv.rateLimiter.Check(f.agentID, BindingDataset, f.datasetID, 1, 60); !d.Allowed {
		t.Fatal("bucket not forgotten on unbind")
	}
	rec = e.manage("DELETE", f.path("/datasets/%d", f.datasetID), nil)
	agentsExpect(t, rec, 404)
	if decode(t, rec)["error"] != "binding not found" {
		t.Fatal(rec.Body)
	}
}

func TestAgentsTokens(t *testing.T) {
	f := newAgentsFixture(t)
	e := f.e

	agentsExpect(t, e.manage("POST", "/manage/agents/999/tokens", map[string]any{}), 404)
	rec := e.manage("POST", f.path("/tokens"), map[string]any{"label": "ci"})
	agentsExpect(t, rec, 201)
	body := decode(t, rec)
	tok := body["token"].(string)
	if tok == "" || body["message"] != "Store this token securely — it won't be shown again" {
		t.Fatal(body)
	}
	tokenID := agentsInt(t, body["id"])
	// Plaintext lands in config.json and is preserved across later resyncs.
	cfgPath := filepath.Join(agentdir.AgentPath(e.srv.dataDir, "Mail Bot"), "config.json")
	readTok := func() any {
		raw, err := os.ReadFile(cfgPath)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		_ = json.Unmarshal(raw, &m)
		return m["token"]
	}
	if readTok() != tok {
		t.Fatal("token not written")
	}
	agentsExpect(t, e.manage("PUT", f.path(""), map[string]any{"name": "Mail Bot"}), 200)
	if readTok() != tok {
		t.Fatal("token not preserved on resync")
	}

	rec = e.manage("POST", f.path("/tokens"), map[string]any{})
	agentsExpect(t, rec, 201)

	rec = e.manage("GET", f.path("/tokens"), nil)
	agentsExpect(t, rec, 200)
	toks := decode(t, rec)["tokens"].([]any)
	if len(toks) != 2 {
		t.Fatalf("tokens: %v", toks)
	}
	labels := map[any]bool{}
	for _, x := range toks {
		labels[x.(map[string]any)["label"]] = true
	}
	if !labels["ci"] || !labels["default"] {
		t.Fatalf("labels: %v", labels)
	}

	rec = e.manage("POST", f.path("/tokens/%d/force-simulate", tokenID), map[string]any{"force_simulate": true})
	agentsExpect(t, rec, 200)
	if decode(t, rec)["force_simulate"] != true {
		t.Fatal(rec.Body)
	}
	rec = e.manage("POST", f.path("/tokens/999/force-simulate"), map[string]any{"force_simulate": true})
	agentsExpect(t, rec, 404)
	if decode(t, rec)["error"] != "token not found" {
		t.Fatal(rec.Body)
	}
	agentsExpect(t, e.manage("POST", f.path("/tokens/%d/force-simulate", tokenID), map[string]any{}), 422)

	agentsExpect(t, e.manage("POST", f.path("/tokens/%d/revoke", tokenID), nil), 200)
	agentsExpect(t, e.manage("DELETE", f.path("/tokens/%d", tokenID), nil), 404)
}

func TestAgentsConfigAndSkill(t *testing.T) {
	f := newAgentsFixture(t)
	e := f.e
	agentsExpect(t, e.manage("POST", f.path("/datasets"), map[string]any{"dataset_id": f.datasetID, "alias": "inbox"}), 201)

	for _, ep := range []string{"/generate-cli-config", "/generate-mcp-config"} {
		rec := e.manage("POST", f.path("%s", ep), nil)
		agentsExpect(t, rec, 200)
		cfg := decode(t, rec)
		if cfg["token"] != agentdir.TokenPlaceholder || !strings.Contains(rec.Body.String(), "127.0.0.1:9820") {
			t.Fatalf("%s: %v", ep, cfg)
		}
		rec = e.manage("POST", f.path("%s", ep), map[string]any{"daemon_url": "http://example.test:1"})
		agentsExpect(t, rec, 200)
		if !strings.Contains(rec.Body.String(), "example.test:1") {
			t.Fatalf("override ignored: %s", rec.Body)
		}
	}
	rec := e.manage("POST", "/manage/agents/999/generate-cli-config", nil)
	agentsExpect(t, rec, 500)
	if decode(t, rec)["error"] != "agent not found" {
		t.Fatal(rec.Body)
	}
	agentsExpect(t, e.manage("POST", f.path("/generate-cli-config"), "{bad"), 400)

	rec = e.manage("POST", f.path("/generate-skill"), map[string]any{"daemon_url": "http://skill.test"})
	agentsExpect(t, rec, 200)
	md := decode(t, rec)["skill"].(string)
	if !strings.Contains(md, "inbox") || !strings.Contains(md, "http://skill.test") {
		t.Fatalf("skill: %s", md)
	}
	agentsExpect(t, e.manage("POST", "/manage/agents/999/generate-skill", nil), 500)
}

func TestAgentsActionBindings(t *testing.T) {
	f := newAgentsFixture(t)
	e := f.e
	ctx := context.Background()

	agentsExpect(t, e.manage("POST", "/manage/agents/999/actions", map[string]any{"action_id": f.actionID, "alias": "send"}), 404)
	rec := e.manage("POST", f.path("/actions"), map[string]any{"action_id": 999, "alias": "send"})
	agentsExpect(t, rec, 400)
	if decode(t, rec)["error"] != "action not found" {
		t.Fatal(rec.Body)
	}
	agentsExpect(t, e.manage("POST", f.path("/actions"), map[string]any{"alias": "send"}), 422)
	agentsExpect(t, e.manage("POST", f.path("/actions"), map[string]any{"action_id": f.actionID}), 422)

	rec = e.manage("POST", f.path("/actions"), map[string]any{"action_id": f.actionID, "alias": "send", "template": "  To: {{to}}  "})
	agentsExpect(t, rec, 201)
	agentsExpect(t, e.manage("POST", f.path("/actions"), map[string]any{"action_id": f.actionID, "alias": "send"}), 409)

	getBinding := func() models.AgentAction {
		bs, err := actions.ListAgentActions(ctx, e.srv.db, f.agentID)
		if err != nil || len(bs) != 1 {
			t.Fatalf("bindings: %v %v", bs, err)
		}
		return bs[0]
	}
	if b := getBinding(); b.Template == nil || *b.Template != "To: {{to}}" {
		t.Fatalf("template not trimmed: %+v", b.Template)
	}

	rec = e.manage("GET", f.path("/actions"), nil)
	agentsExpect(t, rec, 200)
	if len(decode(t, rec)["bindings"].([]any)) != 1 {
		t.Fatal(rec.Body)
	}

	e.srv.rateLimiter.Check(f.agentID, BindingAction, f.actionID, 1, 60)
	rec = e.manage("PUT", f.path("/actions/%d", f.actionID), map[string]any{
		"alias": "mail", "rate_limit_count": 2, "rate_limit_period_seconds": 30,
		"template": "   ", "pinned_params": map[string]any{"to": "a@b.c"},
	})
	agentsExpect(t, rec, 200)
	if d := e.srv.rateLimiter.Check(f.agentID, BindingAction, f.actionID, 1, 60); !d.Allowed {
		t.Fatal("action bucket not forgotten")
	}
	b := getBinding()
	if b.Alias != "mail" || b.Template != nil || b.RateLimitCount == nil || *b.RateLimitCount != 2 {
		t.Fatalf("after update: %+v", b)
	}
	if pp, _ := json.Marshal(b.PinnedParams); !strings.Contains(string(pp), "a@b.c") {
		t.Fatalf("pinned: %s", pp)
	}

	// Empty object clears pinned params; absent template leaves it.
	agentsExpect(t, e.manage("PUT", f.path("/actions/%d", f.actionID), map[string]any{"template": "T"}), 200)
	agentsExpect(t, e.manage("PUT", f.path("/actions/%d", f.actionID), map[string]any{"pinned_params": map[string]any{}}), 200)
	b = getBinding()
	if b.Template == nil || *b.Template != "T" || b.PinnedParams != nil {
		t.Fatalf("after clear: %+v", b)
	}
	agentsExpect(t, e.manage("PUT", f.path("/actions/%d", f.actionID), map[string]any{"template": nil, "pinned_params": nil}), 200)
	if b = getBinding(); b.Template != nil {
		t.Fatalf("template not cleared: %+v", b)
	}
	agentsExpect(t, e.manage("PUT", f.path("/actions/%d", f.actionID), map[string]any{"template": 5}), 422)
	agentsExpect(t, e.manage("PUT", f.path("/actions/%d", f.actionID), map[string]any{"rate_limit_period_seconds": "x"}), 422)
	rec = e.manage("PUT", f.path("/actions/999"), map[string]any{"alias": "x"})
	agentsExpect(t, rec, 404)
	if decode(t, rec)["error"] != "binding not found" {
		t.Fatal(rec.Body)
	}

	agentsExpect(t, e.manage("POST", f.path("/actions/%d/unbind", f.actionID), nil), 200)
	agentsExpect(t, e.manage("DELETE", f.path("/actions/%d", f.actionID), nil), 404)
}

func TestAgentsInstallations(t *testing.T) {
	f := newAgentsFixture(t)
	e := f.e
	folder := t.TempDir()

	rec := e.manage("POST", f.path("/installations"), map[string]any{"folder_path": "   "})
	agentsExpect(t, rec, 400)
	if decode(t, rec)["error"] != "folder_path is required" {
		t.Fatal(rec.Body)
	}
	agentsExpect(t, e.manage("POST", f.path("/installations"), map[string]any{}), 422)
	agentsExpect(t, e.manage("POST", "/manage/agents/999/installations", map[string]any{"folder_path": folder}), 404)

	rec = e.manage("POST", f.path("/installations"), map[string]any{"folder_path": folder})
	agentsExpect(t, rec, 201)
	body := decode(t, rec)
	target := body["target"].(string)
	if target != agentdir.InstallTargetPath(folder, "mail-bot") {
		t.Fatalf("target: %v", target)
	}
	if _, err := os.Stat(target); err != nil {
		t.Fatal(err)
	}
	instID := agentsInt(t, body["installation"].(map[string]any)["id"])

	rec = e.manage("GET", f.path("/installations"), nil)
	agentsExpect(t, rec, 200)
	if len(decode(t, rec)["installations"].([]any)) != 1 {
		t.Fatal(rec.Body)
	}

	agentsExpect(t, e.manage("POST", f.path("/installations/%d/remove", instID), nil), 200)
	if _, err := os.Stat(target); !os.IsNotExist(err) {
		t.Fatalf("file not removed: %v", err)
	}
	rec = e.manage("DELETE", f.path("/installations/%d", instID), nil)
	agentsExpect(t, rec, 404)
	if decode(t, rec)["error"] != "installation not found" {
		t.Fatal(rec.Body)
	}
}

// agentsSeedCLICache pre-populates the clidist cache so export never downloads.
func agentsSeedCLICache(t *testing.T, dataDir string) {
	t.Helper()
	ver := clidist.CLIVersion()
	for i := range clidist.Platforms {
		p := &clidist.Platforms[i]
		d := filepath.Join(dataDir, "bin", ver, p.Dir)
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, p.Exe), []byte("bin-"+p.Dir), 0o755); err != nil {
			t.Fatal(err)
		}
		if _, ok := clidist.CachedCLIBinary(dataDir, ver, p); !ok {
			t.Fatal("cache layout mismatch")
		}
	}
}

func TestAgentsExport(t *testing.T) {
	f := newAgentsFixture(t)
	e := f.e
	agentsSeedCLICache(t, e.srv.dataDir)

	// Explicit output dir, no zip.
	out := t.TempDir()
	rec := e.manage("POST", f.path("/export"), map[string]any{"output_dir": out, "daemon_url": " http://remote.test/ "})
	agentsExpect(t, rec, 200)
	body := decode(t, rec)
	pkg := body["package_dir"].(string)
	if _, has := body["zip_path"]; has {
		t.Fatal(body)
	}
	raw, err := os.ReadFile(filepath.Join(pkg, "config.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), "http://remote.test\"") || strings.Contains(string(raw), agentdir.TokenPlaceholder) {
		t.Fatalf("config: %s", raw)
	}

	// Explicit output dir with zip.
	rec = e.manage("POST", f.path("/export"), map[string]any{"output_dir": out, "zip": true})
	agentsExpect(t, rec, 200)
	body = decode(t, rec)
	if _, err := os.Stat(body["zip_path"].(string)); err != nil {
		t.Fatal(err)
	}

	// Temp dir + zip streams the bundle.
	rec = e.manage("POST", f.path("/export"), map[string]any{"zip": true})
	agentsExpect(t, rec, 200)
	if rec.Header().Get("Content-Type") != "application/zip" ||
		rec.Header().Get("Content-Disposition") != `attachment; filename="bundle.zip"` {
		t.Fatal(rec.Header())
	}
	zr, err := zip.NewReader(bytes.NewReader(rec.Body.Bytes()), int64(rec.Body.Len()))
	if err != nil || len(zr.File) == 0 {
		t.Fatalf("zip: %v", err)
	}

	// Temp dir without zip: package dir is reported (and cleaned up afterwards).
	rec = e.manage("POST", f.path("/export"), map[string]any{"output_dir": "  "})
	agentsExpect(t, rec, 200)
	if _, ok := decode(t, rec)["package_dir"].(string); !ok {
		t.Fatal(rec.Body)
	}

	// Each export mints a token.
	toks, _ := crud.GetAgentTokens(context.Background(), e.srv.db, f.agentID)
	if len(toks) != 4 || !strings.HasPrefix(toks[0].Label, "export-") {
		t.Fatalf("tokens: %+v", toks)
	}

	// Temp dir failure.
	old := agentExportTempDir
	agentExportTempDir = func() (string, error) { return "", fmt.Errorf("boom") }
	rec = e.manage("POST", f.path("/export"), map[string]any{})
	agentExportTempDir = old
	agentsExpect(t, rec, 500)
	if decode(t, rec)["error"] != "failed to create temp dir: boom" {
		t.Fatal(rec.Body)
	}

	// Unknown agent: token insert (FK) or export fails → 500.
	agentsExpect(t, e.manage("POST", "/manage/agents/999/export", map[string]any{"output_dir": out}), 500)
	agentsExpect(t, e.manage("POST", f.path("/export"), "{bad"), 400)
}

func TestAgentsDBErrors(t *testing.T) {
	f := newAgentsFixture(t)
	e := f.e
	e.srv.db.Close()
	for _, c := range []struct {
		method, path string
		body         any
	}{
		{"GET", "/manage/agents", nil},
		{"POST", "/manage/agents", map[string]any{"name": "x"}},
		{"GET", f.path(""), nil},
		{"PUT", f.path(""), map[string]any{"name": "x"}},
		{"DELETE", f.path(""), nil},
		{"GET", f.path("/datasets"), nil},
		{"POST", f.path("/datasets"), map[string]any{"dataset_id": 1, "alias": "a"}},
		{"PUT", f.path("/datasets/1"), map[string]any{"alias": "a"}},
		{"PUT", f.path("/datasets/1"), map[string]any{"rate_limit_count": 1}},
		{"DELETE", f.path("/datasets/1"), nil},
		{"GET", f.path("/tokens"), nil},
		{"POST", f.path("/tokens"), map[string]any{}},
		{"DELETE", f.path("/tokens/1"), nil},
		{"POST", f.path("/tokens/1/force-simulate"), map[string]any{"force_simulate": false}},
		{"POST", f.path("/generate-skill"), nil},
		{"GET", f.path("/actions"), nil},
		{"POST", f.path("/actions"), map[string]any{"action_id": 1, "alias": "a"}},
		{"PUT", f.path("/actions/1"), map[string]any{}},
		{"DELETE", f.path("/actions/1"), nil},
		{"GET", f.path("/installations"), nil},
		{"POST", f.path("/installations"), map[string]any{"folder_path": "/tmp/x"}},
		{"DELETE", f.path("/installations/1"), nil},
		{"POST", f.path("/export"), map[string]any{}},
	} {
		if rec := e.manage(c.method, c.path, c.body); rec.Code < 400 {
			t.Errorf("%s %s: %d", c.method, c.path, rec.Code)
		}
	}
	// Metrics degrade to an empty map.
	rec := e.manage("GET", f.path("/metrics"), nil)
	agentsExpect(t, rec, 200)
}
