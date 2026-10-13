package synthetic

import (
	"fmt"
	"reflect"
	"testing"

	"github.com/ipedrazas/sbx/internal/models"
)

// Reference values computed with Rust's std DefaultHasher.
func TestSeededHashMatchesRust(t *testing.T) {
	cases := []struct {
		row  int64
		col  string
		want uint64
	}{
		{0, "name", 16581364012331935247},
		{5, "email", 16918668234041913027},
		{123, "created_at", 12170402242332722397},
		{7, "", 14252925426473559587},
	}
	for _, c := range cases {
		if got := seededHash(c.row, c.col); got != c.want {
			t.Errorf("seededHash(%d,%q) = %d, want %d", c.row, c.col, got, c.want)
		}
	}
}

func field(name, ft string) models.DatasetField {
	f := models.NewDatasetField()
	f.SourceName = name
	f.DisplayName = name
	f.FieldType = ft
	return f
}

// Rows produced by the Rust implementation for the same inputs.
func TestGenerateRowsMatchesRust(t *testing.T) {
	fields := []models.DatasetField{
		field("email", "string"), field("phone", "string"), field("created_at", "datetime"),
		field("description", "string"), field("price", "float"),
	}
	rows := GenerateRows(fields, 2)
	want := []map[string]any{
		{"id": "1", "email": "wendy.williams@fake.dev", "phone": "+1-533-804-9590",
			"created_at":  "2024-06-15T22:29:00Z",
			"description": "Juliet kilo lima mike november oscar papa quebec romeo sierra tango alpha bravo charlie.",
			"price":       7117.53},
		{"id": "2", "email": "sam.lopez@demo.net", "phone": "+1-217-010-6975",
			"created_at":  "2023-12-14T21:59:00Z",
			"description": "November oscar papa quebec romeo sierra tango alpha.",
			"price":       8958.18},
	}
	if !reflect.DeepEqual(rows, want) {
		t.Fatalf("got %#v\nwant %#v", rows, want)
	}
}

func TestGenerateRowsDeterministicAndShapes(t *testing.T) {
	fields := []models.DatasetField{
		field("ID", "string"), field("first_name", "string"), field("surname", "string"),
		field("full_title", "string"), field("city", "string"), field("country", "string"),
		field("street_address", "string"), field("status", "string"), field("website", "string"),
		field("item_count", "integer"), field("is_admin", "string"), field("flag", "boolean"),
		field("total", "integer"), field("misc", "integer"), field("ratio", "float"),
		field("when", "datetime"), field("blob", "json"), field("other", "string"),
	}
	hidden := field("secret", "string")
	hidden.Visible = false
	fields = append(fields, hidden)

	a := GenerateRows(fields, 3)
	b := GenerateRows(fields, 3)
	if !reflect.DeepEqual(a, b) {
		t.Fatal("not deterministic")
	}
	if len(a) != 3 {
		t.Fatalf("len %d", len(a))
	}
	r := a[0]
	if _, ok := r["id"]; ok {
		t.Error("id column should not be added when a visible ID field exists")
	}
	if r["ID"] != "1" || a[2]["ID"] != "3" {
		t.Errorf("ID = %v", r["ID"])
	}
	if _, ok := r["secret"]; ok {
		t.Error("hidden field emitted")
	}
	for k, typ := range map[string]string{
		"first_name": "string", "surname": "string", "full_title": "string", "city": "string",
		"country": "string", "street_address": "string", "status": "string", "website": "string",
		"item_count": "int64", "is_admin": "bool", "flag": "bool", "total": "int64",
		"misc": "int64", "ratio": "float64", "when": "string", "blob": "map[string]interface {}",
		"other": "string",
	} {
		if got := fmt.Sprintf("%T", r[k]); got != typ {
			t.Errorf("%s: type %s, want %s", k, got, typ)
		}
	}
	if s := r["other"].(string); s[len(s)-2:] != "_1" {
		t.Errorf("generic string %q", s)
	}
	if s := r["when"].(string); s[:5] != "2024-" {
		t.Errorf("generic datetime %q", s)
	}
}

func TestGenerateRowsEdgeCases(t *testing.T) {
	if got := GenerateRows(nil, 5); len(got) != 0 {
		t.Fatal("expected empty")
	}
	f := field("x", "string")
	f.Visible = false
	if got := GenerateRows([]models.DatasetField{f}, 5); len(got) != 0 {
		t.Fatal("expected empty")
	}
	if got := GenerateRows([]models.DatasetField{field("x", "string")}, 0); len(got) != 0 {
		t.Fatal("expected empty")
	}
}

func TestClassifyFieldName(t *testing.T) {
	cases := map[string]fieldHint{
		"id": hintID, "user_id": hintID, "uuid": hintID, "Email": hintEmail, "firstName": hintFirstName,
		"family_name": hintLastName, "subject": hintName, "tel": hintPhone, "town": hintCity,
		"country": hintCountry, "street": hintAddress, "state": hintStatus, "link": hintURL,
		"bio": hintText, "timestamp": hintDateTime, "cost": hintCurrency, "score": hintSmallInt,
		"enabled": hintBoolean, "zzz": hintGeneric,
	}
	for name, want := range cases {
		if got := classifyFieldName(name); got != want {
			t.Errorf("%s: got %d want %d", name, got, want)
		}
	}
}
