// Package ai holds Claude-backed helpers. Currently just DraftDescription:
// given a connector type, schema and sample row, it returns a short
// description and coarse data_kind tag for the dataset form.
package ai

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/ipedrazas/sbx/internal/util"
)

// Overridable in tests.
var (
	anthropicAPI = "https://api.anthropic.com/v1/messages"
	httpClient   = &http.Client{}
)

const anthropicVersion = "2023-06-01"

// ErrMissingAPIKey is returned when no Anthropic API key is configured.
var ErrMissingAPIKey = errors.New("no Anthropic API key configured — set `anthropic_api_key` in ~/.dstudio/config.toml")

// HTTPError wraps a transport-level failure talking to Anthropic.
type HTTPError struct{ Err error }

func (e *HTTPError) Error() string { return fmt.Sprintf("HTTP error calling Anthropic: %v", e.Err) }
func (e *HTTPError) Unwrap() error { return e.Err }

// APIError is a non-2xx response from Anthropic.
type APIError struct {
	Status int
	Body   string
}

func (e *APIError) Error() string { return fmt.Sprintf("Anthropic returned %d: %s", e.Status, e.Body) }

// ParseError means Claude's reply could not be parsed.
type ParseError struct {
	Reason string
	Raw    string
}

func (e *ParseError) Error() string {
	return fmt.Sprintf("could not parse Claude's reply as JSON (%s): %s", e.Reason, e.Raw)
}

type DraftDescriptionInput struct {
	ConnectorType string        `json:"connector_type"`
	DatasetName   *string       `json:"dataset_name"`
	Schema        []SchemaField `json:"schema"`
	SampleRow     any           `json:"sample_row"`
}

type SchemaField struct {
	Name        string  `json:"name"`
	FieldType   string  `json:"field_type"`
	Description *string `json:"description"`
}

type Draft struct {
	Description string `json:"description"`
	DataKind    string `json:"data_kind"`
}

func prettyJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(v); err != nil {
		return "{}"
	}
	return strings.TrimRight(buf.String(), "\n")
}

func buildPrompt(input *DraftDescriptionInput) string {
	lines := make([]string, 0, len(input.Schema))
	for _, f := range input.Schema {
		desc := ""
		if f.Description != nil {
			desc = *f.Description
		}
		lines = append(lines, fmt.Sprintf("- %s (%s): %s", f.Name, f.FieldType, desc))
	}
	sample := "(no sample available)"
	if input.SampleRow != nil {
		sample = prettyJSON(input.SampleRow)
	}
	name := "(none)"
	if input.DatasetName != nil {
		name = *input.DatasetName
	}
	return fmt.Sprintf("Draft metadata for a DStudio dataset. DStudio exposes read-only, pre-filtered "+
		"slices of data to AI agents; the `description` and `data_kind` you return will be "+
		"embedded in the agent's SKILL.md so agents can pick the right dataset.\n\n"+
		"Connector type: %s\n"+
		"Dataset name hint: %s\n\n"+
		"Schema:\n%s\n\n"+
		"Sample row:\n%s\n\n"+
		"Return ONLY a single JSON object, no markdown fencing, with these keys:\n"+
		"- `description`: 1-2 sentences, plain text, written for another AI agent. "+
		"Describe what the dataset contains and when an agent would reach for it. "+
		"Do not speculate about data that isn't in the schema.\n"+
		"- `data_kind`: one of `records`, `messages`, `events`, `tasks`, `items`, `documents`.\n",
		input.ConnectorType, name, strings.Join(lines, "\n"), sample)
}

// DraftDescription asks Claude to draft {description, data_kind} for a
// dataset. An empty apiKey yields ErrMissingAPIKey.
func DraftDescription(ctx context.Context, apiKey, model string, input *DraftDescriptionInput) (*Draft, error) {
	if apiKey == "" {
		return nil, ErrMissingAPIKey
	}
	body := map[string]any{
		"model":      model,
		"max_tokens": 400,
		"system": "You draft concise, factual metadata for datasets exposed to AI agents. " +
			"Reply with a single JSON object and nothing else.",
		"messages": []any{map[string]any{"role": "user", "content": buildPrompt(input)}},
	}
	payload, err := util.MarshalJSON(body)
	if err != nil {
		return nil, &HTTPError{Err: err}
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, anthropicAPI, bytes.NewReader(payload))
	if err != nil {
		return nil, &HTTPError{Err: err}
	}
	req.Header.Set("x-api-key", apiKey)
	req.Header.Set("anthropic-version", anthropicVersion)
	req.Header.Set("content-type", "application/json")

	resp, err := httpClient.Do(req)
	if err != nil {
		return nil, &HTTPError{Err: err}
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, &HTTPError{Err: err}
	}
	text := string(raw)
	if resp.StatusCode < 200 || resp.StatusCode > 299 {
		return nil, &APIError{Status: resp.StatusCode, Body: text}
	}

	var envelope struct {
		Content []map[string]any `json:"content"`
	}
	if err := json.Unmarshal(raw, &envelope); err != nil {
		return nil, &ParseError{Reason: err.Error(), Raw: text}
	}
	reply, found := "", false
	for _, b := range envelope.Content {
		if s, ok := b["text"].(string); ok {
			reply, found = s, true
			break
		}
	}
	if !found {
		return nil, &ParseError{Reason: "missing content[].text in response", Raw: text}
	}

	// Claude sometimes wraps JSON in a ```json fence even when asked not to.
	trimmed := strings.TrimSpace(reply)
	trimmed = trimPrefixAll(trimmed, "```json")
	trimmed = trimPrefixAll(trimmed, "```")
	trimmed = trimSuffixAll(trimmed, "```")
	trimmed = strings.TrimSpace(trimmed)

	var d struct {
		Description *string `json:"description"`
		DataKind    *string `json:"data_kind"`
	}
	if err := json.Unmarshal([]byte(trimmed), &d); err != nil {
		return nil, &ParseError{Reason: err.Error(), Raw: reply}
	}
	if d.Description == nil {
		return nil, &ParseError{Reason: "missing field `description`", Raw: reply}
	}
	if d.DataKind == nil {
		return nil, &ParseError{Reason: "missing field `data_kind`", Raw: reply}
	}
	return &Draft{Description: *d.Description, DataKind: *d.DataKind}, nil
}

func trimPrefixAll(s, p string) string {
	for strings.HasPrefix(s, p) {
		s = s[len(p):]
	}
	return s
}

func trimSuffixAll(s, p string) string {
	for strings.HasSuffix(s, p) {
		s = s[:len(s)-len(p)]
	}
	return s
}
