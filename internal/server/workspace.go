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
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/utopiafallen/bevin/internal/auth"
	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/sandbox"
	"github.com/utopiafallen/bevin/internal/store"
)

// Workspaces are the user's working folders on the server. Each is a
// real directory in the user's data dir —
//
//	<data>/users/<handle>/workspaces/<id>/
//
// created with a .SKILLS/ subfolder (per-workspace agent skills) and a
// .dni/ (Do Not Index — the opt-out from search/ingestion). Because the
// user's container maps their data dir into it (/data on linux, C:\data
// on windows), the workspace shows up inside the user's container at
// <data dir>/workspaces/<id>: it is the user's persistent environment,
// and the workspace "run" command executes in it (arbitrary code is the
// point).
//
// File management is host-side (bevin writes the user's own data dir
// directly — no container needed); only running code goes through the
// container, so a server without the container runtime can still manage
// workspace files. Files are tracked in the workspace_files table
// (metadata: name, path, size, hash, source, kind) when they arrive
// through the API — files that appear on disk some other way (e.g. the
// user ran a git clone in their own container) are visible in the
// listing (it walks the disk) but gain no metadata row.

// wsMaxUploadBytes caps a single upload (the per-workspace global cap is
// the server config's MaxWorkspaceSize; this keeps one request bounded).
const wsMaxUploadBytes = 256 << 20

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
	target, err := joinSafe(ws.FolderPath, path.Join(dirRel, name))
	if err != nil {
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
	head := make([]byte, 512)
	n, _ := h.Read(head)
	mimeDet := http.DetectContentType(head[:n])
	var written int64
	if n > 0 {
		if _, err := out.Write(head[:n]); err != nil {
			out.Close()
			os.Remove(tmp)
			log.Printf("workspace upload: %v", err)
			writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "upload failed"})
			return
		}
		hasher.Write(head[:n])
		written += int64(n)
	}
	cw, cerr := io.Copy(io.MultiWriter(out, hasher), h)
	if cl := out.Close(); cerr == nil {
		cerr = cl
	}
	if cerr != nil {
		os.Remove(tmp)
		log.Printf("workspace upload: %v", cerr)
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "upload failed"})
		return
	}
	written += cw
	if written+currentTotal > maxSize {
		os.Remove(tmp)
		writeJSON(w, http.StatusRequestEntityTooLarge, map[string]string{"error": "upload would exceed the workspace size limit"})
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
	writeJSON(w, http.StatusOK, map[string]bool{"deleted": true})
}

// POST /api/workspaces/{id}/run — run a command in the user's
// environment with the working directory set to the workspace folder (the
// environment is created + started on first use). This is the
// execution half of the workspace: the user's files in the user's
// environment. Containerization is a hidden detail from the user — the
// error wording below never names it (the runtime internals are
// admin-only, see envError). Body:
// {"command": "...", "timeout_ms": 120000} (timeout optional, default 60
// s, capped at 5 min by the sandbox backend).
//
// 503 = the platform has no container backend (bevin targets Linux +
// Windows);
// 412 = the environment is not ready / not running;
// 502 = the exec itself failed.
func (a *App) workspaceRun(w http.ResponseWriter, r *http.Request) {
	_, us, ok := a.workspaceStore(w, r)
	if !ok {
		return
	}
	ws, ok := a.workspaceFor(w, r, us)
	if !ok {
		return
	}
	var req struct {
		Command   string `json:"command"`
		TimeoutMs int    `json:"timeout_ms"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "invalid JSON body"})
		return
	}
	if strings.TrimSpace(req.Command) == "" {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "command is required"})
		return
	}
	timeout := time.Duration(req.TimeoutMs) * time.Millisecond
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	bk, u, ok := a.sandboxGate(w, r)
	if !ok {
		return
	}
	if rt := a.sandboxRuntime(r.Context()); !rt.OK {
		envError(w, http.StatusPreconditionFailed, u,
			"your environment is not ready yet — try again in a moment", &rt, nil)
		return
	}
	// The environment must exist before the exec (first run: rootfs from
	// the base image + boot — can take minutes; the server has no HTTP
	// timeouts).
	if _, err := bk.Ensure(r.Context(), u.Handle); err != nil {
		envError(w, http.StatusBadGateway, u,
			"your environment could not be started — try again", a.sandboxRuntimePtr(r.Context()), err)
		return
	}
	// The workspace folder inside the environment: the data dir is mapped
	// to ContainerDataDir(), so the folder is <data>/workspaces/<id>.
	cwd := filepath.Join(bk.ContainerDataDir(), "workspaces", ws.ID)
	res, err := bk.ExecIn(r.Context(), u.Handle, req.Command, cwd, timeout)
	if err != nil {
		code := http.StatusBadGateway
		if errors.Is(err, sandbox.ErrNotRunning) {
			code = http.StatusPreconditionFailed
		}
		envError(w, code, u, "your environment could not run the command", a.sandboxRuntimePtr(r.Context()), err)
		return
	}
	writeJSON(w, http.StatusOK, res)
}
