// Package session owns the terminal sessions. Each session is a shell running on
// a daemon-owned PTY (no tmux): the browser's xterm.js attaches directly, so it
// owns native selection + scrollback exactly like a local terminal. A server-side
// VT emulator (vt10x) mirrors each PTY so the agent's read_screen sees a faithful
// rendered screen. Reaching a remote host is always the operator/agent typing
// `ssh` into a session (CLAUDE.md invariant 1); nothing here opens a connection.
package session

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/creack/pty"
	"github.com/hinshun/vt10x"

	"github.com/saltylin/hopskip/internal/store"
)

const (
	defaultRows    = 50
	defaultCols    = 200
	ringCap        = 512 * 1024 // recent raw output kept for reconnect replay
	subBuf         = 256        // per-attach output backlog (localhost: never overflows)
	defaultTimeout = 30000      // send_keys await default (ms)
)

// liveSession is one shell+PTY owned by the daemon, mirrored into a VT emulator.
type liveSession struct {
	id   string
	ptmx *os.File
	cmd  *exec.Cmd
	vt   vt10x.Terminal // fed all PTY output; serialized by read_screen

	mu     sync.Mutex
	ring   []byte              // recent raw output (capped) for reconnect replay
	subs   map[int]chan []byte // attached browser clients
	nextID int
	closed bool
	rows   uint16
	cols   uint16
}

// Manager creates and drives PTY sessions.
type Manager struct {
	store *store.Store
	shell string

	mu   sync.Mutex
	live map[string]*liveSession
}

// NewManager returns a Manager. It uses the operator's $SHELL (else /bin/bash,
// else /bin/sh) for new sessions.
func NewManager(st *store.Store) *Manager {
	shell := os.Getenv("SHELL")
	if shell == "" {
		for _, c := range []string{"/bin/bash", "/bin/sh"} {
			if _, err := os.Stat(c); err == nil {
				shell = c
				break
			}
		}
	}
	return &Manager{store: st, shell: shell, live: map[string]*liveSession{}}
}

// Available reports whether a shell is usable.
func (m *Manager) Available() error {
	if m.shell == "" {
		return fmt.Errorf("no shell found (set $SHELL)")
	}
	if _, err := os.Stat(m.shell); err != nil {
		return fmt.Errorf("shell %s not usable: %w", m.shell, err)
	}
	return nil
}

func randHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// Open spawns a shell on a fresh PTY and records the session. It starts on the
// laptop; to reach a host the caller types `ssh ...` via SendKeys (or the
// operator types it in the browser terminal).
func (m *Manager) Open(label string, hostID *int64) (store.Session, error) {
	id := randHex(16)
	if label == "" {
		label = "session-" + id[:6]
	}
	cmd := exec.Command(m.shell)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	ws := &pty.Winsize{Rows: defaultRows, Cols: defaultCols}
	ptmx, err := pty.StartWithSize(cmd, ws)
	if err != nil {
		return store.Session{}, fmt.Errorf("start shell: %w", err)
	}
	ls := &liveSession{
		id:   id,
		ptmx: ptmx,
		cmd:  cmd,
		vt:   vt10x.New(vt10x.WithSize(defaultCols, defaultRows)),
		subs: map[int]chan []byte{},
		rows: defaultRows,
		cols: defaultCols,
	}
	m.mu.Lock()
	m.live[id] = ls
	m.mu.Unlock()
	go m.pump(ls)

	name := fmt.Sprintf("pty:%d", cmd.Process.Pid)
	if err := m.store.CreateSession(id, hostID, name, label); err != nil {
		_ = m.kill(ls)
		return store.Session{}, err
	}
	return m.store.GetSession(id)
}

// pump streams PTY output into the VT mirror, the replay ring, and every attached
// browser. On EOF (shell exited) it tears the session down.
func (m *Manager) pump(ls *liveSession) {
	buf := make([]byte, 32*1024)
	for {
		n, err := ls.ptmx.Read(buf)
		if n > 0 {
			b := buf[:n]
			_, _ = ls.vt.Write(b)
			ls.mu.Lock()
			ls.ring = append(ls.ring, b...)
			if len(ls.ring) > ringCap {
				ls.ring = ls.ring[len(ls.ring)-ringCap:]
			}
			for _, ch := range ls.subs {
				select {
				case ch <- append([]byte(nil), b...):
				default: // localhost browser fell behind; reconnect replay recovers it
				}
			}
			ls.mu.Unlock()
		}
		if err != nil {
			m.teardown(ls)
			return
		}
	}
}

// teardown marks the session closed and notifies attached browsers (closing their
// channels makes Attach.Read return io.EOF).
func (m *Manager) teardown(ls *liveSession) {
	ls.mu.Lock()
	if ls.closed {
		ls.mu.Unlock()
		return
	}
	ls.closed = true
	for id, ch := range ls.subs {
		close(ch)
		delete(ls.subs, id)
	}
	ls.mu.Unlock()
	m.mu.Lock()
	delete(m.live, ls.id)
	m.mu.Unlock()
	_ = m.store.CloseSession(ls.id)
}

func (m *Manager) kill(ls *liveSession) error {
	_ = ls.ptmx.Close()
	if ls.cmd.Process != nil {
		_ = ls.cmd.Process.Kill()
	}
	_, _ = ls.cmd.Process.Wait()
	return nil
}

func (m *Manager) get(id string) (*liveSession, error) {
	m.mu.Lock()
	ls := m.live[id]
	m.mu.Unlock()
	if ls == nil {
		return nil, fmt.Errorf("session %s is not open", id)
	}
	return ls, nil
}

// Close kills the shell and marks the session closed.
func (m *Manager) Close(id string) error {
	ls, err := m.get(id)
	if err != nil {
		// already gone from the live map; ensure the DB reflects closed
		return m.store.CloseSession(id)
	}
	_ = m.kill(ls)
	m.teardown(ls)
	return nil
}

// CloseAllInDB marks every 'open' session closed — used on daemon startup, since
// PTYs do not survive a restart (the live map starts empty).
func (m *Manager) CloseAllInDB() error {
	return m.store.MarkStaleSessionsClosed(func(string) bool { return false })
}

// Capture returns the current rendered screen of the session's VT mirror (the
// agent's read_screen). lines>0 trims to the last N visible lines.
func (m *Manager) Capture(id string, lines int) (string, error) {
	ls, err := m.get(id)
	if err != nil {
		return "", err
	}
	screen := normalizeScreen(ls.vt.String())
	if lines > 0 {
		ll := strings.Split(screen, "\n")
		if len(ll) > lines {
			screen = strings.Join(ll[len(ll)-lines:], "\n")
		}
	}
	return screen, nil
}

// normalizeScreen tidies a rendered VT screen for tool results: right-trims each
// line, drops leading/trailing blank lines (the grid is padded to the full pane
// height), and collapses any internal run of 2+ blank lines to one. (CLAUDE.md
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

var sentinelLine = regexp.MustCompile(`__HOPSKIP_DONE_`)

// SendKeys types keystrokes into a session by writing to its PTY ('\n' becomes a
// carriage return, like a real Enter). With awaitDone it wraps the command in a
// done-sentinel and blocks until the sentinel resolves on screen or the timeout
// elapses (CLAUDE.md §5.3). With awaitDone=false it writes verbatim and returns
// at once — use that for interactive prompts and long-running commands.
func (m *Manager) SendKeys(id, keys string, awaitDone bool, timeoutMs int) (screen string, exitCode *int, completed bool, err error) {
	ls, err := m.get(id)
	if err != nil {
		return "", nil, false, err
	}
	if !awaitDone {
		if _, err := ls.ptmx.Write([]byte(toCR(keys))); err != nil {
			return "", nil, false, err
		}
		time.Sleep(120 * time.Millisecond) // let the shell echo/render
		screen, err = m.Capture(id, 0)
		return screen, nil, false, err
	}

	nonce := randHex(6)
	cmd := strings.TrimSuffix(keys, "\n")
	line := fmt.Sprintf("%s; echo \"__HOPSKIP_DONE_$?_%s__\"\n", cmd, nonce)
	if _, err := ls.ptmx.Write([]byte(toCR(line))); err != nil {
		return "", nil, false, err
	}

	re := regexp.MustCompile(`__HOPSKIP_DONE_(\d+)_` + nonce + `__`)
	if timeoutMs <= 0 {
		timeoutMs = defaultTimeout
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

// toCR maps '\n' (the tool's Enter convention) to '\r', what a real Enter sends.
func toCR(s string) string { return strings.ReplaceAll(s, "\n", "\r") }

// Remote reports whether the session is currently logged into a remote host —
// i.e. there is a live `ssh` process anywhere in the session shell's process
// subtree. When you exit back to the laptop shell, the ssh process is gone and
// this returns false. (Only the FIRST hop runs on the laptop; deeper hops run on
// the intermediate hosts — but one ssh in the local tree is enough to mean
// "you're on a remote host".)
func (m *Manager) Remote(id string) (bool, error) {
	ls, err := m.get(id)
	if err != nil {
		return false, err
	}
	if ls.cmd.Process == nil {
		return false, nil
	}
	return sshInSubtree(ls.cmd.Process.Pid), nil
}

// sshInSubtree reports whether an `ssh` process runs under root, from a live
// `ps` snapshot. The parsing/walk is in sshInPsOutput so it can be unit-tested.
func sshInSubtree(root int) bool {
	out, err := exec.Command("ps", "-axo", "pid=,ppid=,comm=").Output()
	if err != nil {
		return false
	}
	return sshInPsOutput(string(out), root)
}

// sshInPsOutput walks the process tree under root (parsed from `ps -axo
// pid=,ppid=,comm=` output) looking for a process whose command basename is
// exactly "ssh" (so "sshd"/"ssh-agent"/"scp" do not match). macOS `comm` is a
// full path, Linux is the bare name — baseName handles both.
func sshInPsOutput(psout string, root int) bool {
	children := map[int][]int{}
	comm := map[int]string{}
	for _, line := range strings.Split(psout, "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		pid, e1 := strconv.Atoi(f[0])
		ppid, e2 := strconv.Atoi(f[1])
		if e1 != nil || e2 != nil {
			continue
		}
		comm[pid] = strings.Join(f[2:], " ")
		children[ppid] = append(children[ppid], pid)
	}
	queue := []int{root}
	seen := map[int]bool{root: true}
	for len(queue) > 0 {
		pid := queue[0]
		queue = queue[1:]
		for _, c := range children[pid] {
			if seen[c] {
				continue
			}
			seen[c] = true
			if baseName(comm[c]) == "ssh" {
				return true
			}
			queue = append(queue, c)
		}
	}
	return false
}

// baseName returns the last path segment of a command (macOS `ps comm` gives a
// full path; Linux gives the bare name).
func baseName(s string) string {
	if i := strings.LastIndexByte(s, '/'); i >= 0 {
		return s[i+1:]
	}
	return s
}

// stripSentinel removes the sentinel artifact lines (the echoed command and the
// resolved output line) so callers see clean output.
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
