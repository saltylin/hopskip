package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/saltylin/hopskip/internal/agent"
)

// settingDef describes one operator-tunable config. Add a new entry here (plus
// wherever the value is consumed) to expose a new setting in the UI — no env var
// needed. The pattern is uniform: the stored value wins, the env/code default is
// the fallback shown until set.
type settingDef struct {
	Key         string
	Label       string
	Type        string // "text" | "number"
	Help        string
	Placeholder string
	Default     func(*Server) string // value used when unset (env/code default)
	Effective   func(*Server) string // value actually in effect right now
	Validate    func(string) error   // optional; "" is always allowed (= use default)
}

func (s *Server) settingDefs() []settingDef {
	return []settingDef{
		{
			Key:         "upstream_proxy",
			Label:       "Network proxy (HTTP/HTTPS)",
			Type:        "text",
			Help:        "Every provider (LLM) call routes through this proxy — that's where call auditing happens. Blank ⇒ a direct connection (the default).",
			Placeholder: "blank = direct · e.g. http://host:port",
			Default:     func(s *Server) string { return s.agent.DefaultProxy() },
			Effective:   func(s *Server) string { return s.agent.CurrentProxy() },
		},
		{
			Key:         "max_steps",
			Label:       "Max agent steps per turn",
			Type:        "number",
			Help:        "How many tool-use round-trips the agent may take in one turn before the runaway guard stops it.",
			Placeholder: strconv.Itoa(agent.DefaultMaxSteps()),
			Default:     func(s *Server) string { return strconv.Itoa(agent.DefaultMaxSteps()) },
			Effective:   func(s *Server) string { return strconv.Itoa(s.agent.MaxSteps()) },
			Validate: func(v string) error {
				if n, err := strconv.Atoi(v); err != nil || n <= 0 {
					return fmt.Errorf("must be a positive whole number")
				}
				return nil
			},
		},
		{
			Key:         "max_retries",
			Label:       "LLM request retries",
			Type:        "number",
			Help:        "Times to retry a provider request on a rate limit (429) or transient error, using exponential backoff that honors the provider's Retry-After. Bounded by the per-call timeout.",
			Placeholder: strconv.Itoa(agent.DefaultMaxRetries()),
			Default:     func(s *Server) string { return strconv.Itoa(agent.DefaultMaxRetries()) },
			Effective:   func(s *Server) string { return strconv.Itoa(s.agent.MaxRetries()) },
			Validate: func(v string) error {
				if n, err := strconv.Atoi(v); err != nil || n < 0 {
					return fmt.Errorf("must be a whole number of 0 or more")
				}
				return nil
			},
		},
		{
			Key:         "providers",
			Label:       "Cloud providers",
			Type:        "text",
			Help:        "Comma-separated list offered in the host forms and as topology filters (e.g. aliyun, aliyun-hk, bandwagon). Free text is still allowed when tagging a host.",
			Placeholder: defaultProviders,
			Default:     func(s *Server) string { return defaultProviders },
			Effective:   func(s *Server) string { return strings.Join(s.providerList(), ", ") },
		},
		{
			Key:         "terminal_scrollback",
			Label:       "Terminal scrollback (lines)",
			Type:        "number",
			Help:        "How far back the terminal can scroll (tmux history depth). Higher keeps more output, using more memory. Applies to terminals opened after the change.",
			Placeholder: "100000",
			Default:     func(s *Server) string { return strconv.Itoa(defaultScrollback) },
			Effective:   func(s *Server) string { return strconv.Itoa(s.terminalScrollback()) },
			Validate: func(v string) error {
				if n, err := strconv.Atoi(v); err != nil || n < 100 {
					return fmt.Errorf("must be a whole number of 100 or more")
				}
				return nil
			},
		},
		{
			Key:         "web_export_dir",
			Label:       "Frontend export directory",
			Type:        "text",
			Help:        "Where the “Save to disk” button commits the agent-authored frontend: it writes the files here, serves them from here, and removes them from the pending list. Blank uses ./web_overlay.",
			Placeholder: "web_overlay",
			Default:     func(s *Server) string { return s.defaultWebExportDir() },
			Effective:   func(s *Server) string { return s.webExportDir() },
		},
	}
}

// defaultWebExportDir is the committed-overlay directory used when web_export_dir
// is unset. It is intentionally distinct from HOPSKIP_WEB_DIR (the dev override):
// committed files are served BELOW pending DB edits, so a fresh authored change
// still wins until it too is saved.
func (s *Server) defaultWebExportDir() string {
	return "web_overlay"
}

// webExportDir is the directory the "Save to disk" button writes to.
func (s *Server) webExportDir() string {
	if v, ok := s.store.GetSetting("web_export_dir"); ok && strings.TrimSpace(v) != "" {
		return strings.TrimSpace(v)
	}
	return s.defaultWebExportDir()
}

const defaultProviders = "aliyun, aliyun-hk, bandwagon"

// defaultScrollback is the browser-terminal scrollback used when unset. Large by
// design: with the native-scrollback terminal, history lives in xterm's buffer,
// so this is the real "how far back can I scroll" limit.
const defaultScrollback = 100000

// terminalScrollback returns the configured browser-terminal scrollback (lines).
func (s *Server) terminalScrollback() int {
	if v, ok := s.store.GetSetting("terminal_scrollback"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 100 {
			return n
		}
	}
	return defaultScrollback
}

// providerList returns the configured cloud providers (stored setting, else default).
func (s *Server) providerList() []string {
	raw, ok := s.store.GetSetting("providers")
	if !ok || strings.TrimSpace(raw) == "" {
		raw = defaultProviders
	}
	out := []string{}
	for _, p := range strings.Split(raw, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

func (s *Server) handleProviders(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]any{"providers": s.providerList()})
}

func (s *Server) handleListSettings(w http.ResponseWriter, r *http.Request) {
	defs := s.settingDefs()
	out := make([]map[string]any, 0, len(defs))
	for _, d := range defs {
		stored, isSet := s.store.GetSetting(d.Key)
		out = append(out, map[string]any{
			"key":         d.Key,
			"label":       d.Label,
			"type":        d.Type,
			"help":        d.Help,
			"placeholder": d.Placeholder,
			"value":       stored, // raw stored value ("" when unset, or explicitly blank)
			"is_set":      isSet,
			"default":     d.Default(s),
			"effective":   d.Effective(s),
		})
	}
	writeJSON(w, http.StatusOK, map[string]any{"settings": out})
}

func (s *Server) handleSetSetting(w http.ResponseWriter, r *http.Request) {
	key := r.PathValue("key")
	var def *settingDef
	defs := s.settingDefs()
	for i := range defs {
		if defs[i].Key == key {
			def = &defs[i]
			break
		}
	}
	if def == nil {
		writeErr(w, http.StatusNotFound, "unknown setting")
		return
	}
	var req struct {
		Value string `json:"value"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeErr(w, http.StatusBadRequest, "bad json")
		return
	}
	v := strings.TrimSpace(req.Value)
	if v != "" && def.Validate != nil { // "" always allowed → falls back to default
		if err := def.Validate(v); err != nil {
			writeErr(w, http.StatusBadRequest, err.Error())
			return
		}
	}
	if err := s.store.SetSetting(key, v); err != nil {
		writeErr(w, http.StatusInternalServerError, err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"ok": true})
}
