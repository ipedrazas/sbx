// Package gmail implements the Gmail connector (read messages, create drafts).
package gmail

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/oauth"
	"github.com/ipedrazas/sbx/internal/util"
)

// APIBase is the real Gmail API base URL.
const APIBase = "https://gmail.googleapis.com/gmail/v1/users/me"

// detailConcurrency bounds concurrent messages.get calls (well under Gmail's
// per-user rate limit).
const detailConcurrency = 10

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

// do sends a request and returns the body on 2xx. Transport errors are mapped
// via transportErr; non-2xx via FromUpstreamHTTP.
func (c *Connector) do(ctx context.Context, method, url, token string, body any, transportErr func(string, ...any) *connector.Error) ([]byte, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return nil, connector.Query("%s", err.Error())
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, url, rdr)
	if err != nil {
		return nil, transportErr("%s", err.Error())
	}
	req.Header.Set("Authorization", "Bearer "+token)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Client.Do(req)
	if err != nil {
		return nil, transportErr("%s", err.Error())
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, connector.FromUpstreamHTTP(resp.StatusCode, string(data), "Gmail API")
	}
	if err != nil {
		return nil, connector.Query("%s", err.Error())
	}
	return data, nil
}

type messageListResponse struct {
	Messages []struct {
		ID string `json:"id"`
	} `json:"messages"`
	NextPageToken      *string `json:"nextPageToken"`
	ResultSizeEstimate *int64  `json:"resultSizeEstimate"`
}

type gmailMessage struct {
	ID       string   `json:"id"`
	ThreadID string   `json:"threadId"`
	LabelIDs []string `json:"labelIds"`
	Snippet  *string  `json:"snippet"`
	Payload  *struct {
		Headers []struct {
			Name  string `json:"name"`
			Value string `json:"value"`
		} `json:"headers"`
	} `json:"payload"`
}

func (c *Connector) fetchMessages(ctx context.Context, query *string, maxResults int64, pageToken *string) (*messageListResponse, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}
	url := fmt.Sprintf("%s/messages?maxResults=%d", c.BaseURL, maxResults)
	if query != nil {
		url += "&q=" + util.URLEncode(*query)
	}
	if pageToken != nil {
		url += "&pageToken=" + *pageToken
	}
	data, err := c.do(ctx, http.MethodGet, url, token, nil, connector.Query)
	if err != nil {
		return nil, err
	}
	var out messageListResponse
	if err := json.Unmarshal(data, &out); err != nil {
		return nil, connector.Query("%s", err.Error())
	}
	return &out, nil
}

func (c *Connector) fetchMessageDetail(ctx context.Context, id string) (map[string]any, error) {
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}
	return c.fetchMessageDetailWithToken(ctx, id, token)
}

// fetchMessageDetailWithToken reuses a pre-fetched token so concurrent
// fan-out doesn't run the token refresh path once per message.
func (c *Connector) fetchMessageDetailWithToken(ctx context.Context, id, token string) (map[string]any, error) {
	url := fmt.Sprintf("%s/messages/%s?format=metadata&metadataHeaders=From&metadataHeaders=To&metadataHeaders=Subject&metadataHeaders=Date", c.BaseURL, id)
	data, err := c.do(ctx, http.MethodGet, url, token, nil, connector.Query)
	if err != nil {
		return nil, err
	}
	var msg gmailMessage
	if err := json.Unmarshal(data, &msg); err != nil {
		return nil, connector.Query("%s", err.Error())
	}
	return messageToJSON(&msg), nil
}

func (c *Connector) Introspect(context.Context) (*connector.Schema, error) {
	f := func(name string, t connector.FieldType, desc string) connector.SchemaField {
		return connector.SchemaField{Name: name, FieldType: t, Description: models.Ptr(desc)}
	}
	return &connector.Schema{Fields: []connector.SchemaField{
		f("id", connector.FieldString, "Gmail message ID"),
		f("thread_id", connector.FieldString, "Gmail thread ID"),
		f("from", connector.FieldString, "Sender email address"),
		f("to", connector.FieldString, "Recipient email address(es)"),
		f("subject", connector.FieldString, "Email subject line"),
		f("date", connector.FieldDateTime, "Date the email was sent"),
		f("snippet", connector.FieldString, "Short preview of the email body"),
		f("label_ids", connector.FieldJSON, "Gmail label IDs"),
	}}, nil
}

func (c *Connector) Query(ctx context.Context, params *connector.QueryParams) (*models.Envelope, error) {
	// Push from/to/subject filters into a Gmail search query.
	// Gmail OR syntax: {a b} means OR; space means AND.
	var parts []string
	var inMemory []models.DatasetFilter
	hasOr := false
	for _, f := range params.Filters {
		var part string
		switch {
		case f.FieldName == "from" && (f.Operator == models.OpEquals || f.Operator == models.OpContains):
			part = "from:" + util.ValueAsString(f.Value)
		case f.FieldName == "to" && (f.Operator == models.OpEquals || f.Operator == models.OpContains):
			part = "to:" + util.ValueAsString(f.Value)
		case f.FieldName == "subject" && f.Operator == models.OpContains:
			part = "subject:" + util.ValueAsString(f.Value)
		default:
			inMemory = append(inMemory, f)
			continue
		}
		parts = append(parts, part)
		if f.Logic == models.LogicOr {
			hasOr = true
		}
	}

	var queryParts []string
	if len(parts) > 0 {
		if hasOr && len(parts) > 1 {
			queryParts = append(queryParts, "{"+strings.Join(parts, " ")+"}")
		} else {
			queryParts = append(queryParts, parts...)
		}
	}
	if params.Search != nil {
		queryParts = append(queryParts, *params.Search)
	}
	var gmailQuery *string
	if len(queryParts) > 0 {
		gmailQuery = models.Ptr(strings.Join(queryParts, " "))
	}

	if params.ItemID != nil {
		item, err := c.fetchMessageDetail(ctx, *params.ItemID)
		if err != nil {
			return nil, err
		}
		return &models.Envelope{Items: []map[string]any{item}, Meta: models.EnvelopeMeta{Total: 1, Limit: 1}}, nil
	}

	list, err := c.fetchMessages(ctx, gmailQuery, params.Limit, nil)
	if err != nil {
		return nil, err
	}

	// The list call only returns IDs; fan out detail fetches with bounded
	// concurrency, preserving list order.
	items := []map[string]any{}
	if list.Messages != nil {
		token, err := c.getToken(ctx)
		if err != nil {
			return nil, err
		}
		results := make([]map[string]any, len(list.Messages))
		errs := make([]error, len(list.Messages))
		sem := make(chan struct{}, detailConcurrency)
		var wg sync.WaitGroup
		for i, m := range list.Messages {
			wg.Add(1)
			sem <- struct{}{}
			go func() {
				defer wg.Done()
				defer func() { <-sem }()
				results[i], errs[i] = c.fetchMessageDetailWithToken(ctx, m.ID, token)
			}()
		}
		wg.Wait()
		for _, e := range errs {
			if e != nil {
				return nil, e
			}
		}
		items = results
	}

	items = connector.ApplyFiltersInMemory(items, inMemory)
	total := int64(len(items))
	if list.ResultSizeEstimate != nil {
		total = *list.ResultSizeEstimate
	}
	return &models.Envelope{Items: items, Meta: models.EnvelopeMeta{Total: total, Limit: params.Limit, Offset: params.Offset}}, nil
}

func (c *Connector) TestConnection(ctx context.Context) error {
	token, err := c.getToken(ctx)
	if err != nil {
		return err
	}
	_, err = c.do(ctx, http.MethodGet, c.BaseURL+"/profile", token, nil, connector.Connection)
	return err
}

// SimulateAction validates params with the same rules as the real path, then
// returns deterministic fake IDs derived from the params hash.
func (c *Connector) SimulateAction(_ context.Context, actionType string, params map[string]any) (any, error) {
	if actionType != "create_draft" {
		return nil, connector.Unsupported("Gmail does not simulate action '%s'", actionType)
	}
	if _, err := parseCreateDraft(params); err != nil {
		return nil, err
	}
	suffix := util.HashCanonicalJSON(params)
	return map[string]any{
		"draft_id":   "r-SIM-" + suffix[:8],
		"message_id": suffix[:16] + "@sim.gmail",
		"thread_id":  suffix[:16] + "@sim.gmail",
	}, nil
}

func (c *Connector) InvokeAction(ctx context.Context, actionType string, params map[string]any) (any, error) {
	if actionType != "create_draft" {
		return nil, connector.Unsupported("Gmail does not support action '%s'", actionType)
	}
	req, err := parseCreateDraft(params)
	if err != nil {
		return nil, err
	}
	encoded := base64.RawURLEncoding.EncodeToString([]byte(buildRFC2822(req)))
	token, err := c.getToken(ctx)
	if err != nil {
		return nil, err
	}
	body := map[string]any{"message": map[string]any{"raw": encoded}}
	data, err := c.do(ctx, http.MethodPost, c.BaseURL+"/drafts", token, body, connector.Query)
	if err != nil {
		return nil, err
	}
	var draft struct {
		ID      *string `json:"id"`
		Message *struct {
			ID       string `json:"id"`
			ThreadID string `json:"threadId"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &draft); err != nil {
		return nil, connector.Query("%s", err.Error())
	}
	if draft.ID == nil {
		return nil, connector.Query("missing field `id`")
	}
	var msgID, threadID any
	if draft.Message != nil {
		msgID, threadID = draft.Message.ID, draft.Message.ThreadID
	}
	return map[string]any{"draft_id": *draft.ID, "message_id": msgID, "thread_id": threadID}, nil
}

func (c *Connector) GenerateSyntheticRows(_ []models.DatasetField, count int64) ([]map[string]any, bool) {
	return generateRows(count), true
}

type createDraftParams struct {
	To, Subject, Body string
	Cc, Bcc, From     *string // From overrides the sender; Gmail defaults to the authenticated user.
}

// parseCreateDraft mirrors serde's validation of CreateDraftParams.
func parseCreateDraft(params map[string]any) (*createDraftParams, error) {
	bad := func(format string, a ...any) error {
		return connector.Query("invalid create_draft params: "+format, a...)
	}
	req := &createDraftParams{}
	for _, f := range []struct {
		name string
		dst  *string
	}{{"to", &req.To}, {"subject", &req.Subject}, {"body", &req.Body}} {
		v, ok := params[f.name]
		if !ok {
			return nil, bad("missing field `%s`", f.name)
		}
		s, ok := v.(string)
		if !ok {
			return nil, bad("invalid type: %s, expected a string", util.JSONString(v))
		}
		*f.dst = s
	}
	for _, f := range []struct {
		name string
		dst  **string
	}{{"cc", &req.Cc}, {"bcc", &req.Bcc}, {"from", &req.From}} {
		v, ok := params[f.name]
		if !ok || v == nil {
			continue
		}
		s, ok := v.(string)
		if !ok {
			return nil, bad("invalid type: %s, expected a string", util.JSONString(v))
		}
		*f.dst = &s
	}
	return req, nil
}

// buildRFC2822 renders a plain-text message. Headers are raw UTF-8 since the
// whole message is sent base64url-encoded.
func buildRFC2822(p *createDraftParams) string {
	var b strings.Builder
	if p.From != nil {
		fmt.Fprintf(&b, "From: %s\r\n", *p.From)
	}
	fmt.Fprintf(&b, "To: %s\r\n", p.To)
	if p.Cc != nil {
		fmt.Fprintf(&b, "Cc: %s\r\n", *p.Cc)
	}
	if p.Bcc != nil {
		fmt.Fprintf(&b, "Bcc: %s\r\n", *p.Bcc)
	}
	fmt.Fprintf(&b, "Subject: %s\r\n", p.Subject)
	b.WriteString("MIME-Version: 1.0\r\n")
	b.WriteString("Content-Type: text/plain; charset=\"UTF-8\"\r\n")
	b.WriteString("\r\n")
	b.WriteString(p.Body)
	return b.String()
}

func messageToJSON(msg *gmailMessage) map[string]any {
	header := func(name string) string {
		if msg.Payload == nil {
			return ""
		}
		for _, h := range msg.Payload.Headers {
			if strings.EqualFold(h.Name, name) {
				return h.Value
			}
		}
		return ""
	}
	snippet := ""
	if msg.Snippet != nil {
		snippet = *msg.Snippet
	}
	labels := make([]any, 0, len(msg.LabelIDs))
	for _, l := range msg.LabelIDs {
		labels = append(labels, l)
	}
	return map[string]any{
		"id":        msg.ID,
		"thread_id": msg.ThreadID,
		"from":      header("From"),
		"to":        header("To"),
		"subject":   header("Subject"),
		"date":      header("Date"),
		"snippet":   snippet,
		"label_ids": labels,
	}
}
