package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"github.com/ipedrazas/sbx/internal/cache"
	"github.com/ipedrazas/sbx/internal/capabilities"
	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/exportdata"
	"github.com/ipedrazas/sbx/internal/factory"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/pii"
	"github.com/ipedrazas/sbx/internal/util"
)

// liveGlobalRowCap is the hard ceiling on every list/search. A dataset's
// max_rows_per_query may lower it but never raise it.
const liveGlobalRowCap = capabilities.GlobalRowCap

// liveDownloadRowCap bounds a full-dataset download (pull-through sources can
// be unbounded).
const liveDownloadRowCap int64 = 100_000

// liveRefreshPageSize is the page size used when paginating a whole dataset.
const liveRefreshPageSize int64 = 500

func (s *Server) registerLive(mux *http.ServeMux) {
	handle(mux, s.wrap(s.liveListDatasets), "GET /a/{agent_id}/datasets")
	handle(mux, s.wrap(s.liveList), "GET /a/{agent_id}/{alias}/list")
	handle(mux, s.wrap(s.liveGetItem), "GET /a/{agent_id}/{alias}/get/{item_id...}")
	handle(mux, s.wrap(s.liveSearch), "GET /a/{agent_id}/{alias}/search")
	handle(mux, s.wrap(s.liveSchema), "GET /a/{agent_id}/{alias}/schema")
}

// ── Auth ──

// liveCheckAgentAuth validates an agent token and verifies it belongs to agentID.
func (s *Server) liveCheckAgentAuth(r *http.Request, agentID int64) error {
	tok, ok := bearerToken(r)
	if !ok {
		return errUnauthorized("missing or invalid Authorization header")
	}
	tokenAgentID, err := crud.ValidateAgentToken(r.Context(), s.db, util.HashToken(tok))
	if err != nil {
		return errInternal("%s", err.Error())
	}
	if tokenAgentID == nil {
		return errUnauthorized("invalid token")
	}
	if *tokenAgentID != agentID {
		return errUnauthorized("token does not match agent")
	}
	return nil
}

// liveResolveAlias maps an alias to a dataset id for the agent.
func (s *Server) liveResolveAlias(ctx context.Context, agentID int64, alias string) (int64, error) {
	id, err := crud.ResolveAgentAlias(ctx, s.db, agentID, alias)
	if err != nil {
		return 0, errInternal("%s", err.Error())
	}
	if id == nil {
		return 0, errNotFound("dataset alias '%s' not found for this agent", alias)
	}
	return *id, nil
}

// liveAuthAndResolve parses the path, authenticates and resolves the alias.
func (s *Server) liveAuthAndResolve(r *http.Request) (agentID, datasetID int64, err error) {
	agentID, err = pathInt64(r, "agent_id")
	if err != nil {
		return 0, 0, err
	}
	if err = s.liveCheckAgentAuth(r, agentID); err != nil {
		return 0, 0, err
	}
	datasetID, err = s.liveResolveAlias(r.Context(), agentID, r.PathValue("alias"))
	return agentID, datasetID, err
}

// ── Endpoints ──

// liveFieldSchema renders the agent-facing description of the visible fields.
func liveFieldSchema(dataset *models.Dataset, dsType *models.DatasourceType, fields []models.DatasetField) []any {
	out := []any{}
	for i := range fields {
		f := &fields[i]
		if !f.Visible {
			continue
		}
		sortable := dataset != nil && dsType != nil && capabilities.FieldSortable(dataset, *dsType, f)
		out = append(out, map[string]any{
			"name":        f.DisplayName,
			"type":        f.FieldType,
			"description": f.Description,
			"searchable":  f.Searchable,
			"sortable":    sortable,
		})
	}
	return out
}

func liveEffectiveMaxRows(maxRows *int64) int64 {
	if maxRows == nil {
		return liveGlobalRowCap
	}
	return min(max(*maxRows, 1), liveGlobalRowCap)
}

// liveListDatasets lists every dataset bound to the agent.
func (s *Server) liveListDatasets(w http.ResponseWriter, r *http.Request) error {
	agentID, err := pathInt64(r, "agent_id")
	if err != nil {
		return err
	}
	if err := s.liveCheckAgentAuth(r, agentID); err != nil {
		return err
	}
	ctx := r.Context()
	bindings, err := crud.ListAgentDatasets(ctx, s.db, agentID)
	if err != nil {
		return errInternal("%s", err.Error())
	}

	datasets := []any{}
	for _, b := range bindings {
		dataset, err := crud.GetDataset(ctx, s.db, b.DatasetID)
		if err != nil {
			return err
		}
		fields, err := crud.GetDatasetFields(ctx, s.db, b.DatasetID)
		if err != nil {
			fields = nil
		}
		var dsType *models.DatasourceType
		if dataset != nil {
			ds, err := crud.GetDatasource(ctx, s.db, dataset.DatasourceID)
			if err != nil {
				return err
			}
			if ds != nil {
				t := ds.DsType
				dsType = &t
			}
		}

		entry := map[string]any{
			"alias":              b.Alias,
			"dataset_id":         b.DatasetID,
			"name":               nil,
			"mode":               nil,
			"description":        nil,
			"data_kind":          nil,
			"max_rows_per_query": nil,
			"schema":             liveFieldSchema(dataset, dsType, fields),
		}
		if dataset != nil {
			entry["name"] = dataset.Name
			entry["mode"] = string(dataset.Mode)
			entry["description"] = dataset.Description
			entry["data_kind"] = dataset.DataKind
			entry["max_rows_per_query"] = liveEffectiveMaxRows(dataset.MaxRowsPerQuery)
			if dataset.MaxPageBytes != nil {
				entry["max_page_bytes"] = *dataset.MaxPageBytes
			}
			if dsType != nil {
				entry["capabilities"] = capabilities.Compute(dataset, *dsType, fields).ToJSON()
			}
		}
		datasets = append(datasets, entry)
	}
	writeJSON(w, http.StatusOK, map[string]any{"datasets": datasets})
	return nil
}

// liveQueryOne returns the last value for key (later duplicates win).
func liveQueryOne(q url.Values, key string) *string {
	vs := q[key]
	if len(vs) == 0 {
		return nil
	}
	v := vs[len(vs)-1]
	return &v
}

// liveQueryInt parses an optional integer param, falling back to def.
func liveQueryInt(q url.Values, key string, def int64) int64 {
	if v := liveQueryOne(q, key); v != nil {
		if n, err := strconv.ParseInt(*v, 10, 64); err == nil {
			return n
		}
	}
	return def
}

// liveParseRequestFilters parses `?filter=FIELD:OP:VALUE` params. Each field
// must be visible (an agent must not probe hidden columns); each filter gets
// its own high group_index so they AND together and never merge into a
// configured OR group. Empty visibleFields (no schema yet) skips validation.
func liveParseRequestFilters(raw []string, visibleFields []string) ([]models.DatasetFilter, error) {
	out := make([]models.DatasetFilter, 0, len(raw))
	for i, r := range raw {
		f, err := connector.ParseAdhocFilter(r, int32(1000+i))
		if err != nil {
			return nil, errBadRequest("%s", err.Error())
		}
		if len(visibleFields) > 0 && !slices.Contains(visibleFields, f.FieldName) {
			return nil, errBadRequest("cannot filter on '%s' — not a visible field of this dataset", f.FieldName)
		}
		out = append(out, f)
	}
	return out, nil
}

// liveQueryArgs are the parsed list/search query parameters.
type liveQueryArgs struct {
	limit, offset int64
	fields        []string
	sort          *liveResolvedSort
	adhoc         []models.DatasetFilter
}

func liveParseQueryArgs(q url.Values, dc *liveDatasetContext) (*liveQueryArgs, error) {
	a := &liveQueryArgs{
		limit:  min(liveQueryInt(q, "limit", 50), dc.maxRows),
		offset: liveQueryInt(q, "offset", 0),
		fields: liveResolveSelectedFields(liveQueryOne(q, "fields"), dc.visibleFields),
		sort:   dc.resolveSort(liveQueryOne(q, "sort_by"), liveQueryOne(q, "sort_dir")),
	}
	adhoc, err := liveParseRequestFilters(q["filter"], dc.visibleFields)
	if err != nil {
		return nil, err
	}
	a.adhoc = adhoc
	return a, nil
}

func (a *liveQueryArgs) sortSpec() *models.SortSpec {
	if a.sort == nil {
		return nil
	}
	sp := a.sort.spec
	return &sp
}

func (s *Server) liveList(w http.ResponseWriter, r *http.Request) error {
	_, datasetID, err := s.liveAuthAndResolve(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	dc, err := s.liveLoadDatasetContext(ctx, datasetID)
	if err != nil {
		return err
	}
	defer dc.close()
	args, err := liveParseQueryArgs(r.URL.Query(), dc)
	if err != nil {
		return err
	}

	var env models.Envelope
	if dc.cacheBacked() {
		c, err := cache.Open(ctx, s.dataDir, datasetID)
		if err != nil {
			return errInternal("%s", err.Error())
		}
		defer c.Close()
		if err := dc.refreshIfStale(ctx, c); err != nil {
			return err
		}
		// Configured filters are baked into the cache at refresh time; only
		// the ad-hoc filters apply on read.
		if dc.searchQuery != nil {
			env, err = c.Search(ctx, *dc.searchQuery, dc.searchableFields, args.limit, args.offset, args.sortSpec(), args.adhoc)
		} else {
			env, err = c.GetItems(ctx, args.limit, args.offset, args.sortSpec(), args.adhoc)
		}
		if err != nil {
			return errInternal("%s", err.Error())
		}
	} else {
		// Configured AND ad-hoc filters both go to the connector.
		qp := &connector.QueryParams{
			Filters:          append(slices.Clone(dc.filters), args.adhoc...),
			Limit:            args.limit,
			Offset:           args.offset,
			Search:           dc.searchQuery,
			SearchableFields: dc.searchableFields,
			Sort:             args.sortSpec(),
		}
		res, err := dc.conn.Query(ctx, qp)
		if err != nil {
			return errBadGateway("%s", err.Error())
		}
		env = *res
	}

	liveEchoSort(&env.Meta, args.sort)
	env.Items = liveApplyFieldPolicies(env.Items, args.fields, dc.fieldPolicies)
	liveFinalizeEnvelope(w, &env, dc, args.fields)
	return nil
}

func (s *Server) liveGetItem(w http.ResponseWriter, r *http.Request) error {
	_, datasetID, err := s.liveAuthAndResolve(r)
	if err != nil {
		return err
	}
	itemID := r.PathValue("item_id")
	ctx := r.Context()
	dc, err := s.liveLoadDatasetContext(ctx, datasetID)
	if err != nil {
		return err
	}
	defer dc.close()
	effective := liveResolveSelectedFields(liveQueryOne(r.URL.Query(), "fields"), dc.visibleFields)

	var item map[string]any
	if dc.cacheBacked() {
		c, err := cache.Open(ctx, s.dataDir, datasetID)
		if err != nil {
			return errInternal("%s", err.Error())
		}
		defer c.Close()
		if err := dc.refreshIfStale(ctx, c); err != nil {
			return err
		}
		item, err = c.GetItem(ctx, itemID)
		if err != nil {
			return errInternal("%s", err.Error())
		}
		if item == nil {
			return errNotFound("item not found")
		}
	} else {
		qp := connector.DefaultQueryParams()
		qp.Filters = dc.filters
		qp.ItemID = &itemID
		env, err := dc.conn.Query(ctx, &qp)
		if err != nil {
			return errBadGateway("%s", err.Error())
		}
		if len(env.Items) == 0 {
			return errNotFound("item not found")
		}
		item = env.Items[0]
	}

	filtered := liveApplyFieldPolicies([]map[string]any{item}, effective, dc.fieldPolicies)
	value := filtered[0]
	if value == nil {
		value = map[string]any{}
	}
	var itemBytes int64
	if b, err := util.MarshalJSON(value); err == nil {
		itemBytes = int64(len(b))
	}
	if dc.maxPageBytes != nil && itemBytes > *dc.maxPageBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"error":            "response exceeds the configured size limit",
			"status":           413,
			"actual_bytes":     itemBytes,
			"limit_bytes":      *dc.maxPageBytes,
			"available_fields": liveNonNil(dc.visibleFields),
			"selected_fields":  liveNonNil(effective),
			"suggestions":      []string{"Use ?fields= to request only the fields you need"},
		})
		return nil
	}
	writeJSON(w, http.StatusOK, value)
	return nil
}

func (s *Server) liveSearch(w http.ResponseWriter, r *http.Request) error {
	_, datasetID, err := s.liveAuthAndResolve(r)
	if err != nil {
		return err
	}
	q := r.URL.Query()
	term := liveQueryOne(q, "q")
	if term == nil || *term == "" {
		return errBadRequest("missing required query parameter 'q'")
	}
	ctx := r.Context()
	dc, err := s.liveLoadDatasetContext(ctx, datasetID)
	if err != nil {
		return err
	}
	defer dc.close()
	args, err := liveParseQueryArgs(q, dc)
	if err != nil {
		return err
	}

	effectiveSearch := *term
	if dc.searchQuery != nil {
		effectiveSearch = *dc.searchQuery + " " + *term
	}

	var env models.Envelope
	if dc.cacheBacked() {
		c, err := cache.Open(ctx, s.dataDir, datasetID)
		if err != nil {
			return errInternal("%s", err.Error())
		}
		defer c.Close()
		if err := dc.refreshIfStale(ctx, c); err != nil {
			return err
		}
		env, err = c.Search(ctx, effectiveSearch, dc.searchableFields, args.limit, args.offset, args.sortSpec(), args.adhoc)
		if err != nil {
			return errInternal("%s", err.Error())
		}
	} else {
		qp := &connector.QueryParams{
			Filters:          append(slices.Clone(dc.filters), args.adhoc...),
			Limit:            args.limit,
			Offset:           args.offset,
			Search:           &effectiveSearch,
			SearchableFields: dc.searchableFields,
			Sort:             args.sortSpec(),
		}
		res, err := dc.conn.Query(ctx, qp)
		if err != nil {
			return errBadGateway("%s", err.Error())
		}
		env = *res
	}

	liveEchoSort(&env.Meta, args.sort)
	env.Items = liveApplyFieldPolicies(env.Items, args.fields, dc.fieldPolicies)
	liveFinalizeEnvelope(w, &env, dc, args.fields)
	return nil
}

func (s *Server) liveSchema(w http.ResponseWriter, r *http.Request) error {
	_, datasetID, err := s.liveAuthAndResolve(r)
	if err != nil {
		return err
	}
	ctx := r.Context()
	dataset, err := crud.GetDataset(ctx, s.db, datasetID)
	if err != nil {
		return err
	}
	if dataset == nil {
		return errNotFound("dataset not found")
	}
	ds, err := crud.GetDatasource(ctx, s.db, dataset.DatasourceID)
	if err != nil {
		return err
	}
	var dsType *models.DatasourceType
	if ds != nil {
		t := ds.DsType
		dsType = &t
	}
	fields, err := crud.GetDatasetFields(ctx, s.db, datasetID)
	if err != nil {
		return err
	}
	body := map[string]any{"fields": liveFieldSchema(dataset, dsType, fields)}
	if dsType != nil {
		body["capabilities"] = capabilities.Compute(dataset, *dsType, fields).ToJSON()
	}
	writeJSON(w, http.StatusOK, body)
	return nil
}

// ── Helpers ──

// liveSortField is a sortable column as (display_name, source_name).
type liveSortField struct{ display, source string }

// liveDatasetContext is everything a live request needs about a dataset.
type liveDatasetContext struct {
	mode            models.DatasetMode
	cacheTTLSeconds *int64
	filters         []models.DatasetFilter
	conn            connector.Connector
	visibleFields   []string
	// searchableFields is visible AND searchable; nil means no fields are
	// configured yet, so connectors fall back to their default.
	searchableFields []string
	// fieldPolicies maps source_name → non-none PII policy.
	fieldPolicies map[string]pii.Policy
	maxRows       int64
	maxPageBytes  *int64
	// searchQuery is source_config.search_query: scopes /list and prefixes /search.
	searchQuery *string
	// sortableFields are visible AND operator-sortable.
	sortableFields []liveSortField
}

func (dc *liveDatasetContext) close() { factory.Close(dc.conn) }

func (dc *liveDatasetContext) cacheBacked() bool {
	return dc.mode != models.ModePullThrough
}

// liveResolvedSort is a sort request resolved against the sortable set.
type liveResolvedSort struct {
	// spec is keyed by source_name.
	spec models.SortSpec
	// displayName is echoed back in meta.sorted_by.
	displayName string
}

func (dc *liveDatasetContext) resolveSort(sortBy, sortDir *string) *liveResolvedSort {
	return liveResolveSortInner(dc.mode, dc.sortableFields, dc.conn.SortCapability(), sortBy, sortDir)
}

// liveResolveSortInner returns nil (silent no-op) when there is no sort_by,
// the field isn't visible+sortable, or — pull-through only — the connector
// can't order by it. Cache-backed modes sort materialized rows, so any
// sortable column works there.
func liveResolveSortInner(mode models.DatasetMode, sortable []liveSortField, capability connector.SortCapability, sortBy, sortDir *string) *liveResolvedSort {
	if sortBy == nil {
		return nil
	}
	requested := strings.TrimSpace(*sortBy)
	if requested == "" {
		return nil
	}
	idx := slices.IndexFunc(sortable, func(f liveSortField) bool { return f.display == requested })
	if idx < 0 {
		return nil
	}
	f := sortable[idx]
	if mode == models.ModePullThrough && !capability.Allows(f.source) {
		return nil
	}
	desc := sortDir != nil && strings.EqualFold(*sortDir, "desc")
	return &liveResolvedSort{spec: models.SortSpec{Field: f.source, Descending: desc}, displayName: f.display}
}

// liveEchoSort stamps the honored sort onto the meta; absent when a no-op.
func liveEchoSort(meta *models.EnvelopeMeta, sort *liveResolvedSort) {
	if sort == nil {
		return
	}
	name := sort.displayName
	dir := "asc"
	if sort.spec.Descending {
		dir = "desc"
	}
	meta.SortedBy = &name
	meta.SortedDir = &dir
}

// liveLoadDatasetContext loads the dataset, its connector and field config.
// On success the caller must call dc.close().
func (s *Server) liveLoadDatasetContext(ctx context.Context, datasetID int64) (*liveDatasetContext, error) {
	dataset, err := crud.GetDataset(ctx, s.db, datasetID)
	if err != nil {
		return nil, err
	}
	if dataset == nil {
		return nil, errNotFound("dataset not found")
	}
	if !dataset.Enabled {
		return nil, errUnavailable("dataset is paused")
	}
	ds, err := crud.GetDatasource(ctx, s.db, dataset.DatasourceID)
	if err != nil {
		return nil, err
	}
	if ds == nil {
		return nil, errInternal("datasource not found")
	}
	conn, err := factory.CreateConnectorForDataset(ctx, s.db, ds, dataset)
	if err != nil {
		return nil, err
	}

	filters, err := crud.GetDatasetFilters(ctx, s.db, datasetID)
	if err != nil {
		filters = nil
	}
	fields, err := crud.GetDatasetFields(ctx, s.db, datasetID)
	if err != nil {
		fields = nil
	}

	dc := &liveDatasetContext{
		mode:            dataset.Mode,
		cacheTTLSeconds: dataset.CacheTTLSeconds,
		filters:         filters,
		conn:            conn,
		fieldPolicies:   liveBuildFieldPolicies(fields),
		maxRows:         liveEffectiveMaxRows(dataset.MaxRowsPerQuery),
		maxPageBytes:    dataset.MaxPageBytes,
	}
	if len(fields) > 0 {
		// Searchable ⊂ visible: a hidden field must never be searchable.
		dc.searchableFields = []string{}
	}
	for _, f := range fields {
		if !f.Visible {
			continue
		}
		dc.visibleFields = append(dc.visibleFields, f.SourceName)
		if f.Sortable {
			dc.sortableFields = append(dc.sortableFields, liveSortField{f.DisplayName, f.SourceName})
		}
		if f.Searchable {
			dc.searchableFields = append(dc.searchableFields, f.SourceName)
		}
	}

	// Hard gate: with require_pii_review on, the dataset must have been scanned.
	if dataset.RequirePiiReview && dataset.PiiScanAt == nil {
		factory.Close(conn)
		return nil, errForbidden("dataset requires a PII review before it can be served — run the PII scan in the admin UI")
	}

	if sq, ok := dataset.SourceConfig["search_query"].(string); ok && sq != "" {
		dc.searchQuery = &sq
	}
	return dc, nil
}

// pageQuery is the QueryParams for one page of a full-dataset walk.
func (dc *liveDatasetContext) pageQuery(offset int64) *connector.QueryParams {
	return &connector.QueryParams{
		Filters:          slices.Clone(dc.filters),
		Limit:            liveRefreshPageSize,
		Offset:           offset,
		Search:           dc.searchQuery,
		SearchableFields: dc.searchableFields,
	}
}

// liveCollectAllServedRows returns every served row of a dataset — the view an
// agent sees (filters, base search_query, visibility, PII policies), with no
// per-query cap, bounded by liveDownloadRowCap. Used by the management download.
func (s *Server) liveCollectAllServedRows(ctx context.Context, datasetID int64) ([]map[string]any, error) {
	dc, err := s.liveLoadDatasetContext(ctx, datasetID)
	if err != nil {
		return nil, err
	}
	defer dc.close()

	all := []map[string]any{}
	var offset int64
	truncated := false
	if dc.cacheBacked() {
		c, err := cache.Open(ctx, s.dataDir, datasetID)
		if err != nil {
			return nil, errInternal("%s", err.Error())
		}
		defer c.Close()
		if err := dc.refreshIfStale(ctx, c); err != nil {
			return nil, err
		}
		for {
			env, err := c.GetItems(ctx, liveRefreshPageSize, offset, nil, nil)
			if err != nil {
				return nil, errInternal("%s", err.Error())
			}
			all = append(all, env.Items...)
			if int64(len(env.Items)) < liveRefreshPageSize {
				break
			}
			if int64(len(all)) >= liveDownloadRowCap {
				truncated = true
				break
			}
			offset += liveRefreshPageSize
		}
	} else {
		for {
			env, err := dc.conn.Query(ctx, dc.pageQuery(offset))
			if err != nil {
				return nil, errBadGateway("%s", err.Error())
			}
			all = append(all, env.Items...)
			if int64(len(env.Items)) < liveRefreshPageSize || int64(len(all)) >= env.Meta.Total {
				break
			}
			if int64(len(all)) >= liveDownloadRowCap {
				truncated = true
				break
			}
			offset += liveRefreshPageSize
		}
	}
	if truncated {
		all = all[:min(int64(len(all)), liveDownloadRowCap)]
		slog.Warn("dataset download truncated at row cap", "dataset_id", datasetID, "cap", liveDownloadRowCap)
	}
	return liveApplyFieldPolicies(all, dc.visibleFields, dc.fieldPolicies), nil
}

// liveStreamServedRows prepares an incremental export of the served view.
// Pre-flight errors (missing/paused dataset, PII gate, cache open) are
// returned before anything is written; the returned writer then emits the
// body page by page (a mid-stream error truncates it). Unlike the per-query
// API it does NOT refresh a stale cache: a bulk export serves the snapshot.
func (s *Server) liveStreamServedRows(ctx context.Context, datasetID int64, format exportdata.Format) (func(w io.Writer) error, error) {
	dc, err := s.liveLoadDatasetContext(ctx, datasetID)
	if err != nil {
		return nil, err
	}
	var c *cache.DatasetCache
	if dc.cacheBacked() {
		c, err = cache.Open(ctx, s.dataDir, datasetID)
		if err != nil {
			dc.close()
			return nil, errInternal("%s", err.Error())
		}
	}

	return func(w io.Writer) error {
		defer dc.close()
		if c != nil {
			defer c.Close()
		}
		write := func(b []byte) error {
			_, err := w.Write(b)
			if f, ok := w.(http.Flusher); ok {
				f.Flush()
			}
			return err
		}
		var offset, produced int64
		columns := slices.Clone(dc.visibleFields)
		started, jsonFirst, truncated := false, true, false

		for {
			var env models.Envelope
			if c != nil {
				env, err = c.GetItems(ctx, liveRefreshPageSize, offset, nil, nil)
			} else {
				var res *models.Envelope
				res, err = dc.conn.Query(ctx, dc.pageQuery(offset))
				if res != nil {
					env = *res
				}
			}
			if err != nil {
				return err
			}
			total := env.Meta.Total
			pageLen := int64(len(env.Items))
			if pageLen == 0 {
				break
			}
			rows := liveApplyFieldPolicies(env.Items, dc.visibleFields, dc.fieldPolicies)

			switch format {
			case exportdata.CSV:
				if !started {
					if len(columns) == 0 {
						// No declared schema — derive columns from the first page.
						columns = exportdata.CSVUnionColumns(rows)
					}
					if len(columns) > 0 {
						b, err := exportdata.CSVHeader(columns)
						if err != nil {
							return err
						}
						if err := write(b); err != nil {
							return err
						}
					}
					started = true
				}
				if len(columns) > 0 {
					b, err := exportdata.CSVDataRows(columns, rows)
					if err != nil {
						return err
					}
					if err := write(b); err != nil {
						return err
					}
				}
			case exportdata.JSONL:
				b, err := exportdata.SerializeRows(rows, exportdata.JSONL)
				if err != nil {
					return err
				}
				if err := write(b); err != nil {
					return err
				}
			case exportdata.JSON:
				if !started {
					if err := write([]byte("[")); err != nil {
						return err
					}
					started = true
				}
				for _, row := range rows {
					var buf []byte
					if !jsonFirst {
						buf = append(buf, ',')
					}
					jsonFirst = false
					b, err := util.MarshalJSON(row)
					if err != nil {
						return err
					}
					if err := write(append(buf, b...)); err != nil {
						return err
					}
				}
			}

			produced += pageLen
			if pageLen < liveRefreshPageSize || (total > 0 && produced >= total) {
				break
			}
			if produced >= liveDownloadRowCap {
				truncated = true
				break
			}
			offset += liveRefreshPageSize
		}
		if truncated {
			slog.Warn("dataset download stream truncated at row cap", "dataset_id", datasetID, "cap", liveDownloadRowCap)
		}
		if format == exportdata.JSON {
			if started {
				return write([]byte("]"))
			}
			return write([]byte("[]"))
		}
		return nil
	}, nil
}

// refreshIfStale refetches every row from the connector and repopulates the
// cache when the dataset has a TTL and the cache is stale.
func (dc *liveDatasetContext) refreshIfStale(ctx context.Context, c *cache.DatasetCache) error {
	if dc.mode == models.ModeSynthetic {
		return nil
	}
	if dc.cacheTTLSeconds == nil || *dc.cacheTTLSeconds <= 0 {
		return nil
	}
	stale, err := c.IsStale(ctx, *dc.cacheTTLSeconds)
	if err != nil {
		return errInternal("%s", err.Error())
	}
	if !stale {
		return nil
	}

	var offset int64
	all := []map[string]any{}
	for {
		env, err := dc.conn.Query(ctx, dc.pageQuery(offset))
		if err != nil {
			return err
		}
		all = append(all, env.Items...)
		if int64(len(env.Items)) < liveRefreshPageSize || int64(len(all)) >= env.Meta.Total {
			break
		}
		offset += liveRefreshPageSize
	}
	if err := c.Clear(ctx); err != nil {
		return errInternal("%s", err.Error())
	}
	if err := c.StoreItems(ctx, all); err != nil {
		return errInternal("%s", err.Error())
	}
	return nil
}

// liveResolveSelectedFields intersects a comma-separated `?fields=` with the
// visible fields. Unknown names are silently dropped.
func liveResolveSelectedFields(param *string, visible []string) []string {
	if param == nil {
		return slices.Clone(visible)
	}
	var requested []string
	for _, p := range strings.Split(*param, ",") {
		if p = strings.TrimSpace(p); p != "" {
			requested = append(requested, p)
		}
	}
	if len(requested) == 0 {
		return slices.Clone(visible)
	}
	if len(visible) == 0 {
		return requested
	}
	out := []string{}
	for _, f := range visible {
		if slices.Contains(requested, f) {
			out = append(out, f)
		}
	}
	return out
}

func liveNonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}

// liveFinalizeEnvelope sets meta.response_bytes and enforces max_page_bytes,
// writing either the envelope or a 413 with actionable guidance.
func liveFinalizeEnvelope(w http.ResponseWriter, env *models.Envelope, dc *liveDatasetContext, effective []string) {
	if env.Items == nil {
		env.Items = []map[string]any{}
	}
	var itemsBytes int64
	if b, err := util.MarshalJSON(env.Items); err == nil {
		itemsBytes = int64(len(b))
	}
	env.Meta.ResponseBytes = &itemsBytes

	if dc.maxPageBytes != nil && itemsBytes > *dc.maxPageBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]any{
			"error":            "response exceeds the configured size limit",
			"status":           413,
			"actual_bytes":     itemsBytes,
			"limit_bytes":      *dc.maxPageBytes,
			"item_count":       len(env.Items),
			"available_fields": liveNonNil(dc.visibleFields),
			"selected_fields":  liveNonNil(effective),
			"suggestions": []string{
				"Use ?fields= to request only the fields you need",
				"Reduce ?limit= to fetch fewer rows per page",
				"Use ?offset= to paginate through results",
			},
		})
		return
	}
	writeJSON(w, http.StatusOK, env)
}

// liveBuildFieldPolicies maps source_name → PII policy, omitting `none`.
func liveBuildFieldPolicies(fields []models.DatasetField) map[string]pii.Policy {
	out := map[string]pii.Policy{}
	for _, f := range fields {
		if p := pii.PolicyFromString(f.PiiPolicy); p != pii.PolicyNone {
			out[f.SourceName] = p
		}
	}
	return out
}

// liveApplyFieldPolicies drops fields not in visible, then applies per-field
// PII policies. Empty visible means "no schema configured, pass through" —
// policies still apply.
func liveApplyFieldPolicies(items []map[string]any, visible []string, policies map[string]pii.Policy) []map[string]any {
	filterVis := len(visible) > 0
	if !filterVis && len(policies) == 0 {
		if items == nil {
			return []map[string]any{}
		}
		return items
	}
	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		if item == nil {
			out = append(out, nil)
			continue
		}
		m := make(map[string]any, len(item))
		for k, v := range item {
			if filterVis && !slices.Contains(visible, k) {
				continue
			}
			if p, ok := policies[k]; ok {
				nv, keep := pii.ApplyPolicy(v, p)
				if !keep {
					continue
				}
				v = nv
			}
			m[k] = v
		}
		out = append(out, m)
	}
	return out
}
