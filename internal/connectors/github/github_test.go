package github

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ipedrazas/sbx/internal/connector"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/oauth"
	"github.com/ipedrazas/sbx/internal/secretstore"
)

func kindOf(t *testing.T, err error) connector.ErrorKind {
	t.Helper()
	var ce *connector.Error
	if !errors.As(err, &ce) {
		t.Fatalf("not a connector error: %v", err)
	}
	return ce.Kind
}

func setup(t *testing.T, h http.HandlerFunc) {
	t.Helper()
	secretstore.Init(secretstore.NewMemory())
	if err := oauth.StoreTokens(1, &oauth.Tokens{AccessToken: "tok"}); err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(h)
	old := apiBase
	apiBase = srv.URL
	t.Cleanup(func() { apiBase = old; srv.Close() })
}

func conn(repo *string, rt ResourceType, branch *string) *Connector {
	return New(1, oauth.Credentials{ClientID: "x"}, repo, rt, branch)
}

func TestParseResourceType(t *testing.T) {
	if ParseResourceType("repo_files") != RepoFiles || ParseResourceType("issues") != Issues || ParseResourceType("") != Issues {
		t.Fatal("bad parse")
	}
}

func TestIntrospect(t *testing.T) {
	s, _ := conn(nil, Issues, nil).Introspect(context.Background())
	if len(s.Fields) != 13 || s.Fields[0].Name != "id" || s.Fields[8].FieldType != connector.FieldBoolean {
		t.Fatalf("%+v", s)
	}
	s, _ = conn(nil, RepoFiles, nil).Introspect(context.Background())
	if len(s.Fields) != 7 || s.Fields[0].Name != "path" || s.Fields[3].FieldType != connector.FieldInteger {
		t.Fatalf("%+v", s)
	}
}

func TestDefaultsUnsupported(t *testing.T) {
	c := conn(nil, Issues, nil)
	if _, err := c.InvokeAction(context.Background(), "x", nil); kindOf(t, err) != connector.ErrUnsupported {
		t.Fatal(err)
	}
	if _, ok := c.GenerateSyntheticRows(nil, 3); ok {
		t.Fatal("expected fallback")
	}
	if c.SortCapability().Kind != connector.SortNone {
		t.Fatal("expected no sort")
	}
}

func TestQueryIssuesRepoListing(t *testing.T) {
	setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer tok" || r.Header.Get("Accept") != githubAccept ||
			r.Header.Get("X-GitHub-Api-Version") != githubAPIVersion || r.Header.Get("User-Agent") != "dstudio" {
			t.Errorf("bad headers %v", r.Header)
		}
		if r.URL.Path != "/repos/o/r/issues" || r.URL.RawQuery != "state=open&per_page=10&page=3" {
			t.Errorf("bad url %s", r.URL)
		}
		io.WriteString(w, `[{"id":12345678901,"number":7,"title":"T","state":"open",
			"repository_url":"https://api.github.com/repos/o/r","user":{"login":"u"},"assignee":{"login":"a"},
			"labels":[{"name":"bug"},{"color":"x"}],"pull_request":null,"created_at":"c","updated_at":"u2",
			"html_url":"h","body":"b"},{"id":2,"number":8,"repository":{"full_name":"x/y"},"assignee":null}]`)
	})
	p := connector.DefaultQueryParams()
	p.Limit, p.Offset = 10, 25
	env, err := conn(models.Ptr("o/r"), Issues, nil).Query(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	if env.Meta.Total != 27 || env.Meta.Limit != 10 || env.Meta.Offset != 25 || len(env.Items) != 2 {
		t.Fatalf("%+v", env.Meta)
	}
	it := env.Items[0]
	if it["id"].(json.Number).String() != "12345678901" || it["repository"] != "o/r" || it["user"] != "u" ||
		it["assignee"] != "a" || it["is_pull_request"] != true || len(it["labels"].([]any)) != 1 || it["body"] != "b" {
		t.Fatalf("%v", it)
	}
	it = env.Items[1]
	if it["repository"] != "x/y" || it["assignee"] != "" || it["is_pull_request"] != false || it["title"] != "" {
		t.Fatalf("%v", it)
	}
}

func TestQueryIssuesSearchAndAssigned(t *testing.T) {
	var gotQ, gotPath string
	setup(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotQ = r.URL.Query().Get("q")
		if r.URL.Path == "/issues" {
			if r.URL.RawQuery != "filter=assigned&state=open&per_page=50&page=1" {
				t.Errorf("bad query %s", r.URL.RawQuery)
			}
			io.WriteString(w, `[]`)
			return
		}
		io.WriteString(w, `{"total_count":99,"items":[{"id":1,"title":"x"},{"id":2,"title":"y"}]}`)
	})
	p := connector.DefaultQueryParams()
	p.Search = models.Ptr("crash")
	p.Filters = []models.DatasetFilter{{FieldName: "title", Operator: models.OpEquals, Value: "y", Logic: models.LogicAnd}}
	env, err := conn(models.Ptr("o/r"), Issues, nil).Query(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	if gotPath != "/search/issues" || gotQ != "crash repo:o/r is:issue" || env.Meta.Total != 99 || len(env.Items) != 1 {
		t.Fatalf("%s %q %+v", gotPath, gotQ, env)
	}
	_, err = conn(nil, Issues, nil).Query(context.Background(), &p)
	if err != nil || gotQ != "crash is:open assignee:@me" {
		t.Fatal(gotQ, err)
	}
	p = connector.DefaultQueryParams()
	env, err = conn(nil, Issues, nil).Query(context.Background(), &p)
	if err != nil || gotPath != "/issues" || env.Meta.Total != 0 || env.Items == nil {
		t.Fatal(err, env)
	}
}

func TestQueryIssuesErrors(t *testing.T) {
	body, status := `{"message":"x"}`, 200
	setup(t, func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(status); io.WriteString(w, body) })
	c := conn(models.Ptr("o/r"), Issues, nil)
	p := connector.DefaultQueryParams()
	_, err := c.Query(context.Background(), &p)
	if err == nil || !strings.Contains(err.Error(), `unexpected GitHub response shape — top-level keys: Some(["message"])`) {
		t.Fatal(err)
	}
	body = `nope`
	_, err = c.Query(context.Background(), &p)
	if err == nil || !strings.Contains(err.Error(), "invalid JSON from GitHub") {
		t.Fatal(err)
	}
	status = http.StatusUnauthorized
	_, err = c.Query(context.Background(), &p)
	if kindOf(t, err) != connector.ErrAuth || !strings.Contains(err.Error(), "GitHub API") {
		t.Fatal(err)
	}
	status = http.StatusInternalServerError
	_, err = c.Query(context.Background(), &p)
	if kindOf(t, err) != connector.ErrUpstreamHTTP {
		t.Fatal(err)
	}
}

func TestRepoFilesRequiresRepo(t *testing.T) {
	setup(t, func(w http.ResponseWriter, r *http.Request) {})
	p := connector.DefaultQueryParams()
	_, err := conn(nil, RepoFiles, nil).Query(context.Background(), &p)
	if kindOf(t, err) != connector.ErrConfig {
		t.Fatal(err)
	}
}

func TestListTreeResolvesDefaultBranchAndPaginates(t *testing.T) {
	setup(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r":
			io.WriteString(w, `{"default_branch":"main"}`)
		case "/repos/o/r/git/trees/main":
			if r.URL.RawQuery != "recursive=1" {
				t.Errorf("bad query %s", r.URL.RawQuery)
			}
			io.WriteString(w, `{"tree":[{"path":"README.md","type":"blob","size":10,"sha":"a"},
				{"path":"src","type":"tree","sha":"b"},{"path":"src/main.go","size":5,"sha":"c"}]}`)
		default:
			http.NotFound(w, r)
		}
	})
	p := connector.DefaultQueryParams()
	p.Limit, p.Offset = 1, 2
	env, err := conn(models.Ptr("o/r"), RepoFiles, nil).Query(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	if env.Meta.Total != 3 || env.Meta.Limit != 1 || env.Meta.Offset != 2 || len(env.Items) != 1 {
		t.Fatalf("%+v", env)
	}
	it := env.Items[0]
	if it["path"] != "src/main.go" || it["name"] != "main.go" || it["type"] != "blob" || it["size"] != int64(5) || it["content"] != "" {
		t.Fatalf("%v", it)
	}
	p.Offset, p.Limit = 50, 500
	env, err = conn(models.Ptr("o/r"), RepoFiles, nil).Query(context.Background(), &p)
	if err != nil || env.Meta.Offset != 3 || env.Meta.Limit != 100 || len(env.Items) != 0 {
		t.Fatal(err, env)
	}
	p.Offset = -1
	env, _ = conn(models.Ptr("o/r"), RepoFiles, nil).Query(context.Background(), &p)
	if len(env.Items) != 0 {
		t.Fatal(env)
	}
}

func TestListTreeErrors(t *testing.T) {
	body := `{"nobranch":1}`
	setup(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, body) })
	p := connector.DefaultQueryParams()
	_, err := conn(models.Ptr("o/r"), RepoFiles, nil).Query(context.Background(), &p)
	if err == nil || !strings.Contains(err.Error(), "could not determine default branch") {
		t.Fatal(err)
	}
	_, err = conn(models.Ptr("o/r"), RepoFiles, models.Ptr("dev")).Query(context.Background(), &p)
	if err == nil || !strings.Contains(err.Error(), "missing tree array in response") {
		t.Fatal(err)
	}
}

func TestGetFileByPath(t *testing.T) {
	content := base64.StdEncoding.EncodeToString([]byte("hello\nworld"))
	setup(t, func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/repos/o/r/contents/docs/a.md":
			if r.URL.RawQuery != "ref=feature%2Fx" {
				t.Errorf("bad ref %s", r.URL.RawQuery)
			}
			io.WriteString(w, `{"path":"docs/a.md","name":"a.md","type":"file","size":11,"sha":"s",
				"content":"`+content[:8]+`\n`+content[8:]+`","download_url":"d"}`)
		case "/repos/o/r/contents/docs":
			io.WriteString(w, `[{"path":"docs/a.md","name":"a.md"},{"path":"docs/b","name":"b","type":"dir"}]`)
		}
	})
	p := connector.DefaultQueryParams()
	p.ItemID = models.Ptr("docs/a.md")
	env, err := conn(models.Ptr("o/r"), RepoFiles, models.Ptr("feature/x")).Query(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	if env.Meta.Total != 1 || env.Meta.Limit != 1 || env.Items[0]["content"] != "hello\nworld" || env.Items[0]["download_url"] != "d" {
		t.Fatalf("%+v", env)
	}
	p.ItemID = models.Ptr("docs")
	env, err = conn(models.Ptr("o/r"), RepoFiles, models.Ptr("feature/x")).Query(context.Background(), &p)
	if err != nil || env.Meta.Total != 2 || env.Items[0]["type"] != "file" || env.Items[1]["type"] != "dir" {
		t.Fatal(err, env)
	}
}

func TestDecodeContent(t *testing.T) {
	big := make([]byte, maxFileContentBytes+10)
	for i := range big {
		big[i] = 'a'
	}
	got := decodeContent(map[string]any{"content": base64.StdEncoding.EncodeToString(big)})
	if !strings.HasSuffix(got, "\n\n[truncated at 524288 bytes]") || len(got) != maxFileContentBytes+len("\n\n[truncated at 524288 bytes]") {
		t.Fatal(len(got))
	}
	if decodeContent(map[string]any{"content": base64.StdEncoding.EncodeToString([]byte{0xff, 0xfe})}) != "" {
		t.Fatal("invalid utf8 should be empty")
	}
	if decodeContent(map[string]any{"content": "!!!"}) != "" || decodeContent(map[string]any{}) != "" {
		t.Fatal("bad base64 should be empty")
	}
	if lossyUTF8([]byte{'a', 0xff, 'b'}) != "a�b" {
		t.Fatal("lossy")
	}
}

func TestSearchCode(t *testing.T) {
	setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/search/code" || r.Header.Get("Accept") != "application/vnd.github.text-match+json" {
			http.NotFound(w, r)
			return
		}
		if r.URL.Query().Get("q") != "fn main repo:o/r" || r.URL.Query().Get("per_page") != "20" || r.URL.Query().Get("page") != "3" {
			t.Errorf("bad query %s", r.URL.RawQuery)
		}
		io.WriteString(w, `{"total_count":5,"items":[{"path":"a/b.rs","name":"b.rs","sha":"s","html_url":"h",
			"text_matches":[{"fragment":"one"},{"x":1},{"fragment":"two"}]}]}`)
	})
	p := connector.DefaultQueryParams()
	p.Search, p.Limit, p.Offset = models.Ptr("fn main"), 20, 45
	env, err := conn(models.Ptr("o/r"), RepoFiles, nil).Query(context.Background(), &p)
	if err != nil {
		t.Fatal(err)
	}
	if env.Meta.Total != 5 || env.Meta.Limit != 20 || env.Meta.Offset != 45 {
		t.Fatalf("%+v", env.Meta)
	}
	it := env.Items[0]
	if it["content"] != "one\n---\ntwo" || it["download_url"] != "h" || it["type"] != "blob" || it["size"] != int64(0) {
		t.Fatalf("%v", it)
	}
}

func TestTestConnection(t *testing.T) {
	body, status := `{"login":"me"}`, 200
	setup(t, func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/user" {
			http.NotFound(w, r)
			return
		}
		w.WriteHeader(status)
		io.WriteString(w, body)
	})
	c := conn(nil, Issues, nil)
	if err := c.TestConnection(context.Background()); err != nil {
		t.Fatal(err)
	}
	body = `{}`
	if err := c.TestConnection(context.Background()); kindOf(t, err) != connector.ErrConnection {
		t.Fatal(err)
	}
	body = `x`
	if err := c.TestConnection(context.Background()); kindOf(t, err) != connector.ErrConnection {
		t.Fatal(err)
	}
	status = http.StatusForbidden
	if err := c.TestConnection(context.Background()); kindOf(t, err) != connector.ErrAuth {
		t.Fatal(err)
	}
}

func TestMissingTokenIsAuth(t *testing.T) {
	secretstore.Init(secretstore.NewMemory())
	p := connector.DefaultQueryParams()
	for _, rt := range []ResourceType{Issues, RepoFiles} {
		if _, err := conn(models.Ptr("o/r"), rt, nil).Query(context.Background(), &p); kindOf(t, err) != connector.ErrAuth {
			t.Fatal(err)
		}
	}
	if err := conn(nil, Issues, nil).TestConnection(context.Background()); kindOf(t, err) != connector.ErrAuth {
		t.Fatal(err)
	}
}

func TestDebugKeys(t *testing.T) {
	if got := debugKeys(map[string]any{"b": 1, "a": 2}); got != `["a", "b"]` {
		t.Fatal(got)
	}
}
