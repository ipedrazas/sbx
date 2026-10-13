package cache

// Port of dstudio-core/tests/filter_tests.rs (exercises the in-memory
// reference path the SQL push-down must agree with).

import (
	"testing"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
)

func mkFilter(field string, op models.FilterOperator, v any) models.DatasetFilter {
	return models.DatasetFilter{FieldName: field, Operator: op, Value: v, Logic: models.LogicAnd}
}

func mkOrFilter(field string, op models.FilterOperator, v any) models.DatasetFilter {
	f := mkFilter(field, op, v)
	f.Logic = models.LogicOr
	return f
}

func sampleItems() []map[string]any {
	return []map[string]any{
		{"id": "1", "name": "Alice", "age": 30.0, "email": "alice@example.com"},
		{"id": "2", "name": "Bob", "age": 25.0, "email": "bob@org.com"},
		{"id": "3", "name": "Charlie", "age": 35.0, "email": "charlie@example.com"},
	}
}

func TestInMemoryFilters(t *testing.T) {
	cases := []struct {
		name     string
		items    []map[string]any
		filters  []models.DatasetFilter
		want     int
		wantName string
	}{
		{"no filters", sampleItems(), nil, 3, ""},
		{"equals", sampleItems(), []models.DatasetFilter{mkFilter("name", models.OpEquals, "Bob")}, 1, "Bob"},
		{"not equals", sampleItems(), []models.DatasetFilter{mkFilter("name", models.OpNotEquals, "Bob")}, 2, ""},
		{"contains", sampleItems(), []models.DatasetFilter{mkFilter("email", models.OpContains, "example.com")}, 2, ""},
		{"contains ci", sampleItems(), []models.DatasetFilter{mkFilter("email", models.OpContains, "EXAMPLE.COM")}, 2, ""},
		{"not contains", sampleItems(), []models.DatasetFilter{mkFilter("email", models.OpNotContains, "example.com")}, 1, "Bob"},
		{"gt", sampleItems(), []models.DatasetFilter{mkFilter("age", models.OpGreaterThan, "30")}, 1, "Charlie"},
		{"lt", sampleItems(), []models.DatasetFilter{mkFilter("age", models.OpLessThan, "30")}, 1, "Bob"},
		{"in", sampleItems(), []models.DatasetFilter{mkFilter("name", models.OpIn, []any{"Alice", "Charlie"})}, 2, ""},
		{"not in", sampleItems(), []models.DatasetFilter{mkFilter("name", models.OpNotIn, []any{"Alice", "Charlie"})}, 1, "Bob"},
		{"multiple and", sampleItems(), []models.DatasetFilter{
			mkFilter("email", models.OpContains, "example.com"),
			mkFilter("age", models.OpGreaterThan, "30"),
		}, 1, "Charlie"},
		{"missing field ne", []map[string]any{{"name": "Alice"}, {"name": "Bob", "role": "admin"}},
			[]models.DatasetFilter{mkFilter("role", models.OpNotEquals, "admin")}, 1, "Alice"},
		{"missing field eq", []map[string]any{{"name": "Alice"}},
			[]models.DatasetFilter{mkFilter("role", models.OpEquals, "admin")}, 0, ""},
		{"or", sampleItems(), []models.DatasetFilter{
			mkOrFilter("email", models.OpContains, "example.com"),
			mkOrFilter("name", models.OpEquals, "Bob"),
		}, 3, ""},
		{"or no universal", sampleItems(), []models.DatasetFilter{
			mkOrFilter("name", models.OpEquals, "Alice"),
			mkOrFilter("name", models.OpEquals, "Bob"),
		}, 2, ""},
		{"or across groups is and", sampleItems(), func() []models.DatasetFilter {
			f3 := mkFilter("email", models.OpContains, "example.com")
			f3.GroupIndex = 1
			return []models.DatasetFilter{
				mkOrFilter("name", models.OpEquals, "Alice"),
				mkOrFilter("name", models.OpEquals, "Charlie"),
				f3,
			}
		}(), 2, ""},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			got := connector.ApplyFiltersInMemory(c.items, c.filters)
			if len(got) != c.want {
				t.Fatalf("got %d want %d", len(got), c.want)
			}
			if c.wantName != "" && got[0]["name"] != c.wantName {
				t.Fatalf("got %v", got[0]["name"])
			}
			if c.name == "not equals" {
				for _, i := range got {
					if i["name"] == "Bob" {
						t.Fatal("Bob present")
					}
				}
			}

			// The SQL push-down must agree with the in-memory path.
			cache := open(t, 1)
			store(t, cache, c.items...)
			env, err := cache.GetItems(ctx, 100, 0, nil, c.filters)
			if err != nil {
				t.Fatal(err)
			}
			if len(env.Items) != c.want {
				t.Fatalf("sql got %d want %d", len(env.Items), c.want)
			}
		})
	}
}
