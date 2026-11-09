package store

import (
	"database/sql"
	"errors"
)

// Memory is a P5 row (a single learned fact; embedded + FTS5-indexed).
type Memory struct {
	ID           int64     `json:"id"`
	Facet        string    `json:"facet"`
	Statement    string    `json:"statement"`
	Subject      *string   `json:"subject,omitempty"`
	Confidence   float64   `json:"confidence"`
	Status       string    `json:"status"`
	SupersededBy *int64    `json:"superseded_by,omitempty"`
	Provenance   string    `json:"provenance"`
	CreatedAt    string    `json:"created_at"`
	UpdatedAt    string    `json:"updated_at"`
	Embedding    []float32 `json:"-"`
}

const memCols = `id, facet, statement, subject, confidence, status, superseded_by, provenance, created_at, updated_at, embedding`

func scanMem(rs interface{ Scan(...any) error }, m *Memory) error {
	var emb []byte
	if err := rs.Scan(&m.ID, &m.Facet, &m.Statement, &m.Subject, &m.Confidence, &m.Status, &m.SupersededBy, &m.Provenance, &m.CreatedAt, &m.UpdatedAt, &emb); err != nil {
		return err
	}
	if len(emb) > 0 {
		m.Embedding = blobEmbed(emb)
	}
	return nil
}

// CreateMemory stores a fact. subject/provenance may be empty; emb may be nil.
func (s *UserStore) CreateMemory(facet, statement string, subject *string, confidence float64, provenance string, emb []float32) (*Memory, error) {
	if confidence <= 0 {
		confidence = 1.0
	}
	now := nowUTC()
	var embCol any
	if emb != nil {
		embCol = embedBlob(emb)
	}
	res, err := s.db.Exec(
		`INSERT INTO memories (facet, statement, subject, confidence, status, provenance, created_at, updated_at, embedding)
		 VALUES (?,?,?,?, 'active', ?,?,?,?)`,
		facet, statement, subject, confidence, provenance, now, now, embCol)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &Memory{ID: id, Facet: facet, Statement: statement, Subject: subject, Confidence: confidence, Status: "active", Provenance: provenance, CreatedAt: now, UpdatedAt: now, Embedding: emb}, nil
}

func (s *UserStore) GetMemory(id int64) (*Memory, error) {
	m := &Memory{}
	err := scanMem(s.db.QueryRow(`SELECT `+memCols+` FROM memories WHERE id = ?`, id), m)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return m, nil
}

// ListActiveMemories returns every active memory, with embeddings (for
// in-memory cosine ranking). The per-user set is kept small by consolidation.
func (s *UserStore) ListActiveMemories() ([]Memory, error) {
	rows, err := s.db.Query(`SELECT ` + memCols + ` FROM memories WHERE status = 'active'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		var m Memory
		if err := scanMem(rows, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SearchMemories returns active memories matching an FTS5 query, ranked by
// bm25 (lexical recall). query must be valid FTS5 syntax.
func (s *UserStore) SearchMemories(query string, limit int) ([]Memory, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := s.db.Query(`
		SELECT ` + memCols + `
		FROM memories
		WHERE status = 'active'
		  AND EXISTS (SELECT 1 FROM memories_fts WHERE memories_fts.rowid = memories.id AND memories_fts MATCH ?)
		ORDER BY id
		LIMIT ?`, query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Memory
	for rows.Next() {
		var m Memory
		if err := scanMem(rows, &m); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// SupersedeMemory marks a memory superseded by another (supersession, not
// deletion).
func (s *UserStore) SupersedeMemory(id, supersededBy int64) error {
	res, err := s.db.Exec(`UPDATE memories SET status = 'superseded', superseded_by = ?, updated_at = ? WHERE id = ? AND status = 'active'`,
		supersededBy, nowUTC(), id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// SetMemoryConfidence updates a memory's confidence.
func (s *UserStore) SetMemoryConfidence(id int64, confidence float64) error {
	_, err := s.db.Exec(`UPDATE memories SET confidence = ?, updated_at = ? WHERE id = ?`, confidence, nowUTC(), id)
	return err
}

// SetMemoryProvenance replaces the set of messages that a memory was derived
// from.
func (s *UserStore) SetMemoryProvenance(memoryID int64, messageIDs []int64) error {
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if _, err := tx.Exec(`DELETE FROM memory_provenance WHERE memory_id = ?`, memoryID); err != nil {
		return err
	}
	for _, mid := range messageIDs {
		if _, err := tx.Exec(`INSERT OR IGNORE INTO memory_provenance (memory_id, message_id) VALUES (?,?)`, memoryID, mid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// MemoryProvenance returns the message IDs a memory was derived from.
func (s *UserStore) MemoryProvenance(memoryID int64) ([]int64, error) {
	rows, err := s.db.Query(`SELECT message_id FROM memory_provenance WHERE memory_id = ? ORDER BY message_id`, memoryID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []int64
	for rows.Next() {
		var mid int64
		if err := rows.Scan(&mid); err != nil {
			return nil, err
		}
		out = append(out, mid)
	}
	return out, rows.Err()
}