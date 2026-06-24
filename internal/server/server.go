// Package server is the daemon's HTTP layer: it serves the embedded Vue shell
// (overlaid disk-first by HOPSKIP_WEB_DIR), the inventory + session REST API,
// and the terminal/chat WebSockets.
package server

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"io/fs"
	"net/http"
	"os"
	"path"
	"strconv"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"github.com/saltylin/hopskip/internal/agent"
	"github.com/saltylin/hopskip/internal/session"
	"github.com/saltylin/hopskip/internal/store"
)

// Server bundles the daemon's dependencies for HTTP handling.
type Server struct {
	store    *store.Store
	mgr      *session.Manager
	agent    *agent.Agent
	embedFS  fs.FS  // the embedded web/public tree
	webDir   string // optional on-disk overlay (HOPSKIP_WEB_DIR)
	upgrader websocket.Upgrader
}

// New constructs a Server.
func New(st *store.Store, mgr *session.Manager, ag *agent.Agent, embedFS fs.FS, webDir string) *Server {
	return &Server{
		store:   st,
		mgr:     mgr,
		agent:   ag,
		embedFS: embedFS,
		webDir:  webDir,
		upgrader: websocket.Upgrader{
			// Single-operator local app: accept same-machine origins.
			CheckOrigin: func(r *http.Request) bool { return true },
		},
	}
}

// Handler builds the route table.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/hosts", s.handleListHosts)
	mux.HandleFunc("POST /api/hosts", s.handleAddHost)
	mux.HandleFunc("GET /api/hosts/{id}", s.handleGetHost)
	mux.HandleFunc("PATCH /api/hosts/{id}", s.handleUpdateHost)
	mux.HandleFunc("DELETE /api/hosts/{id}", s.handleDeleteHost)

	mux.HandleFunc("GET /api/sessions", s.handleListSessions)
	mux.HandleFunc("POST /api/sessions", s.handleOpenSession)
	mux.HandleFunc("POST /api/sessions/{id}/keys", s.handleSendKeys)
	mux.HandleFunc("GET /api/sessions/{id}/state", s.handleSessionState)
	mux.HandleFunc("DELETE /api/sessions/{id}", s.handleCloseSession)

	mux.HandleFunc("GET /ws/term/{id}", s.handleTermWS)
	mux.HandleFunc("GET /ws/chat", s.handleChatWS)

	mux.HandleFunc("GET /api/llm/credentials", s.handleListCreds)
	mux.HandleFunc("POST /api/llm/credentials", s.handleAddCred)
	mux.HandleFunc("PATCH /api/llm/credentials/{id}", s.handleUpdateCred)
	mux.HandleFunc("DELETE /api/llm/credentials/{id}", s.handleDeleteCred)
	mux.HandleFunc("POST /api/llm/credentials/{id}/activate", s.handleActivateCred)

	mux.HandleFunc("GET /api/chats", s.handleListChats)
	mux.HandleFunc("POST /api/chats", s.handleCreateChat)
	mux.HandleFunc("PATCH /api/chats/{id}", s.handleRenameChat)
	mux.HandleFunc("DELETE /api/chats/{id}", s.handleDeleteChat)
	mux.HandleFunc("GET /api/chats/{id}/messages", s.handleChatMessages)

	mux.HandleFunc("GET /api/settings", s.handleListSettings)
	mux.HandleFunc("PUT /api/settings/{key}", s.handleSetSetting)
	mux.HandleFunc("GET /api/providers", s.handleProviders)

	mux.HandleFunc("GET /api/knowledge", s.handleListKnowledge)
	mux.HandleFunc("POST /api/knowledge", s.handleAddKnowledge)
	mux.HandleFunc("DELETE /api/knowledge/{id}", s.handleDeleteKnowledge)

	mux.HandleFunc("GET /api/status", s.handleStatus)

	// the frontend extension manifest the shell's defensive loader consumes
	mux.HandleFunc("GET /api/extensions", s.handleListExtensions)

	// the "Changes" tab: what the agent authored vs the base shell, + export-to-disk
	mux.HandleFunc("GET /api/web/changes", s.handleWebChanges)
	mux.HandleFunc("GET /api/web/content", s.handleWebContent)
	mux.HandleFunc("POST /api/web/export", s.handleWebExport)

	mux.HandleFunc("/", s.handleStatic)
	return mux
}

// handleListExtensions returns the enabled manifest rows (the agent/operator-
// authored UI the shell loads on refresh). See spec §5.2.
func (s *Server) handleListExtensions(w http.ResponseWriter, r *http.Request) {
	exts, err := s.store.ListExtensions(true)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if exts == nil {
		exts = []store.Extension{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"extensions": exts})
}

// ---- JSON helpers ----

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, code int, msg string) {
	writeJSON(w, code, map[string]string{"error": msg})
}

// ---- inventory handlers ----

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	resp := map[string]any{
		"name":                "hopskip",
		"agent_configured":    s.agent.Configured(),
		"terminal_scrollback": s.terminalScrollback(),
	}
	if provider, model, ok := s.agent.ActiveInfo(); ok {
		resp["active_provider"] = provider
		resp["active_model"] = model
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleListHosts(w http.ResponseWriter, r *http.Request) {
	hosts, err := s.store.ListHosts()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if hosts == nil {
		hosts = []store.Host{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"hosts": hosts})
}

type addHostReq struct {
	Name     string            `json:"name"`
	Notes    string            `json:"notes"`
	SSH      string            `json:"ssh"`      // convenience: stored as tag "ssh"
	Via      string            `json:"via"`      // jump-host name: stored as tag "via"
	Provider string            `json:"provider"` // cloud provider: stored as tag "provider"
	Tags     map[string]string `json:"tags"`
}

func (s *Server) handleAddHost(w http.ResponseWriter, r *http.Request) {
	var req addHostReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	if req.Name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	id, err := s.store.AddHost(req.Name, strings.TrimSpace(req.Notes))
	if err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeErr(w, http.StatusConflict, "a host named "+req.Name+" already exists")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if ssh := strings.TrimSpace(req.SSH); ssh != "" {
		_ = s.store.TagHost(id, "ssh", ssh)
	}
	if via := strings.TrimSpace(req.Via); via != "" {
		_ = s.store.TagHost(id, "via", via)
	}
	if provider := strings.TrimSpace(req.Provider); provider != "" {
		_ = s.store.TagHost(id, "provider", provider)
	}
	for k, v := range req.Tags {
		if k = strings.TrimSpace(k); k != "" {
			_ = s.store.TagHost(id, k, v)
		}
	}
	host, err := s.store.GetHost(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, host)
}

func (s *Server) handleGetHost(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	host, err := s.store.GetHost(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "host not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, host)
}

type updateHostReq struct {
	Name     *string `json:"name"`
	Notes    *string `json:"notes"`
	SSH      *string `json:"ssh"`
	Via      *string `json:"via"`
	Provider *string `json:"provider"`
}

func (s *Server) handleUpdateHost(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	host, err := s.store.GetHost(id)
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "host not found")
		return
	}
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	var req updateHostReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	name, notes := host.Name, host.Notes
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	if req.Notes != nil {
		notes = strings.TrimSpace(*req.Notes)
	}
	if name == "" {
		writeErr(w, http.StatusBadRequest, "name is required")
		return
	}
	if err := s.store.UpdateHost(id, name, notes); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			writeErr(w, http.StatusConflict, "a host named "+name+" already exists")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if req.SSH != nil {
		if ssh := strings.TrimSpace(*req.SSH); ssh != "" {
			_ = s.store.TagHost(id, "ssh", ssh)
		} else {
			_ = s.store.DeleteTag(id, "ssh")
		}
	}
	if req.Via != nil {
		if via := strings.TrimSpace(*req.Via); via != "" {
			_ = s.store.TagHost(id, "via", via)
		} else {
			_ = s.store.DeleteTag(id, "via")
		}
	}
	if req.Provider != nil {
		if p := strings.TrimSpace(*req.Provider); p != "" {
			_ = s.store.TagHost(id, "provider", p)
		} else {
			_ = s.store.DeleteTag(id, "provider")
		}
	}
	updated, err := s.store.GetHost(id)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, updated)
}

func (s *Server) handleDeleteHost(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "bad id")
		return
	}
	if err := s.store.DeleteHost(id); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeErr(w, http.StatusNotFound, "host not found")
			return
		}
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}

// ---- session handlers ----

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.store.ListSessions()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if sessions == nil {
		sessions = []store.Session{}
	}
	writeJSON(w, http.StatusOK, map[string]any{"sessions": sessions})
}

type openSessionReq struct {
	Label  string `json:"label"`
	HostID *int64 `json:"host_id"`
}

func (s *Server) handleOpenSession(w http.ResponseWriter, r *http.Request) {
	var req openSessionReq
	_ = json.NewDecoder(r.Body).Decode(&req)
	ss, err := s.mgr.Open(req.Label, req.HostID)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusCreated, ss)
}

type sendKeysReq struct {
	Keys      string `json:"keys"`
	AwaitDone *bool  `json:"await_done"`
	TimeoutMs int    `json:"timeout_ms"`
}

func (s *Server) handleSendKeys(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	var req sendKeysReq
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	await := true
	if req.AwaitDone != nil {
		await = *req.AwaitDone
	}
	screen, code, completed, err := s.mgr.SendKeys(id, req.Keys, await, req.TimeoutMs)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"session_id": id, "screen": screen, "exit_code": code, "completed": completed,
	})
}

func (s *Server) handleCloseSession(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.mgr.Close(id); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": id, "closed": true})
}

// handleSessionState reports whether a session is open and currently logged into
// a remote host (an ssh process in its subtree) — drives the terminal's live
// indicator so it reflects ssh-session liveness, not just the pipe.
func (s *Server) handleSessionState(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	ss, err := s.store.GetSession(id)
	if err != nil {
		writeErr(w, http.StatusNotFound, "no such session")
		return
	}
	open := ss.Status == "open"
	remote := false
	if open {
		remote, _ = s.mgr.Remote(id)
	}
	writeJSON(w, http.StatusOK, map[string]any{"session_id": id, "open": open, "remote": remote})
}

// ---- static / SPA ----

const csp = "default-src 'self'; connect-src 'self'; img-src 'self' data:; " +
	"style-src 'self' 'unsafe-inline'; script-src 'self' 'unsafe-eval'"

// readStatic resolves a web asset across four layers, highest precedence first:
//  1. HOPSKIP_WEB_DIR disk overlay (optional, for human dev iteration),
//  2. the SQLite web_files overlay — PENDING authored edits (shown in Changes),
//  3. the committed disk overlay (web_export_dir) — what "Save to disk" wrote;
//     below the DB so a fresh authored edit still wins, above embed so saved-and-
//     cleared files keep serving,
//  4. the embedded base shell (always present; the single-binary guarantee).
// Assets are small, so it returns the full bytes.
func (s *Server) readStatic(name string) ([]byte, bool) {
	name = strings.TrimPrefix(path.Clean("/"+name), "/")
	if name == "" {
		name = "index.html"
	}
	if s.webDir != "" {
		if b, err := os.ReadFile(path.Join(s.webDir, name)); err == nil {
			return b, true
		}
	}
	if b, _, ok := s.store.GetWebFile(name); ok {
		return b, true
	}
	if d := s.webExportDir(); d != "" {
		if b, err := os.ReadFile(path.Join(d, name)); err == nil {
			return b, true
		}
	}
	if f, err := s.embedFS.Open(name); err == nil {
		defer f.Close()
		if st, err := f.Stat(); err == nil && !st.IsDir() {
			if b, err := io.ReadAll(f); err == nil {
				return b, true
			}
		}
	}
	return nil, false
}

func (s *Server) handleStatic(w http.ResponseWriter, r *http.Request) {
	upath := strings.TrimPrefix(r.URL.Path, "/")
	if upath == "" {
		upath = "index.html"
	}
	data, ok := s.readStatic(upath)
	if !ok {
		// SPA fallback: serve index.html for client-side routes.
		data, ok = s.readStatic("index.html")
		if !ok {
			http.NotFound(w, r)
			return
		}
		upath = "index.html"
	}

	w.Header().Set("Content-Type", contentType(upath))
	// Local single-operator dev tool: never let the browser serve a stale SPA
	// asset (the embedded files carry no usable Last-Modified/ETag validator).
	w.Header().Set("Cache-Control", "no-cache")
	if upath == "index.html" {
		w.Header().Set("Content-Security-Policy", csp)
	}
	http.ServeContent(w, r, upath, time.Time{}, bytes.NewReader(data))
}

func contentType(name string) string {
	switch {
	case strings.HasSuffix(name, ".html"):
		return "text/html; charset=utf-8"
	case strings.HasSuffix(name, ".js"), strings.HasSuffix(name, ".mjs"):
		return "text/javascript; charset=utf-8"
	case strings.HasSuffix(name, ".css"):
		return "text/css; charset=utf-8"
	case strings.HasSuffix(name, ".json"):
		return "application/json"
	case strings.HasSuffix(name, ".svg"):
		return "image/svg+xml"
	default:
		return "application/octet-stream"
	}
}

func modTime(f fs.File) time.Time {
	if st, err := f.Stat(); err == nil {
		return st.ModTime()
	}
	return time.Time{}
}
