package linear

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"sync"
	"testing"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/oauth"
	"github.com/ipedrazas/sbx/internal/secretstore"
)

// ── Ported unit tests ──

func paramsValue(teamID, title string) map[string]any {
	return map[string]any{"team_id": teamID, "title": title}
}

func TestCreateIssueInputMinimumShape(t *testing.T) {
	p, err := parseCreateIssue(paramsValue("team-uuid", "Fix bug"))
	if err != nil {
		t.Fatal(err)
	}
	in := buildCreateIssueInput(p)
	if in["teamId"] != "team-uuid" || in["title"] != "Fix bug" {
		t.Fatalf("got %v", in)
	}
	for _, k := range []string{"description", "priority", "assigneeId", "projectId", "labelIds"} {
		if _, ok := in[k]; ok {
			t.Errorf("%s should be absent", k)
		}
	}
}

func TestCreateIssueInputIncludesOptionalFields(t *testing.T) {
	v := paramsValue("team-uuid", "Fix bug")
	v["description"] = "details"
	v["priority"] = json.Number("2")
	v["assignee_id"] = "user-uuid"
	v["project_id"] = "proj-uuid"
	v["label_ids"] = []any{"lbl-1", "lbl-2"}
	p, err := parseCreateIssue(v)
	if err != nil {
		t.Fatal(err)
	}
	in := buildCreateIssueInput(p)
	if in["description"] != "details" || in["priority"] != int64(2) || in["assigneeId"] != "user-uuid" ||
		in["projectId"] != "proj-uuid" || in["labelIds"].([]string)[1] != "lbl-2" {
		t.Fatalf("got %v", in)
	}
}

func TestUpdateIssueInputOnlyIncludesSetFields(t *testing.T) {
	p, err := parseUpdateIssue(map[string]any{"issue_id": "ENG-7", "title": "New title"})
	if err != nil {
		t.Fatal(err)
	}
	in := buildUpdateIssueInput(p)
	if !reflect.DeepEqual(in, map[string]any{"title": "New title"}) {
		t.Fatalf("got %v", in)
	}
}

func TestUpdateIssueInputIncludesAllSetFields(t *testing.T) {
	p, err := parseUpdateIssue(map[string]any{
		"issue_id":    "uuid-1",
		"description": "details",
		"priority":    float64(1),
		"assignee_id": "user-uuid",
		"project_id":  "proj-uuid",
		"label_ids":   []any{"lbl-1", "lbl-2"},
	})
	if err != nil {
		t.Fatal(err)
	}
	in := buildUpdateIssueInput(p)
	if in["description"] != "details" || in["priority"] != int64(1) || in["assigneeId"] != "user-uuid" ||
		in["projectId"] != "proj-uuid" || in["labelIds"].([]string)[1] != "lbl-2" {
		t.Fatalf("got %v", in)
	}
	if _, ok := in["issue_id"]; ok {
		t.Fatal("issue_id must not leak into input")
	}
}

func TestUpdateIssueStatusResolvedSeparately(t *testing.T) {
	p, _ := parseUpdateIssue(map[string]any{"issue_id": "ENG-7", "status": "In Progress"})
	if p.Status == nil || *p.Status != "In Progress" {
		t.Fatal("status not parsed")
	}
	in := buildUpdateIssueInput(p)
	if _, ok := in["status"]; ok {
		t.Fatal("status emitted")
	}
	if _, ok := in["stateId"]; ok {
		t.Fatal("stateId emitted")
	}
}

func TestCreateProjectInputShapesTeamIntoArray(t *testing.T) {
	p, err := parseCreateProject(map[string]any{"name": "Reliability", "team_id": "team-uuid"})
	if err != nil {
		t.Fatal(err)
	}
	in := buildCreateProjectInput(p)
	if in["name"] != "Reliability" || in["teamIds"].([]string)[0] != "team-uuid" {
		t.Fatalf("got %v", in)
	}
	if _, ok := in["description"]; ok {
		t.Fatal("description should be absent")
	}
	if _, ok := in["leadId"]; ok {
		t.Fatal("leadId should be absent")
	}
	full, _ := parseCreateProject(map[string]any{"name": "R", "team_id": "t", "description": "d", "lead_id": "l", "state": "planned"})
	in = buildCreateProjectInput(full)
	if in["description"] != "d" || in["leadId"] != "l" || in["state"] != "planned" {
		t.Fatalf("got %v", in)
	}
}

func TestGraphQLErrorFormatting(t *testing.T) {
	got := formatGraphQLError(map[string]any{
		"message":    "Argument Validation Error",
		"extensions": map[string]any{"userPresentableMessage": "Description must be 255 characters or fewer."},
	})
	if got != "Argument Validation Error (Description must be 255 characters or fewer.)" {
		t.Fatalf("got %q", got)
	}
	got = formatGraphQLError(map[string]any{
		"message":    "Argument Validation Error",
		"extensions": map[string]any{"code": "INVALID_INPUT", "field": "description"},
	})
	if !strings.HasPrefix(got, "Argument Validation Error (") || !strings.Contains(got, "INVALID_INPUT") || !strings.Contains(got, "description") {
		t.Fatalf("got %q", got)
	}
	if got := formatGraphQLError(map[string]any{"message": "Entity not found"}); got != "Entity not found" {
		t.Fatalf("got %q", got)
	}
	if got := formatGraphQLError("weird"); got != "unknown error" {
		t.Fatalf("got %q", got)
	}
	// Detail identical to the message isn't repeated.
	if got := formatGraphQLError(map[string]any{"message": "x", "extensions": map[string]any{"userPresentableMessage": "x"}}); got != "x" {
		t.Fatalf("got %q", got)
	}
}

func TestUpdateProjectInputOnlyIncludesSetFields(t *testing.T) {
	p, _ := parseUpdateProject(map[string]any{"project_id": "proj-1", "state": "completed"})
	in := buildUpdateProjectInput(p)
	if !reflect.DeepEqual(in, map[string]any{"state": "completed"}) {
		t.Fatalf("got %v", in)
	}
	p, _ = parseUpdateProject(map[string]any{"project_id": "p", "name": "n", "description": "d", "lead_id": "l"})
	in = buildUpdateProjectInput(p)
	if len(in) != 3 || in["leadId"] != "l" {
		t.Fatalf("got %v", in)
	}
}

func sp(s string) *string { return &s }

func TestIssueToJSONMapsNestedFields(t *testing.T) {
	issue := &linearIssue{
		ID: "uuid-1", Identifier: sp("ENG-7"), Title: sp("Title"), Description: sp("Body"),
		PriorityLabel: sp("High"), CreatedAt: sp("2025-05-01T00:00:00.000Z"), UpdatedAt: sp("2025-05-02T00:00:00.000Z"),
		URL:      sp("https://linear.app/x/issue/ENG-7"),
		State:    &stateNode{Name: sp("In Progress"), Kind: sp("started")},
		Assignee: &namedNode{Name: sp("Alice")}, Creator: &namedNode{Name: sp("Bob")},
		Team:    &teamNode{ID: sp("team-uuid"), Key: sp("ENG")},
		Project: &idNameNode{ID: sp("proj-uuid"), Name: sp("Reliability")},
		Labels:  &labelConnection{Nodes: []namedNode{{Name: sp("Bug")}}},
	}
	j := issueToJSON(issue, descriptionExcerptChars)
	want := map[string]any{
		"identifier": "ENG-7", "status": "In Progress", "state_type": "started", "priority": "High",
		"assignee": "Alice", "creator": "Bob", "team": "ENG", "team_id": "team-uuid",
		"project": "Reliability", "project_id": "proj-uuid",
	}
	for k, v := range want {
		if j[k] != v {
			t.Errorf("%s: got %v want %v", k, j[k], v)
		}
	}
	if j["labels"].([]any)[0] != "Bug" {
		t.Fatal("labels")
	}
}

func TestIssueToJSONFallsBackToIDForIdentifier(t *testing.T) {
	j := issueToJSON(&linearIssue{ID: "uuid-only"}, descriptionExcerptChars)
	if j["identifier"] != "uuid-only" || j["state_type"] != "" || j["title"] != "" || j["team_id"] != "" || j["project_id"] != "" {
		t.Fatalf("got %v", j)
	}
	if len(j["labels"].([]any)) != 0 {
		t.Fatal("labels should be empty")
	}
	b, _ := json.Marshal(j["labels"])
	if string(b) != "[]" {
		t.Fatalf("labels must serialize as [], got %s", b)
	}
}

func TestProjectToJSONMapsNestedFields(t *testing.T) {
	prog := 0.42
	p := &linearProject{
		ID: "proj-1", Name: sp("Reliability"), Description: sp("Harden things"), State: sp("started"),
		Progress: &prog, URL: sp("u"), Lead: &namedNode{Name: sp("Carol")},
		Teams: &teamConnection{Nodes: []teamNode{{ID: sp("team-uuid"), Key: sp("ENG")}}},
	}
	j := projectToJSON(p, descriptionExcerptChars)
	if j["id"] != "proj-1" || j["name"] != "Reliability" || j["status"] != "started" || j["progress"] != 0.42 ||
		j["lead"] != "Carol" || j["teams"].([]any)[0] != "ENG" {
		t.Fatalf("got %v", j)
	}
	empty := projectToJSON(&linearProject{ID: "x"}, descriptionExcerptChars)
	if empty["progress"] != 0.0 || empty["lead"] != "" || len(empty["teams"].([]any)) != 0 {
		t.Fatalf("got %v", empty)
	}
}

func TestTeamToJSONMapsFields(t *testing.T) {
	j := teamToJSON(&linearTeam{ID: "team-uuid", Key: sp("ENG"), Name: sp("Engineering"), Description: sp("Builds things")})
	want := map[string]any{"id": "team-uuid", "key": "ENG", "name": "Engineering", "description": "Builds things"}
	if !reflect.DeepEqual(j, want) {
		t.Fatalf("got %v", j)
	}
}

func TestSyntheticIssueRowsDeterministicAndWellFormed(t *testing.T) {
	a, b := generateIssueRows(5), generateIssueRows(5)
	if !reflect.DeepEqual(a, b) || len(a) != 5 {
		t.Fatal("must be deterministic")
	}
	for _, row := range a {
		if !strings.Contains(row["identifier"].(string), "-") ||
			!strings.HasSuffix(row["created"].(string), "Z") ||
			!strings.HasPrefix(row["team_id"].(string), "team_") ||
			row["state_type"].(string) == "" {
			t.Fatalf("bad row %v", row)
		}
	}
}

func TestSipHashMatchesRustDefaultHasher(t *testing.T) {
	// Reference values computed with Rust's std DefaultHasher.
	cases := []struct {
		row  int64
		salt uint64
		want uint64
	}{
		{0, 0, 8556445246977061536},
		{3, 9, 9386140961290875301},
		{2, 0xC0FFEE, 14113449336249574248},
	}
	for _, c := range cases {
		if got := synthHash(c.row, c.salt); got != c.want {
			t.Errorf("synthHash(%d,%d)=%d want %d", c.row, c.salt, got, c.want)
		}
	}
	if got := sipHash13(append([]byte("ENG"), 0xff)); got != 3844737609311991900 {
		t.Errorf("str hash got %d", got)
	}
	if synthTeamID("ENG") != fmt.Sprintf("team_%012x", uint64(3844737609311991900)&0xffffffffffff) {
		t.Error("team id")
	}
}

func TestSynthRFC3339(t *testing.T) {
	if got := synthRFC3339(1746100800); got != "2025-05-01T12:00:00.000Z" {
		t.Fatalf("got %s", got)
	}
}

func TestExcerptCutsOnCharBoundary(t *testing.T) {
	issue := &linearIssue{ID: "uuid-1", Description: sp(strings.Repeat("x", 2000))}
	if l := len(issueToJSON(issue, descriptionExcerptChars)["description"].(string)); l != 500 {
		t.Fatalf("listed len %d", l)
	}
	if l := len(issueToJSON(issue, fullText)["description"].(string)); l != 2000 {
		t.Fatalf("full len %d", l)
	}
}

func TestFinalizeListTotalCountsAllMatches(t *testing.T) {
	var items []map[string]any
	for i := range 30 {
		items = append(items, map[string]any{"id": fmt.Sprint(i)})
	}
	env := finalizeList(items, &connector.QueryParams{Limit: 10, Offset: 10})
	if len(env.Items) != 10 || env.Items[0]["id"] != "10" || env.Meta.Total != 30 {
		t.Fatalf("got %+v", env.Meta)
	}
	env = finalizeList(items, &connector.QueryParams{Limit: 10, Offset: 100})
	if len(env.Items) != 0 || env.Meta.Total != 30 {
		t.Fatal("offset past end")
	}
	env = finalizeList(items, &connector.QueryParams{Limit: -1, Offset: -5})
	if len(env.Items) != 0 || env.Meta.Total != 30 {
		t.Fatal("negative limit")
	}
}

func TestSyntheticProjectAndTeamRows(t *testing.T) {
	projects := generateProjectRows(8)
	if len(projects) != 8 {
		t.Fatal("len")
	}
	for _, row := range projects {
		if row["name"].(string) == "" {
			t.Fatal("empty name")
		}
		if _, ok := row["progress"].(float64); !ok {
			t.Fatal("progress not a number")
		}
	}
	if projects[7]["name"] != "Mobile App Revamp (8)" || projects[0]["name"] != "Q3 Platform Reliability" {
		t.Fatalf("names %v %v", projects[0]["name"], projects[7]["name"])
	}
	teams := generateTeamRows(5)
	for _, row := range teams {
		if !strings.HasPrefix(row["id"].(string), "team_") || row["key"].(string) == "" {
			t.Fatal("bad team")
		}
	}
	issue := generateIssueRows(1)[0]
	if issue["team_id"] != synthTeamID(issue["team"].(string)) {
		t.Fatal("team ids must agree")
	}
	if !reflect.DeepEqual(issue["team_id"], teamRowFor(teams, issue["team"].(string))) {
		t.Fatal("team entity id mismatch")
	}
}

func teamRowFor(teams []map[string]any, key string) any {
	for _, t := range teams {
		if t["key"] == key {
			return t["id"]
		}
	}
	return nil
}

func TestParseEntityDefaultsToIssues(t *testing.T) {
	if ParseEntity("projects") != EntityProjects || ParseEntity("teams") != EntityTeams ||
		ParseEntity("issues") != EntityIssues || ParseEntity("nonsense") != EntityIssues {
		t.Fatal("parse")
	}
}

func oauthConn() *Connector {
	return New(0, OAuthAuth(oauth.Credentials{ClientID: "x"}))
}

func TestSimulateCreateIssueDeterministic(t *testing.T) {
	c := oauthConn()
	ctx := context.Background()
	ra, _ := c.SimulateAction(ctx, "create_issue", paramsValue("team-1", "Fix bug"))
	rb, _ := c.SimulateAction(ctx, "create_issue", paramsValue("team-1", "Fix bug"))
	rc, _ := c.SimulateAction(ctx, "create_issue", paramsValue("team-1", "Other"))
	if !reflect.DeepEqual(ra, rb) || reflect.DeepEqual(ra, rc) {
		t.Fatal("determinism")
	}
	m := ra.(map[string]any)
	ident := m["identifier"].(string)
	if !strings.HasPrefix(ident, "SIM-") || !strings.Contains(m["url"].(string), ident) ||
		!strings.HasPrefix(m["issue_id"].(string), "sim-") {
		t.Fatalf("got %v", m)
	}
}

func TestSimulateCreateProjectValidatesAndFakes(t *testing.T) {
	c := oauthConn()
	ok, err := c.SimulateAction(context.Background(), "create_project", map[string]any{"name": "Reliability", "team_id": "team-1"})
	if err != nil {
		t.Fatal(err)
	}
	m := ok.(map[string]any)
	if !strings.HasPrefix(m["project_id"].(string), "sim-proj-") || m["name"] != "Reliability" {
		t.Fatalf("got %v", m)
	}
	_, err = c.SimulateAction(context.Background(), "create_project", map[string]any{"team_id": "team-1"})
	assertKind(t, err, connector.ErrQuery)
}

func TestSimulateUpdates(t *testing.T) {
	c := oauthConn()
	r, err := c.SimulateAction(context.Background(), "update_issue", map[string]any{"issue_id": "ENG-1"})
	if err != nil || r.(map[string]any)["issue_id"] != "ENG-1" || r.(map[string]any)["identifier"] != "SIM-UPDATED" {
		t.Fatalf("got %v %v", r, err)
	}
	r, err = c.SimulateAction(context.Background(), "update_project", map[string]any{"project_id": "p", "state": "paused"})
	m := r.(map[string]any)
	if err != nil || m["project_id"] != "p" || m["state"] != "paused" || m["name"] != "" {
		t.Fatalf("got %v %v", r, err)
	}
	_, err = c.SimulateAction(context.Background(), "update_issue", map[string]any{})
	assertKind(t, err, connector.ErrQuery)
	_, err = c.SimulateAction(context.Background(), "update_project", nil)
	assertKind(t, err, connector.ErrQuery)
}

func TestSimulateRejectsMissingTeamID(t *testing.T) {
	_, err := oauthConn().SimulateAction(context.Background(), "create_issue", map[string]any{"title": "x"})
	assertKind(t, err, connector.ErrQuery)
	if !strings.Contains(err.Error(), "invalid create_issue params: missing field `team_id`") {
		t.Fatalf("got %v", err)
	}
}

func TestSimulateRejectsUnknownAction(t *testing.T) {
	_, err := oauthConn().SimulateAction(context.Background(), "add_comment", map[string]any{})
	assertKind(t, err, connector.ErrUnsupported)
}

func TestParamTypeErrors(t *testing.T) {
	cases := []map[string]any{
		{"team_id": 5.0, "title": "x"},
		{"team_id": "t", "title": "x", "priority": 1.5},
		{"team_id": "t", "title": "x", "priority": "high"},
		{"team_id": "t", "title": "x", "label_ids": "a"},
		{"team_id": "t", "title": "x", "label_ids": []any{1.0}},
		{"team_id": "t", "title": "x", "description": true},
		{"team_id": "t", "title": "x", "priority": json.Number("1.5")},
	}
	for i, c := range cases {
		if _, err := parseCreateIssue(c); err == nil {
			t.Errorf("case %d should fail", i)
		}
	}
	// Nulls are treated as absent.
	p, err := parseCreateIssue(map[string]any{"team_id": "t", "title": "x", "priority": nil, "label_ids": nil})
	if err != nil || p.Priority != nil || p.LabelIDs != nil {
		t.Fatalf("got %+v %v", p, err)
	}
	p, err = parseCreateIssue(map[string]any{"team_id": "t", "title": "x", "priority": 3, "label_ids": []string{"a"}})
	if err != nil || *p.Priority != 3 || p.LabelIDs[0] != "a" {
		t.Fatalf("got %+v %v", p, err)
	}
}

func TestIntrospectSchemaDiffersPerEntity(t *testing.T) {
	ctx := context.Background()
	has := func(s *connector.Schema, name string) bool {
		for _, f := range s.Fields {
			if f.Name == name {
				return true
			}
		}
		return false
	}
	auth := APIKeyAuth("k")
	issues, _ := New(1, auth).Introspect(ctx)
	projects, _ := WithEntity(1, auth, EntityProjects).Introspect(ctx)
	teams, _ := WithEntity(1, auth, EntityTeams).Introspect(ctx)
	if !has(issues, "team_id") || !has(issues, "project_id") || len(issues.Fields) != 17 {
		t.Fatal("issues schema")
	}
	if !has(projects, "progress") || len(projects.Fields) != 10 {
		t.Fatal("projects schema")
	}
	if !has(teams, "key") || len(teams.Fields) != 4 {
		t.Fatal("teams schema")
	}
}

func TestGenerateSyntheticRowsPerEntity(t *testing.T) {
	auth := APIKeyAuth("k")
	for e, key := range map[Entity]string{EntityIssues: "identifier", EntityProjects: "progress", EntityTeams: "key"} {
		rows, ok := WithEntity(1, auth, e).GenerateSyntheticRows(nil, 3)
		if !ok || len(rows) != 3 {
			t.Fatal("rows")
		}
		if _, has := rows[0][key]; !has {
			t.Errorf("entity %d missing %s", e, key)
		}
	}
}

func assertKind(t *testing.T, err error, kind connector.ErrorKind) {
	t.Helper()
	var ce *connector.Error
	if !errors.As(err, &ce) || ce.Kind != kind {
		t.Fatalf("expected kind %d, got %v", kind, err)
	}
}

// ── HTTP tests ──

type gqlReq struct {
	Query     string         `json:"query"`
	Variables map[string]any `json:"variables"`
	Auth      string
}

// fakeLinear serves responses picked by handler; records every request.
type fakeLinear struct {
	mu   sync.Mutex
	reqs []gqlReq
}

func startFake(t *testing.T, handler func(r gqlReq) (int, any)) *fakeLinear {
	t.Helper()
	f := &fakeLinear{}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var g gqlReq
		_ = json.Unmarshal(b, &g)
		g.Auth = r.Header.Get("Authorization")
		f.mu.Lock()
		f.reqs = append(f.reqs, g)
		f.mu.Unlock()
		status, body := handler(g)
		w.WriteHeader(status)
		if s, ok := body.(string); ok {
			_, _ = io.WriteString(w, s)
			return
		}
		_ = json.NewEncoder(w).Encode(body)
	}))
	old := apiURL
	apiURL = srv.URL
	t.Cleanup(func() { srv.Close(); apiURL = old })
	return f
}

func data(d any) map[string]any { return map[string]any{"data": d} }

func TestAPIKeyAuthSetsRawHeader(t *testing.T) {
	f := startFake(t, func(gqlReq) (int, any) {
		return 200, data(map[string]any{"viewer": map[string]any{"id": "u1"}})
	})
	if err := New(0, APIKeyAuth("lin_api_secret")).TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.reqs[0].Auth != "lin_api_secret" {
		t.Fatalf("auth header %q", f.reqs[0].Auth)
	}
	if f.reqs[0].Query != "query { viewer { id } }" {
		t.Fatal("query")
	}
}

func TestOAuthAuthSendsBearer(t *testing.T) {
	secretstore.Init(secretstore.NewMemory())
	if err := oauth.StoreTokens(42, &oauth.Tokens{AccessToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	f := startFake(t, func(gqlReq) (int, any) {
		return 200, data(map[string]any{"viewer": map[string]any{"id": "u1"}})
	})
	if err := New(42, OAuthAuth(oauth.Credentials{ClientID: "c"})).TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.reqs[0].Auth != "Bearer tok" {
		t.Fatalf("auth %q", f.reqs[0].Auth)
	}
	// Missing tokens → Auth error, no request sent.
	err := New(43, OAuthAuth(oauth.Credentials{ClientID: "c"})).TestConnection(context.Background())
	assertKind(t, err, connector.ErrAuth)
	if len(f.reqs) != 1 {
		t.Fatal("no request expected")
	}
}

func TestTestConnectionFailures(t *testing.T) {
	startFake(t, func(gqlReq) (int, any) { return 200, data(map[string]any{"viewer": nil}) })
	assertKind(t, New(0, APIKeyAuth("k")).TestConnection(context.Background()), connector.ErrConnection)
}

func TestHTTPErrorsAndGraphQLErrors(t *testing.T) {
	ctx := context.Background()
	c := New(0, APIKeyAuth("k"))

	startFake(t, func(gqlReq) (int, any) { return 401, "nope" })
	assertKind(t, c.TestConnection(ctx), connector.ErrAuth)

	startFake(t, func(gqlReq) (int, any) { return 500, "boom" })
	err := c.TestConnection(ctx)
	assertKind(t, err, connector.ErrUpstreamHTTP)
	if err.(*connector.Error).Status != 500 {
		t.Fatal("status")
	}

	startFake(t, func(gqlReq) (int, any) { return 200, "not json" })
	assertKind(t, c.TestConnection(ctx), connector.ErrQuery)

	startFake(t, func(gqlReq) (int, any) {
		return 200, map[string]any{"errors": []any{
			map[string]any{"message": "Argument Validation Error", "extensions": map[string]any{"userPresentableMessage": "too long"}},
			map[string]any{"message": "second"},
		}}
	})
	err = c.TestConnection(ctx)
	assertKind(t, err, connector.ErrBadInput)
	if err.Error() != "invalid request: Linear GraphQL error: Argument Validation Error (too long); second" {
		t.Fatalf("got %q", err)
	}

	// HTTP 400 with an `errors` array is a user error, not an upstream failure.
	startFake(t, func(gqlReq) (int, any) {
		return 400, map[string]any{"errors": []any{
			map[string]any{"message": "Query too complex", "extensions": map[string]any{
				"userPresentableMessage": "The query is too complex. Complexity: 16025. Maximum allowed complexity: 10000.",
			}},
		}}
	})
	err = c.TestConnection(ctx)
	assertKind(t, err, connector.ErrBadInput)
	if err.Error() != "invalid request: Linear GraphQL error: Query too complex (The query is too complex. Complexity: 16025. Maximum allowed complexity: 10000.)" {
		t.Fatalf("got %q", err)
	}

	// HTTP 400 without a GraphQL body stays an upstream error.
	startFake(t, func(gqlReq) (int, any) { return 400, "bad" })
	err = c.TestConnection(ctx)
	assertKind(t, err, connector.ErrUpstreamHTTP)
	if err.(*connector.Error).Status != 400 {
		t.Fatal("status")
	}

	old := apiURL
	apiURL = "http://127.0.0.1:1"
	defer func() { apiURL = old }()
	assertKind(t, c.TestConnection(ctx), connector.ErrQuery)
}

func issueNode(id, ident, title string) map[string]any {
	return map[string]any{
		"id": id, "identifier": ident, "title": title, "description": strings.Repeat("d", 600),
		"priorityLabel": "High", "state": map[string]any{"name": "Todo", "type": "unstarted"},
		"team": map[string]any{"id": "t1", "key": "ENG"}, "labels": map[string]any{"nodes": []any{map[string]any{"name": "Bug"}}},
	}
}

func TestQueryIssuesPaginatesFiltersAndSlices(t *testing.T) {
	f := startFake(t, func(r gqlReq) (int, any) {
		if r.Variables["after"] == nil {
			return 200, data(map[string]any{"issues": map[string]any{
				"nodes":    []any{issueNode("1", "ENG-1", "a"), issueNode("2", "ENG-2", "b")},
				"pageInfo": map[string]any{"hasNextPage": true, "endCursor": "c1"},
			}})
		}
		n3 := issueNode("3", "ENG-3", "c")
		n3["state"] = map[string]any{"name": "Done", "type": "completed"}
		return 200, data(map[string]any{"issues": map[string]any{
			"nodes":    []any{n3},
			"pageInfo": map[string]any{"hasNextPage": false, "endCursor": "c2"},
		}})
	})
	search := "bug"
	filt, _ := connector.ParseAdhocFilter("status:eq:Todo", 1000)
	env, err := New(0, APIKeyAuth("k")).Query(context.Background(), &connector.QueryParams{
		Limit: 1, Offset: 1, Search: &search, Filters: []models.DatasetFilter{filt},
	})
	if err != nil {
		t.Fatal(err)
	}
	if env.Meta.Total != 2 || len(env.Items) != 1 || env.Items[0]["identifier"] != "ENG-2" || env.Meta.Limit != 1 || env.Meta.Offset != 1 {
		t.Fatalf("got %+v %v", env.Meta, env.Items)
	}
	if len(env.Items[0]["description"].(string)) != 500 {
		t.Fatal("list description should be excerpted")
	}
	if len(f.reqs) != 2 {
		t.Fatalf("requests %d", len(f.reqs))
	}
	v := f.reqs[0].Variables
	if v["first"] != float64(250) || v["orderBy"] != "updatedAt" ||
		!reflect.DeepEqual(v["filter"], map[string]any{"title": map[string]any{"containsIgnoreCase": "bug"}}) {
		t.Fatalf("vars %v", v)
	}
	if f.reqs[1].Variables["after"] != "c1" {
		t.Fatal("cursor")
	}
}

func TestQueryIssuesStopsAtMaxScanRows(t *testing.T) {
	calls := 0
	startFake(t, func(r gqlReq) (int, any) {
		calls++
		nodes := make([]any, 250)
		for i := range nodes {
			nodes[i] = map[string]any{"id": fmt.Sprintf("%d-%d", calls, i)}
		}
		return 200, data(map[string]any{"issues": map[string]any{
			"nodes": nodes, "pageInfo": map[string]any{"hasNextPage": true, "endCursor": fmt.Sprint(calls)},
		}})
	})
	env, err := New(0, APIKeyAuth("k")).Query(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if calls != 4 || env.Meta.Total != 1000 || len(env.Items) != 50 {
		t.Fatalf("calls=%d total=%d", calls, env.Meta.Total)
	}
}

func TestQueryMalformedResponses(t *testing.T) {
	ctx := context.Background()
	c := New(0, APIKeyAuth("k"))
	startFake(t, func(gqlReq) (int, any) { return 200, data(map[string]any{}) })
	_, err := c.Query(ctx, &connector.QueryParams{Limit: 5})
	if err == nil || !strings.Contains(err.Error(), "missing issues") {
		t.Fatalf("got %v", err)
	}
	startFake(t, func(gqlReq) (int, any) { return 200, data(map[string]any{"issues": nil}) })
	_, err = c.Query(ctx, &connector.QueryParams{Limit: 5})
	if err == nil || !strings.Contains(err.Error(), "missing issues.nodes") {
		t.Fatalf("got %v", err)
	}
	startFake(t, func(gqlReq) (int, any) {
		return 200, data(map[string]any{"issues": map[string]any{"nodes": []any{map[string]any{"title": "no id"}}}})
	})
	_, err = c.Query(ctx, &connector.QueryParams{Limit: 5})
	if err == nil || !strings.Contains(err.Error(), "failed to parse Linear issue") {
		t.Fatalf("got %v", err)
	}
	startFake(t, func(gqlReq) (int, any) {
		return 200, data(map[string]any{"issues": map[string]any{"nodes": []any{map[string]any{"id": "1", "title": 5}}}})
	})
	_, err = c.Query(ctx, &connector.QueryParams{Limit: 5})
	assertKind(t, err, connector.ErrQuery)
}

func TestQuerySingleItems(t *testing.T) {
	ctx := context.Background()
	f := startFake(t, func(r gqlReq) (int, any) {
		switch {
		case strings.Contains(r.Query, "issue(id"):
			if r.Variables["id"] == "missing" {
				return 200, data(map[string]any{"issue": nil})
			}
			return 200, data(map[string]any{"issue": issueNode("u1", "ENG-1", "t")})
		case strings.Contains(r.Query, "project(id"):
			return 200, data(map[string]any{"project": map[string]any{"id": "p1", "name": "P", "progress": 0.5, "description": strings.Repeat("x", 700)}})
		case strings.Contains(r.Query, "team(id"):
			return 200, data(map[string]any{"team": map[string]any{"id": "t1", "key": "ENG"}})
		}
		return 500, "unexpected"
	})
	auth := APIKeyAuth("k")
	id := "ENG-1"
	env, err := New(0, auth).Query(ctx, &connector.QueryParams{ItemID: &id})
	if err != nil || len(env.Items) != 1 || env.Meta.Limit != 1 || len(env.Items[0]["description"].(string)) != 600 {
		t.Fatalf("got %v %v", env, err)
	}
	if f.reqs[0].Variables["id"] != "ENG-1" {
		t.Fatal("id var")
	}
	missing := "missing"
	env, _ = New(0, auth).Query(ctx, &connector.QueryParams{ItemID: &missing})
	if len(env.Items) != 0 || env.Meta.Total != 0 {
		t.Fatal("missing should be empty")
	}
	env, err = WithEntity(0, auth, EntityProjects).Query(ctx, &connector.QueryParams{ItemID: &id})
	if err != nil || env.Items[0]["progress"] != 0.5 || len(env.Items[0]["description"].(string)) != 700 {
		t.Fatalf("project %v %v", env, err)
	}
	env, err = WithEntity(0, auth, EntityTeams).Query(ctx, &connector.QueryParams{ItemID: &id})
	if err != nil || env.Items[0]["key"] != "ENG" {
		t.Fatalf("team %v %v", env, err)
	}
}

func TestQueryProjectsAndTeamsLists(t *testing.T) {
	ctx := context.Background()
	f := startFake(t, func(r gqlReq) (int, any) {
		if strings.Contains(r.Query, "projects(") {
			return 200, data(map[string]any{"projects": map[string]any{"nodes": []any{
				map[string]any{"id": "p1", "name": "Rel", "teams": map[string]any{"nodes": []any{map[string]any{"key": "ENG"}}}},
			}}})
		}
		return 200, data(map[string]any{"teams": map[string]any{"nodes": []any{
			map[string]any{"id": "t1", "key": "ENG", "name": "Engineering"},
			map[string]any{"id": "t2", "key": "OPS", "name": "Ops"},
		}}})
	})
	search := "e"
	env, err := WithEntity(0, APIKeyAuth("k"), EntityProjects).Query(ctx, &connector.QueryParams{Limit: 10, Search: &search})
	if err != nil || env.Meta.Total != 1 || env.Items[0]["teams"].([]any)[0] != "ENG" {
		t.Fatalf("got %v %v", env, err)
	}
	if !reflect.DeepEqual(f.reqs[0].Variables["filter"], map[string]any{"name": map[string]any{"containsIgnoreCase": "e"}}) {
		t.Fatal("project filter")
	}
	env, err = WithEntity(0, APIKeyAuth("k"), EntityTeams).Query(ctx, &connector.QueryParams{Limit: 10, Search: &search})
	if err != nil || env.Meta.Total != 2 {
		t.Fatalf("got %v %v", env, err)
	}
	if _, ok := f.reqs[1].Variables["orderBy"]; ok {
		t.Fatal("teams must not send orderBy")
	}
}

func TestInvokeCreateIssue(t *testing.T) {
	f := startFake(t, func(r gqlReq) (int, any) {
		return 200, data(map[string]any{"issueCreate": map[string]any{
			"success": true, "issue": map[string]any{"id": "u1", "identifier": "ENG-9", "url": "https://l/ENG-9"},
		}})
	})
	params := paramsValue("team-1", "Fix")
	params["priority"] = float64(2)
	res, err := New(0, APIKeyAuth("k")).InvokeAction(context.Background(), "create_issue", params)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]any{"issue_id": "u1", "identifier": "ENG-9", "url": "https://l/ENG-9"}
	if !reflect.DeepEqual(res, want) {
		t.Fatalf("got %v", res)
	}
	in := f.reqs[0].Variables["input"].(map[string]any)
	if in["teamId"] != "team-1" || in["priority"] != float64(2) {
		t.Fatalf("input %v", in)
	}
}

func TestInvokeFailuresPerAction(t *testing.T) {
	ctx := context.Background()
	c := New(0, APIKeyAuth("k"))
	startFake(t, func(gqlReq) (int, any) {
		return 200, data(map[string]any{
			"issueCreate":   map[string]any{"success": false},
			"projectCreate": map[string]any{"success": false},
			"projectUpdate": map[string]any{"success": false},
		})
	})
	_, err := c.InvokeAction(ctx, "create_issue", paramsValue("t", "x"))
	if err == nil || !strings.Contains(err.Error(), "Linear reported the issue was not created") {
		t.Fatalf("got %v", err)
	}
	_, err = c.InvokeAction(ctx, "create_project", map[string]any{"name": "n", "team_id": "t"})
	if err == nil || !strings.Contains(err.Error(), "project was not created") {
		t.Fatalf("got %v", err)
	}
	_, err = c.InvokeAction(ctx, "update_project", map[string]any{"project_id": "p"})
	if err == nil || !strings.Contains(err.Error(), "project was not updated") {
		t.Fatalf("got %v", err)
	}
	_, err = c.InvokeAction(ctx, "update_issue", map[string]any{"issue_id": "i"})
	if err == nil || !strings.Contains(err.Error(), "Linear issueUpdate returned no payload") {
		t.Fatalf("got %v", err)
	}
	_, err = c.InvokeAction(ctx, "delete_issue", nil)
	assertKind(t, err, connector.ErrUnsupported)
	for _, a := range []string{"create_issue", "update_issue", "create_project", "update_project"} {
		_, err = c.InvokeAction(ctx, a, map[string]any{})
		assertKind(t, err, connector.ErrQuery)
	}
}

func TestInvokeUpdateIssueResolvesStatus(t *testing.T) {
	f := startFake(t, func(r gqlReq) (int, any) {
		if strings.Contains(r.Query, "states") {
			return 200, data(map[string]any{"issue": map[string]any{"team": map[string]any{"states": map[string]any{"nodes": []any{
				map[string]any{"id": "s-todo", "name": "Todo"},
				map[string]any{"id": "s-prog", "name": "In Progress"},
			}}}}})
		}
		return 200, data(map[string]any{"issueUpdate": map[string]any{
			"success": true, "issue": map[string]any{"id": "u1", "identifier": "ENG-7", "url": "x"},
		}})
	})
	ctx := context.Background()
	c := New(0, APIKeyAuth("k"))
	res, err := c.InvokeAction(ctx, "update_issue", map[string]any{"issue_id": "ENG-7", "status": " in progress ", "title": "T"})
	if err != nil {
		t.Fatal(err)
	}
	if res.(map[string]any)["identifier"] != "ENG-7" {
		t.Fatalf("got %v", res)
	}
	mut := f.reqs[1]
	if mut.Variables["id"] != "ENG-7" || !reflect.DeepEqual(mut.Variables["input"], map[string]any{"title": "T", "stateId": "s-prog"}) {
		t.Fatalf("vars %v", mut.Variables)
	}

	_, err = c.InvokeAction(ctx, "update_issue", map[string]any{"issue_id": "ENG-7", "status": "Shipped"})
	assertKind(t, err, connector.ErrBadInput)
	if !strings.Contains(err.Error(), "unknown workflow state 'Shipped' for this issue's team; valid states: Todo, In Progress") {
		t.Fatalf("got %v", err)
	}
}

func TestResolveStateIssueNotFound(t *testing.T) {
	startFake(t, func(gqlReq) (int, any) { return 200, data(map[string]any{"issue": nil}) })
	_, err := New(0, APIKeyAuth("k")).InvokeAction(context.Background(), "update_issue", map[string]any{"issue_id": "X-1", "status": "Done"})
	assertKind(t, err, connector.ErrBadInput)
	if !strings.Contains(err.Error(), "Linear issue 'X-1' not found or has no team workflow states") {
		t.Fatalf("got %v", err)
	}
}

func TestInvokeProjects(t *testing.T) {
	f := startFake(t, func(r gqlReq) (int, any) {
		if strings.Contains(r.Query, "projectCreate") {
			return 200, data(map[string]any{"projectCreate": map[string]any{"success": true, "project": map[string]any{"id": "p1", "name": "Rel", "url": "u"}}})
		}
		return 200, data(map[string]any{"projectUpdate": map[string]any{"success": true, "project": map[string]any{"id": "p1", "name": "Rel", "url": "u", "state": "paused"}}})
	})
	ctx := context.Background()
	c := New(0, APIKeyAuth("k"))
	res, err := c.InvokeAction(ctx, "create_project", map[string]any{"name": "Rel", "team_id": "t1"})
	if err != nil || !reflect.DeepEqual(res, map[string]any{"project_id": "p1", "name": "Rel", "url": "u"}) {
		t.Fatalf("got %v %v", res, err)
	}
	if !reflect.DeepEqual(f.reqs[0].Variables["input"], map[string]any{"name": "Rel", "teamIds": []any{"t1"}}) {
		t.Fatalf("input %v", f.reqs[0].Variables)
	}
	res, err = c.InvokeAction(ctx, "update_project", map[string]any{"project_id": "p1", "state": "paused"})
	if err != nil || res.(map[string]any)["state"] != "paused" {
		t.Fatalf("got %v %v", res, err)
	}
	if f.reqs[1].Variables["id"] != "p1" {
		t.Fatal("id")
	}
}
