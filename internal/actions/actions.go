// Package actions holds the SQL accessors for the Actions feature: action
// definitions, agent bindings, the approval queue and the idempotency cache,
// plus the predefined action catalog (catalog.go).
package actions

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"log/slog"
	"strconv"
	"time"

	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/util"
)

// ErrPredefined is returned by DeleteAction for catalog-seeded actions.
var ErrPredefined = errors.New("predefined actions cannot be deleted")

// expirySweepInterval is how often SpawnExpirySweeper runs.
var expirySweepInterval = time.Hour

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
}

func affected(res sql.Result, err error) (bool, error) {
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return n > 0, nil
}

func rowsAffected(res sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

func lastID(res sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func nullInt(n sql.NullInt64) *int64 {
	if !n.Valid {
		return nil
	}
	v := n.Int64
	return &v
}

func nullStr(s sql.NullString) *string {
	if !s.Valid {
		return nil
	}
	v := s.String
	return &v
}

// parseJSONOrEmpty decodes s, falling back to {} on invalid JSON.
func parseJSONOrEmpty(s string) any {
	v, err := util.DecodeJSON([]byte(s))
	if err != nil {
		return map[string]any{}
	}
	return v
}

// parseJSONMapOrEmpty is parseJSONOrEmpty for object-typed fields.
func parseJSONMapOrEmpty(s string) map[string]any {
	if m, ok := parseJSONOrEmpty(s).(map[string]any); ok && m != nil {
		return m
	}
	return map[string]any{}
}

func scopesJSON(scopes []string) string {
	if scopes == nil {
		scopes = []string{}
	}
	return util.JSONString(scopes)
}

// ── Actions ──

const actionColumns = "id, datasource_id, name, action_type, description, params_schema, result_schema, " +
	"state, policy_kind, threshold_config, approval_ttl_seconds, risk_level, required_scopes, " +
	"predefined, created_at, updated_at"

type actionRow struct {
	id, datasourceID                                         int64
	name, actionType                                         string
	description                                              sql.NullString
	paramsSchema, resultSchema, state, policyKind, threshold string
	ttl                                                      sql.NullInt64
	riskLevel, requiredScopes                                string
	predefined                                               int64
	createdAt, updatedAt                                     string
}

func (r *actionRow) dest() []any {
	return []any{&r.id, &r.datasourceID, &r.name, &r.actionType, &r.description, &r.paramsSchema,
		&r.resultSchema, &r.state, &r.policyKind, &r.threshold, &r.ttl, &r.riskLevel, &r.requiredScopes,
		&r.predefined, &r.createdAt, &r.updatedAt}
}

func (r *actionRow) action() models.Action {
	var scopes []string
	if err := json.Unmarshal([]byte(r.requiredScopes), &scopes); err != nil || scopes == nil {
		scopes = []string{}
	}
	state := models.ActionState(r.state)
	switch state {
	case models.ActionEnabled, models.ActionDisabled, models.ActionPaused:
	default:
		state = models.ActionEnabled
	}
	policy := models.PolicyKind(r.policyKind)
	switch policy {
	case models.PolicyAuto, models.PolicyAlwaysApprove, models.PolicyThreshold:
	default:
		policy = models.PolicyAuto
	}
	risk := models.RiskLevel(r.riskLevel)
	switch risk {
	case models.RiskLow, models.RiskMedium, models.RiskHigh, models.RiskCritical:
	default:
		risk = models.RiskMedium
	}
	return models.Action{
		ID:                 r.id,
		DatasourceID:       r.datasourceID,
		Name:               r.name,
		ActionType:         r.actionType,
		Description:        nullStr(r.description),
		ParamsSchema:       parseJSONMapOrEmpty(r.paramsSchema),
		ResultSchema:       parseJSONMapOrEmpty(r.resultSchema),
		State:              state,
		PolicyKind:         policy,
		ThresholdConfig:    parseJSONMapOrEmpty(r.threshold),
		ApprovalTTLSeconds: nullInt(r.ttl),
		RiskLevel:          risk,
		RequiredScopes:     scopes,
		Predefined:         r.predefined != 0,
		CreatedAt:          r.createdAt,
		UpdatedAt:          r.updatedAt,
	}
}

func queryActions(ctx context.Context, db *sql.DB, q string, args ...any) ([]models.Action, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.Action{}
	for rows.Next() {
		var r actionRow
		if err := rows.Scan(r.dest()...); err != nil {
			return nil, err
		}
		out = append(out, r.action())
	}
	return out, rows.Err()
}

func queryAction(ctx context.Context, db *sql.DB, q string, args ...any) (*models.Action, error) {
	var r actionRow
	err := db.QueryRowContext(ctx, q, args...).Scan(r.dest()...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	a := r.action()
	return &a, nil
}

// ListActions lists all actions, or only those of datasourceID when non-nil.
func ListActions(ctx context.Context, db *sql.DB, datasourceID *int64) ([]models.Action, error) {
	if datasourceID != nil {
		return queryActions(ctx, db, "SELECT "+actionColumns+" FROM actions WHERE datasource_id = ? ORDER BY id", *datasourceID)
	}
	return queryActions(ctx, db, "SELECT "+actionColumns+" FROM actions ORDER BY id")
}

func GetAction(ctx context.Context, db *sql.DB, id int64) (*models.Action, error) {
	return queryAction(ctx, db, "SELECT "+actionColumns+" FROM actions WHERE id = ?", id)
}

func GetActionByType(ctx context.Context, db *sql.DB, datasourceID int64, actionType string) (*models.Action, error) {
	return queryAction(ctx, db, "SELECT "+actionColumns+" FROM actions WHERE datasource_id = ? AND action_type = ?",
		datasourceID, actionType)
}

// CreateAction creates a custom action (risk medium, no scopes, not predefined).
// Schemas and thresholdConfig are arbitrary JSON values stored as text.
func CreateAction(ctx context.Context, db *sql.DB, datasourceID int64, name, actionType string, description *string,
	paramsSchema, resultSchema any, policyKind models.PolicyKind, thresholdConfig any, approvalTTLSeconds *int64) (int64, error) {
	return CreateActionFull(ctx, db, datasourceID, name, actionType, description, paramsSchema, resultSchema,
		policyKind, thresholdConfig, approvalTTLSeconds, models.RiskMedium, nil, false)
}

func CreateActionFull(ctx context.Context, db *sql.DB, datasourceID int64, name, actionType string, description *string,
	paramsSchema, resultSchema any, policyKind models.PolicyKind, thresholdConfig any, approvalTTLSeconds *int64,
	riskLevel models.RiskLevel, requiredScopes []string, predefined bool) (int64, error) {
	return lastID(db.ExecContext(ctx,
		"INSERT INTO actions (datasource_id, name, action_type, description, params_schema, "+
			"result_schema, policy_kind, threshold_config, approval_ttl_seconds, risk_level, "+
			"required_scopes, predefined) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		datasourceID, name, actionType, description, util.JSONString(paramsSchema), util.JSONString(resultSchema),
		string(policyKind), util.JSONString(thresholdConfig), approvalTTLSeconds, string(riskLevel),
		scopesJSON(requiredScopes), boolInt(predefined)))
}

func UpdateAction(ctx context.Context, db *sql.DB, id int64, name string, description *string,
	paramsSchema, resultSchema any, policyKind models.PolicyKind, thresholdConfig any, approvalTTLSeconds *int64) (bool, error) {
	return affected(db.ExecContext(ctx,
		"UPDATE actions SET name = ?, description = ?, params_schema = ?, result_schema = ?, "+
			"policy_kind = ?, threshold_config = ?, approval_ttl_seconds = ?, updated_at = datetime('now') WHERE id = ?",
		name, description, util.JSONString(paramsSchema), util.JSONString(resultSchema), string(policyKind),
		util.JSONString(thresholdConfig), approvalTTLSeconds, id))
}

func SetActionState(ctx context.Context, db *sql.DB, id int64, state models.ActionState) (bool, error) {
	return affected(db.ExecContext(ctx,
		"UPDATE actions SET state = ?, updated_at = datetime('now') WHERE id = ?", string(state), id))
}

// DeleteAction deletes a custom action. Returns ErrPredefined for catalog
// actions and (false, nil) when the action does not exist.
func DeleteAction(ctx context.Context, db *sql.DB, id int64) (bool, error) {
	var predefined int64
	err := db.QueryRowContext(ctx, "SELECT predefined FROM actions WHERE id = ?", id).Scan(&predefined)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	if predefined == 1 {
		return false, ErrPredefined
	}
	return affected(db.ExecContext(ctx, "DELETE FROM actions WHERE id = ?", id))
}

// ── Predefined action seeding ──

// SeedPredefinedActions inserts the catalog for dsType as disabled,
// predefined actions. INSERT OR IGNORE never overwrites existing rows
// (including manual ones with the same action_type). Returns rows inserted.
func SeedPredefinedActions(ctx context.Context, db *sql.DB, datasourceID int64, dsType models.DatasourceType) (int64, error) {
	var inserted int64
	for _, e := range PredefinedActions(dsType) {
		n, err := rowsAffected(db.ExecContext(ctx,
			"INSERT OR IGNORE INTO actions (datasource_id, name, action_type, description, "+
				"params_schema, result_schema, state, policy_kind, threshold_config, "+
				"approval_ttl_seconds, risk_level, required_scopes, predefined) "+
				"VALUES (?, ?, ?, ?, ?, ?, 'disabled', ?, '{}', 86400, ?, ?, 1)",
			datasourceID, e.Name, e.ActionType, e.Description, util.JSONString(e.ParamsSchema),
			util.JSONString(e.ResultSchema), string(e.PolicyKind), string(e.RiskLevel), scopesJSON(e.RequiredScopes)))
		if err != nil {
			return inserted, err
		}
		inserted += n
	}
	return inserted, nil
}

// BackfillPredefinedActions seeds predefined actions for every existing
// datasource. Idempotent; run once at startup.
func BackfillPredefinedActions(ctx context.Context, db *sql.DB) (int64, error) {
	dss, err := crud.ListDatasources(ctx, db)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, ds := range dss {
		n, err := SeedPredefinedActions(ctx, db, ds.ID, ds.DsType)
		if err != nil {
			return total, err
		}
		if n > 0 {
			slog.Info("backfilled predefined actions", "count", n, "datasource_id", ds.ID, "datasource", ds.Name)
		}
		total += n
	}
	return total, nil
}

// SyncPredefinedActionSchemas rewrites params_schema/result_schema of
// predefined rows whose schema drifted from the current catalog, leaving
// operator-tuned columns alone. Idempotent (serialization is deterministic,
// sorted keys). Run at startup after BackfillPredefinedActions.
func SyncPredefinedActionSchemas(ctx context.Context, db *sql.DB) (int64, error) {
	dss, err := crud.ListDatasources(ctx, db)
	if err != nil {
		return 0, err
	}
	var updated int64
	for _, ds := range dss {
		for _, e := range PredefinedActions(ds.DsType) {
			params := util.JSONString(e.ParamsSchema)
			result := util.JSONString(e.ResultSchema)
			n, err := rowsAffected(db.ExecContext(ctx,
				"UPDATE actions SET params_schema = ?, result_schema = ?, updated_at = datetime('now') "+
					"WHERE datasource_id = ? AND action_type = ? AND predefined = 1 "+
					"AND (params_schema != ? OR result_schema != ?)",
				params, result, ds.ID, e.ActionType, params, result))
			if err != nil {
				return updated, err
			}
			if n > 0 {
				slog.Info("synced predefined action schema", "action_type", e.ActionType, "datasource_id", ds.ID, "datasource", ds.Name)
			}
			updated += n
		}
	}
	return updated, nil
}

// ── Agent-Action bindings ──

func scanAgentAction(dest *models.AgentAction, count, period *sql.NullInt64, template, pinned *sql.NullString) {
	dest.RateLimitCount = nullInt(*count)
	dest.RateLimitPeriodSeconds = nullInt(*period)
	dest.Template = nullStr(*template)
	if pinned.Valid {
		dest.PinnedParams = parseJSONOrEmpty(pinned.String)
	}
}

func ListAgentActions(ctx context.Context, db *sql.DB, agentID int64) ([]models.AgentAction, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT id, agent_id, action_id, alias, rate_limit_count, rate_limit_period_seconds, template, pinned_params "+
			"FROM agent_actions WHERE agent_id = ? ORDER BY id", agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.AgentAction{}
	for rows.Next() {
		var (
			b                models.AgentAction
			count, period    sql.NullInt64
			template, pinned sql.NullString
		)
		if err := rows.Scan(&b.ID, &b.AgentID, &b.ActionID, &b.Alias, &count, &period, &template, &pinned); err != nil {
			return nil, err
		}
		scanAgentAction(&b, &count, &period, &template, &pinned)
		out = append(out, b)
	}
	return out, rows.Err()
}

func BindAgentAction(ctx context.Context, db *sql.DB, agentID, actionID int64, alias string, template *string) (int64, error) {
	return lastID(db.ExecContext(ctx,
		"INSERT INTO agent_actions (agent_id, action_id, alias, template) VALUES (?, ?, ?, ?)",
		agentID, actionID, alias, template))
}

func UnbindAgentAction(ctx context.Context, db *sql.DB, agentID, actionID int64) (bool, error) {
	return affected(db.ExecContext(ctx, "DELETE FROM agent_actions WHERE agent_id = ? AND action_id = ?", agentID, actionID))
}

// UpdateAgentActionAlias updates a binding's alias by binding id.
func UpdateAgentActionAlias(ctx context.Context, db *sql.DB, id int64, alias string) (bool, error) {
	return affected(db.ExecContext(ctx, "UPDATE agent_actions SET alias = ? WHERE id = ?", alias, id))
}

func UpdateAgentActionRateLimit(ctx context.Context, db *sql.DB, id int64, rateLimitCount, rateLimitPeriodSeconds *int64) (bool, error) {
	return affected(db.ExecContext(ctx,
		"UPDATE agent_actions SET rate_limit_count = ?, rate_limit_period_seconds = ? WHERE id = ?",
		rateLimitCount, rateLimitPeriodSeconds, id))
}

// UpdateAgentActionTemplate sets the per-binding template; nil clears it.
func UpdateAgentActionTemplate(ctx context.Context, db *sql.DB, id int64, template *string) (bool, error) {
	return affected(db.ExecContext(ctx, "UPDATE agent_actions SET template = ? WHERE id = ?", template, id))
}

// UpdateAgentActionPinnedParams sets pinned params (stored as JSON); a nil
// value clears them.
func UpdateAgentActionPinnedParams(ctx context.Context, db *sql.DB, id int64, pinnedParams any) (bool, error) {
	var s *string
	if pinnedParams != nil {
		v := util.JSONString(pinnedParams)
		s = &v
	}
	return affected(db.ExecContext(ctx, "UPDATE agent_actions SET pinned_params = ? WHERE id = ?", s, id))
}

// ResolveActionAlias maps an (agent, alias) pair to its binding and action.
// Both are nil when the alias is not bound.
func ResolveActionAlias(ctx context.Context, db *sql.DB, agentID int64, alias string) (*models.AgentAction, *models.Action, error) {
	var (
		b                models.AgentAction
		count, period    sql.NullInt64
		template, pinned sql.NullString
		r                actionRow
	)
	dest := append([]any{&b.ID, &b.AgentID, &b.ActionID, &b.Alias, &count, &period, &template, &pinned}, r.dest()...)
	err := db.QueryRowContext(ctx,
		"SELECT aa.id, aa.agent_id, aa.action_id, aa.alias, aa.rate_limit_count, "+
			"aa.rate_limit_period_seconds, aa.template, aa.pinned_params, "+
			"a.id, a.datasource_id, a.name, a.action_type, a.description, a.params_schema, "+
			"a.result_schema, a.state, a.policy_kind, a.threshold_config, a.approval_ttl_seconds, "+
			"a.risk_level, a.required_scopes, a.predefined, a.created_at, a.updated_at "+
			"FROM agent_actions aa JOIN actions a ON a.id = aa.action_id "+
			"WHERE aa.agent_id = ? AND aa.alias = ?", agentID, alias).Scan(dest...)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	scanAgentAction(&b, &count, &period, &template, &pinned)
	a := r.action()
	return &b, &a, nil
}

// ── Policy evaluation ──

// PolicyDecision is the outcome of evaluating an action's policy: execute
// directly, or park on the approval queue.
type PolicyDecision int

const (
	PolicyDirect PolicyDecision = iota
	PolicyApproval
)

func (d PolicyDecision) String() string {
	if d == PolicyDirect {
		return "direct"
	}
	return "approval"
}

// EvaluatePolicy maps (action, params) to a decision. Threshold conditions are
// not evaluated yet, so Threshold conservatively queues for approval.
func EvaluatePolicy(action *models.Action, _ any) PolicyDecision {
	switch action.PolicyKind {
	case models.PolicyAuto:
		return PolicyDirect
	default: // AlwaysApprove, Threshold (fail safe)
		return PolicyApproval
	}
}

// ── Approval queue ──

const approvalColumns = "id, agent_id, action_id, token_id, idempotency_key, params, params_hash, simulated, " +
	"state, result, created_at, expires_at, resolved_at, resolved_by"

func scanApproval(sc interface{ Scan(...any) error }) (models.ActionApproval, error) {
	var (
		a                                         models.ActionApproval
		tokenID                                   sql.NullInt64
		idemKey, result, expires, resolved, resBy sql.NullString
		params, state                             string
		simulated                                 int64
	)
	if err := sc.Scan(&a.ID, &a.AgentID, &a.ActionID, &tokenID, &idemKey, &params, &a.ParamsHash, &simulated,
		&state, &result, &a.CreatedAt, &expires, &resolved, &resBy); err != nil {
		return a, err
	}
	a.TokenID = nullInt(tokenID)
	a.IdempotencyKey = nullStr(idemKey)
	a.Params = parseJSONOrEmpty(params)
	a.Simulated = simulated != 0
	a.State = models.ApprovalState(state)
	switch a.State {
	case models.ApprovalPending, models.ApprovalApproved, models.ApprovalDenied, models.ApprovalExpired,
		models.ApprovalExecuted, models.ApprovalFailed:
	default:
		a.State = models.ApprovalPending
	}
	if result.Valid {
		a.Result = parseJSONOrEmpty(result.String)
	}
	a.ExpiresAt = nullStr(expires)
	a.ResolvedAt = nullStr(resolved)
	a.ResolvedBy = nullStr(resBy)
	return a, nil
}

func queryApprovals(ctx context.Context, db *sql.DB, q string, args ...any) ([]models.ActionApproval, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.ActionApproval{}
	for rows.Next() {
		a, err := scanApproval(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func queryApproval(ctx context.Context, db *sql.DB, q string, args ...any) (*models.ActionApproval, error) {
	a, err := scanApproval(db.QueryRowContext(ctx, q, args...))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// CreateApproval queues an approval with an explicit expires_at (nil = never).
func CreateApproval(ctx context.Context, db *sql.DB, agentID, actionID int64, tokenID *int64, idempotencyKey *string,
	params any, paramsHash string, simulated bool, expiresAt *string) (int64, error) {
	return lastID(db.ExecContext(ctx,
		"INSERT INTO action_approvals (agent_id, action_id, token_id, idempotency_key, "+
			"params, params_hash, simulated, expires_at) VALUES (?, ?, ?, ?, ?, ?, ?, ?)",
		agentID, actionID, tokenID, idempotencyKey, util.JSONString(params), paramsHash, boolInt(simulated), expiresAt))
}

// CreateApprovalWithTTL queues an approval whose expires_at is computed in SQL.
// A nil ttlSeconds leaves expires_at NULL (durable until resolved).
func CreateApprovalWithTTL(ctx context.Context, db *sql.DB, agentID, actionID int64, tokenID *int64, idempotencyKey *string,
	params any, paramsHash string, simulated bool, ttlSeconds *int64) (int64, error) {
	if ttlSeconds != nil {
		return lastID(db.ExecContext(ctx,
			"INSERT INTO action_approvals (agent_id, action_id, token_id, idempotency_key, "+
				"params, params_hash, simulated, expires_at) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?, datetime('now', ? || ' seconds'))",
			agentID, actionID, tokenID, idempotencyKey, util.JSONString(params), paramsHash, boolInt(simulated),
			strconv.FormatInt(*ttlSeconds, 10)))
	}
	return lastID(db.ExecContext(ctx,
		"INSERT INTO action_approvals (agent_id, action_id, token_id, idempotency_key, "+
			"params, params_hash, simulated) VALUES (?, ?, ?, ?, ?, ?, ?)",
		agentID, actionID, tokenID, idempotencyKey, util.JSONString(params), paramsHash, boolInt(simulated)))
}

// FindApprovalByIdempotency returns the newest approval for (agent, action,
// key), so retries surface the existing row instead of queueing another.
func FindApprovalByIdempotency(ctx context.Context, db *sql.DB, agentID, actionID int64, idempotencyKey string) (*models.ActionApproval, error) {
	return queryApproval(ctx, db,
		"SELECT "+approvalColumns+" FROM action_approvals "+
			"WHERE agent_id = ? AND action_id = ? AND idempotency_key = ? ORDER BY id DESC LIMIT 1",
		agentID, actionID, idempotencyKey)
}

func GetApproval(ctx context.Context, db *sql.DB, id int64) (*models.ActionApproval, error) {
	return queryApproval(ctx, db, "SELECT "+approvalColumns+" FROM action_approvals WHERE id = ?", id)
}

// ListPendingApprovals lists pending approvals (newest first), optionally for one agent.
func ListPendingApprovals(ctx context.Context, db *sql.DB, agentID *int64) ([]models.ActionApproval, error) {
	if agentID != nil {
		return queryApprovals(ctx, db,
			"SELECT "+approvalColumns+" FROM action_approvals WHERE state = 'pending' AND agent_id = ? ORDER BY created_at DESC",
			*agentID)
	}
	return queryApprovals(ctx, db,
		"SELECT "+approvalColumns+" FROM action_approvals WHERE state = 'pending' ORDER BY created_at DESC")
}

// ResolveApproval moves a pending approval to newState. Returns false when the
// row was already resolved, so callers can surface the race. A nil result
// stores NULL.
func ResolveApproval(ctx context.Context, db *sql.DB, id int64, newState models.ApprovalState, result any, resolvedBy *string) (bool, error) {
	var resultStr *string
	if result != nil {
		s := util.JSONString(result)
		resultStr = &s
	}
	return affected(db.ExecContext(ctx,
		"UPDATE action_approvals SET state = ?, result = ?, resolved_at = datetime('now'), resolved_by = ? "+
			"WHERE id = ? AND state = 'pending'",
		string(newState), resultStr, resolvedBy, id))
}

// ExpirePendingApprovals marks pending approvals past expires_at as expired
// and returns how many flipped.
func ExpirePendingApprovals(ctx context.Context, db *sql.DB) (int64, error) {
	return rowsAffected(db.ExecContext(ctx,
		"UPDATE action_approvals SET state = 'expired', resolved_at = datetime('now') "+
			"WHERE state = 'pending' AND expires_at IS NOT NULL AND expires_at <= datetime('now')"))
}

// SpawnExpirySweeper runs ExpirePendingApprovals now and then hourly until
// ctx is cancelled.
func SpawnExpirySweeper(ctx context.Context, db *sql.DB) {
	interval := expirySweepInterval
	go func() {
		for {
			n, err := ExpirePendingApprovals(ctx, db)
			switch {
			case err != nil:
				if ctx.Err() != nil {
					return
				}
				slog.Warn("approval expiry sweep failed", "error", err)
			case n == 0:
				slog.Debug("approval expiry sweep: nothing to expire")
			default:
				slog.Info("approval expiry sweep: marked approvals as expired", "count", n)
			}
			select {
			case <-ctx.Done():
				return
			case <-time.After(interval):
			}
		}
	}()
}

// ── Idempotency cache ──

// LookupIdempotency returns the unexpired cached response for (token, action,
// key, simulated). Callers compare ParamsHash to choose replay vs 409.
func LookupIdempotency(ctx context.Context, db *sql.DB, tokenID, actionID int64, key string, simulated bool) (*models.ActionIdempotency, error) {
	var (
		r      models.ActionIdempotency
		sim    int64
		result string
	)
	err := db.QueryRowContext(ctx,
		"SELECT id, token_id, action_id, key, simulated, params_hash, status, result, created_at, expires_at "+
			"FROM action_idempotency "+
			"WHERE token_id = ? AND action_id = ? AND key = ? AND simulated = ? AND expires_at > datetime('now')",
		tokenID, actionID, key, boolInt(simulated)).
		Scan(&r.ID, &r.TokenID, &r.ActionID, &r.Key, &sim, &r.ParamsHash, &r.Status, &result, &r.CreatedAt, &r.ExpiresAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Simulated = sim != 0
	r.Result = parseJSONOrEmpty(result)
	return &r, nil
}

// InsertIdempotency caches a response for ttlSeconds (negative = already expired).
func InsertIdempotency(ctx context.Context, db *sql.DB, tokenID, actionID int64, key string, simulated bool,
	paramsHash string, status int64, result any, ttlSeconds int64) (int64, error) {
	return lastID(db.ExecContext(ctx,
		"INSERT INTO action_idempotency (token_id, action_id, key, simulated, params_hash, status, result, expires_at) "+
			"VALUES (?, ?, ?, ?, ?, ?, ?, datetime('now', ? || ' seconds'))",
		tokenID, actionID, key, boolInt(simulated), paramsHash, status, util.JSONString(result),
		strconv.FormatInt(ttlSeconds, 10)))
}

func PurgeExpiredIdempotency(ctx context.Context, db *sql.DB) (int64, error) {
	return rowsAffected(db.ExecContext(ctx, "DELETE FROM action_idempotency WHERE expires_at <= datetime('now')"))
}

// ── Agent token extensions ──

func SetTokenForceSimulate(ctx context.Context, db *sql.DB, id int64, forceSimulate bool) (bool, error) {
	return affected(db.ExecContext(ctx, "UPDATE agent_tokens SET force_simulate = ? WHERE id = ?", boolInt(forceSimulate), id))
}

// TokenAuth is what /act/* needs from a validated agent token.
type TokenAuth struct {
	TokenID       int64
	AgentID       int64
	ForceSimulate bool
}

// ValidateTokenForAction is crud.ValidateAgentToken plus the token row id
// (idempotency key namespace) and force_simulate flag. nil when unknown.
func ValidateTokenForAction(ctx context.Context, db *sql.DB, tokenHash string) (*TokenAuth, error) {
	var (
		t  TokenAuth
		fs int64
	)
	err := db.QueryRowContext(ctx, "SELECT id, agent_id, force_simulate FROM agent_tokens WHERE token_hash = ?", tokenHash).
		Scan(&t.TokenID, &t.AgentID, &fs)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	t.ForceSimulate = fs != 0
	return &t, nil
}
