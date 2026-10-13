package server

import (
	"context"
	"fmt"
	"testing"

	"github.com/ipedrazas/sbx/internal/actions"
	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/models"
)

type approvalFixture struct {
	e       *testEnv
	agentID int64
	tokenID int64
	action  *models.Action
}

// newApprovalFixture creates a Linear datasource (API key auth, so simulate
// works offline), an agent + token, and returns the seeded create_issue action.
func newApprovalFixture(t *testing.T, cfg map[string]any) *approvalFixture {
	t.Helper()
	e := newTestEnv(t)
	ctx := context.Background()
	ds := dsCreate(t, e, map[string]any{"name": "lin", "ds_type": "linear", "config": cfg})
	a, err := actions.GetActionByType(ctx, e.srv.db, ds, "create_issue")
	if err != nil || a == nil {
		t.Fatal("create_issue not seeded", err)
	}
	agentID, err := crud.CreateAgent(ctx, e.srv.db, "Bot", nil)
	if err != nil {
		t.Fatal(err)
	}
	tokenID, err := crud.CreateAgentToken(ctx, e.srv.db, agentID, "hash", "t")
	if err != nil {
		t.Fatal(err)
	}
	return &approvalFixture{e: e, agentID: agentID, tokenID: tokenID, action: a}
}

func (f *approvalFixture) queue(t *testing.T, params map[string]any, simulated bool, key *string) int64 {
	t.Helper()
	id, err := actions.CreateApproval(context.Background(), f.e.srv.db, f.agentID, f.action.ID, &f.tokenID, key,
		params, "phash", simulated, nil)
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func TestApprovalsListGetDeny(t *testing.T) {
	f := newApprovalFixture(t, map[string]any{"api_key": "k"})
	e := f.e
	if rec := e.manage("GET", "/manage/approvals", nil); rec.Code != 200 || rec.Body.String() != `{"approvals":[]}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	id := f.queue(t, map[string]any{"team_id": "T", "title": "x"}, true, nil)

	rec := e.manage("GET", "/manage/approvals/?state=pending", nil)
	if l := decode(t, rec)["approvals"].([]any); len(l) != 1 {
		t.Fatal(rec.Body.String())
	}
	rec = e.manage("GET", fmt.Sprintf("/manage/approvals?agent_id=%d", f.agentID+1), nil)
	if l := decode(t, rec)["approvals"].([]any); len(l) != 0 {
		t.Fatal(rec.Body.String())
	}
	rec = e.manage("GET", "/manage/approvals?state=all", nil)
	if rec.Code != 400 || decode(t, rec)["error"] != "unsupported state filter 'all' (v1 supports 'pending')" {
		t.Fatal(rec.Body.String())
	}
	if rec := e.manage("GET", "/manage/approvals?agent_id=x", nil); rec.Code != 400 {
		t.Fatal(rec.Code)
	}

	path := fmt.Sprintf("/manage/approvals/%d", id)
	if rec := e.manage("GET", path, nil); rec.Code != 200 || decode(t, rec)["state"] != "pending" {
		t.Fatal(rec.Body.String())
	}
	if rec := e.manage("GET", "/manage/approvals/999", nil); rec.Code != 404 || decode(t, rec)["error"] != "approval not found" {
		t.Fatal(rec.Body.String())
	}

	if rec := e.manage("POST", path+"/deny", map[string]any{}); rec.Code != 422 {
		t.Fatal(rec.Code)
	}
	if rec := e.manage("POST", "/manage/approvals/999/deny", map[string]any{"resolved_by": "me"}); rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	rec = e.manage("POST", path+"/deny", map[string]any{"resolved_by": "me"})
	if rec.Code != 200 || rec.Body.String() != `{"state":"denied"}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rec = e.manage("POST", path+"/deny", map[string]any{"resolved_by": "me"})
	if rec.Code != 409 || decode(t, rec)["error"] != "approval is already denied" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := e.manage("POST", path+"/approve", map[string]any{"resolved_by": "me"}); rec.Code != 409 {
		t.Fatal(rec.Code)
	}
}

func TestApprovalsApproveSimulatedCachesIdempotency(t *testing.T) {
	f := newApprovalFixture(t, map[string]any{"api_key": "k"})
	key := "idem-1"
	id := f.queue(t, map[string]any{"team_id": "T", "title": "x"}, true, &key)
	rec := f.e.manage("POST", fmt.Sprintf("/manage/approvals/%d/approve", id), map[string]any{"resolved_by": "me"})
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	if m["state"] != "executed" || m["result"].(map[string]any)["identifier"] == nil {
		t.Fatal(m)
	}
	ctx := context.Background()
	ap, _ := actions.GetApproval(ctx, f.e.srv.db, id)
	if ap.State != models.ApprovalExecuted || ap.ResolvedBy == nil || *ap.ResolvedBy != "me" {
		t.Fatalf("%+v", ap)
	}
	cached, err := actions.LookupIdempotency(ctx, f.e.srv.db, f.tokenID, f.action.ID, key, true)
	if err != nil || cached == nil {
		t.Fatal("idempotency not cached", err)
	}
}

func TestApprovalsApproveFailures(t *testing.T) {
	f := newApprovalFixture(t, map[string]any{"api_key": "k"})
	ctx := context.Background()

	// Real invoke with invalid params fails before any network → 502, row Failed.
	id := f.queue(t, map[string]any{}, false, nil)
	rec := f.e.manage("POST", fmt.Sprintf("/manage/approvals/%d/approve", id), map[string]any{"resolved_by": "me"})
	if rec.Code != 502 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	ap, _ := actions.GetApproval(ctx, f.e.srv.db, id)
	if ap.State != models.ApprovalFailed || ap.Result.(map[string]any)["error"] == nil {
		t.Fatalf("%+v", ap)
	}

	if rec := f.e.manage("POST", "/manage/approvals/999/approve", map[string]any{"resolved_by": "me"}); rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	if rec := f.e.manage("POST", fmt.Sprintf("/manage/approvals/%d/approve", id), "{"); rec.Code != 400 {
		t.Fatal(rec.Code)
	}
}

func TestApprovalsApproveConnectorUnavailable(t *testing.T) {
	// No API key / OAuth tokens → connector construction fails → 502.
	f := newApprovalFixture(t, map[string]any{})
	id := f.queue(t, map[string]any{"team_id": "T", "title": "x"}, true, nil)
	rec := f.e.manage("POST", fmt.Sprintf("/manage/approvals/%d/approve", id), map[string]any{"resolved_by": "me"})
	if rec.Code != 502 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
}

func TestApprovalsApproveMissingActionOrDatasource(t *testing.T) {
	f := newApprovalFixture(t, map[string]any{"api_key": "k"})
	ctx := context.Background()
	db := f.e.srv.db
	// The in-memory DB uses a single connection, so the PRAGMA sticks.
	conn := db
	defer func() { _, _ = db.ExecContext(ctx, "PRAGMA foreign_keys = ON") }()

	id := f.queue(t, map[string]any{"team_id": "T", "title": "x"}, true, nil)
	// Point the approval at a datasource-less action, then at a missing action.
	if _, err := conn.ExecContext(ctx, "PRAGMA foreign_keys = OFF"); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.ExecContext(ctx, "UPDATE actions SET datasource_id = 999 WHERE id = ?", f.action.ID); err != nil {
		t.Skip("cannot detach action from datasource:", err)
	}
	rec := f.e.manage("POST", fmt.Sprintf("/manage/approvals/%d/approve", id), map[string]any{"resolved_by": "me"})
	if rec.Code != 400 || decode(t, rec)["error"] != "action's datasource no longer exists" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	id2 := f.queue(t, map[string]any{}, true, nil)
	if _, err := conn.ExecContext(ctx, "UPDATE action_approvals SET action_id = 999 WHERE id = ?", id2); err != nil {
		t.Skip("cannot detach approval from action:", err)
	}
	rec = f.e.manage("POST", fmt.Sprintf("/manage/approvals/%d/approve", id2), map[string]any{"resolved_by": "me"})
	if rec.Code != 400 || decode(t, rec)["error"] != "action no longer exists" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	ap, _ := actions.GetApproval(ctx, db, id2)
	if ap.State != models.ApprovalFailed {
		t.Fatalf("%+v", ap)
	}
}
