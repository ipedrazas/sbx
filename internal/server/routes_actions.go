package server

// Management API for Action definitions (/manage/actions). Actions are
// per-datasource; agent→action bindings live under /manage/agents/{id}/actions.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"

	"github.com/ipedrazas/sbx/internal/actions"
	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/factory"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/util"
)

const defaultApprovalTTLSeconds int64 = 86_400

func (s *Server) registerActions(mux *http.ServeMux) {
	const p = "/manage/actions"
	handle(mux, s.wrap(s.listActions), "GET "+p, "GET "+p+"/")
	handle(mux, s.wrap(s.createAction), "POST "+p, "POST "+p+"/")
	handle(mux, s.wrap(s.getAction), "GET "+p+"/{id}")
	handle(mux, s.wrap(s.updateAction), "PUT "+p+"/{id}", "POST "+p+"/{id}/update")
	handle(mux, s.wrap(s.deleteAction), "DELETE "+p+"/{id}", "POST "+p+"/{id}/delete")
	handle(mux, s.wrap(s.setActionState), "POST "+p+"/{id}/state")
	handle(mux, s.wrap(s.testAction), "POST "+p+"/{id}/test")
}

func (s *Server) listActions(w http.ResponseWriter, r *http.Request) error {
	dsID, err := queryInt64(r, "datasource_id")
	if err != nil {
		return err
	}
	rows, err := actions.ListActions(r.Context(), s.db, dsID)
	if err != nil {
		return errInternal("%v", err)
	}
	scopeMap := s.buildScopeMap(r.Context(), rows)
	out := make([]map[string]any, 0, len(rows))
	for i := range rows {
		out = append(out, enrichAction(&rows[i], scopeMap))
	}
	writeJSON(w, http.StatusOK, map[string]any{"actions": out})
	return nil
}

// actionBody holds the fields shared by create/update, with serde defaults.
type actionBody struct {
	Name            *string         `json:"name"`
	Description     *string         `json:"description"`
	ParamsSchema    json.RawMessage `json:"params_schema"`
	ResultSchema    json.RawMessage `json:"result_schema"`
	PolicyKind      *string         `json:"policy_kind"`
	ThresholdConfig json.RawMessage `json:"threshold_config"`
	// Absent → 24h; explicit null → never expire.
	ApprovalTTLSeconds json.RawMessage `json:"approval_ttl_seconds"`
}

type resolvedActionBody struct {
	paramsSchema, resultSchema, thresholdConfig any
	policyKind                                  models.PolicyKind
	ttl                                         *int64
}

func badVariant(field, v string, allowed ...string) error {
	return apiErr(http.StatusUnprocessableEntity,
		"Failed to deserialize the JSON body into the target type: %s: unknown variant `%s`, expected one of %s",
		field, v, "`"+strings.Join(allowed, "`, `")+"`")
}

// rawOrEmptyObject decodes raw, defaulting to {} when absent.
func rawOrEmptyObject(raw json.RawMessage) (any, error) {
	if raw == nil {
		return map[string]any{}, nil
	}
	return util.DecodeJSON(raw)
}

func (b *actionBody) resolve() (*resolvedActionBody, error) {
	if b.Name == nil {
		return nil, missingJSONField("name")
	}
	out := &resolvedActionBody{policyKind: models.PolicyAuto}
	var err error
	if out.paramsSchema, err = rawOrEmptyObject(b.ParamsSchema); err != nil {
		return nil, err
	}
	if out.resultSchema, err = rawOrEmptyObject(b.ResultSchema); err != nil {
		return nil, err
	}
	if out.thresholdConfig, err = rawOrEmptyObject(b.ThresholdConfig); err != nil {
		return nil, err
	}
	if b.PolicyKind != nil {
		switch pk := models.PolicyKind(*b.PolicyKind); pk {
		case models.PolicyAuto, models.PolicyAlwaysApprove, models.PolicyThreshold:
			out.policyKind = pk
		default:
			return nil, badVariant("policy_kind", *b.PolicyKind, "auto", "always_approve", "threshold")
		}
	}
	ttl := defaultApprovalTTLSeconds
	out.ttl = &ttl
	if b.ApprovalTTLSeconds != nil {
		if bytes.Equal(bytes.TrimSpace(b.ApprovalTTLSeconds), []byte("null")) {
			out.ttl = nil
		} else if err := json.Unmarshal(b.ApprovalTTLSeconds, &ttl); err != nil {
			return nil, apiErr(http.StatusUnprocessableEntity,
				"Failed to deserialize the JSON body into the target type: approval_ttl_seconds: %v", err)
		}
	}
	return out, nil
}

type createActionRequest struct {
	actionBody
	DatasourceID   *int64   `json:"datasource_id"`
	ActionType     *string  `json:"action_type"`
	RiskLevel      *string  `json:"risk_level"`
	RequiredScopes []string `json:"required_scopes"`
	Predefined     bool     `json:"predefined"`
}

func (s *Server) createAction(w http.ResponseWriter, r *http.Request) error {
	var req createActionRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	if req.DatasourceID == nil {
		return missingJSONField("datasource_id")
	}
	if req.Name == nil {
		return missingJSONField("name")
	}
	if req.ActionType == nil {
		return missingJSONField("action_type")
	}
	body, err := req.resolve()
	if err != nil {
		return err
	}
	risk := models.RiskMedium
	if req.RiskLevel != nil {
		switch rl := models.RiskLevel(*req.RiskLevel); rl {
		case models.RiskLow, models.RiskMedium, models.RiskHigh, models.RiskCritical:
			risk = rl
		default:
			return badVariant("risk_level", *req.RiskLevel, "low", "medium", "high", "critical")
		}
	}
	scopes := req.RequiredScopes
	if scopes == nil {
		scopes = []string{}
	}
	id, err := actions.CreateActionFull(r.Context(), s.db, *req.DatasourceID, *req.Name, *req.ActionType,
		req.Description, body.paramsSchema, body.resultSchema, body.policyKind, body.thresholdConfig,
		body.ttl, risk, scopes, req.Predefined)
	if err != nil {
		return errBadRequest("%v", err)
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
	return nil
}

func (s *Server) loadAction(ctx context.Context, id int64) (*models.Action, error) {
	a, err := actions.GetAction(ctx, s.db, id)
	if err != nil {
		return nil, errInternal("%v", err)
	}
	if a == nil {
		return nil, errNotFound("action not found")
	}
	return a, nil
}

func (s *Server) getAction(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt64(r, "id")
	if err != nil {
		return err
	}
	a, err := s.loadAction(r.Context(), id)
	if err != nil {
		return err
	}
	scopeMap := s.buildScopeMap(r.Context(), []models.Action{*a})
	writeJSON(w, http.StatusOK, enrichAction(a, scopeMap))
	return nil
}

func (s *Server) updateAction(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt64(r, "id")
	if err != nil {
		return err
	}
	var req actionBody
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	body, err := req.resolve()
	if err != nil {
		return err
	}
	ok, err := actions.UpdateAction(r.Context(), s.db, id, *req.Name, req.Description, body.paramsSchema,
		body.resultSchema, body.policyKind, body.thresholdConfig, body.ttl)
	if err != nil {
		return errBadRequest("%v", err)
	}
	if !ok {
		return errNotFound("action not found")
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": true})
	return nil
}

func (s *Server) deleteAction(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt64(r, "id")
	if err != nil {
		return err
	}
	ok, err := actions.DeleteAction(r.Context(), s.db, id)
	switch {
	case errors.Is(err, actions.ErrPredefined):
		return errForbidden("predefined actions cannot be deleted")
	case err != nil:
		return errInternal("%v", err)
	case !ok:
		return errNotFound("action not found")
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	return nil
}

func (s *Server) setActionState(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt64(r, "id")
	if err != nil {
		return err
	}
	var req struct {
		State *string `json:"state"`
	}
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	if req.State == nil {
		return missingJSONField("state")
	}
	st := models.ActionState(*req.State)
	switch st {
	case models.ActionEnabled, models.ActionDisabled, models.ActionPaused:
	default:
		return badVariant("state", *req.State, "enabled", "disabled", "paused")
	}
	ok, err := actions.SetActionState(r.Context(), s.db, id, st)
	if err != nil {
		return errInternal("%v", err)
	}
	if !ok {
		return errNotFound("action not found")
	}
	writeJSON(w, http.StatusOK, map[string]any{"state": st})
	return nil
}

// paramsMap converts a JSON value into the connector's params map (nil when
// the value isn't an object).
func paramsMap(v any) map[string]any {
	m, _ := v.(map[string]any)
	return m
}

func (s *Server) testAction(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt64(r, "id")
	if err != nil {
		return err
	}
	var req struct {
		Params json.RawMessage `json:"params"`
	}
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	params, err := rawOrEmptyObject(req.Params)
	if err != nil {
		return err
	}
	a, err := s.loadAction(r.Context(), id)
	if err != nil {
		return err
	}
	ds, err := crud.GetDatasource(r.Context(), s.db, a.DatasourceID)
	if err != nil {
		return err
	}
	if ds == nil {
		return errInternal("action's datasource is missing")
	}
	conn, err := factory.CreateConnector(r.Context(), s.db, ds)
	if err != nil {
		return err
	}
	defer factory.Close(conn)
	result, err := conn.SimulateAction(r.Context(), a.ActionType, paramsMap(params))
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"result": result})
	return nil
}

// buildScopeMap maps datasource_id → granted OAuth scopes (empty when the
// datasource has no OAuth connection).
func (s *Server) buildScopeMap(ctx context.Context, rows []models.Action) map[int64][]string {
	m := map[int64][]string{}
	for _, a := range rows {
		if _, ok := m[a.DatasourceID]; ok {
			continue
		}
		var scopes []string
		if ds, err := crud.GetDatasource(ctx, s.db, a.DatasourceID); err == nil && ds != nil && ds.ConnectionID != nil {
			if c, err := crud.GetOAuthConnection(ctx, s.db, *ds.ConnectionID); err == nil && c != nil {
				scopes = c.Scopes
			}
		}
		m[a.DatasourceID] = scopes
	}
	return m
}

// enrichAction serializes an action and appends `scopes_satisfied` and
// `missing_scopes`. With no granted scopes there is nothing to check, so the
// action counts as satisfied.
func enrichAction(a *models.Action, scopeMap map[int64][]string) map[string]any {
	granted := scopeMap[a.DatasourceID]
	missing := []string{}
	if len(granted) > 0 {
		for _, req := range a.RequiredScopes {
			found := false
			for _, g := range granted {
				if scopeMatches(g, req) {
					found = true
					break
				}
			}
			if !found {
				missing = append(missing, req)
			}
		}
	}
	out := map[string]any{}
	if b, err := json.Marshal(a); err == nil {
		if v, err := util.DecodeJSON(b); err == nil {
			if m, ok := v.(map[string]any); ok {
				out = m
			}
		}
	}
	out["scopes_satisfied"] = len(missing) == 0
	out["missing_scopes"] = missing
	return out
}

// scopeMatches: exact match, or a full-URL / dotted granted scope ending in
// the required short form (".../auth/gmail.compose" satisfies "gmail.compose").
func scopeMatches(granted, required string) bool {
	return granted == required ||
		strings.HasSuffix(granted, "/"+required) ||
		strings.HasSuffix(granted, "."+required)
}
