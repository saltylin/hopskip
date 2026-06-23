// Package session owns the tmux-backed terminal sessions. One tmux session per
// Hopskip session, on a dedicated tmux socket so we never disturb the operator's
// own tmux. Reaching a remote host is always the operator/agent typing `ssh`
// into a session (CLAUDE.md invariant 1); nothing here opens a connection.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/saltylin/hopskip/internal/store"
)

// Manager creates and drives tmux sessions.
type Manager struct {
	tmuxBin  string
	socket   string // tmux -L <socket>
	store    *store.Store
	confPath string // sourced tmux config (chained bindings argv can't express)
}

// tmuxConf holds bindings that need command chaining (\;), which only parses in a
// sourced config file. On the wheel, clear any selection BEFORE scrolling: a held
// tmux selection is an active one, so scrolling would otherwise extend it ("drift").
// Clearing first keeps the selection put while you don't scroll, and removes it
// cleanly (no drift) the moment you do — the text was already copied on release.
const tmuxConf = `bind-key -T copy-mode    WheelUpPane   send-keys -X clear-selection \; send-keys -X -N 3 scroll-up
bind-key -T copy-mode    WheelDownPane send-keys -X clear-selection \; send-keys -X -N 3 scroll-down
bind-key -T copy-mode-vi WheelUpPane   send-keys -X clear-selection \; send-keys -X -N 3 scroll-up
bind-key -T copy-mode-vi WheelDownPane send-keys -X clear-selection \; send-keys -X -N 3 scroll-down
`

// NewManager returns a Manager bound to a tmux binary and the store.
func NewManager(tmuxBin string, st *store.Store) *Manager {
	if tmuxBin == "" {
		tmuxBin = "tmux"
	}
	m := &Manager{tmuxBin: tmuxBin, socket: "hopskip", store: st}
	m.confPath = filepath.Join(os.TempDir(), "hopskip-tmux.conf")
	_ = os.WriteFile(m.confPath, []byte(tmuxConf), 0o644)
	return m
}

// Available reports whether the tmux binary works.
func (m *Manager) Available() error {
	out, err := m.tmux("-V").CombinedOutput()
	if err != nil {
		return fmt.Errorf("tmux not usable (%s): %v: %s", m.tmuxBin, err, strings.TrimSpace(string(out)))
	}
	return nil
}

func (m *Manager) tmux(args ...string) *exec.Cmd {
	full := append([]string{"-L", m.socket}, args...)
	return exec.Command(m.tmuxBin, full...)
}

// run executes a tmux subcommand and returns trimmed combined output.
func (m *Manager) run(args ...string) (string, error) {
	out, err := m.tmux(args...).CombinedOutput()
	if err != nil {
		return strings.TrimSpace(string(out)), fmt.Errorf("tmux %s: %v: %s",
			strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Open creates a new detached tmux session and records it. It starts on the
// laptop; to reach a host the caller types `ssh ...` via SendKeys (or the
// operator types it in the browser terminal).
func (m *Manager) Open(label string, hostID *int64) (store.Session, error) {
	id := randHex(16)
	tmuxName := "hs_" + id[:10]
	if label == "" {
		label = tmuxName
	}
	// Create the session with a DEEP scrollback. With standard (alternate-screen)
	// tmux rendering the display stays clean and the wheel scrolls tmux's own
	// history — so scroll depth is tmux's history-limit, which must be set on the
	// server BEFORE the session is created. We do both in one invocation because
	// an empty tmux server exits between separate commands. (The smcup/status
	// "conduit" we tried corrupts the buffer: tmux repaints with cursor
	// addressing, which the browser's scrollback cannot reconcile — stale/missing
	// lines. So we DON'T disable the alt screen.)
	hist := strconv.Itoa(historyLimit(m.store))
	if _, err := m.run(
		"set-option", "-g", "history-limit", hist, ";",
		"new-session", "-d", "-s", tmuxName, "-x", "200", "-y", "50",
	); err != nil {
		return store.Session{}, err
	}
	// mouse ON gives tmux's native, reliable behavior: the wheel scrolls history
	// (no synthetic keys that could leak into the shell), and the selection is
	// anchored to the buffer (it doesn't drift when you scroll). The only tweaks
	// fix the two real annoyances of tmux's defaults:
	//   - drag-end HOLDS the selection (copy-selection-no-clear) instead of
	//     copy-and-cancel — so the highlight stays and the view does NOT jump to
	//     the bottom; the text is copied to the system clipboard via OSC 52
	//     (set-clipboard on + the advertised Ms capability).
	//   - a plain click: at the live bottom it exits copy-mode (resume typing);
	//     when SCROLLED UP it only clears the selection and STAYS put — so a click
	//     never yanks the view down to the bottom. (Scrolling back to the bottom
	//     also resumes typing.)
	// Standard alt-screen rendering keeps the display clean.
	_, _ = m.run("set-option", "-g", "mouse", "on")
	_, _ = m.run("set-option", "-g", "set-clipboard", "on")
	_, _ = m.run("set-option", "-g", "terminal-overrides", `,*:Ms=\E]52;%p1%s;%p2%s\7`)
	for _, tbl := range []string{"copy-mode", "copy-mode-vi"} {
		_, _ = m.run("bind-key", "-T", tbl, "MouseDragEnd1Pane", "send-keys", "-X", "copy-selection-no-clear")
		_, _ = m.run("bind-key", "-T", tbl, "MouseUp1Pane",
			"if-shell", "-F", "#{==:#{scroll_position},0}",
			"send-keys -X cancel", "send-keys -X clear-selection")
	}
	// wheel bindings that clear the selection before scrolling (need \; chaining)
	if m.confPath != "" {
		_, _ = m.run("source-file", m.confPath)
	}
	if err := m.store.CreateSession(id, hostID, tmuxName, label); err != nil {
		_, _ = m.run("kill-session", "-t", tmuxName) // best-effort rollback
		return store.Session{}, err
	}
	return m.store.GetSession(id)
}

// historyLimit is tmux's scrollback depth (lines), from the operator-tunable
// terminal_scrollback setting (the same knob the browser terminal uses), else a
// generous default.
func historyLimit(st *store.Store) int {
	if v, ok := st.GetSetting("terminal_scrollback"); ok {
		if n, err := strconv.Atoi(strings.TrimSpace(v)); err == nil && n >= 100 {
			return n
		}
	}
	return 100000
}

// tmuxNameFor resolves the live tmux session name for an open session id.
func (m *Manager) tmuxNameFor(id string) (string, error) {
	ss, err := m.store.GetSession(id)
	if err != nil {
		return "", err
	}
	if ss.Status != "open" {
		return "", fmt.Errorf("session %s is %s", id, ss.Status)
	}
	return ss.TmuxName, nil
}

// Close kills the tmux session and marks the record closed.
func (m *Manager) Close(id string) error {
	ss, err := m.store.GetSession(id)
	if err != nil {
		return err
	}
	_, _ = m.run("kill-session", "-t", ss.TmuxName) // ignore: pane may already be gone
	return m.store.CloseSession(id)
}

// Capture returns the visible pane contents. lines>0 limits to the last N lines.
func (m *Manager) Capture(id string, lines int) (string, error) {
	name, err := m.tmuxNameFor(id)
	if err != nil {
		return "", err
	}
	args := []string{"capture-pane", "-p", "-J", "-t", name}
	if lines > 0 {
		args = append(args, "-S", "-"+strconv.Itoa(lines))
	}
	out, err := m.tmux(args...).Output()
	if err != nil {
		return "", fmt.Errorf("capture-pane: %w", err)
	}
	return normalizeScreen(string(out)), nil
}

// normalizeScreen tidies a captured pane for tool results: right-trims each line,
// drops leading/trailing blank lines (tmux pads the capture to the full pane
// height, producing the long run of empty lines at the bottom), and collapses any
// internal run of 2+ blank lines to one. This is display-and-model hygiene only —
// the live terminal stream uses a separate PTY path and is untouched. (CLAUDE.md
// MCP spec §6.5: screen normalization SHOULD.)
func normalizeScreen(s string) string {
	lines := strings.Split(s, "\n")
	for i := range lines {
		lines[i] = strings.TrimRight(lines[i], " \t")
	}
	start := 0
	for start < len(lines) && lines[start] == "" {
		start++
	}
	end := len(lines)
	for end > start && lines[end-1] == "" {
		end--
	}
	out := make([]string, 0, end-start)
	blank := 0
	for _, l := range lines[start:end] {
		if l == "" {
			if blank++; blank > 1 {
				continue
			}
		} else {
			blank = 0
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// sendText types text into a pane, translating '\n' into Enter keypresses.
func (m *Manager) sendText(name, text string) error {
	parts := strings.Split(text, "\n")
	for i, p := range parts {
		if p != "" {
			if _, err := m.run("send-keys", "-t", name, "-l", "--", p); err != nil {
				return err
			}
		}
		if i < len(parts)-1 {
			if _, err := m.run("send-keys", "-t", name, "Enter"); err != nil {
				return err
			}
		}
	}
	return nil
}

var sentinelLine = regexp.MustCompile(`__HOPSKIP_DONE_`)

// SendKeys types keystrokes into a session. With awaitDone it wraps the command
// in a done-sentinel and blocks until the sentinel resolves or timeout elapses
// (see CLAUDE.md §5.3). With awaitDone=false it sends verbatim and returns at
// once — use that for interactive prompts and long-running commands.
func (m *Manager) SendKeys(id, keys string, awaitDone bool, timeoutMs int) (screen string, exitCode *int, completed bool, err error) {
	name, err := m.tmuxNameFor(id)
	if err != nil {
		return "", nil, false, err
	}
	if !awaitDone {
		if err := m.sendText(name, keys); err != nil {
			return "", nil, false, err
		}
		screen, err = m.Capture(id, 0)
		return screen, nil, false, err
	}

	nonce := randHex(6)
	cmd := strings.TrimSuffix(keys, "\n")
	line := fmt.Sprintf(`%s; echo "__HOPSKIP_DONE_$?_%s__"`, cmd, nonce)
	if err := m.sendText(name, line); err != nil {
		return "", nil, false, err
	}
	if _, err := m.run("send-keys", "-t", name, "Enter"); err != nil {
		return "", nil, false, err
	}

	re := regexp.MustCompile(`__HOPSKIP_DONE_(\d+)_` + nonce + `__`)
	if timeoutMs <= 0 {
		timeoutMs = 30000
	}
	deadline := time.Now().Add(time.Duration(timeoutMs) * time.Millisecond)
	for {
		screen, err = m.Capture(id, 0)
		if err != nil {
			return "", nil, false, err
		}
		if mm := re.FindStringSubmatch(screen); mm != nil {
			code, _ := strconv.Atoi(mm[1])
			return stripSentinel(screen), &code, true, nil
		}
		if !time.Now().Before(deadline) {
			return screen, nil, false, nil // timed out; do not kill the command
		}
		time.Sleep(100 * time.Millisecond)
	}
}

// stripSentinel removes sentinel artifact lines (both the echoed command line
// and the resolved output line) so callers see clean output.
func stripSentinel(screen string) string {
	lines := strings.Split(screen, "\n")
	out := lines[:0]
	for _, l := range lines {
		if sentinelLine.MatchString(l) {
			continue
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// LiveTmuxNames returns the set of tmux session names currently alive.
func (m *Manager) LiveTmuxNames() map[string]bool {
	out, err := m.tmux("list-sessions", "-F", "#{session_name}").Output()
	live := map[string]bool{}
	if err != nil {
		return live // no server / no sessions
	}
	for _, l := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		if l != "" {
			live[l] = true
		}
	}
	return live
}
