package agent

import (
	"context"
	"net/http"
	"strings"
	"testing"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"

	"github.com/saltylin/hopskip/internal/store"
)

// TestStopCancelsRun proves the Stop path: when the run's context is cancelled
// (the operator hit Stop), the runner emits a clean "Stopped." notice + done —
// NOT a raw provider error — and stoppedByOperator distinguishes that cancel from
// a live context. The context is cancelled before the call so the outcome is
// deterministic (no reliance on mid-stream cancellation timing).
func TestStopCancelsRun(t *testing.T) {
	st, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.SetSetting("max_retries", "0")

	a := &Agent{store: st, httpClient: http.DefaultClient}
	a.disp = NewDispatcher(nil, st, http.DefaultClient, nil)
	client := openai.NewClient(
		option.WithAPIKey("k"),
		option.WithBaseURL("http://127.0.0.1:0/"), // never reached: ctx is already cancelled
		option.WithHTTPClient(http.DefaultClient),
	)
	r := &openaiRunner{agent: a, client: client, model: "gpt-4o", disp: a.disp}

	ctx, cancel := context.WithCancel(context.Background())
	cancel() // operator stop before the provider call

	var kinds, notices []string
	r.run(ctx, "hello", func(e Event) {
		kinds = append(kinds, e.Kind)
		if e.Kind == "notice" {
			notices = append(notices, e.Text)
		}
	})

	if len(kinds) == 0 || kinds[len(kinds)-1] != "done" {
		t.Fatalf("run must end with done, got %v", kinds)
	}
	for _, k := range kinds {
		if k == "error" {
			t.Fatalf("operator stop must NOT surface as an error: %v", kinds)
		}
	}
	if !strings.Contains(strings.Join(notices, " "), "Stopped") {
		t.Fatalf("expected a Stopped notice, got %v", notices)
	}

	// the helper distinguishes operator-cancel from a live context
	if !stoppedByOperator(ctx) {
		t.Fatal("stoppedByOperator should be true for a cancelled context")
	}
	if stoppedByOperator(context.Background()) {
		t.Fatal("stoppedByOperator should be false for a live context")
	}
}
