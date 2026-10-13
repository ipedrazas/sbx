package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/ipedrazas/sbx/internal/secretstore"
)

func (s *Server) registerSettings(mux *http.ServeMux) {
	handle(mux, s.getAISettings, "GET /manage/settings/ai")
	handle(mux, s.updateAISettings, "PUT /manage/settings/ai", "POST /manage/settings/ai")
	handle(mux, s.deleteAIKey, "DELETE /manage/settings/ai/api-key", "POST /manage/settings/ai/api-key")
}

func (s *Server) aiSettingsBody() map[string]any {
	ai := s.aiRuntime()
	return map[string]any{
		"enabled":            ai.Enabled,
		"model":              ai.Model,
		"api_key_configured": secretstore.GetAnthropicKey() != "",
	}
}

func (s *Server) getAISettings(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, s.aiSettingsBody())
}

type updateAIRequest struct {
	Enabled *bool   `json:"enabled"`
	Model   *string `json:"model"`
	// Present (even as null) means set/clear; absent leaves the key alone.
	APIKey json.RawMessage `json:"api_key"`
}

func (s *Server) updateAISettings(w http.ResponseWriter, r *http.Request) {
	var req updateAIRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, err)
		return
	}

	s.aiMu.Lock()
	if req.Enabled != nil {
		s.ai.Enabled = *req.Enabled
	}
	if req.Model != nil {
		if m := strings.TrimSpace(*req.Model); m != "" {
			s.ai.Model = m
		}
	}
	ai := s.ai
	s.aiMu.Unlock()

	cfg, err := ReadConfig(s.dataDir)
	if err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, "could not re-read config.toml to persist settings")
		return
	}
	cfg.AIEnabled = ai.Enabled
	cfg.AnthropicModel = ai.Model
	if err := cfg.Save(s.dataDir); err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, "failed to persist config: "+err.Error())
		return
	}

	if req.APIKey != nil {
		var key *string
		if err := json.Unmarshal(req.APIKey, &key); err != nil {
			writeError(w, apiErr(http.StatusUnprocessableEntity, "api_key: %v", err))
			return
		}
		if key != nil && strings.TrimSpace(*key) != "" {
			if err := secretstore.StoreAnthropicKey(strings.TrimSpace(*key)); err != nil {
				writeErrorOnly(w, http.StatusInternalServerError, "failed to store key in secret store: "+err.Error())
				return
			}
		} else if err := secretstore.DeleteAnthropicKey(); err != nil {
			writeErrorOnly(w, http.StatusInternalServerError, "failed to clear key: "+err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, s.aiSettingsBody())
}

func (s *Server) deleteAIKey(w http.ResponseWriter, _ *http.Request) {
	if err := secretstore.DeleteAnthropicKey(); err != nil {
		writeErrorOnly(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true})
}
