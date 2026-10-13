package actions

import (
	"context"
	"database/sql"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/db"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/util"
)

func setupDB(t *testing.T) (context.Context, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	pool, err := db.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	return ctx, pool
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func ptr[T any](v T) *T { return &v }

func obj(s string) any {
	v, err := util.DecodeJSON([]byte(s))
	if err != nil {
		panic(err)
	}
	return v
}

func jsonEq(t *testing.T, a, b any) {
	t.Helper()
	if util.JSONString(a) != util.JSONString(b) {
		t.Fatalf("json mismatch:\n%s\n%s", util.JSONString(a), util.JSONString(b))
	}
}

func setupAction(t *testing.T, ctx context.Context, pool *sql.DB) (int64, int64) {
	t.Helper()
	dsID := must[int64](t)(crud.CreateDatasource(ctx, pool, "gmail", "gmail", "{}", nil))
	actionID := must[int64](t)(CreateAction(ctx, pool, dsID, "Create Draft", "create_draft", ptr("Compose a Gmail draft"),
		obj(`{"type": "object", "properties": {"to": {"type": "string"}}}`),
		obj(`{"type": "object", "properties": {"draft_id": {"type": "string"}}}`),
		models.PolicyAuto, obj(`{}`), ptr(int64(86400))))
	return dsID, actionID
}

func newAgent(t *testing.T, ctx context.Context, pool *sql.DB) int64 {
	return must[int64](t)(crud.CreateAgent(ctx, pool, "bot", nil))
}

func TestActionCRUD(t *testing.T) {
	ctx, pool := setupDB(t)
	dsID, actionID := setupAction(t, ctx, pool)

	list := must[[]models.Action](t)(ListActions(ctx, pool, &dsID))
	if len(list) != 1 || list[0].ActionType != "create_draft" || list[0].State != models.ActionEnabled ||
		list[0].PolicyKind != models.PolicyAuto || *list[0].ApprovalTTLSeconds != 86400 {
		t.Fatalf("%+v", list)
	}
	if all := must[[]models.Action](t)(ListActions(ctx, pool, nil)); len(all) != 1 {
		t.Fatal(len(all))
	}
	one := must[*models.Action](t)(GetAction(ctx, pool, actionID))
	if one.Name != "Create Draft" {
		t.Fatal(one.Name)
	}
	if one.ParamsSchema["properties"].(map[string]any)["to"].(map[string]any)["type"] != "string" {
		t.Fatalf("%+v", one.ParamsSchema)
	}
	if byType := must[*models.Action](t)(GetActionByType(ctx, pool, dsID, "create_draft")); byType.ID != actionID {
		t.Fatal(byType.ID)
	}
	if missing := must[*models.Action](t)(GetActionByType(ctx, pool, dsID, "nope")); missing != nil {
		t.Fatal("expected nil")
	}

	if !must[bool](t)(UpdateAction(ctx, pool, actionID, "Draft Email", ptr("Updated"), obj(`{"type":"object"}`), obj(`{"type":"object"}`),
		models.PolicyAlwaysApprove, obj(`{}`), ptr(int64(3600)))) {
		t.Fatal("update")
	}
	after := must[*models.Action](t)(GetAction(ctx, pool, actionID))
	if after.Name != "Draft Email" || after.PolicyKind != models.PolicyAlwaysApprove || *after.ApprovalTTLSeconds != 3600 {
		t.Fatalf("%+v", after)
	}
	must[bool](t)(SetActionState(ctx, pool, actionID, models.ActionPaused))
	if after = must[*models.Action](t)(GetAction(ctx, pool, actionID)); after.State != models.ActionPaused {
		t.Fatal(after.State)
	}
	if !must[bool](t)(DeleteAction(ctx, pool, actionID)) {
		t.Fatal("delete")
	}
	if a := must[*models.Action](t)(GetAction(ctx, pool, actionID)); a != nil {
		t.Fatal("still there")
	}
	if ok := must[bool](t)(DeleteAction(ctx, pool, actionID)); ok {
		t.Fatal("deleting a missing action reports false")
	}
}

func TestActionRowFallbacks(t *testing.T) {
	ctx, pool := setupDB(t)
	_, id := setupAction(t, ctx, pool)
	if _, err := pool.ExecContext(ctx, "UPDATE actions SET state='x', policy_kind='x', risk_level='x', required_scopes='x', params_schema='x', threshold_config='[1]' WHERE id=?", id); err != nil {
		t.Fatal(err)
	}
	a := must[*models.Action](t)(GetAction(ctx, pool, id))
	if a.State != models.ActionEnabled || a.PolicyKind != models.PolicyAuto || a.RiskLevel != models.RiskMedium {
		t.Fatalf("%+v", a)
	}
	if a.RequiredScopes == nil || len(a.RequiredScopes) != 0 || a.ParamsSchema == nil || len(a.ParamsSchema) != 0 || len(a.ThresholdConfig) != 0 {
		t.Fatalf("%+v", a)
	}
}

func TestActionUniquePerDatasource(t *testing.T) {
	ctx, pool := setupDB(t)
	dsID, _ := setupAction(t, ctx, pool)
	_, err := CreateAction(ctx, pool, dsID, "Duplicate", "create_draft", nil, obj(`{}`), obj(`{}`), models.PolicyAuto, obj(`{}`), nil)
	if err == nil {
		t.Fatal("expected UNIQUE violation")
	}
}

func TestActionRiskScopesPredefinedRoundtrip(t *testing.T) {
	ctx, pool := setupDB(t)
	dsID := must[int64](t)(crud.CreateDatasource(ctx, pool, "gmail", "gmail", "{}", nil))
	id := must[int64](t)(CreateActionFull(ctx, pool, dsID, "Send Email", "send_email", ptr("Send an email directly — irreversible"),
		obj(`{"type": "object", "properties": {"to": {"type": "string"}}}`), obj(`{"type": "object"}`),
		models.PolicyAlwaysApprove, obj(`{}`), ptr(int64(86400)), models.RiskHigh, []string{"gmail.send"}, true))
	a := must[*models.Action](t)(GetAction(ctx, pool, id))
	if a.RiskLevel != models.RiskHigh || !slices.Equal(a.RequiredScopes, []string{"gmail.send"}) || !a.Predefined {
		t.Fatalf("%+v", a)
	}
	list := must[[]models.Action](t)(ListActions(ctx, pool, &dsID))
	if list[0].RiskLevel != models.RiskHigh || !list[0].Predefined {
		t.Fatalf("%+v", list[0])
	}
	if bt := must[*models.Action](t)(GetActionByType(ctx, pool, dsID, "send_email")); !slices.Equal(bt.RequiredScopes, []string{"gmail.send"}) {
		t.Fatal(bt.RequiredScopes)
	}
	defID := must[int64](t)(CreateAction(ctx, pool, dsID, "Create Draft", "create_draft", nil, obj(`{}`), obj(`{}`), models.PolicyAuto, obj(`{}`), nil))
	d := must[*models.Action](t)(GetAction(ctx, pool, defID))
	if d.RiskLevel != models.RiskMedium || len(d.RequiredScopes) != 0 || d.RequiredScopes == nil || d.Predefined {
		t.Fatalf("%+v", d)
	}
	if d.ApprovalTTLSeconds != nil {
		t.Fatal("explicit NULL ttl must stay NULL")
	}
}

func TestSeedPredefinedActions(t *testing.T) {
	ctx, pool := setupDB(t)
	dsID := must[int64](t)(crud.CreateDatasource(ctx, pool, "gmail", "gmail", "{}", nil))
	if n := must[int64](t)(SeedPredefinedActions(ctx, pool, dsID, models.DatasourceGmail)); n != 10 {
		t.Fatal(n)
	}
	list := must[[]models.Action](t)(ListActions(ctx, pool, &dsID))
	if len(list) != 10 {
		t.Fatal(len(list))
	}
	for _, a := range list {
		if !a.Predefined || a.State != models.ActionDisabled || *a.ApprovalTTLSeconds != 86400 {
			t.Fatalf("%+v", a)
		}
	}
	find := func(at string) models.Action {
		i := slices.IndexFunc(list, func(a models.Action) bool { return a.ActionType == at })
		return list[i]
	}
	draft := find("create_draft")
	if draft.RiskLevel != models.RiskLow || !slices.Equal(draft.RequiredScopes, []string{"gmail.compose"}) || draft.PolicyKind != models.PolicyAuto {
		t.Fatalf("%+v", draft)
	}
	send := find("send_email")
	if send.RiskLevel != models.RiskHigh || send.PolicyKind != models.PolicyAlwaysApprove {
		t.Fatalf("%+v", send)
	}
	if n := must[int64](t)(SeedPredefinedActions(ctx, pool, dsID, models.DatasourceGmail)); n != 0 {
		t.Fatal("seeding must be idempotent")
	}

	ds2 := must[int64](t)(crud.CreateDatasource(ctx, pool, "gmail2", "gmail", "{}", nil))
	must[int64](t)(CreateAction(ctx, pool, ds2, "My Draft", "create_draft", nil, obj(`{}`), obj(`{}`), models.PolicyAuto, obj(`{}`), nil))
	if n := must[int64](t)(SeedPredefinedActions(ctx, pool, ds2, models.DatasourceGmail)); n != 9 {
		t.Fatal(n)
	}
	manual := must[*models.Action](t)(GetActionByType(ctx, pool, ds2, "create_draft"))
	if manual.Name != "My Draft" || manual.Predefined {
		t.Fatalf("%+v", manual)
	}
}

func TestBackfillPredefinedActions(t *testing.T) {
	ctx, pool := setupDB(t)
	must[int64](t)(crud.CreateDatasource(ctx, pool, "gh", "github", "{}", nil))
	must[int64](t)(crud.CreateDatasource(ctx, pool, "hs", "hubspot", "{}", nil))
	must[int64](t)(crud.CreateDatasource(ctx, pool, "pg", "postgres", "{}", nil))
	if n := must[int64](t)(BackfillPredefinedActions(ctx, pool)); n != 12+4 {
		t.Fatal(n)
	}
	if n := must[int64](t)(BackfillPredefinedActions(ctx, pool)); n != 0 {
		t.Fatal(n)
	}
	// Freshly seeded rows already match the catalog.
	if n := must[int64](t)(SyncPredefinedActionSchemas(ctx, pool)); n != 0 {
		t.Fatal(n)
	}
}

func TestSyncPredefinedActionSchemasUpdatesDriftedRows(t *testing.T) {
	ctx, pool := setupDB(t)
	dsID := must[int64](t)(crud.CreateDatasource(ctx, pool, "linear", "linear", "{}", nil))
	oldParams := obj(`{"type":"object","required":["team_id","title"],"properties":{"team_id":{"type":"string"},"title":{"type":"string"}}}`)
	must[int64](t)(CreateActionFull(ctx, pool, dsID, "Create Issue", "create_issue", ptr("Create a new Linear issue."),
		oldParams, obj(`{"type":"object"}`), models.PolicyAuto, obj(`{}`), ptr(int64(86400)), models.RiskLow, []string{"write"}, true))
	// A custom (non-predefined) row is never touched.
	must[int64](t)(CreateAction(ctx, pool, dsID, "Custom", "update_issue", nil, obj(`{}`), obj(`{}`), models.PolicyAuto, obj(`{}`), nil))

	if n := must[int64](t)(SyncPredefinedActionSchemas(ctx, pool)); n != 1 {
		t.Fatalf("the drifted create_issue row should be updated, got %d", n)
	}
	ci := must[*models.Action](t)(GetActionByType(ctx, pool, dsID, "create_issue"))
	if _, ok := ci.ParamsSchema["properties"].(map[string]any)["project_id"]; !ok {
		t.Fatal("create_issue should now expose the optional project_id")
	}
	if custom := must[*models.Action](t)(GetActionByType(ctx, pool, dsID, "update_issue")); len(custom.ParamsSchema) != 0 {
		t.Fatal("custom row must not be synced")
	}
	if n := must[int64](t)(SyncPredefinedActionSchemas(ctx, pool)); n != 0 {
		t.Fatal("sync must be idempotent")
	}
}

func TestDeletePredefinedActionBlocked(t *testing.T) {
	ctx, pool := setupDB(t)
	dsID := must[int64](t)(crud.CreateDatasource(ctx, pool, "gmail", "gmail", "{}", nil))
	must[int64](t)(SeedPredefinedActions(ctx, pool, dsID, models.DatasourceGmail))
	list := must[[]models.Action](t)(ListActions(ctx, pool, &dsID))
	pid := list[0].ID
	if _, err := DeleteAction(ctx, pool, pid); !errors.Is(err, ErrPredefined) {
		t.Fatal(err)
	}
	if ErrPredefined.Error() != "predefined actions cannot be deleted" {
		t.Fatal(ErrPredefined.Error())
	}
	if a := must[*models.Action](t)(GetAction(ctx, pool, pid)); a == nil {
		t.Fatal("predefined action must survive")
	}
	cid := must[int64](t)(CreateAction(ctx, pool, dsID, "Custom", "custom_thing", nil, obj(`{}`), obj(`{}`), models.PolicyAuto, obj(`{}`), nil))
	if !must[bool](t)(DeleteAction(ctx, pool, cid)) {
		t.Fatal("custom delete")
	}
}

func TestAgentActionBindings(t *testing.T) {
	ctx, pool := setupDB(t)
	_, actionID := setupAction(t, ctx, pool)
	agent := newAgent(t, ctx, pool)

	bid := must[int64](t)(BindAgentAction(ctx, pool, agent, actionID, "send", nil))
	list := must[[]models.AgentAction](t)(ListAgentActions(ctx, pool, agent))
	if len(list) != 1 || list[0].Alias != "send" || list[0].RateLimitCount != nil || list[0].RateLimitPeriodSeconds != nil ||
		list[0].Template != nil || list[0].PinnedParams != nil {
		t.Fatalf("%+v", list)
	}
	b, a := must2(t)(ResolveActionAlias(ctx, pool, agent, "send"))
	if b == nil || b.ID != bid || a.ID != actionID || a.ActionType != "create_draft" {
		t.Fatalf("%+v %+v", b, a)
	}
	if b, a := must2(t)(ResolveActionAlias(ctx, pool, agent, "nope")); b != nil || a != nil {
		t.Fatal("expected nil")
	}

	must[bool](t)(UpdateAgentActionAlias(ctx, pool, bid, "draft"))
	must[bool](t)(UpdateAgentActionRateLimit(ctx, pool, bid, ptr(int64(3)), ptr(int64(3600))))
	must[bool](t)(UpdateAgentActionTemplate(ctx, pool, bid, ptr("Hello {{name}}")))
	must[bool](t)(UpdateAgentActionPinnedParams(ctx, pool, bid, obj(`{"to":"x@y.z"}`)))
	list = must[[]models.AgentAction](t)(ListAgentActions(ctx, pool, agent))
	if list[0].Alias != "draft" || *list[0].RateLimitCount != 3 || *list[0].RateLimitPeriodSeconds != 3600 || *list[0].Template != "Hello {{name}}" {
		t.Fatalf("%+v", list[0])
	}
	jsonEq(t, list[0].PinnedParams, obj(`{"to":"x@y.z"}`))
	b, _ = must2(t)(ResolveActionAlias(ctx, pool, agent, "draft"))
	jsonEq(t, b.PinnedParams, obj(`{"to":"x@y.z"}`))
	if *b.Template != "Hello {{name}}" {
		t.Fatal(*b.Template)
	}

	must[bool](t)(UpdateAgentActionTemplate(ctx, pool, bid, nil))
	must[bool](t)(UpdateAgentActionPinnedParams(ctx, pool, bid, nil))
	list = must[[]models.AgentAction](t)(ListAgentActions(ctx, pool, agent))
	if list[0].Template != nil || list[0].PinnedParams != nil {
		t.Fatalf("%+v", list[0])
	}

	if !must[bool](t)(UnbindAgentAction(ctx, pool, agent, actionID)) {
		t.Fatal("unbind")
	}
	if l := must[[]models.AgentAction](t)(ListAgentActions(ctx, pool, agent)); len(l) != 0 {
		t.Fatal("not empty")
	}
}

func must2(t *testing.T) func(*models.AgentAction, *models.Action, error) (*models.AgentAction, *models.Action) {
	return func(b *models.AgentAction, a *models.Action, err error) (*models.AgentAction, *models.Action) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return b, a
	}
}

func TestActionApprovalQueue(t *testing.T) {
	ctx, pool := setupDB(t)
	_, actionID := setupAction(t, ctx, pool)
	agent := newAgent(t, ctx, pool)

	id := must[int64](t)(CreateApproval(ctx, pool, agent, actionID, nil, ptr("req-1"), obj(`{"to":"a@b.c"}`), "abc123", false, nil))
	pending := must[[]models.ActionApproval](t)(ListPendingApprovals(ctx, pool, &agent))
	if len(pending) != 1 || pending[0].ID != id || pending[0].State != models.ApprovalPending {
		t.Fatalf("%+v", pending)
	}
	if pending[0].Params.(map[string]any)["to"] != "a@b.c" || *pending[0].IdempotencyKey != "req-1" || pending[0].Result != nil {
		t.Fatalf("%+v", pending[0])
	}

	if !must[bool](t)(ResolveApproval(ctx, pool, id, models.ApprovalApproved, obj(`{"draft_id":"DRAFT-1"}`), ptr("ivan"))) {
		t.Fatal("resolve")
	}
	if must[bool](t)(ResolveApproval(ctx, pool, id, models.ApprovalDenied, nil, ptr("ivan"))) {
		t.Fatal("resolving an already-resolved approval must no-op")
	}
	row := must[*models.ActionApproval](t)(GetApproval(ctx, pool, id))
	if row.State != models.ApprovalApproved || *row.ResolvedBy != "ivan" || row.ResolvedAt == nil {
		t.Fatalf("%+v", row)
	}
	if row.Result.(map[string]any)["draft_id"] != "DRAFT-1" {
		t.Fatalf("%+v", row.Result)
	}
	if p := must[[]models.ActionApproval](t)(ListPendingApprovals(ctx, pool, nil)); len(p) != 0 {
		t.Fatal("queue must be empty")
	}
	if a := must[*models.ActionApproval](t)(GetApproval(ctx, pool, 999)); a != nil {
		t.Fatal("expected nil")
	}
}

func TestActionApprovalExpirySweep(t *testing.T) {
	ctx, pool := setupDB(t)
	_, actionID := setupAction(t, ctx, pool)
	agent := newAgent(t, ctx, pool)
	expired := must[int64](t)(CreateApproval(ctx, pool, agent, actionID, nil, nil, obj(`{}`), "h", false, ptr("2000-01-01 00:00:00")))
	durable := must[int64](t)(CreateApproval(ctx, pool, agent, actionID, nil, nil, obj(`{}`), "h2", false, nil))

	if n := must[int64](t)(ExpirePendingApprovals(ctx, pool)); n != 1 {
		t.Fatal(n)
	}
	if r := must[*models.ActionApproval](t)(GetApproval(ctx, pool, durable)); r.State != models.ApprovalPending {
		t.Fatal(r.State)
	}
	if r := must[*models.ActionApproval](t)(GetApproval(ctx, pool, expired)); r.State != models.ApprovalExpired || r.ResolvedAt == nil {
		t.Fatalf("%+v", r)
	}
	if n := must[int64](t)(ExpirePendingApprovals(ctx, pool)); n != 0 {
		t.Fatal(n)
	}
}

func TestSpawnExpirySweeper(t *testing.T) {
	ctx, pool := setupDB(t)
	_, actionID := setupAction(t, ctx, pool)
	agent := newAgent(t, ctx, pool)
	id := must[int64](t)(CreateApproval(ctx, pool, agent, actionID, nil, nil, obj(`{}`), "h", false, ptr("2000-01-01 00:00:00")))

	old := expirySweepInterval
	expirySweepInterval = 10 * time.Millisecond
	defer func() { expirySweepInterval = old }()
	sctx, cancel := context.WithCancel(ctx)
	defer cancel()
	SpawnExpirySweeper(sctx, pool)

	deadline := time.Now().Add(5 * time.Second)
	for {
		r := must[*models.ActionApproval](t)(GetApproval(ctx, pool, id))
		if r.State == models.ApprovalExpired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sweeper did not expire the approval")
		}
		time.Sleep(10 * time.Millisecond)
	}
	// Another past-due row is picked up on a later tick (n > 0 then n == 0 paths).
	id2 := must[int64](t)(CreateApproval(ctx, pool, agent, actionID, nil, nil, obj(`{}`), "h", false, ptr("2000-01-01 00:00:00")))
	for {
		if r := must[*models.ActionApproval](t)(GetApproval(ctx, pool, id2)); r.State == models.ApprovalExpired {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("sweeper stopped ticking")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestActionIdempotencyRoundtrip(t *testing.T) {
	ctx, pool := setupDB(t)
	_, actionID := setupAction(t, ctx, pool)
	agent := newAgent(t, ctx, pool)
	tok := must[int64](t)(crud.CreateAgentToken(ctx, pool, agent, "hash", "t"))

	must[int64](t)(InsertIdempotency(ctx, pool, tok, actionID, "req-1", false, "params-hash", 200, obj(`{"draft_id":"DRAFT-1"}`), 60))
	hit := must[*models.ActionIdempotency](t)(LookupIdempotency(ctx, pool, tok, actionID, "req-1", false))
	if hit == nil || hit.Status != 200 || hit.ParamsHash != "params-hash" || hit.Simulated {
		t.Fatalf("%+v", hit)
	}
	if hit.Result.(map[string]any)["draft_id"] != "DRAFT-1" {
		t.Fatal(hit.Result)
	}
	if miss := must[*models.ActionIdempotency](t)(LookupIdempotency(ctx, pool, tok, actionID, "req-1", true)); miss != nil {
		t.Fatal("simulated namespace must not collide")
	}
	must[int64](t)(InsertIdempotency(ctx, pool, tok, actionID, "req-1", true, "params-hash", 200, obj(`{"draft_id":"SIM-1"}`), 60))
	sim := must[*models.ActionIdempotency](t)(LookupIdempotency(ctx, pool, tok, actionID, "req-1", true))
	if !sim.Simulated || sim.Result.(map[string]any)["draft_id"] != "SIM-1" {
		t.Fatalf("%+v", sim)
	}
	real := must[*models.ActionIdempotency](t)(LookupIdempotency(ctx, pool, tok, actionID, "req-1", false))
	if real.Result.(map[string]any)["draft_id"] != "DRAFT-1" {
		t.Fatal(real.Result)
	}

	must[int64](t)(InsertIdempotency(ctx, pool, tok, actionID, "req-stale", false, "h", 200, obj(`{}`), -60))
	if stale := must[*models.ActionIdempotency](t)(LookupIdempotency(ctx, pool, tok, actionID, "req-stale", false)); stale != nil {
		t.Fatal("expired rows must not be returned")
	}
	if n := must[int64](t)(PurgeExpiredIdempotency(ctx, pool)); n < 1 {
		t.Fatal(n)
	}
}

func TestTokenForceSimulate(t *testing.T) {
	ctx, pool := setupDB(t)
	agent := newAgent(t, ctx, pool)
	tok := must[int64](t)(crud.CreateAgentToken(ctx, pool, agent, "hash-1", "prod"))
	if toks := must[[]models.AgentToken](t)(crud.GetAgentTokens(ctx, pool, agent)); toks[0].ForceSimulate {
		t.Fatal("default must be false")
	}
	auth := must[*TokenAuth](t)(ValidateTokenForAction(ctx, pool, "hash-1"))
	if *auth != (TokenAuth{TokenID: tok, AgentID: agent}) {
		t.Fatalf("%+v", auth)
	}
	must[bool](t)(SetTokenForceSimulate(ctx, pool, tok, true))
	if auth = must[*TokenAuth](t)(ValidateTokenForAction(ctx, pool, "hash-1")); !auth.ForceSimulate {
		t.Fatal("force_simulate flag must flip on read")
	}
	if toks := must[[]models.AgentToken](t)(crud.GetAgentTokens(ctx, pool, agent)); !toks[0].ForceSimulate {
		t.Fatal("token list must reflect force_simulate")
	}
	if m := must[*TokenAuth](t)(ValidateTokenForAction(ctx, pool, "no-such-token")); m != nil {
		t.Fatal("expected nil")
	}
}

// Exercises the /act/* pipeline at the DB layer: auth → allowlist →
// idempotency → (stubbed) invoke → cache → replay.
func TestActPipelineSmoke(t *testing.T) {
	ctx, pool := setupDB(t)
	_, actionID := setupAction(t, ctx, pool)
	agent := newAgent(t, ctx, pool)
	token := "ds_test_token"
	tok := must[int64](t)(crud.CreateAgentToken(ctx, pool, agent, util.HashToken(token), "t"))
	must[int64](t)(BindAgentAction(ctx, pool, agent, actionID, "send", nil))

	auth := must[*TokenAuth](t)(ValidateTokenForAction(ctx, pool, util.HashToken(token)))
	if *auth != (TokenAuth{TokenID: tok, AgentID: agent, ForceSimulate: false}) {
		t.Fatalf("%+v", auth)
	}
	binding, action := must2(t)(ResolveActionAlias(ctx, pool, agent, "send"))
	if binding.AgentID != agent || action.State != models.ActionEnabled {
		t.Fatalf("%+v %+v", binding, action)
	}
	key := "req-abc"
	params := obj(`{"to": "a@b.c", "subject": "hi", "body": "hello"}`)
	hash := util.HashCanonicalJSON(params)
	if m := must[*models.ActionIdempotency](t)(LookupIdempotency(ctx, pool, tok, action.ID, key, false)); m != nil {
		t.Fatal("expected miss")
	}
	resp := obj(`{"draft_id": "r-1234567", "message_id": "18bfc...", "thread_id": "18bfc..."}`)
	must[int64](t)(InsertIdempotency(ctx, pool, tok, action.ID, key, false, hash, 200, resp, 86400))
	replay := must[*models.ActionIdempotency](t)(LookupIdempotency(ctx, pool, tok, action.ID, key, false))
	if replay.ParamsHash != hash || replay.Status != 200 {
		t.Fatalf("%+v", replay)
	}
	jsonEq(t, replay.Result, resp)
	tampered := util.HashCanonicalJSON(obj(`{"to": "x@y.z", "subject": "hi", "body": "hello"}`))
	if replay.ParamsHash == tampered {
		t.Fatal("hash mismatch is what the /act handler uses to return 409")
	}
	if m := must[*models.ActionIdempotency](t)(LookupIdempotency(ctx, pool, tok, action.ID, key, true)); m != nil {
		t.Fatal("simulated namespace must not see real-side cache")
	}
}

func TestActPipelineSimulatedSmoke(t *testing.T) {
	ctx, pool := setupDB(t)
	_, actionID := setupAction(t, ctx, pool)
	agent := newAgent(t, ctx, pool)
	token := "ds_staging_token"
	tok := must[int64](t)(crud.CreateAgentToken(ctx, pool, agent, util.HashToken(token), "staging"))
	must[int64](t)(BindAgentAction(ctx, pool, agent, actionID, "send", nil))
	must[bool](t)(SetTokenForceSimulate(ctx, pool, tok, true))

	if auth := must[*TokenAuth](t)(ValidateTokenForAction(ctx, pool, util.HashToken(token))); !auth.ForceSimulate {
		t.Fatal("force_simulate token must declare itself as simulated")
	}
	key := "req-sim-1"
	hash := util.HashCanonicalJSON(obj(`{"to": "a@b.c", "subject": "hi", "body": "hello"}`))
	if m := must[*models.ActionIdempotency](t)(LookupIdempotency(ctx, pool, tok, actionID, key, true)); m != nil {
		t.Fatal("expected empty simulated cache")
	}
	sim := obj(`{"draft_id": "r-SIM-abc12345", "message_id": "abc12345@sim.gmail", "thread_id": "abc12345@sim.gmail"}`)
	must[int64](t)(InsertIdempotency(ctx, pool, tok, actionID, key, true, hash, 200, sim, 86400))
	replay := must[*models.ActionIdempotency](t)(LookupIdempotency(ctx, pool, tok, actionID, key, true))
	jsonEq(t, replay.Result, sim)
	if m := must[*models.ActionIdempotency](t)(LookupIdempotency(ctx, pool, tok, actionID, key, false)); m != nil {
		t.Fatal("simulated insert must never leak into the real-side cache")
	}
}

func TestPolicyEvaluatorRoutesByKind(t *testing.T) {
	ctx, pool := setupDB(t)
	dsID := must[int64](t)(crud.CreateDatasource(ctx, pool, "gmail", "gmail", "{}", nil))
	mk := func(name, at string, pk models.PolicyKind) *models.Action {
		id := must[int64](t)(CreateAction(ctx, pool, dsID, name, at, nil, obj(`{}`), obj(`{}`), pk, obj(`{}`), ptr(int64(86400))))
		return must[*models.Action](t)(GetAction(ctx, pool, id))
	}
	if d := EvaluatePolicy(mk("Auto", "a_auto", models.PolicyAuto), obj(`{}`)); d != PolicyDirect {
		t.Fatal(d)
	}
	if d := EvaluatePolicy(mk("Always", "a_always", models.PolicyAlwaysApprove), obj(`{}`)); d != PolicyApproval {
		t.Fatal(d)
	}
	if d := EvaluatePolicy(mk("Threshold", "a_thresh", models.PolicyThreshold), obj(`{}`)); d != PolicyApproval {
		t.Fatal(d)
	}
	if PolicyDirect.String() != "direct" || PolicyApproval.String() != "approval" {
		t.Fatal("String()")
	}
}

func TestApprovalDedupeByIdempotencyKey(t *testing.T) {
	ctx, pool := setupDB(t)
	_, actionID := setupAction(t, ctx, pool)
	agent := newAgent(t, ctx, pool)
	tok := must[int64](t)(crud.CreateAgentToken(ctx, pool, agent, "h", "t"))
	params := obj(`{"to": "a@b.c", "subject": "x", "body": "y"}`)
	hash := util.HashCanonicalJSON(params)

	id := must[int64](t)(CreateApprovalWithTTL(ctx, pool, agent, actionID, &tok, ptr("req-1"), params, hash, false, ptr(int64(3600))))
	found := must[*models.ActionApproval](t)(FindApprovalByIdempotency(ctx, pool, agent, actionID, "req-1"))
	if found == nil || found.ID != id || found.State != models.ApprovalPending || found.ExpiresAt == nil || *found.TokenID != tok {
		t.Fatalf("%+v", found)
	}
	if n := must[*models.ActionApproval](t)(FindApprovalByIdempotency(ctx, pool, agent, actionID, "req-2")); n != nil {
		t.Fatal("expected nil")
	}
	// Newest row wins.
	id2 := must[int64](t)(CreateApprovalWithTTL(ctx, pool, agent, actionID, &tok, ptr("req-1"), params, hash, false, ptr(int64(3600))))
	if f := must[*models.ActionApproval](t)(FindApprovalByIdempotency(ctx, pool, agent, actionID, "req-1")); f.ID != id2 {
		t.Fatal(f.ID)
	}
	durable := must[int64](t)(CreateApprovalWithTTL(ctx, pool, agent, actionID, &tok, ptr("req-durable"), params, hash, false, nil))
	if d := must[*models.ActionApproval](t)(GetApproval(ctx, pool, durable)); d.ExpiresAt != nil {
		t.Fatal("TTL=nil must leave expires_at NULL")
	}
}

func TestApprovalToExecutionPopulatesIdempotencyCache(t *testing.T) {
	ctx, pool := setupDB(t)
	_, actionID := setupAction(t, ctx, pool)
	agent := newAgent(t, ctx, pool)
	tok := must[int64](t)(crud.CreateAgentToken(ctx, pool, agent, "h", "t"))
	key := "req-approve-1"
	params := obj(`{"to": "a@b.c", "subject": "x", "body": "y"}`)
	hash := util.HashCanonicalJSON(params)

	aid := must[int64](t)(CreateApprovalWithTTL(ctx, pool, agent, actionID, &tok, &key, params, hash, false, ptr(int64(86400))))
	result := obj(`{"draft_id": "r-real-1"}`)
	if !must[bool](t)(ResolveApproval(ctx, pool, aid, models.ApprovalExecuted, result, ptr("ivan"))) {
		t.Fatal("resolve")
	}
	must[int64](t)(InsertIdempotency(ctx, pool, tok, actionID, key, false, hash, 200, result, 86400))
	cached := must[*models.ActionIdempotency](t)(LookupIdempotency(ctx, pool, tok, actionID, key, false))
	jsonEq(t, cached.Result, result)
	final := must[*models.ActionApproval](t)(GetApproval(ctx, pool, aid))
	if final.State != models.ApprovalExecuted || *final.ResolvedBy != "ivan" {
		t.Fatalf("%+v", final)
	}
	jsonEq(t, final.Result, result)
	if must[bool](t)(ResolveApproval(ctx, pool, aid, models.ApprovalDenied, nil, ptr("ivan"))) {
		t.Fatal("already-resolved approval must not be re-resolved")
	}
}

func TestSimulatedApprovalUsesSimulatedCacheNamespace(t *testing.T) {
	ctx, pool := setupDB(t)
	_, actionID := setupAction(t, ctx, pool)
	agent := newAgent(t, ctx, pool)
	tok := must[int64](t)(crud.CreateAgentToken(ctx, pool, agent, "h", "t"))
	key := "req-sim-approval"
	params := obj(`{"to": "a@b.c", "subject": "x", "body": "y"}`)
	hash := util.HashCanonicalJSON(params)
	sim := obj(`{"draft_id": "r-SIM-xxxxxxxx"}`)

	aid := must[int64](t)(CreateApprovalWithTTL(ctx, pool, agent, actionID, &tok, &key, params, hash, true, ptr(int64(86400))))
	if a := must[*models.ActionApproval](t)(GetApproval(ctx, pool, aid)); !a.Simulated {
		t.Fatal("simulated flag lost")
	}
	must[bool](t)(ResolveApproval(ctx, pool, aid, models.ApprovalExecuted, sim, ptr("ivan")))
	must[int64](t)(InsertIdempotency(ctx, pool, tok, actionID, key, true, hash, 200, sim, 86400))
	if m := must[*models.ActionIdempotency](t)(LookupIdempotency(ctx, pool, tok, actionID, key, false)); m != nil {
		t.Fatal("real-side cache must remain empty")
	}
	if m := must[*models.ActionIdempotency](t)(LookupIdempotency(ctx, pool, tok, actionID, key, true)); m == nil {
		t.Fatal("simulated cache must hit")
	}
}

func TestClosedDBErrors(t *testing.T) {
	ctx, pool := setupDB(t)
	pool.Close()
	if _, err := ListActions(ctx, pool, nil); err == nil {
		t.Fatal("expected error")
	}
	if _, err := GetAction(ctx, pool, 1); err == nil {
		t.Fatal("expected error")
	}
	if _, err := DeleteAction(ctx, pool, 1); err == nil {
		t.Fatal("expected error")
	}
	if _, err := SeedPredefinedActions(ctx, pool, 1, models.DatasourceGmail); err == nil {
		t.Fatal("expected error")
	}
	if _, err := BackfillPredefinedActions(ctx, pool); err == nil {
		t.Fatal("expected error")
	}
	if _, err := SyncPredefinedActionSchemas(ctx, pool); err == nil {
		t.Fatal("expected error")
	}
	if _, err := ListAgentActions(ctx, pool, 1); err == nil {
		t.Fatal("expected error")
	}
	if _, _, err := ResolveActionAlias(ctx, pool, 1, "x"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := ListPendingApprovals(ctx, pool, nil); err == nil {
		t.Fatal("expected error")
	}
	if _, err := GetApproval(ctx, pool, 1); err == nil {
		t.Fatal("expected error")
	}
	if _, err := LookupIdempotency(ctx, pool, 1, 1, "k", false); err == nil {
		t.Fatal("expected error")
	}
	if _, err := ValidateTokenForAction(ctx, pool, "h"); err == nil {
		t.Fatal("expected error")
	}
	if _, err := ExpirePendingApprovals(ctx, pool); err == nil {
		t.Fatal("expected error")
	}
	// Sweeper logs and keeps going on errors; cancel stops it.
	sctx, cancel := context.WithCancel(ctx)
	SpawnExpirySweeper(sctx, pool)
	cancel()
}
