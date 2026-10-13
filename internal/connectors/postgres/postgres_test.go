package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/big"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
)

// ── fake querier ───────────────────────────────────────────────

type fakeResult struct {
	fds  []pgconn.FieldDescription
	rows [][]any
	raw  [][][]byte
	err  error
}

type call struct {
	sql  string
	args []any
}

type fakeDB struct {
	calls []call
	// handler picks a result for a query.
	handler func(sql string, args []any) fakeResult
}

func (f *fakeDB) Query(_ context.Context, sql string, args ...any) (pgx.Rows, error) {
	f.calls = append(f.calls, call{sql, args})
	r := f.handler(sql, args)
	if r.err != nil {
		return nil, r.err
	}
	return &fakeRows{res: r, i: -1}, nil
}

func (f *fakeDB) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	rows, err := f.Query(ctx, sql, args...)
	return &fakeRow{rows: rows, err: err}
}

type fakeRow struct {
	rows pgx.Rows
	err  error
}

func (r *fakeRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	defer r.rows.Close()
	if !r.rows.Next() {
		return pgx.ErrNoRows
	}
	return r.rows.Scan(dest...)
}

type fakeRows struct {
	res fakeResult
	i   int
}

func (r *fakeRows) Close()                                       {}
func (r *fakeRows) Err() error                                   { return nil }
func (r *fakeRows) CommandTag() pgconn.CommandTag                { return pgconn.CommandTag{} }
func (r *fakeRows) FieldDescriptions() []pgconn.FieldDescription { return r.res.fds }
func (r *fakeRows) Conn() *pgx.Conn                              { return nil }
func (r *fakeRows) TypeMap() *pgtype.Map                         { return typeMap }
func (r *fakeRows) Next() bool {
	r.i++
	return r.i < len(r.res.rows)
}
func (r *fakeRows) Values() ([]any, error) { return r.res.rows[r.i], nil }
func (r *fakeRows) RawValues() [][]byte {
	if r.res.raw != nil {
		return r.res.raw[r.i]
	}
	return nil
}
func (r *fakeRows) Scan(dest ...any) error {
	row := r.res.rows[r.i]
	for i, d := range dest {
		switch p := d.(type) {
		case *int64:
			*p = row[i].(int64)
		case *int32:
			*p = row[i].(int32)
		case *string:
			*p = row[i].(string)
		default:
			return fmt.Errorf("unsupported scan dest %T", d)
		}
	}
	return nil
}

func fd(name string, oid uint32) pgconn.FieldDescription {
	return pgconn.FieldDescription{Name: name, DataTypeOID: oid}
}

func newFake(cfg Config, h func(string, []any) fakeResult) (*Connector, *fakeDB) {
	db := &fakeDB{handler: h}
	return &Connector{pool: db, config: cfg}, db
}

func strp(s string) *string { return &s }

// ── pure helpers ───────────────────────────────────────────────

func TestOrderByClause(t *testing.T) {
	if orderByClause(nil) != "" {
		t.Fatal("no sort")
	}
	if got := orderByClause(&models.SortSpec{Field: "created_at"}); got != ` ORDER BY "created_at" ASC` {
		t.Fatal(got)
	}
	if got := orderByClause(&models.SortSpec{Field: "created_at", Descending: true}); got != ` ORDER BY "created_at" DESC` {
		t.Fatal(got)
	}
	if got := orderByClause(&models.SortSpec{Field: `a"b`}); got != ` ORDER BY "a""b" ASC` {
		t.Fatal(got)
	}
}

func TestBuildWhere(t *testing.T) {
	filters := []models.DatasetFilter{
		{FieldName: "a", Operator: models.OpEquals, Value: "1", Logic: models.LogicOr, GroupIndex: 1},
		{FieldName: "b", Operator: models.OpNotEquals, Value: json.Number("2"), Logic: models.LogicOr, GroupIndex: 1},
		{FieldName: "c", Operator: models.OpContains, Value: "x", Logic: models.LogicAnd, GroupIndex: 0},
		{FieldName: "d", Operator: models.OpNotContains, Value: "y", Logic: models.LogicAnd, GroupIndex: 0},
		{FieldName: "e", Operator: models.OpGreaterThan, Value: 5.0, GroupIndex: 2},
		{FieldName: "f", Operator: models.OpLessThan, Value: "9", GroupIndex: 3},
		{FieldName: "g", Operator: models.OpIn, Value: []any{"1"}, GroupIndex: 4},
	}
	where, binds := buildWhere(filters, strp("hi"), []string{"name", "title"})
	want := ` WHERE ("c"::text ILIKE $3 AND "d"::text NOT ILIKE $4) AND ("a" = $1 OR "b" != $2) AND "e" > $5 AND "f" < $6 AND ("name"::text ILIKE $7 OR "title"::text ILIKE $7)`
	if where != want {
		t.Fatalf("\n got %s\nwant %s", where, want)
	}
	wantBinds := []string{"1", "2", "%x%", "%y%", "5", "9", "%hi%"}
	if strings.Join(binds, "|") != strings.Join(wantBinds, "|") {
		t.Fatalf("%q", binds)
	}

	where, binds = buildWhere(nil, strp("q"), nil)
	if where != " WHERE FALSE" || len(binds) != 0 {
		t.Fatal(where, binds)
	}
	where, _ = buildWhere(nil, nil, nil)
	if where != "" {
		t.Fatal(where)
	}
}

func TestPgTypeToFieldType(t *testing.T) {
	cases := map[string]connector.FieldType{
		"INT4": connector.FieldInteger, "bigint": connector.FieldInteger, "numeric": connector.FieldFloat,
		"float8": connector.FieldFloat, "bool": connector.FieldBoolean, "timestamptz": connector.FieldDateTime,
		"timestamp without time zone": connector.FieldDateTime, "jsonb": connector.FieldJSON,
		"text": connector.FieldString, "uuid": connector.FieldString,
	}
	for in, want := range cases {
		if got := pgTypeToFieldType(in); got != want {
			t.Fatalf("%s: %s", in, got)
		}
	}
	if typeName(pgtype.Int4OID) != "int4" || typeName(999999) != "" {
		t.Fatal("typeName")
	}
}

func TestConvertValue(t *testing.T) {
	ts := time.Date(2024, 1, 2, 3, 4, 5, 0, time.UTC)
	var num pgtype.Numeric
	_ = num.Scan("1.5")
	cases := []struct {
		typ  string
		in   any
		raw  []byte
		want any
	}{
		{"int2", int16(2), nil, int64(2)},
		{"int4", int32(4), nil, int64(4)},
		{"int8", int64(8), nil, int64(8)},
		{"bool", true, nil, true},
		{"text", "hi", nil, "hi"},
		{"float8", 1.25, nil, 1.25},
		{"float4", float32(0.5), nil, 0.5},
		{"timestamptz", ts, nil, "2024-01-02T03:04:05Z"},
		{"timestamp", ts, nil, "2024-01-02T03:04:05"},
		{"date", ts, nil, "2024-01-02"},
		{"numeric", num, nil, 1.5},
		{"numeric", pgtype.Numeric{Int: big.NewInt(1), Exp: 0, NaN: true, Valid: true}, nil, nil},
		{"bytea", []byte{1}, nil, nil},
		{"text", nil, nil, nil},
	}
	for _, c := range cases {
		if got := convertValue(c.typ, c.in, c.raw); got != c.want {
			t.Fatalf("%s %v: got %#v want %#v", c.typ, c.in, got, c.want)
		}
	}
	u := convertValue("uuid", [16]byte{0x12, 0x34, 0x56, 0x78, 0x9a, 0xbc, 0xde, 0xf0, 1, 2, 3, 4, 5, 6, 7, 8}, nil)
	if u != "12345678-9abc-def0-0102-030405060708" {
		t.Fatal(u)
	}
	j := convertValue("jsonb", map[string]any{"a": 1.0}, append([]byte{1}, `{"a":12345678901234567890}`...))
	if jm := j.(map[string]any); jm["a"].(json.Number).String() != "12345678901234567890" {
		t.Fatal(j)
	}
	j = convertValue("json", map[string]any{"a": 1.0}, []byte("not json"))
	if j.(map[string]any)["a"] != 1.0 {
		t.Fatal(j)
	}
	arr := convertValue("_int4", []any{int32(1), int32(2)}, nil).([]any)
	if arr[1] != int64(2) {
		t.Fatal(arr)
	}
	if convertValue("float8", 1e400*0, nil) != 0.0 {
		t.Fatal("zero")
	}
}

func TestConfig(t *testing.T) {
	c, err := ParseConfig(map[string]any{"connection_string": "postgres://x", "table": "users"})
	if err != nil || c.Table == nil || *c.Table != "users" || c.Query != nil {
		t.Fatal(c, err)
	}
	if _, err := ParseConfig(map[string]any{"table": "users"}); err == nil || err.Error() != "missing field `connection_string`" {
		t.Fatal(err)
	}
	b, _ := json.Marshal(c)
	if string(b) != `{"connection_string":"postgres://x","table":"users","query":null}` {
		t.Fatal(string(b))
	}
	if _, err := New(context.Background(), Config{ConnectionString: "::not a url"}); err == nil {
		t.Fatal("expected error")
	}
}

func TestBaseQuery(t *testing.T) {
	c := &Connector{config: Config{Table: strp(`we"ird`)}}
	if q, _ := c.baseQuery(); q != `SELECT * FROM "we""ird"` {
		t.Fatal(q)
	}
	c = &Connector{config: Config{Query: strp("select 1")}}
	if q, _ := c.baseQuery(); q != "SELECT * FROM (select 1) AS _dstudio_subq" {
		t.Fatal(q)
	}
	c = &Connector{config: Config{Query: strp("x"), Table: strp("y")}}
	_, err := c.baseQuery()
	var ce *connector.Error
	if !errors.As(err, &ce) || ce.Kind != connector.ErrConfig {
		t.Fatal(err)
	}
	if _, err := c.Introspect(context.Background()); err == nil {
		t.Fatal("expected config error")
	}
	if _, err := c.Query(context.Background(), &connector.QueryParams{}); err == nil {
		t.Fatal("expected config error")
	}
	if c.SortCapability().Kind != connector.SortAll {
		t.Fatal("sort")
	}
}

// ── connector flows over the fake ──────────────────────────────

func usersHandler(sql string, args []any) fakeResult {
	switch {
	case strings.HasPrefix(sql, "SELECT COUNT(*) as cnt"):
		return fakeResult{rows: [][]any{{int64(42)}}}
	case strings.HasSuffix(sql, "LIMIT 1"):
		return fakeResult{
			fds:  []pgconn.FieldDescription{fd("id", pgtype.Int4OID), fd("name", pgtype.TextOID), fd("meta", pgtype.JSONOID)},
			rows: [][]any{{int32(1), "a", map[string]any{}}},
		}
	default:
		return fakeResult{
			fds: []pgconn.FieldDescription{fd("id", pgtype.Int4OID), fd("name", pgtype.TextOID), fd("meta", pgtype.JSONOID)},
			rows: [][]any{
				{int32(1), "alice", map[string]any{"k": 1.0}},
				{int32(2), "bob", nil},
			},
			raw: [][][]byte{{nil, nil, []byte(`{"k":1}`)}, {nil, nil, nil}},
		}
	}
}

func TestIntrospectFromRow(t *testing.T) {
	c, db := newFake(Config{Table: strp("users")}, usersHandler)
	s, err := c.Introspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Fields) != 3 || s.Fields[0].FieldType != connector.FieldInteger || s.Fields[2].FieldType != connector.FieldJSON {
		t.Fatalf("%+v", s.Fields)
	}
	if db.calls[0].sql != `SELECT * FROM "users" LIMIT 1` {
		t.Fatal(db.calls[0].sql)
	}
}

func TestIntrospectEmptyTableFallsBack(t *testing.T) {
	c, db := newFake(Config{Table: strp("users")}, func(sql string, args []any) fakeResult {
		switch {
		case strings.HasSuffix(sql, "LIMIT 1"):
			return fakeResult{}
		case strings.HasPrefix(sql, "SELECT COUNT(*) FROM information_schema.columns"):
			return fakeResult{rows: [][]any{{int64(2)}}}
		default:
			return fakeResult{rows: [][]any{{"id", "integer"}, {"created", "timestamp with time zone"}}}
		}
	})
	s, err := c.Introspect(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(s.Fields) != 2 || s.Fields[1].FieldType != connector.FieldDateTime {
		t.Fatalf("%+v", s)
	}
	if db.calls[2].args[1] != int64(500) {
		t.Fatal(db.calls[2].args)
	}

	// Query mode with no rows → empty schema.
	c, _ = newFake(Config{Query: strp("select 1")}, func(string, []any) fakeResult { return fakeResult{} })
	s, err = c.Introspect(context.Background())
	if err != nil || len(s.Fields) != 0 {
		t.Fatal(s, err)
	}
}

func TestQuery(t *testing.T) {
	c, db := newFake(Config{Table: strp("users")}, usersHandler)
	params := connector.DefaultQueryParams()
	params.Search = strp("ali")
	params.SearchableFields = []string{"name"}
	params.Sort = &models.SortSpec{Field: "name", Descending: true}
	params.Filters = []models.DatasetFilter{
		{FieldName: "id", Operator: models.OpGreaterThan, Value: "0"},
		{FieldName: "name", Operator: models.OpNotIn, Value: []any{"bob"}, GroupIndex: 1},
	}
	env, err := c.Query(context.Background(), &params)
	if err != nil {
		t.Fatal(err)
	}
	if env.Meta.Total != 42 || env.Meta.Limit != 50 || len(env.Items) != 1 || env.Items[0]["name"] != "alice" {
		t.Fatalf("%+v", env)
	}
	if env.Items[0]["id"] != int64(1) || env.Items[0]["meta"].(map[string]any)["k"].(json.Number) != "1" {
		t.Fatalf("%+v", env.Items[0])
	}
	// calls: LIMIT 1 introspect, count, data
	count, data := db.calls[1], db.calls[2]
	if count.sql != `SELECT COUNT(*) as cnt FROM (SELECT * FROM "users") AS _c WHERE "id" > $1 AND ("name"::text ILIKE $2)` {
		t.Fatal(count.sql)
	}
	if data.sql != `SELECT * FROM "users" WHERE "id" > $1 AND ("name"::text ILIKE $2) ORDER BY "name" DESC LIMIT 50 OFFSET 0` {
		t.Fatal(data.sql)
	}
	if len(data.args) != 2 || data.args[1] != "%ali%" {
		t.Fatal(data.args)
	}

	params = connector.DefaultQueryParams()
	params.ItemID = strp("2")
	env, err = c.Query(context.Background(), &params)
	if err != nil || len(env.Items) != 1 || env.Items[0]["name"] != "bob" || env.Items[0]["meta"] != nil {
		t.Fatal(env, err)
	}
}

func TestQueryErrors(t *testing.T) {
	boom := errors.New("boom")
	c, _ := newFake(Config{Table: strp("users")}, func(string, []any) fakeResult { return fakeResult{err: boom} })
	p := connector.DefaultQueryParams()
	_, err := c.Query(context.Background(), &p)
	var ce *connector.Error
	if !errors.As(err, &ce) || ce.Kind != connector.ErrQuery {
		t.Fatal(err)
	}
	if _, err := c.Introspect(context.Background()); !errors.As(err, &ce) || ce.Kind != connector.ErrIntrospection {
		t.Fatal(err)
	}
	if err := c.TestConnection(context.Background()); !errors.As(err, &ce) || ce.Kind != connector.ErrConnection {
		t.Fatal(err)
	}
	if _, _, err := c.ListTables(context.Background(), 10, 0); err == nil {
		t.Fatal("expected")
	}
	if _, _, err := c.IntrospectTable(context.Background(), "x", 10, 0); err == nil {
		t.Fatal("expected")
	}
	p.Search = strp("x")
	if _, err := c.Query(context.Background(), &p); err == nil {
		t.Fatal("expected")
	}
	c.Close()
}

func TestListTablesAndTestConnection(t *testing.T) {
	c, db := newFake(Config{Table: strp("users")}, func(sql string, args []any) fakeResult {
		switch {
		case sql == "SELECT 1":
			return fakeResult{rows: [][]any{{int32(1)}}}
		case strings.HasPrefix(sql, "SELECT COUNT(*)"):
			return fakeResult{rows: [][]any{{int64(3)}}}
		default:
			return fakeResult{rows: [][]any{{"public", "a"}, {"public", "b"}}}
		}
	})
	tables, total, err := c.ListTables(context.Background(), 2, 0)
	if err != nil || total != 3 || len(tables) != 2 || tables[1].Name != "b" {
		t.Fatal(tables, total, err)
	}
	if db.calls[1].args[0] != int64(2) {
		t.Fatal(db.calls[1].args)
	}
	cols, total, err := c.IntrospectTable(context.Background(), "users", 10, 0)
	if err != nil || total != 3 || len(cols) != 2 {
		t.Fatal(cols, total, err)
	}
	if err := c.TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
}

// Live test, opt-in: DSTUDIO_TEST_PG_URL=postgres://... go test ./internal/connectors/postgres
func TestLivePostgres(t *testing.T) {
	url := os.Getenv("DSTUDIO_TEST_PG_URL")
	if url == "" {
		t.Skip("DSTUDIO_TEST_PG_URL not set")
	}
	ctx := context.Background()
	q := "SELECT 1::int4 AS id, 'alice'::text AS name, 1.5::float8 AS score, now() AS ts, '{\"a\":1}'::jsonb AS meta"
	c, err := New(ctx, Config{ConnectionString: url, Query: &q})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if err := c.TestConnection(ctx); err != nil {
		t.Fatal(err)
	}
	s, err := c.Introspect(ctx)
	if err != nil || len(s.Fields) != 5 {
		t.Fatal(s, err)
	}
	p := connector.DefaultQueryParams()
	p.Search = strp("ali")
	p.Filters = []models.DatasetFilter{{FieldName: "id", Operator: models.OpEquals, Value: "1"}}
	env, err := c.Query(ctx, &p)
	if err != nil || env.Meta.Total != 1 || env.Items[0]["score"] != 1.5 {
		t.Fatal(env, err)
	}
	if _, _, err := c.ListTables(ctx, 10, 0); err != nil {
		t.Fatal(err)
	}
}
