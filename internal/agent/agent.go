// Package agent is the LLM agent loop (CLAUDE.md §8). It streams a transcript to
// the browser, dispatches tool_use through the single dispatch() choke point, and
// feeds results back until the model stops.
//
// Providers are pluggable: each chat is bound to the active stored credential
// (Anthropic or OpenAI) and talks to that provider directly, through the
// operator's network proxy — which is where auditing happens (the daemon no
// longer runs its own audit proxy). Tokens are stored locally (SQLite) and only
// ever leave as the provider auth header.
package agent

import (
	"context"
	"io/fs"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/saltylin/hopskip/internal/session"
	"github.com/saltylin/hopskip/internal/store"
)

const turnTimeout = 5 * time.Minute // per-LLM-call wall-clock budget

// DefaultMaxSteps is the built-in step budget, used until one is set in Settings
// (the "max_steps" key). It is not an env var — operator config lives in the DB.
func DefaultMaxSteps() int { return 512 }

// MaxSteps is the tool-use round-trip budget per turn actually used now (stored
// setting, else the env/default).
func (a *Agent) MaxSteps() int {
	if v, ok := a.store.GetSetting("max_steps"); ok {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			return n
		}
	}
	return DefaultMaxSteps()
}

// DefaultMaxRetries is the built-in retry count, used until one is set in Settings
// (the "max_retries" key). Retries use the SDK's exponential backoff, which honors
// the provider's Retry-After header; the per-call timeout bounds total wait.
func DefaultMaxRetries() int { return 8 }

// MaxRetries is the provider-request retry count actually used now (stored
// setting, else the env/default).
func (a *Agent) MaxRetries() int {
	if v, ok := a.store.GetSetting("max_retries"); ok {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return n
		}
	}
	return DefaultMaxRetries()
}

// Event is one streamed transcript event sent to the browser.
type Event struct {
	Kind string `json:"kind"` // token | tool_use | tool_result | notice | error | done | thinking | usage
	Text string `json:"text,omitempty"`
	In   int    `json:"in,omitempty"`  // input/prompt tokens (usage events only)
	Out  int    `json:"out,omitempty"` // output/completion tokens (usage events only)
}

// friendlyErr turns a raw provider error into operator-facing text, adding
// guidance for the common rate-limit (429) case.
func friendlyErr(err error) string {
	s := err.Error()
	ls := strings.ToLower(s)
	if strings.Contains(ls, "429") || strings.Contains(ls, "rate limit") || strings.Contains(ls, "rate_limit") {
		return "⏳ Provider rate limit (429) after automatic retries. Wait a moment and resend, or raise “max_retries” in Settings → General.\n" + s
	}
	return s
}

// runner is one provider-bound chat session holding native message history.
type runner interface {
	run(ctx context.Context, userMessage string, emit func(Event))
}

// Agent builds chats. It holds the shared HTTP client (routed through the
// operator's network proxy) and the tool dispatcher.
type Agent struct {
	store      *store.Store
	disp       *Dispatcher
	httpClient *http.Client

	mu    sync.Mutex
	chats map[string]*Chat
}

// New builds the agent. webFS is the embedded base shell, so the web-authoring
// tools can read the shell the agent overrides (may be nil). The network proxy is
// not configured here — it comes from the "upstream_proxy" Setting (empty =
// direct), resolved per request.
func New(mgr *session.Manager, st *store.Store, webFS fs.FS) *Agent {
	a := &Agent{
		store: st,
		chats: map[string]*Chat{},
	}
	// Proxy is resolved per request so changing it in Settings takes effect with no
	// restart: the stored upstream_proxy wins; unset ⇒ a direct connection.
	tr := &http.Transport{ForceAttemptHTTP2: true}
	tr.Proxy = func(*http.Request) (*url.URL, error) {
		p := strings.TrimSpace(a.currentProxy())
		if p == "" {
			return nil, nil
		}
		return url.Parse(p)
	}
	a.httpClient = &http.Client{Transport: tr} // no Timeout: responses stream
	// The dispatcher shares this proxy-aware client for the fetch_url tool, so a
	// network-restricted host's downloads go through the operator's laptop/proxy.
	a.disp = NewDispatcher(mgr, st, a.httpClient, webFS)
	return a
}

// currentProxy returns the operator's network proxy from Settings; unset ⇒ "" (direct).
func (a *Agent) currentProxy() string {
	if v, ok := a.store.GetSetting("upstream_proxy"); ok {
		return v
	}
	return ""
}

// DefaultProxy is the built-in proxy default — none (a direct connection) until
// one is set in Settings.
func (a *Agent) DefaultProxy() string { return "" }

// CurrentProxy is the proxy actually used for provider calls right now.
func (a *Agent) CurrentProxy() string { return a.currentProxy() }

// Configured reports whether an active credential exists.
func (a *Agent) Configured() bool {
	_, err := a.store.ActiveLLMCredential()
	return err == nil
}

// ActiveInfo returns the active provider + model (ok=false if none).
func (a *Agent) ActiveInfo() (provider, model string, ok bool) {
	c, err := a.store.ActiveLLMCredential()
	if err != nil {
		return "", "", false
	}
	return c.Provider, c.Model, true
}

// Chat is one browser conversation. It (re)builds a provider runner whenever the
// active credential changes, so switching providers in Settings takes effect on
// the next message (starting a fresh history for the new provider).
type Chat struct {
	agent *Agent
	r     runner
	rKey  string
	mu    sync.Mutex
}

// NewChat starts a fresh chat.
func (a *Agent) NewChat() *Chat { return &Chat{agent: a} }

// ChatFor returns the in-memory chat for id, creating it if needed. Keeping the
// chat (and its provider runner's native history) in memory gives continuity
// across reconnects/refreshes within a daemon run; the transcript is persisted
// separately to SQLite for listing, viewing, and surviving restarts.
func (a *Agent) ChatFor(id string) *Chat {
	a.mu.Lock()
	defer a.mu.Unlock()
	c, ok := a.chats[id]
	if !ok {
		c = a.NewChat()
		a.chats[id] = c
	}
	return c
}

// DropChat forgets an in-memory chat (called when a chat is deleted).
func (a *Agent) DropChat(id string) {
	a.mu.Lock()
	delete(a.chats, id)
	a.mu.Unlock()
}

// Send handles one operator message, streaming transcript events. emit must be
// called from the caller's goroutine (the sole WebSocket writer).
func (c *Chat) Send(ctx context.Context, userMessage string, emit func(Event)) {
	c.mu.Lock()
	defer c.mu.Unlock()
	cred, err := c.agent.store.ActiveLLMCredential()
	if err != nil {
		emit(Event{Kind: "notice", Text: "🛈 No API token set. Open Settings, add an Anthropic or OpenAI token, and pick one as active — then chat again."})
		emit(Event{Kind: "done"})
		return
	}
	key := cred.Provider + "|" + cred.Token + "|" + cred.Model
	if c.r == nil || c.rKey != key {
		c.r = c.agent.newRunner(cred)
		c.rKey = key
	}
	c.r.run(ctx, userMessage, emit)
}

func (a *Agent) newRunner(cred store.Credential) runner {
	switch cred.Provider {
	case "openai":
		return newOpenAIRunner(a, cred)
	default:
		return newAnthropicRunner(a, cred)
	}
}
