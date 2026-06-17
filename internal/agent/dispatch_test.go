package agent

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/saltylin/hopskip/internal/store"
)

func TestFetchURLAndKnowledge(t *testing.T) {
	dlDir := t.TempDir()
	t.Setenv("HOPSKIP_DOWNLOAD_DIR", dlDir)
	st, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := NewDispatcher(nil, st, http.DefaultClient, nil)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/script.sh":
			w.Header().Set("Content-Type", "text/x-shellscript")
			_, _ = w.Write([]byte("#!/bin/sh\necho hello from k3s\n"))
		case "/bin":
			w.Header().Set("Content-Type", "application/octet-stream")
			_, _ = w.Write([]byte{0, 1, 2, 3, 255, 254, 0, 9})
		}
	}))
	defer srv.Close()

	// --- text fetch: returns inline text + a saved path under the download dir ---
	out, isErr := d.Dispatch("fetch_url", json.RawMessage(`{"url":"`+srv.URL+`/script.sh"}`))
	if isErr {
		t.Fatalf("text fetch errored: %s", out)
	}
	var res map[string]any
	_ = json.Unmarshal([]byte(out), &res)
	if txt, _ := res["text"].(string); !strings.Contains(txt, "hello from k3s") {
		t.Fatalf("expected inline text, got: %s", out)
	}
	if p, _ := res["path"].(string); !strings.HasPrefix(p, dlDir) {
		t.Fatalf("expected saved path under %s, got: %s", dlDir, out)
	}

	// --- binary fetch: no inline text, includes a transfer note ---
	out, isErr = d.Dispatch("fetch_url", json.RawMessage(`{"url":"`+srv.URL+`/bin","save_as":"k3s"}`))
	if isErr {
		t.Fatalf("bin fetch errored: %s", out)
	}
	res = map[string]any{}
	_ = json.Unmarshal([]byte(out), &res)
	if res["text"] != nil {
		t.Fatalf("binary should not include inline text: %s", out)
	}
	if res["note"] == nil {
		t.Fatalf("binary should include a transfer note: %s", out)
	}

	// --- bad url rejected ---
	if _, isErr := d.Dispatch("fetch_url", json.RawMessage(`{"url":"ftp://x"}`)); !isErr {
		t.Fatal("non-http url should be rejected")
	}

	// --- knowledge: remember -> list -> injected prompt -> forget ---
	d.Dispatch("remember", json.RawMessage(`{"text":"Aliyun hosts cannot reach overseas sites; use fetch_url + scp."}`))
	out, _ = d.Dispatch("list_knowledge", json.RawMessage(`{}`))
	if !strings.Contains(out, "Aliyun hosts cannot reach overseas") {
		t.Fatalf("list_knowledge missing the fact: %s", out)
	}
	kp := d.knowledgePrompt()
	if !strings.Contains(kp, "OPERATOR KNOWLEDGE") || !strings.Contains(kp, "Aliyun hosts cannot reach overseas") {
		t.Fatalf("knowledgePrompt missing the fact: %s", kp)
	}
	ks, _ := st.ListKnowledge()
	d.Dispatch("forget_knowledge", json.RawMessage(`{"id":`+strconv.FormatInt(ks[0].ID, 10)+`}`))
	if ks2, _ := st.ListKnowledge(); len(ks2) != 0 {
		t.Fatalf("expected knowledge cleared, got %d", len(ks2))
	}
}

// TestNotesReliability covers the two mechanisms that make per-host notes
// dependable: notes are injected into the model's context every turn, and an
// ssh toward a noted host surfaces a reminder at dispatch time.
func TestNotesReliability(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	d := NewDispatcher(nil, st, http.DefaultClient, nil)

	const note = "Everytime before ssh to this host, run 'mykinit' command in a local shell session first."
	id, err := st.AddHost("devbox", note)
	if err != nil {
		t.Fatal(err)
	}
	_ = st.TagHost(id, "provider", "aliyun")
	// a second, similarly-named host to guard against token false-positives
	if _, err := st.AddHost("devbox-staging", ""); err != nil {
		t.Fatal(err)
	}

	// 1) the note is injected into the system context (FLEET INVENTORY)
	inv := d.inventoryPrompt()
	if !strings.Contains(inv, "FLEET INVENTORY") || !strings.Contains(inv, "devbox") || !strings.Contains(inv, "mykinit") {
		t.Fatalf("inventoryPrompt missing host/note: %s", inv)
	}
	if !strings.Contains(inv, "provider=aliyun") {
		t.Fatalf("inventoryPrompt should include operator tags: %s", inv)
	}

	// 2) typing ssh toward devbox surfaces the pre-connect reminder (this is what
	// send_keys attaches to its result on success, in production)
	if rem := d.preConnectReminder("ssh devbox\n"); !strings.Contains(rem, "mykinit") || !strings.Contains(rem, "devbox") {
		t.Fatalf("expected a pre-connect reminder for devbox, got: %q", rem)
	}
	if rem := d.preConnectReminder("ssh root@devbox\n"); !strings.Contains(rem, "mykinit") {
		t.Fatalf("reminder should match user@host form, got: %q", rem)
	}

	// 3) no false positives: a non-ssh command, or a different host token
	if rem := d.preConnectReminder("ls -la\n"); rem != "" {
		t.Fatalf("non-ssh command should not trigger a reminder, got: %q", rem)
	}
	if rem := d.preConnectReminder("ssh devbox-staging\n"); rem != "" {
		t.Fatalf("'devbox' must not match the 'devbox-staging' token, got: %q", rem)
	}
}
