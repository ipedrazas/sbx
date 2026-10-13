package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/oauth"
	"github.com/ipedrazas/sbx/internal/secretstore"
)

func TestOAuthNormalizeScopes(t *testing.T) {
	got := normalizeScopes([]string{" a ", "", "b", "a", "  "})
	if !reflect.DeepEqual(got, []string{"a", "b"}) {
		t.Fatal(got)
	}
	if got := normalizeScopes(nil); got == nil || len(got) != 0 {
		t.Fatal(got)
	}
}

func TestOAuthDefaultScopes(t *testing.T) {
	for _, ty := range []string{"gmail", "google_calendar", "google_drive", "jira", "linear", "github", "hubspot"} {
		if len(defaultScopesFor(models.DatasourceType(ty))) == 0 {
			t.Errorf("%s: no default scopes", ty)
		}
	}
	if len(defaultScopesFor("postgres")) != 0 {
		t.Fatal("postgres should have no scopes")
	}
}

func TestOAuthConnectionsCRUD(t *testing.T) {
	e := newTestEnv(t)
	rec := e.manage("GET", "/manage/oauth/connections", nil)
	if rec.Code != 200 || rec.Body.String() != `{"connections":[]}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	rec = e.manage("POST", "/manage/oauth/connections", map[string]any{
		"name": "g", "provider": "google", "client_id": "cid", "client_secret": "sec", "scopes": []string{" s1", "s1", "s2"}})
	if rec.Code != 201 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	id := int64(decode(t, rec)["id"].(float64))
	path := fmt.Sprintf("/manage/oauth/connections/%d", id)

	rec = e.manage("GET", path, nil)
	m := decode(t, rec)
	if rec.Code != 200 || m["client_id"] != "cid" || !reflect.DeepEqual(m["scopes"], []any{"s1", "s2"}) {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := e.manage("GET", "/manage/oauth/connections/999", nil); rec.Code != 404 || rec.Body.String() != `{"error":"not found"}` {
		t.Fatal(rec.Body.String())
	}

	upd := map[string]any{"name": "g2", "client_id": "cid2"}
	if rec := e.manage("PUT", path, upd); rec.Code != 200 || rec.Body.String() != `{"updated":true}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := e.manage("POST", path+"/update", upd); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if rec := e.manage("PUT", "/manage/oauth/connections/999", upd); rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	if rec := e.manage("PUT", path, map[string]any{"name": "x"}); rec.Code != 422 {
		t.Fatal(rec.Code)
	}
	if rec := e.manage("POST", path+"/scopes", map[string]any{"scopes": []string{"z", " z "}}); rec.Code != 200 {
		t.Fatal(rec.Code)
	}
	if m := decode(t, e.manage("GET", path, nil)); !reflect.DeepEqual(m["scopes"], []any{"z"}) || m["name"] != "g2" {
		t.Fatal(m)
	}
	if rec := e.manage("POST", path+"/scopes", map[string]any{}); rec.Code != 422 {
		t.Fatal(rec.Code)
	}
	if rec := e.manage("POST", "/manage/oauth/connections/999/scopes", map[string]any{"scopes": []string{}}); rec.Code != 404 {
		t.Fatal(rec.Code)
	}

	// In use by a datasource → 409.
	dsCreate(t, e, map[string]any{"name": "gm", "ds_type": "gmail", "config": map[string]any{}, "connection_id": id})
	rec = e.manage("DELETE", path, nil)
	if rec.Code != 409 || decode(t, rec)["error"] != "connection is in use by one or more datasources" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	rec = e.manage("POST", "/manage/oauth/connections", map[string]any{"name": "h", "provider": "hubspot", "client_id": "c"})
	id2 := int64(decode(t, rec)["id"].(float64))
	if rec := e.manage("POST", fmt.Sprintf("/manage/oauth/connections/%d/delete", id2), nil); rec.Code != 200 ||
		rec.Body.String() != `{"deleted":true}` {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	for _, body := range []map[string]any{
		{"provider": "p", "client_id": "c"}, {"name": "n", "client_id": "c"}, {"name": "n", "provider": "p"},
	} {
		if rec := e.manage("POST", "/manage/oauth/connections", body); rec.Code != 422 {
			t.Fatalf("%v: %d", body, rec.Code)
		}
	}
}

func TestOAuthStart(t *testing.T) {
	e := newTestEnv(t)
	rec := e.manage("POST", "/manage/oauth/connections", map[string]any{
		"name": "g", "provider": "google", "client_id": "cid", "scopes": []string{"custom.scope"}})
	connID := int64(decode(t, rec)["id"].(float64))
	gm := dsCreate(t, e, map[string]any{"name": "gm", "ds_type": "gmail", "config": map[string]any{}, "connection_id": connID})

	rec = e.manage("POST", "/manage/oauth/start", map[string]any{"datasource_id": gm, "return_to": "http://app/x"})
	if rec.Code != 200 {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	m := decode(t, rec)
	u, err := url.Parse(m["auth_url"].(string))
	if err != nil || !strings.Contains(u.Query().Get("scope"), "custom.scope") ||
		u.Query().Get("redirect_uri") != "http://127.0.0.1:9820/callback" {
		t.Fatalf("%v", m)
	}
	if len(e.srv.pendingOAuth) != 1 {
		t.Fatal("pending flow not stored")
	}
	for _, p := range e.srv.pendingOAuth {
		if p.ReturnTo != "http://app/x" || p.DatasourceID != gm {
			t.Fatalf("%+v", p)
		}
	}

	// Each OAuth provider type builds a URL; jira uses localhost; scopes fall back to defaults.
	for _, ty := range []string{"google_calendar", "google_drive", "jira", "linear", "github", "hubspot"} {
		id := dsCreate(t, e, map[string]any{"name": ty, "ds_type": ty, "config": map[string]any{"client_id": "c"}})
		rec := e.manage("POST", "/manage/oauth/start", map[string]any{"datasource_id": id})
		if rec.Code != 200 {
			t.Fatalf("%s: %d %s", ty, rec.Code, rec.Body.String())
		}
		au := decode(t, rec)["auth_url"].(string)
		if ty == "jira" && !strings.Contains(au, url.QueryEscape("http://localhost:9820/callback")) {
			t.Fatalf("jira redirect: %s", au)
		}
	}

	e.srv.oauthRedirectURL = "https://public.example/callback"
	rec = e.manage("POST", "/manage/oauth/start", map[string]any{"datasource_id": gm})
	if !strings.Contains(decode(t, rec)["auth_url"].(string), url.QueryEscape("https://public.example/callback")) {
		t.Fatal(rec.Body.String())
	}

	pg := dsCreate(t, e, map[string]any{"name": "pg", "ds_type": "postgres", "config": map[string]any{}})
	rec = e.manage("POST", "/manage/oauth/start", map[string]any{"datasource_id": pg})
	if rec.Code != 400 || !strings.HasPrefix(decode(t, rec)["error"].(string), "invalid oauth config: ") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	pg2 := dsCreate(t, e, map[string]any{"name": "pg2", "ds_type": "postgres", "config": map[string]any{"client_id": "c"}})
	rec = e.manage("POST", "/manage/oauth/start", map[string]any{"datasource_id": pg2})
	if rec.Code != 400 || decode(t, rec)["error"] != "datasource type does not use OAuth" {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if rec := e.manage("POST", "/manage/oauth/start", map[string]any{"datasource_id": 999}); rec.Code != 404 {
		t.Fatal(rec.Code)
	}
	if rec := e.manage("POST", "/manage/oauth/start", map[string]any{}); rec.Code != 422 {
		t.Fatal(rec.Code)
	}
}

func TestOAuthCallback(t *testing.T) {
	tokenSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if r.Form.Get("code") == "bad" {
			w.WriteHeader(400)
			_, _ = w.Write([]byte(`{"error":"invalid_grant"}`))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"access_token":"at","refresh_token":"rt","expires_in":3600,"token_type":"Bearer"}`))
	}))
	defer tokenSrv.Close()
	e := newTestEnv(t)
	add := func(state, returnTo string) {
		e.srv.pendingOAuth[state] = oauth.PendingFlow{DatasourceID: 7, CodeVerifier: "v",
			RedirectURI: "http://127.0.0.1/callback", ClientID: "c", TokenURL: tokenSrv.URL, ReturnTo: returnTo}
	}

	// Missing params → 400.
	if rec := e.do("GET", "/callback?state=x", "", nil); rec.Code != 400 {
		t.Fatal(rec.Code)
	}
	// Unknown state → HTML failure page.
	rec := e.do("GET", "/callback?code=c&state=nope", "", nil)
	if rec.Code != 200 || rec.Body.String() != "Authorization failed. You can close this tab." ||
		!strings.HasPrefix(rec.Header().Get("Content-Type"), "text/html") {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}

	add("s1", "")
	rec = e.do("GET", "/callback?code=good&state=s1", "", nil)
	if rec.Code != 200 || rec.Body.String() != "Authorization successful! You can close this tab." {
		t.Fatalf("%d %s", rec.Code, rec.Body.String())
	}
	if _, err := secretstore.Get(oauth.TokensKey(7)); err != nil {
		t.Fatal("tokens not stored")
	}
	if _, ok := e.srv.pendingOAuth["s1"]; ok {
		t.Fatal("pending flow not consumed")
	}

	add("s2", "http://app/done?x=1")
	rec = e.do("GET", "/callback?code=good&state=s2", "", nil)
	if rec.Code != 303 || rec.Header().Get("Location") != "http://app/done?x=1&oauth=ok" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}

	add("s3", "http://app/done")
	rec = e.do("GET", "/callback?code=bad&state=s3", "", nil)
	if rec.Code != 303 || rec.Header().Get("Location") != "http://app/done?oauth=error&reason=token+exchange+failed" {
		t.Fatalf("%d %v", rec.Code, rec.Header())
	}
}
