// Package github implements the GitHub connector (issues and repository files).
package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"unicode"
	"unicode/utf8"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/oauth"
	"github.com/ipedrazas/sbx/internal/util"
)

// apiBase is the GitHub REST API root; overridable in tests.
var apiBase = "https://api.github.com"

// httpClient is used for all GitHub requests; overridable in tests.
var httpClient = &http.Client{}

const (
	githubAccept     = "application/vnd.github+json"
	githubAPIVersion = "2022-11-28"
	userAgent        = "dstudio"

	maxFileContentBytes = 512 * 1024 // 512 KB
)

type ResourceType int

const (
	Issues ResourceType = iota
	RepoFiles
)

// ParseResourceType maps "repo_files" to RepoFiles and anything else to Issues.
func ParseResourceType(s string) ResourceType {
	if s == "repo_files" {
		return RepoFiles
	}
	return Issues
}

type Connector struct {
	connector.Base
	datasourceID int64
	credentials  oauth.Credentials
	repository   *string
	resourceType ResourceType
	branch       *string
}

var _ connector.Connector = (*Connector)(nil)

func New(datasourceID int64, creds oauth.Credentials, repository *string, resourceType ResourceType, branch *string) *Connector {
	return &Connector{
		datasourceID: datasourceID,
		credentials:  creds,
		repository:   repository,
		resourceType: resourceType,
		branch:       branch,
	}
}

func (c *Connector) getToken(ctx context.Context) (string, error) {
	tok, err := oauth.GetValidToken(ctx, c.datasourceID, c.credentials, oauth.GithubTokenURL)
	if err != nil {
		return "", connector.Auth("%s", err.Error())
	}
	return tok, nil
}

// get performs an authenticated GET and returns status and body. Transport
// failures are wrapped with netErr.
func get(ctx context.Context, url, token, accept string, netErr func(string, ...any) *connector.Error) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, nil, netErr("%s", err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", accept)
	req.Header.Set("X-GitHub-Api-Version", githubAPIVersion)
	req.Header.Set("User-Agent", userAgent)
	resp, err := httpClient.Do(req)
	if err != nil {
		return 0, nil, netErr("%s", err.Error())
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return resp.StatusCode, nil, netErr("%s", err.Error())
	}
	return resp.StatusCode, body, nil
}

// getJSON GETs url and decodes the 2xx body; jsonCtx prefixes decode errors.
func getJSON(ctx context.Context, url, token, accept, jsonCtx string) (any, error) {
	status, body, err := get(ctx, url, token, accept, connector.Query)
	if err != nil {
		return nil, err
	}
	if status < 200 || status >= 300 {
		return nil, connector.FromUpstreamHTTP(status, string(body), "GitHub API")
	}
	raw, err := util.DecodeJSON(body)
	if err != nil {
		return nil, connector.Query("%s: %s", jsonCtx, err.Error())
	}
	return raw, nil
}

func (c *Connector) requireRepo() (string, error) {
	if c.repository == nil {
		return "", connector.Config("repository is required for this resource type")
	}
	return *c.repository, nil
}

func (c *Connector) resolveBranch(ctx context.Context, token string) (string, error) {
	if c.branch != nil {
		return *c.branch, nil
	}
	repo, err := c.requireRepo()
	if err != nil {
		return "", err
	}
	raw, err := getJSON(ctx, fmt.Sprintf("%s/repos/%s", apiBase, repo), token, githubAccept, "invalid JSON")
	if err != nil {
		return "", err
	}
	if b, ok := getStr(raw, "default_branch"); ok {
		return b, nil
	}
	return "", connector.Query("could not determine default branch")
}

func field(name string, ft connector.FieldType, desc string) connector.SchemaField {
	return connector.SchemaField{Name: name, FieldType: ft, Description: models.Ptr(desc)}
}

func repoFilesSchema() *connector.Schema {
	return &connector.Schema{Fields: []connector.SchemaField{
		field("path", connector.FieldString, "File path relative to repo root"),
		field("name", connector.FieldString, "File or directory name"),
		field("type", connector.FieldString, "blob (file) or tree (directory)"),
		field("size", connector.FieldInteger, "File size in bytes (0 for directories)"),
		field("sha", connector.FieldString, "Git object SHA"),
		field("content", connector.FieldString, "File content (UTF-8, populated on individual get, empty in listings)"),
		field("download_url", connector.FieldString, "Raw download URL"),
	}}
}

func issuesSchema() *connector.Schema {
	return &connector.Schema{Fields: []connector.SchemaField{
		field("id", connector.FieldInteger, "GitHub issue ID"),
		field("number", connector.FieldInteger, "Issue number within the repository"),
		field("title", connector.FieldString, "Issue title"),
		field("state", connector.FieldString, "open or closed"),
		field("repository", connector.FieldString, "owner/repo slug"),
		field("user", connector.FieldString, "Issue author login"),
		field("assignee", connector.FieldString, "Primary assignee login"),
		field("labels", connector.FieldJSON, "Label names"),
		field("is_pull_request", connector.FieldBoolean, "True when the item is a pull request"),
		field("created_at", connector.FieldDateTime, "When the issue was created"),
		field("updated_at", connector.FieldDateTime, "When the issue was last updated"),
		field("html_url", connector.FieldString, "Web URL for the issue"),
		field("body", connector.FieldString, "Issue body (truncated)"),
	}}
}

func clamp(v, lo, hi int64) int64 { return max(lo, min(v, hi)) }

// ── Repo files ──

func (c *Connector) queryRepoFiles(ctx context.Context, params *connector.QueryParams) (*models.Envelope, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}
	repo, err := c.requireRepo()
	if err != nil {
		return nil, err
	}
	if params.ItemID != nil {
		return c.getFileByPath(ctx, token, repo, *params.ItemID)
	}
	if params.Search != nil {
		return c.searchCode(ctx, token, repo, *params.Search, params)
	}
	return c.listTree(ctx, token, repo, params)
}

func (c *Connector) listTree(ctx context.Context, token, repo string, params *connector.QueryParams) (*models.Envelope, error) {
	branch, err := c.resolveBranch(ctx, token)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/repos/%s/git/trees/%s?recursive=1", apiBase, repo, branch)
	raw, err := getJSON(ctx, url, token, githubAccept, "invalid JSON")
	if err != nil {
		return nil, err
	}
	tree, ok := getArr(raw, "tree")
	if !ok {
		return nil, connector.Query("missing tree array in response")
	}
	items := make([]map[string]any, 0, len(tree))
	for _, e := range tree {
		items = append(items, treeEntryToJSON(e))
	}
	items = connector.ApplyFiltersInMemory(items, params.Filters)

	total := int64(len(items))
	offset := min(params.Offset, total)
	limit := clamp(params.Limit, 1, 100)
	page := items
	if offset > 0 {
		page = items[offset:]
	} else if offset < 0 {
		// Rust casts a negative offset to a huge usize → empty page.
		page = nil
	}
	if int64(len(page)) > limit {
		page = page[:limit]
	}
	if page == nil {
		page = []map[string]any{}
	}
	return &models.Envelope{
		Items: page,
		Meta:  models.EnvelopeMeta{Total: total, Limit: limit, Offset: offset},
	}, nil
}

func (c *Connector) getFileByPath(ctx context.Context, token, repo, path string) (*models.Envelope, error) {
	url := fmt.Sprintf("%s/repos/%s/contents/%s", apiBase, repo, path)
	if c.branch != nil {
		url += "?ref=" + util.URLEncode(*c.branch)
	}
	raw, err := getJSON(ctx, url, token, githubAccept, "invalid JSON")
	if err != nil {
		return nil, err
	}
	var items []map[string]any
	if arr, ok := raw.([]any); ok {
		items = make([]map[string]any, 0, len(arr))
		for _, e := range arr {
			items = append(items, contentsEntryToJSON(e))
		}
	} else {
		items = []map[string]any{contentsEntryToJSON(raw)}
	}
	total := int64(len(items))
	return &models.Envelope{
		Items: items,
		Meta:  models.EnvelopeMeta{Total: total, Limit: total, Offset: 0},
	}, nil
}

func (c *Connector) searchCode(ctx context.Context, token, repo, query string, params *connector.QueryParams) (*models.Envelope, error) {
	perPage := clamp(params.Limit, 1, 100)
	page := params.Offset/perPage + 1

	q := fmt.Sprintf("%s repo:%s", query, repo)
	url := fmt.Sprintf("%s/search/code?q=%s&per_page=%d&page=%d", apiBase, util.URLEncode(q), perPage, page)
	raw, err := getJSON(ctx, url, token, "application/vnd.github.text-match+json", "invalid JSON")
	if err != nil {
		return nil, err
	}
	total, _ := getInt(raw, "total_count")
	arr, _ := getArr(raw, "items")
	items := make([]map[string]any, 0, len(arr))
	for _, e := range arr {
		items = append(items, codeSearchToJSON(e))
	}
	items = connector.ApplyFiltersInMemory(items, params.Filters)
	return &models.Envelope{
		Items: items,
		Meta:  models.EnvelopeMeta{Total: total, Limit: perPage, Offset: params.Offset},
	}, nil
}

// ── Issues ──

func (c *Connector) queryIssues(ctx context.Context, params *connector.QueryParams) (*models.Envelope, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}
	perPage := clamp(params.Limit, 1, 100)
	page := params.Offset/perPage + 1

	var url string
	switch {
	case params.Search != nil:
		var qualifier string
		if c.repository != nil {
			qualifier = fmt.Sprintf("%s repo:%s is:issue", *params.Search, *c.repository)
		} else {
			qualifier = fmt.Sprintf("%s is:open assignee:@me", *params.Search)
		}
		url = fmt.Sprintf("%s/search/issues?q=%s&per_page=%d&page=%d", apiBase, util.URLEncode(qualifier), perPage, page)
	case c.repository != nil:
		url = fmt.Sprintf("%s/repos/%s/issues?state=open&per_page=%d&page=%d", apiBase, *c.repository, perPage, page)
	default:
		url = fmt.Sprintf("%s/issues?filter=assigned&state=open&per_page=%d&page=%d", apiBase, perPage, page)
	}

	raw, err := getJSON(ctx, url, token, githubAccept, "invalid JSON from GitHub")
	if err != nil {
		return nil, err
	}

	var issueArray []any
	var totalHint *int64
	if arr, ok := raw.([]any); ok {
		issueArray = arr
	} else if arr, ok := getArr(raw, "items"); ok {
		issueArray = arr
		if n, ok := getInt(raw, "total_count"); ok {
			totalHint = &n
		}
	} else {
		keys := "None"
		if m, ok := raw.(map[string]any); ok {
			keys = "Some(" + debugKeys(m) + ")"
		}
		return nil, connector.Query("unexpected GitHub response shape — top-level keys: %s", keys)
	}

	items := make([]map[string]any, 0, len(issueArray))
	for _, e := range issueArray {
		items = append(items, issueToJSON(e))
	}
	items = connector.ApplyFiltersInMemory(items, params.Filters)

	total := int64(len(items)) + params.Offset
	if totalHint != nil {
		total = *totalHint
	}
	return &models.Envelope{
		Items: items,
		Meta:  models.EnvelopeMeta{Total: total, Limit: params.Limit, Offset: params.Offset},
	}, nil
}

// ── Connector impl ──

func (c *Connector) Introspect(context.Context) (*connector.Schema, error) {
	if c.resourceType == RepoFiles {
		return repoFilesSchema(), nil
	}
	return issuesSchema(), nil
}

func (c *Connector) Query(ctx context.Context, params *connector.QueryParams) (*models.Envelope, error) {
	if c.resourceType == RepoFiles {
		return c.queryRepoFiles(ctx, params)
	}
	return c.queryIssues(ctx, params)
}

func (c *Connector) TestConnection(ctx context.Context) error {
	token, err := c.getToken(ctx)
	if err != nil {
		return err
	}
	status, body, err := get(ctx, apiBase+"/user", token, githubAccept, connector.Connection)
	if err != nil {
		return err
	}
	if status < 200 || status >= 300 {
		return connector.FromUpstreamHTTP(status, string(body), "GitHub API")
	}
	var user struct {
		Login *string `json:"login"`
	}
	if err := json.Unmarshal(body, &user); err != nil {
		return connector.Connection("%s", err.Error())
	}
	if user.Login == nil {
		return connector.Connection("missing field `login`")
	}
	return nil
}

// ── JSON mapping helpers ──

func getStr(v any, key string) (string, bool) {
	m, _ := v.(map[string]any)
	s, ok := m[key].(string)
	return s, ok
}

func strOr(v any, key, def string) string {
	if s, ok := getStr(v, key); ok {
		return s
	}
	return def
}

func getArr(v any, key string) ([]any, bool) {
	m, _ := v.(map[string]any)
	a, ok := m[key].([]any)
	return a, ok
}

// getInt mirrors serde_json's `.get(key).and_then(Value::as_i64)`.
func getInt(v any, key string) (int64, bool) {
	m, _ := v.(map[string]any)
	switch n := m[key].(type) {
	case json.Number:
		i, err := n.Int64()
		return i, err == nil
	}
	return 0, false
}

func intOr0(v any, key string) int64 {
	n, _ := getInt(v, key)
	return n
}

func treeEntryToJSON(val any) map[string]any {
	path := strOr(val, "path", "")
	name := path
	if i := strings.LastIndexByte(path, '/'); i >= 0 {
		name = path[i+1:]
	}
	return map[string]any{
		"path":         path,
		"name":         name,
		"type":         strOr(val, "type", "blob"),
		"size":         intOr0(val, "size"),
		"sha":          strOr(val, "sha", ""),
		"content":      "",
		"download_url": "",
	}
}

func decodeContent(val any) string {
	encoded, ok := getStr(val, "content")
	if !ok {
		return ""
	}
	cleaned := strings.Map(func(r rune) rune {
		if unicode.IsSpace(r) {
			return -1
		}
		return r
	}, encoded)
	b, err := base64.StdEncoding.Strict().DecodeString(cleaned)
	if err != nil {
		return ""
	}
	if len(b) > maxFileContentBytes {
		return fmt.Sprintf("%s\n\n[truncated at %d bytes]", lossyUTF8(b[:maxFileContentBytes]), maxFileContentBytes)
	}
	if !utf8.Valid(b) {
		return ""
	}
	return string(b)
}

// lossyUTF8 mirrors String::from_utf8_lossy: invalid bytes become U+FFFD.
func lossyUTF8(b []byte) string {
	var sb strings.Builder
	sb.Grow(len(b))
	for len(b) > 0 {
		r, size := utf8.DecodeRune(b)
		if r == utf8.RuneError && size == 1 {
			sb.WriteRune(utf8.RuneError)
		} else {
			sb.Write(b[:size])
		}
		b = b[size:]
	}
	return sb.String()
}

func contentsEntryToJSON(val any) map[string]any {
	return map[string]any{
		"path":         strOr(val, "path", ""),
		"name":         strOr(val, "name", ""),
		"type":         strOr(val, "type", "file"),
		"size":         intOr0(val, "size"),
		"sha":          strOr(val, "sha", ""),
		"content":      decodeContent(val),
		"download_url": strOr(val, "download_url", ""),
	}
}

func codeSearchToJSON(val any) map[string]any {
	var frags []string
	if matches, ok := getArr(val, "text_matches"); ok {
		for _, m := range matches {
			if f, ok := getStr(m, "fragment"); ok {
				frags = append(frags, f)
			}
		}
	}
	return map[string]any{
		"path":         strOr(val, "path", ""),
		"name":         strOr(val, "name", ""),
		"type":         "blob",
		"size":         intOr0(val, "size"),
		"sha":          strOr(val, "sha", ""),
		"content":      strings.Join(frags, "\n---\n"),
		"download_url": strOr(val, "html_url", ""),
	}
}

func issueToJSON(val any) map[string]any {
	m, _ := val.(map[string]any)

	repository := ""
	if u, ok := getStr(m, "repository_url"); ok && strings.HasPrefix(u, "https://api.github.com/repos/") {
		repository = strings.TrimPrefix(u, "https://api.github.com/repos/")
	} else if fn, ok := getStr(m["repository"], "full_name"); ok {
		repository = fn
	}

	labels := []any{}
	if arr, ok := getArr(m, "labels"); ok {
		for _, l := range arr {
			if n, ok := getStr(l, "name"); ok {
				labels = append(labels, n)
			}
		}
	}

	_, isPR := m["pull_request"]

	body := ""
	if b, ok := getStr(m, "body"); ok {
		body = util.Excerpt(b, 500)
	}

	return map[string]any{
		"id":              m["id"],
		"number":          m["number"],
		"title":           strOr(m, "title", ""),
		"state":           strOr(m, "state", ""),
		"repository":      repository,
		"user":            strOr(m["user"], "login", ""),
		"assignee":        strOr(m["assignee"], "login", ""),
		"labels":          labels,
		"is_pull_request": isPR,
		"created_at":      strOr(m, "created_at", ""),
		"updated_at":      strOr(m, "updated_at", ""),
		"html_url":        strOr(m, "html_url", ""),
		"body":            body,
	}
}

// debugKeys renders object keys like Rust's `{:?}` of a Vec<&String> (sorted).
func debugKeys(m map[string]any) string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	parts := make([]string, len(keys))
	for i, k := range keys {
		parts[i] = fmt.Sprintf("%q", k)
	}
	return "[" + strings.Join(parts, ", ") + "]"
}
