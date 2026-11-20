package server

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"mime"
	"net/http"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/utopiafallen/bevin/internal/auth"
	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/store"
)

// Workspaces are the user's working folders on the server. Each is a
// real directory in the user's data dir —
//
//	<data>/users/<handle>/workspaces/<id>/
//
// created with a .SKILLS/ subfolder (per-workspace agent skills) and a
// .dni/ (Do Not Index — the opt-out from search/ingestion). Because the
// user's environment maps their data dir into it (/data on linux,
// C:\data on windows), the workspace shows up inside the user's
// environment at <data dir>/workspaces/<id>: it is the user's persistent
// working state. There is deliberately NO user-facing execution
// surface (user directive): the AGENT manages the user's environment —
// ensuring it and running commands in workspaces via the sandbox
// backend's ExecIn — and the user focuses on workspace state.
//
// File management is host-side (bevin writes the user's own data dir
// directly — no container needed); execution goes through the
// environment (the agent's path), so a server without the container
// runtime can still manage workspace files. Files are tracked in the
// workspace_files table (metadata: name, path, size, hash, source,
// kind) when they arrive through the API — files that appear on disk
// some other way (e.g. the agent ran a git clone in the user's
// environment) are visible in the listing (it walks the disk) but gain
// no metadata row.

// wsMaxUploadBytes caps a single upload (the per-workspace global cap is
// the server config's MaxWorkspaceSize; this keeps one request bounded).
// A var (not a const) so the tests can shrink it.
var wsMaxUploadBytes int64 = 256 << 20

// wsWalkLimit caps how many entries a workspace walk returns (a personal
// server will never get close; this is a runaway-symlink/clone guard).
const wsWalkLimit = 100000

// wsEntry is one file or directory in a workspace listing (relative,
// slash-separated paths).
type wsEntry struct {
	Name string `json:"name"`
	Path string `json:"path"`
	Size int64  `json:"size,omitempty"`
	Mod  string `json:"modified_at,omitempty"`
	Dir  bool   `json:"dir,omitempty"`
}

// workspaceUser resolves the session user for a workspace route.
func (a *App) workspaceUser(r *http.Request) *store.User {
	return auth.UserFromContext(r.Context())
}

// workspaceFolder returns the workspace's on-disk folder (creating the
// workspaces root on the way).
func (a *App) workspaceFolder(u *store.User, w *store.Workspace) string {
	if w.FolderPath != "" {
		return w.FolderPath
	}
	return filepath.Join(a.dataDir, "users", u.Handle, "workspaces", w.ID)
}

// workspaceStore opens the user's store (the agent's cached connection).
func (a *App) workspaceStore(w http.ResponseWriter, r *http.Request) (*store.User, *store.UserStore, bool) {
	u := a.workspaceUser(r)
	us, err := a.ag.UserStoreFor(u.Handle)
	if err != nil {
		log.Printf("workspace store: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil, nil, false
	}
	return u, us, true
}

// workspaceFor resolves the {id} path segment to the user's workspace.
func (a *App) workspaceFor(w http.ResponseWriter, r *http.Request, us *store.UserStore) (*store.Workspace, bool) {
	ws, err := us.GetWorkspace(r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "workspace not found"})
		return nil, false
	}
	if err != nil {
		log.Printf("workspace get: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return nil, false
	}
	return ws, true
}

// walkWorkspace lists a workspace folder (files + directories, relative
// slash-separated paths, sorted by path) and totals the file bytes.
// Unreadable entries are skipped rather than failing the whole listing.
func walkWorkspace(folder string) ([]wsEntry, int64, error) {
	var entries []wsEntry
	var total int64
	var truncated bool
	err := filepath.Walk(folder, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // unreadable entry: skip
		}
		if p == folder {
			return nil
		}
		rel, rerr := filepath.Rel(folder, p)
		if rerr != nil {
			return nil
		}
		rel = filepath.ToSlash(rel)
		e := wsEntry{Name: info.Name(), Path: rel}
		if info.IsDir() {
			e.Dir = true
		} else {
			e.Size = info.Size()
			e.Mod = info.ModTime().UTC().Format(time.RFC3339)
			total += info.Size()
		}
		entries = append(entries, e)
		if len(entries) >= wsWalkLimit {
			truncated = true
			return filepath.SkipDir
		}
		return nil
	})
	if err != nil {
		return nil, 0, err
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Path < entries[j].Path })
	if truncated {
		entries = entries[:wsWalkLimit]
	}
	return entries, total, nil
}

// refreshWorkspaceSize recomputes the workspace's size from disk and
// stores it (best-effort — a size bookkeeping failure never fails the
// operation that triggered it).
func refreshWorkspaceSize(us *store.UserStore, ws *store.Workspace) {
	_, total, err := walkWorkspace(ws.FolderPath)
	if err == nil {
		_ = us.SetWorkspaceSize(ws.ID, total)
	}
}

// windowsPathOK rejects a path segment that, on Windows, would target an
// alternate data stream: a colon in a name is the NTFS ADS separator
// ("file.txt:evil" — content invisible to listings, retrievable by name).
// Colons are legal filename characters on other OSes, so it is a no-op
// there. (The agent's file tools carry the same check; the server package
// is not importable from there, so each keeps its own copy.)
func windowsPathOK(p string) error {
	if runtime.GOOS == "windows" && strings.ContainsRune(p, ':') {
		return errors.New("the name must not contain ':' (on Windows a colon is the NTFS data-stream separator)")
	}
	return nil
}

// safeRelPath validates a client-supplied workspace-relative path:
// forward-slash, not absolute, no ".." escapes ("" = the workspace root).
func safeRelPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", nil
	}
	if strings.ContainsRune(p, '\x00') {
		return "", errors.New("invalid path")
	}
	if err := windowsPathOK(p); err != nil {
		return "", err
	}
	p = strings.ReplaceAll(p, "\\", "/")
	if strings.HasPrefix(p, "/") {
		return "", errors.New("path must be relative to the workspace")
	}
	clean := path.Clean(p)
	if clean == "." {
		return "", nil
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("path escapes the workspace")
	}
	return clean, nil
}

// joinSafe joins a workspace-relative path under base and verifies the
// result is still inside base (defense in depth on top of safeRelPath).
func joinSafe(base, rel string) (string, error) {
	abs := filepath.Join(base, filepath.FromSlash(rel))
	back, err := filepath.Rel(base, abs)
	if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes the workspace")
	}
	return abs, nil
}

// resolveInside re-checks, with the disk consulted, that abs stays inside
// base, resolving symlinks (the lexical checks above cannot see a link
// inside the workspace pointing anywhere — os.Open/os.Create would follow
// it). The file target must not BE a symlink, and the deepest existing
// ancestor must resolve to at or under the resolved root (security
// 2026-11-20; the agent's file tools keep the mirror copy,
// fileResolveInside).
func resolveInside(base, abs string) error {
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return fmt.Errorf("could not resolve the workspace root: %v", err)
	}
	if st, err := os.Lstat(abs); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return errors.New("path is a symlink — it is not followed outside the workspace")
	}
	anc := abs
	for {
		if _, err := os.Lstat(anc); err == nil {
			break
		}
		parent := filepath.Dir(anc)
		if parent == anc {
			break
		}
		anc = parent
	}
	realAnc, err := filepath.EvalSymlinks(anc)
	if err != nil {
		return fmt.Errorf("could not resolve %q: %v", abs, err)
	}
	if realAnc != realBase && !strings.HasPrefix(realAnc, realBase+string(filepath.Separator)) {
		return errors.New("path escapes the workspace (a symlink points outside)")
	}
	return nil
}

// GET /api/workspaces — the user's workspaces, each with its live size
// (walked from disk) and file count.
func (a *App) listWorkspaces(w http.ResponseWriter, r *http.Request) {
	_, us, ok := a.workspaceStore(w, r)
	if !ok {
		return
	}
	wss, err := us.ListWorkspaces()
	if err != nil {
		log.Printf("workspace list: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	type listed struct {
		store.Workspace
		Files int `json:"files"`
	}
	out := make([]listed, 0, len(wss))
	for _, ws := range wss {
		l := listed{Workspace: ws}
		if entries, total, err := walkWorkspace(ws.FolderPath); err == nil {
			l.Size = total
			for _, e := range entries {
				if !e.Dir {
					l.Files++
				}
			}
		}
		out = append(out, l)
	}
	writeJSON(w, http.StatusOK, map[string]any{"workspaces": out})
}

// POST /api/workspaces — create a workspace: the folder (with .SKILLS/
// and .dni/) first, then the metadata row (a row with a missing folder
// would 404 on every file op, so the folder is the source of truth).
// Body: {"title": "...", "description": "..."}.
func (a *App) createWorkspace(w http.ResponseWriter, r *http.Request) {
	u, us, ok := a.workspaceStore(w, r)
	if !ok {
		return
	}
	var req struct {
		Title       string `json:"title"`
		Description string `json:"description"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	req.Title = strings.TrimSpace(req.Title)
	req.Description = strings.TrimSpace(req.Description)
	if req.Title == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title is required"})
		return
	}
	if len(req.Title) > 120 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "title too long (max 120)"})
		return
	}
	if len(req.Description) > 500 {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "description too long (max 500)"})
		return
	}
	id := store.NewID()
	folder := filepath.Join(a.dataDir, "users", u.Handle, "workspaces", id)
	if err := os.MkdirAll(filepath.Join(folder, ".SKILLS"), 0o700); err != nil {
		log.Printf("workspace mkdir: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not create the workspace folder"})
		return
	}
	if err := os.MkdirAll(filepath.Join(folder, ".dni"), 0o700); err != nil {
		os.RemoveAll(folder)
		log.Printf("workspace mkdir .dni: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not create the workspace folder"})
		return
	}
	ws, err := us.CreateWorkspaceWithID(id, req.Title, req.Description, folder)
	if err != nil {
		os.RemoveAll(folder)
		log.Printf("workspace insert: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	writeJSON(w, http.StatusCreated, map[string]any{"workspace": ws})
}

// DELETE /api/workspaces/{id} — delete the workspace: its folder (and
// everything in it) from disk, then its metadata row (the file rows
// cascade).
func (a *App) deleteWorkspace(w http.ResponseWriter, r *http.Request) {
	u, us, ok := a.workspaceStore(w, r)
	if !ok {
		return
	}
	ws, ok := a.workspaceFor(w, r, us)
	if !ok {
		return
	}
	if err := os.RemoveAll(ws.FolderPath); err != nil {
		log.Printf("workspace delete: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not delete the workspace folder"})
		return
	}
	if err := us.DeleteWorkspace(ws.ID); err != nil {
		log.Printf("workspace delete row: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	_ = u
	// A delete frees space — re-check immediately so the frozen state (and
	// the disk page) reflect the new usage without waiting for the next scan.
	a.recheckDisk(u.Handle)
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// GET /api/workspaces/{id}/files — the workspace's contents, walked from
// disk (the disk is the source of truth: files the user created in their
// own container show up here without any bevin-side bookkeeping).
func (a *App) workspaceFiles(w http.ResponseWriter, r *http.Request) {
	_, us, ok := a.workspaceStore(w, r)
	if !ok {
		return
	}
	ws, ok := a.workspaceFor(w, r, us)
	if !ok {
		return
	}
	entries, total, err := walkWorkspace(ws.FolderPath)
	if err != nil {
		log.Printf("workspace walk: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	_ = us.SetWorkspaceSize(ws.ID, total)
	writeJSON(w, http.StatusOK, map[string]any{"files": entries, "size": total})
}

// POST /api/workspaces/{id}/files — upload a file into the workspace.
// Multipart form: "file" (required) + "path" (optional: a
// workspace-relative directory, e.g. "docs/reports"). The file is hashed
// (sha256) and a metadata row is added (source=user). An existing file
// of the same name is replaced. 413 when the upload would push the
// workspace past the server's max workspace size.
func (a *App) uploadWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	_, us, ok := a.workspaceStore(w, r)
	if !ok {
		return
	}
	ws, ok := a.workspaceFor(w, r, us)
	if !ok {
		return
	}
	if cl := r.ContentLength; cl > wsMaxUploadBytes {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("upload too large (max %d bytes per file)", wsMaxUploadBytes)})
		return
	}
	dirRel, err := safeRelPath(r.FormValue("path"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	// Budget: current disk total + this file must fit under the cap.
	_, currentTotal, err := walkWorkspace(ws.FolderPath)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	maxSize := a.liveCfg().MaxWorkspaceSize
	if maxSize <= 0 {
		maxSize = config.DefaultMaxWorkspaceSize
	}
	if currentTotal >= maxSize {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "the workspace is at its size limit"})
		return
	}
	h, fh, err := r.FormFile("file")
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "missing file field (multipart form: file, path)"})
		return
	}
	defer h.Close()
	name := path.Base(filepath.Clean("/" + fh.Filename))
	if name == "." || name == ".." || name == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "the file has no name"})
		return
	}
	if err := windowsPathOK(name); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	target, err := joinSafe(ws.FolderPath, path.Join(dirRel, name))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := resolveInside(ws.FolderPath, target); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if st, err := os.Stat(target); err == nil && st.IsDir() {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "a directory already exists at that path"})
		return
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		log.Printf("workspace upload mkdir: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	tmp := target + ".upload"
	out, err := os.Create(tmp)
	if err != nil {
		log.Printf("workspace upload create: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	hasher := sha256.New()
	// The caps are enforced MID-STREAM (security 2026-11-20): the declared
	// ContentLength check at the top is only the fast path — a chunked
	// request has none, so without this the copy could write unbounded
	// bytes to disk. The per-file cap stops the source reader; the budget
	// writer stops the moment the workspace would cross its limit
	// (nothing past the crossing reaches disk).
	src := io.LimitReader(h, wsMaxUploadBytes+1)
	capW := &uploadCapWriter{w: out, limit: maxSize - currentTotal}
	mw := io.MultiWriter(capW, hasher)
	head := make([]byte, 512)
	n, _ := src.Read(head)
	mimeDet := http.DetectContentType(head[:n])
	if n > 0 {
		if _, err := mw.Write(head[:n]); err != nil {
			out.Close()
			os.Remove(tmp)
			finishUploadError(w, err)
			return
		}
	}
	cw, cerr := io.Copy(mw, src)
	if cl := out.Close(); cerr == nil {
		cerr = cl
	}
	if cerr != nil {
		os.Remove(tmp)
		finishUploadError(w, cerr)
		return
	}
	written := int64(n) + cw
	if written > wsMaxUploadBytes {
		os.Remove(tmp)
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": fmt.Sprintf("upload too large (max %d bytes per file)", wsMaxUploadBytes)})
		return
	}
	if err := os.Rename(tmp, target); err != nil {
		os.Remove(tmp)
		log.Printf("workspace upload rename: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "internal error"})
		return
	}
	rel, _ := filepath.Rel(ws.FolderPath, target)
	rel = filepath.ToSlash(rel)
	kind := "doc"
	if strings.HasPrefix(mimeDet, "image/") || strings.HasPrefix(mimeDet, "video/") ||
		strings.HasPrefix(mimeDet, "audio/") || strings.HasPrefix(mimeDet, "font/") {
		kind = "asset"
	}
	if _, err := us.AddWorkspaceFile(ws.ID, name, rel, mimeDet, written, hex.EncodeToString(hasher.Sum(nil)), "user", kind); err != nil {
		log.Printf("workspace upload metadata: %v", err) // the file is on disk; metadata is best-effort
	}
	refreshWorkspaceSize(us, ws)
	writeJSON(w, http.StatusCreated, map[string]any{
		"file": map[string]any{"name": name, "path": rel, "size": written, "mime": mimeDet, "kind": kind},
		"size": currentTotal + written,
	})
}

// errUploadBudgetExceeded is the mid-stream budget crossing (an uploadCap
// writer stopped the copy): a 413, not a 500 — the request's fault.
var errUploadBudgetExceeded = errors.New("upload would exceed the workspace size limit")

// finishUploadError renders a failed upload copy: a budget crossing is a
// 413 (the request's fault), anything else a 500 (logged).
func finishUploadError(w http.ResponseWriter, err error) {
	if errors.Is(err, errUploadBudgetExceeded) {
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "upload would exceed the workspace size limit"})
		return
	}
	log.Printf("workspace upload: %v", err)
	writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "upload failed"})
}

// uploadCapWriter stops a workspace upload the moment a write would cross
// the remaining budget (the incremental half of the upload cap — see the
// mid-stream comment in uploadWorkspaceFile).
type uploadCapWriter struct {
	w        io.Writer
	written  int64
	limit    int64
	exceeded bool
}

func (c *uploadCapWriter) Write(p []byte) (int, error) {
	if c.exceeded {
		return 0, errUploadBudgetExceeded
	}
	if c.limit <= 0 || c.written+int64(len(p)) > c.limit {
		c.exceeded = true
		return 0, errUploadBudgetExceeded
	}
	n, err := c.w.Write(p)
	c.written += int64(n)
	return n, err
}

// GET /api/workspaces/{id}/files/raw?path=... — download a workspace
// file (the disk is the source of truth).
func (a *App) workspaceFileRaw(w http.ResponseWriter, r *http.Request) {
	_, us, ok := a.workspaceStore(w, r)
	if !ok {
		return
	}
	ws, ok := a.workspaceFor(w, r, us)
	if !ok {
		return
	}
	rel, err := safeRelPath(r.URL.Query().Get("path"))
	if err != nil || rel == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path (a workspace-relative file path) is required"})
		return
	}
	target, err := joinSafe(ws.FolderPath, rel)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if err := resolveInside(ws.FolderPath, target); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	f, err := os.Open(target)
	if err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil || st.IsDir() {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	head := make([]byte, 512)
	n, _ := f.Read(head)
	ctype := mime.TypeByExtension(filepath.Ext(target))
	if ctype == "" {
		ctype = http.DetectContentType(head[:n])
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Content-Disposition", "attachment; filename="+strconv.Quote(path.Base(rel)))
	// ServeContent sets Content-Length + handles Range requests.
	http.ServeContent(w, r, path.Base(rel), st.ModTime(), f)
}

// DELETE /api/workspaces/{id}/files?path=... — delete a file or
// directory from the workspace (disk first, then the metadata rows for
// the path and everything beneath it).
func (a *App) deleteWorkspaceFile(w http.ResponseWriter, r *http.Request) {
	u, us, ok := a.workspaceStore(w, r)
	if !ok {
		return
	}
	ws, ok := a.workspaceFor(w, r, us)
	if !ok {
		return
	}
	rel, err := safeRelPath(r.URL.Query().Get("path"))
	if err != nil || rel == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "path is required"})
		return
	}
	target, err := joinSafe(ws.FolderPath, rel)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	if _, err := os.Stat(target); err != nil {
		writeJSON(w, http.StatusNotFound, map[string]string{"error": "file not found"})
		return
	}
	if err := os.RemoveAll(target); err != nil {
		log.Printf("workspace file delete: %v", err)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not delete the file"})
		return
	}
	if err := us.DeleteWorkspaceFilesAt(ws.ID, rel); err != nil {
		log.Printf("workspace file metadata delete: %v", err)
	}
	refreshWorkspaceSize(us, ws)
	// A delete frees space — re-check immediately so the frozen state (and
	// the disk page) reflect the new usage without waiting for the next scan.
	a.recheckDisk(u.Handle)
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// Note: there is deliberately NO user-facing "run a command in the
// workspace" endpoint (user directive): the AGENT is the one that
// manages the user's environment (ensuring it + running commands in a
// workspace via the sandbox backend's ExecIn) — the user focuses on
// workspace STATE (the files here), not on execution details. The
// sandbox API (internal/server/sandbox.go) stays as the admin-diagnostic
// surface; the agent sandbox tool (a later revision) is the execution
// path.
