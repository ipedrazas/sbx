// Package cache implements the per-dataset SQLite cache (`cache_<id>.db`)
// used by cached-mode datasets.
package cache

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/ipedrazas/sbx/internal/db"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/util"
)

// jsonPath quotes key so names containing `.` or `[` aren't read as nested
// JSON paths; embedded double quotes are doubled.
func jsonPath(field string) string {
	return `$."` + strings.ReplaceAll(field, `"`, `""`) + `"`
}

// orderClause builds the ORDER BY fragment plus the JSON path to bind. The
// field travels as a bound json_extract path (never interpolated); ordering is
// type-aware and ties fall back to the primary key for a stable order.
func orderClause(s *models.SortSpec) (string, string) {
	dir := "ASC"
	if s.Descending {
		dir = "DESC"
	}
	return "ORDER BY json_extract(data, ?) " + dir + ", id ASC", jsonPath(s.Field)
}

// buildFilterSQL builds a boolean SQL fragment (no WHERE) plus ordered binds,
// mirroring connector.ApplyFiltersInMemory: grouped by GroupIndex, OR-ed within
// an "or" group, otherwise AND-ed; groups AND-ed together. Pushing the predicate
// into SQL keeps LIMIT/OFFSET pagination correct. ok is false with no filters.
func buildFilterSQL(filters []models.DatasetFilter) (clause string, binds []any, ok bool) {
	if len(filters) == 0 {
		return "", nil, false
	}
	groups := map[int32][]models.DatasetFilter{}
	var keys []int32
	for _, f := range filters {
		if _, seen := groups[f.GroupIndex]; !seen {
			keys = append(keys, f.GroupIndex)
		}
		groups[f.GroupIndex] = append(groups[f.GroupIndex], f)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	groupSQLs := make([]string, 0, len(keys))
	for _, k := range keys {
		g := groups[k]
		frags := make([]string, 0, len(g))
		for _, f := range g {
			frag, fb := filterFragment(f)
			frags = append(frags, frag)
			binds = append(binds, fb...)
		}
		joiner := " AND "
		if g[0].Logic == models.LogicOr {
			joiner = " OR "
		}
		groupSQLs = append(groupSQLs, "("+strings.Join(frags, joiner)+")")
	}
	return strings.Join(groupSQLs, " AND "), binds, true
}

// filterFragment renders one filter. Comparisons use CAST(... AS TEXT) so
// results match the string-based in-memory path, including missing-field
// semantics (ne/ncontains/nin are true when the field is absent).
func filterFragment(f models.DatasetFilter) (string, []any) {
	path := jsonPath(f.FieldName)
	const je = "CAST(json_extract(data, ?) AS TEXT)"
	val := util.ValueAsString(f.Value)

	switch f.Operator {
	case models.OpEquals:
		return "(" + je + " = ?)", []any{path, val}
	case models.OpNotEquals:
		// IFNULL → a missing field counts as "not equal".
		return "(IFNULL(" + je + " <> ?, 1) = 1)", []any{path, val}
	case models.OpContains:
		return "(INSTR(LOWER(" + je + "), LOWER(?)) > 0)", []any{path, val}
	case models.OpNotContains:
		return "(IFNULL(INSTR(LOWER(" + je + "), LOWER(?)), 0) = 0)", []any{path, val}
	case models.OpGreaterThan:
		return "(" + je + " > ?)", []any{path, val}
	case models.OpLessThan:
		return "(" + je + " < ?)", []any{path, val}
	case models.OpIn, models.OpNotIn:
		arr, _ := f.Value.([]any)
		if len(arr) == 0 {
			// In over a non-array/empty list never matches; NotIn always does.
			if f.Operator == models.OpNotIn {
				return "(1=1)", nil
			}
			return "(1=0)", nil
		}
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(arr)), ",")
		binds := make([]any, 0, len(arr)+1)
		binds = append(binds, path)
		for _, v := range arr {
			binds = append(binds, util.ValueAsString(v))
		}
		if f.Operator == models.OpNotIn {
			return "(IFNULL(" + je + " NOT IN (" + placeholders + "), 1) = 1)", binds
		}
		return "(" + je + " IN (" + placeholders + "))", binds
	}
	// Unknown operator: match nothing (the Rust enum is closed).
	return "(1=0)", nil
}

// DatasetCache is an open per-dataset cache database.
type DatasetCache struct {
	db   *sql.DB
	path string
}

// Open opens (creating if needed) `<cacheDir>/cache_<datasetID>.db`. WAL +
// synchronous=NORMAL avoids an fsync per commit during bulk writes.
func Open(ctx context.Context, cacheDir string, datasetID int64) (*DatasetCache, error) {
	path := filepath.Join(cacheDir, fmt.Sprintf("cache_%d.db", datasetID))
	pool, err := sql.Open("sqlite", db.DSN(path))
	if err != nil {
		return nil, err
	}
	pool.SetMaxOpenConns(3)
	for _, stmt := range []string{
		`CREATE TABLE IF NOT EXISTS cached_items (
                id TEXT PRIMARY KEY,
                data TEXT NOT NULL,
                cached_at TEXT NOT NULL DEFAULT (datetime('now'))
            )`,
		`CREATE TABLE IF NOT EXISTS cache_meta (
                key TEXT PRIMARY KEY,
                value TEXT NOT NULL
            )`,
	} {
		if _, err := pool.ExecContext(ctx, stmt); err != nil {
			pool.Close()
			return nil, err
		}
	}
	return &DatasetCache{db: pool, path: path}, nil
}

// Path returns the cache database file path.
func (c *DatasetCache) Path() string { return c.path }

// Close closes the underlying pool.
func (c *DatasetCache) Close() error { return c.db.Close() }

// StoreItems upserts items in a single transaction (one commit instead of one
// per row) and records the last sync time. Items without a usable "id" get a
// deterministic `_auto_<hash>` id derived from their JSON and position.
func (c *DatasetCache) StoreItems(ctx context.Context, items []map[string]any) error {
	tx, err := c.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck // no-op after commit

	stmt, err := tx.PrepareContext(ctx,
		"INSERT OR REPLACE INTO cached_items (id, data, cached_at) VALUES (?, ?, datetime('now'))")
	if err != nil {
		return err
	}
	defer stmt.Close()

	for idx, item := range items {
		b, err := util.MarshalJSON(item)
		if err != nil {
			return err
		}
		data := string(b)
		id, ok := extractID(item)
		if !ok {
			var h rustHasher
			h.writeStr(data)
			h.writeUsize(idx)
			id = fmt.Sprintf("_auto_%x", h.finish())
		}
		if _, err := stmt.ExecContext(ctx, id, data); err != nil {
			return err
		}
	}
	if _, err := tx.ExecContext(ctx,
		"INSERT OR REPLACE INTO cache_meta (key, value) VALUES ('last_sync', datetime('now'))"); err != nil {
		return err
	}
	return tx.Commit()
}

func extractID(item map[string]any) (string, bool) {
	switch v := item["id"].(type) {
	case string:
		return v, v != ""
	case json.Number, float64, float32, int, int32, int64, uint64:
		return util.ValueAsString(v), true
	}
	return "", false
}

func newEnvelope(items []map[string]any, total, limit, offset int64) models.Envelope {
	if items == nil {
		items = []map[string]any{}
	}
	return models.Envelope{Items: items, Meta: models.EnvelopeMeta{Total: total, Limit: limit, Offset: offset}}
}

// list runs the COUNT and the paged SELECT for a WHERE clause (may be empty).
func (c *DatasetCache) list(ctx context.Context, where string, binds []any, limit, offset int64, sortSpec *models.SortSpec) (models.Envelope, error) {
	var total int64
	if err := c.db.QueryRowContext(ctx, "SELECT COUNT(*) FROM cached_items "+where, binds...).Scan(&total); err != nil {
		return models.Envelope{}, err
	}

	// Default order keeps the historical "newest cached first" view.
	order := "ORDER BY cached_at DESC"
	args := append([]any{}, binds...)
	if sortSpec != nil {
		var path string
		order, path = orderClause(sortSpec)
		args = append(args, path)
	}
	args = append(args, limit, offset)

	rows, err := c.db.QueryContext(ctx, "SELECT data FROM cached_items "+where+" "+order+" LIMIT ? OFFSET ?", args...)
	if err != nil {
		return models.Envelope{}, err
	}
	defer rows.Close()
	items := []map[string]any{}
	for rows.Next() {
		var data string
		if err := rows.Scan(&data); err != nil {
			return models.Envelope{}, err
		}
		if v, err := util.DecodeJSON([]byte(data)); err == nil {
			if m, ok := v.(map[string]any); ok {
				items = append(items, m)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return models.Envelope{}, err
	}
	return newEnvelope(items, total, limit, offset), nil
}

// GetItems pages through the cache. Filters are pushed into SQL so COUNT and
// LIMIT/OFFSET both see the filtered set; sort orders the whole set first.
func (c *DatasetCache) GetItems(ctx context.Context, limit, offset int64, sortSpec *models.SortSpec, filters []models.DatasetFilter) (models.Envelope, error) {
	where := ""
	clause, binds, ok := buildFilterSQL(filters)
	if ok {
		where = "WHERE " + clause
	}
	return c.list(ctx, where, binds, limit, offset, sortSpec)
}

// GetItem returns the cached item with the given id, or nil when absent.
func (c *DatasetCache) GetItem(ctx context.Context, id string) (map[string]any, error) {
	var data string
	err := c.db.QueryRowContext(ctx, "SELECT data FROM cached_items WHERE id = ?", id).Scan(&data)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	v, err := util.DecodeJSON([]byte(data))
	if err != nil {
		return nil, nil
	}
	m, _ := v.(map[string]any)
	return m, nil
}

// Search finds items where query appears (SQL LIKE) in a searchable field.
//
//   - searchableFields == nil: match against the full JSON blob (legacy).
//   - non-nil but empty: every field is excluded; nothing matches.
//   - otherwise only values under those keys are considered (via json_each).
//
// Filters are AND-ed onto the search predicate.
func (c *DatasetCache) Search(ctx context.Context, query string, searchableFields []string, limit, offset int64, sortSpec *models.SortSpec, filters []models.DatasetFilter) (models.Envelope, error) {
	pattern := "%" + query + "%"
	if searchableFields != nil && len(searchableFields) == 0 {
		return newEnvelope(nil, 0, limit, offset), nil
	}

	var searchClause string
	var binds []any
	if searchableFields == nil {
		searchClause = "data LIKE ?"
	} else {
		placeholders := strings.TrimSuffix(strings.Repeat("?,", len(searchableFields)), ",")
		searchClause = "EXISTS (SELECT 1 FROM json_each(data) WHERE key IN (" + placeholders + ") AND CAST(value AS TEXT) LIKE ?)"
		for _, k := range searchableFields {
			binds = append(binds, k)
		}
	}
	binds = append(binds, pattern)

	where := searchClause
	if clause, fbinds, ok := buildFilterSQL(filters); ok {
		where = "(" + searchClause + ") AND (" + clause + ")"
		binds = append(binds, fbinds...)
	}
	return c.list(ctx, "WHERE "+where, binds, limit, offset, sortSpec)
}

// LastSyncTime returns the last sync timestamp (SQLite datetime), or nil if
// the cache was never synced.
func (c *DatasetCache) LastSyncTime(ctx context.Context) (*string, error) {
	var v string
	err := c.db.QueryRowContext(ctx, "SELECT value FROM cache_meta WHERE key = 'last_sync'").Scan(&v)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &v, nil
}

// IsStale reports true when the cache has never been synced or the last sync
// is at least ttlSeconds old.
func (c *DatasetCache) IsStale(ctx context.Context, ttlSeconds int64) (bool, error) {
	var age sql.NullInt64
	err := c.db.QueryRowContext(ctx,
		"SELECT CAST((julianday('now') - julianday(value)) * 86400 AS INTEGER) FROM cache_meta WHERE key = 'last_sync'").Scan(&age)
	if errors.Is(err, sql.ErrNoRows) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if !age.Valid {
		return true, nil
	}
	return age.Int64 >= ttlSeconds, nil
}

// Clear deletes every cached item (sync metadata is kept).
func (c *DatasetCache) Clear(ctx context.Context) error {
	_, err := c.db.ExecContext(ctx, "DELETE FROM cached_items")
	return err
}

// Destroy closes the cache and removes its database file.
func (c *DatasetCache) Destroy() error {
	c.db.Close()
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(c.path + suffix)
	}
	if err := os.Remove(c.path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}
