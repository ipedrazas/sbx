package fileupload

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/ipedrazas/sbx/internal/connector"
)

func row(t *testing.T, items []any, i int) map[string]any {
	t.Helper()
	m, ok := items[i].(map[string]any)
	if !ok {
		t.Fatalf("item %d is %T", i, items[i])
	}
	return m
}

func TestParseCSVBasic(t *testing.T) {
	schema, items, err := ParseCSV([]byte("name,age,active\nAlice,30,true\nBob,25,false\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(schema.Fields) != 3 || schema.Fields[0].Name != "name" || schema.Fields[0].FieldType != connector.FieldString {
		t.Fatalf("schema %+v", schema)
	}
	if schema.Fields[1].FieldType != connector.FieldInteger || schema.Fields[2].FieldType != connector.FieldBoolean {
		t.Fatalf("schema %+v", schema)
	}
	if len(items) != 2 {
		t.Fatal(len(items))
	}
	r := row(t, items, 0)
	if r["name"] != "Alice" || r["age"] != int64(30) || r["active"] != true {
		t.Fatalf("row %+v", r)
	}
}

func TestParseCSVDatesAndJSON(t *testing.T) {
	data := "id,date,labels\n1,2026-05-06T10:30:00,\"[\"\"a\"\",\"\"b\"\"]\"\n2,2026-05-07T11:00:00,\"[\"\"c\"\"]\"\n"
	schema, items, err := ParseCSV([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	if schema.Fields[1].FieldType != connector.FieldDateTime || schema.Fields[2].FieldType != connector.FieldJSON {
		t.Fatalf("schema %+v", schema)
	}
	labels, ok := row(t, items, 0)["labels"].([]any)
	if !ok || len(labels) != 2 || labels[0] != "a" {
		t.Fatalf("labels %+v", row(t, items, 0)["labels"])
	}
}

func TestParseCSVFloatsEmptyAndFlexible(t *testing.T) {
	data := "\xef\xbb\xbfa,b,c,d\n1.5,yes,2026-05-06,\n2,no,2026-05-07\n"
	schema, items, err := ParseCSV([]byte(data))
	if err != nil {
		t.Fatal(err)
	}
	want := []connector.FieldType{connector.FieldFloat, connector.FieldBoolean, connector.FieldDateTime, connector.FieldString}
	for i, w := range want {
		if schema.Fields[i].FieldType != w {
			t.Fatalf("field %d = %s want %s", i, schema.Fields[i].FieldType, w)
		}
	}
	if schema.Fields[0].Name != "a" {
		t.Fatalf("BOM not stripped: %q", schema.Fields[0].Name)
	}
	r0, r1 := row(t, items, 0), row(t, items, 1)
	if r0["a"] != 1.5 || r0["b"] != true || r0["d"] != nil || r1["a"] != 2.0 || r1["b"] != false || r1["d"] != nil {
		t.Fatalf("rows %+v %+v", r0, r1)
	}
}

func TestEmptyCSVErrors(t *testing.T) {
	if _, _, err := ParseCSV([]byte("name,age\n")); err == nil {
		t.Fatal("expected error")
	}
	_, _, err := ParseCSV(nil)
	var ce *connector.Error
	if !errors.As(err, &ce) || ce.Message != "CSV has no columns" {
		t.Fatalf("got %v", err)
	}
}

func TestParseJSONL(t *testing.T) {
	schema, items, err := ParseJSONL([]byte("{\"name\":\"Alice\",\"age\":30}\r\n\n{\"name\":\"Bob\",\"age\":25.5}\n"))
	if err != nil {
		t.Fatal(err)
	}
	if len(schema.Fields) != 2 || len(items) != 2 {
		t.Fatalf("%+v %d", schema, len(items))
	}
	// sorted: age, name; age is int+float → float
	if schema.Fields[0].Name != "age" || schema.Fields[0].FieldType != connector.FieldFloat {
		t.Fatalf("%+v", schema.Fields)
	}
	if _, _, err := ParseJSONL([]byte("\n\n")); err == nil {
		t.Fatal("expected error for empty")
	}
	_, _, err = ParseJSONL([]byte("{}\n{bad\n"))
	if err == nil || !strings.Contains(err.Error(), "line 2") {
		t.Fatalf("got %v", err)
	}
	if _, _, err := ParseJSONL([]byte{0xff}); err == nil {
		t.Fatal("expected utf8 error")
	}
}

func TestParseJSONArray(t *testing.T) {
	schema, items, err := ParseJSONArray([]byte(`[{"x":1},{"x":2,"y":"hello","z":null,"w":[1],"b":true,"d":"2026-01-01"}]`))
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatal(len(items))
	}
	got := map[string]connector.FieldType{}
	for _, f := range schema.Fields {
		got[f.Name] = f.FieldType
	}
	if got["x"] != connector.FieldInteger || got["y"] != connector.FieldString || got["w"] != connector.FieldJSON ||
		got["b"] != connector.FieldBoolean || got["d"] != connector.FieldDateTime {
		t.Fatalf("%+v", got)
	}
	if _, ok := got["z"]; ok {
		t.Fatal("null-only field should be absent")
	}
	if _, _, err := ParseJSONArray([]byte(`[]`)); err == nil {
		t.Fatal("expected empty error")
	}
	if _, _, err := ParseJSONArray([]byte(`{"a":1}`)); err == nil {
		t.Fatal("expected type error")
	}
	if _, _, err := ParseJSONArray([]byte(`[1] x`)); err == nil {
		t.Fatal("expected trailing error")
	}
}

func TestMixedTypesAndHelpers(t *testing.T) {
	if resolveMixedTypes(map[string]bool{"string": true, "integer": true}) != connector.FieldString {
		t.Fatal("mixed → string")
	}
	if inferColumn([]string{`{"a":1}`, `[1]`}) != connector.FieldJSON {
		t.Fatal("json col")
	}
	if inferColumn([]string{"x"}) != connector.FieldString {
		t.Fatal("string col")
	}
	if looksLikeDatetime("2026-0x-06") || !looksLikeDatetime("2026-05-06") || looksLikeDatetime("short") {
		t.Fatal("datetime")
	}
	if coerceValue("abc", connector.FieldInteger) != "abc" || coerceValue("abc", connector.FieldFloat) != "abc" ||
		coerceValue("maybe", connector.FieldBoolean) != "maybe" || coerceValue("{x", connector.FieldJSON) != "{x" {
		t.Fatal("coerce fallbacks")
	}
	if coerceValue("1e400", connector.FieldFloat) != nil {
		t.Fatal("inf should be null")
	}
	if isFloat("0x1p3") {
		t.Fatal("hex float accepted")
	}
	if jsonTypeTag(json.Number("1.5")) != "float" || jsonTypeTag(float64(2)) != "integer" || jsonTypeTag(map[string]any{}) != "json" {
		t.Fatal("tags")
	}
}

func TestConnector(t *testing.T) {
	c := New()
	ctx := context.Background()
	s, err := c.Introspect(ctx)
	if err != nil || len(s.Fields) != 0 {
		t.Fatal(err)
	}
	if err := c.TestConnection(ctx); err != nil {
		t.Fatal(err)
	}
	_, err = c.Query(ctx, &connector.QueryParams{})
	var ce *connector.Error
	if !errors.As(err, &ce) || ce.Kind != connector.ErrUnsupported {
		t.Fatalf("got %v", err)
	}
}
