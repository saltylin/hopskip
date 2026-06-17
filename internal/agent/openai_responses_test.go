package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/openai/openai-go"
	"github.com/openai/openai-go/option"

	"github.com/saltylin/hopskip/internal/store"
)

// TestOpenAIResponsesFallback proves that when a model rejects Chat Completions
// with the "not a chat model" 404, the runner transparently switches to the
// Responses API, chains turns with previous_response_id, runs a tool call, and
// streams the final text — all without a real OpenAI key.
func TestOpenAIResponsesFallback(t *testing.T) {
	var chatHits, respHits int
	var sawPrevID string

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			chatHits++
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusNotFound)
			_, _ = io.WriteString(w, `{"error":{"message":"This is not a chat model and thus not supported in the v1/chat/completions endpoint. Did you mean to use v1/completions?","type":"invalid_request_error","param":"model","code":null}}`)

		case strings.HasSuffix(r.URL.Path, "/responses"):
			respHits++
			body, _ := io.ReadAll(r.Body)
			var req map[string]any
			_ = json.Unmarshal(body, &req)
			w.Header().Set("Content-Type", "application/json")
			if prev, ok := req["previous_response_id"].(string); ok && prev != "" {
				// 2nd turn: the tool output came back; finish with text.
				sawPrevID = prev
				_, _ = io.WriteString(w, `{"id":"resp_2","object":"response","status":"completed","output":[{"type":"message","id":"msg_1","role":"assistant","status":"completed","content":[{"type":"output_text","text":"all good","annotations":[]}]}]}`)
				return
			}
			// 1st turn: ask for a tool call.
			_, _ = io.WriteString(w, `{"id":"resp_1","object":"response","status":"completed","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"list_knowledge","arguments":"{}","status":"completed"}]}`)

		default:
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusBadRequest)
		}
	}))
	defer srv.Close()

	st, err := store.Open(t.TempDir() + "/t.db")
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	_ = st.SetSetting("max_steps", "5")
	_ = st.SetSetting("max_retries", "0")

	a := &Agent{store: st, httpClient: http.DefaultClient}
	a.disp = NewDispatcher(nil, st, http.DefaultClient, nil)

	client := openai.NewClient(
		option.WithAPIKey("test-key"),
		option.WithBaseURL(srv.URL+"/"),
		option.WithHTTPClient(srv.Client()),
	)
	r := &openaiRunner{agent: a, client: client, model: "gpt-5.5-pro", disp: a.disp}

	var kinds []string
	var text, toolUse, notice string
	r.run(context.Background(), "are we ok?", func(e Event) {
		kinds = append(kinds, e.Kind)
		switch e.Kind {
		case "token":
			text += e.Text
		case "tool_use":
			toolUse += e.Text
		case "notice":
			notice += e.Text
		}
	})

	if chatHits != 1 {
		t.Fatalf("expected exactly 1 chat-completions hit (the rejected probe), got %d", chatHits)
	}
	if respHits != 2 {
		t.Fatalf("expected 2 responses hits (tool call + final), got %d", respHits)
	}
	if sawPrevID != "resp_1" {
		t.Fatalf("2nd responses turn must chain previous_response_id=resp_1, got %q", sawPrevID)
	}
	if strings.Contains(notice, "Responses-API") || strings.Contains(strings.ToLower(notice), "switching") {
		t.Fatalf("the chat->responses switch must be silent, but got notice %q", notice)
	}
	if !strings.Contains(toolUse, "list_knowledge") {
		t.Fatalf("expected a list_knowledge tool_use, got %q", toolUse)
	}
	if text != "all good" {
		t.Fatalf("expected final text 'all good', got %q", text)
	}
	if kinds[len(kinds)-1] != "done" {
		t.Fatalf("stream must end with done, got %v", kinds)
	}
	if !r.useResponses {
		t.Fatal("runner should have latched into responses mode")
	}

	// A second user message must go straight to /responses (no new chat probe).
	chatHits, respHits, sawPrevID = 0, 0, ""
	r.run(context.Background(), "still ok?", func(e Event) {})
	if chatHits != 0 {
		t.Fatalf("latched runner must not re-probe chat completions, got %d hits", chatHits)
	}
	if respHits < 1 {
		t.Fatalf("2nd message should use responses, got %d hits", respHits)
	}
	if sawPrevID != "resp_2" {
		t.Fatalf("2nd message should chain from resp_2 (prior turn), got %q", sawPrevID)
	}
}
