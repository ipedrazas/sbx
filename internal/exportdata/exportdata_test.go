package exportdata

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/ipedrazas/sbx/internal/connectors/fileupload"
)

func rows() []map[string]any {
	return []map[string]any{
		{"id": "1", "name": "Alice", "age": 30, "active": true},
		{"id": "2", "name": "Bob", "age": 25, "active": false},
	}
}

func lines(b []byte) []string {
	return strings.Split(strings.TrimSuffix(string(b), "\n"), "\n")
}

func TestParseFormat(t *testing.T) {
	cases := map[string]Format{"CSV": CSV, "json": JSON, "ndjson": JSONL, " jsonl ": JSONL}
	for in, want := range cases {
		got, err := ParseFormat(in)
		if err != nil || got != want {
			t.Fatalf("%q: %v %v", in, got, err)
		}
	}
	_, err := ParseFormat("XML")
	if err == nil || err.Error() != "unsupported format: 'xml' (expected csv, json, or jsonl)" {
		t.Fatalf("got %v", err)
	}
	if CSV.Extension() != "csv" || JSON.Extension() != "json" || JSONL.Extension() != "jsonl" {
		t.Fatal("ext")
	}
	if CSV.ContentType() != "text/csv" || JSON.ContentType() != "application/json" || JSONL.ContentType() != "application/x-ndjson" {
		t.Fatal("ctype")
	}
}

func TestCSVHeaderAndRows(t *testing.T) {
	out, err := SerializeRows(rows(), CSV)
	if err != nil {
		t.Fatal(err)
	}
	l := lines(out)
	if l[0] != "active,age,id,name" || l[1] != "true,30,1,Alice" || l[2] != "false,25,2,Bob" {
		t.Fatalf("%q", l)
	}
}

func TestCSVUnionsHeterogeneous(t *testing.T) {
	out, _ := SerializeRows([]map[string]any{{"a": 1}, {"b": 2}}, CSV)
	l := lines(out)
	if l[0] != "a,b" || l[1] != "1," || l[2] != ",2" {
		t.Fatalf("%q", l)
	}
}

func TestCSVNestedAndNull(t *testing.T) {
	out, _ := SerializeRows([]map[string]any{{"id": "1", "tags": []any{"x", "y"}, "note": nil}}, CSV)
	l := lines(out)
	if l[0] != "id,note,tags" || l[1] != `1,,"[""x"",""y""]"` {
		t.Fatalf("%q", l)
	}
}

func TestStreamingMatchesBuffered(t *testing.T) {
	r := rows()
	cols := CSVUnionColumns(r)
	streamed, _ := CSVHeader(cols)
	a, _ := CSVDataRows(cols, r[:1])
	b, _ := CSVDataRows(cols, r[1:])
	streamed = append(append(streamed, a...), b...)
	buffered, _ := SerializeRows(r, CSV)
	if string(streamed) != string(buffered) {
		t.Fatalf("%q vs %q", streamed, buffered)
	}
}

func TestStreamingProjects(t *testing.T) {
	cols := []string{"id", "name"}
	h, _ := CSVHeader(cols)
	d, _ := CSVDataRows(cols, []map[string]any{{"id": "1", "name": "Alice", "secret": "x"}, nil})
	if string(h) != "id,name\n" || string(d) != "1,Alice\n" {
		t.Fatalf("%q %q", h, d)
	}
	one, _ := CSVDataRows([]string{"a"}, []map[string]any{{"a": ""}})
	if string(one) != "\"\"\n" {
		t.Fatalf("%q", one)
	}
	q, _ := CSVDataRows([]string{"a"}, []map[string]any{{"a": "he said \"hi\"\nbye"}})
	if string(q) != "\"he said \"\"hi\"\"\nbye\"\n" {
		t.Fatalf("%q", q)
	}
}

func TestJSONL(t *testing.T) {
	out, _ := SerializeRows(rows(), JSONL)
	l := lines(out)
	if len(l) != 2 {
		t.Fatal(l)
	}
	var first map[string]any
	if err := json.Unmarshal([]byte(l[0]), &first); err != nil || first["name"] != "Alice" {
		t.Fatal(err, first)
	}
	if l[0] != `{"active":true,"age":30,"id":"1","name":"Alice"}` {
		t.Fatal(l[0])
	}
}

func TestJSON(t *testing.T) {
	out, _ := SerializeRows(rows(), JSON)
	var parsed []map[string]any
	if err := json.Unmarshal(out, &parsed); err != nil || len(parsed) != 2 || parsed[1]["name"] != "Bob" {
		t.Fatal(err, parsed)
	}
	if !strings.HasPrefix(string(out), "[\n  {\n    \"active\": true,") {
		t.Fatalf("%s", out)
	}
}

func TestEmptyRows(t *testing.T) {
	if b, _ := SerializeRows(nil, JSONL); len(b) != 0 {
		t.Fatal(b)
	}
	if b, _ := SerializeRows(nil, JSON); string(b) != "[]" {
		t.Fatal(string(b))
	}
	if b, _ := SerializeRows(nil, CSV); len(b) != 0 {
		t.Fatal(b)
	}
}

// Round-trip: upload CSV → rows → export CSV yields the same text.
func TestRoundTripWithFileUpload(t *testing.T) {
	in := "a,b\n1,x\n2,y\n"
	_, items, err := fileupload.ParseCSV([]byte(in))
	if err != nil {
		t.Fatal(err)
	}
	var rs []map[string]any
	for _, it := range items {
		rs = append(rs, it.(map[string]any))
	}
	out, _ := SerializeRows(rs, CSV)
	if string(out) != in {
		t.Fatalf("%q", out)
	}
}
