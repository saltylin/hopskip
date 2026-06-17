package store

import (
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"strings"
)

// ChatMeta is a saved conversation's header.
type ChatMeta struct {
	ID        string `json:"id"`
	Title     string `json:"title"`
	CreatedAt string `json:"created_at"`
	UpdatedAt string `json:"updated_at"`
}

// ChatMessage is one persisted transcript line (mirrors what the UI renders).
type ChatMessage struct {
	Role      string `json:"role"`
	Text      string `json:"text"`
	CreatedAt string `json:"created_at"`
}

func randChatID() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

func truncTitle(s string) string {
	s = strings.TrimSpace(strings.ReplaceAll(s, "\n", " "))
	if len(s) > 48 {
		s = s[:48] + "…"
	}
	return s
}

// CreateChat inserts a new empty chat and returns its id.
func (s *Store) CreateChat() (string, error) {
	id := randChatID()
	now := nowRFC3339()
	_, err := s.db.Exec(`INSERT INTO chats (id, title, created_at, updated_at) VALUES (?, '', ?, ?)`, id, now, now)
	return id, err
}

// EnsureChat creates a chat row if it doesn't already exist (idempotent).
func (s *Store) EnsureChat(id string) error {
	now := nowRFC3339()
	_, err := s.db.Exec(`INSERT OR IGNORE INTO chats (id, title, created_at, updated_at) VALUES (?, '', ?, ?)`, id, now, now)
	return err
}

// ListChats returns chats, most-recently-updated first.
func (s *Store) ListChats() ([]ChatMeta, error) {
	rows, err := s.db.Query(`SELECT id, title, created_at, updated_at FROM chats ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatMeta
	for rows.Next() {
		var c ChatMeta
		if err := rows.Scan(&c.ID, &c.Title, &c.CreatedAt, &c.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// DeleteChat removes a chat and its messages (cascade).
func (s *Store) DeleteChat(id string) error {
	_, err := s.db.Exec(`DELETE FROM chats WHERE id = ?`, id)
	return err
}

// RenameChat sets a chat's title.
func (s *Store) RenameChat(id, title string) error {
	_, err := s.db.Exec(`UPDATE chats SET title = ? WHERE id = ?`, title, id)
	return err
}

// AddChatMessage appends a transcript line, bumps updated_at, and derives the
// title from the first user message when the chat is still untitled.
func (s *Store) AddChatMessage(chatID, role, text string) error {
	now := nowRFC3339()
	if _, err := s.db.Exec(
		`INSERT INTO chat_messages (chat_id, role, text, created_at) VALUES (?, ?, ?, ?)`,
		chatID, role, text, now); err != nil {
		return err
	}
	_, _ = s.db.Exec(`UPDATE chats SET updated_at = ? WHERE id = ?`, now, chatID)
	if role == "user" {
		_, _ = s.db.Exec(`UPDATE chats SET title = ? WHERE id = ? AND (title IS NULL OR title = '')`,
			truncTitle(text), chatID)
	}
	return nil
}

// ListChatMessages returns a chat's transcript in order.
func (s *Store) ListChatMessages(chatID string) ([]ChatMessage, error) {
	rows, err := s.db.Query(
		`SELECT role, text, created_at FROM chat_messages WHERE chat_id = ? ORDER BY id`, chatID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChatMessage
	for rows.Next() {
		var m ChatMessage
		if err := rows.Scan(&m.Role, &m.Text, &m.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ---- app settings (key/value) ----

// GetSetting returns a setting value and whether it was set.
func (s *Store) GetSetting(key string) (string, bool) {
	var v string
	err := s.db.QueryRow(`SELECT value FROM app_settings WHERE key = ?`, key).Scan(&v)
	if err == sql.ErrNoRows {
		return "", false
	}
	if err != nil {
		return "", false
	}
	return v, true
}

// SetSetting upserts a setting value.
func (s *Store) SetSetting(key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO app_settings (key, value) VALUES (?, ?)
		 ON CONFLICT(key) DO UPDATE SET value = excluded.value`, key, value)
	return err
}
