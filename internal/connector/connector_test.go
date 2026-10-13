package connector

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/ipedrazas/sbx/internal/models"
)

func TestParseAdhocScalarOps(t *testing.T) {
	f, err := ParseAdhocFilter("status:eq:Done", 1000)
	if err != nil {
		t.Fatal(err)
	}
	if f.FieldName != "status" || f.Operator != models.OpEquals || f.Value != "Done" || f.GroupIndex != 1000 || f.Logic != models.LogicAnd {
		t.Fatalf("unexpected %+v", f)
	}
	for raw, op := range map[string]models.FilterOperator{
		"title:contains:bug": models.OpContains,
		"status:ne:Canceled": models.OpNotEquals,
		"a:ncontains:x":      models.OpNotContains,
		"a:gt:1":             models.OpGreaterThan,
		"a:lt:1":             models.OpLessThan,
	} {
		got, err := ParseAdhocFilter(raw, 0)
		if err != nil || got.Operator != op {
			t.Errorf("%s: got %v %v", raw, got.Operator, err)
		}
	}
}

func TestParseAdhocInNin(t *testing.T) {
	f, _ := ParseAdhocFilter("labels:in:bug, urgent ,security", 0)
	if f.Operator != models.OpIn || !reflect.DeepEqual(f.Value, []any{"bug", "urgent", "security"}) {
		t.Fatalf("unexpected %+v", f)
	}
	n, _ := ParseAdhocFilter("status:nin:Done,Canceled", 0)
	if n.Operator != models.OpNotIn || !reflect.DeepEqual(n.Value, []any{"Done", "Canceled"}) {
		t.Fatalf("unexpected %+v", n)
	}
}

func TestParseAdhocColonsInValue(t *testing.T) {
	f, _ := ParseAdhocFilter("updated:gt:2025-06-01T10:00:00", 0)
	if f.Value != "2025-06-01T10:00:00" {
		t.Fatalf("got %v", f.Value)
	}
}

func TestParseAdhocRejects(t *testing.T) {
	for _, raw := range []string{"statusDone", "status:eq", ":eq:x", "status:like:x"} {
		if _, err := ParseAdhocFilter(raw, 0); err == nil {
			t.Errorf("%s should fail", raw)
		}
	}
}

func TestApplyFiltersInMemory(t *testing.T) {
	items := []map[string]any{
		{"status": "Done", "team": "ENG"},
		{"status": "Todo", "team": "ENG"},
		{"status": "Done", "team": "OPS"},
	}
	a, _ := ParseAdhocFilter("status:eq:Done", 1000)
	b, _ := ParseAdhocFilter("team:eq:ENG", 1001)
	out := ApplyFiltersInMemory(items, []models.DatasetFilter{a, b})
	if len(out) != 1 || out[0]["team"] != "ENG" {
		t.Fatalf("got %v", out)
	}

	// OR group
	or := []models.DatasetFilter{
		{FieldName: "team", Operator: models.OpEquals, Value: "OPS", Logic: models.LogicOr},
		{FieldName: "status", Operator: models.OpEquals, Value: "Todo", Logic: models.LogicOr},
	}
	if got := ApplyFiltersInMemory(items, or); len(got) != 2 {
		t.Fatalf("or: got %v", got)
	}
	if got := ApplyFiltersInMemory(items, nil); len(got) != 3 {
		t.Fatal("no filters should pass all")
	}
}

func TestMatchesFilterOperators(t *testing.T) {
	item := map[string]any{"n": float64(5), "s": "Hello World"}
	cases := []struct {
		f    models.DatasetFilter
		want bool
	}{
		{models.DatasetFilter{FieldName: "s", Operator: models.OpContains, Value: "world"}, true},
		{models.DatasetFilter{FieldName: "s", Operator: models.OpNotContains, Value: "world"}, false},
		{models.DatasetFilter{FieldName: "n", Operator: models.OpGreaterThan, Value: "4"}, true},
		{models.DatasetFilter{FieldName: "n", Operator: models.OpLessThan, Value: "4"}, false},
		{models.DatasetFilter{FieldName: "n", Operator: models.OpIn, Value: []any{float64(5)}}, true},
		{models.DatasetFilter{FieldName: "n", Operator: models.OpIn, Value: "5"}, false},
		{models.DatasetFilter{FieldName: "n", Operator: models.OpNotIn, Value: "5"}, true},
		{models.DatasetFilter{FieldName: "n", Operator: models.OpNotIn, Value: []any{"5"}}, false},
		{models.DatasetFilter{FieldName: "missing", Operator: models.OpNotEquals, Value: "x"}, true},
		{models.DatasetFilter{FieldName: "missing", Operator: models.OpEquals, Value: "x"}, false},
	}
	for i, c := range cases {
		if got := MatchesFilter(item, c.f); got != c.want {
			t.Errorf("case %d: got %v want %v", i, got, c.want)
		}
	}
}

func TestSortCapability(t *testing.T) {
	if !SortCapabilityFor(models.DatasourceGmail, models.ModeCached).Allows("x") {
		t.Fatal("cached should allow all")
	}
	if SortCapabilityFor(models.DatasourceGmail, models.ModePullThrough).Allows("x") {
		t.Fatal("gmail pull-through should allow none")
	}
	j := SortCapabilityFor(models.DatasourceJira, models.ModePullThrough)
	if !j.Allows("created") || j.Allows("labels") {
		t.Fatal("jira fields wrong")
	}
	if !SortCapabilityFor(models.DatasourcePostgres, models.ModePullThrough).Allows("any") {
		t.Fatal("postgres should allow all")
	}
}

func TestErrors(t *testing.T) {
	e := FromUpstreamHTTP(401, "nope", "gmail")
	if e.Kind != ErrAuth {
		t.Fatal("401 should be auth")
	}
	e = FromUpstreamHTTP(404, "missing", "gmail")
	if e.Kind != ErrUpstreamHTTP || e.Status != 404 {
		t.Fatal("404 should be upstream")
	}
	var target *Error
	if !errors.As(error(Config("bad %d", 1)), &target) || target.Error() != "configuration error: bad 1" {
		t.Fatalf("got %v", target)
	}
	if FieldDateTime.String() != "datetime" || FieldJSON.String() != "json" {
		t.Fatal("field type strings")
	}
	for _, k := range []*Error{Connection("x"), Query("x"), BadInput("x"), Introspection("x"), Unsupported("x"), e} {
		if k.Error() == "" {
			t.Fatal("empty error string")
		}
	}
}

func TestBaseDefaults(t *testing.T) {
	var b Base
	if b.SortCapability().Kind != SortNone {
		t.Fatal("base sort")
	}
	if _, err := b.InvokeAction(context.Background(), "x", nil); err == nil {
		t.Fatal("expected unsupported")
	}
	if _, err := b.SimulateAction(context.Background(), "x", nil); err == nil {
		t.Fatal("expected unsupported")
	}
	if _, ok := b.GenerateSyntheticRows(nil, 1); ok {
		t.Fatal("expected fallback")
	}
	if DefaultQueryParams().Limit != 50 {
		t.Fatal("default limit")
	}
}
