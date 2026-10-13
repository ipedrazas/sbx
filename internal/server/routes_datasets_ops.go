package server

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"

	"github.com/ipedrazas/sbx/internal/ai"
	"github.com/ipedrazas/sbx/internal/cache"
	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/connectors/fileupload"
	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/factory"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/pii"
	"github.com/ipedrazas/sbx/internal/secretstore"
	"github.com/ipedrazas/sbx/internal/synthetic"
	"github.com/ipedrazas/sbx/internal/util"
)

// ── Preview ──

type previewParams struct {
	Limit  *int64  `json:"limit"`
	Offset *int64  `json:"offset"`
	Q      *string `json:"q"`
	// Field (source_name) to order by; unknown fields are silently ignored.
	SortBy *string `json:"sort_by"`
	// `desc` (case-insensitive) sorts descending; anything else ascending.
	SortDir *string `json:"sort_dir"`
}

func (s *Server) previewDataset(w http.ResponseWriter, r *http.Request) {
	var p previewParams
	var err error
	if p.Limit, err = queryInt64(r, "limit"); err != nil {
		writeError(w, err)
		return
	}
	if p.Offset, err = queryInt64(r, "offset"); err != nil {
		writeError(w, err)
		return
	}
	p.Q = queryString(r, "q")
	p.SortBy = queryString(r, "sort_by")
	p.SortDir = queryString(r, "sort_dir")
	s.preview(w, r, p)
}

func (s *Server) previewDatasetPost(w http.ResponseWriter, r *http.Request) {
	var p previewParams
	if err := decodeJSON(r, &p); err != nil {
		writeError(w, err)
		return
	}
	s.preview(w, r, p)
}

func (s *Server) preview(w http.ResponseWriter, r *http.Request, p previewParams) {
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
		writeErrorOnly(w, http.StatusNotFound, "dataset not found")
		return
	}
	ds, err := crud.GetDatasource(ctx, s.db, dataset.DatasourceID)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ds == nil {
		writeErrorOnly(w, http.StatusInternalServerError, "datasource not found")
		return
	}

	fields, _ := crud.GetDatasetFields(ctx, s.db, id)
	visible := datasetsVisibleFields(fields)

	// Honour sort_by only when it names a real field (by source_name).
	var sort *models.SortSpec
	if p.SortBy != nil {
		if f := strings.TrimSpace(*p.SortBy); f != "" {
			for _, fd := range fields {
				if fd.SourceName == f {
					sort = &models.SortSpec{
						Field:      f,
						Descending: p.SortDir != nil && strings.EqualFold(*p.SortDir, "desc"),
					}
					break
				}
			}
		}
	}

	limit := int64(20)
	if p.Limit != nil {
		limit = min(*p.Limit, 100)
	}
	offset := int64(0)
	if p.Offset != nil {
		offset = *p.Offset
	}

	base := datasetsSearchQuery(dataset)
	var search *string
	switch {
	case base != nil && p.Q != nil:
		v := *base + " " + *p.Q
		search = &v
	case base != nil:
		search = base
	case p.Q != nil:
		search = p.Q
	}

	var env models.Envelope
	switch dataset.Mode {
	case models.ModeCached, models.ModeMirror, models.ModeSynthetic:
		c, err := cache.Open(ctx, s.dataDir, id)
		if err != nil {
			slog.Error("failed to open cache", "dataset_id", id, "err", err)
			writeErrorOnly(w, http.StatusInternalServerError, fmt.Sprintf("cache error: %v", err))
			return
		}
		defer c.Close()
		if search != nil {
			env, err = c.Search(ctx, *search, nil, limit, offset, sort, nil)
		} else {
			env, err = c.GetItems(ctx, limit, offset, sort, nil)
		}
		if err != nil {
			slog.Error("preview query failed", "dataset_id", id, "err", err)
			writeErrorOnly(w, http.StatusBadGateway, err.Error())
			return
		}
	default:
		// Pull-through: push the sort down; connectors that can't honour it
		// ignore it (silent no-op).
		conn, err := factory.CreateConnectorForDataset(ctx, s.db, ds, dataset)
		if err != nil {
			slog.Error("connector creation failed", "dataset_id", id, "err", err)
			datasetsConnectorError(w, err)
			return
		}
		defer factory.Close(conn)
		filters, _ := crud.GetDatasetFilters(ctx, s.db, id)
		qp := connector.QueryParams{Filters: filters, Limit: limit, Offset: offset, Search: search, Sort: sort}
		res, err := conn.Query(ctx, &qp)
		if err != nil {
			slog.Error("preview query failed", "dataset_id", id, "err", err)
			datasetsConnectorError(w, err)
			return
		}
		env = *res
	}

	// Apply the same visibility + PII-policy transform agents are served.
	env.Items = liveApplyFieldPolicies(env.Items, visible, liveBuildFieldPolicies(fields))
	writeJSON(w, http.StatusOK, env)
}

func datasetsVisibleFields(fields []models.DatasetField) []string {
	out := []string{}
	for _, f := range fields {
		if f.Visible {
			out = append(out, f.SourceName)
		}
	}
	return out
}

// datasetsSearchQuery returns source_config.search_query when non-empty.
func datasetsSearchQuery(d *models.Dataset) *string {
	if v, ok := d.SourceConfig["search_query"].(string); ok && v != "" {
		return &v
	}
	return nil
}

// ── Introspect ──

func (s *Server) introspectDataset(w http.ResponseWriter, r *http.Request) {
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
		writeErrorOnly(w, http.StatusNotFound, "dataset not found")
		return
	}
	if err := s.autoIntrospectFields(ctx, id, dataset.DatasourceID); err != nil {
		datasetsConnectorError(w, err)
		return
	}
	fields, _ := crud.GetDatasetFields(ctx, s.db, id)
	if fields == nil {
		fields = []models.DatasetField{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"fields": fields})
}

// ── AI assist: draft description ──

type draftDescriptionRequest struct {
	DatasourceID *int64  `json:"datasource_id"`
	SourceConfig any     `json:"source_config"`
	DatasetName  *string `json:"dataset_name"`
}

func (s *Server) draftDatasetDescription(w http.ResponseWriter, r *http.Request) {
	var req draftDescriptionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.DatasourceID == nil {
		writeError(w, missingField("datasource_id"))
		return
	}
	// AI assist is opt-in; the key is read from the secret store per call.
	rt := s.aiRuntime()
	if !rt.Enabled {
		writeErrorOnly(w, http.StatusPreconditionFailed, "AI assist is disabled — enable it in Settings first")
		return
	}
	apiKey := secretstore.GetAnthropicKey()
	if apiKey == "" {
		writeErrorOnly(w, http.StatusPreconditionFailed,
			"AI assist is enabled but no Anthropic API key is stored — set one in Settings")
		return
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

	// Throwaway dataset (id 0) to feed the connector factory; nothing is written.
	sc, _ := req.SourceConfig.(map[string]any)
	if sc == nil {
		sc = map[string]any{}
	}
	throwaway := &models.Dataset{
		DatasourceID: *req.DatasourceID,
		Mode:         models.ModePullThrough,
		SourceConfig: sc,
		Enabled:      true,
	}
	if req.DatasetName != nil {
		throwaway.Name = *req.DatasetName
	}
	conn, err := factory.CreateConnectorForDataset(ctx, s.db, ds, throwaway)
	if err != nil {
		datasetsConnectorError(w, err)
		return
	}
	defer factory.Close(conn)
	schema, err := conn.Introspect(ctx)
	if err != nil {
		datasetsConnectorError(w, err)
		return
	}
	// One sample row; nil keeps the draft working for empty upstreams.
	var sampleRow any
	qp := connector.DefaultQueryParams()
	qp.Limit = 1
	if env, err := conn.Query(ctx, &qp); err == nil && env != nil && len(env.Items) > 0 {
		sampleRow = env.Items[0]
	}

	dsType := string(ds.DsType)
	if dsType == "" {
		dsType = "unknown"
	}
	input := &ai.DraftDescriptionInput{
		ConnectorType: dsType,
		DatasetName:   req.DatasetName,
		Schema:        []ai.SchemaField{},
		SampleRow:     sampleRow,
	}
	if schema != nil {
		for _, f := range schema.Fields {
			input.Schema = append(input.Schema, ai.SchemaField{Name: f.Name, FieldType: f.FieldType.String(), Description: f.Description})
		}
	}
	draft, err := ai.DraftDescription(ctx, apiKey, rt.Model, input)
	if err != nil {
		writeErrorOnly(w, http.StatusBadGateway, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"description": draft.Description, "data_kind": draft.DataKind})
}

// ── PII scan ──

// piiScanSample is the number of rows sampled by the PII scan.
const piiScanSample int64 = 500

func (s *Server) piiScanDataset(w http.ResponseWriter, r *http.Request) {
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
		writeErrorOnly(w, http.StatusNotFound, "dataset not found")
		return
	}
	ds, err := crud.GetDatasource(ctx, s.db, dataset.DatasourceID)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ds == nil {
		writeErrorOnly(w, http.StatusInternalServerError, "datasource missing")
		return
	}
	fields, err := crud.GetDatasetFields(ctx, s.db, id)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(fields) == 0 {
		writeErrorOnly(w, http.StatusBadRequest, "dataset has no fields — run introspection first")
		return
	}
	conn, err := factory.CreateConnectorForDataset(ctx, s.db, ds, dataset)
	if err != nil {
		datasetsConnectorError(w, err)
		return
	}
	defer factory.Close(conn)

	// The scan uses the admin's full view: no visibility or searchable filter.
	qp := connector.DefaultQueryParams()
	qp.Limit = piiScanSample
	env, err := conn.Query(ctx, &qp)
	if err != nil {
		datasetsConnectorError(w, err)
		return
	}

	results := make([]crud.PIIScanResult, 0, len(fields))
	summary := map[string]any{}
	anyCredential := false
	for _, f := range fields {
		samples := []any{}
		for _, row := range env.Items {
			if v, ok := row[f.SourceName]; ok {
				samples = append(samples, v)
			}
		}
		out := pii.ScanField(f.SourceName, f.FieldType, samples)
		var class *string
		if out.PiiClass != nil {
			c := string(*out.PiiClass)
			class = &c
		}
		results = append(results, crud.PIIScanResult{SourceName: f.SourceName, PiiClass: class, CredentialDetected: out.CredentialDetected})
		if out.CredentialDetected {
			anyCredential = true
		}
		summary[f.SourceName] = map[string]any{
			"pii_class":           class,
			"credential_detected": out.CredentialDetected,
			"samples_considered":  len(samples),
		}
	}
	if err := crud.ApplyPIIScanResults(ctx, s.db, id, results); err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.resyncAgentsForDataset(ctx, id)
	writeJSON(w, http.StatusOK, map[string]any{
		"rows_scanned":        len(env.Items),
		"fields":              summary,
		"credential_detected": anyCredential,
	})
}

// ── Snapshot (mirror datasets) ──

// datasetsFetchAll pages through the connector (500 rows/page) until a short
// page or the reported total is reached.
func datasetsFetchAll(ctx context.Context, conn connector.Connector, filters []models.DatasetFilter) ([]map[string]any, error) {
	const pageSize int64 = 500
	all := []map[string]any{}
	var offset int64
	for {
		qp := connector.QueryParams{Filters: filters, Limit: pageSize, Offset: offset}
		env, err := conn.Query(ctx, &qp)
		if err != nil {
			return nil, err
		}
		fetched := int64(len(env.Items))
		all = append(all, env.Items...)
		if fetched < pageSize || int64(len(all)) >= env.Meta.Total {
			return all, nil
		}
		offset += pageSize
	}
}

// datasetsReplaceCache clears the dataset cache and stores items.
func (s *Server) datasetsReplaceCache(ctx context.Context, id int64, items []map[string]any) error {
	c, err := cache.Open(ctx, s.dataDir, id)
	if err != nil {
		return err
	}
	defer c.Close()
	if err := c.Clear(ctx); err != nil {
		return err
	}
	return c.StoreItems(ctx, items)
}

func (s *Server) snapshotDataset(w http.ResponseWriter, r *http.Request) {
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
		writeErrorOnly(w, http.StatusNotFound, "dataset not found")
		return
	}
	if dataset.Mode != models.ModeMirror {
		writeErrorOnly(w, http.StatusBadRequest, "snapshot is only available for mirror datasets")
		return
	}
	ds, err := crud.GetDatasource(ctx, s.db, dataset.DatasourceID)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ds == nil {
		writeErrorOnly(w, http.StatusInternalServerError, "datasource not found")
		return
	}
	conn, err := factory.CreateConnectorForDataset(ctx, s.db, ds, dataset)
	if err != nil {
		datasetsConnectorError(w, err)
		return
	}
	defer factory.Close(conn)
	filters, _ := crud.GetDatasetFilters(ctx, s.db, id)
	items, err := datasetsFetchAll(ctx, conn, filters)
	if err != nil {
		datasetsConnectorError(w, err)
		return
	}
	if err := s.datasetsReplaceCache(ctx, id, items); err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, _ = crud.SetDatasetSnapshotAt(ctx, s.db, id)
	s.resyncAgentsForDataset(ctx, id)
	writeJSON(w, http.StatusOK, map[string]any{"rows_captured": len(items)})
}

// ── Generate (synthetic datasets) ──

func (s *Server) generateSyntheticDataset(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	var body struct {
		RowCount *int64 `json:"row_count"`
	}
	if _, err := decodeOptionalJSON(r, &body); err != nil {
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
		writeErrorOnly(w, http.StatusNotFound, "dataset not found")
		return
	}
	if dataset.Mode != models.ModeSynthetic {
		writeErrorOnly(w, http.StatusBadRequest, "generate is only available for synthetic datasets")
		return
	}
	fields, err := crud.GetDatasetFields(ctx, s.db, id)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if len(fields) == 0 {
		writeErrorOnly(w, http.StatusBadRequest, "dataset has no fields — run introspection first")
		return
	}
	count := int64(500)
	switch {
	case body.RowCount != nil:
		count = *body.RowCount
	case dataset.SyntheticRowCount != nil:
		count = *dataset.SyntheticRowCount
	}
	ds, err := crud.GetDatasource(ctx, s.db, dataset.DatasourceID)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ds == nil {
		writeErrorOnly(w, http.StatusInternalServerError, "datasource not found")
		return
	}

	var items []map[string]any
	if conn, err := factory.CreateConnectorForDataset(ctx, s.db, ds, dataset); err == nil {
		rows, ok := conn.GenerateSyntheticRows(fields, count)
		factory.Close(conn)
		if ok {
			items = rows
		}
	}
	if items == nil {
		items = synthetic.GenerateRows(fields, count)
	}

	if err := s.datasetsReplaceCache(ctx, id, items); err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	_, _ = crud.SetDatasetSnapshotAt(ctx, s.db, id)
	s.resyncAgentsForDataset(ctx, id)
	writeJSON(w, http.StatusOK, map[string]any{"rows_generated": len(items)})
}

// ── File upload (file_upload datasources) ──

// maxUploadBytes caps the decoded upload size. (The request body itself is
// also bounded by the 2MB JSON body limit, as with axum's default.)
const maxUploadBytes = 10 * 1024 * 1024

func (s *Server) uploadDatasetFile(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	var req struct {
		Filename *string `json:"filename"`
		Content  *string `json:"content"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.Filename == nil {
		writeError(w, missingField("filename"))
		return
	}
	if req.Content == nil {
		writeError(w, missingField("content"))
		return
	}
	ctx := r.Context()
	dataset, err := crud.GetDataset(ctx, s.db, id)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if dataset == nil {
		writeErrorOnly(w, http.StatusNotFound, "dataset not found")
		return
	}
	ds, err := crud.GetDatasource(ctx, s.db, dataset.DatasourceID)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ds == nil {
		writeErrorOnly(w, http.StatusInternalServerError, "datasource not found")
		return
	}
	if ds.DsType != models.DatasourceFileUpload {
		writeErrorOnly(w, http.StatusBadRequest, "upload is only available for file_upload datasources")
		return
	}
	data, err := base64.StdEncoding.DecodeString(*req.Content)
	if err != nil {
		writeErrorOnly(w, http.StatusBadRequest, fmt.Sprintf("invalid base64: %v", err))
		return
	}
	if len(data) > maxUploadBytes {
		writeErrorOnly(w, http.StatusBadRequest,
			fmt.Sprintf("file too large: %d bytes (max %d)", len(data), maxUploadBytes))
		return
	}

	ext := *req.Filename
	if i := strings.LastIndex(ext, "."); i >= 0 {
		ext = ext[i+1:]
	}
	ext = strings.ToLower(ext)

	var schema *connector.Schema
	var rows []any
	switch ext {
	case "csv":
		schema, rows, err = fileupload.ParseCSV(data)
	case "jsonl", "ndjson":
		schema, rows, err = fileupload.ParseJSONL(data)
	case "json":
		schema, rows, err = fileupload.ParseJSONArray(data)
	default:
		writeErrorOnly(w, http.StatusBadRequest, fmt.Sprintf(
			"unsupported file format: .%s (expected .csv, .jsonl, .ndjson, or .json)", ext))
		return
	}
	if err != nil {
		writeErrorOnly(w, http.StatusBadRequest, err.Error())
		return
	}

	items := datasetsEnsureRowIDs(rows)

	c, err := cache.Open(ctx, s.dataDir, id)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, fmt.Sprintf("cache error: %v", err))
		return
	}
	defer c.Close()
	if err := c.Clear(ctx); err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, fmt.Sprintf("cache clear error: %v", err))
		return
	}
	if err := c.StoreItems(ctx, items); err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, fmt.Sprintf("cache store error: %v", err))
		return
	}

	fields := schemaToDatasetFields(id, schema)
	if err := crud.SetDatasetFields(ctx, s.db, id, fields); err != nil {
		slog.Warn(fmt.Sprintf("Failed to set fields for dataset %d: %v", id, err))
	}
	_, _ = crud.SetDatasetSnapshotAt(ctx, s.db, id)
	s.resyncAgentsForDataset(ctx, id)
	writeJSON(w, http.StatusOK, map[string]any{"rows_loaded": len(items), "fields_detected": len(fields)})
}

// datasetsEnsureRowIDs gives every object row a non-empty string `id`
// (required by the cache): an existing non-null id is stringified, otherwise
// `row_<i>`. Non-object rows cannot be cached and are dropped.
func datasetsEnsureRowIDs(rows []any) []map[string]any {
	items := make([]map[string]any, 0, len(rows))
	for i, row := range rows {
		obj, ok := row.(map[string]any)
		if !ok {
			continue
		}
		if sv, ok := obj["id"].(string); !ok || sv == "" {
			idVal := ""
			if v, present := obj["id"]; present && v != nil {
				if b, err := util.MarshalJSON(v); err == nil {
					idVal = strings.Trim(string(b), `"`)
				}
			}
			if idVal == "" {
				idVal = "row_" + strconv.Itoa(i)
			}
			obj["id"] = idVal
		}
		items = append(items, obj)
	}
	return items
}

// seedCache fetches all rows from the connector and populates the cache for a
// newly created (or newly cached) dataset.
func (s *Server) seedCache(ctx context.Context, datasetID int64) error {
	dataset, err := crud.GetDataset(ctx, s.db, datasetID)
	if err != nil {
		return err
	}
	if dataset == nil {
		return errors.New("dataset not found")
	}
	ds, err := crud.GetDatasource(ctx, s.db, dataset.DatasourceID)
	if err != nil {
		return err
	}
	if ds == nil {
		return errors.New("datasource not found")
	}
	conn, err := factory.CreateConnectorForDataset(ctx, s.db, ds, dataset)
	if err != nil {
		return err
	}
	defer factory.Close(conn)
	filters, _ := crud.GetDatasetFilters(ctx, s.db, datasetID)
	c, err := cache.Open(ctx, s.dataDir, datasetID)
	if err != nil {
		return err
	}
	defer c.Close()
	items, err := datasetsFetchAll(ctx, conn, filters)
	if err != nil {
		return err
	}
	if err := c.Clear(ctx); err != nil {
		return err
	}
	if err := c.StoreItems(ctx, items); err != nil {
		return err
	}
	slog.Info(fmt.Sprintf("Seeded cache for dataset %d: %d items", datasetID, len(items)))
	return nil
}

// ── Stats ──

func (s *Server) getDatasetStats(w http.ResponseWriter, r *http.Request) {
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
	visibleCount := 0
	for _, f := range fields {
		if f.Visible {
			visibleCount++
		}
	}
	c, err := cache.Open(ctx, s.dataDir, id)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	defer c.Close()
	sample, err := c.GetItems(ctx, 100, 0, nil, nil)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	totalRows := sample.Meta.Total
	sampleCount := int64(len(sample.Items))
	if sampleCount == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"dataset_id":      id,
			"total_rows":      0,
			"sample_rows":     0,
			"avg_row_bytes":   0,
			"total_fields":    len(fields),
			"visible_fields":  visibleCount,
			"max_page_bytes":  dataset.MaxPageBytes,
			"estimated_pages": map[string]any{},
		})
		return
	}
	var total, minB, maxB int64
	for i, item := range sample.Items {
		var n int64
		if b, err := util.MarshalJSON(item); err == nil {
			n = int64(len(b))
		}
		total += n
		if i == 0 || n < minB {
			minB = n
		}
		if n > maxB {
			maxB = n
		}
	}
	avg := total / sampleCount
	writeJSON(w, http.StatusOK, map[string]any{
		"dataset_id":            id,
		"total_rows":            totalRows,
		"sample_rows":           sampleCount,
		"avg_row_bytes":         avg,
		"min_row_bytes":         minB,
		"max_row_bytes":         maxB,
		"estimated_total_bytes": avg * totalRows,
		"total_fields":          len(fields),
		"visible_fields":        visibleCount,
		"max_page_bytes":        dataset.MaxPageBytes,
		"estimated_page_bytes": map[string]any{
			"limit_10":  avg * min(10, totalRows),
			"limit_25":  avg * min(25, totalRows),
			"limit_50":  avg * min(50, totalRows),
			"limit_100": avg * min(100, totalRows),
		},
	})
}
