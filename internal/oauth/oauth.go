// Package oauth implements the authorization-code + PKCE flows for the
// supported providers and persists tokens in the secret store.
package oauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/ipedrazas/sbx/internal/secretstore"
	"github.com/ipedrazas/sbx/internal/util"
)

const (
	GoogleAuthURL   = "https://accounts.google.com/o/oauth2/v2/auth"
	GoogleTokenURL  = "https://oauth2.googleapis.com/token"
	JiraAuthURL     = "https://auth.atlassian.com/authorize"
	JiraTokenURL    = "https://auth.atlassian.com/oauth/token"
	GithubAuthURL   = "https://github.com/login/oauth/authorize"
	GithubTokenURL  = "https://github.com/login/oauth/access_token"
	HubspotAuthURL  = "https://app.hubspot.com/oauth/authorize"
	HubspotTokenURL = "https://api.hubapi.com/oauth/2026-03/token"
	LinearAuthURL   = "https://linear.app/oauth/authorize"
	LinearTokenURL  = "https://api.linear.app/oauth/token"
)

// HTTPClient is used for token exchange/refresh. Tests may replace it.
var HTTPClient = &http.Client{Timeout: 30 * time.Second}

// now is overridable in tests.
var now = func() int64 { return time.Now().Unix() }

type Credentials struct {
	ClientID     string  `json:"client_id"`
	ClientSecret *string `json:"client_secret,omitempty"`
}

type Tokens struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken *string `json:"refresh_token"`
	ExpiresAt    *int64  `json:"expires_at"`
}

type tokenResponse struct {
	AccessToken  string  `json:"access_token"`
	RefreshToken *string `json:"refresh_token"`
	ExpiresIn    *int64  `json:"expires_in"`
}

var (
	ErrNoRefreshToken = errors.New("no refresh token available")
	ErrStateMismatch  = errors.New("OAuth state mismatch — possible CSRF attack")
	ErrUnknownFlow    = errors.New("unknown or expired OAuth flow")
)

// TokenExchangeError carries the provider's error body.
type TokenExchangeError struct{ Body string }

func (e *TokenExchangeError) Error() string { return "token exchange failed: " + e.Body }

// PendingFlow is held by the daemon until the provider redirects back.
type PendingFlow struct {
	DatasourceID int64
	CodeVerifier string
	RedirectURI  string
	ClientID     string
	ClientSecret *string
	TokenURL     string
	// ReturnTo is where to send the browser after the exchange; empty renders HTML.
	ReturnTo string
}

type provider struct {
	authURL, tokenURL string
	scopeSep          string
	extra             string // provider-specific params inserted after client/redirect/type/scope
	pkce              bool
	prefix            string // params inserted before client_id
}

func buildAuthURL(p provider, creds Credentials, scopes []string, redirectURI string, dsID int64) (string, string, PendingFlow) {
	verifier, challenge := util.GeneratePKCEPair()
	state := util.GenerateState()
	var sb strings.Builder
	sb.WriteString(p.authURL + "?")
	sb.WriteString(p.prefix)
	fmt.Fprintf(&sb, "client_id=%s&redirect_uri=%s&response_type=code&scope=%s",
		util.URLEncode(creds.ClientID), util.URLEncode(redirectURI), util.URLEncode(strings.Join(scopes, p.scopeSep)))
	sb.WriteString(p.extra)
	if p.pkce {
		fmt.Fprintf(&sb, "&code_challenge=%s&code_challenge_method=S256", util.URLEncode(challenge))
	}
	fmt.Fprintf(&sb, "&state=%s", util.URLEncode(state))
	return sb.String(), state, PendingFlow{
		DatasourceID: dsID,
		CodeVerifier: verifier,
		RedirectURI:  redirectURI,
		ClientID:     creds.ClientID,
		ClientSecret: creds.ClientSecret,
		TokenURL:     p.tokenURL,
	}
}

// BuildGoogleAuthURL returns (auth URL, state token, pending flow).
func BuildGoogleAuthURL(c Credentials, scopes []string, redirectURI string, dsID int64) (string, string, PendingFlow) {
	return buildAuthURL(provider{authURL: GoogleAuthURL, tokenURL: GoogleTokenURL, scopeSep: " ", extra: "&access_type=offline&prompt=consent", pkce: true}, c, scopes, redirectURI, dsID)
}

// BuildJiraAuthURL builds an Atlassian auth URL (audience=api.atlassian.com).
func BuildJiraAuthURL(c Credentials, scopes []string, redirectURI string, dsID int64) (string, string, PendingFlow) {
	return buildAuthURL(provider{authURL: JiraAuthURL, tokenURL: JiraTokenURL, scopeSep: " ", prefix: "audience=api.atlassian.com&", extra: "&prompt=consent", pkce: true}, c, scopes, redirectURI, dsID)
}

// BuildGithubAuthURL builds a GitHub OAuth App auth URL.
func BuildGithubAuthURL(c Credentials, scopes []string, redirectURI string, dsID int64) (string, string, PendingFlow) {
	return buildAuthURL(provider{authURL: GithubAuthURL, tokenURL: GithubTokenURL, scopeSep: " ", pkce: true}, c, scopes, redirectURI, dsID)
}

// BuildHubspotAuthURL builds a HubSpot auth URL (no PKCE challenge sent).
func BuildHubspotAuthURL(c Credentials, scopes []string, redirectURI string, dsID int64) (string, string, PendingFlow) {
	return buildAuthURL(provider{authURL: HubspotAuthURL, tokenURL: HubspotTokenURL, scopeSep: " "}, c, scopes, redirectURI, dsID)
}

// BuildLinearAuthURL builds a Linear auth URL (comma-separated scopes).
func BuildLinearAuthURL(c Credentials, scopes []string, redirectURI string, dsID int64) (string, string, PendingFlow) {
	return buildAuthURL(provider{authURL: LinearAuthURL, tokenURL: LinearTokenURL, scopeSep: ",", extra: "&prompt=consent", pkce: true}, c, scopes, redirectURI, dsID)
}

func postToken(ctx context.Context, tokenURL string, form url.Values) (*tokenResponse, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, tokenURL, strings.NewReader(form.Encode()))
	if err != nil {
		return nil, err
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	// GitHub defaults to form-encoded responses; Accept coerces JSON.
	req.Header.Set("Accept", "application/json")
	resp, err := HTTPClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("HTTP error: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode/100 != 2 {
		return nil, &TokenExchangeError{Body: string(body)}
	}
	var tr tokenResponse
	if err := json.Unmarshal(body, &tr); err != nil {
		return nil, fmt.Errorf("HTTP error: decoding token response: %w", err)
	}
	if tr.AccessToken == "" {
		return nil, &TokenExchangeError{Body: string(body)}
	}
	return &tr, nil
}

func expiry(in *int64) *int64 {
	if in == nil {
		return nil
	}
	v := now() + *in
	return &v
}

// ExchangeCode trades an authorization code for tokens.
func ExchangeCode(ctx context.Context, p *PendingFlow, code string) (*Tokens, error) {
	form := url.Values{}
	form.Set("code", code)
	form.Set("client_id", p.ClientID)
	form.Set("redirect_uri", p.RedirectURI)
	form.Set("grant_type", "authorization_code")
	form.Set("code_verifier", p.CodeVerifier)
	if p.ClientSecret != nil {
		form.Set("client_secret", *p.ClientSecret)
	}
	tr, err := postToken(ctx, p.TokenURL, form)
	if err != nil {
		return nil, err
	}
	return &Tokens{AccessToken: tr.AccessToken, RefreshToken: tr.RefreshToken, ExpiresAt: expiry(tr.ExpiresIn)}, nil
}

// Refresh exchanges a refresh token at tokenURL. The original refresh token
// is kept on the returned Tokens.
func Refresh(ctx context.Context, c Credentials, refreshToken, tokenURL string) (*Tokens, error) {
	form := url.Values{}
	form.Set("client_id", c.ClientID)
	form.Set("refresh_token", refreshToken)
	form.Set("grant_type", "refresh_token")
	if c.ClientSecret != nil {
		form.Set("client_secret", *c.ClientSecret)
	}
	tr, err := postToken(ctx, tokenURL, form)
	if err != nil {
		return nil, err
	}
	rt := refreshToken
	return &Tokens{AccessToken: tr.AccessToken, RefreshToken: &rt, ExpiresAt: expiry(tr.ExpiresIn)}, nil
}

// TokensKey is the secret-store key for a datasource's tokens.
func TokensKey(datasourceID int64) string { return fmt.Sprintf("oauth_tokens_%d", datasourceID) }

// StoreTokens persists tokens for a datasource.
func StoreTokens(datasourceID int64, t *Tokens) error {
	b, err := json.Marshal(t)
	if err != nil {
		return err
	}
	if err := secretstore.Put(TokensKey(datasourceID), b); err != nil {
		return fmt.Errorf("secret store error: %w", err)
	}
	return nil
}

// RetrieveTokens loads tokens for a datasource.
func RetrieveTokens(datasourceID int64) (*Tokens, error) {
	b, err := secretstore.Get(TokensKey(datasourceID))
	if err != nil {
		return nil, fmt.Errorf("secret store error: %w", err)
	}
	var t Tokens
	if err := json.Unmarshal(b, &t); err != nil {
		return nil, &TokenExchangeError{Body: "corrupt token data: " + err.Error()}
	}
	return &t, nil
}

// GetValidToken returns an access token, refreshing (and persisting) it when
// it expires within 60 seconds.
func GetValidToken(ctx context.Context, datasourceID int64, c Credentials, tokenURL string) (string, error) {
	t, err := RetrieveTokens(datasourceID)
	if err != nil {
		return "", err
	}
	if t.ExpiresAt != nil && now() >= *t.ExpiresAt-60 {
		if t.RefreshToken == nil {
			return "", ErrNoRefreshToken
		}
		nt, err := Refresh(ctx, c, *t.RefreshToken, tokenURL)
		if err != nil {
			return "", err
		}
		if err := StoreTokens(datasourceID, nt); err != nil {
			return "", err
		}
		return nt.AccessToken, nil
	}
	return t.AccessToken, nil
}
