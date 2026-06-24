package session

import (
	"io"

	"github.com/creack/pty"
)

// Attach is one browser client subscribed to a session's PTY. On attach it first
// replays the recent output ring (so a reconnecting browser sees the current
// screen), then streams live output. Writes go to the shared PTY; many browsers
// can attach to the same session and all see the same stream. Closing an Attach
// detaches that client WITHOUT killing the session.
type Attach struct {
	ls       *liveSession
	subID    int
	ch       chan []byte
	replay   []byte
	rpos     int
	detached bool
}

// Attach subscribes a browser client to session id with an initial geometry.
func (m *Manager) Attach(id string, rows, cols uint16) (*Attach, error) {
	ls, err := m.get(id)
	if err != nil {
		return nil, err
	}
	ls.mu.Lock()
	if ls.closed {
		ls.mu.Unlock()
		return nil, io.EOF
	}
	// Snapshot the ring and subscribe atomically: replay = output so far, channel
	// = output after now — no gap, no duplication.
	replay := append([]byte(nil), ls.ring...)
	ch := make(chan []byte, subBuf)
	subID := ls.nextID
	ls.nextID++
	ls.subs[subID] = ch
	ls.mu.Unlock()

	a := &Attach{ls: ls, subID: subID, ch: ch, replay: replay}
	if rows > 0 && cols > 0 {
		_ = a.Resize(rows, cols)
	}
	return a, nil
}

// Read returns the replay backlog first, then blocks for live output. Returns
// io.EOF once the session ends (its channel is closed).
func (a *Attach) Read(p []byte) (int, error) {
	if a.rpos < len(a.replay) {
		n := copy(p, a.replay[a.rpos:])
		a.rpos += n
		return n, nil
	}
	b, ok := <-a.ch
	if !ok {
		return 0, io.EOF
	}
	n := copy(p, b)
	// If the chunk is larger than p, stash the remainder back as replay.
	if n < len(b) {
		a.replay = b[n:]
		a.rpos = 0
	}
	return n, nil
}

// Write sends keystrokes/bytes from the browser into the shared PTY.
func (a *Attach) Write(p []byte) (int, error) { return a.ls.ptmx.Write(p) }

// Resize updates the PTY geometry and the VT mirror to match the browser.
func (a *Attach) Resize(rows, cols uint16) error {
	if rows == 0 || cols == 0 {
		return nil
	}
	a.ls.mu.Lock()
	a.ls.rows, a.ls.cols = rows, cols
	a.ls.mu.Unlock()
	a.ls.vt.Resize(int(cols), int(rows))
	return pty.Setsize(a.ls.ptmx, &pty.Winsize{Rows: rows, Cols: cols})
}

// Close detaches this client; the session and its shell keep running.
func (a *Attach) Close() error {
	if a.detached {
		return nil
	}
	a.detached = true
	a.ls.mu.Lock()
	if ch, ok := a.ls.subs[a.subID]; ok {
		delete(a.ls.subs, a.subID)
		close(ch)
	}
	a.ls.mu.Unlock()
	return nil
}
