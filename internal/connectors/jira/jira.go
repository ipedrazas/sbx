// Package jira implements the Jira Cloud connector (OAuth 2.0 3LO).
package jira

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/oauth"
	"github.com/ipedrazas/sbx/internal/util"
)

// atlassianBase is the Atlassian API host; overridable in tests.
var atlassianBase = "https://api.atlassian.com"

// httpClient is used for all Jira requests; overridable in tests.
var httpClient = &http.Client{}

const issueFields = "summary,status,issuetype,priority,assignee,reporter,project,created,updated,labels,description"

type Connector struct {
	connector.Base
	datasourceID int64
	credentials  oauth.Credentials
	// Pre-resolved Atlassian Cloud ID (resolved lazily if nil).
	cloudID *string
}

var _ connector.Connector = (*Connector)(nil)

func New(datasourceID int64, creds oauth.Credentials, cloudID *string) *Connector {
	return &Connector{datasourceID: datasourceID, credentials: creds, cloudID: cloudID}
}

func (c *Connector) getToken(ctx context.Context) (string, error) {
	tok, err := oauth.GetValidToken(ctx, c.datasourceID, c.credentials, oauth.JiraTokenURL)
	if err != nil {
		return "", connector.Auth("%s", err.Error())
	}
	return tok, nil
}

// do sends a request with bearer auth. A transport failure is wrapped with
// netErr (Connection or Query, depending on the caller).
func do(ctx context.Context, method, url, token string, body any, netErr func(string, ...any) *connector.Error) (*http.Response, error) {
	var rd io.Reader
	if body != nil {
		b, err := util.MarshalJSON(body)
		if err != nil {
			return nil, netErr("%s", err.Error())
		}
		rd = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rd)
	if err != nil {
		return nil, netErr("%s", err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, netErr("%s", err.Error())
	}
	return resp, nil
}

func isSuccess(code int) bool { return code >= 200 && code < 300 }

// upstreamErr consumes the body and classifies a non-2xx response.
func upstreamErr(resp *http.Response) error {
	b, _ := io.ReadAll(resp.Body)
	return connector.FromUpstreamHTTP(resp.StatusCode, string(b), "Jira API")
}

// resolveCloudID returns the configured cloud ID or the first accessible
// resource from the accessible-resources endpoint.
func (c *Connector) resolveCloudID(ctx context.Context) (string, error) {
	if c.cloudID != nil {
		return *c.cloudID, nil
	}
	token, err := c.getToken(ctx)
	if err != nil {
		return "", err
	}
	resp, err := do(ctx, http.MethodGet, atlassianBase+"/oauth/token/accessible-resources", token, nil, connector.Connection)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return "", upstreamErr(resp)
	}
	var resources []struct {
		ID *string `json:"id"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&resources); err != nil {
		return "", connector.Connection("%s", err.Error())
	}
	if len(resources) == 0 {
		return "", connector.Connection("no accessible Jira sites found for this OAuth token")
	}
	if resources[0].ID == nil {
		return "", connector.Connection("missing field `id`")
	}
	return *resources[0].ID, nil
}

func apiBase(cloudID string) string {
	return fmt.Sprintf("%s/ex/jira/%s/rest/api/3", atlassianBase, cloudID)
}

func strField(name, desc string, ft connector.FieldType) connector.SchemaField {
	return connector.SchemaField{Name: name, FieldType: ft, Description: models.Ptr(desc)}
}

func (c *Connector) Introspect(context.Context) (*connector.Schema, error) {
	return &connector.Schema{Fields: []connector.SchemaField{
		strField("id", "Jira issue ID", connector.FieldString),
		strField("key", "Issue key (e.g. PROJ-123)", connector.FieldString),
		strField("summary", "Issue summary / title", connector.FieldString),
		strField("status", "Current status name", connector.FieldString),
		strField("issue_type", "Issue type (Bug, Story, Task, etc.)", connector.FieldString),
		strField("priority", "Priority name", connector.FieldString),
		strField("assignee", "Assignee display name", connector.FieldString),
		strField("reporter", "Reporter display name", connector.FieldString),
		strField("project", "Project key", connector.FieldString),
		strField("created", "When the issue was created", connector.FieldDateTime),
		strField("updated", "When the issue was last updated", connector.FieldDateTime),
		strField("labels", "Issue labels", connector.FieldJSON),
		strField("description", "Issue description (plain text excerpt)", connector.FieldString),
	}}, nil
}

func (c *Connector) Query(ctx context.Context, params *connector.QueryParams) (*models.Envelope, error) {
	cloudID, err := c.resolveCloudID(ctx)
	if err != nil {
		return nil, err
	}
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}
	base := apiBase(cloudID)

	// Single-issue lookup by key or ID.
	if params.ItemID != nil {
		resp, err := do(ctx, http.MethodGet, base+"/issue/"+*params.ItemID, token, nil, connector.Query)
		if err != nil {
			return nil, err
		}
		defer resp.Body.Close()
		if !isSuccess(resp.StatusCode) {
			return nil, upstreamErr(resp)
		}
		b, err := io.ReadAll(resp.Body)
		if err != nil {
			return nil, connector.Query("%s", err.Error())
		}
		issue, err := parseIssue(b)
		if err != nil {
			return nil, connector.Query("%s", err.Error())
		}
		return &models.Envelope{
			Items: []map[string]any{issueToJSON(issue)},
			Meta:  models.EnvelopeMeta{Total: 1, Limit: 1, Offset: 0},
		}, nil
	}

	// /search/jql requires a bounded query — `ORDER BY` alone is rejected.
	order := jqlOrderClause(params.Sort)
	var jql string
	if params.Search != nil {
		jql = fmt.Sprintf("text ~ \"%s\" %s", strings.ReplaceAll(*params.Search, `"`, `\"`), order)
	} else {
		jql = "updated >= -30d " + order
	}

	url := fmt.Sprintf("%s/search/jql?jql=%s&maxResults=%d&startAt=%d&fields=%s",
		base, util.URLEncode(jql), params.Limit, params.Offset, util.URLEncode(issueFields))
	resp, err := do(ctx, http.MethodGet, url, token, nil, connector.Query)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, connector.Query("%s", err.Error())
	}
	if !isSuccess(resp.StatusCode) {
		return nil, connector.FromUpstreamHTTP(resp.StatusCode, string(body), "Jira API")
	}

	raw, err := util.DecodeJSON(body)
	if err != nil {
		return nil, connector.Query("invalid JSON from Jira: %s", err.Error())
	}

	// The endpoint may return issues under "issues" or at the top level.
	var issueArray []any
	obj, isObj := raw.(map[string]any)
	if arr, ok := obj["issues"].([]any); isObj && ok {
		issueArray = arr
	} else if arr, ok := raw.([]any); ok {
		issueArray = arr
	} else {
		keys := "None"
		if isObj {
			keys = "Some(" + debugKeys(obj) + ")"
		}
		return nil, connector.Query("unexpected Jira response shape — top-level keys: %s", keys)
	}

	if len(issueArray) == 0 {
		slog.Debug("Jira returned 0 issues for JQL: " + jql)
	}

	items := make([]map[string]any, 0, len(issueArray))
	for _, val := range issueArray {
		b, _ := json.Marshal(val)
		issue, err := parseIssue(b)
		if err != nil {
			m, _ := val.(map[string]any)
			return nil, connector.Query("failed to parse Jira issue (keys: %s): %s", debugKeys(m), err.Error())
		}
		items = append(items, issueToJSON(issue))
	}

	items = connector.ApplyFiltersInMemory(items, params.Filters)

	total := int64(len(items))
	if n, ok := asInt64(obj["total"]); isObj && ok {
		total = n
	}

	return &models.Envelope{
		Items: items,
		Meta:  models.EnvelopeMeta{Total: total, Limit: params.Limit, Offset: params.Offset},
	}, nil
}

func (c *Connector) InvokeAction(ctx context.Context, actionType string, params map[string]any) (any, error) {
	if actionType != "create_issue" {
		return nil, connector.Unsupported("Jira does not support action '%s'", actionType)
	}
	req, err := parseCreateIssueParams(anyMap(params))
	if err != nil {
		return nil, connector.Query("invalid create_issue params: %s", err.Error())
	}
	cloudID, err := c.resolveCloudID(ctx)
	if err != nil {
		return nil, err
	}
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}
	resp, err := do(ctx, http.MethodPost, apiBase(cloudID)+"/issue", token, buildCreateIssueBody(req), connector.Query)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return nil, upstreamErr(resp)
	}
	var created struct {
		ID  *string `json:"id"`
		Key *string `json:"key"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&created); err != nil {
		return nil, connector.Query("%s", err.Error())
	}
	if created.ID == nil {
		return nil, connector.Query("missing field `id`")
	}
	if created.Key == nil {
		return nil, connector.Query("missing field `key`")
	}
	// Surface a browser URL so callers can link to the new issue.
	url := fmt.Sprintf("%s/ex/jira/%s/browse/%s", atlassianBase, cloudID, *created.Key)
	return map[string]any{
		"issue_id":  *created.ID,
		"issue_key": *created.Key,
		"url":       url,
	}, nil
}

func (c *Connector) SimulateAction(_ context.Context, actionType string, params map[string]any) (any, error) {
	if actionType != "create_issue" {
		return nil, connector.Unsupported("Jira does not simulate action '%s'", actionType)
	}
	// Validate via the real params parser so bad bodies surface identically
	// in staging and production.
	req, err := parseCreateIssueParams(anyMap(params))
	if err != nil {
		return nil, connector.Query("invalid create_issue params: %s", err.Error())
	}
	hash := util.HashCanonicalJSON(anyMap(params))
	// Fake numeric id roughly in the shape Jira returns (10000+).
	suffix, _ := strconv.ParseUint(hash[:8], 16, 64)
	issueID := strconv.FormatUint(10_000+(suffix%900_000), 10)
	issueKey := fmt.Sprintf("%s-SIM-%s", req.ProjectKey, hash[:6])
	return map[string]any{
		"issue_id":  issueID,
		"issue_key": issueKey,
		"url":       "https://sim.jira/browse/" + issueKey,
	}, nil
}

func (c *Connector) GenerateSyntheticRows(_ []models.DatasetField, count int64) ([]map[string]any, bool) {
	return generateJiraRows(count), true
}

func (c *Connector) TestConnection(ctx context.Context) error {
	cloudID, err := c.resolveCloudID(ctx)
	if err != nil {
		return err
	}
	token, err := c.getToken(ctx)
	if err != nil {
		return err
	}
	resp, err := do(ctx, http.MethodGet, apiBase(cloudID)+"/myself", token, nil, connector.Connection)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if !isSuccess(resp.StatusCode) {
		return upstreamErr(resp)
	}
	return nil
}

// SortCapability is sourced from the shared list so what's advertised matches
// what's honored.
func (c *Connector) SortCapability() connector.SortCapability {
	return connector.SortCapability{Kind: connector.SortFields, Fields: slices.Clone(connector.JiraSortableFields)}
}

// anyMap keeps a nil params map distinct from an empty object (Rust: Value::Null).
func anyMap(m map[string]any) any {
	if m == nil {
		return nil
	}
	return m
}

// jqlFieldName maps a source_name to its JQL ORDER BY name (only issue_type differs).
func jqlFieldName(source string) string {
	if source == "issue_type" {
		return "issuetype"
	}
	return source
}

// jqlOrderClause returns the ORDER BY clause for a requested sort, falling back
// to `ORDER BY updated DESC` when absent or not JQL-orderable (a silent no-op).
func jqlOrderClause(sort *models.SortSpec) string {
	if sort != nil && slices.Contains(connector.JiraSortableFields, sort.Field) {
		dir := "ASC"
		if sort.Descending {
			dir = "DESC"
		}
		return fmt.Sprintf("ORDER BY %s %s", jqlFieldName(sort.Field), dir)
	}
	return "ORDER BY updated DESC"
}

// ── Jira API response types ──

type jiraNameField struct {
	Name *string `json:"name"`
}

type jiraUser struct {
	DisplayName *string `json:"displayName"`
}

type jiraProject struct {
	Key *string `json:"key"`
}

type jiraFields struct {
	Summary     *string        `json:"summary"`
	Status      *jiraNameField `json:"status"`
	IssueType   *jiraNameField `json:"issuetype"`
	Priority    *jiraNameField `json:"priority"`
	Assignee    *jiraUser      `json:"assignee"`
	Reporter    *jiraUser      `json:"reporter"`
	Project     *jiraProject   `json:"project"`
	Created     *string        `json:"created"`
	Updated     *string        `json:"updated"`
	Labels      []string       `json:"labels"`
	Description any            `json:"description"`
}

type jiraIssue struct {
	ID     *string     `json:"id"`
	Key    *string     `json:"key"`
	Fields *jiraFields `json:"fields"`
}

func parseIssue(b []byte) (*jiraIssue, error) {
	var issue jiraIssue
	if err := json.Unmarshal(b, &issue); err != nil {
		var te *json.UnmarshalTypeError
		if errors.As(err, &te) {
			return nil, fmt.Errorf("invalid type: %s, expected %s at %s", te.Value, te.Type, te.Field)
		}
		return nil, err
	}
	if issue.ID == nil {
		return nil, errors.New("missing field `id`")
	}
	return &issue, nil
}

func deref(s *string, def string) string {
	if s == nil {
		return def
	}
	return *s
}

func issueToJSON(issue *jiraIssue) map[string]any {
	f := issue.Fields
	if f == nil {
		f = &jiraFields{}
	}
	name := func(n *jiraNameField) string {
		if n == nil {
			return ""
		}
		return deref(n.Name, "")
	}
	user := func(u *jiraUser) string {
		if u == nil {
			return ""
		}
		return deref(u.DisplayName, "")
	}
	project := ""
	if f.Project != nil {
		project = deref(f.Project.Key, "")
	}
	description := ""
	if f.Description != nil {
		description = extractPlainText(f.Description)
	}
	labels := f.Labels
	if labels == nil {
		labels = []string{}
	}
	return map[string]any{
		"id":          *issue.ID,
		"key":         deref(issue.Key, *issue.ID),
		"summary":     deref(f.Summary, ""),
		"status":      name(f.Status),
		"issue_type":  name(f.IssueType),
		"priority":    name(f.Priority),
		"assignee":    user(f.Assignee),
		"reporter":    user(f.Reporter),
		"project":     project,
		"created":     deref(f.Created, ""),
		"updated":     deref(f.Updated, ""),
		"labels":      strSlice(labels),
		"description": description,
	}
}

// extractPlainText walks Jira's ADF description and concatenates text nodes.
func extractPlainText(adf any) string {
	var sb strings.Builder
	collectText(adf, &sb)
	return util.Excerpt(sb.String(), 500)
}

func collectText(node any, out *strings.Builder) {
	m, ok := node.(map[string]any)
	if !ok {
		return
	}
	if text, ok := m["text"].(string); ok {
		out.WriteString(text)
	}
	if content, ok := m["content"].([]any); ok {
		for _, child := range content {
			collectText(child, out)
		}
		// Newline between top-level blocks (paragraphs, etc.).
		if t, _ := m["type"].(string); t == "paragraph" {
			out.WriteByte('\n')
		}
	}
}

// debugKeys renders object keys like Rust's `{:?}` of a Vec<&str> (sorted, as
// serde_json's default map is a BTreeMap).
func debugKeys(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = strconv.Quote(k)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}

// asInt64 mirrors serde_json's Value::as_i64.
func asInt64(v any) (int64, bool) {
	switch n := v.(type) {
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}
