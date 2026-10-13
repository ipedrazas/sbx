// Package gdrive implements the Google Drive connector (read-only file listing).
package gdrive

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/oauth"
	"github.com/ipedrazas/sbx/internal/util"
)

// APIBase is the real Drive API base URL.
const APIBase = "https://www.googleapis.com/drive/v3"

const fileFields = "id,name,mimeType,createdTime,modifiedTime,size,webViewLink,owners,shared,starred,trashed"

type Connector struct {
	connector.Base
	datasourceID int64
	credentials  oauth.Credentials
	// BaseURL defaults to APIBase; overridable for tests.
	BaseURL string
	// TokenURL defaults to oauth.GoogleTokenURL.
	TokenURL string
	Client   *http.Client
}

func New(datasourceID int64, creds oauth.Credentials) *Connector {
	return &Connector{
		datasourceID: datasourceID,
		credentials:  creds,
		BaseURL:      APIBase,
		TokenURL:     oauth.GoogleTokenURL,
		Client:       &http.Client{},
	}
}

func (c *Connector) getToken(ctx context.Context) (string, error) {
	tok, err := oauth.GetValidToken(ctx, c.datasourceID, c.credentials, c.TokenURL)
	if err != nil {
		return "", connector.Auth("%s", err.Error())
	}
	return tok, nil
}

func (c *Connector) get(ctx context.Context, url, token string, transportErr func(string, ...any) *connector.Error) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, transportErr("%s", err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+token)
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, transportErr("%s", err.Error())
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, connector.FromUpstreamHTTP(resp.StatusCode, string(data), "Drive API")
	}
	if err != nil {
		return nil, connector.Query("%s", err.Error())
	}
	return data, nil
}

type driveFile struct {
	ID           *string `json:"id"`
	Name         *string `json:"name"`
	MimeType     *string `json:"mimeType"`
	CreatedTime  *string `json:"createdTime"`
	ModifiedTime *string `json:"modifiedTime"`
	Size         *string `json:"size"`
	WebViewLink  *string `json:"webViewLink"`
	Owners       []struct {
		EmailAddress *string `json:"emailAddress"`
	} `json:"owners"`
	Shared  *bool `json:"shared"`
	Starred *bool `json:"starred"`
	Trashed *bool `json:"trashed"`
}

func (c *Connector) fetchFile(ctx context.Context, fileID string) (map[string]any, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/files/%s?fields=%s", c.BaseURL, util.URLEncode(fileID), fileFields)
	data, err := c.get(ctx, url, token, connector.Query)
	if err != nil {
		return nil, err
	}
	var f driveFile
	if err := json.Unmarshal(data, &f); err != nil {
		return nil, connector.Query("%s", err.Error())
	}
	return fileToJSON(&f), nil
}

func (c *Connector) Introspect(context.Context) (*connector.Schema, error) {
	f := func(name string, t connector.FieldType, desc string) connector.SchemaField {
		return connector.SchemaField{Name: name, FieldType: t, Description: models.Ptr(desc)}
	}
	return &connector.Schema{Fields: []connector.SchemaField{
		f("id", connector.FieldString, "Drive file ID"),
		f("name", connector.FieldString, "File name"),
		f("mime_type", connector.FieldString, "MIME type"),
		f("created_time", connector.FieldDateTime, "When the file was created"),
		f("modified_time", connector.FieldDateTime, "When the file was last modified"),
		f("size", connector.FieldString, "File size in bytes"),
		f("web_view_link", connector.FieldString, "Link to open the file in a browser"),
		f("owner", connector.FieldString, "Owner email address"),
		f("shared", connector.FieldBoolean, "Whether the file is shared"),
		f("starred", connector.FieldBoolean, "Whether the file is starred"),
		f("trashed", connector.FieldBoolean, "Whether the file is in the trash"),
	}}, nil
}

// buildQuery splits filters into a Drive `q` expression and the remainder to
// apply in memory.
func buildQuery(params *connector.QueryParams) (string, []models.DatasetFilter) {
	var parts []string
	var inMemory []models.DatasetFilter
	hasOr := false
	for _, f := range params.Filters {
		v := util.ValueAsString(f.Value)
		var part string
		switch {
		case f.FieldName == "name" && f.Operator == models.OpContains:
			part = fmt.Sprintf("name contains '%s'", escapeDriveQuery(v))
		case f.FieldName == "name" && f.Operator == models.OpEquals:
			part = fmt.Sprintf("name = '%s'", escapeDriveQuery(v))
		case f.FieldName == "mime_type" && f.Operator == models.OpEquals:
			part = fmt.Sprintf("mimeType = '%s'", escapeDriveQuery(v))
		case f.FieldName == "mime_type" && f.Operator == models.OpContains:
			part = fmt.Sprintf("mimeType contains '%s'", escapeDriveQuery(v))
		case f.FieldName == "trashed" && f.Operator == models.OpEquals:
			part = "trashed = " + v
		case f.FieldName == "starred" && f.Operator == models.OpEquals:
			part = "starred = " + v
		default:
			inMemory = append(inMemory, f)
			continue
		}
		if f.Logic == models.LogicOr {
			hasOr = true
		}
		parts = append(parts, part)
	}

	var q []string
	// Exclude trashed files unless there's an explicit trashed filter.
	if !slices.ContainsFunc(params.Filters, func(f models.DatasetFilter) bool { return f.FieldName == "trashed" }) {
		q = append(q, "trashed = false")
	}
	if len(parts) > 0 {
		if hasOr && len(parts) > 1 {
			q = append(q, "("+strings.Join(parts, " or ")+")")
		} else {
			q = append(q, strings.Join(parts, " and "))
		}
	}
	if params.Search != nil {
		q = append(q, fmt.Sprintf("fullText contains '%s'", escapeDriveQuery(*params.Search)))
	}
	return strings.Join(q, " and "), inMemory
}

func (c *Connector) Query(ctx context.Context, params *connector.QueryParams) (*models.Envelope, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}

	if params.ItemID != nil {
		item, err := c.fetchFile(ctx, *params.ItemID)
		if err != nil {
			return nil, err
		}
		return &models.Envelope{Items: []map[string]any{item}, Meta: models.EnvelopeMeta{Total: 1, Limit: 1}}, nil
	}

	q, inMemory := buildQuery(params)
	url := fmt.Sprintf("%s/files?pageSize=%d&fields=files(%s),nextPageToken", c.BaseURL, params.Limit, fileFields)
	if q != "" {
		url += "&q=" + util.URLEncode(q)
	}
	data, err := c.get(ctx, url, token, connector.Query)
	if err != nil {
		return nil, err
	}
	var list struct {
		Files         []driveFile `json:"files"`
		NextPageToken *string     `json:"nextPageToken"`
	}
	if err := json.Unmarshal(data, &list); err != nil {
		return nil, connector.Query("%s", err.Error())
	}
	items := make([]map[string]any, 0, len(list.Files))
	for i := range list.Files {
		items = append(items, fileToJSON(&list.Files[i]))
	}
	items = connector.ApplyFiltersInMemory(items, inMemory)
	return &models.Envelope{Items: items, Meta: models.EnvelopeMeta{Total: int64(len(items)), Limit: params.Limit, Offset: params.Offset}}, nil
}

func (c *Connector) TestConnection(ctx context.Context) error {
	token, err := c.getToken(ctx)
	if err != nil {
		return err
	}
	_, err = c.get(ctx, c.BaseURL+"/about?fields=user", token, connector.Connection)
	return err
}

func (c *Connector) GenerateSyntheticRows(_ []models.DatasetField, count int64) ([]map[string]any, bool) {
	return generateRows(count), true
}

func escapeDriveQuery(s string) string {
	return strings.ReplaceAll(strings.ReplaceAll(s, `\`, `\\`), `'`, `\'`)
}

func strOr(p *string) string {
	if p == nil {
		return ""
	}
	return *p
}

func boolOr(p *bool) bool { return p != nil && *p }

func fileToJSON(f *driveFile) map[string]any {
	owner := ""
	if len(f.Owners) > 0 {
		owner = strOr(f.Owners[0].EmailAddress)
	}
	return map[string]any{
		"id":            strOr(f.ID),
		"name":          strOr(f.Name),
		"mime_type":     strOr(f.MimeType),
		"created_time":  strOr(f.CreatedTime),
		"modified_time": strOr(f.ModifiedTime),
		"size":          strOr(f.Size),
		"web_view_link": strOr(f.WebViewLink),
		"owner":         owner,
		"shared":        boolOr(f.Shared),
		"starred":       boolOr(f.Starred),
		"trashed":       boolOr(f.Trashed),
	}
}
