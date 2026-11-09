package store

import (
	"database/sql"
	"errors"
	"sort"
	"strings"
)

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// ---- P7 tool call ----

type ToolCall struct {
	ID        int64  `json:"id"`
	MessageID int64  `json:"message_id"`
	Tool      string `json:"tool"`
	Args      string `json:"args"`
	Result    string `json:"result"`
	Success   bool   `json:"success"`
	CreatedAt string `json:"created_at"`
}

func (s *UserStore) RecordToolCall(messageID int64, tool, args, result string, success bool) error {
	_, err := s.db.Exec(`INSERT INTO tool_calls (message_id, tool, args, result, success, created_at) VALUES (?,?,?,?,?,?)`,
		messageID, tool, args, result, boolInt(success), nowUTC())
	return err
}

func (s *UserStore) ToolCallsForMessage(messageID int64) ([]ToolCall, error) {
	rows, err := s.db.Query(`SELECT id, message_id, tool, args, result, success, created_at FROM tool_calls WHERE message_id = ? ORDER BY id`, messageID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ToolCall
	for rows.Next() {
		var tc ToolCall
		var ok int
		if err := rows.Scan(&tc.ID, &tc.MessageID, &tc.Tool, &tc.Args, &tc.Result, &ok, &tc.CreatedAt); err != nil {
			return nil, err
		}
		tc.Success = ok == 1
		out = append(out, tc)
	}
	return out, rows.Err()
}

// ---- P9 audit event ----

type AuditEvent struct {
	ID        int64  `json:"id"`
	Kind      string `json:"kind"`
	Detail    string `json:"detail"`
	CreatedAt string `json:"created_at"`
}

func (s *UserStore) RecordAudit(kind, detail string) error {
	_, err := s.db.Exec(`INSERT INTO audit_events (kind, detail, created_at) VALUES (?,?,?)`, kind, detail, nowUTC())
	return err
}

// ---- P10 skill (user-global) ----

type Skill struct {
	ID           int64   `json:"id"`
	Name         string  `json:"name"`
	Description  string  `json:"description"`
	Content      string  `json:"content"`
	CreatedAt    string  `json:"created_at"`
	UpdatedAt    string  `json:"updated_at"`
	MemoryRefs   []int64 `json:"memory_refs,omitempty"`
}

func (s *UserStore) CreateSkill(name, description, content string, memoryRefs []int64) (*Skill, error) {
	now := nowUTC()
	res, err := s.db.Exec(`INSERT INTO skills (name, description, content, created_at, updated_at) VALUES (?,?,?,?,?)`,
		name, description, content, now, now)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	for _, mid := range memoryRefs {
		if _, err := s.db.Exec(`INSERT OR IGNORE INTO skill_memory_refs (skill_id, memory_id) VALUES (?,?)`, id, mid); err != nil {
			return nil, err
		}
	}
	return &Skill{ID: id, Name: name, Description: description, Content: content, CreatedAt: now, UpdatedAt: now, MemoryRefs: memoryRefs}, nil
}

func (s *UserStore) ListSkills() ([]Skill, error) {
	rows, err := s.db.Query(`SELECT id, name, description, content, created_at, updated_at FROM skills ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Skill
	for rows.Next() {
		var sk Skill
		if err := rows.Scan(&sk.ID, &sk.Name, &sk.Description, &sk.Content, &sk.CreatedAt, &sk.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, sk)
	}
	return out, rows.Err()
}

func (s *UserStore) GetSkillByName(name string) (*Skill, error) {
	sk := &Skill{}
	err := s.db.QueryRow(`SELECT id, name, description, content, created_at, updated_at FROM skills WHERE name = ?`, name).
		Scan(&sk.ID, &sk.Name, &sk.Description, &sk.Content, &sk.CreatedAt, &sk.UpdatedAt)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	sk.MemoryRefs, err = s.GetSkillMemoryRefs(sk.ID)
	return sk, err
}

// GetSkillMemoryRefs returns the memory IDs a skill references.
func (s *UserStore) GetSkillMemoryRefs(skillID int64) ([]int64, error) {
	rows, err := s.db.Query(`SELECT memory_id FROM skill_memory_refs WHERE skill_id = ? ORDER BY memory_id`, skillID)
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

func (s *UserStore) UpdateSkill(name, description, content string) error {
	res, err := s.db.Exec(`UPDATE skills SET description = ?, content = ?, updated_at = ? WHERE name = ?`,
		description, content, nowUTC(), name)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- P11 workspace ----

type Workspace struct {
	ID          string    `json:"id"`
	Title       string    `json:"title"`
	Description string    `json:"description"`
	AgentNotes  string    `json:"agent_notes"`
	CreatedAt   string    `json:"created_at"`
	Size        int64     `json:"size"`
	Status      string    `json:"status"`
	FolderPath  string    `json:"folder_path"`
	Embedding   []float32 `json:"-"`
}

const wsCols = `id, title, description, agent_notes, created_at, size, status, folder_path, embedding`

func scanWS(rs interface{ Scan(...any) error }, w *Workspace) error {
	var emb []byte
	if err := rs.Scan(&w.ID, &w.Title, &w.Description, &w.AgentNotes, &w.CreatedAt, &w.Size, &w.Status, &w.FolderPath, &emb); err != nil {
		return err
	}
	if len(emb) > 0 {
		w.Embedding = blobEmbed(emb)
	}
	return nil
}

func (s *UserStore) CreateWorkspace(title, description, folderPath string) (*Workspace, error) {
	id := newID()
	now := nowUTC()
	_, err := s.db.Exec(`INSERT INTO workspaces (id, title, description, agent_notes, created_at, size, status, folder_path) VALUES (?,?,?,?,?,0,'active',?)`,
		id, title, description, "", now, folderPath)
	if err != nil {
		return nil, err
	}
	return &Workspace{ID: id, Title: title, Description: description, CreatedAt: now, Status: "active", FolderPath: folderPath}, nil
}

func (s *UserStore) GetWorkspace(id string) (*Workspace, error) {
	w := &Workspace{}
	err := scanWS(s.db.QueryRow(`SELECT `+wsCols+` FROM workspaces WHERE id = ?`, id), w)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	return w, nil
}

func (s *UserStore) ListWorkspaces() ([]Workspace, error) {
	rows, err := s.db.Query(`SELECT ` + wsCols + ` FROM workspaces ORDER BY created_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Workspace
	for rows.Next() {
		var w Workspace
		if err := scanWS(rows, &w); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// UpdateWorkspaceNotes updates the hidden agent_notes (agent-written, <=1000
// chars) used as a routing signal.
func (s *UserStore) UpdateWorkspaceNotes(id, notes string) error {
	_, err := s.db.Exec(`UPDATE workspaces SET agent_notes = ? WHERE id = ?`, notes, id)
	return err
}

func (s *UserStore) SetWorkspaceSize(id string, size int64) error {
	_, err := s.db.Exec(`UPDATE workspaces SET size = ? WHERE id = ?`, size, id)
	return err
}

// SetWorkspaceEmbedding stores the workspace routing vector (title+desc+notes).
func (s *UserStore) SetWorkspaceEmbedding(id string, emb []float32) error {
	var col any
	if emb != nil {
		col = embedBlob(emb)
	}
	_, err := s.db.Exec(`UPDATE workspaces SET embedding = ? WHERE id = ?`, col, id)
	return err
}

// ---- P12 workspace file ----

type WorkspaceFile struct {
	ID           int64   `json:"id"`
	WorkspaceID  string  `json:"workspace_id"`
	Name         string  `json:"name"`
	RelativePath string  `json:"relative_path"`
	Mime         *string `json:"mime,omitempty"`
	Size         int64   `json:"size"`
	Hash         *string `json:"hash,omitempty"`
	AddedAt      string  `json:"added_at"`
	Source       string  `json:"source"` // user | agent
	Kind         string  `json:"kind"`   // doc | asset
	Ingested     bool    `json:"ingested"`
}

const wfCols = `id, workspace_id, name, relative_path, mime, size, hash, added_at, source, kind, ingested`

func scanWF(rs interface{ Scan(...any) error }, f *WorkspaceFile) error {
	var ok int
	if err := rs.Scan(&f.ID, &f.WorkspaceID, &f.Name, &f.RelativePath, &f.Mime, &f.Size, &f.Hash, &f.AddedAt, &f.Source, &f.Kind, &ok); err != nil {
		return err
	}
	f.Ingested = ok == 1
	return nil
}

func (s *UserStore) AddWorkspaceFile(wsid, name, relPath, mime string, size int64, hash string, source, kind string) (*WorkspaceFile, error) {
	if source == "" {
		source = "user"
	}
	if kind == "" {
		kind = "doc"
	}
	now := nowUTC()
	var mimeCol, hashCol any
	if mime != "" {
		mimeCol = mime
	}
	if hash != "" {
		hashCol = hash
	}
	res, err := s.db.Exec(`INSERT INTO workspace_files (workspace_id, name, relative_path, mime, size, hash, added_at, source, kind) VALUES (?,?,?,?,?,?,?,?,?)`,
		wsid, name, relPath, mimeCol, size, hashCol, now, source, kind)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return &WorkspaceFile{ID: id, WorkspaceID: wsid, Name: name, RelativePath: relPath, Size: size, AddedAt: now, Source: source, Kind: kind}, nil
}

func (s *UserStore) ListWorkspaceFiles(wsid string) ([]WorkspaceFile, error) {
	rows, err := s.db.Query(`SELECT ` + wfCols + ` FROM workspace_files WHERE workspace_id = ? ORDER BY relative_path`, wsid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WorkspaceFile
	for rows.Next() {
		var f WorkspaceFile
		if err := scanWF(rows, &f); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

func (s *UserStore) SetWorkspaceFileIngested(id int64, ingested bool) error {
	_, err := s.db.Exec(`UPDATE workspace_files SET ingested = ? WHERE id = ?`, boolInt(ingested), id)
	return err
}

// DeleteWorkspaceFile removes a file's metadata (used on retraction into .dni).
func (s *UserStore) DeleteWorkspaceFile(id int64) error {
	_, err := s.db.Exec(`DELETE FROM workspace_files WHERE id = ?`, id)
	return err
}

// ---- P1 profile (durable, small, injected) ----

type ProfileKV struct {
	Key       string `json:"key"`
	Value     string `json:"value"`
	Source    string `json:"source"`
	UpdatedAt string `json:"updated_at"`
}

func (s *UserStore) SetProfile(key, value, source string) error {
	if source == "" {
		source = "learned"
	}
	_, err := s.db.Exec(`
		INSERT INTO profile (key, value, source, updated_at) VALUES (?,?,?,?)
		ON CONFLICT(key) DO UPDATE SET value = excluded.value, source = excluded.source, updated_at = excluded.updated_at`,
		key, value, source, nowUTC())
	return err
}

func (s *UserStore) GetProfile() (map[string]string, error) {
	rows, err := s.db.Query(`SELECT key, value FROM profile`)
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

// ProfileText assembles the profile into "key: value" lines (for injection
// into the system prompt).
func (s *UserStore) ProfileText() (string, error) {
	kv, err := s.GetProfile()
	if err != nil {
		return "", err
	}
	keys := make([]string, 0, len(kv))
	for k := range kv {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	for _, k := range keys {
		if kv[k] == "" {
			continue
		}
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(kv[k])
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n"), nil
}