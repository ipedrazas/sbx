package server

import (
	"context"
	"database/sql"
	"encoding/json"
	"log/slog"
	"net/http"
	"strings"
	"unicode"

	"github.com/ipedrazas/sbx/internal/actions"
	"github.com/ipedrazas/sbx/internal/agentdir"
	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/connectors/postgres"
	"github.com/ipedrazas/sbx/internal/connectors/rest"
	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/entitycatalog"
	"github.com/ipedrazas/sbx/internal/factory"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/secretstore"
	"github.com/ipedrazas/sbx/internal/util"
)

const (
	dsDefaultPageLimit int64 = 100
	dsMaxPageLimit     int64 = 500
)

func (s *Server) registerDatasources(mux *http.ServeMux) {
	const p = "/manage/datasources"
	handle(mux, s.wrap(s.listDatasources), "GET "+p, "GET "+p+"/")
	handle(mux, s.wrap(s.createDatasource), "POST "+p, "POST "+p+"/")
	handle(mux, s.wrap(s.getDatasource), "GET "+p+"/{id}")
	handle(mux, s.wrap(s.updateDatasource), "PUT "+p+"/{id}", "POST "+p+"/{id}/update")
	handle(mux, s.wrap(s.deleteDatasource), "DELETE "+p+"/{id}", "POST "+p+"/{id}/delete")
	handle(mux, s.wrap(s.testDatasource), "POST "+p+"/{id}/test")
	handle(mux, s.wrap(s.listDatasourceTables), "GET "+p+"/{id}/tables")
	handle(mux, s.wrap(s.listDatasourceTableColumns), "GET "+p+"/{id}/tables/{table}/columns")
	handle(mux, s.wrap(s.listDatasourceEntities), "GET "+p+"/{id}/entities")
	handle(mux, s.wrap(s.restSample), "POST "+p+"/{id}/rest/sample")
	handle(mux, s.wrap(s.restSpecEndpoints), "GET "+p+"/{id}/rest/spec/endpoints")
	handle(mux, s.setDatasourceReadOnly, "POST "+p+"/{id}/read-only")
	handle(mux, s.setDatasourceReadWrite, "POST "+p+"/{id}/read-write")
}

// missingJSONField is the 422 serde reports for an absent required field.
func missingJSONField(name string) error {
	return apiErr(http.StatusUnprocessableEntity,
		"Failed to deserialize the JSON body into the target type: missing field `%s`", name)
}

// dsPagination parses ?limit=&offset= clamped to 1..=500 (default 100) and >= 0.
func dsPagination(r *http.Request) (int64, int64, error) {
	lim, err := queryInt64(r, "limit")
	if err != nil {
		return 0, 0, err
	}
	off, err := queryInt64(r, "offset")
	if err != nil {
		return 0, 0, err
	}
	limit := dsDefaultPageLimit
	if lim != nil {
		limit = *lim
	}
	limit = min(max(limit, 1), dsMaxPageLimit)
	offset := int64(0)
	if off != nil {
		offset = max(*off, 0)
	}
	return limit, offset, nil
}

// loadDatasource returns the datasource or a 404 "datasource not found".
func (s *Server) loadDatasource(ctx context.Context, id int64) (*models.Datasource, error) {
	ds, err := crud.GetDatasource(ctx, s.db, id)
	if err != nil {
		return nil, err
	}
	if ds == nil {
		return nil, errNotFound("datasource not found")
	}
	return ds, nil
}

func (s *Server) listDatasources(w http.ResponseWriter, r *http.Request) error {
	list, err := crud.ListDatasources(r.Context(), s.db)
	if err != nil {
		return err
	}
	if list == nil {
		list = []models.Datasource{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"datasources": list})
	return nil
}

type createDatasourceRequest struct {
	Name         *string         `json:"name"`
	DsType       *string         `json:"ds_type"`
	Config       json.RawMessage `json:"config"`
	ConnectionID *int64          `json:"connection_id"`
}

func (s *Server) createDatasource(w http.ResponseWriter, r *http.Request) error {
	var req createDatasourceRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	switch {
	case req.Name == nil:
		return missingJSONField("name")
	case req.DsType == nil:
		return missingJSONField("ds_type")
	case req.Config == nil:
		return missingJSONField("config")
	}
	cfg, err := util.DecodeJSON(req.Config)
	if err != nil {
		return apiErr(http.StatusUnprocessableEntity, "config: %v", err)
	}
	cfg, err = normalizeSecrets(*req.DsType, *req.Name, cfg)
	if err != nil {
		return err
	}
	cfgStr, err := util.MarshalJSON(cfg)
	if err != nil {
		return errInternal("%v", err)
	}
	id, err := crud.CreateDatasource(r.Context(), s.db, *req.Name, *req.DsType, string(cfgStr), req.ConnectionID)
	if err != nil {
		return err
	}

	if dsType := models.DatasourceType(*req.DsType); dsType.Valid() {
		seeded, err := actions.SeedPredefinedActions(r.Context(), s.db, id, dsType)
		if err == nil && seeded > 0 {
			slog.Info("seeded predefined actions", "count", seeded, "datasource", id)
		}
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
	return nil
}

// normalizeSecrets moves inline plaintext secrets into the secret store and
// rewrites the config to hold only refs. No-op for types without inline secrets.
func normalizeSecrets(dsType, name string, cfg any) (any, error) {
	switch dsType {
	case "rest":
		return normalizeRestSecrets(name, cfg)
	case "linear":
		out, err := moveLinearAPIKey(name, cfg)
		if err != nil {
			return nil, errInternal("%v", err)
		}
		return out, nil
	default:
		return cfg, nil
	}
}

// normalizeRestSecrets moves inline token/value/password into the secret store.
func normalizeRestSecrets(name string, cfg any) (any, error) {
	m, ok := cfg.(map[string]any)
	if !ok {
		return nil, errBadRequest("invalid type: expected struct RestDatasourceConfig")
	}
	parsed, err := rest.ParseDatasourceConfig(m)
	if err != nil {
		return nil, errBadRequest("%v", err)
	}
	if err := parsed.PersistInlineSecrets(name); err != nil {
		return nil, errBadRequest("%v", err)
	}
	b, err := json.Marshal(parsed)
	if err != nil {
		return nil, errInternal("%v", err)
	}
	out, err := util.DecodeJSON(b)
	if err != nil {
		return nil, errInternal("%v", err)
	}
	return out, nil
}

// moveLinearAPIKey moves an inline Linear `api_key` into the secret store,
// replacing it with `api_key_ref`. Shared by create/update and the startup
// migration. The `api_key` key is always removed when present.
func moveLinearAPIKey(name string, cfg any) (any, error) {
	obj, ok := cfg.(map[string]any)
	if !ok {
		return cfg, nil
	}
	raw, present := obj["api_key"]
	if !present {
		return cfg, nil
	}
	delete(obj, "api_key")
	plain, _ := raw.(string)
	if plain == "" {
		return obj, nil
	}
	key := "dstudio-linear-" + secretSlug(name) + "-apikey"
	if err := secretstore.Put(key, []byte(plain)); err != nil {
		return nil, err
	}
	obj["api_key_ref"] = key
	return obj, nil
}

// MigrateLinearAPIKeys is the startup self-heal: it relocates any plaintext
// Linear `api_key` left in a datasource config into the secret store,
// rewriting it to `api_key_ref`. Idempotent. Returns the number migrated.
func MigrateLinearAPIKeys(ctx context.Context, db *sql.DB) int {
	list, err := crud.ListDatasources(ctx, db)
	if err != nil {
		slog.Warn("Linear api_key migration: list_datasources failed", "err", err)
		return 0
	}
	migrated := 0
	for _, ds := range list {
		if !needsLinearKeyMigration(ds.DsType, ds.Config) {
			continue
		}
		cfg := make(map[string]any, len(ds.Config))
		for k, v := range ds.Config {
			cfg[k] = v
		}
		newCfg, err := moveLinearAPIKey(ds.Name, cfg)
		if err != nil {
			slog.Warn("Linear api_key migration: store failed", "datasource", ds.ID, "err", err)
			continue
		}
		cfgStr, err := util.MarshalJSON(newCfg)
		if err != nil {
			continue
		}
		ok, err := crud.UpdateDatasource(ctx, db, ds.ID, ds.Name, string(cfgStr), ds.ConnectionID)
		switch {
		case err != nil:
			slog.Warn("Linear api_key migration: db update failed", "datasource", ds.ID, "err", err)
		case ok:
			migrated++
			slog.Info("migrated Linear api_key into the secret store", "datasource", ds.ID)
		}
	}
	return migrated
}

// secretSlug builds a lowercase alphanumeric slug for secret-store keys.
func secretSlug(s string) string {
	var b strings.Builder
	for _, c := range s {
		if unicode.IsLetter(c) || unicode.IsNumber(c) {
			if c < unicode.MaxASCII {
				c = unicode.ToLower(c)
			}
			b.WriteRune(c)
		} else {
			b.WriteByte('-')
		}
	}
	return strings.Trim(b.String(), "-")
}

// needsLinearKeyMigration: Linear, no api_key_ref, and a non-empty string api_key.
func needsLinearKeyMigration(dsType models.DatasourceType, cfg map[string]any) bool {
	if dsType != models.DatasourceLinear {
		return false
	}
	if _, ok := cfg["api_key_ref"]; ok {
		return false
	}
	k, _ := cfg["api_key"].(string)
	return k != ""
}

func (s *Server) getDatasource(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt64(r, "id")
	if err != nil {
		return err
	}
	ds, err := s.loadDatasource(r.Context(), id)
	if err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, ds)
	return nil
}

type updateDatasourceRequest struct {
	Name         *string         `json:"name"`
	Config       json.RawMessage `json:"config"`
	ConnectionID *int64          `json:"connection_id"`
}

func (s *Server) updateDatasource(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt64(r, "id")
	if err != nil {
		return err
	}
	var req updateDatasourceRequest
	if err := decodeJSON(r, &req); err != nil {
		return err
	}
	if req.Name == nil {
		return missingJSONField("name")
	}
	if req.Config == nil {
		return missingJSONField("config")
	}
	existing, err := s.loadDatasource(r.Context(), id)
	if err != nil {
		return err
	}
	cfg, err := util.DecodeJSON(req.Config)
	if err != nil {
		return apiErr(http.StatusUnprocessableEntity, "config: %v", err)
	}
	cfg, err = normalizeSecrets(string(existing.DsType), *req.Name, cfg)
	if err != nil {
		return err
	}
	cfgStr, err := util.MarshalJSON(cfg)
	if err != nil {
		return errInternal("%v", err)
	}
	ok, err := crud.UpdateDatasource(r.Context(), s.db, id, *req.Name, string(cfgStr), req.ConnectionID)
	if err != nil {
		return err
	}
	if !ok {
		return errNotFound("datasource not found")
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": true})
	return nil
}

func (s *Server) deleteDatasource(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt64(r, "id")
	if err != nil {
		return err
	}
	// Collect secret refs before the delete; scrub them only once it succeeds
	// (a CONFLICT means dependent datasets still need them).
	refs := s.collectSecretRefs(r.Context(), id)
	ok, err := crud.DeleteDatasource(r.Context(), s.db, id)
	if err != nil {
		return err
	}
	if !ok {
		return errConflict("datasource has dependent datasets, delete them first")
	}
	for _, ref := range refs {
		if err := secretstore.Delete(ref); err != nil {
			slog.Warn("failed to remove secret from secret store", "ref", ref, "error", err)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	return nil
}

// collectSecretRefs returns every secret-store ref a datasource config points at.
func (s *Server) collectSecretRefs(ctx context.Context, id int64) []string {
	ds, err := crud.GetDatasource(ctx, s.db, id)
	if err != nil || ds == nil {
		return nil
	}
	switch ds.DsType {
	case models.DatasourceRest:
		cfg, err := rest.ParseDatasourceConfig(ds.Config)
		if err != nil {
			return nil
		}
		if ref := cfg.Auth.SecretRef(); ref != nil {
			return []string{*ref}
		}
	case models.DatasourceLinear:
		if ref, ok := ds.Config["api_key_ref"].(string); ok {
			return []string{ref}
		}
	}
	return nil
}

func (s *Server) testDatasource(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt64(r, "id")
	if err != nil {
		return err
	}
	ds, err := s.loadDatasource(r.Context(), id)
	if err != nil {
		return err
	}
	conn, err := factory.CreateConnector(r.Context(), s.db, ds)
	if err != nil {
		return err
	}
	defer factory.Close(conn)
	if err := conn.TestConnection(r.Context()); err != nil {
		return err
	}
	writeJSON(w, http.StatusOK, map[string]any{"status": "ok"})
	return nil
}

// openPostgres validates the datasource is Postgres and connects to it
// (constructed directly, like the Rust handler, so errors map to 400/502).
func (s *Server) openPostgres(ctx context.Context, id int64, what string) (*postgres.Connector, error) {
	ds, err := s.loadDatasource(ctx, id)
	if err != nil {
		return nil, err
	}
	if ds.DsType != models.DatasourcePostgres {
		return nil, errBadRequest("%s listing is only available for Postgres datasources", what)
	}
	cfg, err := postgres.ParseConfig(ds.Config)
	if err != nil {
		return nil, errBadRequest("%v", err)
	}
	conn, err := postgres.New(ctx, cfg)
	if err != nil {
		return nil, errBadGateway("%v", err)
	}
	return conn, nil
}

func (s *Server) listDatasourceTables(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt64(r, "id")
	if err != nil {
		return err
	}
	limit, offset, err := dsPagination(r)
	if err != nil {
		return err
	}
	conn, err := s.openPostgres(r.Context(), id, "table")
	if err != nil {
		return err
	}
	defer conn.Close()
	tables, total, err := conn.ListTables(r.Context(), limit, offset)
	if err != nil {
		return errBadGateway("%v", err)
	}
	if tables == nil {
		tables = []postgres.TableInfo{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"tables": tables, "total": total, "limit": limit, "offset": offset})
	return nil
}

func (s *Server) listDatasourceTableColumns(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt64(r, "id")
	if err != nil {
		return err
	}
	table := r.PathValue("table")
	limit, offset, err := dsPagination(r)
	if err != nil {
		return err
	}
	conn, err := s.openPostgres(r.Context(), id, "column")
	if err != nil {
		return err
	}
	defer conn.Close()
	cols, total, err := conn.IntrospectTable(r.Context(), table, limit, offset)
	if err != nil {
		return errBadGateway("%v", err)
	}
	if cols == nil {
		cols = []connector.SchemaField{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"columns": cols, "total": total, "limit": limit, "offset": offset})
	return nil
}

// listDatasourceEntities lists the entities a datasource exposes (empty for
// single-view types, so the GUI hides the selector).
func (s *Server) listDatasourceEntities(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt64(r, "id")
	if err != nil {
		return err
	}
	ds, err := s.loadDatasource(r.Context(), id)
	if err != nil {
		return err
	}
	ents := entitycatalog.Entities(ds.DsType)
	if ents == nil {
		ents = []entitycatalog.EntityDescriptor{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"entities": ents})
	return nil
}

// restSample fires one live call against a REST datasource with a candidate
// dataset config and returns a sample plus a schema inferred from the first item.
func (s *Server) restSample(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt64(r, "id")
	if err != nil {
		return err
	}
	var body map[string]any
	if err := decodeJSON(r, &body); err != nil {
		return err
	}
	sampleLimit := int64(5)
	if v, ok := body["sample_limit"]; ok {
		n, isNum := v.(json.Number)
		i, err := n.Int64()
		if !isNum || err != nil {
			return apiErr(http.StatusUnprocessableEntity,
				"Failed to deserialize the JSON body into the target type: sample_limit: invalid type")
		}
		sampleLimit = i
		delete(body, "sample_limit")
	}

	ds, err := s.loadDatasource(r.Context(), id)
	if err != nil {
		return err
	}
	if ds.DsType != models.DatasourceRest {
		return errBadRequest("sample is only available for REST datasources")
	}
	dcfg, err := rest.ParseDatasetConfig(body)
	if err != nil {
		return errBadRequest("%v", err)
	}
	b, err := json.Marshal(dcfg)
	if err != nil {
		return errInternal("%v", err)
	}
	sourceConfig := util.ParseJSONMap(string(b))
	dataset := &models.Dataset{
		Name:         "__sample__",
		DatasourceID: id,
		Mode:         models.ModePullThrough,
		SourceConfig: sourceConfig,
		Enabled:      true,
	}
	conn, err := factory.CreateConnectorForDataset(r.Context(), s.db, ds, dataset)
	if err != nil {
		return err
	}
	defer factory.Close(conn)

	params := connector.DefaultQueryParams()
	params.Limit = min(max(sampleLimit, 1), 50)
	env, err := conn.Query(r.Context(), &params)
	if err != nil {
		return err
	}
	schema := connector.Schema{Fields: []connector.SchemaField{}}
	if len(env.Items) > 0 {
		schema = rest.InferSchemaFromSample(env.Items[0])
	}
	items := env.Items
	if items == nil {
		items = []map[string]any{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"schema": schema, "sample": items, "total": env.Meta.Total})
	return nil
}

// restSpecEndpoints parses the datasource's stored OpenAPI spec and returns
// its operations, each with a pre-filled dataset config draft.
func (s *Server) restSpecEndpoints(w http.ResponseWriter, r *http.Request) error {
	id, err := pathInt64(r, "id")
	if err != nil {
		return err
	}
	ds, err := s.loadDatasource(r.Context(), id)
	if err != nil {
		return err
	}
	if ds.DsType != models.DatasourceRest {
		return errBadRequest("spec endpoints are only available for REST datasources")
	}
	cfg, err := rest.ParseDatasourceConfig(ds.Config)
	if err != nil {
		return errBadRequest("%v", err)
	}
	if cfg.Spec == nil {
		return errBadRequest("datasource has no OpenAPI spec configured")
	}
	eps, err := rest.ParseSpec(r.Context(), *cfg.Spec)
	if err != nil {
		return errBadGateway("%v", err)
	}
	if eps == nil {
		eps = []rest.SpecEndpoint{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"endpoints": eps, "total": len(eps)})
	return nil
}

func (s *Server) setDatasourceReadOnly(w http.ResponseWriter, r *http.Request) {
	s.toggleReadOnly(w, r, true)
}

func (s *Server) setDatasourceReadWrite(w http.ResponseWriter, r *http.Request) {
	s.toggleReadOnly(w, r, false)
}

func (s *Server) toggleReadOnly(w http.ResponseWriter, r *http.Request, readOnly bool) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	ok, err := crud.SetDatasourceReadOnly(r.Context(), s.db, id, readOnly)
	switch {
	case err != nil:
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
	case !ok:
		writeErrorOnly(w, http.StatusNotFound, "not found")
	default:
		s.resyncAgentsForDatasource(r.Context(), id)
		writeJSON(w, http.StatusOK, map[string]any{"updated": true, "read_only": readOnly})
	}
}

// resyncAgentsForDatasource rewrites the agent dir of every agent bound to a
// dataset backed by this datasource (best effort).
func (s *Server) resyncAgentsForDatasource(ctx context.Context, datasourceID int64) {
	datasets, err := crud.ListDatasets(ctx, s.db)
	if err != nil {
		return
	}
	for _, d := range datasets {
		if d.DatasourceID != datasourceID {
			continue
		}
		agentIDs, _ := crud.AgentsForDataset(ctx, s.db, d.ID)
		for _, aid := range agentIDs {
			_, _ = agentdir.SyncAgentDir(ctx, s.db, s.dataDir, aid, nil, s.daemonURL)
		}
	}
}
