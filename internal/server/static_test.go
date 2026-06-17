package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/saltylin/hopskip/internal/store"
)

// TestStaticOverlayAndManifest verifies the three-layer resolver (DB overlay
// shadows embed; embed is the fallback) and the /api/extensions manifest.
func TestStaticOverlayAndManifest(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	s := &Server{
		store:   st,
		embedFS: fstest.MapFS{"app.js": {Data: []byte("BASE_SHELL")}},
	}

	get := func(path string) (*httptest.ResponseRecorder, string) {
		rr := httptest.NewRecorder()
		s.handleStatic(rr, httptest.NewRequest(http.MethodGet, path, nil))
		return rr, rr.Body.String()
	}

	// 1. base shell served from embed when no overlay
	if rr, body := get("/app.js"); rr.Code != 200 || body != "BASE_SHELL" {
		t.Fatalf("embed base: code=%d body=%q", rr.Code, body)
	}

	// 2. DB overlay shadows embed
	if err := st.PutWebFile("app.js", []byte("AUTHORED_OVERLAY"), "text/javascript", "agent"); err != nil {
		t.Fatal(err)
	}
	if rr, body := get("/app.js"); body != "AUTHORED_OVERLAY" {
		t.Fatalf("overlay should shadow embed: code=%d body=%q", rr.Code, body)
	}

	// 3. a brand-new authored path (no embed equivalent) is served from the overlay
	if err := st.PutWebFile("ext/connect/entry.mjs", []byte("export default {}"), "text/javascript", "agent"); err != nil {
		t.Fatal(err)
	}
	if rr, body := get("/ext/connect/entry.mjs"); rr.Code != 200 || !strings.Contains(body, "export default") {
		t.Fatalf("authored module not served: code=%d body=%q", rr.Code, body)
	}

	// 4. no-cache header so authored UI is never stale
	if rr, _ := get("/app.js"); rr.Header().Get("Cache-Control") != "no-cache" {
		t.Fatalf("expected Cache-Control: no-cache, got %q", rr.Header().Get("Cache-Control"))
	}

	// 5. /api/extensions returns only enabled rows
	_ = st.UpsertExtension(store.Extension{Slug: "on", Kind: "slot", Mount: "global.nav", Entry: "/ext/on/e.mjs", Enabled: true, CreatedBy: "agent"})
	_ = st.UpsertExtension(store.Extension{Slug: "off", Kind: "slot", Mount: "global.nav", Entry: "/ext/off/e.mjs", Enabled: true, CreatedBy: "agent"})
	_ = st.SetExtensionEnabled("off", false)
	rr := httptest.NewRecorder()
	s.handleListExtensions(rr, httptest.NewRequest(http.MethodGet, "/api/extensions", nil))
	body := rr.Body.String()
	if !strings.Contains(body, `"on"`) || strings.Contains(body, `"off"`) {
		t.Fatalf("manifest should list only enabled extensions: %s", body)
	}
}
