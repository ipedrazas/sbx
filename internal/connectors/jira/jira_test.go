package jira

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/oauth"
	"github.com/ipedrazas/sbx/internal/secretstore"
)

func kindOf(t *testing.T, err error) connector.ErrorKind {
	t.Helper()
	var ce *connector.Error
	if !errors.As(err, &ce) {
		t.Fatalf("not a connector error: %v", err)
	}
	return ce.Kind
}

func newConn(cloud *string) *Connector {
	return New(1, oauth.Credentials{ClientID: "x"}, cloud)
}

// setup seeds a token and points the connector at a test server.
func setup(t *testing.T, h http.HandlerFunc) *httptest.Server {
	t.Helper()
	secretstore.Init(secretstore.NewMemory())
	if err := oauth.StoreTokens(1, &oauth.Tokens{AccessToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	old := atlassianBase
	atlassianBase = srv.URL
	t.Cleanup(func() { atlassianBase = old; srv.Close() })
	return srv
}

// ── sort ──

func TestNoSortFallsBackToDefault(t *testing.T) {
	if got := jqlOrderClause(nil); got != "ORDER BY updated DESC" {
		t.Fatal(got)
	}
}

func TestOrderableFieldIsUsedWithDirection(t *testing.T) {
	if got := jqlOrderClause(&models.SortSpec{Field: "summary"}); got != "ORDER BY summary ASC" {
		t.Fatal(got)
	}
}

func TestSourceNameMapsToJQLField(t *testing.T) {
	if got := jqlOrderClause(&models.SortSpec{Field: "issue_type", Descending: true}); got != "ORDER BY issuetype DESC" {
		t.Fatal(got)
	}
}

func TestUnsortableFieldIsNoop(t *testing.T) {
	if got := jqlOrderClause(&models.SortSpec{Field: "description", Descending: true}); got != "ORDER BY updated DESC" {
		t.Fatal(got)
	}
}

func TestCapabilityExcludesNonOrderableReturnedFields(t *testing.T) {
	cap := newConn(nil).SortCapability()
	if !cap.Allows("updated") || !cap.Allows("issue_type") || cap.Allows("description") || cap.Allows("labels") {
		t.Fatalf("unexpected capability %+v", cap)
	}
}

func TestEverySortableFieldIsReturnedByIssueJSON(t *testing.T) {
	id, key := "1", "K-1"
	issue := issueToJSON(&jiraIssue{ID: &id, Key: &key, Fields: &jiraFields{}})
	for _, f := range connector.JiraSortableFields {
		if _, ok := issue[f]; !ok {
			t.Fatalf("sortable field `%s` is not an item key", f)
		}
	}
}

// ── create_issue ──

func TestCreateIssueBodyMinimumShape(t *testing.T) {
	p, err := parseCreateIssueParams(map[string]any{"project_key": "PROJ", "summary": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	body := buildCreateIssueBody(p)
	f := body["fields"].(map[string]any)
	if f["project"].(map[string]any)["key"] != "PROJ" || f["summary"] != "hi" || f["issuetype"].(map[string]any)["name"] != "Task" {
		t.Fatalf("bad body %v", body)
	}
	for _, k := range []string{"description", "priority", "labels", "assignee"} {
		if _, ok := f[k]; ok {
			t.Fatalf("%s should be absent", k)
		}
	}
}

func TestCreateIssueBodyWrapsDescriptionInADF(t *testing.T) {
	p, err := parseCreateIssueParams(map[string]any{
		"project_key": "PROJ", "summary": "hi", "description": "hello world",
		"priority": "High", "labels": []any{"a"}, "assignee_account_id": "acc", "issue_type": "Bug",
	})
	if err != nil {
		t.Fatal(err)
	}
	f := buildCreateIssueBody(p)["fields"].(map[string]any)
	desc := f["description"].(map[string]any)
	para := desc["content"].([]any)[0].(map[string]any)
	if desc["type"] != "doc" || para["type"] != "paragraph" || para["content"].([]any)[0].(map[string]any)["text"] != "hello world" {
		t.Fatalf("bad desc %v", desc)
	}
	if f["priority"].(map[string]any)["name"] != "High" || f["assignee"].(map[string]any)["accountId"] != "acc" ||
		f["issuetype"].(map[string]any)["name"] != "Bug" || len(f["labels"].([]any)) != 1 {
		t.Fatalf("bad fields %v", f)
	}
}

func TestParseCreateIssueParamsErrors(t *testing.T) {
	cases := map[string]any{
		"missing field `project_key`":                               map[string]any{"summary": "x"},
		"missing field `summary`":                                   map[string]any{"project_key": "x"},
		"invalid type: integer `5`, expected a string":              map[string]any{"project_key": json.Number("5"), "summary": "x"},
		"invalid type: null, expected a string":                     map[string]any{"project_key": "P", "summary": "x", "issue_type": nil},
		"invalid type: boolean `true`, expected a string":           map[string]any{"project_key": "P", "summary": "x", "priority": true},
		"invalid type: string \"a\", expected a sequence":           map[string]any{"project_key": "P", "summary": "x", "labels": "a"},
		"invalid type: map, expected a string":                      map[string]any{"project_key": "P", "summary": "x", "labels": []any{map[string]any{}}},
		"invalid type: null, expected struct CreateIssueParams":     nil,
		"invalid type: sequence, expected struct CreateIssueParams": []any{},
	}
	for want, in := range cases {
		_, err := parseCreateIssueParams(in)
		if err == nil || err.Error() != want {
			t.Errorf("want %q got %v", want, err)
		}
	}
}

func TestSimulateCreateIssueIsDeterministicByParams(t *testing.T) {
	c := newConn(models.Ptr("cloud"))
	ctx := context.Background()
	a := map[string]any{"project_key": "PROJ", "summary": "Fix bug"}
	b := map[string]any{"project_key": "PROJ", "summary": "Fix bug"}
	cc := map[string]any{"project_key": "PROJ", "summary": "Different summary"}
	ra, err := c.SimulateAction(ctx, "create_issue", a)
	if err != nil {
		t.Fatal(err)
	}
	rb, _ := c.SimulateAction(ctx, "create_issue", b)
	rc, _ := c.SimulateAction(ctx, "create_issue", cc)
	ma, mb, mc := ra.(map[string]any), rb.(map[string]any), rc.(map[string]any)
	if ma["issue_key"] != mb["issue_key"] || ma["issue_id"] != mb["issue_id"] {
		t.Fatal("same params → same fake IDs")
	}
	if ma["issue_key"] == mc["issue_key"] {
		t.Fatal("different params → different fake IDs")
	}
	key := ma["issue_key"].(string)
	if _, ok := ma["issue_id"].(string); !ok || !strings.HasPrefix(key, "PROJ-SIM-") || !strings.Contains(ma["url"].(string), key) {
		t.Fatalf("bad shape %v", ma)
	}
}

func TestSimulateRejectsMissingProjectKey(t *testing.T) {
	_, err := newConn(models.Ptr("cloud")).SimulateAction(context.Background(), "create_issue", map[string]any{"summary": "x"})
	if kindOf(t, err) != connector.ErrQuery {
		t.Fatal(err)
	}
	if !strings.Contains(err.Error(), "invalid create_issue params: missing field `project_key`") {
		t.Fatal(err)
	}
}

func TestSimulateRejectsUnknownActionType(t *testing.T) {
	_, err := newConn(models.Ptr("cloud")).SimulateAction(context.Background(), "add_comment", map[string]any{})
	if kindOf(t, err) != connector.ErrUnsupported {
		t.Fatal(err)
	}
}

// ── synthetic ──

func TestSynthHashMatchesRustDefaultHasher(t *testing.T) {
	// Reference values produced by rustc's std DefaultHasher.
	cases := []struct {
		row  int64
		salt uint64
		want uint64
	}{
		{0, 0, 8556445246977061536},
		{1, 1, 7912899488978770657},
		{5, 7, 9745303606092932007},
		{42, 8, 11414355875025544267},
	}
	for _, c := range cases {
		if got := synthHash(c.row, c.salt); got != c.want {
			t.Errorf("synthHash(%d,%d)=%d want %d", c.row, c.salt, got, c.want)
		}
	}
}

func TestSynthRFC3339(t *testing.T) {
	if got := synthRFC3339(1746100800); got != "2025-05-01T12:00:00.000+0000" {
		t.Fatal(got)
	}
}

func TestGenerateSyntheticRows(t *testing.T) {
	rows, ok := newConn(nil).GenerateSyntheticRows(nil, 25)
	if !ok || len(rows) != 25 {
		t.Fatalf("ok=%v len=%d", ok, len(rows))
	}
	r := rows[0]
	if r["id"] != "10001" || !strings.HasSuffix(r["key"].(string), "-1") {
		t.Fatalf("bad row %v", r)
	}
	for _, k := range []string{"summary", "status", "issue_type", "priority", "assignee", "reporter", "project", "created", "updated", "labels", "description"} {
		if _, ok := r[k]; !ok {
			t.Fatalf("missing %s", k)
		}
	}
	for _, row := range rows {
		if row["created"].(string) > row["updated"].(string) {
			t.Fatalf("created after updated: %v", row)
		}
		if _, ok := row["labels"].([]any); !ok {
			t.Fatalf("labels not array")
		}
	}
	again, _ := newConn(nil).GenerateSyntheticRows(nil, 25)
	if again[7]["key"] != rows[7]["key"] || again[7]["created"] != rows[7]["created"] {
		t.Fatal("not deterministic")
	}
	empty, ok := newConn(nil).GenerateSyntheticRows(nil, 0)
	if !ok || len(empty) != 0 {
		t.Fatal("expected empty rows")
	}
}

// ── ADF / mapping ──

func TestExtractPlainText(t *testing.T) {
	adf := map[string]any{"type": "doc", "content": []any{
		map[string]any{"type": "paragraph", "content": []any{
			map[string]any{"type": "text", "text": "Hello "},
			map[string]any{"type": "text", "text": "world"},
		}},
		map[string]any{"type": "paragraph", "content": []any{map[string]any{"type": "text", "text": "Two"}}},
	}}
	if got := extractPlainText(adf); got != "Hello world\nTwo\n" {
		t.Fatalf("%q", got)
	}
	if got := extractPlainText("plain"); got != "" {
		t.Fatalf("%q", got)
	}
}

func TestIssueToJSONDefaults(t *testing.T) {
	id := "77"
	j := issueToJSON(&jiraIssue{ID: &id})
	if j["key"] != "77" || j["summary"] != "" || len(j["labels"].([]any)) != 0 || j["description"] != "" {
		t.Fatalf("%v", j)
	}
}

// ── HTTP paths ──

const issueJSON = `{"id":"10","key":"P-1","fields":{"summary":"S","status":{"name":"Done"},"issuetype":{"name":"Bug"},
"priority":{"name":"High"},"assignee":{"displayName":"Ann"},"reporter":{"displayName":"Rob"},"project":{"key":"P"},
"created":"c","updated":"u","labels":["x"],"description":{"type":"doc","content":[{"type":"paragraph","content":[{"type":"text","text":"d"}]}]}}}`

func TestQueryListResolvesCloudIDAndSearches(t *testing.T) {
	var gotJQL, gotAuth string
	setup(t, func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		switch {
		case r.URL.Path == "/oauth/token/accessible-resources":
			io.WriteString(w, `[{"id":"cid"},{"id":"other"}]`)
		case r.URL.Path == "/ex/jira/cid/rest/api/3/search/jql":
			gotJQL = r.URL.Query().Get("jql")
			if r.URL.Query().Get("maxResults") != "50" || r.URL.Query().Get("fields") != issueFields {
				t.Errorf("bad query %s", r.URL.RawQuery)
			}
			io.WriteString(w, `{"issues":[`+issueJSON+`,{"id":"11","fields":{"status":{"name":"Open"}}}],"total":42}`)
		default:
			http.NotFound(w, r)
		}
	})
	p := connector.DefaultQueryParams()
	p.Search = models.Ptr(`say "hi"`)
	p.Sort = &models.SortSpec{Field: "key"}
	env, err := newConn(nil).Query(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	if gotAuth != "Bearer tok" || gotJQL != `text ~ "say \"hi\"" ORDER BY key ASC` {
		t.Fatalf("auth=%q jql=%q", gotAuth, gotJQL)
	}
	if env.Meta.Total != 42 || len(env.Items) != 2 {
		t.Fatalf("%+v", env.Meta)
	}
	it := env.Items[0]
	if it["key"] != "P-1" || it["status"] != "Done" || it["issue_type"] != "Bug" || it["assignee"] != "Ann" ||
		it["project"] != "P" || it["description"] != "d\n" {
		t.Fatalf("%v", it)
	}
	if env.Items[1]["key"] != "11" {
		t.Fatal(env.Items[1])
	}
}

func TestQueryTopLevelArrayAndFilters(t *testing.T) {
	var gotJQL string
	setup(t, func(w http.ResponseWriter, r *http.Request) {
		gotJQL = r.URL.Query().Get("jql")
		io.WriteString(w, `[`+issueJSON+`,{"id":"11","fields":{"status":{"name":"Open"}}}]`)
	})
	p := connector.DefaultQueryParams()
	p.Filters = []models.DatasetFilter{{FieldName: "status", Operator: models.OpEquals, Value: "Open", Logic: models.LogicAnd}}
	env, err := newConn(models.Ptr("cid")).Query(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	if gotJQL != "updated >= -30d ORDER BY updated DESC" {
		t.Fatal(gotJQL)
	}
	if len(env.Items) != 1 || env.Meta.Total != 1 || env.Items[0]["id"] != "11" {
		t.Fatalf("%+v", env)
	}
}

func TestQueryUnexpectedShapeAndBadIssue(t *testing.T) {
	body := `{"foo":1,"bar":2}`
	setup(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) })
	c := newConn(models.Ptr("cid"))
	p := connector.DefaultQueryParams()
	_, err := c.Query(context.Background(), &p)
	if err == nil || !strings.Contains(err.Error(), `top-level keys: Some(["bar", "foo"])`) {
		t.Fatal(err)
	}
	body = `"str"`
	_, err = c.Query(context.Background(), &p)
	if err == nil || !strings.Contains(err.Error(), "top-level keys: None") {
		t.Fatal(err)
	}
	body = `{"issues":[{"key":"x"}]}`
	_, err = c.Query(context.Background(), &p)
	if err == nil || !strings.Contains(err.Error(), `failed to parse Jira issue (keys: ["key"]): missing field `+"`id`") {
		t.Fatal(err)
	}
	body = `not json`
	_, err = c.Query(context.Background(), &p)
	if err == nil || !strings.Contains(err.Error(), "invalid JSON from Jira") {
		t.Fatal(err)
	}
}

func TestQueryUpstreamErrors(t *testing.T) {
	status := http.StatusUnauthorized
	setup(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status); io.WriteString(w, "nope") })
	c := newConn(models.Ptr("cid"))
	p := connector.DefaultQueryParams()
	_, err := c.Query(context.Background(), &p)
	if kindOf(t, err) != connector.ErrAuth {
		t.Fatal(err)
	}
	status = http.StatusTeapot
	_, err = c.Query(context.Background(), &p)
	var ce *connector.Error
	if !errors.As(err, &ce) || ce.Kind != connector.ErrUpstreamHTTP || ce.Status != 418 || !strings.Contains(ce.Message, "Jira API") {
		t.Fatal(err)
	}
	p.ItemID = models.Ptr("P-1")
	_, err = c.Query(context.Background(), &p)
	if kindOf(t, err) != connector.ErrUpstreamHTTP {
		t.Fatal(err)
	}
	// cloud-id resolution failure
	_, err = newConn(nil).Query(context.Background(), &p)
	if kindOf(t, err) != connector.ErrUpstreamHTTP {
		t.Fatal(err)
	}
}

func TestQuerySingleIssue(t *testing.T) {
	setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ex/jira/cid/rest/api/3/issue/P-1" {
			http.NotFound(w, r)
			return
		}
		io.WriteString(w, issueJSON)
	})
	p := connector.DefaultQueryParams()
	p.ItemID = models.Ptr("P-1")
	env, err := newConn(models.Ptr("cid")).Query(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	if env.Meta != (models.EnvelopeMeta{Total: 1, Limit: 1}) || env.Items[0]["summary"] != "S" {
		t.Fatalf("%+v", env)
	}
}

func TestNoAccessibleSites(t *testing.T) {
	setup(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, `[]`) })
	err := newConn(nil).TestConnection(context.Background())
	if kindOf(t, err) != connector.ErrConnection || !strings.Contains(err.Error(), "no accessible Jira sites") {
		t.Fatal(err)
	}
}

func TestTestConnection(t *testing.T) {
	ok := true
	setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/ex/jira/cid/rest/api/3/myself" || !ok {
			w.WriteHeader(http.StatusForbidden)
			return
		}
		io.WriteString(w, `{}`)
	})
	c := newConn(models.Ptr("cid"))
	if err := c.TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	ok = false
	if err := c.TestConnection(context.Background()); kindOf(t, err) != connector.ErrAuth {
		t.Fatal(err)
	}
}

func TestMissingTokenIsAuthError(t *testing.T) {
	secretstore.Init(secretstore.NewMemory())
	err := newConn(models.Ptr("cid")).TestConnection(context.Background())
	if kindOf(t, err) != connector.ErrAuth {
		t.Fatal(err)
	}
}

func TestInvokeCreateIssue(t *testing.T) {
	var got map[string]any
	fail := false
	setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost || r.URL.Path != "/ex/jira/cid/rest/api/3/issue" || r.Header.Get("Content-Type") != "application/json" {
			http.NotFound(w, r)
			return
		}
		if fail {
			w.WriteHeader(http.StatusBadRequest)
			io.WriteString(w, `{"errors":{}}`)
			return
		}
		json.NewDecoder(r.Body).Decode(&got)
		io.WriteString(w, `{"id":"10042","key":"PROJ-7","self":"x"}`)
	})
	c := newConn(models.Ptr("cid"))
	res, err := c.InvokeAction(context.Background(), "create_issue", map[string]any{"project_key": "PROJ", "summary": "hi"})
	if err != nil {
		t.Fatal(err)
	}
	m := res.(map[string]any)
	if m["issue_id"] != "10042" || m["issue_key"] != "PROJ-7" || m["url"] != atlassianBase+"/ex/jira/cid/browse/PROJ-7" {
		t.Fatalf("%v", m)
	}
	if got["fields"].(map[string]any)["summary"] != "hi" {
		t.Fatalf("%v", got)
	}
	fail = true
	if _, err := c.InvokeAction(context.Background(), "create_issue", map[string]any{"project_key": "PROJ", "summary": "hi"}); kindOf(t, err) != connector.ErrUpstreamHTTP {
		t.Fatal(err)
	}
	if _, err := c.InvokeAction(context.Background(), "create_issue", map[string]any{}); kindOf(t, err) != connector.ErrQuery {
		t.Fatal(err)
	}
	if _, err := c.InvokeAction(context.Background(), "nope", nil); kindOf(t, err) != connector.ErrUnsupported || !strings.Contains(err.Error(), "Jira does not support action 'nope'") {
		t.Fatal(err)
	}
}

func TestIntrospect(t *testing.T) {
	s, err := newConn(nil).Introspect(context.Background())
	if err != nil || len(s.Fields) != 13 || s.Fields[9].FieldType != connector.FieldDateTime || s.Fields[11].FieldType != connector.FieldJSON {
		t.Fatalf("%v %v", s, err)
	}
}
