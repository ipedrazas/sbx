// Package fileupload implements the file_upload connector (served from cache
// only) and the CSV / JSONL / JSON-array parsers used when a file is uploaded.
package fileupload

import (
	"bytes"
	"context"
	"encoding/csv"
	"encoding/json"
	"errors"
	"io"
	"math"
	"sort"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
)

// Connector is the file_upload connector. Rows live in the cache; it cannot
// query a live source.
type Connector struct {
	connector.Base
}

var _ connector.Connector = (*Connector)(nil)

func New() *Connector { return &Connector{} }

func (*Connector) Introspect(context.Context) (*connector.Schema, error) {
	return &connector.Schema{Fields: []connector.SchemaField{}}, nil
}

func (*Connector) Query(context.Context, *connector.QueryParams) (*models.Envelope, error) {
	return nil, connector.Unsupported("file_upload datasets are served from cache — use mirror mode")
}

func (*Connector) TestConnection(context.Context) error { return nil }

// ── File parsing ────────────────────────────────────────────────

// ParseCSV parses a CSV with a header row. Column types are inferred from the
// first 100 rows and values are coerced accordingly.
// Items are []any (each a map[string]any) for parity with the JSON parsers.
func ParseCSV(data []byte) (*connector.Schema, []any, error) {
	data = bytes.TrimPrefix(data, []byte("\xef\xbb\xbf"))
	r := csv.NewReader(bytes.NewReader(data))
	r.FieldsPerRecord = -1
	r.LazyQuotes = true

	headers, err := r.Read()
	if err != nil && !errors.Is(err, io.EOF) {
		return nil, nil, connector.Config("failed to read CSV headers: %v", err)
	}
	if len(headers) == 0 {
		return nil, nil, connector.Config("CSV has no columns")
	}

	var records [][]string
	for {
		rec, err := r.Read()
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			return nil, nil, connector.Config("CSV parse error: %v", err)
		}
		records = append(records, rec)
	}
	if len(records) == 0 {
		return nil, nil, connector.Config("CSV file contains no data rows")
	}

	colTypes := inferCSVTypes(headers, records)
	schema := &connector.Schema{Fields: make([]connector.SchemaField, len(headers))}
	for i, name := range headers {
		schema.Fields[i] = connector.SchemaField{Name: name, FieldType: colTypes[i]}
	}

	items := make([]any, len(records))
	for ri, rec := range records {
		obj := make(map[string]any, len(headers))
		for i, h := range headers {
			raw := ""
			if i < len(rec) {
				raw = rec[i]
			}
			obj[h] = coerceValue(raw, colTypes[i])
		}
		items[ri] = obj
	}
	return schema, items, nil
}

// ParseJSONL parses newline-delimited JSON; blank lines are skipped.
func ParseJSONL(data []byte) (*connector.Schema, []any, error) {
	if !utf8.Valid(data) {
		return nil, nil, connector.Config("invalid UTF-8: invalid utf-8 sequence")
	}
	var items []any
	for i, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(strings.TrimSuffix(line, "\r"))
		if line == "" {
			continue
		}
		v, err := decodeStrict([]byte(line))
		if err != nil {
			return nil, nil, connector.Config("JSONL parse error on line %d: %v", i+1, err)
		}
		items = append(items, v)
	}
	if len(items) == 0 {
		return nil, nil, connector.Config("JSONL file contains no data rows")
	}
	return inferJSONSchema(items), items, nil
}

// ParseJSONArray parses a top-level JSON array.
func ParseJSONArray(data []byte) (*connector.Schema, []any, error) {
	v, err := decodeStrict(data)
	if err != nil {
		return nil, nil, connector.Config("JSON parse error: %v", err)
	}
	items, ok := v.([]any)
	if !ok {
		return nil, nil, connector.Config("JSON parse error: invalid type: expected a sequence")
	}
	if len(items) == 0 {
		return nil, nil, connector.Config("JSON array is empty")
	}
	return inferJSONSchema(items), items, nil
}

// decodeStrict decodes exactly one JSON value (numbers as json.Number),
// rejecting trailing content like serde_json::from_str.
func decodeStrict(b []byte) (any, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	var v any
	if err := dec.Decode(&v); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing characters")
	}
	return v, nil
}

// ── Type inference helpers ──────────────────────────────────────

const sampleSize = 100

func inferCSVTypes(headers []string, records [][]string) []connector.FieldType {
	sample := records[:min(len(records), sampleSize)]
	out := make([]connector.FieldType, len(headers))
	for col := range headers {
		var values []string
		for _, r := range sample {
			if col < len(r) && r[col] != "" {
				values = append(values, r[col])
			}
		}
		out[col] = inferColumn(values)
	}
	return out
}

func inferColumn(values []string) connector.FieldType {
	if len(values) == 0 {
		return connector.FieldString
	}
	all := func(pred func(string) bool) bool {
		for _, v := range values {
			if !pred(v) {
				return false
			}
		}
		return true
	}
	switch {
	case all(isInt):
		return connector.FieldInteger
	case all(isFloat):
		return connector.FieldFloat
	case all(looksLikeBool):
		return connector.FieldBoolean
	case all(looksLikeDatetime):
		return connector.FieldDateTime
	case all(looksLikeJSON):
		return connector.FieldJSON
	}
	return connector.FieldString
}

func isInt(s string) bool {
	_, err := strconv.ParseInt(s, 10, 64)
	return err == nil
}

// parseFloat mirrors Rust's f64 parse: out-of-range values become ±inf rather
// than errors, and hex/underscore forms are rejected.
func parseFloat(s string) (float64, bool) {
	if strings.ContainsAny(s, "_xXpP") && !strings.EqualFold(strings.TrimLeft(s, "+-"), "infinity") {
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		var ne *strconv.NumError
		if errors.As(err, &ne) && errors.Is(ne.Err, strconv.ErrRange) {
			return f, true
		}
		return 0, false
	}
	return f, true
}

func isFloat(s string) bool {
	_, ok := parseFloat(s)
	return ok
}

func inferJSONSchema(items []any) *connector.Schema {
	sample := items[:min(len(items), sampleSize)]
	fieldTypes := map[string]map[string]bool{}
	for _, item := range sample {
		obj, ok := item.(map[string]any)
		if !ok {
			continue
		}
		for k, v := range obj {
			tag := jsonTypeTag(v)
			if tag == "" {
				continue
			}
			if fieldTypes[k] == nil {
				fieldTypes[k] = map[string]bool{}
			}
			fieldTypes[k][tag] = true
		}
	}
	names := make([]string, 0, len(fieldTypes))
	for k := range fieldTypes {
		names = append(names, k)
	}
	sort.Strings(names)
	schema := &connector.Schema{Fields: make([]connector.SchemaField, 0, len(names))}
	for _, n := range names {
		schema.Fields = append(schema.Fields, connector.SchemaField{Name: n, FieldType: resolveMixedTypes(fieldTypes[n])})
	}
	return schema
}

func jsonTypeTag(v any) string {
	switch t := v.(type) {
	case nil:
		return ""
	case bool:
		return "boolean"
	case json.Number:
		if _, err := strconv.ParseInt(t.String(), 10, 64); err == nil {
			return "integer"
		}
		return "float"
	case float64:
		if t == math.Trunc(t) && !strings.ContainsAny(strconv.FormatFloat(t, 'g', -1, 64), ".e") {
			return "integer"
		}
		return "float"
	case int, int64, int32:
		return "integer"
	case string:
		if looksLikeDatetime(t) {
			return "datetime"
		}
		return "string"
	default:
		return "json"
	}
}

func resolveMixedTypes(types map[string]bool) connector.FieldType {
	if len(types) == 1 {
		for t := range types {
			switch t {
			case "integer":
				return connector.FieldInteger
			case "float":
				return connector.FieldFloat
			case "boolean":
				return connector.FieldBoolean
			case "datetime":
				return connector.FieldDateTime
			case "json":
				return connector.FieldJSON
			}
		}
		return connector.FieldString
	}
	if len(types) == 2 && types["integer"] && types["float"] {
		return connector.FieldFloat
	}
	return connector.FieldString
}

func looksLikeBool(s string) bool {
	switch strings.ToLower(s) {
	case "true", "false", "yes", "no", "1", "0":
		return true
	}
	return false
}

func parseUintPlus(s string, bits int) bool {
	_, err := strconv.ParseUint(strings.TrimPrefix(s, "+"), 10, bits)
	return err == nil && s != "+"
}

func looksLikeDatetime(s string) bool {
	if len(s) < 8 {
		return false
	}
	// ISO 8601 variants
	if len(s) >= 19 && s[4] == '-' && s[10] == 'T' {
		return true
	}
	// Date only: 2026-05-06
	if len(s) == 10 && s[4] == '-' && s[7] == '-' {
		return parseUintPlus(s[:4], 16) && parseUintPlus(s[5:7], 8) && parseUintPlus(s[8:10], 8)
	}
	return false
}

func looksLikeJSON(s string) bool {
	t := strings.TrimSpace(s)
	return (strings.HasPrefix(t, "{") && strings.HasSuffix(t, "}")) ||
		(strings.HasPrefix(t, "[") && strings.HasSuffix(t, "]"))
}

func coerceValue(raw string, ft connector.FieldType) any {
	if raw == "" {
		return nil
	}
	switch ft {
	case connector.FieldInteger:
		if n, err := strconv.ParseInt(raw, 10, 64); err == nil {
			return n
		}
		return raw
	case connector.FieldFloat:
		f, ok := parseFloat(raw)
		if !ok {
			return raw
		}
		if math.IsNaN(f) || math.IsInf(f, 0) {
			return nil // serde_json maps non-finite floats to null
		}
		return f
	case connector.FieldBoolean:
		switch strings.ToLower(raw) {
		case "true", "yes", "1":
			return true
		case "false", "no", "0":
			return false
		}
		return raw
	case connector.FieldJSON:
		if v, err := decodeStrict([]byte(raw)); err == nil {
			return v
		}
		return raw
	}
	return raw
}
