package oauth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/ipedrazas/sbx/internal/secretstore"
)

func sp(s string) *string { return &s }
func ip(i int64) *int64   { return &i }

func TestGoogleAuthURL(t *testing.T) {
	u, state, p := BuildGoogleAuthURL(Credentials{ClientID: "test-client.apps.googleusercontent.com"},
		[]string{"https://www.googleapis.com/auth/gmail.readonly"}, "http://127.0.0.1:9820/callback", 42)
	for _, want := range []string{"code_challenge=", "code_challenge_method=S256", "state=", "client_id=test-client", "127.0.0.1%3A9820", "access_type=offline"} {
		if !strings.Contains(u, want) {
			t.Errorf("missing %q in %s", want, u)
		}
	}
	if state == "" || p.DatasourceID != 42 || p.RedirectURI != "http://127.0.0.1:9820/callback" || p.TokenURL != GoogleTokenURL {
		t.Fatalf("bad pending %+v", p)
	}
}

func TestProviderURLs(t *testing.T) {
	c := Credentials{ClientID: "Iv1.client", ClientSecret: sp("s")}
	u, _, p := BuildGithubAuthURL(c, []string{"repo", "read:user"}, "http://x/cb", 7)
	if !strings.Contains(u, "github.com/login/oauth/authorize") || !strings.Contains(u, "scope=repo+read%3Auser") || p.TokenURL != GithubTokenURL {
		t.Fatalf("github: %s", u)
	}
	u, _, p = BuildHubspotAuthURL(c, []string{"crm.objects.contacts.read"}, "http://x/cb", 55)
	if !strings.Contains(u, "app.hubspot.com/oauth/authorize") || strings.Contains(u, "code_challenge") || p.TokenURL != HubspotTokenURL {
		t.Fatalf("hubspot: %s", u)
	}
	u, _, p = BuildJiraAuthURL(c, []string{"read:jira-work", "offline_access"}, "http://x/cb", 99)
	if !strings.Contains(u, "auth.atlassian.com/authorize?audience=api.atlassian.com&client_id=") || p.TokenURL != JiraTokenURL {
		t.Fatalf("jira: %s", u)
	}
	u, _, p = BuildLinearAuthURL(c, []string{"read", "write"}, "http://x/cb", 17)
	if !strings.Contains(u, "scope=read%2Cwrite") || !strings.Contains(u, "code_challenge=") || p.TokenURL != LinearTokenURL {
		t.Fatalf("linear: %s", u)
	}
	if p.ClientSecret == nil || *p.ClientSecret != "s" {
		t.Fatal("client secret not carried")
	}
}

func tokenServer(t *testing.T, status int, body string, check func(r *http.Request)) *httptest.Server {
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = r.ParseForm()
		if check != nil {
			check(r)
		}
		w.WriteHeader(status)
		_, _ = w.Write([]byte(body))
	}))
}

func TestExchangeAndRefresh(t *testing.T) {
	now = func() int64 { return 1000 }
	defer func() { now = defaultNow }()

	srv := tokenServer(t, 200, `{"access_token":"at","refresh_token":"rt","expires_in":3600}`, func(r *http.Request) {
		if r.Header.Get("Accept") != "application/json" {
			t.Error("missing Accept header")
		}
		if r.Form.Get("grant_type") == "authorization_code" && (r.Form.Get("code") != "c" || r.Form.Get("code_verifier") != "v" || r.Form.Get("client_secret") != "s") {
			t.Errorf("bad form %v", r.Form)
		}
	})
	defer srv.Close()

	tok, err := ExchangeCode(context.Background(), &PendingFlow{CodeVerifier: "v", ClientID: "id", ClientSecret: sp("s"), TokenURL: srv.URL}, "c")
	if err != nil || tok.AccessToken != "at" || *tok.RefreshToken != "rt" || *tok.ExpiresAt != 4600 {
		t.Fatalf("exchange: %+v %v", tok, err)
	}

	tok, err = Refresh(context.Background(), Credentials{ClientID: "id"}, "orig", srv.URL)
	if err != nil || *tok.RefreshToken != "orig" {
		t.Fatalf("refresh should keep original refresh token: %+v %v", tok, err)
	}

	bad := tokenServer(t, 400, `{"error":"invalid_grant"}`, nil)
	defer bad.Close()
	_, err = Refresh(context.Background(), Credentials{ClientID: "id"}, "orig", bad.URL)
	var te *TokenExchangeError
	if !errors.As(err, &te) || !strings.Contains(te.Body, "invalid_grant") {
		t.Fatalf("want token exchange error, got %v", err)
	}
}

var defaultNow = now

func TestGetValidToken(t *testing.T) {
	secretstore.Init(secretstore.NewMemory())
	now = func() int64 { return 1000 }
	defer func() { now = defaultNow }()

	if _, err := GetValidToken(context.Background(), 1, Credentials{}, "http://unused"); err == nil {
		t.Fatal("expected error when no tokens stored")
	}

	// Not expired: returned as-is.
	if err := StoreTokens(1, &Tokens{AccessToken: "fresh", ExpiresAt: ip(5000)}); err != nil {
		t.Fatal(err)
	}
	if got, err := GetValidToken(context.Background(), 1, Credentials{}, "http://unused"); err != nil || got != "fresh" {
		t.Fatalf("got %q %v", got, err)
	}

	// Expired without refresh token.
	_ = StoreTokens(2, &Tokens{AccessToken: "old", ExpiresAt: ip(1030)})
	if _, err := GetValidToken(context.Background(), 2, Credentials{}, "http://unused"); !errors.Is(err, ErrNoRefreshToken) {
		t.Fatalf("want ErrNoRefreshToken, got %v", err)
	}

	// Expired with refresh token: refreshed and persisted.
	srv := tokenServer(t, 200, `{"access_token":"new","expires_in":60}`, nil)
	defer srv.Close()
	_ = StoreTokens(3, &Tokens{AccessToken: "old", RefreshToken: sp("rt"), ExpiresAt: ip(900)})
	if got, err := GetValidToken(context.Background(), 3, Credentials{ClientID: "id"}, srv.URL); err != nil || got != "new" {
		t.Fatalf("got %q %v", got, err)
	}
	stored, _ := RetrieveTokens(3)
	if stored.AccessToken != "new" || *stored.RefreshToken != "rt" {
		t.Fatalf("not persisted: %+v", stored)
	}

	_ = secretstore.Put(TokensKey(4), []byte("not json"))
	if _, err := RetrieveTokens(4); err == nil {
		t.Fatal("expected corrupt token error")
	}
}
