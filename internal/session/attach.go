package session

import (
	"os"
	"os/exec"

	"github.com/creack/pty"
)

// Attach is a live PTY running `tmux attach` against a session. It gives the
// browser a real interactive terminal: bytes read from it are pane output,
// bytes written to it are keystrokes. Closing it detaches the client WITHOUT
// killing the tmux session, so session state persists. The agent's SendKeys /
// Capture operate on the same tmux session concurrently — agent and human share
// the session, exactly as intended.
type Attach struct {
	ptmx *os.File
	cmd  *exec.Cmd
}

// Attach opens a PTY attached to the session's tmux. rows/cols set the initial
// terminal geometry (the session resizes to follow this client).
func (m *Manager) Attach(id string, rows, cols uint16) (*Attach, error) {
	name, err := m.tmuxNameFor(id)
	if err != nil {
		return nil, err
	}
	cmd := m.tmux("attach", "-t", name)
	cmd.Env = append(os.Environ(), "TERM=xterm-256color")
	winsize := &pty.Winsize{Rows: rows, Cols: cols}
	if rows == 0 || cols == 0 {
		winsize = &pty.Winsize{Rows: 40, Cols: 120}
	}
	ptmx, err := pty.StartWithSize(cmd, winsize)
	if err != nil {
		return nil, err
	}
	return &Attach{ptmx: ptmx, cmd: cmd}, nil
}

// Read reads pane output (for streaming to the browser).
func (a *Attach) Read(p []byte) (int, error) { return a.ptmx.Read(p) }

// Write sends keystrokes/bytes from the browser into the terminal.
func (a *Attach) Write(p []byte) (int, error) { return a.ptmx.Write(p) }

// Resize updates the terminal geometry; tmux resizes the session to match.
func (a *Attach) Resize(rows, cols uint16) error {
	if rows == 0 || cols == 0 {
		return nil
	}
	return pty.Setsize(a.ptmx, &pty.Winsize{Rows: rows, Cols: cols})
}

// Close detaches the client (kills the attach process) but leaves the session.
func (a *Attach) Close() error {
	_ = a.ptmx.Close()
	if a.cmd.Process != nil {
		_ = a.cmd.Process.Kill()
	}
	_ = a.cmd.Wait()
	return nil
}
