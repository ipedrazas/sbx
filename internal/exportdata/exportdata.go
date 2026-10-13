// Package exportdata serializes dataset rows to a downloadable file (CSV, JSON
// or JSONL). It is the inverse of the fileupload parsers, so a dataset can
// round-trip upload → serve → download. Pure; the caller owns fetching and IO.
package exportdata

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"github.com/ipedrazas/sbx/internal/util"
)

// Format is a supported download format.
type Format int

const (
	CSV Format = iota
	// JSON is a pretty-printed JSON array of objects.
	JSON
	// JSONL is newline-delimited JSON — one object per line.
	JSONL
)

// ParseFormat parses a user-supplied format name (case-insensitive); `ndjson`
// is an alias for `jsonl`.
func ParseFormat(s string) (Format, error) {
	switch other := strings.ToLower(strings.TrimSpace(s)); other {
	case "csv":
		return CSV, nil
	case "json":
		return JSON, nil
	case "jsonl", "ndjson":
		return JSONL, nil
	default:
		return 0, fmt.Errorf("unsupported format: '%s' (expected csv, json, or jsonl)", other)
	}
}

// Extension is the file extension (no leading dot).
func (f Format) Extension() string {
	switch f {
	case JSON:
		return "json"
	case JSONL:
		return "jsonl"
	}
	return "csv"
}

// ContentType is the MIME type for the Content-Type header.
func (f Format) ContentType() string {
	switch f {
	case JSON:
		return "application/json"
	case JSONL:
		// No registered type for NDJSON; this is the conventional choice.
		return "application/x-ndjson"
	}
	return "text/csv"
}

// SerializeRows serializes rows in the given format.
func SerializeRows(rows []map[string]any, f Format) ([]byte, error) {
	switch f {
	case JSON:
		return serializeJSON(rows)
	case JSONL:
		return serializeJSONL(rows)
	}
	return serializeCSV(rows)
}

func serializeJSON(rows []map[string]any) ([]byte, error) {
	if rows == nil {
		rows = []map[string]any{}
	}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(rows); err != nil {
		return nil, fmt.Errorf("JSON serialization failed: %v", err)
	}
	return bytes.TrimRight(buf.Bytes(), "\n"), nil
}

func serializeJSONL(rows []map[string]any) ([]byte, error) {
	var out []byte
	for _, r := range rows {
		line, err := util.MarshalJSON(r)
		if err != nil {
			return nil, fmt.Errorf("JSONL serialization failed: %v", err)
		}
		out = append(out, line...)
		out = append(out, '\n')
	}
	if out == nil {
		out = []byte{}
	}
	return out, nil
}

// serializeCSV writes a header (the union of keys) plus one record per row.
func serializeCSV(rows []map[string]any) ([]byte, error) {
	columns := CSVUnionColumns(rows)
	// No columns means no data at all — emit an empty file.
	if len(columns) == 0 {
		return []byte{}, nil
	}
	out, _ := CSVHeader(columns)
	data, _ := CSVDataRows(columns, rows)
	return append(out, data...), nil
}

// CSVUnionColumns is the union of object keys across rows, ordered by first
// appearance. Keys within a row are visited in sorted order (serde_json maps
// are key-sorted), so homogeneous rows yield alphabetical columns.
func CSVUnionColumns(rows []map[string]any) []string {
	var columns []string
	seen := map[string]bool{}
	for _, r := range rows {
		keys := make([]string, 0, len(r))
		for k := range r {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		for _, k := range keys {
			if !seen[k] {
				seen[k] = true
				columns = append(columns, k)
			}
		}
	}
	return columns
}

// CSVHeader is the newline-terminated header row for columns. Split from
// CSVDataRows so streaming exports can emit the header once up front.
func CSVHeader(columns []string) ([]byte, error) {
	var buf bytes.Buffer
	writeRecord(&buf, columns)
	return buf.Bytes(), nil
}

// CSVDataRows renders data rows (no header) projected onto columns. A missing
// column gets an empty cell; extra keys are dropped.
func CSVDataRows(columns []string, rows []map[string]any) ([]byte, error) {
	var buf bytes.Buffer
	rec := make([]string, len(columns))
	for _, r := range rows {
		if r == nil {
			continue
		}
		for i, c := range columns {
			v, ok := r[c]
			if !ok {
				rec[i] = ""
				continue
			}
			rec[i] = csvCell(v)
		}
		writeRecord(&buf, rec)
	}
	b := buf.Bytes()
	if b == nil {
		b = []byte{}
	}
	return b, nil
}

// writeRecord matches the Rust csv crate's default writer: fields quoted only
// when they contain a delimiter, quote or line break; a lone empty field is
// written as "" so the record isn't a blank line; "\n" terminator.
func writeRecord(buf *bytes.Buffer, fields []string) {
	if len(fields) == 1 && fields[0] == "" {
		buf.WriteString("\"\"\n")
		return
	}
	for i, f := range fields {
		if i > 0 {
			buf.WriteByte(',')
		}
		if strings.ContainsAny(f, ",\"\r\n") {
			buf.WriteByte('"')
			buf.WriteString(strings.ReplaceAll(f, `"`, `""`))
			buf.WriteByte('"')
		} else {
			buf.WriteString(f)
		}
	}
	buf.WriteByte('\n')
}

// csvCell renders a value as a CSV cell: strings as-is, null empty, scalars in
// canonical form, structured values as compact JSON.
func csvCell(v any) string {
	switch v.(type) {
	case nil:
		return ""
	case map[string]any, []any:
		return util.JSONString(v)
	}
	return util.ValueAsString(v)
}
