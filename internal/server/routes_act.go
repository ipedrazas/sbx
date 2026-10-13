package server

// Agent-facing Actions API (/act/*), the write-side counterpart to /a/*:
// bearer-auth → allowlist → pinned params → rate limit → action-state gate →
// idempotency lookup (replay, or 409 on body mismatch) → policy → either the
// approval queue (202, deduped on Idempotency-Key) or a real/simulated
// connector invocation → cache result → audit → respond. Approved actions are
// re-invoked by the management API so the result lands under the same key.

import (
	"fmt"
	"log/slog"
	"maps"
	"net/http"
	"time"

	"github.com/ipedrazas/sbx/internal/actions"
	"github.com/ipedrazas/sbx/internal/audit"
	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/factory"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/util"
)

// actIdempotencyTTLSeconds is the default idempotency TTL (24h) when the
// action doesn't set its own.
const actIdempotencyTTLSeconds int64 = 86_400

func (s *Server) registerAct(mux *http.ServeMux) {
	handle(mux, s.wrap(s.actInvoke), "POST /act/{agent_id}/{alias}")
	handle(mux, s.wrap(s.actGetApproval), "GET /act/{agent_id}/approvals/{approval_id}")
}

// actAuth validates the bearer token for /act and checks it matches agentID.
func (s *Server) actAuth(r *http.Request, agentID int64) (*actions.TokenAuth, error) {
	tok, ok := bearerToken(r)
	if !ok {
		return nil, errUnauthorized("missing or invalid Authorization header")
	}
	auth, err := actions.ValidateTokenForAction(r.Context(), s.db, util.HashToken(tok))
	if err != nil {
		return nil, errInternal("%s", err.Error())
	}
	if auth == nil {
		return nil, errUnauthorized("invalid token")
	}
	if auth.AgentID != agentID {
		return nil, errUnauthorized("token does not match agent")
	}
	return auth, nil
}

// actCall carries the per-request values shared by the response/audit helpers.
type actCall struct {
	agentID        int64
	alias          string
	action         *models.Action
	idempotencyKey string
	paramsHash     string
	simulated      bool
	started        time.Time
}

func (c *actCall) metadata(replayed bool) map[string]any {
	return map[string]any{
		"action_id":   c.action.ID,
		"action_type": c.action.ActionType,
		"replayed":    replayed,
		"simulated":   c.simulated,
	}
}

func (s *Server) actInvoke(w http.ResponseWriter, r *http.Request) error {
	started := time.Now()
	// Extractors run in axum order: path, query, body — before auth.
	agentID, err := pathInt64(r, "agent_id")
	if err != nil {
		return err
	}
	alias := r.PathValue("alias")
	simQuery, err := queryBool(r, "simulated")
	if err != nil {
		return err
	}
	var params any
	if err := decodeJSON(r, &params); err != nil {
		return err
	}
	ctx := r.Context()

	auth, err := s.actAuth(r, agentID)
	if err != nil {
		return err
	}
	// force_simulate on the token trumps the per-call flag.
	simulated := (simQuery != nil && *simQuery) || auth.ForceSimulate

	if _, ok := r.Header["Idempotency-Key"]; !ok {
		return errBadRequest("missing Idempotency-Key header")
	}
	idempotencyKey := r.Header.Get("Idempotency-Key")

	// Allowlist: the agent must be bound to this alias.
	binding, action, err := actions.ResolveActionAlias(ctx, s.db, agentID, alias)
	if err != nil {
		return errInternal("%s", err.Error())
	}
	if binding == nil || action == nil {
		return errNotFound("action alias '%s' not found for this agent", alias)
	}

	// Pinned params always win over the agent's values.
	if pinned, ok := binding.PinnedParams.(map[string]any); ok {
		if obj, ok := params.(map[string]any); ok {
			merged := maps.Clone(obj)
			maps.Copy(merged, pinned)
			params = merged
		}
	}

	// Rate limit before the state check so a paused action doesn't leak the limit.
	if c, p := binding.RateLimitCount, binding.RateLimitPeriodSeconds; c != nil && p != nil && *c > 0 && *p > 0 {
		d := s.rateLimiter.Check(agentID, BindingAction, binding.ActionID, uint32(*c), uint32(*p))
		if !d.Allowed {
			return apiErr(http.StatusTooManyRequests, "rate limit exceeded; retry after %ds", d.RetryAfter)
		}
	}

	switch action.State {
	case models.ActionPaused:
		return apiErr(http.StatusUnprocessableEntity, "action '%s' is paused", action.ActionType)
	case models.ActionDisabled:
		return apiErr(http.StatusUnprocessableEntity, "action '%s' is disabled", action.ActionType)
	}

	call := &actCall{
		agentID:        agentID,
		alias:          alias,
		action:         action,
		idempotencyKey: idempotencyKey,
		paramsHash:     util.HashCanonicalJSON(params),
		simulated:      simulated,
		started:        started,
	}

	// Idempotency cache (real and simulated namespaces are disjoint).
	cached, err := actions.LookupIdempotency(ctx, s.db, auth.TokenID, action.ID, idempotencyKey, simulated)
	if err != nil {
		return errInternal("%s", err.Error())
	}
	if cached != nil {
		if cached.ParamsHash != call.paramsHash {
			return errConflict("Idempotency-Key reused with a different request body")
		}
		s.actAudit(call, uint16(cached.Status), nil, nil)
		status := int(cached.Status)
		if status < 100 || status > 999 {
			status = http.StatusOK
		}
		writeJSON(w, status, map[string]any{"result": cached.Result, "metadata": call.metadata(true)})
		return nil
	}

	if actions.EvaluatePolicy(action, params) == actions.PolicyApproval {
		return s.actRouteThroughApproval(w, r, call, auth.TokenID, params)
	}

	ds, err := crud.GetDatasource(ctx, s.db, action.DatasourceID)
	if err != nil {
		return err
	}
	if ds == nil {
		return errInternal("action's datasource is missing")
	}
	if ds.ReadOnly {
		return errForbidden("action '%s' blocked: datasource is read-only", action.ActionType)
	}

	conn, err := factory.CreateConnector(ctx, s.db, ds)
	if err != nil {
		return err
	}
	defer factory.Close(conn)

	paramsObj, _ := params.(map[string]any)
	var result any
	if simulated {
		result, err = conn.SimulateAction(ctx, action.ActionType, paramsObj)
	} else {
		result, err = conn.InvokeAction(ctx, action.ActionType, paramsObj)
	}
	if err != nil {
		return err
	}

	ttl := actIdempotencyTTLSeconds
	if action.ApprovalTTLSeconds != nil {
		ttl = *action.ApprovalTTLSeconds
	}
	if _, err := actions.InsertIdempotency(ctx, s.db, auth.TokenID, action.ID, idempotencyKey, simulated,
		call.paramsHash, 200, result, ttl); err != nil {
		// Best-effort: the action already executed.
		slog.Warn(fmt.Sprintf("failed to insert idempotency row (action=%s, key=%s, simulated=%t): %v",
			action.ActionType, idempotencyKey, simulated, err))
	}

	s.actAudit(call, http.StatusOK, nil, nil)
	writeJSON(w, http.StatusOK, map[string]any{"result": result, "metadata": call.metadata(false)})
	return nil
}

// actGetApproval is the agent's poll endpoint. A missing approval and one
// owned by another agent both 404, so agents can't probe foreign ids.
func (s *Server) actGetApproval(w http.ResponseWriter, r *http.Request) error {
	agentID, err := pathInt64(r, "agent_id")
	if err != nil {
		return err
	}
	approvalID, err := pathInt64(r, "approval_id")
	if err != nil {
		return err
	}
	if _, err := s.actAuth(r, agentID); err != nil {
		return err
	}
	a, err := actions.GetApproval(r.Context(), s.db, approvalID)
	if err != nil {
		return errInternal("%s", err.Error())
	}
	if a == nil || a.AgentID != agentID {
		return errNotFound("approval not found")
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"approval_id": a.ID,
		"state":       a.State,
		"simulated":   a.Simulated,
		"result":      a.Result,
		"created_at":  a.CreatedAt,
		"expires_at":  a.ExpiresAt,
		"resolved_at": a.ResolvedAt,
		"resolved_by": a.ResolvedBy,
	})
	return nil
}

func (s *Server) actRouteThroughApproval(w http.ResponseWriter, r *http.Request, call *actCall, tokenID int64, params any) error {
	ctx := r.Context()
	// Dedupe: an earlier submit with the same key may already have a row.
	existing, err := actions.FindApprovalByIdempotency(ctx, s.db, call.agentID, call.action.ID, call.idempotencyKey)
	if err != nil {
		return errInternal("%s", err.Error())
	}
	if existing != nil {
		if existing.ParamsHash != call.paramsHash {
			return errConflict("Idempotency-Key reused with a different request body")
		}
		s.actApprovalStateResponse(w, call, existing)
		return nil
	}

	key := call.idempotencyKey
	id, err := actions.CreateApprovalWithTTL(ctx, s.db, call.agentID, call.action.ID, &tokenID, &key,
		params, call.paramsHash, call.simulated, call.action.ApprovalTTLSeconds)
	if err != nil {
		return errInternal("%s", err.Error())
	}
	fresh, err := actions.GetApproval(ctx, s.db, id)
	if err != nil {
		return errInternal("%s", err.Error())
	}
	if fresh == nil {
		return errInternal("approval row disappeared after insert")
	}
	st := fresh.State
	s.actAudit(call, http.StatusAccepted, &st, &fresh.ID)
	writeJSON(w, http.StatusAccepted, actPendingApprovalBody(call, fresh))
	return nil
}

func actPendingApprovalBody(call *actCall, a *models.ActionApproval) map[string]any {
	return map[string]any{
		"status":      "pending_approval",
		"approval_id": a.ID,
		"expires_at":  a.ExpiresAt,
		"poll_url":    fmt.Sprintf("/act/%d/approvals/%d", call.agentID, a.ID),
		"metadata":    call.metadata(false),
	}
}

// actApprovalStateResponse maps an existing approval onto an HTTP response:
// pending/approved → 202 (keep polling), executed → 200 with the result,
// and terminal failures to their HTTP equivalents.
func (s *Server) actApprovalStateResponse(w http.ResponseWriter, call *actCall, a *models.ActionApproval) {
	var status int
	var body map[string]any
	switch a.State {
	case models.ApprovalExecuted:
		status = http.StatusOK
		body = map[string]any{"result": a.Result, "metadata": call.metadata(true)}
	case models.ApprovalDenied:
		status = http.StatusForbidden
		body = map[string]any{"error": "approval denied", "approval_id": a.ID, "resolved_by": a.ResolvedBy}
	case models.ApprovalExpired:
		status = http.StatusGone
		body = map[string]any{
			"error":       "approval expired; submit a new request with a fresh Idempotency-Key",
			"approval_id": a.ID,
		}
	case models.ApprovalFailed:
		status = http.StatusBadGateway
		body = map[string]any{"error": "action execution failed after approval", "approval_id": a.ID, "result": a.Result}
	default: // pending, approved
		status = http.StatusAccepted
		body = actPendingApprovalBody(call, a)
	}
	st := a.State
	s.actAudit(call, uint16(status), &st, &a.ID)
	writeJSON(w, status, body)
}

func (s *Server) actAudit(call *actCall, status uint16, approvalState *models.ApprovalState, approvalID *int64) {
	agentID, actionID := call.agentID, call.action.ID
	alias, key, hash := call.alias, call.idempotencyKey, call.paramsHash
	var st *string
	if approvalState != nil {
		v := string(*approvalState)
		st = &v
	}
	s.audit.Send(audit.AuditEvent{
		AgentID:        &agentID,
		Alias:          &alias,
		Endpoint:       fmt.Sprintf("/act/%d/%s", call.agentID, call.alias),
		Status:         status,
		LatencyMs:      elapsedMS(call.started),
		ActionID:       &actionID,
		ApprovalID:     approvalID,
		IdempotencyKey: &key,
		ParamsHash:     &hash,
		Simulated:      call.simulated,
		ApprovalState:  st,
	})
}
