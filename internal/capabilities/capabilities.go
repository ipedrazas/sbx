// Package capabilities computes the agent-facing "what knobs exist" metadata
// for a dataset, from stored field flags gated by the datasource's actual
// ability, so what's advertised matches what /list and /search honor.
// Per-response runtime info lives on models.EnvelopeMeta.
package capabilities

import (
	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
)

// GlobalRowCap is the hard ceiling on rows per agent query. A dataset-level
// max_rows_per_query may lower it but not raise it.
const GlobalRowCap int64 = 1000

// DatasetCapabilities is the capability summary for one dataset. Field names
// are display names, matching /schema and what agents pass to sort_by.
type DatasetCapabilities struct {
	SortableFields   []string
	SearchableFields []string
	MaxRowsPerQuery  int64
	MaxPageBytes     *int64
	SupportsSearch   bool
}

// Compute derives effective capabilities. A field is sortable when visible,
// not withdrawn by the operator, and orderable by the datasource (cache-backed
// datasets order anything; pull-through delegates to the connector type).
// Searchable is visible ∧ searchable.
func Compute(dataset *models.Dataset, dsType models.DatasourceType, fields []models.DatasetField) DatasetCapabilities {
	sc := connector.SortCapabilityFor(dsType, dataset.Mode)
	sortable := []string{}
	searchable := []string{}
	for _, f := range fields {
		if f.Visible && f.Sortable && sc.Allows(f.SourceName) {
			sortable = append(sortable, f.DisplayName)
		}
		if f.Visible && f.Searchable {
			searchable = append(searchable, f.DisplayName)
		}
	}
	maxRows := GlobalRowCap
	if dataset.MaxRowsPerQuery != nil {
		maxRows = min(max(*dataset.MaxRowsPerQuery, 1), GlobalRowCap)
	}
	return DatasetCapabilities{
		SortableFields:   sortable,
		SearchableFields: searchable,
		MaxRowsPerQuery:  maxRows,
		MaxPageBytes:     dataset.MaxPageBytes,
		SupportsSearch:   true,
	}
}

// FieldSortable reports whether a field is effectively sortable, consistent
// with Compute's SortableFields.
func FieldSortable(dataset *models.Dataset, dsType models.DatasourceType, field *models.DatasetField) bool {
	return field.Visible && field.Sortable && connector.SortCapabilityFor(dsType, dataset.Mode).Allows(field.SourceName)
}

// ToJSON renders the capabilities object; max_page_bytes is omitted when unset.
func (c DatasetCapabilities) ToJSON() map[string]any {
	m := map[string]any{
		"sortable_fields":    nonNil(c.SortableFields),
		"searchable_fields":  nonNil(c.SearchableFields),
		"max_rows_per_query": c.MaxRowsPerQuery,
		"supports_search":    c.SupportsSearch,
	}
	if c.MaxPageBytes != nil {
		m["max_page_bytes"] = *c.MaxPageBytes
	}
	return m
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
