package server

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"

	"github.com/saltylin/hopskip/internal/agent"
	"github.com/saltylin/hopskip/internal/store"
)

// handleTermWS bridges a browser xterm.js terminal to a session's PTY attach.
//
// Frame protocol:
//   - browser -> server: binary frames are raw input bytes; text frames are
//     JSON control messages, currently {"type":"resize","cols":N,"rows":N}.
//   - server -> browser: binary frames are raw pane output.
func (s *Server) handleTermWS(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")

	rows := parseUint16(r.URL.Query().Get("rows"), 40)
	cols := parseUint16(r.URL.Query().Get("cols"), 120)

	att, err := s.mgr.Attach(id, rows, cols)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		att.Close()
		return
	}
	defer conn.Close()
	defer att.Close()

	// Keepalive: ping the browser periodically so an idle terminal connection
	// isn't dropped by the browser/OS/intermediaries. WriteControl is safe to
	// call concurrently with the message writer below. The browser auto-pongs.
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
					return
				}
			}
		}
	}()

	// PTY -> browser. Only this goroutine writes data frames to the websocket.
	go func() {
		buf := make([]byte, 32*1024)
		for {
			n, err := att.Read(buf)
			if n > 0 {
				if werr := conn.WriteMessage(websocket.BinaryMessage, buf[:n]); werr != nil {
					return
				}
			}
			if err != nil {
				_ = conn.WriteControl(websocket.CloseMessage,
					websocket.FormatCloseMessage(websocket.CloseNormalClosure, "session ended"),
					time.Now().Add(time.Second))
				_ = conn.Close()
				return
			}
		}
	}()

	// browser -> PTY (read side; never writes to the websocket).
	for {
		mt, data, err := conn.ReadMessage()
		if err != nil {
			return
		}
		switch mt {
		case websocket.BinaryMessage:
			if _, err := att.Write(data); err != nil {
				return
			}
		case websocket.TextMessage:
			var ctl struct {
				Type string `json:"type"`
				Cols uint16 `json:"cols"`
				Rows uint16 `json:"rows"`
			}
			if json.Unmarshal(data, &ctl) == nil && ctl.Type == "resize" {
				_ = att.Resize(ctl.Rows, ctl.Cols)
			}
		}
	}
}

// handleChatWS runs the agent loop for the chat window.
//
// browser -> server: {"type":"user_message","text":"..."}
// server -> browser: agent.Event objects ({"kind":"token","text":"..."} etc.)
func (s *Server) handleChatWS(w http.ResponseWriter, r *http.Request) {
	conn, err := s.upgrader.Upgrade(w, r, nil)
	if err != nil {
		return
	}
	defer conn.Close()

	// Keepalive: ping the browser periodically so an idle chat connection isn't
	// dropped by the browser/OS/intermediaries (which would grey out Send until a
	// refresh). WriteControl is safe concurrently with the recorder's writes.
	done := make(chan struct{})
	defer close(done)
	go func() {
		t := time.NewTicker(30 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-done:
				return
			case <-t.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(10*time.Second)); err != nil {
					return
				}
			}
		}
	}()

	// Agent runs go through a single worker goroutine so there is exactly one
	// WebSocket writer at a time, while the read loop below stays free to receive
	// a "stop". cancelCur cancels the in-flight run (the Stop button / a disconnect).
	type job struct{ chatID, text string }
	jobs := make(chan job, 8)
	var mu sync.Mutex
	var cancelCur context.CancelFunc
	cancelNow := func() {
		mu.Lock()
		if cancelCur != nil {
			cancelCur()
		}
		mu.Unlock()
	}
	go func() {
		for j := range jobs {
			ctx, cancel := context.WithCancel(context.Background())
			mu.Lock()
			cancelCur = cancel
			mu.Unlock()
			rec := &recorder{store: s.store, chatID: j.chatID, conn: conn}
			s.agent.ChatFor(j.chatID).Send(ctx, j.text, rec.emit)
			mu.Lock()
			cancelCur = nil
			mu.Unlock()
			cancel()
		}
	}()
	defer close(jobs) // ends the worker after the read loop returns

	for {
		_, data, err := conn.ReadMessage()
		if err != nil {
			cancelNow() // browser/socket gone — stop any in-flight run
			return
		}
		var msg struct {
			Type   string `json:"type"`
			Text   string `json:"text"`
			ChatID string `json:"chat_id"`
		}
		if json.Unmarshal(data, &msg) != nil {
			continue
		}
		switch msg.Type {
		case "stop":
			cancelNow()
		case "user_message":
			if msg.ChatID == "" {
				continue
			}
			// Persist the user turn, then queue the run keyed by its chat id (so it
			// keeps its in-memory context across reconnects). The recorder streams to
			// the browser and persists each transcript line.
			_ = s.store.EnsureChat(msg.ChatID)
			_ = s.store.AddChatMessage(msg.ChatID, "user", msg.Text)
			jobs <- job{chatID: msg.ChatID, text: msg.Text}
		}
	}
}

// recorder forwards transcript events to the browser and persists them. Assistant
// text is accumulated across token events and flushed as one message at the next
// boundary (a tool call, notice, error, or done).
type recorder struct {
	store  *store.Store
	chatID string
	conn   *websocket.Conn
	asst   strings.Builder
}

func (r *recorder) flushAssistant() {
	if r.asst.Len() > 0 {
		_ = r.store.AddChatMessage(r.chatID, "assistant", r.asst.String())
		r.asst.Reset()
	}
}

func (r *recorder) emit(ev agent.Event) {
	if err := r.conn.WriteJSON(ev); err != nil {
		log.Printf("chat ws write: %v", err)
	}
	switch ev.Kind {
	case "token":
		r.asst.WriteString(ev.Text)
	case "tool_use":
		r.flushAssistant()
		_ = r.store.AddChatMessage(r.chatID, "tool", ev.Text)
	case "tool_result":
		_ = r.store.AddChatMessage(r.chatID, "tool-result", ev.Text)
	case "notice":
		r.flushAssistant()
		_ = r.store.AddChatMessage(r.chatID, "notice", ev.Text)
	case "error":
		r.flushAssistant()
		_ = r.store.AddChatMessage(r.chatID, "error", ev.Text)
	case "done":
		r.flushAssistant()
	}
}

func parseUint16(s string, def uint16) uint16 {
	if s == "" {
		return def
	}
	var n int
	for _, c := range s {
		if c < '0' || c > '9' {
			return def
		}
		n = n*10 + int(c-'0')
		if n > 65535 {
			return def
		}
	}
	if n == 0 {
		return def
	}
	return uint16(n)
}
