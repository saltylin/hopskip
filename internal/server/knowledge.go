package server

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/saltylin/hopskip/internal/store"
)

func (s *Server) handleListKnowledge(w http.ResponseWriter, r *http.Request) {
	ks, err := s.store.ListKnowledge()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ks == nil {
		ks = []store.Knowledge{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"knowledge": ks})
}

func (s *Server) handleAddKnowledge(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Text string `json:"text"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	text := strings.TrimSpace(req.Text)
	if text == "" {
		writeErr(w, http.StatusBadRequest, "text is required")
		return
	}
	id, err := s.store.AddKnowledge(text)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) handleDeleteKnowledge(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.store.DeleteKnowledge(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
