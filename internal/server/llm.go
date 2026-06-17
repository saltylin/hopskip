package server

import (
	"encoding/json"
	"errors"
	"net/http"
	"strconv"
	"strings"

	"github.com/saltylin/hopskip/internal/store"
)

// suggested models per provider for the Settings dropdown (free text is allowed).
var providerModels = map[string][]string{
	"anthropic": {"claude-opus-4-8", "claude-sonnet-4-6", "claude-haiku-4-5"},
	"openai":    {"gpt-5.5-pro", "gpt-5.5", "gpt-5-pro", "gpt-5", "gpt-5-mini", "gpt-4.1", "gpt-4o", "gpt-4o-mini", "o3"},
}

var providerDefaultModel = map[string]string{
	"anthropic": "claude-opus-4-8",
	"openai":    "gpt-4o",
}

// maskToken returns a non-secret display form (prefix…last4).
func maskToken(t string) string {
	if len(t) <= 8 {
		return "••••"
	}
	prefix := t
	if len(prefix) > 6 {
		prefix = prefix[:6]
	}
	return prefix + "…" + t[len(t)-4:]
}

func credView(c store.Credential) map[string]any {
	return map[string]any{
		"id":           c.ID,
		"label":        c.Label,
		"provider":     c.Provider,
		"model":        c.Model,
		"active":       c.Active,
		"token_masked": maskToken(c.Token),
		"created_at":   c.CreatedAt,
	}
}

func (s *Server) handleListCreds(w http.ResponseWriter, r *http.Request) {
	creds, err := s.store.ListLLMCredentials()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	views := make([]map[string]any, 0, len(creds))
	for _, c := range creds {
		views = append(views, credView(c))
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"credentials": views,
		"providers":   providerModels,
		"defaults":    providerDefaultModel,
	})
}

type credReq struct {
	Label    string `json:"label"`
	Provider string `json:"provider"`
	Token    string `json:"token"`
	Model    string `json:"model"`
}

func (s *Server) handleAddCred(w http.ResponseWriter, r *http.Request) {
	var req credReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	req.Provider = strings.TrimSpace(req.Provider)
	req.Token = strings.TrimSpace(req.Token)
	if _, ok := providerModels[req.Provider]; !ok {
		writeErr(w, http.StatusBadRequest, "provider must be anthropic or openai")
		return
	}
	if req.Token == "" {
		writeErr(w, http.StatusBadRequest, "token is required")
		return
	}
	if strings.TrimSpace(req.Model) == "" {
		req.Model = providerDefaultModel[req.Provider]
	}
	if strings.TrimSpace(req.Label) == "" {
		req.Label = req.Provider
	}
	id, err := s.store.AddLLMCredential(req.Label, req.Provider, req.Token, req.Model)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) handleUpdateCred(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	var req credReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if err := s.store.UpdateLLMCredential(id, strings.TrimSpace(req.Label), strings.TrimSpace(req.Model), strings.TrimSpace(req.Token)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteCred(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.store.DeleteLLMCredential(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleActivateCred(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.store.SetActiveLLMCredential(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "credential not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
