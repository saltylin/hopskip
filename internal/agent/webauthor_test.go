package agent

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"testing/fstest"

	"github.com/saltylin/hopskip/internal/store"
)

// TestWebAuthoringTools exercises the agent's frontend self-programming tools:
// read the embedded base shell, write an overlay file that shadows it, register
// an extension, list both, then delete/unregister.
func TestWebAuthoringTools(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()

	// fake embedded base shell
	base := fstest.MapFS{
		"app.js":     {Data: []byte("// base shell")},
		"index.html": {Data: []byte("<html></html>")},
	}
	d := NewDispatcher(nil, st, http.DefaultClient, base)

	// read_web_file resolves the embedded base when no overlay exists
	out, isErr := d.Dispatch("read_web_file", json.RawMessage(`{"path":"app.js"}`))
	if isErr || !strings.Contains(out, "base shell") || !strings.Contains(out, `"source":"embed"`) {
		t.Fatalf("read_web_file(base) wrong: %s", out)
	}

	// write_web_file creates an overlay entry
	out, isErr = d.Dispatch("write_web_file", json.RawMessage(`{"path":"/ext/connect/entry.mjs","content":"export default {}"}`))
	if isErr || !strings.Contains(out, `"served_at":"/ext/connect/entry.mjs"`) {
		t.Fatalf("write_web_file failed: %s", out)
	}

	// overlay shadows / is readable
	out, _ = d.Dispatch("read_web_file", json.RawMessage(`{"path":"ext/connect/entry.mjs"}`))
	if !strings.Contains(out, "export default") || !strings.Contains(out, `"source":"overlay"`) {
		t.Fatalf("read_web_file(overlay) wrong: %s", out)
	}

	// path traversal is rejected
	if _, isErr := d.Dispatch("write_web_file", json.RawMessage(`{"path":"../secret","content":"x"}`)); !isErr {
		t.Fatal("path traversal must be rejected")
	}

	// list_web_files shows the overlay file AND the embedded base paths
	out, _ = d.Dispatch("list_web_files", json.RawMessage(`{}`))
	if !strings.Contains(out, "ext/connect/entry.mjs") || !strings.Contains(out, "app.js") {
		t.Fatalf("list_web_files missing entries: %s", out)
	}

	// register_extension pins a content hash of the entry module
	out, isErr = d.Dispatch("register_extension", json.RawMessage(
		`{"slug":"connect-pre","kind":"override","mount":"connect.steps","entry":"/ext/connect/entry.mjs","title":"connect pre-steps"}`))
	if isErr || !strings.Contains(out, `"content_hash":`) {
		t.Fatalf("register_extension failed: %s", out)
	}
	// reject a bad kind
	if _, isErr := d.Dispatch("register_extension", json.RawMessage(`{"slug":"x","kind":"bogus","mount":"m","entry":"/e.mjs"}`)); !isErr {
		t.Fatal("bad extension kind must be rejected")
	}

	// list_extensions surfaces it
	out, _ = d.Dispatch("list_extensions", json.RawMessage(`{}`))
	if !strings.Contains(out, "connect-pre") || !strings.Contains(out, "connect.steps") {
		t.Fatalf("list_extensions missing the registration: %s", out)
	}
	if exts, _ := st.ListExtensions(true); len(exts) != 1 || !exts[0].Enabled {
		t.Fatalf("expected 1 enabled extension, got %+v", exts)
	}

	// delete_web_file reverts the overlay (base shows through again if present)
	if _, isErr := d.Dispatch("delete_web_file", json.RawMessage(`{"path":"ext/connect/entry.mjs"}`)); isErr {
		t.Fatal("delete_web_file failed")
	}
	if _, _, ok := st.GetWebFile("ext/connect/entry.mjs"); ok {
		t.Fatal("overlay file should be gone after delete")
	}

	// unregister_extension removes the manifest row
	if _, isErr := d.Dispatch("unregister_extension", json.RawMessage(`{"slug":"connect-pre"}`)); isErr {
		t.Fatal("unregister_extension failed")
	}
	if exts, _ := st.ListExtensions(false); len(exts) != 0 {
		t.Fatalf("expected 0 extensions after unregister, got %d", len(exts))
	}
}
