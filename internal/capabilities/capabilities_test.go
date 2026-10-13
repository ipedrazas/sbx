package capabilities

import (
	"slices"
	"testing"

	"github.com/ipedrazas/sbx/internal/models"
)

func dataset(mode models.DatasetMode, maxRows *int64) *models.Dataset {
	return &models.Dataset{ID: 1, Name: "ds", DatasourceID: 1, Mode: mode, SourceConfig: map[string]any{}, Enabled: true, MaxRowsPerQuery: maxRows}
}

func field(source, display string, visible, sortable bool) models.DatasetField {
	f := models.NewDatasetField()
	f.SourceName, f.DisplayName, f.Visible, f.Sortable = source, display, visible, sortable
	return f
}

func TestPullthroughJiraOnlyAdvertisesSupportedFields(t *testing.T) {
	ds := dataset(models.ModePullThrough, nil)
	fields := []models.DatasetField{field("updated", "Updated", true, true), field("description", "Description", true, true)}
	caps := Compute(ds, models.DatasourceJira, fields)
	if !slices.Equal(caps.SortableFields, []string{"Updated"}) {
		t.Fatal(caps.SortableFields)
	}
}

func TestCacheBackedAdvertisesAnyVisibleSortableField(t *testing.T) {
	ds := dataset(models.ModeMirror, nil)
	caps := Compute(ds, models.DatasourceJira, []models.DatasetField{field("description", "Description", true, true)})
	if !slices.Equal(caps.SortableFields, []string{"Description"}) {
		t.Fatal(caps.SortableFields)
	}
}

func TestOperatorWithdrawalAndVisibilityExcludeFields(t *testing.T) {
	ds := dataset(models.ModeCached, nil)
	fields := []models.DatasetField{field("a", "A", true, false), field("b", "B", false, true), field("c", "C", true, true)}
	caps := Compute(ds, models.DatasourcePostgres, fields)
	if !slices.Equal(caps.SortableFields, []string{"C"}) {
		t.Fatal(caps.SortableFields)
	}
	if !slices.Equal(caps.SearchableFields, []string{"A", "C"}) {
		t.Fatal(caps.SearchableFields)
	}
}

func TestAdvertisedNamesAreDisplayNames(t *testing.T) {
	ds := dataset(models.ModePullThrough, nil)
	fields := []models.DatasetField{field("created_at", "Created At", true, true)}
	caps := Compute(ds, models.DatasourcePostgres, fields)
	if !slices.Equal(caps.SortableFields, []string{"Created At"}) {
		t.Fatal(caps.SortableFields)
	}
	if !FieldSortable(ds, models.DatasourcePostgres, &fields[0]) {
		t.Fatal("expected sortable")
	}
	hidden := field("x", "X", false, true)
	if FieldSortable(ds, models.DatasourcePostgres, &hidden) {
		t.Fatal("hidden field must not be sortable")
	}
}

func TestMaxRowsClampsToGlobalCap(t *testing.T) {
	big := GlobalRowCap + 500
	if c := Compute(dataset(models.ModeCached, &big), models.DatasourcePostgres, nil); c.MaxRowsPerQuery != GlobalRowCap {
		t.Fatal(c.MaxRowsPerQuery)
	}
	if c := Compute(dataset(models.ModeCached, nil), models.DatasourcePostgres, nil); c.MaxRowsPerQuery != GlobalRowCap {
		t.Fatal(c.MaxRowsPerQuery)
	}
	zero := int64(0)
	if c := Compute(dataset(models.ModeCached, &zero), models.DatasourcePostgres, nil); c.MaxRowsPerQuery != 1 {
		t.Fatal(c.MaxRowsPerQuery)
	}
	fifty := int64(50)
	if c := Compute(dataset(models.ModeCached, &fifty), models.DatasourcePostgres, nil); c.MaxRowsPerQuery != 50 {
		t.Fatal(c.MaxRowsPerQuery)
	}
}

func TestToJSON(t *testing.T) {
	ds := dataset(models.ModeCached, nil)
	m := Compute(ds, models.DatasourcePostgres, nil).ToJSON()
	if _, ok := m["max_page_bytes"]; ok {
		t.Fatal("max_page_bytes must be omitted when unset")
	}
	if s, ok := m["sortable_fields"].([]string); !ok || s == nil {
		t.Fatal("sortable_fields must be a non-nil list")
	}
	if m["supports_search"] != true || m["max_rows_per_query"] != GlobalRowCap {
		t.Fatal(m)
	}
	mpb := int64(4096)
	ds.MaxPageBytes = &mpb
	m = Compute(ds, models.DatasourcePostgres, nil).ToJSON()
	if m["max_page_bytes"] != int64(4096) {
		t.Fatal(m)
	}
	if (DatasetCapabilities{}).ToJSON()["searchable_fields"] == nil {
		t.Fatal("nil slices must render as []")
	}
}
