package server

import (
	"io/fs"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/saltylin/hopskip/internal/store"
)

// handleWebChanges reports what the agent/operator has authored on top of the
// embedded base shell: each overlay file tagged 'new' (no base equivalent) or
// 'override' (shadows a base file), plus the registered extensions (the human-
// readable feature list). Powers the "Changes" tab.
func (s *Server) handleWebChanges(w http.ResponseWriter, r *http.Request) {
	files, err := s.store.ListWebFiles()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	exts, err := s.store.ListExtensions(false)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	if exts == nil {
		exts = []store.Extension{}
	}
	type fileChange struct {
		store.WebFile
		Status   string `json:"status"`    // new | override
		BaseSize int    `json:"base_size"` // bytes in the embedded base (0 if new)
	}
	out := []fileChange{}
	pending := map[string]bool{}
	for _, f := range files {
		fc := fileChange{WebFile: f, Status: "new"}
		if s.embedFS != nil {
			if b, err := fs.ReadFile(s.embedFS, f.Path); err == nil {
				fc.Status, fc.BaseSize = "override", len(b)
			}
		}
		out = append(out, fc)
		pending[f.Path] = true
	}
	// Show an extension as a change only while its code is still PENDING (in the
	// DB overlay). Once its files are saved to disk they leave the DB, so the
	// feature is committed and drops off the Changes tab.
	shown := []store.Extension{}
	for _, e := range exts {
		if ep, ok := store.NormalizeWebPath(e.Entry); ok && pending[ep] {
			shown = append(shown, e)
		}
	}
	writeJSON(w, http.StatusOK, map[string]any{"files": out, "extensions": shown})
}

// handleWebContent returns one layer of a web file (?layer=base|overlay), so the
// Changes tab can diff an override against the shipped base.
func (s *Server) handleWebContent(w http.ResponseWriter, r *http.Request) {
	norm, ok := store.NormalizeWebPath(r.URL.Query().Get("path"))
	if !ok {
		writeErr(w, http.StatusBadRequest, "bad path")
		return
	}
	if r.URL.Query().Get("layer") == "base" {
		if s.embedFS != nil {
			if b, err := fs.ReadFile(s.embedFS, norm); err == nil {
				writeJSON(w, http.StatusOK, map[string]any{"path": norm, "layer": "base", "content": string(b)})
				return
			}
		}
		writeJSON(w, http.StatusOK, map[string]any{"path": norm, "layer": "base", "content": "", "absent": true})
		return
	}
	if b, _, ok := s.store.GetWebFile(norm); ok {
		writeJSON(w, http.StatusOK, map[string]any{"path": norm, "layer": "overlay", "content": string(b)})
		return
	}
	writeErr(w, http.StatusNotFound, "no such overlay file")
}

// handleWebExport writes the DB-stored overlay to disk (the "Save to disk"
// button) — a faithful copy for editing/backup/version-control. Paths are
// already normalized (no traversal) on write; we re-check containment anyway.
func (s *Server) handleWebExport(w http.ResponseWriter, r *http.Request) {
	files, err := s.store.ListWebFiles()
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	abs, err := filepath.Abs(s.webExportDir())
	if err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	count := 0
	for _, f := range files {
		content, _, ok := s.store.GetWebFile(f.Path)
		if !ok {
			continue
		}
		full := filepath.Join(abs, filepath.FromSlash(f.Path))
		if full != abs && !strings.HasPrefix(full, abs+string(os.PathSeparator)) {
			continue // defense in depth against traversal
		}
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		if err := os.WriteFile(full, content, 0o644); err != nil {
			writeErr(w, http.StatusInternalServerError, err.Error())
			return
		}
		// committed: it now serves from disk (below pending DB edits), so drop it
		// from the DB overlay — that's what clears it from the Changes tab.
		_ = s.store.DeleteWebFile(f.Path)
		count++
	}
	writeJSON(w, http.StatusOK, map[string]any{"dir": abs, "count": count})
}
