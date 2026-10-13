package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/ipedrazas/sbx/internal/actions"
	"github.com/ipedrazas/sbx/internal/models"
)

func TestActionsScopeMatches(t *testing.T) {
	cases := []struct {
		g, r string
		want bool
	}{
		{"gmail.compose", "gmail.compose", true},
		{"https://www.googleapis.com/auth/gmail.compose", "gmail.compose", true},
		{"crm.objects.deals.read", "deals.read", true},
		{"read", "write", false},
		{"xgmail.compose", "gmail.compose", false},
	}
	for _, c := range cases {
		if got := scopeMatches(c.g, c.r); got != c.want {
			t.Errorf("%s/%s: %v", c.g, c.r, got)
		}
	}
}

func TestActionsEnrich(t *testing.T) {
	a := &models.Action{ID: 1, DatasourceID: 5, RequiredScopes: []string{"gmail.compose", "gmail.send"}}
	m := enrichAction(a, map[int64][]string{5: {"https://www.googleapis.com/auth/gmail.compose"}})
	if m["scopes_satisfied"] != false || len(m["missing_scopes"].([]string)) != 1 || m["id"] == nil {
		t.Fatal(m)
	}
	m = enrichAction(a, map[int64][]string{})
	if m["scopes_satisfied"] != true || len(m["missing_scopes"].([]string)) != 0 {
		t.Fatal(m)
	}
}

func TestActionsCRUD(t *testing.T) {
	e := newTestEnv(t)
	ds := dsCreate(t, e, map[string]any{"name": "r", "ds_type": "rest", "config": map[string]any{"base_url": "x"}})

	rec := e.manage("POST", "/manage/actions", map[string]any{"datasource_id": ds, "name": "Ping", "action_type": "ping"})
	if rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	id := int64(decode(t, rec)["id"].(float64))
	path := fmt.Sprintf("/manage/actions/%d", id)

	rec = e.manage("GET", path, nil)
	m := decode(t, rec)
	if rec.Code != 200 || m["policy_kind"] != "auto" || m["risk_level"] != "medium" ||
		m["approval_ttl_seconds"].(float64) != 86400 || m["scopes_satisfied"] != true {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	// Explicit null TTL = never expire; custom risk/policy.
	rec = e.manage("POST", "/manage/actions/", map[string]any{"datasource_id": ds, "name": "B", "action_type": "b",
		"approval_ttl_seconds": nil, "risk_level": "high", "policy_kind": "always_approve",
		"required_scopes": []string{"s"}, "params_schema": map[string]any{"type": "object"}})
	if rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	id2 := int64(decode(t, rec)["id"].(float64))
	m = decode(t, e.manage("GET", fmt.Sprintf("/manage/actions/%d", id2), nil))
	if _, ok := m["approval_ttl_seconds"]; ok || m["risk_level"] != "high" || m["policy_kind"] != "always_approve" {
		t.Fatal(m)
	}

	for _, body := range []map[string]any{
		{"name": "x", "action_type": "x"},
		{"datasource_id": ds, "action_type": "x"},
		{"datasource_id": ds, "name": "x"},
		{"datasource_id": ds, "name": "x", "action_type": "y", "risk_level": "extreme"},
		{"datasource_id": ds, "name": "x", "action_type": "y", "policy_kind": "never"},
		{"datasource_id": ds, "name": "x", "action_type": "y", "approval_ttl_seconds": "1h"},
	} {
		if rec := e.manage("POST", "/manage/actions", body); rec.Code != 422 {
			t.Fatalf("%v: %d %s", body, rec.Code, rec.Body.String())
		}
	}
	// Duplicate action_type on same datasource → DB error → 400.
	if rec := e.manage("POST", "/manage/actions", map[string]any{"datasource_id": ds, "name": "P", "action_type": "ping"}); rec.Code != 400 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	rec = e.manage("GET", fmt.Sprintf("/manage/actions?datasource_id=%d", ds), nil)
	if l := decode(t, rec)["actions"].([]any); len(l) != 6 { // 4 predefined REST + 2
		t.Fatal(rec.Body.String())
	}
	if rec := e.manage("GET", "/manage/actions?datasource_id=zz", nil); rec.Code != 400 {
		t.Fatal(rec.Code)
	}

	upd := map[string]any{"name": "Ping2", "approval_ttl_seconds": 60}
	if rec := e.manage("PUT", path, upd); rec.Code != 200 || rec.Body.String() != `{"updated":true}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := e.manage("POST", path+"/update", upd); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if m := decode(t, e.manage("GET", path, nil)); m["name"] != "Ping2" || m["approval_ttl_seconds"].(float64) != 60 {
		t.Fatal(m)
	}
	if rec := e.manage("PUT", "/manage/actions/999", upd); rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	if rec := e.manage("PUT", path, map[string]any{}); rec.Code != 422 {
		t.Fatal(rec.Code)
	}

	if rec := e.manage("POST", path+"/state", map[string]any{"state": "enabled"}); rec.Code != 200 ||
		rec.Body.String() != `{"state":"enabled"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := e.manage("POST", path+"/state", map[string]any{"state": "bogus"}); rec.Code != 422 {
		t.Fatal(rec.Code)
	}
	if rec := e.manage("POST", path+"/state", map[string]any{}); rec.Code != 422 {
		t.Fatal(rec.Code)
	}
	if rec := e.manage("POST", "/manage/actions/999/state", map[string]any{"state": "paused"}); rec.Code != 404 {
		t.Fatal(rec.Code)
	}

	if rec := e.manage("DELETE", path, nil); rec.Code != 200 || rec.Body.String() != `{"deleted":true}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := e.manage("POST", path+"/delete", nil); rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	if rec := e.manage("GET", path, nil); rec.Code != 404 || decode(t, rec)["error"] != "action not found" {
		t.Fatal(rec.Code)
	}
}

func TestActionsPredefinedAndScopes(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	rec := e.manage("POST", "/manage/oauth/connections", map[string]any{
		"name": "l", "provider": "linear", "client_id": "c", "scopes": []string{"read"}})
	connID := int64(decode(t, rec)["id"].(float64))
	ds := dsCreate(t, e, map[string]any{"name": "lin", "ds_type": "linear", "config": map[string]any{}, "connection_id": connID})
	acts, _ := actions.ListActions(ctx, e.srv.db, &ds)
	if len(acts) == 0 {
		t.Fatal("no predefined actions")
	}
	rec = e.manage("DELETE", fmt.Sprintf("/manage/actions/%d", acts[0].ID), nil)
	if rec.Code != 403 || decode(t, rec)["error"] != "predefined actions cannot be deleted" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	rec = e.manage("POST", "/manage/actions", map[string]any{"datasource_id": ds, "name": "W", "action_type": "w",
		"required_scopes": []string{"write"}})
	id := int64(decode(t, rec)["id"].(float64))
	m := decode(t, e.manage("GET", fmt.Sprintf("/manage/actions/%d", id), nil))
	if m["scopes_satisfied"] != false || len(m["missing_scopes"].([]any)) != 1 {
		t.Fatal(m)
	}
	rec = e.manage("GET", "/manage/actions", nil)
	if len(decode(t, rec)["actions"].([]any)) != len(acts)+1 {
		t.Fatal(rec.Body.String())
	}
}

func TestActionsTest(t *testing.T) {
	e := newTestEnv(t)
	ctx := context.Background()
	ds := dsCreate(t, e, map[string]any{"name": "lin", "ds_type": "linear", "config": map[string]any{"api_key": "k"}})
	a, err := actions.GetActionByType(ctx, e.srv.db, ds, "create_issue")
	if err != nil || a == nil {
		t.Fatal("create_issue not seeded", err)
	}
	path := fmt.Sprintf("/manage/actions/%d/test", a.ID)
	rec := e.manage("POST", path, map[string]any{"params": map[string]any{"team_id": "T", "title": "Hi"}})
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if r := decode(t, rec)["result"].(map[string]any); r["identifier"] == nil {
		t.Fatal(r)
	}
	// Invalid params → connector Query error → 502.
	if rec := e.manage("POST", path, map[string]any{}); rec.Code != 502 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := e.manage("POST", "/manage/actions/999/test", map[string]any{}); rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	// Datasource without credentials → connector config error.
	ds2 := dsCreate(t, e, map[string]any{"name": "lin2", "ds_type": "linear", "config": map[string]any{}})
	a2, _ := actions.GetActionByType(ctx, e.srv.db, ds2, "create_issue")
	if rec := e.manage("POST", fmt.Sprintf("/manage/actions/%d/test", a2.ID), map[string]any{}); rec.Code != 500 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}
