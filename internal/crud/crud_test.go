package crud

import (
	"context"
	"database/sql"
	"slices"
	"testing"

	"github.com/ipedrazas/sbx/internal/db"
	"github.com/ipedrazas/sbx/internal/models"
)

func setupDB(t *testing.T) (context.Context, *sql.DB) {
	t.Helper()
	ctx := context.Background()
	pool, err := db.OpenMemory(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { pool.Close() })
	return ctx, pool
}

func must[T any](t *testing.T) func(T, error) T {
	return func(v T, err error) T {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
}

func ptr[T any](v T) *T { return &v }

func newDataset(t *testing.T, ctx context.Context, pool *sql.DB, dsID int64, name string, ttl *int64) int64 {
	t.Helper()
	return must[int64](t)(CreateDataset(ctx, pool, name, dsID, "pull_through", ttl, nil, nil, nil, nil))
}

func TestDatasourceCRUD(t *testing.T) {
	ctx, pool := setupDB(t)
	id := must[int64](t)(CreateDatasource(ctx, pool, "test-pg", "postgres", `{"connection_string":"postgres://localhost/test"}`, nil))
	if id <= 0 {
		t.Fatal(id)
	}
	list := must[[]models.Datasource](t)(ListDatasources(ctx, pool))
	if len(list) != 1 || list[0].Name != "test-pg" || list[0].DsType != models.DatasourcePostgres {
		t.Fatalf("%+v", list)
	}
	if list[0].Config["connection_string"] != "postgres://localhost/test" || list[0].ReadOnly {
		t.Fatalf("%+v", list[0])
	}
	ds := must[*models.Datasource](t)(GetDatasource(ctx, pool, id))
	if ds.Name != "test-pg" {
		t.Fatal(ds.Name)
	}
	if !must[bool](t)(UpdateDatasource(ctx, pool, id, "renamed", `{"connection_string":"postgres://localhost/other"}`, nil)) {
		t.Fatal("update")
	}
	ds = must[*models.Datasource](t)(GetDatasource(ctx, pool, id))
	if ds.Name != "renamed" {
		t.Fatal(ds.Name)
	}
	if !must[bool](t)(SetDatasourceReadOnly(ctx, pool, id, true)) {
		t.Fatal("read_only")
	}
	if ds = must[*models.Datasource](t)(GetDatasource(ctx, pool, id)); !ds.ReadOnly {
		t.Fatal("read_only not persisted")
	}
	if must[bool](t)(SetDatasourceReadOnly(ctx, pool, 999, true)) {
		t.Fatal("missing row must report false")
	}
	if !must[bool](t)(DeleteDatasource(ctx, pool, id)) {
		t.Fatal("delete")
	}
	if ds := must[*models.Datasource](t)(GetDatasource(ctx, pool, id)); ds != nil {
		t.Fatal("still there")
	}
}

func TestDatasourceRowFallbacks(t *testing.T) {
	ctx, pool := setupDB(t)
	id := must[int64](t)(CreateDatasource(ctx, pool, "x", "nonsense", "not json", nil))
	ds := must[*models.Datasource](t)(GetDatasource(ctx, pool, id))
	if ds.DsType != models.DatasourcePostgres {
		t.Fatal(ds.DsType)
	}
	if ds.Config != nil {
		t.Fatal("invalid config must decode to null")
	}
}

func TestDatasourceDeleteBlockedByDataset(t *testing.T) {
	ctx, pool := setupDB(t)
	dsID := must[int64](t)(CreateDatasource(ctx, pool, "pg", "postgres", "{}", nil))
	newDataset(t, ctx, pool, dsID, "ds1", nil)
	if must[bool](t)(DeleteDatasource(ctx, pool, dsID)) {
		t.Fatal("delete should be blocked")
	}
}

func TestOAuthConnections(t *testing.T) {
	ctx, pool := setupDB(t)
	id := must[int64](t)(CreateOAuthConnection(ctx, pool, "google", "google", "cid", ptr("secret"), []string{"a", "b"}))
	c := must[*models.OAuthConnection](t)(GetOAuthConnection(ctx, pool, id))
	if c.ClientID != "cid" || *c.ClientSecret != "secret" || !slices.Equal(c.Scopes, []string{"a", "b"}) {
		t.Fatalf("%+v", c)
	}
	if !must[bool](t)(UpdateOAuthConnection(ctx, pool, id, "g2", "cid2", nil, nil)) {
		t.Fatal("update")
	}
	c = must[*models.OAuthConnection](t)(GetOAuthConnection(ctx, pool, id))
	if c.Name != "g2" || c.ClientSecret != nil || c.Scopes == nil || len(c.Scopes) != 0 {
		t.Fatalf("%+v", c)
	}
	if !must[bool](t)(UpdateOAuthConnectionScopes(ctx, pool, id, []string{"x"})) {
		t.Fatal("scopes")
	}
	list := must[[]models.OAuthConnection](t)(ListOAuthConnections(ctx, pool))
	if len(list) != 1 || !slices.Equal(list[0].Scopes, []string{"x"}) {
		t.Fatalf("%+v", list)
	}
	// Malformed scopes decode to [].
	if _, err := pool.ExecContext(ctx, "UPDATE oauth_connections SET scopes = 'bogus' WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	if c = must[*models.OAuthConnection](t)(GetOAuthConnection(ctx, pool, id)); c.Scopes == nil || len(c.Scopes) != 0 {
		t.Fatalf("%+v", c.Scopes)
	}

	// In use by a datasource → delete refused.
	dsID := must[int64](t)(CreateDatasource(ctx, pool, "gm", "gmail", "{}", &id))
	if ds := must[*models.Datasource](t)(GetDatasource(ctx, pool, dsID)); ds.ConnectionID == nil || *ds.ConnectionID != id {
		t.Fatal("connection_id not stored")
	}
	if must[bool](t)(DeleteOAuthConnection(ctx, pool, id)) {
		t.Fatal("delete should be blocked")
	}
	must[bool](t)(DeleteDatasource(ctx, pool, dsID))
	if !must[bool](t)(DeleteOAuthConnection(ctx, pool, id)) {
		t.Fatal("delete")
	}
	if c := must[*models.OAuthConnection](t)(GetOAuthConnection(ctx, pool, id)); c != nil {
		t.Fatal("still there")
	}
}

func TestDatasetCRUD(t *testing.T) {
	ctx, pool := setupDB(t)
	dsID := must[int64](t)(CreateDatasource(ctx, pool, "pg", "postgres", "{}", nil))
	id := newDataset(t, ctx, pool, dsID, "emails", nil)
	d := must[*models.Dataset](t)(GetDataset(ctx, pool, id))
	if d.Name != "emails" || d.Mode != models.ModePullThrough || !d.Enabled || len(d.SourceConfig) != 0 || d.SourceConfig == nil {
		t.Fatalf("%+v", d)
	}
	if d.SyntheticRowCount != nil {
		t.Fatal("create_dataset binds NULL synthetic_row_count")
	}
	if !must[bool](t)(UpdateDataset(ctx, pool, id, "cached-emails", "cached", ptr(int64(300)), nil, nil, nil, nil, nil)) {
		t.Fatal("update")
	}
	d = must[*models.Dataset](t)(GetDataset(ctx, pool, id))
	if d.Name != "cached-emails" || d.Mode != models.ModeCached || *d.CacheTTLSeconds != 300 {
		t.Fatalf("%+v", d)
	}
	// Update with source_config + extras.
	if !must[bool](t)(UpdateDataset(ctx, pool, id, "n", "mirror", nil, ptr("desc"), ptr("email"), ptr(int64(50)), ptr(`{"q":"x"}`), ptr(int64(1024)))) {
		t.Fatal("update2")
	}
	d = must[*models.Dataset](t)(GetDataset(ctx, pool, id))
	if d.SourceConfig["q"] != "x" || *d.Description != "desc" || *d.DataKind != "email" || *d.MaxRowsPerQuery != 50 || *d.MaxPageBytes != 1024 || d.CacheTTLSeconds != nil {
		t.Fatalf("%+v", d)
	}
	if !must[bool](t)(SetDatasetEnabled(ctx, pool, id, false)) {
		t.Fatal("enabled")
	}
	if !must[bool](t)(SetDatasetRequirePIIReview(ctx, pool, id, true)) {
		t.Fatal("pii review")
	}
	if !must[bool](t)(SetDatasetSnapshotAt(ctx, pool, id)) {
		t.Fatal("snapshot")
	}
	d = must[*models.Dataset](t)(GetDataset(ctx, pool, id))
	if d.Enabled || !d.RequirePiiReview || d.SnapshotAt == nil {
		t.Fatalf("%+v", d)
	}
	list := must[[]models.Dataset](t)(ListDatasets(ctx, pool))
	if len(list) != 1 {
		t.Fatal(len(list))
	}
	if !must[bool](t)(DeleteDataset(ctx, pool, id)) {
		t.Fatal("delete")
	}
	if d := must[*models.Dataset](t)(GetDataset(ctx, pool, id)); d != nil {
		t.Fatal("still there")
	}
}

func TestCreateDatasetFull(t *testing.T) {
	ctx, pool := setupDB(t)
	dsID := must[int64](t)(CreateDatasource(ctx, pool, "pg", "postgres", "{}", nil))
	id := must[int64](t)(CreateDatasetFull(ctx, pool, "s", dsID, "synthetic", nil, ptr(`{"a":1}`), nil, nil, nil, ptr(int64(42)), ptr(int64(9))))
	d := must[*models.Dataset](t)(GetDataset(ctx, pool, id))
	if d.Mode != models.ModeSynthetic || *d.SyntheticRowCount != 42 || *d.MaxPageBytes != 9 {
		t.Fatalf("%+v", d)
	}
	// Unknown mode / bad source_config fall back.
	if _, err := pool.ExecContext(ctx, "UPDATE datasets SET mode = 'zzz', source_config = 'nope' WHERE id = ?", id); err != nil {
		t.Fatal(err)
	}
	d = must[*models.Dataset](t)(GetDataset(ctx, pool, id))
	if d.Mode != models.ModePullThrough || d.SourceConfig == nil || len(d.SourceConfig) != 0 {
		t.Fatalf("%+v", d)
	}
}

func TestDatasetDuplicate(t *testing.T) {
	ctx, pool := setupDB(t)
	dsID := must[int64](t)(CreateDatasource(ctx, pool, "pg", "postgres", "{}", nil))
	id := newDataset(t, ctx, pool, dsID, "emails", ptr(int64(300)))

	f := models.NewDatasetField()
	f.DatasetID, f.SourceName, f.DisplayName, f.FieldType, f.Description = id, "subject", "Subject", "string", ptr("Email subject")
	if err := SetDatasetFields(ctx, pool, id, []models.DatasetField{f}); err != nil {
		t.Fatal(err)
	}
	filters := []models.DatasetFilter{{DatasetID: id, FieldName: "from", Operator: models.OpContains, Value: "@myorg.com", Logic: models.LogicAnd}}
	if err := SetDatasetFilters(ctx, pool, id, filters); err != nil {
		t.Fatal(err)
	}

	newID := must[*int64](t)(DuplicateDataset(ctx, pool, id, nil))
	if newID == nil || *newID == id {
		t.Fatal("bad dup id")
	}
	dup := must[*models.Dataset](t)(GetDataset(ctx, pool, *newID))
	if dup.Name != "emails (copy)" || *dup.CacheTTLSeconds != 300 {
		t.Fatalf("%+v", dup)
	}
	df := must[[]models.DatasetField](t)(GetDatasetFields(ctx, pool, *newID))
	if len(df) != 1 || df[0].SourceName != "subject" || df[0].DatasetID != *newID {
		t.Fatalf("%+v", df)
	}
	dfl := must[[]models.DatasetFilter](t)(GetDatasetFilters(ctx, pool, *newID))
	if len(dfl) != 1 || dfl[0].FieldName != "from" || dfl[0].Value != "@myorg.com" {
		t.Fatalf("%+v", dfl)
	}

	id2 := must[*int64](t)(DuplicateDataset(ctx, pool, id, ptr("my-copy")))
	if d := must[*models.Dataset](t)(GetDataset(ctx, pool, *id2)); d.Name != "my-copy" {
		t.Fatal(d.Name)
	}
	if none := must[*int64](t)(DuplicateDataset(ctx, pool, 9999, nil)); none != nil {
		t.Fatal("expected nil")
	}
}

func TestDatasetFields(t *testing.T) {
	ctx, pool := setupDB(t)
	dsID := must[int64](t)(CreateDatasource(ctx, pool, "pg", "postgres", "{}", nil))
	id := newDataset(t, ctx, pool, dsID, "ds", nil)
	a := models.NewDatasetField()
	a.DatasetID, a.SourceName, a.DisplayName, a.Description = id, "email", "Email Address", ptr("The email")
	b := models.NewDatasetField()
	b.DatasetID, b.SourceName, b.DisplayName, b.Visible, b.Sortable = id, "secret", "Secret", false, false
	if err := SetDatasetFields(ctx, pool, id, []models.DatasetField{a, b}); err != nil {
		t.Fatal(err)
	}
	loaded := must[[]models.DatasetField](t)(GetDatasetFields(ctx, pool, id))
	if len(loaded) != 2 || loaded[0].DisplayName != "Email Address" || !loaded[0].Visible || loaded[1].Visible {
		t.Fatalf("%+v", loaded)
	}
	if !loaded[0].Sortable || loaded[1].Sortable || !loaded[0].Searchable || loaded[0].PiiPolicy != "none" {
		t.Fatalf("%+v", loaded)
	}

	// PII scan results are applied atomically and stamp pii_scan_at.
	err := ApplyPIIScanResults(ctx, pool, id, []PIIScanResult{
		{SourceName: "email", PiiClass: ptr("email"), CredentialDetected: false},
		{SourceName: "secret", CredentialDetected: true},
	})
	if err != nil {
		t.Fatal(err)
	}
	loaded = must[[]models.DatasetField](t)(GetDatasetFields(ctx, pool, id))
	if *loaded[0].PiiClass != "email" || loaded[0].CredentialDetected || loaded[1].PiiClass != nil || !loaded[1].CredentialDetected {
		t.Fatalf("%+v", loaded)
	}
	if d := must[*models.Dataset](t)(GetDataset(ctx, pool, id)); d.PiiScanAt == nil {
		t.Fatal("pii_scan_at not stamped")
	}

	// Replacing with an empty list clears fields.
	if err := SetDatasetFields(ctx, pool, id, nil); err != nil {
		t.Fatal(err)
	}
	if l := must[[]models.DatasetField](t)(GetDatasetFields(ctx, pool, id)); l == nil || len(l) != 0 {
		t.Fatal("expected empty non-nil list")
	}
}

func TestDatasetFilters(t *testing.T) {
	ctx, pool := setupDB(t)
	dsID := must[int64](t)(CreateDatasource(ctx, pool, "pg", "postgres", "{}", nil))
	id := newDataset(t, ctx, pool, dsID, "ds", nil)
	filters := []models.DatasetFilter{
		{DatasetID: id, FieldName: "from", Operator: models.OpContains, Value: "@myorg.com", Logic: models.LogicAnd},
		{DatasetID: id, FieldName: "n", Operator: models.OpIn, Value: []any{1.0, 2.0}, Logic: models.LogicOr, GroupIndex: 1},
		{DatasetID: id, FieldName: "z"}, // empty operator/logic default to equals/and
	}
	if err := SetDatasetFilters(ctx, pool, id, filters); err != nil {
		t.Fatal(err)
	}
	loaded := must[[]models.DatasetFilter](t)(GetDatasetFilters(ctx, pool, id))
	if len(loaded) != 3 || loaded[0].FieldName != "from" || loaded[0].Operator != models.OpContains {
		t.Fatalf("%+v", loaded)
	}
	// Ordered by group_index, id.
	if loaded[1].FieldName != "z" || loaded[1].Operator != models.OpEquals || loaded[1].Logic != models.LogicAnd {
		t.Fatalf("%+v", loaded[1])
	}
	if loaded[2].Logic != models.LogicOr || loaded[2].GroupIndex != 1 {
		t.Fatalf("%+v", loaded[2])
	}
	if arr, ok := loaded[2].Value.([]any); !ok || len(arr) != 2 {
		t.Fatalf("%#v", loaded[2].Value)
	}
	// The stored value is JSON text.
	var raw string
	if err := pool.QueryRowContext(ctx, "SELECT value FROM dataset_filters WHERE field_name = 'from'").Scan(&raw); err != nil {
		t.Fatal(err)
	}
	if raw != `"@myorg.com"` {
		t.Fatal(raw)
	}
	// Legacy raw-string values and unknown enums fall back.
	if _, err := pool.ExecContext(ctx, "UPDATE dataset_filters SET value = 'plain', operator = 'bad', logic = 'bad' WHERE field_name = 'from'"); err != nil {
		t.Fatal(err)
	}
	loaded = must[[]models.DatasetFilter](t)(GetDatasetFilters(ctx, pool, id))
	if loaded[0].Value != "plain" || loaded[0].Operator != models.OpEquals || loaded[0].Logic != models.LogicAnd {
		t.Fatalf("%+v", loaded[0])
	}
}

func TestAgentCRUD(t *testing.T) {
	ctx, pool := setupDB(t)
	id := must[int64](t)(CreateAgent(ctx, pool, "my-agent", ptr("A test agent")))
	a := must[*models.Agent](t)(GetAgent(ctx, pool, id))
	if a.Name != "my-agent" || *a.Description != "A test agent" {
		t.Fatalf("%+v", a)
	}
	if l := must[[]models.Agent](t)(ListAgents(ctx, pool)); len(l) != 1 {
		t.Fatal(len(l))
	}
	if !must[bool](t)(UpdateAgent(ctx, pool, id, "renamed-agent", nil)) {
		t.Fatal("update")
	}
	if a = must[*models.Agent](t)(GetAgent(ctx, pool, id)); a.Name != "renamed-agent" || a.Description != nil {
		t.Fatalf("%+v", a)
	}
	if !must[bool](t)(DeleteAgent(ctx, pool, id)) {
		t.Fatal("delete")
	}
	if a := must[*models.Agent](t)(GetAgent(ctx, pool, id)); a != nil {
		t.Fatal("still there")
	}
}

func TestAgentMeta(t *testing.T) {
	ctx, pool := setupDB(t)
	meta := AgentMeta{AgentType: ptr("assistant"), Owner: ptr("ivan"), Status: ptr("active"), Mission: ptr("help")}
	id := must[int64](t)(CreateAgentWithMeta(ctx, pool, "a", nil, meta))
	a := must[*models.Agent](t)(GetAgent(ctx, pool, id))
	if *a.AgentType != "assistant" || *a.Owner != "ivan" || *a.Status != "active" || *a.Mission != "help" {
		t.Fatalf("%+v", a)
	}
	if !must[bool](t)(UpdateAgentWithMeta(ctx, pool, id, "a", nil, AgentMeta{Owner: ptr("bob")})) {
		t.Fatal("update")
	}
	a = must[*models.Agent](t)(GetAgent(ctx, pool, id))
	if *a.Owner != "bob" || a.AgentType != nil || a.Mission != nil {
		t.Fatalf("%+v", a)
	}
}

func TestAgentDatasetBindings(t *testing.T) {
	ctx, pool := setupDB(t)
	dsID := must[int64](t)(CreateDatasource(ctx, pool, "pg", "postgres", "{}", nil))
	dataset := newDataset(t, ctx, pool, dsID, "ds", nil)
	agent := must[int64](t)(CreateAgent(ctx, pool, "agent", nil))

	bid := must[int64](t)(BindAgentDataset(ctx, pool, agent, dataset, "emails", nil, nil))
	if bid <= 0 {
		t.Fatal(bid)
	}
	bs := must[[]models.AgentDataset](t)(ListAgentDatasets(ctx, pool, agent))
	if len(bs) != 1 || bs[0].Alias != "emails" || bs[0].RateLimitCount != nil {
		t.Fatalf("%+v", bs)
	}
	if r := must[*int64](t)(ResolveAgentAlias(ctx, pool, agent, "emails")); r == nil || *r != dataset {
		t.Fatal("resolve")
	}
	if r := must[*int64](t)(ResolveAgentAlias(ctx, pool, agent, "nope")); r != nil {
		t.Fatal("expected nil")
	}
	if !must[bool](t)(UpdateAgentDatasetAlias(ctx, pool, agent, dataset, "mail")) {
		t.Fatal("alias")
	}
	if r := must[*int64](t)(ResolveAgentAlias(ctx, pool, agent, "mail")); r == nil || *r != dataset {
		t.Fatal("resolve after rename")
	}
	if !must[bool](t)(UpdateAgentDatasetRateLimit(ctx, pool, agent, dataset, ptr(int64(5)), ptr(int64(60)))) {
		t.Fatal("rate")
	}
	b := must[*models.AgentDataset](t)(GetAgentBinding(ctx, pool, agent, "mail"))
	if b == nil || *b.RateLimitCount != 5 || *b.RateLimitPeriodSeconds != 60 {
		t.Fatalf("%+v", b)
	}
	if b := must[*models.AgentDataset](t)(GetAgentBinding(ctx, pool, agent, "zzz")); b != nil {
		t.Fatal("expected nil")
	}
	if ids := must[[]int64](t)(AgentsForDataset(ctx, pool, dataset)); !slices.Equal(ids, []int64{agent}) {
		t.Fatal(ids)
	}
	if !must[bool](t)(UnbindAgentDataset(ctx, pool, agent, dataset)) {
		t.Fatal("unbind")
	}
	if bs := must[[]models.AgentDataset](t)(ListAgentDatasets(ctx, pool, agent)); len(bs) != 0 {
		t.Fatal("not empty")
	}
}

func TestAgentTokens(t *testing.T) {
	ctx, pool := setupDB(t)
	agent := must[int64](t)(CreateAgent(ctx, pool, "agent", nil))
	tid := must[int64](t)(CreateAgentToken(ctx, pool, agent, "abc123hash", "test-token"))
	if v := must[*int64](t)(ValidateAgentToken(ctx, pool, "abc123hash")); v == nil || *v != agent {
		t.Fatal("validate")
	}
	if v := must[*int64](t)(ValidateAgentToken(ctx, pool, "wrong")); v != nil {
		t.Fatal("expected nil")
	}
	toks := must[[]models.AgentToken](t)(GetAgentTokens(ctx, pool, agent))
	if len(toks) != 1 || toks[0].Label != "test-token" || toks[0].ForceSimulate {
		t.Fatalf("%+v", toks)
	}
	if !must[bool](t)(DeleteAgentToken(ctx, pool, tid)) {
		t.Fatal("delete")
	}
	if v := must[*int64](t)(ValidateAgentToken(ctx, pool, "abc123hash")); v != nil {
		t.Fatal("expected nil after delete")
	}
}

func TestAgentInstallations(t *testing.T) {
	ctx, pool := setupDB(t)
	agent := must[int64](t)(CreateAgent(ctx, pool, "agent", nil))
	// An unrelated insert first, so a stale last_insert_rowid would be detectable.
	other := must[int64](t)(UpsertAgentInstallation(ctx, pool, agent, "/tmp/other"))
	id := must[int64](t)(UpsertAgentInstallation(ctx, pool, agent, "/tmp/a"))
	if id == other {
		t.Fatal("ids must differ")
	}
	must[int64](t)(CreateAgent(ctx, pool, "noise", nil))
	again := must[int64](t)(UpsertAgentInstallation(ctx, pool, agent, "/tmp/a"))
	if again != id {
		t.Fatalf("reinstall must return the existing id: %d vs %d", again, id)
	}
	list := must[[]models.AgentSkillInstallation](t)(ListAgentInstallations(ctx, pool, agent))
	if len(list) != 2 {
		t.Fatal(len(list))
	}
	inst := must[*models.AgentSkillInstallation](t)(GetAgentInstallation(ctx, pool, id))
	if inst == nil || inst.FolderPath != "/tmp/a" {
		t.Fatalf("%+v", inst)
	}
	if err := TouchAgentInstallation(ctx, pool, id); err != nil {
		t.Fatal(err)
	}
	if !must[bool](t)(DeleteAgentInstallation(ctx, pool, id)) {
		t.Fatal("delete")
	}
	if inst := must[*models.AgentSkillInstallation](t)(GetAgentInstallation(ctx, pool, id)); inst != nil {
		t.Fatal("still there")
	}
}

func TestClosedDBErrors(t *testing.T) {
	ctx, pool := setupDB(t)
	pool.Close()
	if _, err := ListDatasources(ctx, pool); err == nil {
		t.Fatal("expected error")
	}
	if _, err := GetDataset(ctx, pool, 1); err == nil {
		t.Fatal("expected error")
	}
	if _, err := DeleteDatasource(ctx, pool, 1); err == nil {
		t.Fatal("expected error")
	}
	if _, err := DuplicateDataset(ctx, pool, 1, nil); err == nil {
		t.Fatal("expected error")
	}
	if err := ApplyPIIScanResults(ctx, pool, 1, nil); err == nil {
		t.Fatal("expected error")
	}
	if _, err := CreateAgent(ctx, pool, "x", nil); err == nil {
		t.Fatal("expected error")
	}
}
