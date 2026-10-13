package rest

// OpenAPI 3.x spec parsing for the REST datasource "Swagger" setup flow.
//
// Given a spec (URL or inline JSON/YAML) we walk paths/operations and return
// endpoint candidates, each with a draft DatasetConfig. Scope caveats:
//   - OpenAPI 3.x only; Swagger 2.0 is rejected.
//   - $ref resolution is one hop into #/components/schemas/*; deeper refs
//     degrade to json.
//   - Response inference prefers the first 2xx application/json media type.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
)

type SpecErrorKind int

const (
	SpecFetch SpecErrorKind = iota
	SpecParse
	SpecEmptySource
)

type SpecError struct {
	Kind    SpecErrorKind
	Message string
}

func (e *SpecError) Error() string {
	switch e.Kind {
	case SpecFetch:
		return "spec fetch failed: " + e.Message
	case SpecParse:
		return "spec parse failed: " + e.Message
	}
	return "spec is empty or has no 'url' / 'inline' source"
}

// SpecEndpoint is one operation from the spec plus a draft dataset config
// (schema, list_path, pagination and filter hints pre-filled).
type SpecEndpoint struct {
	Path        string        `json:"path"`
	Method      string        `json:"method"`
	OperationID *string       `json:"operation_id"`
	Summary     *string       `json:"summary"`
	Description *string       `json:"description"`
	Draft       DatasetConfig `json:"draft"`
}

// SpecHTTPClient fetches spec URLs (overridable in tests).
var SpecHTTPClient = http.DefaultClient

// ParseSpec fetches (or reads inline) and walks an OpenAPI spec.
func ParseSpec(ctx context.Context, spec Spec) ([]SpecEndpoint, error) {
	body, err := fetchSpecBody(ctx, spec)
	if err != nil {
		return nil, err
	}
	return ParseSpecBody(body)
}

// ParseSpecBody walks an OpenAPI document given as JSON or YAML text.
func ParseSpecBody(body string) ([]SpecEndpoint, error) {
	api, err := decodeOpenAPI(body)
	if err != nil {
		return nil, err
	}
	return walk(api), nil
}

func fetchSpecBody(ctx context.Context, spec Spec) (string, error) {
	if spec.Inline != nil && strings.TrimSpace(*spec.Inline) != "" {
		return *spec.Inline, nil
	}
	if spec.URL != nil && strings.TrimSpace(*spec.URL) != "" {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, *spec.URL, nil)
		if err != nil {
			return "", &SpecError{SpecFetch, err.Error()}
		}
		resp, err := SpecHTTPClient.Do(req)
		if err != nil {
			return "", &SpecError{SpecFetch, err.Error()}
		}
		defer resp.Body.Close()
		if resp.StatusCode < 200 || resp.StatusCode > 299 {
			return "", &SpecError{SpecFetch, "HTTP " + statusText(resp)}
		}
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return "", &SpecError{SpecFetch, err.Error()}
		}
		return string(b), nil
	}
	return "", &SpecError{Kind: SpecEmptySource}
}

// decodeOpenAPI parses JSON first, then YAML, into an order-preserving node
// tree, and checks the required top-level OpenAPI 3 fields.
func decodeOpenAPI(body string) (*yaml.Node, error) {
	root, err := jsonToNode([]byte(body))
	if err != nil {
		var doc yaml.Node
		if yerr := yaml.Unmarshal([]byte(body), &doc); yerr != nil {
			return nil, &SpecError{SpecParse, yerr.Error()}
		}
		if len(doc.Content) == 0 {
			return nil, &SpecError{SpecParse, "EOF while parsing a value"}
		}
		root = doc.Content[0]
	}
	root = deref(root)
	if root.Kind != yaml.MappingNode {
		return nil, &SpecError{SpecParse, "invalid type: expected struct OpenAPI"}
	}
	for _, k := range []string{"openapi", "info"} {
		if get(root, k) == nil {
			return nil, &SpecError{SpecParse, fmt.Sprintf("missing field `%s`", k)}
		}
	}
	return root, nil
}

// ── JSON → yaml.Node (keeps object key order) ───────────────────

func jsonToNode(b []byte) (*yaml.Node, error) {
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.UseNumber()
	n, err := readJSONNode(dec)
	if err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		return nil, errors.New("trailing characters")
	}
	return n, nil
}

func readJSONNode(dec *json.Decoder) (*yaml.Node, error) {
	tok, err := dec.Token()
	if err != nil {
		return nil, err
	}
	switch t := tok.(type) {
	case json.Delim:
		switch t {
		case '{':
			n := &yaml.Node{Kind: yaml.MappingNode, Tag: "!!map"}
			for dec.More() {
				kt, err := dec.Token()
				if err != nil {
					return nil, err
				}
				k, _ := kt.(string)
				v, err := readJSONNode(dec)
				if err != nil {
					return nil, err
				}
				n.Content = append(n.Content, &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: k}, v)
			}
			_, err := dec.Token()
			return n, err
		case '[':
			n := &yaml.Node{Kind: yaml.SequenceNode, Tag: "!!seq"}
			for dec.More() {
				v, err := readJSONNode(dec)
				if err != nil {
					return nil, err
				}
				n.Content = append(n.Content, v)
			}
			_, err := dec.Token()
			return n, err
		}
		return nil, fmt.Errorf("unexpected delimiter %v", t)
	case string:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!str", Value: t}, nil
	case json.Number:
		tag := "!!int"
		if strings.ContainsAny(t.String(), ".eE") {
			tag = "!!float"
		}
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: tag, Value: t.String()}, nil
	case bool:
		return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!bool", Value: strconv.FormatBool(t)}, nil
	}
	return &yaml.Node{Kind: yaml.ScalarNode, Tag: "!!null", Value: "null"}, nil
}

// ── node helpers ────────────────────────────────────────────────

func deref(n *yaml.Node) *yaml.Node {
	for n != nil && n.Kind == yaml.AliasNode {
		n = n.Alias
	}
	return n
}

// get looks up key in a mapping node (nil if absent or not a mapping).
func get(n *yaml.Node, key string) *yaml.Node {
	n = deref(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	for i := 0; i+1 < len(n.Content); i += 2 {
		if deref(n.Content[i]).Value == key {
			return deref(n.Content[i+1])
		}
	}
	return nil
}

type entry struct {
	key string
	val *yaml.Node
}

// entries returns a mapping's key/value pairs in document order.
func entries(n *yaml.Node) []entry {
	n = deref(n)
	if n == nil || n.Kind != yaml.MappingNode {
		return nil
	}
	out := make([]entry, 0, len(n.Content)/2)
	for i := 0; i+1 < len(n.Content); i += 2 {
		out = append(out, entry{deref(n.Content[i]).Value, deref(n.Content[i+1])})
	}
	return out
}

// strVal returns a string scalar's value.
func strVal(n *yaml.Node) *string {
	n = deref(n)
	if n == nil || n.Kind != yaml.ScalarNode || n.Tag == "!!null" {
		return nil
	}
	s := n.Value
	return &s
}

func isRef(n *yaml.Node) bool { return get(n, "$ref") != nil }

// ── walker ──────────────────────────────────────────────────────

func walk(api *yaml.Node) []SpecEndpoint {
	out := []SpecEndpoint{}
	for _, p := range entries(get(api, "paths")) {
		if isRef(p.val) {
			continue
		}
		// Only GET/POST: write methods are rarely useful for a read-only dataset.
		for _, m := range []struct{ key, method string }{{"get", "GET"}, {"post", "POST"}} {
			if op := get(p.val, m.key); op != nil {
				out = append(out, buildEndpoint(api, p.key, m.method, op))
			}
		}
	}
	return out
}

func buildEndpoint(api *yaml.Node, path, method string, op *yaml.Node) SpecEndpoint {
	schema, listPath := responseSchema(api, op)
	return SpecEndpoint{
		Path:        path,
		Method:      method,
		OperationID: strVal(get(op, "operationId")),
		Summary:     strVal(get(op, "summary")),
		Description: strVal(get(op, "description")),
		Draft: DatasetConfig{
			Path:          path,
			Method:        method,
			QueryParams:   map[string]string{},
			Headers:       map[string]string{},
			ListPath:      listPath,
			ItemIDField:   "id",
			Pagination:    guessPagination(op),
			FilterMapping: parametersToFilterHints(op),
			Schema:        schema,
		},
	}
}

func emptySchema() connector.Schema { return connector.Schema{Fields: []connector.SchemaField{}} }

// responseSchema finds the success response body and returns the item schema
// plus list_path.
func responseSchema(api, op *yaml.Node) (connector.Schema, *string) {
	responses := get(op, "responses")
	var resp *yaml.Node
	for _, e := range entries(responses) {
		if code, err := strconv.Atoi(e.key); err == nil && len(e.key) == 3 && code >= 200 && code < 300 {
			resp = e.val
			break
		}
	}
	if resp == nil {
		resp = get(responses, "default")
	}
	if resp == nil || isRef(resp) {
		return emptySchema(), nil
	}
	content := get(resp, "content")
	media := get(content, "application/json")
	if media == nil {
		if es := entries(content); len(es) > 0 {
			media = es[0].val
		}
	}
	if media == nil {
		return emptySchema(), nil
	}
	schema := resolveSchema(api, get(media, "schema"))
	if schema == nil {
		return emptySchema(), nil
	}
	return extractItemSchema(api, schema)
}

// resolveSchema follows a #/components/schemas/<name> ref one hop.
func resolveSchema(api, s *yaml.Node) *yaml.Node {
	if s == nil {
		return nil
	}
	ref := get(s, "$ref")
	if ref == nil {
		return s
	}
	name, ok := strings.CutPrefix(ref.Value, "#/components/schemas/")
	if !ok {
		return nil
	}
	target := get(get(get(api, "components"), "schemas"), name)
	if target == nil || isRef(target) {
		// Transitive ref: bail rather than loop.
		return nil
	}
	return target
}

func schemaType(s *yaml.Node) string {
	if t := strVal(get(s, "type")); t != nil {
		return *t
	}
	return ""
}

// extractItemSchema decides what an item looks like and where the list is:
// array → its items (root is the list); object with exactly one array
// property → that array's items at "/<prop>"; other objects → the object.
func extractItemSchema(api, schema *yaml.Node) (connector.Schema, *string) {
	switch schemaType(schema) {
	case "array":
		return itemsSchema(api, schema), nil
	case "object":
		var arrayProps []entry
		for _, p := range entries(get(schema, "properties")) {
			if r := resolveSchema(api, p.val); r != nil && schemaType(r) == "array" {
				arrayProps = append(arrayProps, entry{p.key, r})
			}
		}
		if len(arrayProps) == 1 {
			lp := "/" + arrayProps[0].key
			return itemsSchema(api, arrayProps[0].val), &lp
		}
		return objectToSchema(schema), nil
	}
	return emptySchema(), nil
}

func itemsSchema(api, arr *yaml.Node) connector.Schema {
	if item := resolveSchema(api, get(arr, "items")); item != nil {
		return objectToSchema(item)
	}
	return emptySchema()
}

func objectToSchema(s *yaml.Node) connector.Schema {
	if schemaType(s) != "object" {
		return emptySchema()
	}
	fields := []connector.SchemaField{}
	for _, p := range entries(get(s, "properties")) {
		f := connector.SchemaField{Name: p.key, FieldType: propertyFieldType(p.val)}
		if !isRef(p.val) {
			f.Description = strVal(get(p.val, "description"))
		}
		fields = append(fields, f)
	}
	return connector.Schema{Fields: fields}
}

func propertyFieldType(p *yaml.Node) connector.FieldType {
	if isRef(p) {
		return connector.FieldJSON
	}
	switch schemaType(p) {
	case "string":
		if f := strVal(get(p, "format")); f != nil && (*f == "date-time" || *f == "date") {
			return connector.FieldDateTime
		}
		return connector.FieldString
	case "integer":
		return connector.FieldInteger
	case "number":
		return connector.FieldFloat
	case "boolean":
		return connector.FieldBoolean
	}
	return connector.FieldJSON
}

// queryParams returns the operation's inline (non-$ref) query parameters.
func queryParams(op *yaml.Node) []*yaml.Node {
	params := deref(get(op, "parameters"))
	if params == nil || params.Kind != yaml.SequenceNode {
		return nil
	}
	var out []*yaml.Node
	for _, p := range params.Content {
		p = deref(p)
		if isRef(p) {
			continue
		}
		if in := strVal(get(p, "in")); in != nil && *in == "query" {
			out = append(out, p)
		}
	}
	return out
}

// parametersToFilterHints maps schema-typed query params to filter_mapping
// entries (equals only). Path/header/cookie params are ignored.
func parametersToFilterHints(op *yaml.Node) map[string]FilterParam {
	out := map[string]FilterParam{}
	for _, p := range queryParams(op) {
		name := strVal(get(p, "name"))
		if name == nil || get(p, "schema") == nil {
			continue
		}
		out[*name] = FilterParam{Param: *name, Operators: []models.FilterOperator{models.OpEquals}}
	}
	return out
}

// guessPagination looks for common pagination parameter names.
func guessPagination(op *yaml.Node) Pagination {
	names := map[string]bool{}
	for _, p := range queryParams(op) {
		if n := strVal(get(p, "name")); n != nil {
			names[strings.ToLower(*n)] = true
		}
	}
	if names["offset"] && (names["limit"] || names["per_page"]) {
		limit := "per_page"
		if names["limit"] {
			limit = "limit"
		}
		return Pagination{Kind: PaginationOffset, LimitParam: limit, OffsetParam: "offset"}
	}
	if names["page"] && (names["per_page"] || names["page_size"] || names["size"] || names["limit"]) {
		size := "per_page"
		for _, n := range []string{"per_page", "page_size", "size", "limit"} {
			if names[n] {
				size = n
				break
			}
		}
		return Pagination{Kind: PaginationPage, PageParam: "page", SizeParam: size, StartPage: 1}
	}
	return Pagination{Kind: PaginationNone}
}
