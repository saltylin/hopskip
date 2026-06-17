package store

import (
	"crypto/sha256"
	"encoding/hex"
	"strings"
)

// WebFile is one authored frontend asset (metadata; content fetched separately).
type WebFile struct {
	Path        string `json:"path"`
	ContentType string `json:"content_type"`
	Size        int    `json:"size"`
	CreatedBy   string `json:"created_by"`
	UpdatedAt   string `json:"updated_at"`
}

// NormalizeWebPath canonicalizes a served path to the storage key: no leading
// slash, forward slashes, no '.'/'..' segments. Returns ok=false for a traversal
// attempt or an empty result.
func NormalizeWebPath(p string) (string, bool) {
	p = strings.TrimSpace(p)
	p = strings.ReplaceAll(p, "\\", "/")
	p = strings.TrimLeft(p, "/")
	if p == "" {
		return "", false
	}
	parts := strings.Split(p, "/")
	out := make([]string, 0, len(parts))
	for _, seg := range parts {
		if seg == "" || seg == "." {
			continue
		}
		if seg == ".." {
			return "", false // never allow traversal
		}
		out = append(out, seg)
	}
	if len(out) == 0 {
		return "", false
	}
	return strings.Join(out, "/"), true
}

// PutWebFile upserts an authored asset. createdBy is provenance ('agent'|'operator').
func (s *Store) PutWebFile(path string, content []byte, contentType, createdBy string) error {
	if createdBy == "" {
		createdBy = "agent"
	}
	_, err := s.db.Exec(
		`INSERT INTO web_files (path, content, content_type, created_by, updated_at)
		 VALUES (?, ?, ?, ?, ?)
		 ON CONFLICT(path) DO UPDATE SET content=excluded.content,
		     content_type=excluded.content_type, created_by=excluded.created_by,
		     updated_at=excluded.updated_at`,
		path, content, contentType, createdBy, nowRFC3339())
	return err
}

// GetWebFile returns the stored content + content_type for path, ok=false if absent.
func (s *Store) GetWebFile(path string) ([]byte, string, bool) {
	var content []byte
	var ct string
	err := s.db.QueryRow(`SELECT content, COALESCE(content_type,'') FROM web_files WHERE path = ?`, path).
		Scan(&content, &ct)
	if err != nil {
		return nil, "", false
	}
	return content, ct, true
}

// ListWebFiles returns metadata for all authored assets (no content), path-sorted.
func (s *Store) ListWebFiles() ([]WebFile, error) {
	rows, err := s.db.Query(
		`SELECT path, COALESCE(content_type,''), LENGTH(content), created_by, updated_at
		 FROM web_files ORDER BY path`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WebFile
	for rows.Next() {
		var f WebFile
		if err := rows.Scan(&f.Path, &f.ContentType, &f.Size, &f.CreatedBy, &f.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// DeleteWebFile removes an authored asset (reverting that path to embed/disk).
func (s *Store) DeleteWebFile(path string) error {
	_, err := s.db.Exec(`DELETE FROM web_files WHERE path = ?`, path)
	return err
}

// Extension is one row of the frontend manifest.
type Extension struct {
	ID          int64  `json:"id"`
	Slug        string `json:"slug"`
	Kind        string `json:"kind"`  // route | slot | override
	Mount       string `json:"mount"` // route path | slot name | region
	Entry       string `json:"entry"` // module URL
	Title       string `json:"title"`
	SDKRange    string `json:"sdk_range"`
	Enabled     bool   `json:"enabled"`
	CreatedBy   string `json:"created_by"`
	CreatedAt   string `json:"created_at"`
	UpdatedAt   string `json:"updated_at"`
	ContentHash string `json:"content_hash"`
	Notes       string `json:"notes"`
}

// UpsertExtension registers (or updates) an extension by slug.
func (s *Store) UpsertExtension(e Extension) error {
	if e.CreatedBy == "" {
		e.CreatedBy = "agent"
	}
	now := nowRFC3339()
	_, err := s.db.Exec(
		`INSERT INTO web_extensions (slug, kind, mount, entry, title, sdk_range, enabled, created_by, created_at, updated_at, content_hash, notes)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
		 ON CONFLICT(slug) DO UPDATE SET kind=excluded.kind, mount=excluded.mount, entry=excluded.entry,
		     title=excluded.title, sdk_range=excluded.sdk_range, enabled=excluded.enabled,
		     updated_at=excluded.updated_at, content_hash=excluded.content_hash, notes=excluded.notes`,
		e.Slug, e.Kind, e.Mount, e.Entry, e.Title, e.SDKRange, boolToInt(e.Enabled), e.CreatedBy, now, now, e.ContentHash, e.Notes)
	return err
}

// ListExtensions returns manifest rows; enabledOnly filters to enabled=1.
func (s *Store) ListExtensions(enabledOnly bool) ([]Extension, error) {
	q := `SELECT id, slug, kind, mount, entry, COALESCE(title,''), COALESCE(sdk_range,''),
	             enabled, created_by, created_at, COALESCE(updated_at,''), COALESCE(content_hash,''), COALESCE(notes,'')
	      FROM web_extensions`
	if enabledOnly {
		q += ` WHERE enabled = 1`
	}
	q += ` ORDER BY slug`
	rows, err := s.db.Query(q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Extension
	for rows.Next() {
		var e Extension
		var en int
		if err := rows.Scan(&e.ID, &e.Slug, &e.Kind, &e.Mount, &e.Entry, &e.Title, &e.SDKRange,
			&en, &e.CreatedBy, &e.CreatedAt, &e.UpdatedAt, &e.ContentHash, &e.Notes); err != nil {
			return nil, err
		}
		e.Enabled = en != 0
		out = append(out, e)
	}
	return out, rows.Err()
}

// SetExtensionEnabled toggles one extension without deleting it.
func (s *Store) SetExtensionEnabled(slug string, enabled bool) error {
	_, err := s.db.Exec(`UPDATE web_extensions SET enabled=?, updated_at=? WHERE slug=?`,
		boolToInt(enabled), nowRFC3339(), slug)
	return err
}

// DeleteExtension removes a manifest row (its files, if any, are removed separately).
func (s *Store) DeleteExtension(slug string) error {
	_, err := s.db.Exec(`DELETE FROM web_extensions WHERE slug = ?`, slug)
	return err
}

// HashContent returns the hex sha256 of content, for content_hash pinning.
func HashContent(b []byte) string {
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
