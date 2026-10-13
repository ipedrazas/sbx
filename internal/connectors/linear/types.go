package linear

import (
	"encoding/json"
	"fmt"
	"math"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/util"
)

// ── Schemas ──

func field(name string, ft connector.FieldType, desc string) connector.SchemaField {
	return connector.SchemaField{Name: name, FieldType: ft, Description: &desc}
}

func issuesSchema() *connector.Schema {
	S, D, J := connector.FieldString, connector.FieldDateTime, connector.FieldJSON
	return &connector.Schema{Fields: []connector.SchemaField{
		field("id", S, "Linear issue ID (UUID)"),
		field("identifier", S, "Human issue identifier (e.g. ENG-123)"),
		field("title", S, "Issue title"),
		field("status", S, "Workflow state name (e.g. Todo, In Progress, Done)"),
		field("state_type", S, "Kind of workflow state, fixed across teams: triage, backlog, unstarted, "+
			"started, completed or canceled. Open issues are those not completed or canceled"),
		field("priority", S, "Priority label (e.g. Urgent, High, No priority)"),
		field("assignee", S, "Assignee display name"),
		field("creator", S, "Creator display name"),
		field("team", S, "Team key (e.g. ENG)"),
		field("team_id", S, "Team UUID — pass to the create_issue action"),
		field("project", S, "Project name the issue belongs to (empty if none)"),
		field("project_id", S, "Project UUID (empty if the issue has no project)"),
		field("created", D, "When the issue was created"),
		field("updated", D, "When the issue was last updated"),
		field("labels", J, "Issue label names"),
		field("description", S, "Issue description in Markdown: a 500-character excerpt in lists and "+
			"searches, the full text when fetching a single issue"),
		field("url", S, "Browser URL for the issue"),
	}}
}

func projectsSchema() *connector.Schema {
	S, D, J := connector.FieldString, connector.FieldDateTime, connector.FieldJSON
	return &connector.Schema{Fields: []connector.SchemaField{
		field("id", S, "Linear project ID (UUID)"),
		field("name", S, "Project name"),
		field("status", S, "Project state (backlog, planned, started, paused, completed, canceled)"),
		field("progress", connector.FieldFloat, "Completion fraction 0.0–1.0"),
		field("lead", S, "Project lead display name"),
		field("teams", J, "Keys of teams the project belongs to"),
		field("created", D, "When the project was created"),
		field("updated", D, "When the project was last updated"),
		field("description", S, "Project description (excerpt)"),
		field("url", S, "Browser URL for the project"),
	}}
}

func teamsSchema() *connector.Schema {
	S := connector.FieldString
	return &connector.Schema{Fields: []connector.SchemaField{
		field("id", S, "Linear team ID (UUID) — pass to create_issue/create_project"),
		field("key", S, "Team key (e.g. ENG)"),
		field("name", S, "Team name"),
		field("description", S, "Team description"),
	}}
}

// ── Linear API response types ──

type namedNode struct {
	Name *string `json:"name"`
}

type stateNode struct {
	Name *string `json:"name"`
	// WorkflowState.type: triage, backlog, unstarted, started, completed, canceled.
	Kind *string `json:"type"`
}

type idNameNode struct {
	ID   *string `json:"id"`
	Name *string `json:"name"`
}

type teamNode struct {
	ID  *string `json:"id"`
	Key *string `json:"key"`
}

type labelConnection struct {
	Nodes []namedNode `json:"nodes"`
}

type teamConnection struct {
	Nodes []teamNode `json:"nodes"`
}

type linearIssue struct {
	ID            string           `json:"id"`
	Identifier    *string          `json:"identifier"`
	Title         *string          `json:"title"`
	Description   *string          `json:"description"`
	PriorityLabel *string          `json:"priorityLabel"`
	CreatedAt     *string          `json:"createdAt"`
	UpdatedAt     *string          `json:"updatedAt"`
	URL           *string          `json:"url"`
	State         *stateNode       `json:"state"`
	Assignee      *namedNode       `json:"assignee"`
	Creator       *namedNode       `json:"creator"`
	Team          *teamNode        `json:"team"`
	Project       *idNameNode      `json:"project"`
	Labels        *labelConnection `json:"labels"`
}

type linearProject struct {
	ID          string          `json:"id"`
	Name        *string         `json:"name"`
	Description *string         `json:"description"`
	State       *string         `json:"state"`
	Progress    *float64        `json:"progress"`
	URL         *string         `json:"url"`
	CreatedAt   *string         `json:"createdAt"`
	UpdatedAt   *string         `json:"updatedAt"`
	Lead        *namedNode      `json:"lead"`
	Teams       *teamConnection `json:"teams"`
}

type linearTeam struct {
	ID          string  `json:"id"`
	Key         *string `json:"key"`
	Name        *string `json:"name"`
	Description *string `json:"description"`
}

func or(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}

func issueToJSON(i *linearIssue, descChars int) map[string]any {
	labels := []any{}
	if i.Labels != nil {
		for _, n := range i.Labels.Nodes {
			if n.Name != nil {
				labels = append(labels, *n.Name)
			}
		}
	}
	var status, stateType, assignee, creator, team, teamID, project, projectID string
	if i.State != nil {
		status, stateType = or(i.State.Name, ""), or(i.State.Kind, "")
	}
	if i.Assignee != nil {
		assignee = or(i.Assignee.Name, "")
	}
	if i.Creator != nil {
		creator = or(i.Creator.Name, "")
	}
	if i.Team != nil {
		team, teamID = or(i.Team.Key, ""), or(i.Team.ID, "")
	}
	if i.Project != nil {
		project, projectID = or(i.Project.Name, ""), or(i.Project.ID, "")
	}
	return map[string]any{
		"id":          i.ID,
		"identifier":  or(i.Identifier, i.ID),
		"title":       or(i.Title, ""),
		"status":      status,
		"state_type":  stateType,
		"priority":    or(i.PriorityLabel, ""),
		"assignee":    assignee,
		"creator":     creator,
		"team":        team,
		"team_id":     teamID,
		"project":     project,
		"project_id":  projectID,
		"created":     or(i.CreatedAt, ""),
		"updated":     or(i.UpdatedAt, ""),
		"labels":      labels,
		"description": util.Excerpt(or(i.Description, ""), descChars),
		"url":         or(i.URL, ""),
	}
}

func projectToJSON(p *linearProject, descChars int) map[string]any {
	teams := []any{}
	if p.Teams != nil {
		for _, n := range p.Teams.Nodes {
			if n.Key != nil {
				teams = append(teams, *n.Key)
			}
		}
	}
	progress := 0.0
	if p.Progress != nil {
		progress = *p.Progress
	}
	lead := ""
	if p.Lead != nil {
		lead = or(p.Lead.Name, "")
	}
	return map[string]any{
		"id":          p.ID,
		"name":        or(p.Name, ""),
		"status":      or(p.State, ""),
		"progress":    progress,
		"lead":        lead,
		"teams":       teams,
		"created":     or(p.CreatedAt, ""),
		"updated":     or(p.UpdatedAt, ""),
		"description": util.Excerpt(or(p.Description, ""), descChars),
		"url":         or(p.URL, ""),
	}
}

func teamToJSON(t *linearTeam) map[string]any {
	return map[string]any{
		"id":          t.ID,
		"key":         or(t.Key, ""),
		"name":        or(t.Name, ""),
		"description": or(t.Description, ""),
	}
}

// ── Action params (serde-like decoding) ──

// paramReader extracts typed fields from an action body, producing
// serde-style messages ("missing field `x`") wrapped as Query errors.
type paramReader struct {
	m      map[string]any
	action string
	err    error
}

func newReader(params map[string]any, action string) *paramReader {
	r := &paramReader{m: params, action: action}
	if params == nil {
		r.fail("invalid type: null, expected a map")
	}
	return r
}

func (r *paramReader) fail(format string, a ...any) {
	if r.err == nil {
		r.err = connector.Query("invalid %s params: %s", r.action, fmt.Sprintf(format, a...))
	}
}

func describe(v any) string {
	switch t := v.(type) {
	case bool:
		return fmt.Sprintf("boolean `%v`", t)
	case string:
		return fmt.Sprintf("string %q", t)
	case float64, json.Number:
		return "number `" + util.ValueAsString(t) + "`"
	case []any:
		return "sequence"
	case map[string]any:
		return "map"
	}
	return "value"
}

func (r *paramReader) reqString(k string) string {
	if r.err != nil {
		return ""
	}
	v, ok := r.m[k]
	if !ok {
		r.fail("missing field `%s`", k)
		return ""
	}
	s, ok := v.(string)
	if !ok {
		r.fail("invalid type: %s, expected a string", describe(v))
	}
	return s
}

func (r *paramReader) optString(k string) *string {
	if r.err != nil {
		return nil
	}
	v, ok := r.m[k]
	if !ok || v == nil {
		return nil
	}
	s, ok := v.(string)
	if !ok {
		r.fail("invalid type: %s, expected a string", describe(v))
		return nil
	}
	return &s
}

func (r *paramReader) optInt(k string) *int64 {
	if r.err != nil {
		return nil
	}
	v, ok := r.m[k]
	if !ok || v == nil {
		return nil
	}
	var f float64
	switch t := v.(type) {
	case float64:
		f = t
	case int:
		f = float64(t)
	case int64:
		return &t
	case json.Number:
		if n, err := t.Int64(); err == nil {
			return &n
		}
		x, err := t.Float64()
		if err != nil {
			r.fail("invalid type: %s, expected i64", describe(v))
			return nil
		}
		f = x
	default:
		r.fail("invalid type: %s, expected i64", describe(v))
		return nil
	}
	if f != math.Trunc(f) || math.Abs(f) > math.MaxInt64 {
		r.fail("invalid type: floating point `%v`, expected i64", f)
		return nil
	}
	n := int64(f)
	return &n
}

func (r *paramReader) optStrings(k string) []string {
	if r.err != nil {
		return nil
	}
	v, ok := r.m[k]
	if !ok || v == nil {
		return nil
	}
	arr, ok := v.([]any)
	if !ok {
		if ss, ok := v.([]string); ok {
			return append([]string{}, ss...)
		}
		r.fail("invalid type: %s, expected a sequence", describe(v))
		return nil
	}
	out := make([]string, 0, len(arr))
	for _, e := range arr {
		s, ok := e.(string)
		if !ok {
			r.fail("invalid type: %s, expected a string", describe(e))
			return nil
		}
		out = append(out, s)
	}
	return out
}

type createIssueParams struct {
	// Linear team UUID (issue creation keys off teamId, not the key).
	TeamID      string
	Title       string
	Description *string
	// 0 = none, 1 = urgent, 2 = high, 3 = medium, 4 = low.
	Priority   *int64
	AssigneeID *string
	ProjectID  *string
	LabelIDs   []string
}

func parseCreateIssue(params map[string]any) (*createIssueParams, error) {
	r := newReader(params, "create_issue")
	p := &createIssueParams{
		TeamID:      r.reqString("team_id"),
		Title:       r.reqString("title"),
		Description: r.optString("description"),
		Priority:    r.optInt("priority"),
		AssigneeID:  r.optString("assignee_id"),
		ProjectID:   r.optString("project_id"),
		LabelIDs:    r.optStrings("label_ids"),
	}
	return p, r.err
}

func buildCreateIssueInput(p *createIssueParams) map[string]any {
	in := map[string]any{"teamId": p.TeamID, "title": p.Title}
	if p.Description != nil {
		in["description"] = *p.Description
	}
	if p.Priority != nil {
		in["priority"] = *p.Priority
	}
	if p.AssigneeID != nil {
		in["assigneeId"] = *p.AssigneeID
	}
	if p.ProjectID != nil {
		in["projectId"] = *p.ProjectID
	}
	if p.LabelIDs != nil {
		in["labelIds"] = p.LabelIDs
	}
	return in
}

type updateIssueParams struct {
	// UUID or identifier (ENG-123); issueUpdate resolves both.
	IssueID     string
	Title       *string
	Description *string
	Priority    *int64
	AssigneeID  *string
	ProjectID   *string
	// Replaces the existing label set.
	LabelIDs []string
	// Workflow state name; resolved to a stateId before the mutation.
	Status *string
}

func parseUpdateIssue(params map[string]any) (*updateIssueParams, error) {
	r := newReader(params, "update_issue")
	p := &updateIssueParams{
		IssueID:     r.reqString("issue_id"),
		Title:       r.optString("title"),
		Description: r.optString("description"),
		Priority:    r.optInt("priority"),
		AssigneeID:  r.optString("assignee_id"),
		ProjectID:   r.optString("project_id"),
		LabelIDs:    r.optStrings("label_ids"),
		Status:      r.optString("status"),
	}
	return p, r.err
}

// buildUpdateIssueInput emits only set fields; status is resolved separately.
func buildUpdateIssueInput(p *updateIssueParams) map[string]any {
	in := map[string]any{}
	if p.Title != nil {
		in["title"] = *p.Title
	}
	if p.Description != nil {
		in["description"] = *p.Description
	}
	if p.Priority != nil {
		in["priority"] = *p.Priority
	}
	if p.AssigneeID != nil {
		in["assigneeId"] = *p.AssigneeID
	}
	if p.ProjectID != nil {
		in["projectId"] = *p.ProjectID
	}
	if p.LabelIDs != nil {
		in["labelIds"] = p.LabelIDs
	}
	return in
}

type createProjectParams struct {
	Name string
	// Filed as teamIds: [team_id]; Linear requires at least one team.
	TeamID      string
	Description *string
	LeadID      *string
	// backlog, planned, started, paused, completed, canceled.
	State *string
}

func parseCreateProject(params map[string]any) (*createProjectParams, error) {
	r := newReader(params, "create_project")
	p := &createProjectParams{
		Name:        r.reqString("name"),
		TeamID:      r.reqString("team_id"),
		Description: r.optString("description"),
		LeadID:      r.optString("lead_id"),
		State:       r.optString("state"),
	}
	return p, r.err
}

func buildCreateProjectInput(p *createProjectParams) map[string]any {
	in := map[string]any{"name": p.Name, "teamIds": []string{p.TeamID}}
	if p.Description != nil {
		in["description"] = *p.Description
	}
	if p.LeadID != nil {
		in["leadId"] = *p.LeadID
	}
	if p.State != nil {
		in["state"] = *p.State
	}
	return in
}

type updateProjectParams struct {
	ProjectID   string
	Name        *string
	Description *string
	LeadID      *string
	State       *string
}

func parseUpdateProject(params map[string]any) (*updateProjectParams, error) {
	r := newReader(params, "update_project")
	p := &updateProjectParams{
		ProjectID:   r.reqString("project_id"),
		Name:        r.optString("name"),
		Description: r.optString("description"),
		LeadID:      r.optString("lead_id"),
		State:       r.optString("state"),
	}
	return p, r.err
}

func buildUpdateProjectInput(p *updateProjectParams) map[string]any {
	in := map[string]any{}
	if p.Name != nil {
		in["name"] = *p.Name
	}
	if p.Description != nil {
		in["description"] = *p.Description
	}
	if p.LeadID != nil {
		in["leadId"] = *p.LeadID
	}
	if p.State != nil {
		in["state"] = *p.State
	}
	return in
}
