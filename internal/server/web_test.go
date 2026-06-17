package server

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/saltylin/hopskip/internal/store"
)

func TestWebChangesContentExport(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &Server{store: st, embedFS: fstest.MapFS{"app.js": {Data: []byte("BASE\nLINE2\n")}}}

	// an override of a base file + a brand-new module + a registered feature
	_ = st.PutWebFile("app.js", []byte("BASE\nCHANGED\n"), "text/javascript", "agent")
	_ = st.PutWebFile("ext/connect/entry.mjs", []byte("export default {}"), "", "agent")
	_ = st.UpsertExtension(store.Extension{Slug: "connect-pre", Kind: "override", Mount: "connect.steps",
		Entry: "/ext/connect/entry.mjs", Title: "connect pre-steps", Notes: "runs mykinit before ssh", Enabled: true, CreatedBy: "agent"})

	// /api/web/changes: override + new status, and the feature
	rr := httptest.NewRecorder()
	s.handleWebChanges(rr, httptest.NewRequest(http.MethodGet, "/api/web/changes", nil))
	body := rr.Body.String()
	if !strings.Contains(body, `"status":"override"`) || !strings.Contains(body, `"status":"new"`) ||
		!strings.Contains(body, "connect-pre") || !strings.Contains(body, "runs mykinit") {
		t.Fatalf("changes wrong: %s", body)
	}

	// /api/web/content base vs overlay
	get := func(q string) string {
		rr := httptest.NewRecorder()
		s.handleWebContent(rr, httptest.NewRequest(http.MethodGet, "/api/web/content?"+q, nil))
		return rr.Body.String()
	}
	if !strings.Contains(get("layer=base&path=app.js"), "LINE2") {
		t.Fatal("base content should be the embedded shell")
	}
	if !strings.Contains(get("layer=overlay&path=app.js"), "CHANGED") {
		t.Fatal("overlay content should be the authored version")
	}

	// /api/web/export writes the DB overlay to disk
	dir := t.TempDir()
	_ = st.SetSetting("web_export_dir", dir)
	rr = httptest.NewRecorder()
	s.handleWebExport(rr, httptest.NewRequest(http.MethodPost, "/api/web/export", nil))
	if !strings.Contains(rr.Body.String(), `"count":2`) {
		t.Fatalf("export count wrong: %s", rr.Body.String())
	}
	if b, err := os.ReadFile(filepath.Join(dir, "app.js")); err != nil || string(b) != "BASE\nCHANGED\n" {
		t.Fatalf("exported app.js wrong: err=%v body=%q", err, b)
	}
	if _, err := os.Stat(filepath.Join(dir, "ext", "connect", "entry.mjs")); err != nil {
		t.Fatalf("nested exported module missing: %v", err)
	}

	// committing clears the saved items from the DB overlay (and thus the Changes tab)
	if wf, _ := st.ListWebFiles(); len(wf) != 0 {
		t.Fatalf("saved files should be removed from the DB overlay, still have %d", len(wf))
	}
	rr = httptest.NewRecorder()
	s.handleWebChanges(rr, httptest.NewRequest(http.MethodGet, "/api/web/changes", nil))
	body = rr.Body.String()
	if !strings.Contains(body, `"files":[]`) || !strings.Contains(body, `"extensions":[]`) {
		t.Fatalf("Changes tab should be empty after committing, got: %s", body)
	}

	// ...but the committed file still SERVES (from the export dir, below the now-empty DB)
	if b, ok := s.readStatic("app.js"); !ok || string(b) != "BASE\nCHANGED\n" {
		t.Fatalf("committed app.js should still serve from disk, got ok=%v body=%q", ok, b)
	}
	// a fresh authored edit still wins over the committed copy (DB above export dir)
	_ = st.PutWebFile("app.js", []byte("BASE\nNEWER\n"), "text/javascript", "agent")
	if b, _ := s.readStatic("app.js"); string(b) != "BASE\nNEWER\n" {
		t.Fatalf("a new DB edit must override the committed copy, got %q", b)
	}
}
