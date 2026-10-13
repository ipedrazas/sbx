// Package connector defines the contract every datasource connector
// implements, plus the shared error type and in-memory filter helpers.
package connector

import (
	"context"
	"fmt"
	"net/http"
	"slices"
	"sort"
	"strings"

	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/util"
)

type FieldType string

const (
	FieldString   FieldType = "string"
	FieldInteger  FieldType = "integer"
	FieldFloat    FieldType = "float"
	FieldBoolean  FieldType = "boolean"
	FieldDateTime FieldType = "date_time"
	FieldJSON     FieldType = "json"
)

// String renders the type as stored in dataset_fields.field_type (note
// "datetime", not the serde "date_time").
func (f FieldType) String() string {
	if f == FieldDateTime {
		return "datetime"
	}
	return string(f)
}

type SchemaField struct {
	Name        string    `json:"name"`
	FieldType   FieldType `json:"field_type"`
	Description *string   `json:"description"`
}

type Schema struct {
	Fields []SchemaField `json:"fields"`
}

// QueryParams carries filters, pagination, search and sort into a connector.
type QueryParams struct {
	Filters []models.DatasetFilter
	Limit   int64
	Offset  int64
	Search  *string
	ItemID  *string
	// When non-nil, /search must only match these source_name fields.
	SearchableFields []string
	// Requested ordering; connectors ignore it unless SortCapability allows the field.
	Sort *models.SortSpec
}

// DefaultQueryParams mirrors the Rust Default impl (limit 50).
func DefaultQueryParams() QueryParams {
	return QueryParams{Limit: 50}
}

type SortKind int

const (
	SortNone SortKind = iota
	SortAll
	SortFields
)

// SortCapability declares which fields a connector can order by at the source.
type SortCapability struct {
	Kind   SortKind
	Fields []string
}

// Allows reports whether sourceName can be ordered by.
func (s SortCapability) Allows(sourceName string) bool {
	switch s.Kind {
	case SortAll:
		return true
	case SortFields:
		return slices.Contains(s.Fields, sourceName)
	}
	return false
}

// JiraSortableFields are the source_name fields Jira can ORDER BY.
var JiraSortableFields = []string{
	"key", "summary", "status", "issue_type", "priority", "assignee", "reporter", "project", "created", "updated",
}

// SortCapabilityFor is the credential-free sort capability for a datasource
// type and dataset mode.
func SortCapabilityFor(dsType models.DatasourceType, mode models.DatasetMode) SortCapability {
	if mode != models.ModePullThrough {
		return SortCapability{Kind: SortAll}
	}
	switch dsType {
	case models.DatasourcePostgres:
		return SortCapability{Kind: SortAll}
	case models.DatasourceJira:
		return SortCapability{Kind: SortFields, Fields: slices.Clone(JiraSortableFields)}
	}
	return SortCapability{Kind: SortNone}
}

type ErrorKind int

const (
	ErrConnection ErrorKind = iota
	ErrQuery
	ErrBadInput
	ErrIntrospection
	ErrAuth
	ErrConfig
	ErrUnsupported
	ErrUpstreamHTTP
)

// Error is the connector error type. Use errors.As to inspect Kind.
type Error struct {
	Kind    ErrorKind
	Message string
	// Status is set for ErrUpstreamHTTP.
	Status int
}

func (e *Error) Error() string {
	switch e.Kind {
	case ErrConnection:
		return "connection failed: " + e.Message
	case ErrQuery:
		return "query failed: " + e.Message
	case ErrBadInput:
		return "invalid request: " + e.Message
	case ErrIntrospection:
		return "introspection failed: " + e.Message
	case ErrAuth:
		return "authentication expired — re-authenticate this datasource: " + e.Message
	case ErrConfig:
		return "configuration error: " + e.Message
	case ErrUnsupported:
		return "unsupported operation: " + e.Message
	case ErrUpstreamHTTP:
		return fmt.Sprintf("upstream HTTP %d: %s", e.Status, e.Message)
	}
	return e.Message
}

func errf(kind ErrorKind, format string, args ...any) *Error {
	return &Error{Kind: kind, Message: fmt.Sprintf(format, args...)}
}

func Connection(format string, a ...any) *Error    { return errf(ErrConnection, format, a...) }
func Query(format string, a ...any) *Error         { return errf(ErrQuery, format, a...) }
func BadInput(format string, a ...any) *Error      { return errf(ErrBadInput, format, a...) }
func Introspection(format string, a ...any) *Error { return errf(ErrIntrospection, format, a...) }
func Auth(format string, a ...any) *Error          { return errf(ErrAuth, format, a...) }
func Config(format string, a ...any) *Error        { return errf(ErrConfig, format, a...) }
func Unsupported(format string, a ...any) *Error   { return errf(ErrUnsupported, format, a...) }

// UpstreamHTTP builds an upstream error preserving the status code.
func UpstreamHTTP(status int, message string) *Error {
	return &Error{Kind: ErrUpstreamHTTP, Status: status, Message: message}
}

// FromUpstreamHTTP classifies an upstream HTTP failure: 401/403 → Auth,
// anything else → UpstreamHTTP with the original status.
func FromUpstreamHTTP(status int, body, label string) *Error {
	msg := fmt.Sprintf("%s: HTTP %d %s — %s", label, status, http.StatusText(status), body)
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		return Auth("%s", msg)
	}
	return UpstreamHTTP(status, msg)
}

// Connector is implemented by every datasource connector. Embed Base to get
// the default (unsupported / no-op) implementations of the optional methods.
type Connector interface {
	Introspect(ctx context.Context) (*Schema, error)
	Query(ctx context.Context, params *QueryParams) (*models.Envelope, error)
	SortCapability() SortCapability
	TestConnection(ctx context.Context) error
	InvokeAction(ctx context.Context, actionType string, params map[string]any) (any, error)
	SimulateAction(ctx context.Context, actionType string, params map[string]any) (any, error)
	// GenerateSyntheticRows returns domain-specific fake rows, or ok=false to
	// fall back to the generic generator.
	GenerateSyntheticRows(fields []models.DatasetField, count int64) (rows []map[string]any, ok bool)
}

// Base provides the default implementations of the optional Connector methods.
type Base struct{}

func (Base) SortCapability() SortCapability { return SortCapability{Kind: SortNone} }

func (Base) InvokeAction(_ context.Context, actionType string, _ map[string]any) (any, error) {
	return nil, Unsupported("action '%s' is not supported by this connector", actionType)
}

func (Base) SimulateAction(_ context.Context, actionType string, _ map[string]any) (any, error) {
	return nil, Unsupported("action '%s' has no simulator fixture", actionType)
}

func (Base) GenerateSyntheticRows([]models.DatasetField, int64) ([]map[string]any, bool) {
	return nil, false
}

// ApplyFiltersInMemory filters items. Filters are grouped by GroupIndex;
// within a group they are OR-ed when the group's first filter has logic "or",
// otherwise AND-ed. Groups are always AND-ed together.
func ApplyFiltersInMemory(items []map[string]any, filters []models.DatasetFilter) []map[string]any {
	if len(filters) == 0 {
		return items
	}
	groups := map[int32][]models.DatasetFilter{}
	var keys []int32
	for _, f := range filters {
		if _, ok := groups[f.GroupIndex]; !ok {
			keys = append(keys, f.GroupIndex)
		}
		groups[f.GroupIndex] = append(groups[f.GroupIndex], f)
	}
	sort.Slice(keys, func(i, j int) bool { return keys[i] < keys[j] })

	out := make([]map[string]any, 0, len(items))
	for _, item := range items {
		keep := true
		for _, k := range keys {
			g := groups[k]
			isOr := g[0].Logic == models.LogicOr
			var ok bool
			if isOr {
				ok = slices.ContainsFunc(g, func(f models.DatasetFilter) bool { return MatchesFilter(item, f) })
			} else {
				ok = !slices.ContainsFunc(g, func(f models.DatasetFilter) bool { return !MatchesFilter(item, f) })
			}
			if !ok {
				keep = false
				break
			}
		}
		if keep {
			out = append(out, item)
		}
	}
	return out
}

// MatchesFilter evaluates a single filter against an item using string comparisons.
func MatchesFilter(item map[string]any, f models.DatasetFilter) bool {
	raw, present := item[f.FieldName]
	if !present {
		return f.Operator == models.OpNotEquals || f.Operator == models.OpNotContains || f.Operator == models.OpNotIn
	}
	val := util.ValueAsString(raw)
	fv := util.ValueAsString(f.Value)
	switch f.Operator {
	case models.OpEquals:
		return val == fv
	case models.OpNotEquals:
		return val != fv
	case models.OpContains:
		return strings.Contains(strings.ToLower(val), strings.ToLower(fv))
	case models.OpNotContains:
		return !strings.Contains(strings.ToLower(val), strings.ToLower(fv))
	case models.OpGreaterThan:
		return val > fv
	case models.OpLessThan:
		return val < fv
	case models.OpIn:
		arr, ok := f.Value.([]any)
		if !ok {
			return false
		}
		return slices.ContainsFunc(arr, func(v any) bool { return util.ValueAsString(v) == val })
	case models.OpNotIn:
		arr, ok := f.Value.([]any)
		if !ok {
			return true
		}
		return !slices.ContainsFunc(arr, func(v any) bool { return util.ValueAsString(v) == val })
	}
	return false
}

// ParseAdhocFilter parses the live API's `?filter=FIELD:OP:VALUE` form. Only
// the first two ':' are split on, so VALUE may contain colons.
func ParseAdhocFilter(raw string, groupIndex int32) (models.DatasetFilter, error) {
	parts := strings.SplitN(raw, ":", 3)
	field := strings.TrimSpace(parts[0])
	op := ""
	if len(parts) > 1 {
		op = strings.TrimSpace(parts[1])
	}
	if field == "" || op == "" {
		return models.DatasetFilter{}, fmt.Errorf("filter '%s': expected FIELD:OP:VALUE", raw)
	}
	if len(parts) < 3 {
		return models.DatasetFilter{}, fmt.Errorf("filter '%s': expected FIELD:OP:VALUE (missing value)", raw)
	}
	value := parts[2]

	var operator models.FilterOperator
	var v any = value
	switch op {
	case "eq":
		operator = models.OpEquals
	case "ne":
		operator = models.OpNotEquals
	case "contains":
		operator = models.OpContains
	case "ncontains":
		operator = models.OpNotContains
	case "gt":
		operator = models.OpGreaterThan
	case "lt":
		operator = models.OpLessThan
	case "in", "nin":
		operator = models.OpIn
		if op == "nin" {
			operator = models.OpNotIn
		}
		var arr []any
		for _, p := range strings.Split(value, ",") {
			arr = append(arr, strings.TrimSpace(p))
		}
		v = arr
	default:
		return models.DatasetFilter{}, fmt.Errorf("filter '%s': unknown operator '%s' (use eq, ne, contains, ncontains, gt, lt, in, nin)", raw, op)
	}
	return models.DatasetFilter{
		FieldName:  field,
		Operator:   operator,
		Value:      v,
		Logic:      models.LogicAnd,
		GroupIndex: groupIndex,
	}, nil
}
