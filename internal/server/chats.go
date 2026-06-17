package server

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/saltylin/hopskip/internal/store"
)

func (s *Server) handleListChats(w http.ResponseWriter, r *http.Request) {
	chats, err := s.store.ListChats()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if chats == nil {
		chats = []store.ChatMeta{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"chats": chats})
}

func (s *Server) handleCreateChat(w http.ResponseWriter, r *http.Request) {
	id, err := s.store.CreateChat()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"id": id, "title": ""})
}

func (s *Server) handleRenameChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req struct {
		Title string `json:"title"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	if err := s.store.RenameChat(id, strings.TrimSpace(req.Title)); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleDeleteChat(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.store.DeleteChat(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	s.agent.DropChat(id) // free the in-memory conversation too
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

func (s *Server) handleChatMessages(w http.ResponseWriter, r *http.Request) {
	msgs, err := s.store.ListChatMessages(r.PathValue("id"))
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if msgs == nil {
		msgs = []store.ChatMessage{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"messages": msgs})
}
