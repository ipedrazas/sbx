package server

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ipedrazas/sbx/internal/actions"
	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/util"
)

type actFixture struct {
	e        *testEnv
	ctx      context.Context
	agentID  int64
	tokenID  int64
	token    string
	dsID     int64
	actionID int64
	bindID   int64
}

var actDraft = map[string]any{"to": "a@b.com", "subject": "hi", "body": "hello"}

// newActFixture binds a Gmail create_draft action (simulation needs no network)
// to an agent under alias "draft".
func newActFixture(t *testing.T, policy models.PolicyKind) *actFixture {
	t.Helper()
	f := &actFixture{e: newTestEnv(t), ctx: context.Background(), token: "act-token"}
	db := f.e.srv.db
	var err error
	if f.agentID, err = crud.CreateAgent(f.ctx, db, "writer", nil); err != nil {
		t.Fatal(err)
	}
	if f.tokenID, err = crud.CreateAgentToken(f.ctx, db, f.agentID, util.HashToken(f.token), "t"); err != nil {
		t.Fatal(err)
	}
	if f.dsID, err = crud.CreateDatasource(f.ctx, db, "mail", "gmail", `{"client_id":"cid","client_secret":"sec"}`, nil); err != nil {
		t.Fatal(err)
	}
	ttl := int64(3600)
	if f.actionID, err = actions.CreateAction(f.ctx, db, f.dsID, "Create draft", "create_draft", nil,
		map[string]any{}, map[string]any{}, policy, nil, &ttl); err != nil {
		t.Fatal(err)
	}
	if f.bindID, err = actions.BindAgentAction(f.ctx, db, f.agentID, f.actionID, "draft", nil); err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *actFixture) invoke(query, key string, body any) *httptest.ResponseRecorder {
	f.e.t.Helper()
	return f.invokeAs(f.token, query, key, body)
}

func (f *actFixture) invokeAs(token, query, key string, body any) *httptest.ResponseRecorder {
	f.e.t.Helper()
	raw, ok := body.(string)
	if !ok {
		raw = util.JSONString(body)
	}
	req := httptest.NewRequest("POST", fmt.Sprintf("/act/%d/draft%s", f.agentID, query), strings.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	if key != "" {
		req.Header.Set("Idempotency-Key", key)
	}
	rec := httptest.NewRecorder()
	f.e.handler.ServeHTTP(rec, req)
	return rec
}

func actWantStatus(t *testing.T, rec *httptest.ResponseRecorder, code int) map[string]any {
	t.Helper()
	if rec.Code != code {
		t.Fatalf("want %d, got %d: %s", code, rec.Code, rec.Body.String())
	}
	return decode(t, rec)
}

func TestActAuthAndValidation(t *testing.T) {
	f := newActFixture(t, models.PolicyAuto)

	m := actWantStatus(t, f.invokeAs("", "", "k", actDraft), 401)
	if m["error"] != "missing or invalid Authorization header" {
		t.Fatal(m)
	}
	if m := actWantStatus(t, f.invokeAs("bad", "", "k", actDraft), 401); m["error"] != "invalid token" {
		t.Fatal(m)
	}
	rec := f.e.do("POST", "/act/999/draft", f.token, actDraft)
	if m := actWantStatus(t, rec, 401); m["error"] != "token does not match agent" {
		t.Fatal(m)
	}
	if m := actWantStatus(t, f.invoke("", "", actDraft), 400); m["error"] != "missing Idempotency-Key header" {
		t.Fatal(m)
	}
	req := httptest.NewRequest("POST", fmt.Sprintf("/act/%d/nope", f.agentID), strings.NewReader("{}"))
	req.Header.Set("Authorization", "Bearer "+f.token)
	req.Header.Set("Idempotency-Key", "k")
	rec = httptest.NewRecorder()
	f.e.handler.ServeHTTP(rec, req)
	if m := actWantStatus(t, rec, 404); m["error"] != "action alias 'nope' not found for this agent" {
		t.Fatal(m)
	}
	actWantStatus(t, f.invoke("", "k", "{not json"), 400)
	actWantStatus(t, f.e.do("POST", "/act/x/draft", f.token, actDraft), 400)
	actWantStatus(t, f.invoke("?simulated=maybe", "k", actDraft), 400)
}

func TestActSimulateAndReplay(t *testing.T) {
	f := newActFixture(t, models.PolicyAuto)

	m := actWantStatus(t, f.invoke("?simulated=true", "k1", actDraft), 200)
	res := m["result"].(map[string]any)
	meta := m["metadata"].(map[string]any)
	if !strings.HasPrefix(res["draft_id"].(string), "r-SIM-") || meta["replayed"] != false || meta["simulated"] != true ||
		meta["action_type"] != "create_draft" || meta["action_id"].(float64) != float64(f.actionID) {
		t.Fatalf("body = %v", m)
	}

	// Same key + same body → replay.
	m2 := actWantStatus(t, f.invoke("?simulated=true", "k1", actDraft), 200)
	if m2["metadata"].(map[string]any)["replayed"] != true || m2["result"].(map[string]any)["draft_id"] != res["draft_id"] {
		t.Fatalf("replay = %v", m2)
	}
	// Same key, different body → 409.
	other := map[string]any{"to": "x@y.com", "subject": "s", "body": "b"}
	if m := actWantStatus(t, f.invoke("?simulated=true", "k1", other), 409); m["error"] != "Idempotency-Key reused with a different request body" {
		t.Fatal(m)
	}

	// Connector validation errors propagate (and are not cached).
	actWantStatus(t, f.invoke("?simulated=true", "k2", map[string]any{"to": "a"}), 502)
	actWantStatus(t, f.invoke("?simulated=true", "k2", actDraft), 200)

	// Real invocation without OAuth tokens fails upstream auth; nothing is cached.
	rec := f.invoke("", "k3", actDraft)
	if rec.Code == 200 {
		t.Fatalf("real invoke unexpectedly succeeded: %s", rec.Body.String())
	}
	if c, _ := actions.LookupIdempotency(f.ctx, f.e.srv.db, f.tokenID, f.actionID, "k3", false); c != nil {
		t.Fatal("failed invoke must not be cached")
	}
}

func TestActForceSimulateToken(t *testing.T) {
	f := newActFixture(t, models.PolicyAuto)
	if _, err := actions.SetTokenForceSimulate(f.ctx, f.e.srv.db, f.tokenID, true); err != nil {
		t.Fatal(err)
	}
	m := actWantStatus(t, f.invoke("?simulated=false", "k", actDraft), 200)
	if m["metadata"].(map[string]any)["simulated"] != true {
		t.Fatalf("force_simulate ignored: %v", m)
	}
}

func TestActStateReadOnlyAndRateLimit(t *testing.T) {
	f := newActFixture(t, models.PolicyAuto)
	db := f.e.srv.db

	if _, err := actions.SetActionState(f.ctx, db, f.actionID, models.ActionPaused); err != nil {
		t.Fatal(err)
	}
	if m := actWantStatus(t, f.invoke("?simulated=true", "k", actDraft), 422); m["error"] != "action 'create_draft' is paused" {
		t.Fatal(m)
	}
	if _, err := actions.SetActionState(f.ctx, db, f.actionID, models.ActionDisabled); err != nil {
		t.Fatal(err)
	}
	if m := actWantStatus(t, f.invoke("?simulated=true", "k", actDraft), 422); m["error"] != "action 'create_draft' is disabled" {
		t.Fatal(m)
	}
	if _, err := actions.SetActionState(f.ctx, db, f.actionID, models.ActionEnabled); err != nil {
		t.Fatal(err)
	}

	if _, err := crud.SetDatasourceReadOnly(f.ctx, db, f.dsID, true); err != nil {
		t.Fatal(err)
	}
	if m := actWantStatus(t, f.invoke("?simulated=true", "k", actDraft), 403); m["error"] != "action 'create_draft' blocked: datasource is read-only" {
		t.Fatal(m)
	}
	if _, err := crud.SetDatasourceReadOnly(f.ctx, db, f.dsID, false); err != nil {
		t.Fatal(err)
	}

	one, period := int64(1), int64(3600)
	if _, err := actions.UpdateAgentActionRateLimit(f.ctx, db, f.bindID, &one, &period); err != nil {
		t.Fatal(err)
	}
	actWantStatus(t, f.invoke("?simulated=true", "r1", actDraft), 200)
	m := actWantStatus(t, f.invoke("?simulated=true", "r2", actDraft), 429)
	if !strings.HasPrefix(m["error"].(string), "rate limit exceeded; retry after ") || m["status"].(float64) != 429 {
		t.Fatal(m)
	}
}

func TestActApprovalFlow(t *testing.T) {
	f := newActFixture(t, models.PolicyAlwaysApprove)
	db := f.e.srv.db
	if _, err := actions.UpdateAgentActionPinnedParams(f.ctx, db, f.bindID, map[string]any{"to": "pinned@x.com"}); err != nil {
		t.Fatal(err)
	}

	m := actWantStatus(t, f.invoke("", "a1", actDraft), 202)
	approvalID := int64(m["approval_id"].(float64))
	if m["status"] != "pending_approval" || m["poll_url"] != fmt.Sprintf("/act/%d/approvals/%d", f.agentID, approvalID) ||
		m["expires_at"] == nil || m["metadata"].(map[string]any)["replayed"] != false {
		t.Fatalf("body = %v", m)
	}
	ap, err := actions.GetApproval(f.ctx, db, approvalID)
	if err != nil || ap == nil {
		t.Fatal(err)
	}
	if ap.Params.(map[string]any)["to"] != "pinned@x.com" {
		t.Fatalf("pinned params not applied: %v", ap.Params)
	}

	// Resubmit: deduped onto the same approval.
	m = actWantStatus(t, f.invoke("", "a1", actDraft), 202)
	if int64(m["approval_id"].(float64)) != approvalID {
		t.Fatalf("dedupe failed: %v", m)
	}
	// The agent can't override pinned keys, so changing "to" is the same body.
	alt := map[string]any{"to": "evil@x.com", "subject": "hi", "body": "hello"}
	actWantStatus(t, f.invoke("", "a1", alt), 202)
	// A genuinely different body conflicts.
	actWantStatus(t, f.invoke("", "a1", map[string]any{"to": "a", "subject": "other", "body": "b"}), 409)

	// Poll.
	poll := func(id int64) *httptest.ResponseRecorder {
		return f.e.do("GET", fmt.Sprintf("/act/%d/approvals/%d", f.agentID, id), f.token, nil)
	}
	m = actWantStatus(t, poll(approvalID), 200)
	if m["state"] != "pending" || m["simulated"] != false || m["result"] != nil || m["resolved_by"] != nil {
		t.Fatalf("poll = %v", m)
	}
	if _, ok := m["resolved_at"]; !ok {
		t.Fatal("resolved_at must be present (null)")
	}

	by := "ops"
	if _, err := actions.ResolveApproval(f.ctx, db, approvalID, models.ApprovalExecuted, map[string]any{"ok": true}, &by); err != nil {
		t.Fatal(err)
	}
	m = actWantStatus(t, f.invoke("", "a1", actDraft), 200)
	if m["result"].(map[string]any)["ok"] != true || m["metadata"].(map[string]any)["replayed"] != true {
		t.Fatalf("executed = %v", m)
	}
	m = actWantStatus(t, poll(approvalID), 200)
	if m["state"] != "executed" || m["resolved_by"] != "ops" {
		t.Fatalf("poll = %v", m)
	}

	terminal := []struct {
		state models.ApprovalState
		code  int
		err   string
	}{
		{models.ApprovalDenied, 403, "approval denied"},
		{models.ApprovalExpired, 410, "approval expired; submit a new request with a fresh Idempotency-Key"},
		{models.ApprovalFailed, 502, "action execution failed after approval"},
		{models.ApprovalApproved, 202, ""},
	}
	for i, c := range terminal {
		key := fmt.Sprintf("t%d", i)
		m := actWantStatus(t, f.invoke("?simulated=true", key, actDraft), 202)
		id := int64(m["approval_id"].(float64))
		if _, err := actions.ResolveApproval(f.ctx, db, id, c.state, nil, &by); err != nil {
			t.Fatal(err)
		}
		m = actWantStatus(t, f.invoke("?simulated=true", key, actDraft), c.code)
		if c.err != "" && (m["error"] != c.err || int64(m["approval_id"].(float64)) != id) {
			t.Fatalf("%s: %v", c.state, m)
		}
	}
}

func TestActGetApprovalErrors(t *testing.T) {
	f := newActFixture(t, models.PolicyAlwaysApprove)
	m := actWantStatus(t, f.invoke("", "a1", actDraft), 202)
	id := int64(m["approval_id"].(float64))

	// Another agent can't see it.
	other, err := crud.CreateAgent(f.ctx, f.e.srv.db, "other", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := crud.CreateAgentToken(f.ctx, f.e.srv.db, other, util.HashToken("other-token"), "t"); err != nil {
		t.Fatal(err)
	}
	rec := f.e.do("GET", fmt.Sprintf("/act/%d/approvals/%d", other, id), "other-token", nil)
	if m := actWantStatus(t, rec, 404); m["error"] != "approval not found" {
		t.Fatal(m)
	}
	actWantStatus(t, f.e.do("GET", fmt.Sprintf("/act/%d/approvals/9999", f.agentID), f.token, nil), 404)
	actWantStatus(t, f.e.do("GET", fmt.Sprintf("/act/%d/approvals/%d", other, id), f.token, nil), 401)
	actWantStatus(t, f.e.do("GET", fmt.Sprintf("/act/%d/approvals/%d", f.agentID, id), "", nil), 401)
	actWantStatus(t, f.e.do("GET", fmt.Sprintf("/act/%d/approvals/x", f.agentID), f.token, nil), 400)
	actWantStatus(t, f.e.do("GET", "/act/x/approvals/1", f.token, nil), 400)
	if rec := f.e.do("GET", fmt.Sprintf("/act/%d/approvals/%d", f.agentID, id), f.token, nil); rec.Code != http.StatusOK {
		t.Fatal(rec.Body.String())
	}
}
