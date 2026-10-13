package rest

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/secretstore"
)

// ---------------------------------------------------------------------------
// Datasource-level config (stored in `datasources.config`)
// ---------------------------------------------------------------------------

// DatasourceConfig mirrors RestDatasourceConfig.
type DatasourceConfig struct {
	// Root URL, e.g. "https://api.example.com/v1". No trailing slash required.
	BaseURL string `json:"base_url"`
	Auth    Auth   `json:"auth"`
	// Headers applied to every request.
	DefaultHeaders map[string]string `json:"default_headers"`
	// Spec used by the GUI's "Swagger" flow; not consulted at query time.
	Spec *Spec `json:"spec"`
}

func (c *DatasourceConfig) UnmarshalJSON(b []byte) error {
	if err := requireFields(b, "base_url"); err != nil {
		return err
	}
	type plain DatasourceConfig
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	if p.Auth.Kind == "" {
		p.Auth.Kind = AuthNone
	}
	*c = DatasourceConfig(p)
	return nil
}

func (c DatasourceConfig) MarshalJSON() ([]byte, error) {
	type plain DatasourceConfig
	p := plain(c)
	if p.DefaultHeaders == nil {
		p.DefaultHeaders = map[string]string{}
	}
	return json.Marshal(p)
}

// PersistInlineSecrets moves inline secrets in the auth block to the secret store.
func (c *DatasourceConfig) PersistInlineSecrets(datasourceName string) error {
	return c.Auth.PersistInlineSecrets(datasourceName)
}

// ParseDatasourceConfig decodes a datasources.config map.
func ParseDatasourceConfig(m map[string]any) (DatasourceConfig, error) {
	var c DatasourceConfig
	err := remarshal(m, &c)
	return c, err
}

// Spec mirrors RestSpec (tag "kind"; only "openapi" exists). Either URL to
// fetch or Inline JSON/YAML text.
type Spec struct {
	Kind   string  `json:"kind"`
	URL    *string `json:"url"`
	Inline *string `json:"inline"`
}

func (s *Spec) UnmarshalJSON(b []byte) error {
	type plain Spec
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	if err := requireFields(b, "kind"); err != nil {
		return err
	}
	if p.Kind != "openapi" {
		return fmt.Errorf("unknown variant `%s`, expected `openapi`", p.Kind)
	}
	*s = Spec(p)
	return nil
}

const (
	AuthNone   = "none"
	AuthBearer = "bearer"
	AuthAPIKey = "api_key"
	AuthBasic  = "basic"
)

// Auth mirrors the RestAuth tagged union (`"kind": none|bearer|api_key|basic`).
// Inline secrets (Token, Value, Password) are only present at create time;
// PersistInlineSecrets moves them into the secret store and sets the *Ref.
type Auth struct {
	Kind string
	// bearer
	Token    *string
	TokenRef *string
	// api_key: sent in Header or as QueryParam.
	Header     *string
	QueryParam *string
	Value      *string
	ValueRef   *string
	// basic
	Username    string
	Password    *string
	PasswordRef *string
}

func (a Auth) MarshalJSON() ([]byte, error) {
	m := map[string]any{}
	put := func(k string, v *string) {
		if v != nil {
			m[k] = *v
		}
	}
	switch a.Kind {
	case AuthBearer:
		put("token", a.Token)
		put("token_ref", a.TokenRef)
	case AuthAPIKey:
		put("header", a.Header)
		put("query_param", a.QueryParam)
		put("value", a.Value)
		put("value_ref", a.ValueRef)
	case AuthBasic:
		m["username"] = a.Username
		put("password", a.Password)
		put("password_ref", a.PasswordRef)
	case AuthNone, "":
		m["kind"] = AuthNone
		return json.Marshal(m)
	default:
		return nil, fmt.Errorf("unknown auth kind %q", a.Kind)
	}
	m["kind"] = a.Kind
	return json.Marshal(m)
}

func (a *Auth) UnmarshalJSON(b []byte) error {
	if string(b) == "null" {
		return errors.New("invalid type: null, expected internally tagged enum RestAuth")
	}
	var raw struct {
		Kind        *string `json:"kind"`
		Token       *string `json:"token"`
		TokenRef    *string `json:"token_ref"`
		Header      *string `json:"header"`
		QueryParam  *string `json:"query_param"`
		Value       *string `json:"value"`
		ValueRef    *string `json:"value_ref"`
		Username    *string `json:"username"`
		Password    *string `json:"password"`
		PasswordRef *string `json:"password_ref"`
	}
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	if raw.Kind == nil {
		return errors.New("missing field `kind`")
	}
	out := Auth{Kind: *raw.Kind}
	switch *raw.Kind {
	case AuthNone:
	case AuthBearer:
		out.Token, out.TokenRef = raw.Token, raw.TokenRef
	case AuthAPIKey:
		out.Header, out.QueryParam, out.Value, out.ValueRef = raw.Header, raw.QueryParam, raw.Value, raw.ValueRef
	case AuthBasic:
		if raw.Username == nil {
			return errors.New("missing field `username`")
		}
		out.Username, out.Password, out.PasswordRef = *raw.Username, raw.Password, raw.PasswordRef
	default:
		return fmt.Errorf("unknown variant `%s`, expected one of `none`, `bearer`, `api_key`, `basic`", *raw.Kind)
	}
	*a = out
	return nil
}

// PersistInlineSecrets writes inline secret fields to the secret store under
// `dstudio-rest-<slug(name)>-<suffix>` and fills the matching *_ref. After
// this the struct is safe to persist in SQLite.
func (a *Auth) PersistInlineSecrets(datasourceName string) error {
	mkRef := func(suffix string) string {
		return fmt.Sprintf("dstudio-rest-%s-%s", slug(datasourceName), suffix)
	}
	store := func(plain *string, suffix string) (*string, error) {
		key := mkRef(suffix)
		if err := secretstore.Put(key, []byte(*plain)); err != nil {
			return nil, err
		}
		return &key, nil
	}
	switch a.Kind {
	case AuthBearer:
		if a.Token != nil {
			ref, err := store(a.Token, "bearer")
			if err != nil {
				return err
			}
			a.Token, a.TokenRef = nil, ref
		}
		if a.TokenRef == nil {
			return errors.New("bearer auth needs either `token` or `token_ref`")
		}
	case AuthAPIKey:
		if a.Header == nil && a.QueryParam == nil {
			return errors.New("api_key auth needs either `header` or `query_param`")
		}
		if a.Value != nil {
			ref, err := store(a.Value, "apikey")
			if err != nil {
				return err
			}
			a.Value, a.ValueRef = nil, ref
		}
		if a.ValueRef == nil {
			return errors.New("api_key auth needs either `value` or `value_ref`")
		}
	case AuthBasic:
		if a.Password != nil {
			ref, err := store(a.Password, "basic")
			if err != nil {
				return err
			}
			a.Password, a.PasswordRef = nil, ref
		}
		if a.PasswordRef == nil {
			return errors.New("basic auth needs either `password` or `password_ref`")
		}
	}
	return nil
}

// SecretRef returns the secret-store key this auth block points at, if any.
func (a Auth) SecretRef() *string {
	switch a.Kind {
	case AuthBearer:
		return a.TokenRef
	case AuthAPIKey:
		return a.ValueRef
	case AuthBasic:
		return a.PasswordRef
	}
	return nil
}

// LoadAuthSecret resolves the auth secret from the secret store the way the
// connector factory does (nil for no-auth).
func LoadAuthSecret(a Auth) (*string, error) {
	var missing string
	switch a.Kind {
	case AuthBearer:
		missing = "bearer token_ref missing"
	case AuthAPIKey:
		missing = "api_key value_ref missing"
	case AuthBasic:
		missing = "basic password_ref missing"
	default:
		return nil, nil
	}
	ref := a.SecretRef()
	if ref == nil {
		return nil, connector.Auth("%s", missing)
	}
	b, err := secretstore.Get(*ref)
	if err != nil {
		return nil, connector.Auth("%v", err)
	}
	s := string(b)
	return &s, nil
}

// slug lowercases ASCII, keeps Unicode alphanumerics, maps everything else to
// '-', and trims leading/trailing '-'.
func slug(s string) string {
	var sb strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsNumber(r):
			if r < 128 {
				r = unicode.ToLower(r)
			}
			sb.WriteRune(r)
		default:
			sb.WriteByte('-')
		}
	}
	return strings.Trim(sb.String(), "-")
}

// ---------------------------------------------------------------------------
// Dataset-level config (stored in `datasets.source_config`)
// ---------------------------------------------------------------------------

// DatasetConfig mirrors RestDatasetConfig.
type DatasetConfig struct {
	// Path relative to base_url, e.g. "/users".
	Path string `json:"path"`
	// HTTP method; defaults to "GET".
	Method string `json:"method"`
	// Static query params merged onto every call.
	QueryParams map[string]string `json:"query_params"`
	// Static headers merged onto every call.
	Headers map[string]string `json:"headers"`
	// JSON pointer (RFC 6901) to the item array; nil => body is the list (or one item).
	ListPath *string `json:"list_path"`
	// Field used as the item's stable ID; defaults to "id".
	ItemIDField string     `json:"item_id_field"`
	Pagination  Pagination `json:"pagination"`
	// Dataset filter field → outgoing query param. Unmapped filters run in memory.
	FilterMapping map[string]FilterParam `json:"filter_mapping"`
	// Schema captured at dataset creation; returned verbatim by Introspect.
	Schema connector.Schema `json:"schema"`
}

func (c *DatasetConfig) UnmarshalJSON(b []byte) error {
	if err := requireFields(b, "path"); err != nil {
		return err
	}
	type plain DatasetConfig
	p := plain{Method: "GET", ItemIDField: "id", Pagination: Pagination{Kind: PaginationNone}}
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	if p.Schema.Fields == nil {
		p.Schema.Fields = []connector.SchemaField{}
	}
	*c = DatasetConfig(p)
	return nil
}

func (c DatasetConfig) MarshalJSON() ([]byte, error) {
	type plain DatasetConfig
	p := plain(c)
	if p.QueryParams == nil {
		p.QueryParams = map[string]string{}
	}
	if p.Headers == nil {
		p.Headers = map[string]string{}
	}
	if p.FilterMapping == nil {
		p.FilterMapping = map[string]FilterParam{}
	}
	if p.Schema.Fields == nil {
		p.Schema.Fields = []connector.SchemaField{}
	}
	if p.Pagination.Kind == "" {
		p.Pagination.Kind = PaginationNone
	}
	return json.Marshal(p)
}

// ParseDatasetConfig decodes a datasets.source_config map.
func ParseDatasetConfig(m map[string]any) (DatasetConfig, error) {
	var c DatasetConfig
	err := remarshal(m, &c)
	return c, err
}

const (
	PaginationNone   = "none"
	PaginationOffset = "offset"
	PaginationPage   = "page"
	PaginationCursor = "cursor"
)

// Pagination mirrors the tagged enum (`"kind": none|offset|page|cursor`).
type Pagination struct {
	Kind string
	// offset
	LimitParam  string
	OffsetParam string
	// page
	PageParam string
	SizeParam string
	StartPage int64
	// cursor (JSON pointer to the cursor in the response)
	CursorParam string
	CursorPath  string
}

func (p Pagination) MarshalJSON() ([]byte, error) {
	switch p.Kind {
	case PaginationOffset:
		return json.Marshal(map[string]any{"kind": p.Kind, "limit_param": p.LimitParam, "offset_param": p.OffsetParam})
	case PaginationPage:
		return json.Marshal(map[string]any{"kind": p.Kind, "page_param": p.PageParam, "size_param": p.SizeParam, "start_page": p.StartPage})
	case PaginationCursor:
		return json.Marshal(map[string]any{"kind": p.Kind, "cursor_param": p.CursorParam, "cursor_path": p.CursorPath})
	case PaginationNone, "":
		return []byte(`{"kind":"none"}`), nil
	}
	return nil, fmt.Errorf("unknown pagination kind %q", p.Kind)
}

func (p *Pagination) UnmarshalJSON(b []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	str := func(k string) (string, error) {
		v, ok := raw[k]
		if !ok {
			return "", fmt.Errorf("missing field `%s`", k)
		}
		var s string
		err := json.Unmarshal(v, &s)
		return s, err
	}
	kind, err := str("kind")
	if err != nil {
		return err
	}
	out := Pagination{Kind: kind}
	switch kind {
	case PaginationNone:
	case PaginationOffset:
		if out.LimitParam, err = str("limit_param"); err != nil {
			return err
		}
		if out.OffsetParam, err = str("offset_param"); err != nil {
			return err
		}
	case PaginationPage:
		if out.PageParam, err = str("page_param"); err != nil {
			return err
		}
		if out.SizeParam, err = str("size_param"); err != nil {
			return err
		}
		out.StartPage = 1
		if v, ok := raw["start_page"]; ok {
			if err := json.Unmarshal(v, &out.StartPage); err != nil {
				return err
			}
		}
	case PaginationCursor:
		if out.CursorParam, err = str("cursor_param"); err != nil {
			return err
		}
		if out.CursorPath, err = str("cursor_path"); err != nil {
			return err
		}
	default:
		return fmt.Errorf("unknown variant `%s`, expected one of `none`, `offset`, `page`, `cursor`", kind)
	}
	*p = out
	return nil
}

// FilterParam maps a dataset filter onto an outgoing query param for the
// listed operators; other operators fall back to in-memory filtering.
type FilterParam struct {
	Param     string                  `json:"param"`
	Operators []models.FilterOperator `json:"operators"`
}

func (f *FilterParam) UnmarshalJSON(b []byte) error {
	if err := requireFields(b, "param"); err != nil {
		return err
	}
	type plain FilterParam
	var p plain
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	if p.Operators == nil {
		p.Operators = []models.FilterOperator{}
	}
	*f = FilterParam(p)
	return nil
}

func (f FilterParam) MarshalJSON() ([]byte, error) {
	type plain FilterParam
	p := plain(f)
	if p.Operators == nil {
		p.Operators = []models.FilterOperator{}
	}
	return json.Marshal(p)
}

// requireFields returns serde's "missing field" error for the first absent key.
func requireFields(b []byte, keys ...string) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	for _, k := range keys {
		if _, ok := raw[k]; !ok {
			return fmt.Errorf("missing field `%s`", k)
		}
	}
	return nil
}

func remarshal(in any, out any) error {
	b, err := json.Marshal(in)
	if err != nil {
		return err
	}
	return json.Unmarshal(b, out)
}
