// Package rest implements the generic REST connector and the OpenAPI import
// used by the GUI's "Swagger" setup flow.
package rest

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"math"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/util"
)

// Connector queries a REST endpoint described by a DatasetConfig.
type Connector struct {
	connector.Base
	dsConfig      DatasourceConfig
	datasetConfig DatasetConfig
	client        *http.Client
	// Resolved auth material (e.g. the bearer token); nil in no-auth mode.
	authSecret *string
}

var _ connector.Connector = (*Connector)(nil)

func New(dsCfg DatasourceConfig, datasetCfg DatasetConfig, authSecret *string) (*Connector, error) {
	return &Connector{
		dsConfig:      dsCfg,
		datasetConfig: datasetCfg,
		client:        &http.Client{Timeout: 30 * time.Second},
		authSecret:    authSecret,
	}, nil
}

func (c *Connector) buildURL() (*url.URL, error) {
	joined := strings.TrimRight(c.dsConfig.BaseURL, "/") + "/" + strings.TrimLeft(c.datasetConfig.Path, "/")
	u, err := url.Parse(joined)
	if err != nil {
		return nil, connector.Config("%v", err)
	}
	if u.Scheme == "" {
		return nil, connector.Config("relative URL without a base")
	}
	if u.Host == "" && (u.Scheme == "http" || u.Scheme == "https") {
		return nil, connector.Config("empty host")
	}
	return u, nil
}

func validHeaderName(k string) bool {
	if k == "" {
		return false
	}
	for i := 0; i < len(k); i++ {
		ch := k[i]
		if ch <= ' ' || ch >= 0x7f || strings.IndexByte(`"(),/:;<=>?@[\]{}`, ch) >= 0 {
			return false
		}
	}
	return true
}

func validHeaderValue(v string) bool {
	for i := 0; i < len(v); i++ {
		ch := v[i]
		if (ch < ' ' && ch != '\t') || ch == 0x7f {
			return false
		}
	}
	return true
}

func (c *Connector) buildHeaders() (http.Header, error) {
	h := http.Header{}
	insert := func(k, v string) error {
		if !validHeaderName(k) {
			return connector.Config("invalid HTTP header name")
		}
		if !validHeaderValue(v) {
			return connector.Config("failed to parse header value")
		}
		h.Set(k, v)
		return nil
	}
	for _, m := range []map[string]string{c.dsConfig.DefaultHeaders, c.datasetConfig.Headers} {
		for _, k := range sortedKeys(m) {
			if err := insert(k, m[k]); err != nil {
				return nil, err
			}
		}
	}
	a := c.dsConfig.Auth
	switch a.Kind {
	case AuthBearer:
		if c.authSecret == nil {
			return nil, connector.Auth("bearer token secret not loaded")
		}
		if err := insert("Authorization", "Bearer "+*c.authSecret); err != nil {
			return nil, err
		}
	case AuthAPIKey:
		if a.Header != nil {
			if c.authSecret == nil {
				return nil, connector.Auth("api-key secret not loaded")
			}
			if err := insert(*a.Header, *c.authSecret); err != nil {
				return nil, err
			}
		}
	case AuthBasic:
		pwd := ""
		if c.authSecret != nil {
			pwd = *c.authSecret
		}
		enc := base64.StdEncoding.EncodeToString([]byte(a.Username + ":" + pwd))
		if err := insert("Authorization", "Basic "+enc); err != nil {
			return nil, err
		}
	}
	return h, nil
}

// authQueryParam returns (param, secret) when auth is api_key via query param.
func (c *Connector) authQueryParam() (string, string, bool) {
	a := c.dsConfig.Auth
	if a.Kind == AuthAPIKey && a.QueryParam != nil && c.authSecret != nil {
		return *a.QueryParam, *c.authSecret, true
	}
	return "", "", false
}

type pair struct{ k, v string }

// partitionFilters splits filters into pushed-down query params and the
// leftovers to apply in memory.
func (c *Connector) partitionFilters(filters []models.DatasetFilter) ([]pair, []models.DatasetFilter) {
	var pushed []pair
	var leftover []models.DatasetFilter
	for _, f := range filters {
		m, ok := c.datasetConfig.FilterMapping[f.FieldName]
		if ok && slices.Contains(m.Operators, f.Operator) {
			pushed = append(pushed, pair{m.Param, util.ValueAsString(f.Value)})
			continue
		}
		leftover = append(leftover, f)
	}
	return pushed, leftover
}

// extractItems pulls the item list out of a response body via list_path.
// Non-object items are dropped (the Go Envelope holds objects only).
func (c *Connector) extractItems(body any) []map[string]any {
	target := body
	if c.datasetConfig.ListPath != nil {
		v, ok := jsonPointer(body, *c.datasetConfig.ListPath)
		if !ok {
			v = nil
		}
		target = v
	}
	var raw []any
	switch t := target.(type) {
	case []any:
		raw = t
	case nil:
		raw = nil
	default:
		raw = []any{t}
	}
	items := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		if m, ok := it.(map[string]any); ok {
			items = append(items, m)
		}
	}
	return items
}

// jsonPointer resolves an RFC 6901 pointer like serde_json::Value::pointer.
func jsonPointer(v any, ptr string) (any, bool) {
	if ptr == "" {
		return v, true
	}
	if !strings.HasPrefix(ptr, "/") {
		return nil, false
	}
	for _, tok := range strings.Split(ptr[1:], "/") {
		tok = strings.ReplaceAll(strings.ReplaceAll(tok, "~1", "/"), "~0", "~")
		switch t := v.(type) {
		case map[string]any:
			next, ok := t[tok]
			if !ok {
				return nil, false
			}
			v = next
		case []any:
			if tok == "" || (len(tok) > 1 && tok[0] == '0') || tok[0] == '+' {
				return nil, false
			}
			i, err := strconv.ParseUint(tok, 10, 64)
			if err != nil || i >= uint64(len(t)) {
				return nil, false
			}
			v = t[i]
		default:
			return nil, false
		}
	}
	return v, true
}

func appendQuery(u *url.URL, pairs []pair) {
	if len(pairs) == 0 {
		return
	}
	var sb strings.Builder
	sb.WriteString(u.RawQuery)
	for _, p := range pairs {
		if sb.Len() > 0 {
			sb.WriteByte('&')
		}
		sb.WriteString(url.QueryEscape(p.k))
		sb.WriteByte('=')
		sb.WriteString(url.QueryEscape(p.v))
	}
	u.RawQuery = sb.String()
}

func (c *Connector) Introspect(context.Context) (*connector.Schema, error) {
	// Schema was materialised at dataset creation time; hand it back.
	s := c.datasetConfig.Schema
	if s.Fields == nil {
		s.Fields = []connector.SchemaField{}
	}
	return &s, nil
}

func (c *Connector) Query(ctx context.Context, params *connector.QueryParams) (*models.Envelope, error) {
	u, err := c.buildURL()
	if err != nil {
		return nil, err
	}
	var qp []pair
	if k, v, ok := c.authQueryParam(); ok {
		qp = append(qp, pair{k, v})
	}
	for _, k := range sortedKeys(c.datasetConfig.QueryParams) {
		qp = append(qp, pair{k, c.datasetConfig.QueryParams[k]})
	}
	pushed, leftover := c.partitionFilters(params.Filters)
	qp = append(qp, pushed...)
	pg := c.datasetConfig.Pagination
	switch pg.Kind {
	case PaginationOffset:
		qp = append(qp, pair{pg.LimitParam, strconv.FormatInt(params.Limit, 10)},
			pair{pg.OffsetParam, strconv.FormatInt(params.Offset, 10)})
	case PaginationPage:
		page := pg.StartPage + params.Offset/max(params.Limit, 1)
		qp = append(qp, pair{pg.PageParam, strconv.FormatInt(page, 10)},
			pair{pg.SizeParam, strconv.FormatInt(params.Limit, 10)})
	case PaginationCursor:
		// Cursor pagination needs stateful traversal; not implemented.
	}
	if params.Search != nil {
		qp = append(qp, pair{"q", *params.Search})
	}
	appendQuery(u, qp)

	method := c.datasetConfig.Method
	headers, err := c.buildHeaders()
	if err != nil {
		return nil, err
	}
	req, err := http.NewRequestWithContext(ctx, method, u.String(), nil)
	if err != nil {
		return nil, connector.Config("%v", err)
	}
	req.Header = headers
	slog.Info("REST connector outgoing request", "method", method, "url", u.String())
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, connector.Query("%v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		b, _ := io.ReadAll(resp.Body)
		return nil, connector.FromUpstreamHTTP(resp.StatusCode, string(b), "REST "+resp.Request.URL.String())
	}
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, connector.Query("%v", err)
	}
	body, err := util.DecodeJSON(b)
	if err != nil {
		return nil, connector.Query("error decoding response body: %v", err)
	}

	items := c.extractItems(body)
	items = connector.ApplyFiltersInMemory(items, leftover)

	if params.ItemID != nil {
		idField := c.datasetConfig.ItemIDField
		kept := items[:0]
		for _, it := range items {
			if v, ok := it[idField]; ok && util.ValueAsString(v) == *params.ItemID {
				kept = append(kept, it)
			}
		}
		items = kept
	}

	return &models.Envelope{
		Items: items,
		Meta:  models.EnvelopeMeta{Total: int64(len(items)), Limit: params.Limit, Offset: params.Offset},
	}, nil
}

func (c *Connector) TestConnection(ctx context.Context) error {
	u, err := c.buildURL()
	if err != nil {
		return err
	}
	if k, v, ok := c.authQueryParam(); ok {
		appendQuery(u, []pair{{k, v}})
	}
	headers, err := c.buildHeaders()
	if err != nil {
		return err
	}
	slog.Info("REST connector test_connection", "url", u.String())

	if req, err := http.NewRequestWithContext(ctx, http.MethodHead, u.String(), nil); err == nil {
		req.Header = headers.Clone()
		if resp, err := c.client.Do(req); err == nil {
			resp.Body.Close()
			if resp.StatusCode >= 200 && resp.StatusCode < 400 {
				return nil
			}
		}
	}
	// Some APIs don't support HEAD; fall back to GET.
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return connector.Connection("%v", err)
	}
	req.Header = headers
	resp, err := c.client.Do(req)
	if err != nil {
		return connector.Connection("%v", err)
	}
	resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 300 {
		return nil
	}
	return connector.Connection("HTTP %s", statusText(resp))
}

func statusText(resp *http.Response) string {
	if resp.Status != "" {
		return resp.Status
	}
	return fmt.Sprintf("%d %s", resp.StatusCode, http.StatusText(resp.StatusCode))
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// ---------------------------------------------------------------------------
// Schema-building helpers (dataset creation time)
// ---------------------------------------------------------------------------

// InferSchemaFromSample guesses a schema from one item (keys sorted). The
// GUI should let the user review it before saving.
func InferSchemaFromSample(sample any) connector.Schema {
	obj, ok := sample.(map[string]any)
	if !ok {
		return connector.Schema{Fields: []connector.SchemaField{}}
	}
	fields := make([]connector.SchemaField, 0, len(obj))
	for _, k := range sortedKeys(obj) {
		fields = append(fields, connector.SchemaField{Name: k, FieldType: inferFieldType(obj[k])})
	}
	return connector.Schema{Fields: fields}
}

func inferFieldType(v any) connector.FieldType {
	switch t := v.(type) {
	case bool:
		return connector.FieldBoolean
	case json.Number:
		s := t.String()
		if _, err := strconv.ParseInt(s, 10, 64); err == nil {
			return connector.FieldInteger
		}
		if _, err := strconv.ParseUint(s, 10, 64); err == nil {
			return connector.FieldInteger
		}
		return connector.FieldFloat
	case float64:
		if t == math.Trunc(t) && !math.IsInf(t, 0) && math.Abs(t) < 1<<63 {
			return connector.FieldInteger
		}
		return connector.FieldFloat
	case int, int64, int32, uint64:
		return connector.FieldInteger
	case string:
		if looksLikeDatetime(t) {
			return connector.FieldDateTime
		}
		return connector.FieldString
	case []any, map[string]any:
		return connector.FieldJSON
	}
	return connector.FieldString
}

func looksLikeDatetime(s string) bool {
	if len(s) < 10 || s[4] != '-' || s[7] != '-' {
		return false
	}
	for i := 0; i < 4; i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
