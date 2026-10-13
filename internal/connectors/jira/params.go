package jira

import (
	"encoding/json"
	"fmt"
	"strings"
)

// createIssueParams is the create_issue action input.
type createIssueParams struct {
	ProjectKey string
	Summary    string
	// Jira issue type name; defaults to "Task".
	IssueType   string
	Description *string
	Priority    *string
	Labels      []string // nil = absent
	// Atlassian accountId (Jira Cloud no longer allows assigning by username).
	AssigneeAccountID *string
}

// serdeDesc renders a JSON value the way serde's "invalid type" errors do.
func serdeDesc(v any) string {
	switch t := v.(type) {
	case nil:
		return "null"
	case bool:
		return fmt.Sprintf("boolean `%t`", t)
	case json.Number:
		if strings.ContainsAny(t.String(), ".eE") {
			return fmt.Sprintf("floating point `%s`", t)
		}
		return fmt.Sprintf("integer `%s`", t)
	case float64:
		if t == float64(int64(t)) {
			return fmt.Sprintf("integer `%d`", int64(t))
		}
		return fmt.Sprintf("floating point `%v`", t)
	case string:
		return fmt.Sprintf("string %q", t)
	case []any:
		return "sequence"
	case map[string]any:
		return "map"
	}
	return fmt.Sprintf("%v", v)
}

func reqString(m map[string]any, key string) (string, error) {
	v, ok := m[key]
	if !ok {
		return "", fmt.Errorf("missing field `%s`", key)
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("invalid type: %s, expected a string", serdeDesc(v))
	}
	return s, nil
}

func optString(m map[string]any, key string) (*string, error) {
	v, ok := m[key]
	if !ok || v == nil {
		return nil, nil
	}
	s, ok := v.(string)
	if !ok {
		return nil, fmt.Errorf("invalid type: %s, expected a string", serdeDesc(v))
	}
	return &s, nil
}

// parseCreateIssueParams mirrors serde deserialization of CreateIssueParams.
func parseCreateIssueParams(v any) (*createIssueParams, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("invalid type: %s, expected struct CreateIssueParams", serdeDesc(v))
	}
	var p createIssueParams
	var err error
	if p.ProjectKey, err = reqString(m, "project_key"); err != nil {
		return nil, err
	}
	if p.Summary, err = reqString(m, "summary"); err != nil {
		return nil, err
	}
	p.IssueType = "Task"
	if _, present := m["issue_type"]; present {
		if p.IssueType, err = reqString(m, "issue_type"); err != nil {
			return nil, err
		}
	}
	if p.Description, err = optString(m, "description"); err != nil {
		return nil, err
	}
	if p.Priority, err = optString(m, "priority"); err != nil {
		return nil, err
	}
	if lv, present := m["labels"]; present && lv != nil {
		arr, ok := lv.([]any)
		if !ok {
			return nil, fmt.Errorf("invalid type: %s, expected a sequence", serdeDesc(lv))
		}
		p.Labels = make([]string, 0, len(arr))
		for _, e := range arr {
			s, ok := e.(string)
			if !ok {
				return nil, fmt.Errorf("invalid type: %s, expected a string", serdeDesc(e))
			}
			p.Labels = append(p.Labels, s)
		}
	}
	if p.AssigneeAccountID, err = optString(m, "assignee_account_id"); err != nil {
		return nil, err
	}
	return &p, nil
}

// buildCreateIssueBody builds the POST /issue body. The description is wrapped
// in a minimal ADF document because Jira Cloud v3 rejects plain text.
func buildCreateIssueBody(p *createIssueParams) map[string]any {
	fields := map[string]any{
		"project":   map[string]any{"key": p.ProjectKey},
		"summary":   p.Summary,
		"issuetype": map[string]any{"name": p.IssueType},
	}
	if p.Description != nil {
		fields["description"] = map[string]any{
			"type":    "doc",
			"version": 1,
			"content": []any{map[string]any{
				"type":    "paragraph",
				"content": []any{map[string]any{"type": "text", "text": *p.Description}},
			}},
		}
	}
	if p.Priority != nil {
		fields["priority"] = map[string]any{"name": *p.Priority}
	}
	if p.Labels != nil {
		fields["labels"] = strSlice(p.Labels)
	}
	if p.AssigneeAccountID != nil {
		fields["assignee"] = map[string]any{"accountId": *p.AssigneeAccountID}
	}
	return map[string]any{"fields": fields}
}
