// Package store is the SQLite persistence layer. The schema is intentionally
// schema-less for capabilities: hosts carry free-form key/value tags (operator
// asserted) and facts (model discovered). See CLAUDE.md §6.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite" // pure-Go driver, registered as "sqlite"
)

// ErrNotFound is returned when a host or session does not exist.
var ErrNotFound = errors.New("not found")

// Store wraps the SQLite database.
type Store struct {
	db *sql.DB
}

// Fact is a model-discovered key/value with its observation time.
type Fact struct {
	Value      string `json:"value"`
	ObservedAt string `json:"observed_at"`
}

// Host is the full inventory record for a host.
type Host struct {
	ID        int64             `json:"id"`
	Name      string            `json:"name"`
	Notes     string            `json:"notes"`
	CreatedAt string            `json:"created_at"`
	Tags      map[string]string `json:"tags"`
	Facts     map[string]Fact   `json:"facts"`
}

// Credential is a stored LLM provider API token. Token is never JSON-serialized
// (the API layer returns a masked form); it stays local and is only ever sent as
// the provider auth header.
type Credential struct {
	ID        int64  `json:"id"`
	Label     string `json:"label"`
	Provider  string `json:"provider"`
	Token     string `json:"-"`
	Model     string `json:"model"`
	Active    bool   `json:"active"`
	CreatedAt string `json:"created_at"`
}

// Session is a terminal session (tmux pane) record.
type Session struct {
	ID       string `json:"session_id"`
	HostID   *int64 `json:"host_id,omitempty"`
	TmuxName string `json:"tmux_name"`
	Label    string `json:"label"`
	Status   string `json:"status"`
	OpenedAt string `json:"opened_at"`
	ClosedAt string `json:"closed_at,omitempty"`
}

// Open opens (creating if needed) the SQLite file and ensures the schema.
func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1) // SQLite single-writer; keep it simple and safe.
	s := &Store{db: db}
	if err := s.migrate(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// Close closes the underlying database.
func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS hosts (
			id          INTEGER PRIMARY KEY,
			name        TEXT UNIQUE NOT NULL,
			notes       TEXT,
			created_at  TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS host_tags (
			host_id  INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
			key      TEXT NOT NULL,
			value    TEXT NOT NULL,
			PRIMARY KEY (host_id, key)
		)`,
		`CREATE TABLE IF NOT EXISTS host_facts (
			host_id     INTEGER NOT NULL REFERENCES hosts(id) ON DELETE CASCADE,
			key         TEXT NOT NULL,
			value       TEXT NOT NULL,
			observed_at TEXT NOT NULL,
			PRIMARY KEY (host_id, key)
		)`,
		`CREATE TABLE IF NOT EXISTS sessions (
			id         TEXT PRIMARY KEY,
			host_id    INTEGER REFERENCES hosts(id),
			tmux_name  TEXT NOT NULL,
			label      TEXT,
			status     TEXT NOT NULL,
			opened_at  TEXT NOT NULL,
			closed_at  TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS audit_log (
			id           INTEGER PRIMARY KEY,
			ts           TEXT NOT NULL,
			direction    TEXT NOT NULL,
			provider     TEXT NOT NULL,
			conversation TEXT,
			body_path    TEXT NOT NULL,
			meta         TEXT
		)`,
		`CREATE TABLE IF NOT EXISTS llm_credentials (
			id          INTEGER PRIMARY KEY,
			label       TEXT NOT NULL,
			provider    TEXT NOT NULL,           -- anthropic | openai
			token       TEXT NOT NULL,           -- secret; stays local, only sent as the provider auth header
			model       TEXT,
			active      INTEGER NOT NULL DEFAULT 0,
			created_at  TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS chats (
			id          TEXT PRIMARY KEY,
			title       TEXT NOT NULL DEFAULT '',
			created_at  TEXT NOT NULL,
			updated_at  TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS chat_messages (
			id          INTEGER PRIMARY KEY,
			chat_id     TEXT NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
			role        TEXT NOT NULL,           -- user|assistant|tool|tool-result|notice|error
			text        TEXT NOT NULL,
			created_at  TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS app_settings (
			key   TEXT PRIMARY KEY,
			value TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS knowledge (
			id         INTEGER PRIMARY KEY,
			text       TEXT NOT NULL,            -- durable operator-told fact/instruction
			created_at TEXT NOT NULL
		)`,
		// LLM/operator-authored frontend overlay. Stored in SQLite (not on disk)
		// so the whole authored UI migrates with the .db and authoring is a scoped
		// upsert with no filesystem path to traverse. Served above embed.FS, below
		// an optional HOPSKIP_WEB_DIR dev overlay. See spec/frontend-extension-protocol.md §2/§5.
		`CREATE TABLE IF NOT EXISTS web_files (
			path         TEXT PRIMARY KEY,        -- normalized, no leading slash (e.g. 'ext/connect/entry.mjs')
			content      BLOB NOT NULL,
			content_type TEXT,
			created_by   TEXT NOT NULL DEFAULT 'agent',
			updated_at   TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS web_extensions (
			id           INTEGER PRIMARY KEY,
			slug         TEXT UNIQUE NOT NULL,     -- id + grouping for its files
			kind         TEXT NOT NULL,            -- route | slot | override
			mount        TEXT NOT NULL,            -- route path | slot name | overridden region
			entry        TEXT NOT NULL,            -- module URL, e.g. '/ext/connect/entry.mjs'
			title        TEXT,
			sdk_range    TEXT,
			enabled      INTEGER NOT NULL DEFAULT 1,
			created_by   TEXT NOT NULL,            -- operator | agent
			created_at   TEXT NOT NULL,
			updated_at   TEXT,
			content_hash TEXT,
			notes        TEXT
		)`,
	}
	for _, q := range stmts {
		if _, err := s.db.Exec(q); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	return nil
}

func nowRFC3339() string { return time.Now().UTC().Format(time.RFC3339) }

// AddHost inserts a host and returns its id.
func (s *Store) AddHost(name, notes string) (int64, error) {
	res, err := s.db.Exec(
		`INSERT INTO hosts (name, notes, created_at) VALUES (?, ?, ?)`,
		name, notes, nowRFC3339())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// DeleteHost removes a host (and, via cascade, its tags/facts).
func (s *Store) DeleteHost(id int64) error {
	res, err := s.db.Exec(`DELETE FROM hosts WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// TagHost upserts an operator-asserted tag.
func (s *Store) TagHost(hostID int64, key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO host_tags (host_id, key, value) VALUES (?, ?, ?)
		 ON CONFLICT(host_id, key) DO UPDATE SET value = excluded.value`,
		hostID, key, value)
	return err
}

// DeleteTag removes an operator-asserted tag (idempotent).
func (s *Store) DeleteTag(hostID int64, key string) error {
	_, err := s.db.Exec(`DELETE FROM host_tags WHERE host_id = ? AND key = ?`, hostID, key)
	return err
}

// UpdateHost changes a host's display name and notes. A duplicate name surfaces
// as a UNIQUE constraint error.
func (s *Store) UpdateHost(id int64, name, notes string) error {
	res, err := s.db.Exec(`UPDATE hosts SET name = ?, notes = ? WHERE id = ?`, name, notes, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// RecordFact upserts a model-discovered fact, stamping observed_at = now.
func (s *Store) RecordFact(hostID int64, key, value string) error {
	_, err := s.db.Exec(
		`INSERT INTO host_facts (host_id, key, value, observed_at) VALUES (?, ?, ?, ?)
		 ON CONFLICT(host_id, key) DO UPDATE SET value = excluded.value, observed_at = excluded.observed_at`,
		hostID, key, value, nowRFC3339())
	return err
}

func (s *Store) loadTags(hostID int64) (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM host_tags WHERE host_id = ? ORDER BY key`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		out[k] = v
	}
	return out, rows.Err()
}

func (s *Store) loadFacts(hostID int64) (map[string]Fact, error) {
	rows, err := s.db.Query(`SELECT key, value, observed_at FROM host_facts WHERE host_id = ? ORDER BY key`, hostID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]Fact{}
	for rows.Next() {
		var k string
		var f Fact
		if err := rows.Scan(&k, &f.Value, &f.ObservedAt); err != nil {
			return nil, err
		}
		out[k] = f
	}
	return out, rows.Err()
}

func (s *Store) hydrate(h *Host) error {
	tags, err := s.loadTags(h.ID)
	if err != nil {
		return err
	}
	facts, err := s.loadFacts(h.ID)
	if err != nil {
		return err
	}
	h.Tags, h.Facts = tags, facts
	return nil
}

// ListHosts returns all hosts with their tags and facts.
func (s *Store) ListHosts() ([]Host, error) {
	rows, err := s.db.Query(`SELECT id, name, COALESCE(notes,''), created_at FROM hosts ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var hosts []Host
	for rows.Next() {
		var h Host
		if err := rows.Scan(&h.ID, &h.Name, &h.Notes, &h.CreatedAt); err != nil {
			return nil, err
		}
		hosts = append(hosts, h)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	for i := range hosts {
		if err := s.hydrate(&hosts[i]); err != nil {
			return nil, err
		}
	}
	return hosts, nil
}

// GetHost returns a single host by id.
func (s *Store) GetHost(id int64) (Host, error) {
	var h Host
	err := s.db.QueryRow(
		`SELECT id, name, COALESCE(notes,''), created_at FROM hosts WHERE id = ?`, id).
		Scan(&h.ID, &h.Name, &h.Notes, &h.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	if err != nil {
		return h, err
	}
	return h, s.hydrate(&h)
}

// CreateSession records an open session.
func (s *Store) CreateSession(id string, hostID *int64, tmuxName, label string) error {
	_, err := s.db.Exec(
		`INSERT INTO sessions (id, host_id, tmux_name, label, status, opened_at)
		 VALUES (?, ?, ?, ?, 'open', ?)`,
		id, hostID, tmuxName, label, nowRFC3339())
	return err
}

// GetSession returns a session by id.
func (s *Store) GetSession(id string) (Session, error) {
	var ss Session
	var hostID sql.NullInt64
	var closedAt sql.NullString
	err := s.db.QueryRow(
		`SELECT id, host_id, tmux_name, COALESCE(label,''), status, opened_at, closed_at
		 FROM sessions WHERE id = ?`, id).
		Scan(&ss.ID, &hostID, &ss.TmuxName, &ss.Label, &ss.Status, &ss.OpenedAt, &closedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return ss, ErrNotFound
	}
	if err != nil {
		return ss, err
	}
	if hostID.Valid {
		ss.HostID = &hostID.Int64
	}
	if closedAt.Valid {
		ss.ClosedAt = closedAt.String
	}
	return ss, nil
}

// ListSessions returns sessions, newest first.
func (s *Store) ListSessions() ([]Session, error) {
	rows, err := s.db.Query(
		`SELECT id, host_id, tmux_name, COALESCE(label,''), status, opened_at, closed_at
		 FROM sessions ORDER BY opened_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var ss Session
		var hostID sql.NullInt64
		var closedAt sql.NullString
		if err := rows.Scan(&ss.ID, &hostID, &ss.TmuxName, &ss.Label, &ss.Status, &ss.OpenedAt, &closedAt); err != nil {
			return nil, err
		}
		if hostID.Valid {
			ss.HostID = &hostID.Int64
		}
		if closedAt.Valid {
			ss.ClosedAt = closedAt.String
		}
		out = append(out, ss)
	}
	return out, rows.Err()
}

// CloseSession marks a session closed.
func (s *Store) CloseSession(id string) error {
	_, err := s.db.Exec(
		`UPDATE sessions SET status = 'closed', closed_at = ? WHERE id = ? AND status = 'open'`,
		nowRFC3339(), id)
	return err
}

// GetHostByName returns a single host by exact name.
func (s *Store) GetHostByName(name string) (Host, error) {
	var h Host
	err := s.db.QueryRow(
		`SELECT id, name, COALESCE(notes,''), created_at FROM hosts WHERE name = ?`, name).
		Scan(&h.ID, &h.Name, &h.Notes, &h.CreatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return h, ErrNotFound
	}
	if err != nil {
		return h, err
	}
	return h, s.hydrate(&h)
}

// DeleteFact removes a discovered fact (idempotent).
func (s *Store) DeleteFact(hostID int64, key string) error {
	_, err := s.db.Exec(`DELETE FROM host_facts WHERE host_id = ? AND key = ?`, hostID, key)
	return err
}

// InsertAudit appends one append-only audit row (one per LLM request/response).
func (s *Store) InsertAudit(direction, provider, conversation, bodyPath, meta string) error {
	_, err := s.db.Exec(
		`INSERT INTO audit_log (ts, direction, provider, conversation, body_path, meta)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		nowRFC3339(), direction, provider, conversation, bodyPath, meta)
	return err
}

// ---- LLM credentials ----

func scanCredential(sc interface{ Scan(...any) error }) (Credential, error) {
	var c Credential
	var model sql.NullString
	var active int
	err := sc.Scan(&c.ID, &c.Label, &c.Provider, &c.Token, &model, &active, &c.CreatedAt)
	if err != nil {
		return c, err
	}
	c.Model = model.String
	c.Active = active != 0
	return c, nil
}

// AddLLMCredential stores a provider token. The first credential becomes active.
func (s *Store) AddLLMCredential(label, provider, token, model string) (int64, error) {
	var count int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM llm_credentials`).Scan(&count)
	active := 0
	if count == 0 {
		active = 1
	}
	res, err := s.db.Exec(
		`INSERT INTO llm_credentials (label, provider, token, model, active, created_at)
		 VALUES (?, ?, ?, ?, ?, ?)`,
		label, provider, token, model, active, nowRFC3339())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListLLMCredentials returns all stored credentials, newest first.
func (s *Store) ListLLMCredentials() ([]Credential, error) {
	rows, err := s.db.Query(
		`SELECT id, label, provider, token, model, active, created_at
		 FROM llm_credentials ORDER BY id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Credential
	for rows.Next() {
		c, err := scanCredential(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ActiveLLMCredential returns the active credential, or ErrNotFound.
func (s *Store) ActiveLLMCredential() (Credential, error) {
	row := s.db.QueryRow(
		`SELECT id, label, provider, token, model, active, created_at
		 FROM llm_credentials WHERE active = 1 LIMIT 1`)
	c, err := scanCredential(row)
	if errors.Is(err, sql.ErrNoRows) {
		return c, ErrNotFound
	}
	return c, err
}

// SetActiveLLMCredential makes one credential active (and all others inactive).
func (s *Store) SetActiveLLMCredential(id int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`UPDATE llm_credentials SET active = 0`); err != nil {
		return err
	}
	res, err := tx.Exec(`UPDATE llm_credentials SET active = 1 WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return tx.Commit()
}

// UpdateLLMCredential updates label/model, and token only when token != "".
func (s *Store) UpdateLLMCredential(id int64, label, model, token string) error {
	if token != "" {
		_, err := s.db.Exec(`UPDATE llm_credentials SET label=?, model=?, token=? WHERE id=?`, label, model, token, id)
		return err
	}
	_, err := s.db.Exec(`UPDATE llm_credentials SET label=?, model=? WHERE id=?`, label, model, id)
	return err
}

// DeleteLLMCredential removes a credential; if it was active, promotes the newest.
func (s *Store) DeleteLLMCredential(id int64) error {
	if _, err := s.db.Exec(`DELETE FROM llm_credentials WHERE id = ?`, id); err != nil {
		return err
	}
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM llm_credentials WHERE active = 1`).Scan(&n)
	if n == 0 {
		_, _ = s.db.Exec(`UPDATE llm_credentials SET active = 1
			WHERE id = (SELECT id FROM llm_credentials ORDER BY id DESC LIMIT 1)`)
	}
	return nil
}

// HasLLMProvider reports whether any credential exists for a provider.
func (s *Store) HasLLMProvider(provider string) bool {
	var n int
	_ = s.db.QueryRow(`SELECT COUNT(*) FROM llm_credentials WHERE provider = ?`, provider).Scan(&n)
	return n > 0
}

// MarkStaleSessionsClosed closes any 'open' sessions whose tmux pane is gone.
// keep returns true for tmux names that still exist.
func (s *Store) MarkStaleSessionsClosed(keep func(tmuxName string) bool) error {
	rows, err := s.db.Query(`SELECT id, tmux_name FROM sessions WHERE status = 'open'`)
	if err != nil {
		return err
	}
	var stale []string
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			rows.Close()
			return err
		}
		if !keep(name) {
			stale = append(stale, id)
		}
	}
	rows.Close()
	for _, id := range stale {
		if err := s.CloseSession(id); err != nil {
			return err
		}
	}
	return nil
}
