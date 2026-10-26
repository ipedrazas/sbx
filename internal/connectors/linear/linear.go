// Package linear implements the Linear (GraphQL) connector: issues, projects
// and teams, plus create/update actions, simulators and synthetic rows.
package linear

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"net/http"
	"strings"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/oauth"
	"github.com/ipedrazas/sbx/internal/util"
)

// apiURL is the Linear GraphQL endpoint; tests point it at httptest.
var apiURL = "https://api.linear.app/graphql"

// tokenURL is the OAuth refresh endpoint; overridable in tests.
var tokenURL = oauth.LinearTokenURL

// The issue fields requested in every GraphQL selection. `team` and `project`
// carry their UUIDs so agents can feed them straight into create actions.
//
// Nested connections must set `first`: Linear multiplies their cost by the
// page size, and the default of 50 pushes a 250-row page past the 10,000
// complexity limit (~16,000). 20 keeps a full page around 8,000.
const issueFields = `
    id
    identifier
    title
    description
    priorityLabel
    createdAt
    updatedAt
    url
    state { name type }
    assignee { name }
    creator { name }
    team { id key }
    project { id name }
    labels(first: 20) { nodes { name } }
`

const projectFields = `
    id
    name
    description
    state
    progress
    url
    createdAt
    updatedAt
    lead { name }
    teams(first: 20) { nodes { id key } }
`

const teamFields = `
    id
    key
    name
    description
`

const (
	// Linear's maximum page size for a connection.
	linearPageSize = 250
	// The most rows a list/search pulls before filtering and paging locally.
	maxScanRows = 1000
	// How much of a description list and search results carry.
	descriptionExcerptChars = 500
	// Single-record lookups return the description in full.
	fullText = math.MaxInt
)

// formatGraphQLError renders one entry of Linear's `errors` array, appending
// extensions.userPresentableMessage (or the raw extensions JSON) when present.
func formatGraphQLError(e any) string {
	m, _ := e.(map[string]any)
	msg, ok := m["message"].(string)
	if !ok {
		msg = "unknown error"
	}
	ext, has := m["extensions"]
	if !has {
		return msg
	}
	var detail string
	if em, ok := ext.(map[string]any); ok {
		if s, ok := em["userPresentableMessage"].(string); ok && s != "" {
			detail = s
		}
	}
	if detail == "" {
		detail = util.JSONString(ext)
	}
	if detail != msg {
		return fmt.Sprintf("%s (%s)", msg, detail)
	}
	return msg
}

// graphqlErrors returns a BadInput error built from a response's top-level
// `errors` array, or nil when there are none.
func graphqlErrors(top map[string]any) error {
	errs, ok := top["errors"].([]any)
	if !ok || len(errs) == 0 {
		return nil
	}
	parts := make([]string, len(errs))
	for i, e := range errs {
		parts[i] = formatGraphQLError(e)
	}
	return connector.BadInput("Linear GraphQL error: %s", strings.Join(parts, "; "))
}

// Entity is the Linear object a dataset is bound to
// (source_config.resource_type).
type Entity int

const (
	EntityIssues Entity = iota
	EntityProjects
	EntityTeams
)

// ParseEntity maps "projects"/"teams" to their entity; anything else is issues.
func ParseEntity(s string) Entity {
	switch s {
	case "projects":
		return EntityProjects
	case "teams":
		return EntityTeams
	}
	return EntityIssues
}

// Auth is how the connector authenticates. OAuth tokens are sent as
// `Authorization: Bearer <token>` (refreshed on demand); personal API keys are
// sent raw (`Authorization: <key>`, no Bearer prefix) and never expire.
type Auth struct {
	apiKey *string
	creds  *oauth.Credentials
}

// APIKeyAuth authenticates with a Linear personal API key.
func APIKeyAuth(key string) Auth { return Auth{apiKey: &key} }

// OAuthAuth authenticates with OAuth tokens stored for the datasource.
func OAuthAuth(creds oauth.Credentials) Auth { return Auth{creds: &creds} }

// Connector is the Linear connector.
type Connector struct {
	connector.Base
	datasourceID int64
	auth         Auth
	entity       Entity
	client       *http.Client
}

var _ connector.Connector = (*Connector)(nil)

// New builds a connector bound to the default entity (issues).
func New(dsID int64, auth Auth) *Connector {
	return WithEntity(dsID, auth, EntityIssues)
}

// WithEntity builds a connector bound to a specific entity.
func WithEntity(dsID int64, auth Auth, entity Entity) *Connector {
	return &Connector{datasourceID: dsID, auth: auth, entity: entity, client: &http.Client{}}
}

// authorize sets the Authorization header, refreshing the OAuth token first.
func (c *Connector) authorize(ctx context.Context, req *http.Request) error {
	if c.auth.apiKey != nil {
		req.Header.Set("Authorization", *c.auth.apiKey)
		return nil
	}
	var creds oauth.Credentials
	if c.auth.creds != nil {
		creds = *c.auth.creds
	}
	token, err := oauth.GetValidToken(ctx, c.datasourceID, creds, tokenURL)
	if err != nil {
		return connector.Auth("%s", err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+token)
	return nil
}

// graphql executes an operation and returns the `data` payload. Linear returns
// HTTP 200 with a top-level `errors` array for rejected requests; those are the
// caller's input to fix, so they surface as BadInput.
func (c *Connector) graphql(ctx context.Context, query string, variables map[string]any) (map[string]any, error) {
	if variables == nil {
		variables = map[string]any{}
	}
	body, err := util.MarshalJSON(map[string]any{"query": query, "variables": variables})
	if err != nil {
		return nil, connector.Query("%s", err.Error())
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, apiURL, bytes.NewReader(body))
	if err != nil {
		return nil, connector.Query("%s", err.Error())
	}
	req.Header.Set("Content-Type", "application/json")
	if err := c.authorize(ctx, req); err != nil {
		return nil, err
	}
	resp, err := c.client.Do(req)
	if err != nil {
		return nil, connector.Query("%s", err.Error())
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, connector.Query("%s", err.Error())
	}
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		// Linear rejects some invalid queries (e.g. "Query too complex") with
		// HTTP 400 and the usual `errors` array; report those like the 200 case.
		if resp.StatusCode == http.StatusBadRequest {
			if v, err := util.DecodeJSON(raw); err == nil {
				top, _ := v.(map[string]any)
				if err := graphqlErrors(top); err != nil {
					return nil, err
				}
			}
		}
		return nil, connector.FromUpstreamHTTP(resp.StatusCode, string(raw), "Linear API")
	}
	v, err := util.DecodeJSON(raw)
	if err != nil {
		return nil, connector.Query("invalid JSON from Linear: %s", err.Error())
	}
	top, _ := v.(map[string]any)
	if err := graphqlErrors(top); err != nil {
		return nil, err
	}
	data, _ := top["data"].(map[string]any)
	if data == nil {
		data = map[string]any{}
	}
	return data, nil
}

// fetchAllNodes walks a cursor-paginated connection and returns every node,
// up to maxScanRows. Linear has no totalCount, so this is the only way
// meta.total can be exact. The query must declare $first and $after and select
// pageInfo { hasNextPage endCursor }.
func (c *Connector) fetchAllNodes(ctx context.Context, query string, variables map[string]any, conn string) ([]any, error) {
	var nodes []any
	variables["first"] = linearPageSize
	for {
		data, err := c.graphql(ctx, query, variables)
		if err != nil {
			return nil, err
		}
		cv, ok := data[conn]
		if !ok {
			return nil, connector.Query("unexpected Linear response: missing %s", conn)
		}
		cm, _ := cv.(map[string]any)
		page, ok := cm["nodes"].([]any)
		if !ok {
			return nil, connector.Query("unexpected Linear response: missing %s.nodes", conn)
		}
		nodes = append(nodes, page...)

		cursor := ""
		if pi, ok := cm["pageInfo"].(map[string]any); ok {
			if hn, _ := pi["hasNextPage"].(bool); hn {
				cursor, _ = pi["endCursor"].(string)
			}
		}
		if cursor == "" || len(nodes) >= maxScanRows {
			break
		}
		variables["after"] = cursor
	}
	if len(nodes) > maxScanRows {
		nodes = nodes[:maxScanRows]
	}
	return nodes, nil
}

// decodeNode re-decodes a generic JSON node into a typed struct; `id` is
// required, mirroring the Rust struct's non-optional field.
func decodeNode(node any, out any, kind string) error {
	m, ok := node.(map[string]any)
	if !ok {
		return connector.Query("failed to parse Linear %s: invalid type, expected struct", kind)
	}
	if _, ok := m["id"].(string); !ok {
		return connector.Query("failed to parse Linear %s: missing field `id`", kind)
	}
	b, err := json.Marshal(m)
	if err != nil {
		return connector.Query("failed to parse Linear %s: %s", kind, err.Error())
	}
	if err := json.Unmarshal(b, out); err != nil {
		return connector.Query("failed to parse Linear %s: %s", kind, err.Error())
	}
	return nil
}

func searchFilter(field string, search *string) map[string]any {
	return map[string]any{field: map[string]any{"containsIgnoreCase": *search}}
}

// ── Issues ──

func (c *Connector) queryIssues(ctx context.Context, p *connector.QueryParams) (*models.Envelope, error) {
	// Linear's issue(id:) resolves both the UUID and the identifier (ENG-123).
	if p.ItemID != nil {
		q := fmt.Sprintf("query($id: String!) { issue(id: $id) { %s } }", issueFields)
		data, err := c.graphql(ctx, q, map[string]any{"id": *p.ItemID})
		if err != nil {
			return nil, err
		}
		if v := data["issue"]; v != nil {
			var issue linearIssue
			if err := decodeNode(v, &issue, "issue"); err != nil {
				return nil, err
			}
			return oneItemEnvelope(issueToJSON(&issue, fullText)), nil
		}
		return oneItemEnvelope(nil), nil
	}

	vars := map[string]any{"orderBy": "updatedAt"}
	if p.Search != nil {
		vars["filter"] = searchFilter("title", p.Search)
	}
	q := fmt.Sprintf("query($first: Int!, $after: String, $orderBy: PaginationOrderBy, $filter: IssueFilter) { "+
		"issues(first: $first, after: $after, orderBy: $orderBy, filter: $filter) { "+
		"nodes { %s } pageInfo { hasNextPage endCursor } "+
		"} "+
		"}", issueFields)
	nodes, err := c.fetchAllNodes(ctx, q, vars, "issues")
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		var issue linearIssue
		if err := decodeNode(n, &issue, "issue"); err != nil {
			return nil, err
		}
		items = append(items, issueToJSON(&issue, descriptionExcerptChars))
	}
	return finalizeList(items, p), nil
}

// resolveIssueStateID maps a workflow-state name (team-scoped, customizable)
// to its stateId for the issue's team, case-insensitively.
func (c *Connector) resolveIssueStateID(ctx context.Context, issueID, status string) (string, error) {
	q := "query($id: String!) { issue(id: $id) { team { states { nodes { id name } } } } }"
	data, err := c.graphql(ctx, q, map[string]any{"id": issueID})
	if err != nil {
		return "", err
	}
	issue, _ := data["issue"].(map[string]any)
	team, _ := issue["team"].(map[string]any)
	states, _ := team["states"].(map[string]any)
	nodes, ok := states["nodes"].([]any)
	if !ok {
		return "", connector.BadInput("Linear issue '%s' not found or has no team workflow states", issueID)
	}
	wanted := strings.TrimSpace(status)
	var available []string
	for _, n := range nodes {
		nm, _ := n.(map[string]any)
		name, isStr := nm["name"].(string)
		if asciiEqualFold(name, wanted) {
			if id, ok := nm["id"].(string); ok {
				return id, nil
			}
		}
		if isStr {
			available = append(available, name)
		}
	}
	return "", connector.BadInput("unknown workflow state '%s' for this issue's team; valid states: %s",
		status, strings.Join(available, ", "))
}

func asciiEqualFold(a, b string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := 0; i < len(a); i++ {
		x, y := a[i], b[i]
		if 'A' <= x && x <= 'Z' {
			x += 'a' - 'A'
		}
		if 'A' <= y && y <= 'Z' {
			y += 'a' - 'A'
		}
		if x != y {
			return false
		}
	}
	return true
}

// ── Projects ──

func (c *Connector) queryProjects(ctx context.Context, p *connector.QueryParams) (*models.Envelope, error) {
	if p.ItemID != nil {
		q := fmt.Sprintf("query($id: String!) { project(id: $id) { %s } }", projectFields)
		data, err := c.graphql(ctx, q, map[string]any{"id": *p.ItemID})
		if err != nil {
			return nil, err
		}
		if v := data["project"]; v != nil {
			var project linearProject
			if err := decodeNode(v, &project, "project"); err != nil {
				return nil, err
			}
			return oneItemEnvelope(projectToJSON(&project, fullText)), nil
		}
		return oneItemEnvelope(nil), nil
	}

	vars := map[string]any{"orderBy": "updatedAt"}
	if p.Search != nil {
		vars["filter"] = searchFilter("name", p.Search)
	}
	q := fmt.Sprintf("query($first: Int!, $after: String, $orderBy: PaginationOrderBy, $filter: ProjectFilter) { "+
		"projects(first: $first, after: $after, orderBy: $orderBy, filter: $filter) { "+
		"nodes { %s } pageInfo { hasNextPage endCursor } "+
		"} "+
		"}", projectFields)
	nodes, err := c.fetchAllNodes(ctx, q, vars, "projects")
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		var project linearProject
		if err := decodeNode(n, &project, "project"); err != nil {
			return nil, err
		}
		items = append(items, projectToJSON(&project, descriptionExcerptChars))
	}
	return finalizeList(items, p), nil
}

// ── Teams ──

func (c *Connector) queryTeams(ctx context.Context, p *connector.QueryParams) (*models.Envelope, error) {
	if p.ItemID != nil {
		q := fmt.Sprintf("query($id: String!) { team(id: $id) { %s } }", teamFields)
		data, err := c.graphql(ctx, q, map[string]any{"id": *p.ItemID})
		if err != nil {
			return nil, err
		}
		if v := data["team"]; v != nil {
			var team linearTeam
			if err := decodeNode(v, &team, "team"); err != nil {
				return nil, err
			}
			return oneItemEnvelope(teamToJSON(&team)), nil
		}
		return oneItemEnvelope(nil), nil
	}

	vars := map[string]any{}
	if p.Search != nil {
		vars["filter"] = searchFilter("name", p.Search)
	}
	q := fmt.Sprintf("query($first: Int!, $after: String, $filter: TeamFilter) { "+
		"teams(first: $first, after: $after, filter: $filter) { "+
		"nodes { %s } pageInfo { hasNextPage endCursor } "+
		"} "+
		"}", teamFields)
	nodes, err := c.fetchAllNodes(ctx, q, vars, "teams")
	if err != nil {
		return nil, err
	}
	items := make([]map[string]any, 0, len(nodes))
	for _, n := range nodes {
		var team linearTeam
		if err := decodeNode(n, &team, "team"); err != nil {
			return nil, err
		}
		items = append(items, teamToJSON(&team))
	}
	return finalizeList(items, p), nil
}

// ── Connector interface ──

func (c *Connector) Introspect(context.Context) (*connector.Schema, error) {
	switch c.entity {
	case EntityProjects:
		return projectsSchema(), nil
	case EntityTeams:
		return teamsSchema(), nil
	}
	return issuesSchema(), nil
}

func (c *Connector) Query(ctx context.Context, p *connector.QueryParams) (*models.Envelope, error) {
	if p == nil {
		d := connector.DefaultQueryParams()
		p = &d
	}
	switch c.entity {
	case EntityProjects:
		return c.queryProjects(ctx, p)
	case EntityTeams:
		return c.queryTeams(ctx, p)
	}
	return c.queryIssues(ctx, p)
}

// TestConnection queries `viewer`, the cheapest authorized query.
func (c *Connector) TestConnection(ctx context.Context) error {
	data, err := c.graphql(ctx, "query { viewer { id } }", map[string]any{})
	if err != nil {
		return err
	}
	if v, ok := data["viewer"].(map[string]any); ok {
		if _, ok := v["id"]; ok {
			return nil
		}
	}
	return connector.Connection("Linear did not return a viewer for this token")
}

func str(m map[string]any, k string) string {
	s, _ := m[k].(string)
	return s
}

// mutationPayload extracts `data[key]`, checking `success`.
func mutationPayload(data map[string]any, key, what string) (map[string]any, error) {
	v, ok := data[key]
	if !ok {
		return nil, connector.Query("Linear %s returned no payload", key)
	}
	m, _ := v.(map[string]any)
	if s, _ := m["success"].(bool); !s {
		return nil, connector.Query("Linear reported the %s", what)
	}
	return m, nil
}

func (c *Connector) InvokeAction(ctx context.Context, actionType string, params map[string]any) (any, error) {
	switch actionType {
	case "create_issue":
		req, err := parseCreateIssue(params)
		if err != nil {
			return nil, err
		}
		q := "mutation($input: IssueCreateInput!) { " +
			"issueCreate(input: $input) { success issue { id identifier url } } " +
			"}"
		data, err := c.graphql(ctx, q, map[string]any{"input": buildCreateIssueInput(req)})
		if err != nil {
			return nil, err
		}
		created, err := mutationPayload(data, "issueCreate", "issue was not created")
		if err != nil {
			return nil, err
		}
		issue, _ := created["issue"].(map[string]any)
		return map[string]any{
			"issue_id":   str(issue, "id"),
			"identifier": str(issue, "identifier"),
			"url":        str(issue, "url"),
		}, nil

	case "update_issue":
		req, err := parseUpdateIssue(params)
		if err != nil {
			return nil, err
		}
		input := buildUpdateIssueInput(req)
		// Status names are team-relative; resolve to the stateId the mutation needs.
		if req.Status != nil {
			stateID, err := c.resolveIssueStateID(ctx, req.IssueID, *req.Status)
			if err != nil {
				return nil, err
			}
			input["stateId"] = stateID
		}
		q := "mutation($id: String!, $input: IssueUpdateInput!) { " +
			"issueUpdate(id: $id, input: $input) { success issue { id identifier url } } " +
			"}"
		data, err := c.graphql(ctx, q, map[string]any{"id": req.IssueID, "input": input})
		if err != nil {
			return nil, err
		}
		updated, err := mutationPayload(data, "issueUpdate", "issue was not updated")
		if err != nil {
			return nil, err
		}
		issue, _ := updated["issue"].(map[string]any)
		return map[string]any{
			"issue_id":   str(issue, "id"),
			"identifier": str(issue, "identifier"),
			"url":        str(issue, "url"),
		}, nil

	case "create_project":
		req, err := parseCreateProject(params)
		if err != nil {
			return nil, err
		}
		q := "mutation($input: ProjectCreateInput!) { " +
			"projectCreate(input: $input) { success project { id name url } } " +
			"}"
		data, err := c.graphql(ctx, q, map[string]any{"input": buildCreateProjectInput(req)})
		if err != nil {
			return nil, err
		}
		created, err := mutationPayload(data, "projectCreate", "project was not created")
		if err != nil {
			return nil, err
		}
		project, _ := created["project"].(map[string]any)
		return map[string]any{
			"project_id": str(project, "id"),
			"name":       str(project, "name"),
			"url":        str(project, "url"),
		}, nil

	case "update_project":
		req, err := parseUpdateProject(params)
		if err != nil {
			return nil, err
		}
		q := "mutation($id: String!, $input: ProjectUpdateInput!) { " +
			"projectUpdate(id: $id, input: $input) { success project { id name url state } } " +
			"}"
		data, err := c.graphql(ctx, q, map[string]any{"id": req.ProjectID, "input": buildUpdateProjectInput(req)})
		if err != nil {
			return nil, err
		}
		updated, err := mutationPayload(data, "projectUpdate", "project was not updated")
		if err != nil {
			return nil, err
		}
		project, _ := updated["project"].(map[string]any)
		return map[string]any{
			"project_id": str(project, "id"),
			"name":       str(project, "name"),
			"state":      str(project, "state"),
			"url":        str(project, "url"),
		}, nil
	}
	return nil, connector.Unsupported("Linear does not support action '%s'", actionType)
}

// hashPrefix returns the first 8 hex chars of the canonical params hash and
// their numeric value.
func hashPrefix(params map[string]any) (string, uint64) {
	h := util.HashCanonicalJSON(params)[:8]
	var n uint64
	fmt.Sscanf(h, "%x", &n)
	return h, n
}

func (c *Connector) SimulateAction(_ context.Context, actionType string, params map[string]any) (any, error) {
	switch actionType {
	case "create_issue":
		// Validate with the real parser so bad bodies fail identically in staging.
		if _, err := parseCreateIssue(params); err != nil {
			return nil, err
		}
		h, suffix := hashPrefix(params)
		identifier := fmt.Sprintf("SIM-%d", 100+suffix%900)
		return map[string]any{
			"issue_id":   "sim-" + h,
			"identifier": identifier,
			"url":        "https://sim.linear.app/issue/" + identifier,
		}, nil
	case "update_issue":
		req, err := parseUpdateIssue(params)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"issue_id":   req.IssueID,
			"identifier": "SIM-UPDATED",
			"url":        "https://sim.linear.app/issue/updated",
		}, nil
	case "create_project":
		if _, err := parseCreateProject(params); err != nil {
			return nil, err
		}
		h, _ := hashPrefix(params)
		name, ok := params["name"].(string)
		if !ok {
			name = "Simulated Project"
		}
		return map[string]any{
			"project_id": "sim-proj-" + h,
			"name":       name,
			"url":        "https://sim.linear.app/project/" + h,
		}, nil
	case "update_project":
		req, err := parseUpdateProject(params)
		if err != nil {
			return nil, err
		}
		return map[string]any{
			"project_id": req.ProjectID,
			"name":       deref(req.Name),
			"state":      deref(req.State),
			"url":        "https://sim.linear.app/project/updated",
		}, nil
	}
	return nil, connector.Unsupported("Linear does not simulate action '%s'", actionType)
}

func (c *Connector) GenerateSyntheticRows(_ []models.DatasetField, count int64) ([]map[string]any, bool) {
	switch c.entity {
	case EntityProjects:
		return generateProjectRows(count), true
	case EntityTeams:
		return generateTeamRows(count), true
	}
	return generateIssueRows(count), true
}

func deref(s *string) string {
	if s == nil {
		return ""
	}
	return *s
}

// ── Shared list helpers ──

// finalizeList filters in memory, then slices offset/limit. total counts all
// matches (exact while the result set fits in maxScanRows).
func finalizeList(items []map[string]any, p *connector.QueryParams) *models.Envelope {
	items = connector.ApplyFiltersInMemory(items, p.Filters)
	total := int64(len(items))
	offset := max(p.Offset, 0)
	if offset < int64(len(items)) {
		items = items[offset:]
	} else {
		items = []map[string]any{}
	}
	if limit := max(p.Limit, 0); int64(len(items)) > limit {
		items = items[:limit]
	}
	return &models.Envelope{
		Items: items,
		Meta:  models.EnvelopeMeta{Total: total, Limit: p.Limit, Offset: p.Offset},
	}
}

// oneItemEnvelope is the envelope for a single-item lookup that may have missed.
func oneItemEnvelope(item map[string]any) *models.Envelope {
	items := []map[string]any{}
	if item != nil {
		items = append(items, item)
	}
	return &models.Envelope{
		Items: items,
		Meta:  models.EnvelopeMeta{Total: int64(len(items)), Limit: 1, Offset: 0},
	}
}
