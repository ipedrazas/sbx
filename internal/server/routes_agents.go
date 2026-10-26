package server

import (
	"encoding/json"
	"fmt"
	"log/slog"
	"mime"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/ipedrazas/sbx/internal/actions"
	"github.com/ipedrazas/sbx/internal/agentdir"
	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/util"
)

func (s *Server) registerAgents(mux *http.ServeMux) {
	const p = "/manage/agents"
	handle(mux, s.listAgentsHandler, "GET "+p, "GET "+p+"/")
	handle(mux, s.createAgentHandler, "POST "+p, "POST "+p+"/")
	handle(mux, s.getAgentHandler, "GET "+p+"/{id}")
	handle(mux, s.updateAgentHandler, "PUT "+p+"/{id}", "POST "+p+"/{id}/update")
	handle(mux, s.deleteAgentHandler, "DELETE "+p+"/{id}", "POST "+p+"/{id}/delete")

	handle(mux, s.listAgentDatasetsHandler, "GET "+p+"/{id}/datasets")
	handle(mux, s.bindAgentDatasetHandler, "POST "+p+"/{id}/datasets")
	handle(mux, s.updateAgentDatasetBindingHandler, "PUT "+p+"/{id}/datasets/{dataset_id}")
	handle(mux, s.unbindAgentDatasetHandler,
		"DELETE "+p+"/{id}/datasets/{dataset_id}", "POST "+p+"/{id}/datasets/{dataset_id}/unbind")

	handle(mux, s.listAgentTokensHandler, "GET "+p+"/{id}/tokens")
	handle(mux, s.createAgentTokenHandler, "POST "+p+"/{id}/tokens")
	handle(mux, s.deleteAgentTokenHandler,
		"DELETE "+p+"/{id}/tokens/{token_id}", "POST "+p+"/{id}/tokens/{token_id}/revoke")
	// Per-token force-simulate flag: route every action invoked via the token to
	// the simulator — intended for staging tokens.
	handle(mux, s.setAgentTokenForceSimulateHandler, "POST "+p+"/{id}/tokens/{token_id}/force-simulate")

	handle(mux, s.agentMetricsHandler, "GET "+p+"/{id}/metrics")

	handle(mux, s.generateAgentConfigHandler, "POST "+p+"/{id}/generate-cli-config", "POST "+p+"/{id}/generate-mcp-config")
	handle(mux, s.generateAgentSkillHandler, "POST "+p+"/{id}/generate-skill")

	// Action bindings: which actions this agent may invoke, plus alias and rate cap.
	handle(mux, s.listAgentActionsHandler, "GET "+p+"/{id}/actions")
	handle(mux, s.bindAgentActionHandler, "POST "+p+"/{id}/actions")
	handle(mux, s.updateAgentActionBindingHandler, "PUT "+p+"/{id}/actions/{action_id}")
	handle(mux, s.unbindAgentActionHandler,
		"DELETE "+p+"/{id}/actions/{action_id}", "POST "+p+"/{id}/actions/{action_id}/unbind")

	// Skill installations — external folders where this agent's SKILL.md is mirrored.
	handle(mux, s.listAgentInstallationsHandler, "GET "+p+"/{id}/installations")
	handle(mux, s.createAgentInstallationHandler, "POST "+p+"/{id}/installations")
	handle(mux, s.deleteAgentInstallationHandler,
		"DELETE "+p+"/{id}/installations/{inst_id}", "POST "+p+"/{id}/installations/{inst_id}/remove")

	handle(mux, s.exportAgentSkillPackageHandler, "POST "+p+"/{id}/export")
}

// resyncAgentDir re-syncs the agent directory on disk, preserving the existing
// token from config.json when no new token is given. Failures are logged.
func (s *Server) resyncAgentDir(r *http.Request, agentID int64, newToken *string) {
	token := newToken
	if token == nil {
		token = s.readExistingAgentToken(r, agentID)
	}
	if _, err := agentdir.SyncAgentDir(r.Context(), s.db, s.dataDir, agentID, token, s.daemonURL); err != nil {
		slog.Warn(fmt.Sprintf("Failed to sync agent dir for agent %d: %v", agentID, err))
	}
}

// readExistingAgentToken reads the token from the agent's existing config.json.
func (s *Server) readExistingAgentToken(r *http.Request, agentID int64) *string {
	agent, err := crud.GetAgent(r.Context(), s.db, agentID)
	if err != nil || agent == nil {
		return nil
	}
	data, err := os.ReadFile(filepath.Join(agentdir.AgentPath(s.dataDir, agent.Name), "config.json"))
	if err != nil {
		return nil
	}
	var parsed map[string]any
	if json.Unmarshal(data, &parsed) != nil {
		return nil
	}
	tok, ok := parsed["token"].(string)
	if !ok || tok == agentdir.TokenPlaceholder {
		return nil
	}
	return &tok
}

// agentPathIDs parses the named int64 path params, writing a 400 on failure.
func agentPathIDs(w http.ResponseWriter, r *http.Request, names ...string) ([]int64, bool) {
	out := make([]int64, len(names))
	for i, n := range names {
		v, err := pathInt64(r, n)
		if err != nil {
			writeError(w, err)
			return nil, false
		}
		out[i] = v
	}
	return out, true
}

// agentDecode decodes the JSON body, writing the rejection on failure.
func agentDecode(w http.ResponseWriter, r *http.Request, v any) bool {
	if err := decodeJSON(r, v); err != nil {
		writeError(w, err)
		return false
	}
	return true
}

// agentDecodeOptional mirrors axum 0.8's `Option<Json<T>>`: no JSON content
// type ⇒ absent; otherwise the body must parse.
func agentDecodeOptional(w http.ResponseWriter, r *http.Request, v any) (present, ok bool) {
	mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || !(mt == "application/json" || (strings.HasPrefix(mt, "application/") && strings.HasSuffix(mt, "+json"))) {
		return false, true
	}
	if !agentDecode(w, r, v) {
		return false, false
	}
	return true, true
}

// agentNonEmpty returns nil for nil or empty strings (`.filter(|s| !s.is_empty())`).
func agentNonEmpty(s *string) *string {
	if s == nil || *s == "" {
		return nil
	}
	return s
}

// agentTrimmedNonEmpty trims and returns nil when the result is empty.
func agentTrimmedNonEmpty(s *string) *string {
	if s == nil {
		return nil
	}
	t := strings.TrimSpace(*s)
	if t == "" {
		return nil
	}
	return &t
}

// agentRawPresent reports whether a sentinel field was present (incl. null).
func agentRawPresent(raw json.RawMessage) bool { return raw != nil }

// agentRawInt64 decodes a present sentinel field as an optional int64.
func agentRawInt64(raw json.RawMessage, name string) (*int64, error) {
	if raw == nil {
		return nil, nil
	}
	var v *int64
	if err := json.Unmarshal(raw, &v); err != nil {
		return nil, apiErr(http.StatusUnprocessableEntity,
			"Failed to deserialize the JSON body into the target type: %s: %v", name, err)
	}
	return v, nil
}

// ── Agent CRUD ──

func (s *Server) listAgentsHandler(w http.ResponseWriter, r *http.Request) {
	agents, err := crud.ListAgents(r.Context(), s.db)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"agents": agents})
}

type agentWriteRequest struct {
	Name        string  `json:"name"`
	Description *string `json:"description"`
	AgentType   *string `json:"agent_type"`
	Owner       *string `json:"owner"`
	Status      *string `json:"status"`
	Mission     *string `json:"mission"`
}

func (req *agentWriteRequest) meta() crud.AgentMeta {
	return crud.AgentMeta{
		AgentType: agentNonEmpty(req.AgentType),
		Owner:     agentNonEmpty(req.Owner),
		Status:    agentNonEmpty(req.Status),
		Mission:   agentNonEmpty(req.Mission),
	}
}

// decodeAgentWriteRequest requires `name` like serde's non-optional String.
func decodeAgentWriteRequest(w http.ResponseWriter, r *http.Request) (*agentWriteRequest, bool) {
	var raw struct {
		agentWriteRequest
		Name *string `json:"name"`
	}
	if !agentDecode(w, r, &raw) {
		return nil, false
	}
	if raw.Name == nil {
		writeError(w, apiErr(http.StatusUnprocessableEntity,
			"Failed to deserialize the JSON body into the target type: missing field `name`"))
		return nil, false
	}
	req := raw.agentWriteRequest
	req.Name = *raw.Name
	return &req, true
}

func (s *Server) createAgentHandler(w http.ResponseWriter, r *http.Request) {
	req, ok := decodeAgentWriteRequest(w, r)
	if !ok {
		return
	}
	id, err := crud.CreateAgentWithMeta(r.Context(), s.db, req.Name, req.Description, req.meta())
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) getAgentHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := agentPathIDs(w, r, "id")
	if !ok {
		return
	}
	agent, err := crud.GetAgent(r.Context(), s.db, ids[0])
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if agent == nil {
		writeErrorOnly(w, http.StatusNotFound, "not found")
		return
	}
	datasets, err := crud.ListAgentDatasets(r.Context(), s.db, ids[0])
	if err != nil || datasets == nil {
		datasets = []models.AgentDataset{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent": agent, "datasets": datasets})
}

func (s *Server) updateAgentHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := agentPathIDs(w, r, "id")
	if !ok {
		return
	}
	req, ok := decodeAgentWriteRequest(w, r)
	if !ok {
		return
	}
	updated, err := crud.UpdateAgentWithMeta(r.Context(), s.db, ids[0], req.Name, req.Description, req.meta())
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !updated {
		writeErrorOnly(w, http.StatusNotFound, "not found")
		return
	}
	s.resyncAgentDir(r, ids[0], nil)
	writeJSON(w, http.StatusOK, map[string]any{"updated": true})
}

func (s *Server) deleteAgentHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := agentPathIDs(w, r, "id")
	if !ok {
		return
	}
	// Grab the name before deletion so we can remove the directory.
	var name *string
	if a, err := crud.GetAgent(r.Context(), s.db, ids[0]); err == nil && a != nil {
		name = &a.Name
	}
	deleted, err := crud.DeleteAgent(r.Context(), s.db, ids[0])
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !deleted {
		writeErrorOnly(w, http.StatusNotFound, "not found")
		return
	}
	if name != nil {
		agentdir.RemoveAgentDir(s.dataDir, *name)
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// ── Dataset bindings ──

func (s *Server) listAgentDatasetsHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := agentPathIDs(w, r, "id")
	if !ok {
		return
	}
	datasets, err := crud.ListAgentDatasets(r.Context(), s.db, ids[0])
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"datasets": datasets})
}

type agentBindDatasetRequest struct {
	DatasetID *int64  `json:"dataset_id"`
	Alias     *string `json:"alias"`
	// Per-binding rate cap: count per period; either missing = unlimited.
	RateLimitCount         *int64 `json:"rate_limit_count"`
	RateLimitPeriodSeconds *int64 `json:"rate_limit_period_seconds"`
}

// agentMissingField renders serde's missing-field rejection.
func agentMissingField(w http.ResponseWriter, field string) {
	writeError(w, apiErr(http.StatusUnprocessableEntity,
		"Failed to deserialize the JSON body into the target type: missing field `%s`", field))
}

func (s *Server) bindAgentDatasetHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := agentPathIDs(w, r, "id")
	if !ok {
		return
	}
	id := ids[0]
	var req agentBindDatasetRequest
	if !agentDecode(w, r, &req) {
		return
	}
	if req.DatasetID == nil {
		agentMissingField(w, "dataset_id")
		return
	}
	if req.Alias == nil {
		agentMissingField(w, "alias")
		return
	}
	ctx := r.Context()
	agent, err := crud.GetAgent(ctx, s.db, id)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if agent == nil {
		writeErrorOnly(w, http.StatusNotFound, "agent not found")
		return
	}
	ds, err := crud.GetDataset(ctx, s.db, *req.DatasetID)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ds == nil {
		writeErrorOnly(w, http.StatusBadRequest, "dataset not found")
		return
	}
	bindings, err := crud.ListAgentDatasets(ctx, s.db, id)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, b := range bindings {
		if b.DatasetID == *req.DatasetID {
			writeErrorOnly(w, http.StatusConflict, fmt.Sprintf(
				"dataset %d (%q) is already bound on %s as %q", ds.ID, ds.Name, s.agentLabel(r, id), b.Alias))
			return
		}
	}
	if msg, err := s.datasetAliasConflict(r, id, *req.DatasetID, *req.Alias); err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	} else if msg != "" {
		writeErrorOnly(w, http.StatusConflict, msg)
		return
	}
	bindingID, err := crud.BindAgentDataset(ctx, s.db, id, *req.DatasetID, *req.Alias,
		req.RateLimitCount, req.RateLimitPeriodSeconds)
	if err != nil {
		writeBindingWriteError(w, err)
		return
	}
	s.resyncAgentDir(r, id, nil)
	writeJSON(w, http.StatusCreated, map[string]any{"id": bindingID})
}

// datasetAliasConflict returns a user-facing message when alias is already
// bound on agentID to a dataset other than datasetID, or "" when it is free.
func (s *Server) datasetAliasConflict(r *http.Request, agentID, datasetID int64, alias string) (string, error) {
	ctx := r.Context()
	b, err := crud.GetAgentBinding(ctx, s.db, agentID, alias)
	if err != nil || b == nil || b.DatasetID == datasetID {
		return "", err
	}
	other, err := crud.GetDataset(ctx, s.db, b.DatasetID)
	if err != nil {
		return "", err
	}
	if other == nil {
		return fmt.Sprintf("alias %q is already bound on %s to dataset %d",
			alias, s.agentLabel(r, agentID), b.DatasetID), nil
	}
	return fmt.Sprintf("alias %q is already bound on %s to dataset %d (%q)",
		alias, s.agentLabel(r, agentID), other.ID, other.Name), nil
}

// agentLabel names an agent in error messages, e.g. `agent 113 ("Mail Bot")`.
// Aliases are scoped to the agent, which may be shared by several people, so
// conflicts say whose namespace the alias lives in.
func (s *Server) agentLabel(r *http.Request, agentID int64) string {
	a, err := crud.GetAgent(r.Context(), s.db, agentID)
	if err != nil || a == nil {
		return fmt.Sprintf("agent %d", agentID)
	}
	return fmt.Sprintf("agent %d (%q)", a.ID, a.Name)
}

// writeBindingWriteError maps a binding insert/update failure: a UNIQUE
// violation that slipped past the pre-checks (a concurrent write) is a 409,
// anything else a 500.
func writeBindingWriteError(w http.ResponseWriter, err error) {
	if strings.Contains(err.Error(), "UNIQUE constraint failed") {
		writeErrorOnly(w, http.StatusConflict, "binding conflicts with an existing binding for this agent")
		return
	}
	writeErrorOnly(w, http.StatusInternalServerError, err.Error())
}

type agentUpdateDatasetBindingRequest struct {
	Alias *string `json:"alias"`
	// Sentinels: present (incl. null) to set/clear; absent = unchanged.
	RateLimitCount         json.RawMessage `json:"rate_limit_count"`
	RateLimitPeriodSeconds json.RawMessage `json:"rate_limit_period_seconds"`
}

func (s *Server) updateAgentDatasetBindingHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := agentPathIDs(w, r, "id", "dataset_id")
	if !ok {
		return
	}
	id, datasetID := ids[0], ids[1]
	var req agentUpdateDatasetBindingRequest
	if !agentDecode(w, r, &req) {
		return
	}
	count, err := agentRawInt64(req.RateLimitCount, "rate_limit_count")
	if err != nil {
		writeError(w, err)
		return
	}
	period, err := agentRawInt64(req.RateLimitPeriodSeconds, "rate_limit_period_seconds")
	if err != nil {
		writeError(w, err)
		return
	}
	ctx := r.Context()
	if req.Alias != nil {
		if msg, err := s.datasetAliasConflict(r, id, datasetID, *req.Alias); err != nil {
			writeErrorOnly(w, http.StatusInternalServerError, err.Error())
			return
		} else if msg != "" {
			writeErrorOnly(w, http.StatusConflict, msg)
			return
		}
		if _, err := crud.UpdateAgentDatasetAlias(ctx, s.db, id, datasetID, *req.Alias); err != nil {
			writeBindingWriteError(w, err)
			return
		}
	}
	// Either rate-limit field present ⇒ apply the pair (both null clears).
	if agentRawPresent(req.RateLimitCount) || agentRawPresent(req.RateLimitPeriodSeconds) {
		if _, err := crud.UpdateAgentDatasetRateLimit(ctx, s.db, id, datasetID, count, period); err != nil {
			writeErrorOnly(w, http.StatusInternalServerError, err.Error())
			return
		}
		// Drop the in-memory bucket so the next call rebuilds with the new period.
		s.rateLimiter.Forget(id, BindingDataset, datasetID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": true})
}

func (s *Server) unbindAgentDatasetHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := agentPathIDs(w, r, "id", "dataset_id")
	if !ok {
		return
	}
	id, datasetID := ids[0], ids[1]
	deleted, err := crud.UnbindAgentDataset(r.Context(), s.db, id, datasetID)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !deleted {
		writeErrorOnly(w, http.StatusNotFound, "binding not found")
		return
	}
	s.rateLimiter.Forget(id, BindingDataset, datasetID)
	s.resyncAgentDir(r, id, nil)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// ── Agent tokens ──

func (s *Server) listAgentTokensHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := agentPathIDs(w, r, "id")
	if !ok {
		return
	}
	tokens, err := crud.GetAgentTokens(r.Context(), s.db, ids[0])
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"tokens": tokens})
}

func (s *Server) createAgentTokenHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := agentPathIDs(w, r, "id")
	if !ok {
		return
	}
	id := ids[0]
	var req struct {
		Label *string `json:"label"`
	}
	if !agentDecode(w, r, &req) {
		return
	}
	agent, err := crud.GetAgent(r.Context(), s.db, id)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if agent == nil {
		writeErrorOnly(w, http.StatusNotFound, "agent not found")
		return
	}
	raw := util.GenerateBearerToken()
	label := "default"
	if req.Label != nil {
		label = *req.Label
	}
	tokenID, err := crud.CreateAgentToken(r.Context(), s.db, id, util.HashToken(raw), label)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.resyncAgentDir(r, id, &raw)
	writeJSON(w, http.StatusCreated, map[string]any{
		"id":      tokenID,
		"token":   raw,
		"message": "Store this token securely — it won't be shown again",
	})
}

func (s *Server) deleteAgentTokenHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := agentPathIDs(w, r, "id", "token_id")
	if !ok {
		return
	}
	deleted, err := crud.DeleteAgentToken(r.Context(), s.db, ids[1])
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !deleted {
		writeErrorOnly(w, http.StatusNotFound, "not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

func (s *Server) setAgentTokenForceSimulateHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := agentPathIDs(w, r, "id", "token_id")
	if !ok {
		return
	}
	var req struct {
		ForceSimulate *bool `json:"force_simulate"`
	}
	if !agentDecode(w, r, &req) {
		return
	}
	if req.ForceSimulate == nil {
		agentMissingField(w, "force_simulate")
		return
	}
	updated, err := actions.SetTokenForceSimulate(r.Context(), s.db, ids[1], *req.ForceSimulate)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !updated {
		writeErrorOnly(w, http.StatusNotFound, "token not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"force_simulate": *req.ForceSimulate})
}

// ── Metrics ──

func (s *Server) agentMetricsHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := agentPathIDs(w, r, "id")
	if !ok {
		return
	}
	bindings, _ := crud.ListAgentDatasets(r.Context(), s.db, ids[0])
	perDataset := map[string]any{}
	for _, b := range bindings {
		perDataset[b.Alias] = s.metrics.Snapshot(b.DatasetID)
	}
	writeJSON(w, http.StatusOK, map[string]any{"agent_id": ids[0], "datasets": perDataset})
}

// ── Config generation ──

type agentDaemonURLOverride struct {
	DaemonURL *string `json:"daemon_url"`
}

// agentLoadForRender fetches the agent plus its dataset and action bindings.
func (s *Server) agentLoadForRender(r *http.Request, agentID int64) (*models.Agent, []models.AgentDataset, []models.AgentAction, error) {
	ctx := r.Context()
	agent, err := crud.GetAgent(ctx, s.db, agentID)
	if err != nil {
		return nil, nil, nil, err
	}
	if agent == nil {
		return nil, nil, nil, fmt.Errorf("agent not found")
	}
	bindings, err := crud.ListAgentDatasets(ctx, s.db, agentID)
	if err != nil {
		return nil, nil, nil, err
	}
	actionBindings, err := actions.ListAgentActions(ctx, s.db, agentID)
	if err != nil {
		return nil, nil, nil, err
	}
	return agent, bindings, actionBindings, nil
}

// buildAgentAPIConfig delegates to agentdir for config generation.
func (s *Server) buildAgentAPIConfig(r *http.Request, agentID int64, daemonURL *string) (map[string]any, error) {
	agent, bindings, actionBindings, err := s.agentLoadForRender(r, agentID)
	if err != nil {
		return nil, err
	}
	url := s.daemonURL
	if daemonURL != nil {
		url = *daemonURL
	}
	return agentdir.BuildConfig(r.Context(), s.db, agent, bindings, actionBindings, nil, url)
}

// buildAgentAPISkill delegates to agentdir for SKILL.md generation.
func (s *Server) buildAgentAPISkill(r *http.Request, agentID int64, daemonURL *string) (string, error) {
	agent, bindings, actionBindings, err := s.agentLoadForRender(r, agentID)
	if err != nil {
		return "", err
	}
	url := s.daemonURL
	if daemonURL != nil {
		url = *daemonURL
	}
	return agentdir.BuildSkillMD(r.Context(), s.db, agent, bindings, actionBindings, url)
}

// Serves both generate-cli-config and generate-mcp-config (identical in Rust).
func (s *Server) generateAgentConfigHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := agentPathIDs(w, r, "id")
	if !ok {
		return
	}
	var body agentDaemonURLOverride
	if _, ok := agentDecodeOptional(w, r, &body); !ok {
		return
	}
	cfg, err := s.buildAgentAPIConfig(r, ids[0], body.DaemonURL)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, cfg)
}

func (s *Server) generateAgentSkillHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := agentPathIDs(w, r, "id")
	if !ok {
		return
	}
	var body agentDaemonURLOverride
	if _, ok := agentDecodeOptional(w, r, &body); !ok {
		return
	}
	md, err := s.buildAgentAPISkill(r, ids[0], body.DaemonURL)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"skill": md})
}

// ── Agent → action bindings ──

func (s *Server) listAgentActionsHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := agentPathIDs(w, r, "id")
	if !ok {
		return
	}
	bindings, err := actions.ListAgentActions(r.Context(), s.db, ids[0])
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"bindings": bindings})
}

func (s *Server) bindAgentActionHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := agentPathIDs(w, r, "id")
	if !ok {
		return
	}
	id := ids[0]
	var req struct {
		ActionID *int64  `json:"action_id"`
		Alias    *string `json:"alias"`
		Template *string `json:"template"`
	}
	if !agentDecode(w, r, &req) {
		return
	}
	if req.ActionID == nil {
		agentMissingField(w, "action_id")
		return
	}
	if req.Alias == nil {
		agentMissingField(w, "alias")
		return
	}
	ctx := r.Context()
	// Reject missing agent/action early for a clean 404/400 instead of an FK 500.
	if a, err := crud.GetAgent(ctx, s.db, id); err == nil && a == nil {
		writeErrorOnly(w, http.StatusNotFound, "agent not found")
		return
	}
	if a, err := actions.GetAction(ctx, s.db, *req.ActionID); err == nil && a == nil {
		writeErrorOnly(w, http.StatusBadRequest, "action not found")
		return
	}
	bindings, err := actions.ListAgentActions(ctx, s.db, id)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	for _, b := range bindings {
		if b.ActionID == *req.ActionID {
			writeErrorOnly(w, http.StatusConflict, fmt.Sprintf(
				"action %d (%q) is already bound on %s as %q",
				b.ActionID, s.actionName(r, b.ActionID), s.agentLabel(r, id), b.Alias))
			return
		}
	}
	if msg := s.actionAliasConflict(r, bindings, *req.ActionID, *req.Alias); msg != "" {
		writeErrorOnly(w, http.StatusConflict, msg)
		return
	}
	bindingID, err := actions.BindAgentAction(ctx, s.db, id, *req.ActionID, *req.Alias, agentTrimmedNonEmpty(req.Template))
	if err != nil {
		writeBindingWriteError(w, err)
		return
	}
	s.resyncAgentDir(r, id, nil)
	writeJSON(w, http.StatusCreated, map[string]any{"id": bindingID})
}

// actionAliasConflict returns a user-facing message when alias is already used
// in bindings by an action other than actionID, or "" when it is free.
func (s *Server) actionAliasConflict(r *http.Request, bindings []models.AgentAction, actionID int64, alias string) string {
	for _, b := range bindings {
		if b.Alias == alias && b.ActionID != actionID {
			return fmt.Sprintf("alias %q is already bound on %s to action %d (%q)",
				alias, s.agentLabel(r, b.AgentID), b.ActionID, s.actionName(r, b.ActionID))
		}
	}
	return ""
}

// actionName is the action's name for error messages; "" if it can't be read.
func (s *Server) actionName(r *http.Request, actionID int64) string {
	a, err := actions.GetAction(r.Context(), s.db, actionID)
	if err != nil || a == nil {
		return ""
	}
	return a.Name
}

type agentUpdateActionBindingRequest struct {
	Alias *string `json:"alias"`
	// Sentinels: present (incl. null) to set/clear; absent = unchanged.
	RateLimitCount         json.RawMessage `json:"rate_limit_count"`
	RateLimitPeriodSeconds json.RawMessage `json:"rate_limit_period_seconds"`
	Template               json.RawMessage `json:"template"`
	// Pinned params are force-merged into the agent's request at invocation.
	PinnedParams json.RawMessage `json:"pinned_params"`
}

func (s *Server) updateAgentActionBindingHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := agentPathIDs(w, r, "id", "action_id")
	if !ok {
		return
	}
	id, actionID := ids[0], ids[1]
	var req agentUpdateActionBindingRequest
	if !agentDecode(w, r, &req) {
		return
	}
	count, err := agentRawInt64(req.RateLimitCount, "rate_limit_count")
	if err != nil {
		writeError(w, err)
		return
	}
	period, err := agentRawInt64(req.RateLimitPeriodSeconds, "rate_limit_period_seconds")
	if err != nil {
		writeError(w, err)
		return
	}
	var template *string
	if req.Template != nil {
		if err := json.Unmarshal(req.Template, &template); err != nil {
			writeError(w, apiErr(http.StatusUnprocessableEntity,
				"Failed to deserialize the JSON body into the target type: template: %v", err))
			return
		}
	}
	var pinned any
	if req.PinnedParams != nil {
		v, err := util.DecodeJSON(req.PinnedParams)
		if err != nil {
			writeError(w, apiErr(http.StatusUnprocessableEntity,
				"Failed to deserialize the JSON body into the target type: pinned_params: %v", err))
			return
		}
		pinned = v
	}

	ctx := r.Context()
	// The update-by-id CRUD functions take the binding row id; resolve it first.
	bindings, err := actions.ListAgentActions(ctx, s.db, id)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	var binding *models.AgentAction
	for i := range bindings {
		if bindings[i].ActionID == actionID {
			binding = &bindings[i]
			break
		}
	}
	if binding == nil {
		writeErrorOnly(w, http.StatusNotFound, "binding not found")
		return
	}

	if req.Alias != nil {
		if msg := s.actionAliasConflict(r, bindings, actionID, *req.Alias); msg != "" {
			writeErrorOnly(w, http.StatusConflict, msg)
			return
		}
		if _, err := actions.UpdateAgentActionAlias(ctx, s.db, binding.ID, *req.Alias); err != nil {
			writeBindingWriteError(w, err)
			return
		}
	}
	if agentRawPresent(req.RateLimitCount) || agentRawPresent(req.RateLimitPeriodSeconds) {
		if _, err := actions.UpdateAgentActionRateLimit(ctx, s.db, binding.ID, count, period); err != nil {
			writeErrorOnly(w, http.StatusInternalServerError, err.Error())
			return
		}
		s.rateLimiter.Forget(id, BindingAction, actionID)
	}
	if req.Template != nil {
		// Empty string clears like null, so the UI needn't send an explicit null.
		if _, err := actions.UpdateAgentActionTemplate(ctx, s.db, binding.ID, agentTrimmedNonEmpty(template)); err != nil {
			writeErrorOnly(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	if req.PinnedParams != nil {
		if m, isObj := pinned.(map[string]any); isObj && len(m) == 0 {
			pinned = nil
		}
		if _, err := actions.UpdateAgentActionPinnedParams(ctx, s.db, binding.ID, pinned); err != nil {
			writeErrorOnly(w, http.StatusInternalServerError, err.Error())
			return
		}
	}
	// Any of the above change what SKILL.md / config.json render.
	s.resyncAgentDir(r, id, nil)
	writeJSON(w, http.StatusOK, map[string]any{"updated": true})
}

func (s *Server) unbindAgentActionHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := agentPathIDs(w, r, "id", "action_id")
	if !ok {
		return
	}
	deleted, err := actions.UnbindAgentAction(r.Context(), s.db, ids[0], ids[1])
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !deleted {
		writeErrorOnly(w, http.StatusNotFound, "binding not found")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}

// ── Skill installations ──

func (s *Server) listAgentInstallationsHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := agentPathIDs(w, r, "id")
	if !ok {
		return
	}
	installs, err := crud.ListAgentInstallations(r.Context(), s.db, ids[0])
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"installations": installs})
}

func (s *Server) createAgentInstallationHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := agentPathIDs(w, r, "id")
	if !ok {
		return
	}
	agentID := ids[0]
	var req struct {
		FolderPath *string `json:"folder_path"`
	}
	if !agentDecode(w, r, &req) {
		return
	}
	if req.FolderPath == nil {
		agentMissingField(w, "folder_path")
		return
	}
	folder := strings.TrimSpace(*req.FolderPath)
	if folder == "" {
		writeErrorOnly(w, http.StatusBadRequest, "folder_path is required")
		return
	}
	ctx := r.Context()
	agent, err := crud.GetAgent(ctx, s.db, agentID)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if agent == nil {
		writeErrorOnly(w, http.StatusNotFound, "agent not found")
		return
	}
	// The slug is the skill name harnesses use.
	slug := agentdir.AgentSlug(agent.Name)

	// Render now so rendering failures surface to the user.
	skillMD, err := s.buildAgentAPISkill(r, agentID, nil)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Persist first, then write: a failed insert leaves no file behind.
	id, err := crud.UpsertAgentInstallation(ctx, s.db, agentID, folder)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	target, err := agentdir.InstallSkillTo(folder, slug, skillMD)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	_ = crud.TouchAgentInstallation(ctx, s.db, id)
	install, _ := crud.GetAgentInstallation(ctx, s.db, id)
	writeJSON(w, http.StatusCreated, map[string]any{"installation": install, "target": target})
}

func (s *Server) deleteAgentInstallationHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := agentPathIDs(w, r, "id", "inst_id")
	if !ok {
		return
	}
	agentID, instID := ids[0], ids[1]
	ctx := r.Context()
	// Grab the row before deleting so we can clean the file on disk.
	inst, err := crud.GetAgentInstallation(ctx, s.db, instID)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if inst == nil {
		writeErrorOnly(w, http.StatusNotFound, "installation not found")
		return
	}
	slug := "dstudio" // legacy dir fallback if the agent is gone
	if a, err := crud.GetAgent(ctx, s.db, agentID); err == nil && a != nil {
		slug = agentdir.AgentSlug(a.Name)
	}
	deleted, err := crud.DeleteAgentInstallation(ctx, s.db, instID)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if !deleted {
		writeErrorOnly(w, http.StatusNotFound, "installation not found")
		return
	}
	// Best-effort; the parent `<slug>/` dir is left for sibling tooling.
	_ = os.Remove(agentdir.InstallTargetPath(inst.FolderPath, slug))
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// ── Portable skill export ──

type agentExportRequest struct {
	OutputDir *string `json:"output_dir"`
	// Overrides the daemon URL written into the exported config.json / SKILL.md.
	DaemonURL  *string `json:"daemon_url"`
	Zip        bool    `json:"zip"`
	IncludeMCP bool    `json:"include_mcp"`
}

// agentExportTempDir is swappable in tests.
var agentExportTempDir = func() (string, error) { return os.MkdirTemp("", "") }

func (s *Server) exportAgentSkillPackageHandler(w http.ResponseWriter, r *http.Request) {
	ids, ok := agentPathIDs(w, r, "id")
	if !ok {
		return
	}
	id := ids[0]
	var body agentExportRequest
	if !agentDecode(w, r, &body) {
		return
	}
	ctx := r.Context()
	raw := util.GenerateBearerToken()
	label := fmt.Sprintf("export-%d", time.Now().Unix())
	if _, err := crud.CreateAgentToken(ctx, s.db, id, util.HashToken(raw), label); err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, "failed to create export token: "+err.Error())
		return
	}

	useTempdir := body.OutputDir == nil || strings.TrimSpace(*body.OutputDir) == ""
	var outputDir string
	if useTempdir {
		td, err := agentExportTempDir()
		if err != nil {
			writeErrorOnly(w, http.StatusInternalServerError, "failed to create temp dir: "+err.Error())
			return
		}
		// Like Rust's TempDir guard, the directory is removed when the handler returns.
		defer os.RemoveAll(td)
		outputDir = td
	} else {
		outputDir = agentdir.ExpandTildePub(*body.OutputDir)
	}

	daemonURL := s.daemonURL
	if body.DaemonURL != nil {
		if u := strings.TrimSpace(*body.DaemonURL); u != "" {
			daemonURL = strings.TrimRight(u, "/")
		}
	}

	opts := &agentdir.ExportOptions{IncludeMCP: body.IncludeMCP, Zip: body.Zip}
	pkgDir, zipPath, err := agentdir.ExportSkillPackage(ctx, s.db, s.dataDir, id, &raw, daemonURL, outputDir, opts)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if zipPath == nil {
		writeJSON(w, http.StatusOK, map[string]any{"package_dir": pkgDir})
		return
	}
	if useTempdir {
		data, err := os.ReadFile(*zipPath)
		if err != nil {
			writeErrorOnly(w, http.StatusInternalServerError, "failed to read zip: "+err.Error())
			return
		}
		_ = os.RemoveAll(pkgDir)
		_ = os.Remove(*zipPath)
		w.Header().Set("Content-Type", "application/zip")
		w.Header().Set("Content-Disposition", `attachment; filename="bundle.zip"`)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write(data)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"package_dir": pkgDir, "zip_path": *zipPath})
}
