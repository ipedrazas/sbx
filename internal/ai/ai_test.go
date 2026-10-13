package ai

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ipedrazas/sbx/internal/models"
)

func serve(t *testing.T, status int, body string, check func(*http.Request, map[string]any)) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		if check != nil {
			check(r, m)
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	}))
	t.Cleanup(srv.Close)
	old := anthropicAPI
	anthropicAPI = srv.URL
	t.Cleanup(func() { anthropicAPI = old })
}

func reply(text string) string {
	b, _ := json.Marshal(map[string]any{"content": []any{map[string]any{"type": "text", "text": text}}})
	return string(b)
}

func input() *DraftDescriptionInput {
	return &DraftDescriptionInput{
		ConnectorType: "gmail",
		DatasetName:   models.Ptr("Inbox"),
		Schema: []SchemaField{
			{Name: "subject", FieldType: "string", Description: models.Ptr("Email subject")},
			{Name: "from", FieldType: "string"},
		},
		SampleRow: map[string]any{"subject": "Hi <there>", "from": "a@b.com"},
	}
}

func TestDraftDescriptionSuccess(t *testing.T) {
	serve(t, 200, reply("```json\n{\"description\":\"Emails.\",\"data_kind\":\"messages\"}\n```"), func(r *http.Request, m map[string]any) {
		if r.Method != http.MethodPost || r.Header.Get("x-api-key") != "k" ||
			r.Header.Get("anthropic-version") != "2023-06-01" || r.Header.Get("content-type") != "application/json" {
			t.Errorf("bad request headers: %v", r.Header)
		}
		if m["model"] != "claude-x" || m["max_tokens"] != 400.0 || !strings.Contains(m["system"].(string), "single JSON object") {
			t.Errorf("bad body: %v", m)
		}
		prompt := m["messages"].([]any)[0].(map[string]any)["content"].(string)
		for _, want := range []string{
			"Connector type: gmail\n", "Dataset name hint: Inbox\n",
			"- subject (string): Email subject\n- from (string): \n",
			"Sample row:\n{\n  \"from\": \"a@b.com\",\n  \"subject\": \"Hi <there>\"\n}\n",
		} {
			if !strings.Contains(prompt, want) {
				t.Errorf("prompt missing %q:\n%s", want, prompt)
			}
		}
	})
	d, err := DraftDescription(context.Background(), "k", "claude-x", input())
	if err != nil {
		t.Fatal(err)
	}
	if d.Description != "Emails." || d.DataKind != "messages" {
		t.Fatalf("%+v", d)
	}
}

func TestDraftDescriptionNoSample(t *testing.T) {
	serve(t, 200, reply(`{"description":"d","data_kind":"records"}`), func(_ *http.Request, m map[string]any) {
		prompt := m["messages"].([]any)[0].(map[string]any)["content"].(string)
		if !strings.Contains(prompt, "(no sample available)") || !strings.Contains(prompt, "Dataset name hint: (none)") {
			t.Errorf("prompt:\n%s", prompt)
		}
	})
	in := &DraftDescriptionInput{ConnectorType: "rest"}
	if _, err := DraftDescription(context.Background(), "k", "m", in); err != nil {
		t.Fatal(err)
	}
}

func TestDraftDescriptionErrors(t *testing.T) {
	ctx := context.Background()
	if _, err := DraftDescription(ctx, "", "m", input()); !errors.Is(err, ErrMissingAPIKey) {
		t.Fatal(err)
	}

	serve(t, 401, `{"error":"nope"}`, nil)
	_, err := DraftDescription(ctx, "k", "m", input())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != 401 || apiErr.Body != `{"error":"nope"}` {
		t.Fatal(err)
	}
	if err.Error() != `Anthropic returned 401: {"error":"nope"}` {
		t.Fatal(err.Error())
	}

	var pe *ParseError
	for _, body := range []string{
		"not json",
		`{"content":[{"type":"tool_use"}]}`,
		reply("sorry, I can't"),
		reply(`{"description":"only"}`),
		reply(`{"data_kind":"only"}`),
	} {
		serve(t, 200, body, nil)
		if _, err := DraftDescription(ctx, "k", "m", input()); !errors.As(err, &pe) {
			t.Errorf("%s: %v", body, err)
		} else if !strings.HasPrefix(err.Error(), "could not parse Claude's reply as JSON (") {
			t.Error(err.Error())
		}
	}

	anthropicAPI = "http://127.0.0.1:1/unreachable"
	var he *HTTPError
	if _, err := DraftDescription(ctx, "k", "m", input()); !errors.As(err, &he) || !strings.HasPrefix(err.Error(), "HTTP error calling Anthropic: ") {
		t.Fatal(err)
	}
	if he.Unwrap() == nil {
		t.Fatal("unwrap")
	}
}

func TestTrimHelpers(t *testing.T) {
	if trimPrefixAll("``````x", "```") != "x" || trimSuffixAll("x``````", "```") != "x" {
		t.Fatal("trim")
	}
	if prettyJSON(func() {}) != "{}" {
		t.Fatal("pretty fallback")
	}
}
