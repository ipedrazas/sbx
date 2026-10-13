// Package postgres implements the Postgres connector: table or raw-query
// datasets with filter/search/sort pushdown.
package postgres

import (
	"context"
	"database/sql/driver"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/util"
)

type TableInfo struct {
	Schema string `json:"schema"`
	Name   string `json:"name"`
}

// Config mirrors PostgresConfig. Exactly one of Table / Query must be set.
type Config struct {
	ConnectionString string `json:"connection_string"`
	// If set, query this table directly.
	Table *string `json:"table"`
	// If set, use this raw SQL query.
	Query *string `json:"query"`
}

// UnmarshalJSON enforces the required connection_string like serde.
func (c *Config) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if _, ok := raw["connection_string"]; !ok {
		return errors.New("missing field `connection_string`")
	}
	type plain Config
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*c = Config(p)
	return nil
}

// ParseConfig decodes a merged datasource+dataset config map.
func ParseConfig(m map[string]any) (Config, error) {
	var c Config
	b, err := json.Marshal(m)
	if err != nil {
		return c, err
	}
	err = json.Unmarshal(b, &c)
	return c, err
}

// querier is the subset of *pgxpool.Pool the connector uses (faked in tests).
type querier interface {
	Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error)
	QueryRow(ctx context.Context, sql string, args ...any) pgx.Row
}

type Connector struct {
	connector.Base
	pool   querier
	closer func()
	config Config
}

var _ connector.Connector = (*Connector)(nil)

// New opens a small pool (max 3 conns) and verifies connectivity within 10s.
func New(ctx context.Context, cfg Config) (*Connector, error) {
	pc, err := pgxpool.ParseConfig(cfg.ConnectionString)
	if err != nil {
		return nil, connector.Connection("%v", err)
	}
	pc.MaxConns = 3
	pool, err := pgxpool.NewWithConfig(ctx, pc)
	if err != nil {
		return nil, connector.Connection("%v", err)
	}
	pctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := pool.Ping(pctx); err != nil {
		pool.Close()
		return nil, connector.Connection("%v", err)
	}
	return &Connector{pool: pool, closer: pool.Close, config: cfg}, nil
}

// Close releases the pool.
func (c *Connector) Close() {
	if c.closer != nil {
		c.closer()
	}
}

// ListTables lists user tables (excluding system schemas), paginated.
func (c *Connector) ListTables(ctx context.Context, limit, offset int64) ([]TableInfo, int64, error) {
	var total int64
	err := c.pool.QueryRow(ctx, "SELECT COUNT(*) FROM information_schema.tables "+
		"WHERE table_schema NOT IN ('pg_catalog', 'information_schema') "+
		"AND table_type = 'BASE TABLE'").Scan(&total)
	if err != nil {
		return nil, 0, connector.Introspection("%v", err)
	}
	rows, err := c.pool.Query(ctx, "SELECT table_schema, table_name FROM information_schema.tables "+
		"WHERE table_schema NOT IN ('pg_catalog', 'information_schema') "+
		"AND table_type = 'BASE TABLE' "+
		"ORDER BY table_schema, table_name "+
		"LIMIT $1 OFFSET $2", limit, offset)
	if err != nil {
		return nil, 0, connector.Introspection("%v", err)
	}
	defer rows.Close()
	tables := []TableInfo{}
	for rows.Next() {
		var t TableInfo
		if err := rows.Scan(&t.Schema, &t.Name); err != nil {
			return nil, 0, connector.Introspection("%v", err)
		}
		tables = append(tables, t)
	}
	if err := rows.Err(); err != nil {
		return nil, 0, connector.Introspection("%v", err)
	}
	return tables, total, nil
}

// IntrospectTable returns a table's columns, paginated.
func (c *Connector) IntrospectTable(ctx context.Context, table string, limit, offset int64) ([]connector.SchemaField, int64, error) {
	return introspectFromInformationSchema(ctx, c.pool, table, limit, offset)
}

func (c *Connector) baseQuery() (string, error) {
	switch {
	case c.config.Table != nil && c.config.Query == nil:
		return "SELECT * FROM " + quoteIdent(*c.config.Table), nil
	case c.config.Table == nil && c.config.Query != nil:
		return "SELECT * FROM (" + *c.config.Query + ") AS _dstudio_subq", nil
	}
	return "", connector.Config("exactly one of 'table' or 'query' must be set")
}

func (c *Connector) Introspect(ctx context.Context) (*connector.Schema, error) {
	base, err := c.baseQuery()
	if err != nil {
		return nil, err
	}
	rows, err := c.pool.Query(ctx, base+" LIMIT 1")
	if err != nil {
		return nil, connector.Introspection("%v", err)
	}
	hasRow := rows.Next()
	fds := rows.FieldDescriptions()
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, connector.Introspection("%v", err)
	}

	fields := []connector.SchemaField{}
	if hasRow {
		for _, fd := range fds {
			fields = append(fields, connector.SchemaField{Name: fd.Name, FieldType: pgTypeToFieldType(typeName(fd.DataTypeOID))})
		}
	} else if c.config.Table != nil {
		// No rows — fall back to information_schema (tables rarely have >500 cols).
		cols, _, err := introspectFromInformationSchema(ctx, c.pool, *c.config.Table, 500, 0)
		if err != nil {
			return nil, err
		}
		fields = cols
	}
	return &connector.Schema{Fields: fields}, nil
}

func (c *Connector) Query(ctx context.Context, params *connector.QueryParams) (*models.Envelope, error) {
	base, err := c.baseQuery()
	if err != nil {
		return nil, err
	}

	var searchCols []string
	if params.Search != nil {
		schema, err := c.Introspect(ctx)
		if err != nil {
			return nil, err
		}
		searchCols = searchableTextColumns(schema, params.SearchableFields)
	}
	where, binds := buildWhere(params.Filters, params.Search, searchCols)
	args := make([]any, len(binds))
	for i, b := range binds {
		args[i] = b
	}

	var total int64
	countSQL := fmt.Sprintf("SELECT COUNT(*) as cnt FROM (%s) AS _c%s", base, where)
	if err := c.pool.QueryRow(ctx, countSQL, args...).Scan(&total); err != nil {
		return nil, connector.Query("%v", err)
	}

	// ORDER BY goes between WHERE and LIMIT so the whole filtered set is ordered.
	dataSQL := fmt.Sprintf("%s%s%s LIMIT %d OFFSET %d", base, where, orderByClause(params.Sort), params.Limit, params.Offset)
	rows, err := c.pool.Query(ctx, dataSQL, args...)
	if err != nil {
		return nil, connector.Query("%v", err)
	}
	items, err := collectRows(rows)
	if err != nil {
		return nil, connector.Query("%v", err)
	}

	// In/NotIn can't be pushed down; apply them in memory.
	var inMem []models.DatasetFilter
	for _, f := range params.Filters {
		if f.Operator == models.OpIn || f.Operator == models.OpNotIn {
			inMem = append(inMem, f)
		}
	}
	items = connector.ApplyFiltersInMemory(items, inMem)

	if params.ItemID != nil {
		kept := items[:0]
		for _, it := range items {
			if v, ok := it["id"]; ok && util.ValueAsString(v) == *params.ItemID {
				kept = append(kept, it)
			}
		}
		items = kept
	}

	return &models.Envelope{
		Items: items,
		Meta:  models.EnvelopeMeta{Total: total, Limit: params.Limit, Offset: params.Offset},
	}, nil
}

// SortCapability: Postgres can ORDER BY any selected column.
func (c *Connector) SortCapability() connector.SortCapability {
	return connector.SortCapability{Kind: connector.SortAll}
}

func (c *Connector) TestConnection(ctx context.Context) error {
	var one int32
	if err := c.pool.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		return connector.Connection("%v", err)
	}
	return nil
}

// searchableTextColumns picks string-typed columns, restricted to allowed
// when non-nil (dataset-level searchable=false enforcement).
func searchableTextColumns(schema *connector.Schema, allowed []string) []string {
	var out []string
	for _, f := range schema.Fields {
		if f.FieldType != connector.FieldString {
			continue
		}
		if allowed != nil {
			found := false
			for _, a := range allowed {
				if a == f.Name {
					found = true
					break
				}
			}
			if !found {
				continue
			}
		}
		out = append(out, f.Name)
	}
	return out
}

// buildWhere translates filters (grouped by group_index; OR within a group
// whose first filter has logic "or", AND otherwise; groups AND-ed) and the
// search term into a WHERE clause with $n placeholders and string binds.
// In/NotIn filters are skipped (applied in memory). When search is set but
// searchCols is empty, FALSE is emitted so no rows leak through.
func buildWhere(filters []models.DatasetFilter, search *string, searchCols []string) (string, []string) {
	var binds []string
	type group struct {
		conds []string
		isOr  bool
	}
	groups := map[int32]*group{}
	for _, f := range filters {
		col := quoteIdent(f.FieldName)
		idx := len(binds) + 1
		val := util.ValueAsString(f.Value)
		var cond string
		switch f.Operator {
		case models.OpEquals:
			binds = append(binds, val)
			cond = fmt.Sprintf("%s = $%d", col, idx)
		case models.OpNotEquals:
			binds = append(binds, val)
			cond = fmt.Sprintf("%s != $%d", col, idx)
		case models.OpContains:
			binds = append(binds, "%"+val+"%")
			cond = fmt.Sprintf("%s::text ILIKE $%d", col, idx)
		case models.OpNotContains:
			binds = append(binds, "%"+val+"%")
			cond = fmt.Sprintf("%s::text NOT ILIKE $%d", col, idx)
		case models.OpGreaterThan:
			binds = append(binds, val)
			cond = fmt.Sprintf("%s > $%d", col, idx)
		case models.OpLessThan:
			binds = append(binds, val)
			cond = fmt.Sprintf("%s < $%d", col, idx)
		default:
			continue
		}
		g, ok := groups[f.GroupIndex]
		if !ok {
			g = &group{isOr: f.Logic == models.LogicOr}
			groups[f.GroupIndex] = g
		}
		g.conds = append(g.conds, cond)
	}

	keys := make([]int32, 0, len(groups))
	for k := range groups {
		keys = append(keys, k)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	var conditions []string
	for _, k := range keys {
		g := groups[k]
		if len(g.conds) == 1 {
			conditions = append(conditions, g.conds[0])
			continue
		}
		joiner := " AND "
		if g.isOr {
			joiner = " OR "
		}
		conditions = append(conditions, "("+strings.Join(g.conds, joiner)+")")
	}

	if search != nil {
		if len(searchCols) > 0 {
			idx := len(binds) + 1
			parts := make([]string, len(searchCols))
			for i, c := range searchCols {
				parts[i] = fmt.Sprintf("%s::text ILIKE $%d", quoteIdent(c), idx)
			}
			conditions = append(conditions, "("+strings.Join(parts, " OR ")+")")
			binds = append(binds, "%"+*search+"%")
		} else {
			// All text columns excluded — reject rather than return every row.
			conditions = append(conditions, "FALSE")
		}
	}

	if len(conditions) == 0 {
		return "", binds
	}
	return " WHERE " + strings.Join(conditions, " AND "), binds
}

func introspectFromInformationSchema(ctx context.Context, q querier, table string, limit, offset int64) ([]connector.SchemaField, int64, error) {
	var total int64
	if err := q.QueryRow(ctx, "SELECT COUNT(*) FROM information_schema.columns WHERE table_name = $1", table).Scan(&total); err != nil {
		return nil, 0, connector.Introspection("%v", err)
	}
	rows, err := q.Query(ctx, "SELECT column_name, data_type FROM information_schema.columns "+
		"WHERE table_name = $1 ORDER BY ordinal_position LIMIT $2 OFFSET $3", table, limit, offset)
	if err != nil {
		return nil, 0, connector.Introspection("%v", err)
	}
	defer rows.Close()
	fields := []connector.SchemaField{}
	for rows.Next() {
		var name, dataType string
		if err := rows.Scan(&name, &dataType); err != nil {
			return nil, 0, connector.Introspection("%v", err)
		}
		fields = append(fields, connector.SchemaField{Name: name, FieldType: pgTypeToFieldType(dataType)})
	}
	if err := rows.Err(); err != nil {
		return nil, 0, connector.Introspection("%v", err)
	}
	return fields, total, nil
}

var typeMap = pgtype.NewMap()

// typeName returns the Postgres type name (e.g. "int4") for an OID, or "".
func typeName(oid uint32) string {
	if t, ok := typeMap.TypeForOID(oid); ok {
		return t.Name
	}
	return ""
}

func pgTypeToFieldType(pgType string) connector.FieldType {
	switch strings.ToLower(pgType) {
	case "int2", "int4", "int8", "smallint", "integer", "bigint", "serial", "bigserial":
		return connector.FieldInteger
	case "float4", "float8", "real", "double precision", "numeric", "decimal":
		return connector.FieldFloat
	case "bool", "boolean":
		return connector.FieldBoolean
	case "timestamp", "timestamptz", "date", "time", "timetz", "timestamp with time zone", "timestamp without time zone":
		return connector.FieldDateTime
	case "json", "jsonb":
		return connector.FieldJSON
	}
	return connector.FieldString
}

func collectRows(rows pgx.Rows) ([]map[string]any, error) {
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		vals, err := rows.Values()
		if err != nil {
			return nil, err
		}
		items = append(items, rowToMap(rows.FieldDescriptions(), vals, rows.RawValues()))
	}
	return items, rows.Err()
}

func rowToMap(fds []pgconn.FieldDescription, vals []any, raw [][]byte) map[string]any {
	m := make(map[string]any, len(fds))
	for i, fd := range fds {
		var v any
		if i < len(vals) {
			var r []byte
			if i < len(raw) {
				r = raw[i]
			}
			v = convertValue(typeName(fd.DataTypeOID), vals[i], r)
		}
		m[fd.Name] = v
	}
	return m
}

// convertValue maps a decoded pgx value to JSON. json/jsonb are re-decoded
// from the raw bytes so large numbers survive.
func convertValue(pgType string, v any, raw []byte) any {
	if v == nil {
		return nil
	}
	switch pgType {
	case "json", "jsonb":
		if raw != nil {
			b := raw
			if pgType == "jsonb" && len(b) > 0 && b[0] == 1 {
				b = b[1:] // binary jsonb version byte
			}
			if d, err := util.DecodeJSON(b); err == nil {
				return d
			}
		}
		return v
	case "date":
		if t, ok := v.(time.Time); ok {
			return t.Format("2006-01-02")
		}
	case "timestamp":
		if t, ok := v.(time.Time); ok {
			return t.Format("2006-01-02T15:04:05.999999999")
		}
	case "bytea":
		return nil
	}
	switch t := v.(type) {
	case int16:
		return int64(t)
	case int32:
		return int64(t)
	case int64, bool, string:
		return t
	case float32:
		return finite(float64(t))
	case float64:
		return finite(t)
	case time.Time:
		return t.Format(time.RFC3339Nano)
	case [16]byte:
		return fmt.Sprintf("%x-%x-%x-%x-%x", t[0:4], t[4:6], t[6:8], t[8:10], t[10:16])
	case pgtype.Numeric:
		f, err := t.Float64Value()
		if err != nil || !f.Valid {
			return nil
		}
		return finite(f.Float64)
	case []any:
		out := make([]any, len(t))
		for i, e := range t {
			out[i] = convertValue("", e, nil)
		}
		return out
	case map[string]any:
		return t
	case fmt.Stringer:
		return t.String()
	case driver.Valuer:
		if dv, err := t.Value(); err == nil {
			if s, ok := dv.(string); ok {
				return s
			}
		}
	}
	return nil
}

func finite(f float64) any {
	if math.IsNaN(f) || math.IsInf(f, 0) {
		return nil
	}
	return f
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

// orderByClause builds " ORDER BY <col> <dir>" for a pushed-down sort. The
// column is identifier-quoted; only the direction keyword is interpolated.
func orderByClause(s *models.SortSpec) string {
	if s == nil {
		return ""
	}
	dir := "ASC"
	if s.Descending {
		dir = "DESC"
	}
	return " ORDER BY " + quoteIdent(s.Field) + " " + dir
}
