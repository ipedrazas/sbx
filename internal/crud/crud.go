// Package crud holds the SQL accessors for datasources, OAuth connections,
// datasets (fields, filters), agents, agent bindings, tokens and skill
// installations.
package crud

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"

	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/util"
)

func boolInt(b bool) int64 {
	if b {
		return 1
	}
	return 0
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

func lastID(res sql.Result, err error) (int64, error) {
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// scopesJSON renders a scopes list as a JSON array ("[]" for nil).
func scopesJSON(scopes []string) string {
	if scopes == nil {
		scopes = []string{}
	}
	return util.JSONString(scopes)
}

// ── Datasources ──

const datasourceColumns = "id, name, ds_type, config, connection_id, read_only, created_at, updated_at"

func scanDatasource(sc interface{ Scan(...any) error }) (models.Datasource, error) {
	var (
		d      models.Datasource
		dsType string
		config string
		connID sql.NullInt64
	)
	if err := sc.Scan(&d.ID, &d.Name, &dsType, &config, &connID, &d.ReadOnly, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return d, err
	}
	d.DsType = models.DatasourceType(dsType)
	if !d.DsType.Valid() {
		d.DsType = models.DatasourcePostgres
	}
	// Invalid JSON decodes to null (serde unwrap_or_default on Value).
	if v, err := util.DecodeJSON([]byte(config)); err == nil {
		if m, ok := v.(map[string]any); ok {
			d.Config = m
		}
	}
	d.ConnectionID = nullInt(connID)
	return d, nil
}

func ListDatasources(ctx context.Context, db *sql.DB) ([]models.Datasource, error) {
	rows, err := db.QueryContext(ctx, "SELECT "+datasourceColumns+" FROM datasources ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.Datasource{}
	for rows.Next() {
		d, err := scanDatasource(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func GetDatasource(ctx context.Context, db *sql.DB, id int64) (*models.Datasource, error) {
	d, err := scanDatasource(db.QueryRowContext(ctx, "SELECT "+datasourceColumns+" FROM datasources WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func SetDatasourceReadOnly(ctx context.Context, db *sql.DB, id int64, readOnly bool) (bool, error) {
	return affected(db.ExecContext(ctx,
		"UPDATE datasources SET read_only = ?, updated_at = datetime('now') WHERE id = ?", boolInt(readOnly), id))
}

func CreateDatasource(ctx context.Context, db *sql.DB, name, dsType, config string, connectionID *int64) (int64, error) {
	return lastID(db.ExecContext(ctx,
		"INSERT INTO datasources (name, ds_type, config, connection_id) VALUES (?, ?, ?, ?)",
		name, dsType, config, connectionID))
}

func UpdateDatasource(ctx context.Context, db *sql.DB, id int64, name, config string, connectionID *int64) (bool, error) {
	return affected(db.ExecContext(ctx,
		"UPDATE datasources SET name = ?, config = ?, connection_id = ?, updated_at = datetime('now') WHERE id = ?",
		name, config, connectionID, id))
}

// DeleteDatasource returns false (without deleting) when datasets still reference it.
func DeleteDatasource(ctx context.Context, db *sql.DB, id int64) (bool, error) {
	var count int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM datasets WHERE datasource_id = ?", id).Scan(&count); err != nil {
		return false, err
	}
	if count > 0 {
		return false, nil
	}
	return affected(db.ExecContext(ctx, "DELETE FROM datasources WHERE id = ?", id))
}

// ── OAuth Connections ──

const oauthColumns = "id, name, provider, client_id, client_secret, scopes, created_at, updated_at"

func scanOAuth(sc interface{ Scan(...any) error }) (models.OAuthConnection, error) {
	var (
		c      models.OAuthConnection
		secret sql.NullString
		scopes sql.NullString
	)
	if err := sc.Scan(&c.ID, &c.Name, &c.Provider, &c.ClientID, &secret, &scopes, &c.CreatedAt, &c.UpdatedAt); err != nil {
		return c, err
	}
	c.ClientSecret = nullStr(secret)
	// Malformed or legacy NULL decodes to an empty list; the daemon then
	// falls back to hardcoded per-type defaults.
	if err := json.Unmarshal([]byte(scopes.String), &c.Scopes); err != nil || c.Scopes == nil {
		c.Scopes = []string{}
	}
	return c, nil
}

func ListOAuthConnections(ctx context.Context, db *sql.DB) ([]models.OAuthConnection, error) {
	rows, err := db.QueryContext(ctx, "SELECT "+oauthColumns+" FROM oauth_connections ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.OAuthConnection{}
	for rows.Next() {
		c, err := scanOAuth(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func GetOAuthConnection(ctx context.Context, db *sql.DB, id int64) (*models.OAuthConnection, error) {
	c, err := scanOAuth(db.QueryRowContext(ctx, "SELECT "+oauthColumns+" FROM oauth_connections WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

func CreateOAuthConnection(ctx context.Context, db *sql.DB, name, provider, clientID string, clientSecret *string, scopes []string) (int64, error) {
	return lastID(db.ExecContext(ctx,
		"INSERT INTO oauth_connections (name, provider, client_id, client_secret, scopes) VALUES (?, ?, ?, ?, ?)",
		name, provider, clientID, clientSecret, scopesJSON(scopes)))
}

func UpdateOAuthConnection(ctx context.Context, db *sql.DB, id int64, name, clientID string, clientSecret *string, scopes []string) (bool, error) {
	return affected(db.ExecContext(ctx,
		"UPDATE oauth_connections SET name = ?, client_id = ?, client_secret = ?, scopes = ?, updated_at = datetime('now') WHERE id = ?",
		name, clientID, clientSecret, scopesJSON(scopes), id))
}

// UpdateOAuthConnectionScopes updates only the scopes, leaving credentials untouched.
func UpdateOAuthConnectionScopes(ctx context.Context, db *sql.DB, id int64, scopes []string) (bool, error) {
	return affected(db.ExecContext(ctx,
		"UPDATE oauth_connections SET scopes = ?, updated_at = datetime('now') WHERE id = ?",
		scopesJSON(scopes), id))
}

// DeleteOAuthConnection returns false (without deleting) when a datasource still uses it.
func DeleteOAuthConnection(ctx context.Context, db *sql.DB, id int64) (bool, error) {
	var inUse int64
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM datasources WHERE connection_id = ?", id).Scan(&inUse); err != nil {
		return false, err
	}
	if inUse > 0 {
		return false, nil
	}
	return affected(db.ExecContext(ctx, "DELETE FROM oauth_connections WHERE id = ?", id))
}

// ── Datasets ──

const datasetColumns = "id, name, datasource_id, mode, cache_ttl_seconds, source_config, " +
	"enabled, description, data_kind, max_rows_per_query, pii_scan_at, require_pii_review, " +
	"snapshot_at, synthetic_row_count, max_page_bytes, created_at, updated_at"

func scanDataset(sc interface{ Scan(...any) error }) (models.Dataset, error) {
	var (
		d                                          models.Dataset
		mode, sourceConfig                         string
		ttl, maxRows, synthRows, maxPageBytes      sql.NullInt64
		description, dataKind, piiScan, snapshotAt sql.NullString
	)
	if err := sc.Scan(&d.ID, &d.Name, &d.DatasourceID, &mode, &ttl, &sourceConfig, &d.Enabled,
		&description, &dataKind, &maxRows, &piiScan, &d.RequirePiiReview, &snapshotAt, &synthRows,
		&maxPageBytes, &d.CreatedAt, &d.UpdatedAt); err != nil {
		return d, err
	}
	d.Mode = models.DatasetMode(mode)
	if !d.Mode.Valid() {
		d.Mode = models.ModePullThrough
	}
	d.CacheTTLSeconds = nullInt(ttl)
	d.SourceConfig = util.ParseJSONMap(sourceConfig)
	d.Description = nullStr(description)
	d.DataKind = nullStr(dataKind)
	d.MaxRowsPerQuery = nullInt(maxRows)
	d.PiiScanAt = nullStr(piiScan)
	d.SnapshotAt = nullStr(snapshotAt)
	d.SyntheticRowCount = nullInt(synthRows)
	d.MaxPageBytes = nullInt(maxPageBytes)
	return d, nil
}

func ListDatasets(ctx context.Context, db *sql.DB) ([]models.Dataset, error) {
	rows, err := db.QueryContext(ctx, "SELECT "+datasetColumns+" FROM datasets ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.Dataset{}
	for rows.Next() {
		d, err := scanDataset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func GetDataset(ctx context.Context, db *sql.DB, id int64) (*models.Dataset, error) {
	d, err := scanDataset(db.QueryRowContext(ctx, "SELECT "+datasetColumns+" FROM datasets WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &d, nil
}

func SetDatasetEnabled(ctx context.Context, db *sql.DB, id int64, enabled bool) (bool, error) {
	return affected(db.ExecContext(ctx,
		"UPDATE datasets SET enabled = ?, updated_at = datetime('now') WHERE id = ?", boolInt(enabled), id))
}

func CreateDataset(ctx context.Context, db *sql.DB, name string, datasourceID int64, mode string,
	cacheTTLSeconds *int64, sourceConfig, description, dataKind *string, maxRowsPerQuery *int64) (int64, error) {
	return CreateDatasetFull(ctx, db, name, datasourceID, mode, cacheTTLSeconds, sourceConfig, description,
		dataKind, maxRowsPerQuery, nil, nil)
}

// CreateDatasetFull is CreateDataset plus synthetic_row_count and max_page_bytes.
// A nil sourceConfig is stored as "{}".
func CreateDatasetFull(ctx context.Context, db *sql.DB, name string, datasourceID int64, mode string,
	cacheTTLSeconds *int64, sourceConfig, description, dataKind *string, maxRowsPerQuery *int64,
	syntheticRowCount, maxPageBytes *int64) (int64, error) {
	config := "{}"
	if sourceConfig != nil {
		config = *sourceConfig
	}
	return lastID(db.ExecContext(ctx,
		"INSERT INTO datasets (name, datasource_id, mode, cache_ttl_seconds, source_config, description, data_kind, max_rows_per_query, synthetic_row_count, max_page_bytes) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
		name, datasourceID, mode, cacheTTLSeconds, config, description, dataKind, maxRowsPerQuery,
		syntheticRowCount, maxPageBytes))
}

// UpdateDataset updates a dataset. source_config is left untouched when sourceConfig is nil.
func UpdateDataset(ctx context.Context, db *sql.DB, id int64, name, mode string, cacheTTLSeconds *int64,
	description, dataKind *string, maxRowsPerQuery *int64, sourceConfig *string, maxPageBytes *int64) (bool, error) {
	if sourceConfig != nil {
		return affected(db.ExecContext(ctx,
			"UPDATE datasets SET name = ?, mode = ?, cache_ttl_seconds = ?, description = ?, data_kind = ?, max_rows_per_query = ?, source_config = ?, max_page_bytes = ?, updated_at = datetime('now') WHERE id = ?",
			name, mode, cacheTTLSeconds, description, dataKind, maxRowsPerQuery, *sourceConfig, maxPageBytes, id))
	}
	return affected(db.ExecContext(ctx,
		"UPDATE datasets SET name = ?, mode = ?, cache_ttl_seconds = ?, description = ?, data_kind = ?, max_rows_per_query = ?, max_page_bytes = ?, updated_at = datetime('now') WHERE id = ?",
		name, mode, cacheTTLSeconds, description, dataKind, maxRowsPerQuery, maxPageBytes, id))
}

func DeleteDataset(ctx context.Context, db *sql.DB, id int64) (bool, error) {
	return affected(db.ExecContext(ctx, "DELETE FROM datasets WHERE id = ?", id))
}

// DuplicateDataset clones the dataset row, fields and filters (not agent
// bindings or tokens). Returns nil when the source dataset does not exist.
func DuplicateDataset(ctx context.Context, db *sql.DB, id int64, newName *string) (*int64, error) {
	ds, err := GetDataset(ctx, db, id)
	if err != nil || ds == nil {
		return nil, err
	}
	displayName := ds.Name + " (copy)"
	if newName != nil {
		displayName = *newName
	}
	sourceConfig := util.JSONString(ds.SourceConfig)
	newID, err := CreateDataset(ctx, db, displayName, ds.DatasourceID, string(ds.Mode), ds.CacheTTLSeconds,
		&sourceConfig, ds.Description, ds.DataKind, ds.MaxRowsPerQuery)
	if err != nil {
		return nil, err
	}

	fields, err := GetDatasetFields(ctx, db, id)
	if err != nil {
		return nil, err
	}
	if len(fields) > 0 {
		for i := range fields {
			fields[i].ID = 0
			fields[i].DatasetID = newID
		}
		if err := SetDatasetFields(ctx, db, newID, fields); err != nil {
			return nil, err
		}
	}

	filters, err := GetDatasetFilters(ctx, db, id)
	if err != nil {
		return nil, err
	}
	if len(filters) > 0 {
		for i := range filters {
			filters[i].ID = 0
			filters[i].DatasetID = newID
		}
		if err := SetDatasetFilters(ctx, db, newID, filters); err != nil {
			return nil, err
		}
	}
	return &newID, nil
}

// ── Dataset Fields ──

func GetDatasetFields(ctx context.Context, db *sql.DB, datasetID int64) ([]models.DatasetField, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT id, dataset_id, source_name, display_name, field_type, description, visible, "+
			"searchable, sortable, pii_class, pii_policy, credential_detected "+
			"FROM dataset_fields WHERE dataset_id = ? ORDER BY id", datasetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.DatasetField{}
	for rows.Next() {
		var (
			f                   models.DatasetField
			description, piiCls sql.NullString
		)
		if err := rows.Scan(&f.ID, &f.DatasetID, &f.SourceName, &f.DisplayName, &f.FieldType, &description,
			&f.Visible, &f.Searchable, &f.Sortable, &piiCls, &f.PiiPolicy, &f.CredentialDetected); err != nil {
			return nil, err
		}
		f.Description = nullStr(description)
		f.PiiClass = nullStr(piiCls)
		out = append(out, f)
	}
	return out, rows.Err()
}

// SetDatasetFields replaces all fields of a dataset.
func SetDatasetFields(ctx context.Context, db *sql.DB, datasetID int64, fields []models.DatasetField) error {
	if _, err := db.ExecContext(ctx, "DELETE FROM dataset_fields WHERE dataset_id = ?", datasetID); err != nil {
		return err
	}
	for _, f := range fields {
		if _, err := db.ExecContext(ctx,
			"INSERT INTO dataset_fields "+
				"(dataset_id, source_name, display_name, field_type, description, visible, "+
				"searchable, sortable, pii_class, pii_policy, credential_detected) "+
				"VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)",
			datasetID, f.SourceName, f.DisplayName, f.FieldType, f.Description, boolInt(f.Visible),
			boolInt(f.Searchable), boolInt(f.Sortable), f.PiiClass, f.PiiPolicy, boolInt(f.CredentialDetected)); err != nil {
			return err
		}
	}
	return nil
}

// PIIScanResult is one field's PII scan outcome.
type PIIScanResult struct {
	SourceName         string
	PiiClass           *string
	CredentialDetected bool
}

// ApplyPIIScanResults persists classification + credential detection for the
// given fields and stamps datasets.pii_scan_at, atomically.
func ApplyPIIScanResults(ctx context.Context, db *sql.DB, datasetID int64, results []PIIScanResult) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, r := range results {
		if _, err := tx.ExecContext(ctx,
			"UPDATE dataset_fields SET pii_class = ?, credential_detected = ? WHERE dataset_id = ? AND source_name = ?",
			r.PiiClass, boolInt(r.CredentialDetected), datasetID, r.SourceName); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		"UPDATE datasets SET pii_scan_at = datetime('now'), updated_at = datetime('now') WHERE id = ?",
		datasetID); err != nil {
		return err
	}
	return tx.Commit()
}

func SetDatasetRequirePIIReview(ctx context.Context, db *sql.DB, datasetID int64, require bool) (bool, error) {
	return affected(db.ExecContext(ctx,
		"UPDATE datasets SET require_pii_review = ?, updated_at = datetime('now') WHERE id = ?",
		boolInt(require), datasetID))
}

func SetDatasetSnapshotAt(ctx context.Context, db *sql.DB, id int64) (bool, error) {
	return affected(db.ExecContext(ctx,
		"UPDATE datasets SET snapshot_at = datetime('now'), updated_at = datetime('now') WHERE id = ?", id))
}

// ── Dataset Filters ──

func GetDatasetFilters(ctx context.Context, db *sql.DB, datasetID int64) ([]models.DatasetFilter, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT id, dataset_id, field_name, operator, value, logic, group_index FROM dataset_filters WHERE dataset_id = ? ORDER BY group_index, id",
		datasetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.DatasetFilter{}
	for rows.Next() {
		var (
			f                models.DatasetFilter
			op, value, logic string
		)
		if err := rows.Scan(&f.ID, &f.DatasetID, &f.FieldName, &op, &value, &logic, &f.GroupIndex); err != nil {
			return nil, err
		}
		f.Operator = models.FilterOperator(op)
		if !f.Operator.Valid() {
			f.Operator = models.OpEquals
		}
		f.Logic = models.FilterLogic(logic)
		if f.Logic != models.LogicAnd && f.Logic != models.LogicOr {
			f.Logic = models.LogicAnd
		}
		// Stored as JSON; fall back to the raw string for legacy rows.
		if v, err := util.DecodeJSON([]byte(value)); err == nil {
			f.Value = v
		} else {
			f.Value = value
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// SetDatasetFilters replaces all filters of a dataset. Values are stored as JSON.
func SetDatasetFilters(ctx context.Context, db *sql.DB, datasetID int64, filters []models.DatasetFilter) error {
	if _, err := db.ExecContext(ctx, "DELETE FROM dataset_filters WHERE dataset_id = ?", datasetID); err != nil {
		return err
	}
	for _, f := range filters {
		op := string(f.Operator)
		if op == "" {
			op = string(models.OpEquals)
		}
		logic := string(f.Logic)
		if logic == "" {
			logic = string(models.LogicAnd)
		}
		if _, err := db.ExecContext(ctx,
			"INSERT INTO dataset_filters (dataset_id, field_name, operator, value, logic, group_index) VALUES (?, ?, ?, ?, ?, ?)",
			datasetID, f.FieldName, op, util.JSONString(f.Value), logic, f.GroupIndex); err != nil {
			return err
		}
	}
	return nil
}

// ── Agents ──

const agentColumns = "id, name, description, agent_type, owner, status, mission, created_at, updated_at"

func scanAgent(sc interface{ Scan(...any) error }) (models.Agent, error) {
	var (
		a                                            models.Agent
		description, agentType, owner, status, missn sql.NullString
	)
	if err := sc.Scan(&a.ID, &a.Name, &description, &agentType, &owner, &status, &missn, &a.CreatedAt, &a.UpdatedAt); err != nil {
		return a, err
	}
	a.Description = nullStr(description)
	a.AgentType = nullStr(agentType)
	a.Owner = nullStr(owner)
	a.Status = nullStr(status)
	a.Mission = nullStr(missn)
	return a, nil
}

func ListAgents(ctx context.Context, db *sql.DB) ([]models.Agent, error) {
	rows, err := db.QueryContext(ctx, "SELECT "+agentColumns+" FROM agents ORDER BY id")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.Agent{}
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func GetAgent(ctx context.Context, db *sql.DB, id int64) (*models.Agent, error) {
	a, err := scanAgent(db.QueryRowContext(ctx, "SELECT "+agentColumns+" FROM agents WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// AgentMeta holds optional descriptive agent fields surfaced by the UI only.
type AgentMeta struct {
	AgentType *string
	Owner     *string
	Status    *string
	Mission   *string
}

func CreateAgent(ctx context.Context, db *sql.DB, name string, description *string) (int64, error) {
	return CreateAgentWithMeta(ctx, db, name, description, AgentMeta{})
}

func CreateAgentWithMeta(ctx context.Context, db *sql.DB, name string, description *string, meta AgentMeta) (int64, error) {
	return lastID(db.ExecContext(ctx,
		"INSERT INTO agents (name, description, agent_type, owner, status, mission) VALUES (?, ?, ?, ?, ?, ?)",
		name, description, meta.AgentType, meta.Owner, meta.Status, meta.Mission))
}

// UpdateAgent updates name/description and clears the meta fields (as in Rust).
func UpdateAgent(ctx context.Context, db *sql.DB, id int64, name string, description *string) (bool, error) {
	return UpdateAgentWithMeta(ctx, db, id, name, description, AgentMeta{})
}

func UpdateAgentWithMeta(ctx context.Context, db *sql.DB, id int64, name string, description *string, meta AgentMeta) (bool, error) {
	return affected(db.ExecContext(ctx,
		"UPDATE agents SET name = ?, description = ?, agent_type = ?, owner = ?, status = ?, mission = ?, "+
			"updated_at = datetime('now') WHERE id = ?",
		name, description, meta.AgentType, meta.Owner, meta.Status, meta.Mission, id))
}

func DeleteAgent(ctx context.Context, db *sql.DB, id int64) (bool, error) {
	return affected(db.ExecContext(ctx, "DELETE FROM agents WHERE id = ?", id))
}

// ── Agent-Dataset Bindings ──

const agentDatasetColumns = "id, agent_id, dataset_id, alias, rate_limit_count, rate_limit_period_seconds"

func scanAgentDataset(sc interface{ Scan(...any) error }) (models.AgentDataset, error) {
	var (
		b             models.AgentDataset
		count, period sql.NullInt64
	)
	if err := sc.Scan(&b.ID, &b.AgentID, &b.DatasetID, &b.Alias, &count, &period); err != nil {
		return b, err
	}
	b.RateLimitCount = nullInt(count)
	b.RateLimitPeriodSeconds = nullInt(period)
	return b, nil
}

func ListAgentDatasets(ctx context.Context, db *sql.DB, agentID int64) ([]models.AgentDataset, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT "+agentDatasetColumns+" FROM agent_datasets WHERE agent_id = ? ORDER BY id", agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.AgentDataset{}
	for rows.Next() {
		b, err := scanAgentDataset(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	return out, rows.Err()
}

func BindAgentDataset(ctx context.Context, db *sql.DB, agentID, datasetID int64, alias string,
	rateLimitCount, rateLimitPeriodSeconds *int64) (int64, error) {
	return lastID(db.ExecContext(ctx,
		"INSERT INTO agent_datasets (agent_id, dataset_id, alias, rate_limit_count, rate_limit_period_seconds) VALUES (?, ?, ?, ?, ?)",
		agentID, datasetID, alias, rateLimitCount, rateLimitPeriodSeconds))
}

func UnbindAgentDataset(ctx context.Context, db *sql.DB, agentID, datasetID int64) (bool, error) {
	return affected(db.ExecContext(ctx,
		"DELETE FROM agent_datasets WHERE agent_id = ? AND dataset_id = ?", agentID, datasetID))
}

func UpdateAgentDatasetAlias(ctx context.Context, db *sql.DB, agentID, datasetID int64, alias string) (bool, error) {
	return affected(db.ExecContext(ctx,
		"UPDATE agent_datasets SET alias = ? WHERE agent_id = ? AND dataset_id = ?", alias, agentID, datasetID))
}

// UpdateAgentDatasetRateLimit sets the per-binding rate limit; nil clears it.
func UpdateAgentDatasetRateLimit(ctx context.Context, db *sql.DB, agentID, datasetID int64,
	rateLimitCount, rateLimitPeriodSeconds *int64) (bool, error) {
	return affected(db.ExecContext(ctx,
		"UPDATE agent_datasets SET rate_limit_count = ?, rate_limit_period_seconds = ? WHERE agent_id = ? AND dataset_id = ?",
		rateLimitCount, rateLimitPeriodSeconds, agentID, datasetID))
}

// GetAgentBinding fetches a binding by (agent_id, alias).
func GetAgentBinding(ctx context.Context, db *sql.DB, agentID int64, alias string) (*models.AgentDataset, error) {
	b, err := scanAgentDataset(db.QueryRowContext(ctx,
		"SELECT "+agentDatasetColumns+" FROM agent_datasets WHERE agent_id = ? AND alias = ?", agentID, alias))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &b, nil
}

// AgentsForDataset lists the IDs of agents bound to a dataset.
func AgentsForDataset(ctx context.Context, db *sql.DB, datasetID int64) ([]int64, error) {
	rows, err := db.QueryContext(ctx, "SELECT DISTINCT agent_id FROM agent_datasets WHERE dataset_id = ?", datasetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []int64{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// ResolveAgentAlias returns the dataset_id bound under alias, or nil.
func ResolveAgentAlias(ctx context.Context, db *sql.DB, agentID int64, alias string) (*int64, error) {
	var id int64
	err := db.QueryRowContext(ctx,
		"SELECT dataset_id FROM agent_datasets WHERE agent_id = ? AND alias = ?", agentID, alias).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// ── Agent Tokens ──

func GetAgentTokens(ctx context.Context, db *sql.DB, agentID int64) ([]models.AgentToken, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT id, agent_id, token_hash, label, force_simulate, created_at FROM agent_tokens WHERE agent_id = ? ORDER BY id",
		agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.AgentToken{}
	for rows.Next() {
		var (
			t  models.AgentToken
			fs int64
		)
		if err := rows.Scan(&t.ID, &t.AgentID, &t.TokenHash, &t.Label, &fs, &t.CreatedAt); err != nil {
			return nil, err
		}
		t.ForceSimulate = fs != 0
		out = append(out, t)
	}
	return out, rows.Err()
}

func CreateAgentToken(ctx context.Context, db *sql.DB, agentID int64, tokenHash, label string) (int64, error) {
	return lastID(db.ExecContext(ctx,
		"INSERT INTO agent_tokens (agent_id, token_hash, label) VALUES (?, ?, ?)", agentID, tokenHash, label))
}

func DeleteAgentToken(ctx context.Context, db *sql.DB, id int64) (bool, error) {
	return affected(db.ExecContext(ctx, "DELETE FROM agent_tokens WHERE id = ?", id))
}

// ValidateAgentToken returns the agent_id owning tokenHash, or nil.
func ValidateAgentToken(ctx context.Context, db *sql.DB, tokenHash string) (*int64, error) {
	var id int64
	err := db.QueryRowContext(ctx, "SELECT agent_id FROM agent_tokens WHERE token_hash = ?", tokenHash).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &id, nil
}

// ── Agent Skill Installations ──

const installationColumns = "id, agent_id, folder_path, installed_at, updated_at"

func scanInstallation(sc interface{ Scan(...any) error }) (models.AgentSkillInstallation, error) {
	var i models.AgentSkillInstallation
	err := sc.Scan(&i.ID, &i.AgentID, &i.FolderPath, &i.InstalledAt, &i.UpdatedAt)
	return i, err
}

func ListAgentInstallations(ctx context.Context, db *sql.DB, agentID int64) ([]models.AgentSkillInstallation, error) {
	rows, err := db.QueryContext(ctx,
		"SELECT "+installationColumns+" FROM agent_skill_installations WHERE agent_id = ? ORDER BY id", agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []models.AgentSkillInstallation{}
	for rows.Next() {
		i, err := scanInstallation(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, i)
	}
	return out, rows.Err()
}

// UpsertAgentInstallation inserts an installation, or bumps updated_at on an
// existing (agent_id, folder_path) row, returning the row id either way.
func UpsertAgentInstallation(ctx context.Context, db *sql.DB, agentID int64, folderPath string) (int64, error) {
	// RETURNING yields the id for both the insert and the conflict-update path,
	// avoiding SQLite's stale last_insert_rowid on upserts.
	var id int64
	err := db.QueryRowContext(ctx,
		"INSERT INTO agent_skill_installations (agent_id, folder_path) VALUES (?, ?) "+
			"ON CONFLICT(agent_id, folder_path) DO UPDATE SET updated_at = datetime('now') RETURNING id",
		agentID, folderPath).Scan(&id)
	return id, err
}

func GetAgentInstallation(ctx context.Context, db *sql.DB, id int64) (*models.AgentSkillInstallation, error) {
	i, err := scanInstallation(db.QueryRowContext(ctx,
		"SELECT "+installationColumns+" FROM agent_skill_installations WHERE id = ?", id))
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &i, nil
}

func TouchAgentInstallation(ctx context.Context, db *sql.DB, id int64) error {
	_, err := db.ExecContext(ctx, "UPDATE agent_skill_installations SET updated_at = datetime('now') WHERE id = ?", id)
	return err
}

func DeleteAgentInstallation(ctx context.Context, db *sql.DB, id int64) (bool, error) {
	return affected(db.ExecContext(ctx, "DELETE FROM agent_skill_installations WHERE id = ?", id))
}
