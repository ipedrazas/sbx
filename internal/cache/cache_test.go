package cache

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"slices"
	"testing"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
)

func open(t *testing.T, id int64) *DatasetCache {
	t.Helper()
	c, err := Open(context.Background(), t.TempDir(), id)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { c.Close() })
	return c
}

func store(t *testing.T, c *DatasetCache, items ...map[string]any) {
	t.Helper()
	if err := c.StoreItems(context.Background(), items); err != nil {
		t.Fatal(err)
	}
}

func ids(env models.Envelope) []string {
	out := []string{}
	for _, i := range env.Items {
		out = append(out, fmt.Sprint(i["id"]))
	}
	return out
}

var ctx = context.Background()

func TestStoreAndRetrieve(t *testing.T) {
	c := open(t, 1)
	store(t, c, map[string]any{"id": "a", "name": "Alice"}, map[string]any{"id": "b", "name": "Bob"})
	env, err := c.GetItems(ctx, 10, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(env.Items) != 2 || env.Meta.Total != 2 {
		t.Fatalf("%+v", env)
	}
}

func TestGetSingleItem(t *testing.T) {
	c := open(t, 2)
	store(t, c, map[string]any{"id": "x", "name": "X"}, map[string]any{"id": "y", "name": "Y"})
	item, err := c.GetItem(ctx, "x")
	if err != nil || item == nil || item["name"] != "X" {
		t.Fatalf("%v %v", item, err)
	}
	missing, err := c.GetItem(ctx, "z")
	if err != nil || missing != nil {
		t.Fatalf("%v %v", missing, err)
	}
}

func TestSearch(t *testing.T) {
	c := open(t, 3)
	store(t, c,
		map[string]any{"id": "1", "name": "Registration Email"},
		map[string]any{"id": "2", "name": "Welcome Email"},
		map[string]any{"id": "3", "name": "Invoice"})
	check := func(q string, fields []string, want int) {
		t.Helper()
		r, err := c.Search(ctx, q, fields, 10, 0, nil, nil)
		if err != nil {
			t.Fatal(err)
		}
		if len(r.Items) != want || r.Meta.Total != int64(want) {
			t.Errorf("search %q %v: got %d (total %d), want %d", q, fields, len(r.Items), r.Meta.Total, want)
		}
	}
	check("Email", nil, 2)
	check("Invoice", nil, 1)
	check("Email", []string{"name"}, 2)
	check("Email", []string{"id"}, 0)
	check("Email", []string{}, 0)
}

func TestPagination(t *testing.T) {
	c := open(t, 4)
	var items []map[string]any
	for i := 0; i < 10; i++ {
		items = append(items, map[string]any{"id": fmt.Sprint(i), "n": i})
	}
	store(t, c, items...)
	p1, err := c.GetItems(ctx, 3, 0, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(p1.Items) != 3 || p1.Meta.Total != 10 || p1.Meta.Limit != 3 || p1.Meta.Offset != 0 {
		t.Fatalf("%+v", p1.Meta)
	}
	p2, _ := c.GetItems(ctx, 3, 3, nil, nil)
	if len(p2.Items) != 3 || p2.Meta.Offset != 3 {
		t.Fatalf("%+v", p2.Meta)
	}
}

func TestClear(t *testing.T) {
	c := open(t, 5)
	store(t, c, map[string]any{"id": "a", "name": "A"})
	before, _ := c.GetItems(ctx, 10, 0, nil, nil)
	if len(before.Items) != 1 {
		t.Fatal("before")
	}
	if err := c.Clear(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ := c.GetItems(ctx, 10, 0, nil, nil)
	if len(after.Items) != 0 || after.Items == nil {
		t.Fatalf("after: %#v", after.Items)
	}
}

func TestSortOrdersFullSetBeforePaging(t *testing.T) {
	c := open(t, 7)
	store(t, c,
		map[string]any{"id": "c", "n": 3, "name": "Charlie"},
		map[string]any{"id": "a", "n": 1, "name": "Alice"},
		map[string]any{"id": "b", "n": 2, "name": "Bob"},
		map[string]any{"id": "d", "n": 10, "name": "Dave"})
	asc := &models.SortSpec{Field: "n"}
	p1, _ := c.GetItems(ctx, 2, 0, asc, nil)
	if got := ids(p1); !slices.Equal(got, []string{"a", "b"}) {
		t.Fatal(got)
	}
	p2, _ := c.GetItems(ctx, 2, 2, asc, nil)
	if got := ids(p2); !slices.Equal(got, []string{"c", "d"}) {
		t.Fatal(got)
	}
	all, _ := c.GetItems(ctx, 10, 0, &models.SortSpec{Field: "n", Descending: true}, nil)
	if got := ids(all); !slices.Equal(got, []string{"d", "c", "b", "a"}) {
		t.Fatal(got)
	}
	r, _ := c.Search(ctx, "e", []string{"name"}, 10, 0, asc, nil)
	if got := ids(r); !slices.Equal(got, []string{"a", "c", "d"}) {
		t.Fatal(got)
	}
}

func TestSortFieldNameIsNotInjectable(t *testing.T) {
	c := open(t, 9)
	store(t, c, map[string]any{"id": "1", `weird.key[0]`: 2}, map[string]any{"id": "2", `weird.key[0]`: 1})
	r, err := c.GetItems(ctx, 10, 0, &models.SortSpec{Field: `weird.key[0]`}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(r); !slices.Equal(got, []string{"2", "1"}) {
		t.Fatal(got)
	}
	r, err = c.GetItems(ctx, 10, 0, &models.SortSpec{Field: "x') DESC; DROP TABLE cached_items; --"}, nil)
	if err != nil || len(r.Items) != 2 {
		t.Fatal(r, err)
	}
}

func TestFilterPushdownMatchesInMemoryAndPaginates(t *testing.T) {
	c := open(t, 8)
	items := []map[string]any{
		{"id": "1", "status": "Done", "team": "ENG", "labels": []any{"bug"}},
		{"id": "2", "status": "Todo", "team": "ENG", "labels": []any{"feature"}},
		{"id": "3", "status": "Done", "team": "OPS", "labels": []any{"bug"}},
		{"id": "4", "status": "Done", "team": "ENG", "labels": []any{}},
		{"id": "5", "team": "ENG"}, // missing status
	}
	store(t, c, items...)
	sortByID := &models.SortSpec{Field: "id"}
	for _, spec := range []string{
		"status:eq:Done", "status:ne:Done", "status:contains:o", "status:ncontains:o",
		"team:in:ENG,OPS", "team:nin:OPS", "id:gt:2", "id:lt:3",
	} {
		f, err := connector.ParseAdhocFilter(spec, 1000)
		if err != nil {
			t.Fatal(err)
		}
		filters := []models.DatasetFilter{f}
		got, err := c.GetItems(ctx, 100, 0, sortByID, filters)
		if err != nil {
			t.Fatal(spec, err)
		}
		sqlIDs := ids(got)
		slices.Sort(sqlIDs)
		memIDs := []string{}
		for _, i := range connector.ApplyFiltersInMemory(items, filters) {
			memIDs = append(memIDs, i["id"].(string))
		}
		slices.Sort(memIDs)
		if !slices.Equal(sqlIDs, memIDs) {
			t.Errorf("%s: sql %v vs mem %v", spec, sqlIDs, memIDs)
		}
		if got.Meta.Total != int64(len(memIDs)) {
			t.Errorf("%s: total %d", spec, got.Meta.Total)
		}
	}

	done, _ := connector.ParseAdhocFilter("status:eq:Done", 1000)
	p1, _ := c.GetItems(ctx, 2, 0, sortByID, []models.DatasetFilter{done})
	if got := ids(p1); !slices.Equal(got, []string{"1", "3"}) || p1.Meta.Total != 3 {
		t.Fatal(got, p1.Meta.Total)
	}
	p2, _ := c.GetItems(ctx, 2, 2, sortByID, []models.DatasetFilter{done})
	if got := ids(p2); !slices.Equal(got, []string{"4"}) {
		t.Fatal(got)
	}

	eng, _ := connector.ParseAdhocFilter("team:eq:ENG", 1000)
	r, err := c.Search(ctx, "bug", []string{"labels"}, 100, 0, nil, []models.DatasetFilter{eng})
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(r); !slices.Equal(got, []string{"1"}) {
		t.Fatal(got)
	}
}

func TestFilterGroupsAndEmptyIn(t *testing.T) {
	c := open(t, 10)
	store(t, c,
		map[string]any{"id": "1", "name": "Alice", "email": "alice@example.com"},
		map[string]any{"id": "2", "name": "Bob", "email": "bob@org.com"},
		map[string]any{"id": "3", "name": "Charlie", "email": "charlie@example.com"})
	or := func(field string, op models.FilterOperator, v any, g int32) models.DatasetFilter {
		return models.DatasetFilter{FieldName: field, Operator: op, Value: v, Logic: models.LogicOr, GroupIndex: g}
	}
	filters := []models.DatasetFilter{
		or("name", models.OpEquals, "Alice", 0),
		or("name", models.OpEquals, "Bob", 0),
		{FieldName: "email", Operator: models.OpContains, Value: "EXAMPLE", Logic: models.LogicAnd, GroupIndex: 1},
	}
	r, err := c.GetItems(ctx, 10, 0, &models.SortSpec{Field: "id"}, filters)
	if err != nil {
		t.Fatal(err)
	}
	if got := ids(r); !slices.Equal(got, []string{"1"}) {
		t.Fatal(got)
	}
	for op, want := range map[models.FilterOperator]int{models.OpIn: 0, models.OpNotIn: 3} {
		r, err := c.GetItems(ctx, 10, 0, nil, []models.DatasetFilter{{FieldName: "name", Operator: op, Value: "notarray"}})
		if err != nil || len(r.Items) != want {
			t.Errorf("%s: %d %v", op, len(r.Items), err)
		}
	}
	r, err = c.GetItems(ctx, 10, 0, nil, []models.DatasetFilter{{FieldName: "name", Operator: "bogus", Value: "x"}})
	if err != nil || len(r.Items) != 0 {
		t.Errorf("unknown op: %d %v", len(r.Items), err)
	}
}

func TestLastSyncTimeAndStaleness(t *testing.T) {
	c := open(t, 6)
	before, err := c.LastSyncTime(ctx)
	if err != nil || before != nil {
		t.Fatal(before, err)
	}
	stale, err := c.IsStale(ctx, 3600)
	if err != nil || !stale {
		t.Fatal("never-synced cache should be stale")
	}
	store(t, c, map[string]any{"id": "a"})
	after, err := c.LastSyncTime(ctx)
	if err != nil || after == nil {
		t.Fatal(after, err)
	}
	if stale, _ := c.IsStale(ctx, 3600); stale {
		t.Fatal("fresh cache reported stale")
	}
	if stale, _ := c.IsStale(ctx, 0); !stale {
		t.Fatal("ttl 0 should be stale")
	}
	if _, err := c.db.Exec("UPDATE cache_meta SET value = 'garbage'"); err != nil {
		t.Fatal(err)
	}
	if stale, err := c.IsStale(ctx, 3600); err != nil || !stale {
		t.Fatal("unparseable timestamp should be stale", err)
	}
}

func TestAutoIDsAndNumericIDs(t *testing.T) {
	c := open(t, 11)
	store(t, c,
		map[string]any{"id": nil, "name": "A"}, // falls back to a hashed id
		map[string]any{"name": "B"},
		map[string]any{"id": "", "name": "C"},
		map[string]any{"id": 42.0, "name": "D"},
	)
	var n int
	c.db.QueryRow("SELECT COUNT(*) FROM cached_items WHERE id LIKE '_auto_%'").Scan(&n)
	if n != 3 {
		t.Fatalf("auto ids: %d", n)
	}
	if item, _ := c.GetItem(ctx, "42"); item == nil || item["name"] != "D" {
		t.Fatal("numeric id")
	}
	// Re-storing the same batch replaces rather than duplicates.
	store(t, c, map[string]any{"id": nil, "name": "A"})
	var total int
	c.db.QueryRow("SELECT COUNT(*) FROM cached_items").Scan(&total)
	if total != 4 {
		t.Fatalf("total %d", total)
	}
}

// Reference value computed with Rust's DefaultHasher over the item JSON + index.
func TestAutoIDMatchesRust(t *testing.T) {
	var h rustHasher
	h.writeStr(`{"id":null,"name":"A"}`)
	h.writeUsize(3)
	if got := fmt.Sprintf("_auto_%x", h.finish()); got != "_auto_8e17167ef78a7e21" {
		t.Fatal(got)
	}
}

func TestGetItemNumbersAndDestroy(t *testing.T) {
	c, err := Open(ctx, t.TempDir(), 12)
	if err != nil {
		t.Fatal(err)
	}
	store(t, c, map[string]any{"id": "big", "n": int64(9007199254740993)})
	item, _ := c.GetItem(ctx, "big")
	if fmt.Sprint(item["n"]) != "9007199254740993" {
		t.Fatalf("large int lost: %v", item["n"])
	}
	if !reflect.DeepEqual(item["id"], "big") {
		t.Fatal(item)
	}
	if err := c.Destroy(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(c.Path()); !os.IsNotExist(err) {
		t.Fatal("file still exists")
	}
}

func TestOpenBadDir(t *testing.T) {
	if _, err := Open(ctx, "/nonexistent/dir/for/sure", 1); err == nil {
		t.Fatal("expected error")
	}
}
