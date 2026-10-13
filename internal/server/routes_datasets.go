package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/ipedrazas/sbx/internal/agentdir"
	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/factory"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/pii"
	"github.com/ipedrazas/sbx/internal/util"
)

func (s *Server) registerDatasets(mux *http.ServeMux) {
	const p = "/manage/datasets"
	handle(mux, s.listDatasets, "GET "+p, "GET "+p+"/{$}")
	handle(mux, s.createDataset, "POST "+p, "POST "+p+"/{$}")
	handle(mux, s.draftDatasetDescription, "POST "+p+"/draft-description")
	handle(mux, s.getDataset, "GET "+p+"/{id}")
	handle(mux, s.updateDataset, "PUT "+p+"/{id}", "POST "+p+"/{id}/update")
	handle(mux, s.deleteDataset, "DELETE "+p+"/{id}", "POST "+p+"/{id}/delete")
	handle(mux, s.getDatasetFields, "GET "+p+"/{id}/fields")
	handle(mux, s.updateDatasetFields, "PUT "+p+"/{id}/fields", "POST "+p+"/{id}/fields")
	handle(mux, s.getDatasetFilters, "GET "+p+"/{id}/filters")
	handle(mux, s.updateDatasetFilters, "PUT "+p+"/{id}/filters", "POST "+p+"/{id}/filters")
	handle(mux, s.previewDataset, "GET "+p+"/{id}/preview")
	handle(mux, s.previewDatasetPost, "POST "+p+"/{id}/preview")
	handle(mux, s.introspectDataset, "POST "+p+"/{id}/introspect")
	handle(mux, s.getDatasetMetrics, "GET "+p+"/{id}/metrics")
	handle(mux, s.pauseDataset, "POST "+p+"/{id}/pause")
	handle(mux, s.resumeDataset, "POST "+p+"/{id}/resume")
	handle(mux, s.duplicateDataset, "POST "+p+"/{id}/duplicate")
	handle(mux, s.piiScanDataset, "POST "+p+"/{id}/pii-scan")
	handle(mux, s.setDatasetRequirePIIReview, "POST "+p+"/{id}/require-pii-review")
	handle(mux, s.snapshotDataset, "POST "+p+"/{id}/snapshot")
	handle(mux, s.generateSyntheticDataset, "POST "+p+"/{id}/generate")
	handle(mux, s.uploadDatasetFile, "POST "+p+"/{id}/upload")
	handle(mux, s.wrap(s.downloadDataset), "POST "+p+"/{id}/download")
	handle(mux, s.getDatasetStats, "GET "+p+"/{id}/stats")
}

// datasetsConnectorError renders a connector failure the way the dataset
// routes do: auth → 401 with error_code, upstream 4xx passed through, anything
// else 502.
func datasetsConnectorError(w http.ResponseWriter, err error) {
	var ce *connector.Error
	if errors.As(err, &ce) {
		switch {
		case ce.Kind == connector.ErrAuth:
			writeJSON(w, http.StatusUnauthorized, map[string]any{
				"error":      "authentication expired — re-authenticate this datasource",
				"error_code": "auth_expired",
			})
			return
		case ce.Kind == connector.ErrUpstreamHTTP && ce.Status >= 400 && ce.Status < 500:
			writeErrorOnly(w, ce.Status, ce.Error())
			return
		}
	}
	writeErrorOnly(w, http.StatusBadGateway, err.Error())
}

// missingField mimics axum's Json rejection for an absent required field.
func missingField(name string) *APIError {
	return apiErr(http.StatusUnprocessableEntity,
		"Failed to deserialize the JSON body into the target type: missing field `%s`", name)
}

// decodeOptionalJSON decodes the body into v when one is present; an empty
// body leaves v untouched (axum `Option<Json<T>>`). Reports whether a body
// was decoded.
func decodeOptionalJSON(r *http.Request, v any) (bool, error) {
	if r.ContentLength == 0 && r.Header.Get("Content-Type") == "" {
		return false, nil
	}
	err := decodeJSON(r, v)
	var ae *APIError
	if errors.As(err, &ae) && ae.Status == http.StatusBadRequest && strings.Contains(ae.Message, "EOF while parsing") {
		return false, nil
	}
	return err == nil, err
}

// resyncAgentsForDataset regenerates the agent directories of every agent
// bound to the dataset.
func (s *Server) resyncAgentsForDataset(ctx context.Context, datasetID int64) {
	agentIDs, _ := crud.AgentsForDataset(ctx, s.db, datasetID)
	for _, agentID := range agentIDs {
		tok := s.datasetsReadAgentToken(ctx, agentID)
		if _, err := agentdir.SyncAgentDir(ctx, s.db, s.dataDir, agentID, tok, s.daemonURL); err != nil {
			slog.Warn(fmt.Sprintf("Failed to resync agent dir for agent %d: %v", agentID, err))
		}
	}
}

// datasetsReadAgentToken reads the plaintext token from the agent's existing
// config.json (nil when absent or the placeholder).
func (s *Server) datasetsReadAgentToken(ctx context.Context, agentID int64) *string {
	agent, err := crud.GetAgent(ctx, s.db, agentID)
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

func (s *Server) pauseDataset(w http.ResponseWriter, r *http.Request) {
	s.setDatasetEnabled(w, r, false)
}

func (s *Server) resumeDataset(w http.ResponseWriter, r *http.Request) {
	s.setDatasetEnabled(w, r, true)
}

func (s *Server) setDatasetEnabled(w http.ResponseWriter, r *http.Request, enabled bool) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	ok, err := crud.SetDatasetEnabled(r.Context(), s.db, id, enabled)
	switch {
	case err != nil:
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
	case !ok:
		writeErrorOnly(w, http.StatusNotFound, "not found")
	default:
		s.resyncAgentsForDataset(r.Context(), id)
		writeJSON(w, http.StatusOK, map[string]any{"updated": true, "enabled": enabled})
	}
}

func (s *Server) getDatasetMetrics(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	writeJSON(w, http.StatusOK, s.metrics.Snapshot(id))
}

// datasetWithSource is a Dataset enriched with its datasource name.
type datasetWithSource struct {
	models.Dataset
	DatasourceName *string `json:"datasource_name,omitempty"`
}

func (s *Server) listDatasets(w http.ResponseWriter, r *http.Request) {
	datasets, err := crud.ListDatasets(r.Context(), s.db)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	// Enrich with datasource names; a lookup failure just skips enrichment.
	names := map[int64]string{}
	if dss, err := crud.ListDatasources(r.Context(), s.db); err == nil {
		for _, d := range dss {
			names[d.ID] = d.Name
		}
	}
	out := make([]datasetWithSource, 0, len(datasets))
	for _, d := range datasets {
		e := datasetWithSource{Dataset: d}
		if n, ok := names[d.DatasourceID]; ok {
			e.DatasourceName = &n
		}
		out = append(out, e)
	}
	writeJSON(w, http.StatusOK, map[string]any{"datasets": out})
}

type createDatasetRequest struct {
	Name              *string `json:"name"`
	DatasourceID      *int64  `json:"datasource_id"`
	Mode              *string `json:"mode"`
	CacheTTLSeconds   *int64  `json:"cache_ttl_seconds"`
	SourceConfig      any     `json:"source_config"`
	AutoIntrospect    *bool   `json:"auto_introspect"`
	Description       *string `json:"description"`
	DataKind          *string `json:"data_kind"`
	MaxRowsPerQuery   *int64  `json:"max_rows_per_query"`
	SyntheticRowCount *int64  `json:"synthetic_row_count"`
}

// jsonString serializes v like serde's Value::to_string (nil → nil).
func jsonStringPtr(v any) *string {
	if v == nil {
		return nil
	}
	b, err := util.MarshalJSON(v)
	if err != nil {
		return nil
	}
	s := string(b)
	return &s
}

func (s *Server) createDataset(w http.ResponseWriter, r *http.Request) {
	var req createDatasetRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.Name == nil {
		writeError(w, missingField("name"))
		return
	}
	if req.DatasourceID == nil {
		writeError(w, missingField("datasource_id"))
		return
	}
	mode := "pull_through"
	if req.Mode != nil {
		mode = *req.Mode
	}
	ctx := r.Context()

	ds, err := crud.GetDatasource(ctx, s.db, *req.DatasourceID)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ds == nil {
		writeErrorOnly(w, http.StatusBadRequest, "datasource not found")
		return
	}

	datasetID, err := crud.CreateDatasetFull(ctx, s.db, *req.Name, *req.DatasourceID, mode,
		req.CacheTTLSeconds, jsonStringPtr(req.SourceConfig), req.Description, req.DataKind,
		req.MaxRowsPerQuery, req.SyntheticRowCount, nil)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}

	if req.AutoIntrospect == nil || *req.AutoIntrospect {
		if err := s.autoIntrospectFields(ctx, datasetID, *req.DatasourceID); err != nil {
			slog.Warn(fmt.Sprintf("Auto-introspection failed for dataset %d: %v", datasetID, err))
		}
	}

	// Seed the cache in the background so the dataset is ready immediately.
	if mode == "cached" || mode == "mirror" {
		s.spawnSeedCache(datasetID, "Initial cache seed failed")
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": datasetID})
}

// datasetsBackground tracks in-flight seed goroutines (lets tests wait for them).
var datasetsBackground sync.WaitGroup

// spawnSeedCache seeds the dataset's cache in a goroutine scoped to s.ctx and
// tracked in s.syncCancels so deleting the dataset stops it.
func (s *Server) spawnSeedCache(datasetID int64, failMsg string) {
	ctx, cancel := context.WithCancel(s.ctx)
	s.syncMu.Lock()
	if prev, ok := s.syncCancels[datasetID]; ok {
		prev()
	}
	s.syncCancels[datasetID] = cancel
	s.syncMu.Unlock()
	datasetsBackground.Add(1)
	go func() {
		defer datasetsBackground.Done()
		// The map entry is left in place (a newer seed may have replaced it);
		// calling a spent cancel func from delete is a no-op.
		defer cancel()
		if err := s.seedCache(ctx, datasetID); err != nil {
			slog.Warn(fmt.Sprintf("%s for dataset %d: %v", failMsg, datasetID, err))
		}
	}()
}

// autoIntrospectFields introspects the connector and replaces the dataset's
// fields with the discovered schema.
func (s *Server) autoIntrospectFields(ctx context.Context, datasetID, datasourceID int64) error {
	ds, err := crud.GetDatasource(ctx, s.db, datasourceID)
	if err != nil {
		return connector.Config("%v", err)
	}
	if ds == nil {
		return connector.Config("datasource not found")
	}
	dataset, err := crud.GetDataset(ctx, s.db, datasetID)
	if err != nil {
		return connector.Config("%v", err)
	}
	if dataset == nil {
		return connector.Config("dataset not found")
	}
	conn, err := factory.CreateConnectorForDataset(ctx, s.db, ds, dataset)
	if err != nil {
		return err
	}
	defer factory.Close(conn)
	schema, err := conn.Introspect(ctx)
	if err != nil {
		return err
	}
	fields := schemaToDatasetFields(datasetID, schema)
	if err := crud.SetDatasetFields(ctx, s.db, datasetID, fields); err != nil {
		return connector.Config("%v", err)
	}
	return nil
}

func schemaToDatasetFields(datasetID int64, schema *connector.Schema) []models.DatasetField {
	fields := []models.DatasetField{}
	if schema == nil {
		return fields
	}
	for _, f := range schema.Fields {
		df := models.NewDatasetField()
		df.DatasetID = datasetID
		df.SourceName = f.Name
		df.DisplayName = f.Name
		df.FieldType = f.FieldType.String()
		df.Description = f.Description
		fields = append(fields, df)
	}
	return fields
}

func (s *Server) getDataset(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	ctx := r.Context()
	dataset, err := crud.GetDataset(ctx, s.db, id)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if dataset == nil {
		writeErrorOnly(w, http.StatusNotFound, "not found")
		return
	}
	fields, _ := crud.GetDatasetFields(ctx, s.db, id)
	filters, _ := crud.GetDatasetFilters(ctx, s.db, id)
	if fields == nil {
		fields = []models.DatasetField{}
	}
	if filters == nil {
		filters = []models.DatasetFilter{}
	}
	resp := map[string]any{"dataset": dataset, "fields": fields, "filters": filters}
	if ds, err := crud.GetDatasource(ctx, s.db, dataset.DatasourceID); err == nil && ds != nil {
		resp["datasource_type"] = ds.DsType
	}
	writeJSON(w, http.StatusOK, resp)
}

type updateDatasetRequest struct {
	Name            *string `json:"name"`
	Mode            *string `json:"mode"`
	CacheTTLSeconds *int64  `json:"cache_ttl_seconds"`
	Description     *string `json:"description"`
	DataKind        *string `json:"data_kind"`
	MaxRowsPerQuery *int64  `json:"max_rows_per_query"`
	SourceConfig    any     `json:"source_config"`
	MaxPageBytes    *int64  `json:"max_page_bytes"`
}

func (s *Server) updateDataset(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	var req updateDatasetRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.Name == nil {
		writeError(w, missingField("name"))
		return
	}
	if req.Mode == nil {
		writeError(w, missingField("mode"))
		return
	}
	ctx := r.Context()
	var oldMode *models.DatasetMode
	if d, err := crud.GetDataset(ctx, s.db, id); err == nil && d != nil {
		oldMode = &d.Mode
	}

	ok, err := crud.UpdateDataset(ctx, s.db, id, *req.Name, *req.Mode, req.CacheTTLSeconds,
		req.Description, req.DataKind, req.MaxRowsPerQuery, jsonStringPtr(req.SourceConfig), req.MaxPageBytes)
	switch {
	case err != nil:
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	case !ok:
		writeErrorOnly(w, http.StatusNotFound, "not found")
		return
	}
	s.resyncAgentsForDataset(ctx, id)

	wasCached := oldMode != nil && (*oldMode == models.ModeCached || *oldMode == models.ModeMirror)
	if (*req.Mode == "cached" || *req.Mode == "mirror") && !wasCached {
		s.spawnSeedCache(id, "Cache seed after mode change failed")
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": true})
}

func (s *Server) deleteDataset(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	// Stop any background sync.
	s.syncMu.Lock()
	if cancel, ok := s.syncCancels[id]; ok {
		cancel()
		delete(s.syncCancels, id)
	}
	s.syncMu.Unlock()

	_ = os.Remove(filepath.Join(s.dataDir, fmt.Sprintf("cache_%d.db", id)))

	ok, err := crud.DeleteDataset(r.Context(), s.db, id)
	switch {
	case err != nil:
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
	case !ok:
		writeErrorOnly(w, http.StatusNotFound, "not found")
	default:
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	}
}

// ── Fields ──

func (s *Server) getDatasetFields(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	fields, err := crud.GetDatasetFields(r.Context(), s.db, id)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if fields == nil {
		fields = []models.DatasetField{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"fields": fields})
}

func (s *Server) updateDatasetFields(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	var fields []models.DatasetField
	if err := decodeJSON(r, &fields); err != nil {
		writeError(w, err)
		return
	}
	// Reject unknown PII policies: they would be coerced to `none` at serve
	// time, silently disabling redaction. Fail loud instead of failing open.
	for _, f := range fields {
		if _, ok := pii.ParsePolicy(f.PiiPolicy); !ok {
			writeErrorOnly(w, http.StatusBadRequest, fmt.Sprintf(
				"field '%s' has invalid pii_policy '%s' — must be one of: %s",
				f.SourceName, f.PiiPolicy, strings.Join(pii.ValidPolicyValues[:], ", ")))
			return
		}
	}
	if err := crud.SetDatasetFields(r.Context(), s.db, id, fields); err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.resyncAgentsForDataset(r.Context(), id)
	writeJSON(w, http.StatusOK, map[string]any{"updated": true})
}

// ── Filters ──

func (s *Server) getDatasetFilters(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	filters, err := crud.GetDatasetFilters(r.Context(), s.db, id)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if filters == nil {
		filters = []models.DatasetFilter{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"filters": filters})
}

func (s *Server) updateDatasetFilters(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	var filters []models.DatasetFilter
	if err := decodeJSON(r, &filters); err != nil {
		writeError(w, err)
		return
	}
	if err := crud.SetDatasetFilters(r.Context(), s.db, id, filters); err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.resyncAgentsForDataset(r.Context(), id)
	writeJSON(w, http.StatusOK, map[string]any{"updated": true})
}

// ── Duplicate ──

func (s *Server) duplicateDataset(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	var body struct {
		Name *string `json:"name"`
	}
	if _, err := decodeOptionalJSON(r, &body); err != nil {
		writeError(w, err)
		return
	}
	newID, err := crud.DuplicateDataset(r.Context(), s.db, id, body.Name)
	switch {
	case err != nil:
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
	case newID == nil:
		writeErrorOnly(w, http.StatusNotFound, "dataset not found")
	default:
		writeJSON(w, http.StatusCreated, map[string]any{"id": *newID})
	}
}

// ── Require PII review ──

func (s *Server) setDatasetRequirePIIReview(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	var req struct {
		Require *bool `json:"require"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.Require == nil {
		writeError(w, missingField("require"))
		return
	}
	ok, err := crud.SetDatasetRequirePIIReview(r.Context(), s.db, id, *req.Require)
	switch {
	case err != nil:
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
	case !ok:
		writeErrorOnly(w, http.StatusNotFound, "dataset not found")
	default:
		s.resyncAgentsForDataset(r.Context(), id)
		writeJSON(w, http.StatusOK, map[string]any{"require_pii_review": *req.Require})
	}
}
