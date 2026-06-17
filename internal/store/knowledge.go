package store

// Knowledge is a durable operator-told fact/instruction the agent should carry
// across all chats (injected into the system prompt each turn).
type Knowledge struct {
	ID        int64  `json:"id"`
	Text      string `json:"text"`
	CreatedAt string `json:"created_at"`
}

// AddKnowledge stores a remembered fact and returns its id.
func (s *Store) AddKnowledge(text string) (int64, error) {
	res, err := s.db.Exec(`INSERT INTO knowledge (text, created_at) VALUES (?, ?)`, text, nowRFC3339())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ListKnowledge returns all remembered facts, oldest first.
func (s *Store) ListKnowledge() ([]Knowledge, error) {
	rows, err := s.db.Query(`SELECT id, text, created_at FROM knowledge ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Knowledge
	for rows.Next() {
		var k Knowledge
		if err := rows.Scan(&k.ID, &k.Text, &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// DeleteKnowledge removes a remembered fact (idempotent).
func (s *Store) DeleteKnowledge(id int64) error {
	_, err := s.db.Exec(`DELETE FROM knowledge WHERE id = ?`, id)
	return err
}
