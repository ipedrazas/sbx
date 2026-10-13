package server

import (
	"fmt"
	"log/slog"
	"net/http"
	"strings"

	"github.com/ipedrazas/sbx/internal/crud"
	"github.com/ipedrazas/sbx/internal/factory"
	"github.com/ipedrazas/sbx/internal/models"
	"github.com/ipedrazas/sbx/internal/oauth"
	"github.com/ipedrazas/sbx/internal/util"
)

func (s *Server) registerOAuth(mux *http.ServeMux) {
	const p = "/manage/oauth"
	handle(mux, s.startOAuth, "POST "+p+"/start")
	handle(mux, s.listOAuthConnections, "GET "+p+"/connections")
	handle(mux, s.createOAuthConnection, "POST "+p+"/connections")
	handle(mux, s.getOAuthConnection, "GET "+p+"/connections/{id}")
	handle(mux, s.updateOAuthConnection, "PUT "+p+"/connections/{id}", "POST "+p+"/connections/{id}/update")
	handle(mux, s.updateOAuthConnectionScopes, "POST "+p+"/connections/{id}/scopes")
	handle(mux, s.deleteOAuthConnection, "DELETE "+p+"/connections/{id}", "POST "+p+"/connections/{id}/delete")
}

func (s *Server) listOAuthConnections(w http.ResponseWriter, r *http.Request) {
	conns, err := crud.ListOAuthConnections(r.Context(), s.db)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if conns == nil {
		conns = []models.OAuthConnection{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"connections": conns})
}

func (s *Server) getOAuthConnection(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	c, err := crud.GetOAuthConnection(r.Context(), s.db, id)
	switch {
	case err != nil:
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
	case c == nil:
		writeErrorOnly(w, http.StatusNotFound, "not found")
	default:
		writeJSON(w, http.StatusOK, c)
	}
}

type createOAuthConnectionRequest struct {
	Name         *string  `json:"name"`
	Provider     *string  `json:"provider"`
	ClientID     *string  `json:"client_id"`
	ClientSecret *string  `json:"client_secret"`
	Scopes       []string `json:"scopes"`
}

func (s *Server) createOAuthConnection(w http.ResponseWriter, r *http.Request) {
	var req createOAuthConnectionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	switch {
	case req.Name == nil:
		writeError(w, missingJSONField("name"))
		return
	case req.Provider == nil:
		writeError(w, missingJSONField("provider"))
		return
	case req.ClientID == nil:
		writeError(w, missingJSONField("client_id"))
		return
	}
	id, err := crud.CreateOAuthConnection(r.Context(), s.db, *req.Name, *req.Provider, *req.ClientID,
		req.ClientSecret, normalizeScopes(req.Scopes))
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

type updateOAuthConnectionRequest struct {
	Name         *string  `json:"name"`
	ClientID     *string  `json:"client_id"`
	ClientSecret *string  `json:"client_secret"`
	Scopes       []string `json:"scopes"`
}

func (s *Server) updateOAuthConnection(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	var req updateOAuthConnectionRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	switch {
	case req.Name == nil:
		writeError(w, missingJSONField("name"))
		return
	case req.ClientID == nil:
		writeError(w, missingJSONField("client_id"))
		return
	}
	ok, err := crud.UpdateOAuthConnection(r.Context(), s.db, id, *req.Name, *req.ClientID, req.ClientSecret,
		normalizeScopes(req.Scopes))
	writeUpdateResult(w, ok, err)
}

func (s *Server) updateOAuthConnectionScopes(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	var req struct {
		Scopes *[]string `json:"scopes"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.Scopes == nil {
		writeError(w, missingJSONField("scopes"))
		return
	}
	ok, err := crud.UpdateOAuthConnectionScopes(r.Context(), s.db, id, normalizeScopes(*req.Scopes))
	writeUpdateResult(w, ok, err)
}

func writeUpdateResult(w http.ResponseWriter, ok bool, err error) {
	switch {
	case err != nil:
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
	case !ok:
		writeErrorOnly(w, http.StatusNotFound, "not found")
	default:
		writeJSON(w, http.StatusOK, map[string]any{"updated": true})
	}
}

// normalizeScopes trims, drops empties and dedupes (order preserved).
func normalizeScopes(raw []string) []string {
	seen := map[string]bool{}
	out := []string{}
	for _, s := range raw {
		t := strings.TrimSpace(s)
		if t == "" || seen[t] {
			continue
		}
		seen[t] = true
		out = append(out, t)
	}
	return out
}

// defaultScopesFor is the fallback scope list for connections created before
// scopes were editable (empty `scopes` column).
func defaultScopesFor(t models.DatasourceType) []string {
	switch t {
	case models.DatasourceGmail:
		return []string{
			"https://www.googleapis.com/auth/gmail.readonly",
			"https://www.googleapis.com/auth/gmail.compose",
		}
	case models.DatasourceGoogleCalendar:
		return []string{"https://www.googleapis.com/auth/calendar.readonly"}
	case models.DatasourceGoogleDrive:
		return []string{"https://www.googleapis.com/auth/drive.readonly"}
	case models.DatasourceJira:
		return []string{"read:jira-work", "read:jira-user", "offline_access"}
	case models.DatasourceLinear:
		return []string{"read", "write"}
	case models.DatasourceGithub:
		return []string{"repo", "read:user", "read:org"}
	case models.DatasourceHubspot:
		return []string{
			"crm.objects.contacts.read",
			"crm.objects.companies.read",
			"crm.objects.deals.read",
		}
	default:
		return []string{}
	}
}

func (s *Server) deleteOAuthConnection(w http.ResponseWriter, r *http.Request) {
	id, err := pathInt64(r, "id")
	if err != nil {
		writeError(w, err)
		return
	}
	ok, err := crud.DeleteOAuthConnection(r.Context(), s.db, id)
	switch {
	case err != nil:
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
	case !ok:
		writeErrorOnly(w, http.StatusConflict, "connection is in use by one or more datasources")
	default:
		writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
	}
}

type oauthStartRequest struct {
	DatasourceID *int64 `json:"datasource_id"`
	// Where to redirect the browser after OAuth completes; nil renders a static page.
	ReturnTo *string `json:"return_to"`
}

func (s *Server) startOAuth(w http.ResponseWriter, r *http.Request) {
	var req oauthStartRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}
	if req.DatasourceID == nil {
		writeError(w, missingJSONField("datasource_id"))
		return
	}
	dsID := *req.DatasourceID
	ctx := r.Context()
	ds, err := crud.GetDatasource(ctx, s.db, dsID)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ds == nil {
		writeErrorOnly(w, http.StatusNotFound, "datasource not found")
		return
	}
	creds, err := factory.ResolveOAuthCredentials(ctx, s.db, ds)
	if err != nil {
		writeErrorOnly(w, http.StatusBadRequest, fmt.Sprintf("invalid oauth config: %v", err))
		return
	}

	// Scopes come from the bound connection when non-empty, else per-type defaults.
	var scopes []string
	if ds.ConnectionID != nil {
		if c, err := crud.GetOAuthConnection(ctx, s.db, *ds.ConnectionID); err == nil && c != nil {
			scopes = c.Scopes
		}
	}
	if len(scopes) == 0 {
		scopes = defaultScopesFor(ds.DsType)
	}

	redirectURI := s.oauthRedirectURL
	if redirectURI == "" {
		host := "127.0.0.1"
		if ds.DsType == models.DatasourceJira {
			host = "localhost"
		}
		redirectURI = fmt.Sprintf("http://%s:%d/callback", host, s.daemonPort)
	}

	var build func(oauth.Credentials, []string, string, int64) (string, string, oauth.PendingFlow)
	switch ds.DsType {
	case models.DatasourceGmail, models.DatasourceGoogleCalendar, models.DatasourceGoogleDrive:
		build = oauth.BuildGoogleAuthURL
	case models.DatasourceJira:
		build = oauth.BuildJiraAuthURL
	case models.DatasourceLinear:
		build = oauth.BuildLinearAuthURL
	case models.DatasourceGithub:
		build = oauth.BuildGithubAuthURL
	case models.DatasourceHubspot:
		build = oauth.BuildHubspotAuthURL
	default:
		writeErrorOnly(w, http.StatusBadRequest, "datasource type does not use OAuth")
		return
	}
	authURL, stateToken, pending := build(creds, scopes, redirectURI, dsID)
	pending.ReturnTo = ""
	if req.ReturnTo != nil {
		pending.ReturnTo = *req.ReturnTo
	}

	s.oauthMu.Lock()
	s.pendingOAuth[stateToken] = pending
	s.oauthMu.Unlock()

	writeJSON(w, http.StatusOK, map[string]any{
		"auth_url": authURL,
		"message":  "Open the auth_url in your browser to authorize",
	})
}

// handleOAuthCallback handles the provider redirect at GET /callback (outside
// the manage auth gate — the browser carries no bearer token).
func (s *Server) handleOAuthCallback(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	for _, f := range []string{"code", "state"} {
		if !q.Has(f) {
			writeError(w, errBadRequest("Failed to deserialize query string: missing field `%s`", f))
			return
		}
	}
	code, state := q.Get("code"), q.Get("state")

	s.oauthMu.Lock()
	pending, ok := s.pendingOAuth[state]
	delete(s.pendingOAuth, state)
	s.oauthMu.Unlock()

	if !ok {
		slog.Warn("OAuth callback with unknown state token")
		finishCallback(w, "", "error", "unknown or expired OAuth flow")
		return
	}
	tokens, err := oauth.ExchangeCode(r.Context(), &pending, code)
	if err != nil {
		slog.Error("OAuth token exchange failed", "err", err)
		finishCallback(w, pending.ReturnTo, "error", "token exchange failed")
		return
	}
	if err := oauth.StoreTokens(pending.DatasourceID, tokens); err != nil {
		slog.Error("Failed to store OAuth tokens", "err", err)
		finishCallback(w, pending.ReturnTo, "error", "failed to store tokens")
		return
	}
	slog.Info("OAuth tokens stored", "datasource", pending.DatasourceID)
	finishCallback(w, pending.ReturnTo, "ok", "")
}

// finishCallback redirects to returnTo with ?oauth=<status>[&reason=...] (303,
// like axum's Redirect::to), or renders a static HTML page when returnTo is empty.
func finishCallback(w http.ResponseWriter, returnTo, status, reason string) {
	if returnTo != "" {
		sep := "?"
		if strings.Contains(returnTo, "?") {
			sep = "&"
		}
		u := returnTo + sep + "oauth=" + status
		if reason != "" {
			u += "&reason=" + util.URLEncode(reason)
		}
		w.Header().Set("Location", u)
		w.WriteHeader(http.StatusSeeOther)
		return
	}
	msg := "Authorization failed. You can close this tab."
	if status == "ok" {
		msg = "Authorization successful! You can close this tab."
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(msg))
}
