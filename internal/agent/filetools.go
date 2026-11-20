package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"strings"

	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/store"
)

// edit_file (WS4): exact-match file edits over the USER'S WORKSPACES
// (workspace STATE; the agent's execution surface for code is the
// container). Relative paths resolve against a workspace and are confined
// to its root. All failure modes are RESULTS back to the model (the
// self-healing signal) — never a turn error, the same contract as
// web_search/web_browser.

// editArgs is the raw argument shape (unknown fields are ignored, missing
// optional ones zero out).
type editArgs struct {
	Workspace  string `json:"workspace"`
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func registerEditTool(r *ToolRegistry, a *Agent) error {
	wsParam := map[string]any{
		"type":        "string",
		"description": "workspace id or title (omit when the user has one workspace)",
	}
	pathParam := map[string]any{
		"type":        "string",
		"description": "file path, relative to the workspace root",
	}
	return r.Register(Tool{
		Name:        "edit_file",
		Description: "Replace an exact string in a text file in the user's workspace. The match must be unique unless replace_all is set; on failure it shows the closest region.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"workspace": wsParam,
				"path":      pathParam,
				"old_string": map[string]any{
					"type":        "string",
					"description": "the exact text to find (byte-for-byte, including whitespace)",
				},
				"new_string": map[string]any{
					"type":        "string",
					"description": "the replacement text (empty deletes the match)",
				},
				"replace_all": map[string]any{
					"type":        "boolean",
					"description": "replace every occurrence (default: the match must be unique)",
				},
			},
			"required": []string{"path", "old_string"},
		},
		Execute: a.execEditFile,
	})
}

// resolveWorkspace resolves the workspace + its folder for a file call.
// Omitted name = the user's single workspace (else an error listing the
// options); a name matches by id first, then title.
func (a *Agent) resolveWorkspace(handle, name string) (*store.Workspace, string, error) {
	us, err := a.userStore(handle)
	if err != nil {
		return nil, "", err
	}
	wss, err := us.ListWorkspaces()
	if err != nil {
		return nil, "", err
	}
	if len(wss) == 0 {
		return nil, "", errors.New("the user has no workspaces — ask them to create one in the workspace UI before managing files")
	}
	var ws *store.Workspace
	if strings.TrimSpace(name) == "" {
		if len(wss) == 1 {
			ws = &wss[0]
		} else {
			var opts []string
			for _, w := range wss {
				opts = append(opts, fmt.Sprintf("%q (%s)", w.ID, w.Title))
			}
			sort.Strings(opts)
			return nil, "", fmt.Errorf("the user has %d workspaces — name one with the \"workspace\" argument: %s", len(wss), strings.Join(opts, ", "))
		}
	} else {
		match := func(pred func(w store.Workspace) bool) *store.Workspace {
			for i := range wss {
				if pred(wss[i]) {
					return &wss[i]
				}
			}
			return nil
		}
		ws = match(func(w store.Workspace) bool { return w.ID == name })
		if ws == nil {
			ws = match(func(w store.Workspace) bool { return w.Title == name })
		}
		if ws == nil {
			lower := strings.ToLower(name)
			ws = match(func(w store.Workspace) bool { return strings.ToLower(w.Title) == lower })
		}
		if ws == nil {
			var opts []string
			for _, w := range wss {
				opts = append(opts, fmt.Sprintf("%q (%s)", w.ID, w.Title))
			}
			sort.Strings(opts)
			return nil, "", fmt.Errorf("no workspace named %q — the user's workspaces: %s", name, strings.Join(opts, ", "))
		}
	}
	folder := ws.FolderPath
	if folder == "" {
		folder = filepath.Join(a.dataDir, "users", handle, "workspaces", ws.ID)
	}
	return ws, folder, nil
}

// fileSafeRelPath + fileJoinSafe mirror the workspace upload endpoint's
// path safety (forward-slash relative, no .., post-join containment).
// Duplicated in the agent (the server package is not importable from here).
func fileSafeRelPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("path is required")
	}
	if strings.ContainsRune(p, '\x00') {
		return "", errors.New("invalid path")
	}
	// Windows: a colon in a path is the NTFS alternate data stream
	// separator — "file.txt:evil" targets a hidden stream whose content is
	// invisible to listings but retrievable by name. Colons are legal
	// filename characters on other OSes, so the check is Windows-only.
	if runtime.GOOS == "windows" && strings.ContainsRune(p, ':') {
		return "", errors.New("paths must not contain ':' (on Windows a colon is the NTFS data-stream separator)")
	}
	p = strings.ReplaceAll(p, `\`, "/")
	if strings.HasPrefix(p, "/") {
		return "", errors.New("path must be relative to the workspace (no leading /)")
	}
	clean := path.Clean(p)
	if clean == "." {
		return "", errors.New("path is required")
	}
	if clean == ".." || strings.HasPrefix(clean, "../") {
		return "", errors.New("path escapes the workspace")
	}
	return clean, nil
}

func fileJoinSafe(base, rel string) (string, error) {
	abs := filepath.Join(base, filepath.FromSlash(rel))
	back, err := filepath.Rel(base, abs)
	if err != nil || back == ".." || strings.HasPrefix(back, ".."+string(filepath.Separator)) {
		return "", errors.New("path escapes the workspace")
	}
	return abs, nil
}

// fileResolveInside re-checks, with the disk consulted, that abs stays
// inside the workspace root. The lexical checks (fileSafeRelPath +
// fileJoinSafe) cannot see symlinks: a link inside the workspace (created
// out-of-band, or by a sandboxed command) can point anywhere, and
// os.Open/os.Create would follow it. The rule: the file target must not BE
// a symlink (a read would follow it out), and the deepest existing ancestor
// must resolve to at or under the resolved root (security 2026-11-20).
func fileResolveInside(base, abs string) error {
	realBase, err := filepath.EvalSymlinks(base)
	if err != nil {
		return fmt.Errorf("could not resolve the workspace root: %v", err)
	}
	if st, err := os.Lstat(abs); err == nil && st.Mode()&os.ModeSymlink != 0 {
		return errors.New("path is a symlink — the file tools do not follow symlinks out of the workspace")
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

// fileWriteTarget resolves the arguments to a workspace-confined absolute
// path. The error text is model-facing.
func (a *Agent) fileWriteTarget(handle string, ea *editArgs) (*store.Workspace, string, string, string, error) {
	ws, folder, err := a.resolveWorkspace(handle, ea.Workspace)
	if err != nil {
		return nil, "", "", "", err
	}
	rel, err := fileSafeRelPath(ea.Path)
	if err != nil {
		return nil, "", "", "", err
	}
	abs, err := fileJoinSafe(folder, rel)
	if err != nil {
		return nil, "", "", "", err
	}
	if err := fileResolveInside(folder, abs); err != nil {
		return nil, "", "", "", err
	}
	return ws, folder, abs, rel, nil
}

// writeWorkspaceFile atomically writes content under abs (parents created),
// enforcing the user's max_workspace_size budget against the WHOLE
// workspace's current disk total first. Returns the bytes written.
func (a *Agent) writeWorkspaceFile(folder, abs string, content []byte) (int64, error) {
	// Budget: the workspace's current disk total + this write must fit under
	// the cap (the agent must not bypass the user's disk cap in one write).
	currentTotal, err := workspaceSize(folder)
	if err != nil {
		return 0, fmt.Errorf("could not check the workspace size: %v", err)
	}
	maxSize := a.liveMaxWorkspaceSize()
	if currentTotal+int64(len(content)) > maxSize {
		return 0, fmt.Errorf("write would exceed the workspace size limit (%d bytes free of the %d-byte cap)", maxSize-currentTotal, maxSize)
	}
	if err := os.MkdirAll(filepath.Dir(abs), 0o755); err != nil {
		return 0, err
	}
	tmp := abs + ".write"
	if err := os.WriteFile(tmp, content, 0o644); err != nil {
		return 0, err
	}
	if err := os.Rename(tmp, abs); err != nil {
		os.Remove(tmp)
		return 0, err
	}
	return int64(len(content)), nil
}

// liveMaxWorkspaceSize is the admin's cap (live config, the npCfg pattern).
func (a *Agent) liveMaxWorkspaceSize() int64 {
	a.cfgMu.Lock()
	defer a.cfgMu.Unlock()
	if a.cfg.MaxWorkspaceSize > 0 {
		return a.cfg.MaxWorkspaceSize
	}
	return config.DefaultMaxWorkspaceSize
}

// workspaceSize walks a folder's files (the budget check only needs the
// total, not the listing).
func workspaceSize(folder string) (int64, error) {
	var total int64
	err := filepath.Walk(folder, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return nil // unreadable entry: skip
		}
		if !info.IsDir() {
			total += info.Size()
		}
		return nil
	})
	return total, err
}

// recordWorkspaceFile updates the P12 metadata row so search/ingest sees
// the agent's edit (best-effort — the file is on disk and listings walk
// from disk; a metadata failure is logged, never returned).
func (a *Agent) recordWorkspaceFile(handle string, ws *store.Workspace, abs, rel string, size int64) {
	us, err := a.userStore(handle)
	if err != nil {
		return
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return
	}
	sum := sha256.Sum256(data)
	name := path.Base(rel)
	if err := us.UpsertWorkspaceFile(ws.ID, name, rel, size, hex.EncodeToString(sum[:]), "agent", "doc"); err != nil {
		log.Printf("edit_file metadata: %v", err)
	}
}

// execEditFile — edit_file.
func (a *Agent) execEditFile(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var ea editArgs
	if err := json.Unmarshal(args, &ea); err != nil {
		return fmt.Sprintf("invalid arguments: %v — \"path\" and \"old_string\" are required.", err), nil
	}
	if strings.TrimSpace(ea.OldString) == "" {
		return "edit_file needs a non-empty \"old_string\" (the exact text to replace).", nil
	}
	if ea.OldString == ea.NewString {
		return "old_string and new_string are identical — nothing to change.", nil
	}
	ws, folder, abs, rel, err := a.fileWriteTarget(handle, &ea)
	if err != nil {
		return err.Error(), nil
	}
	st, err := os.Stat(abs)
	if errors.Is(err, os.ErrNotExist) {
		return fmt.Sprintf("no file at %q in workspace %q — create it first.", rel, ws.ID), nil
	}
	if err != nil {
		return fmt.Sprintf("could not read %q: %v", rel, err), nil
	}
	if st.IsDir() {
		return fmt.Sprintf("%q is a directory, not a file.", rel), nil
	}
	data, err := os.ReadFile(abs)
	if err != nil {
		return fmt.Sprintf("could not read %q: %v", rel, err), nil
	}
	s := string(data)
	count := strings.Count(s, ea.OldString)
	switch {
	case count == 0:
		return "no exact match in " + rel + " — " + editNearMiss(s, ea.OldString), nil
	case count > 1 && !ea.ReplaceAll:
		return fmt.Sprintf("%d occurrences in %q — include more surrounding context to make the match unique, or set replace_all to replace them all.", count, rel), nil
	}
	n := count
	if ea.ReplaceAll {
		n = -1
	}
	updated := strings.Replace(s, ea.OldString, ea.NewString, n)
	written, err := a.writeWorkspaceFile(folder, abs, []byte(updated))
	if err != nil {
		return err.Error(), nil
	}
	a.recordWorkspaceFile(handle, ws, abs, rel, written)
	if ea.ReplaceAll {
		return fmt.Sprintf("replaced all %d occurrences in %q.", count, rel), nil
	}
	return fmt.Sprintf("replaced the match in %q (%d bytes written).", rel, written), nil
}

// editNearMiss turns a 0-match edit into the model's self-healing signal:
// casing hints, the closest line region (±3 lines, numbered), and the
// widen-the-match advice. Never a silent fuzzy apply.
func editNearMiss(content, old string) string {
	if countFold := foldCount(content, old); countFold == 1 {
		return "a case-insensitive match exists — fix the casing of old_string to match the file exactly"
	}
	oldLines := strings.Split(strings.TrimRight(old, "\n"), "\n")
	fileLines := strings.Split(content, "\n")
	best, bestScore := -1, 0
	for i, fl := range fileLines {
		score := 0
		for _, ol := range oldLines {
			score += lineOverlap(fl, ol)
		}
		if score > bestScore {
			best, bestScore = i, score
		}
	}
	if best < 0 {
		return "nothing similar in the file — look at the current content first (e.g. cat it), then edit what is actually there"
	}
	start := best - 3
	if start < 0 {
		start = 0
	}
	end := best + 4
	if end > len(fileLines) {
		end = len(fileLines)
	}
	var b strings.Builder
	for i := start; i < end; i++ {
		mark := " "
		if i == best {
			mark = ">"
		}
		fmt.Fprintf(&b, "%s%d|%s\n", mark, i+1, fileLines[i])
	}
	return "closest region (\" > \" marks the best-matching line):\n" + b.String() + "— include more surrounding context in old_string to make the match unique and exact"
}

// lineOverlap scores how much two lines share at their edges (prefix +
// suffix, capped at the shorter line) — enough to surface a near-miss
// region without a real edit-distance implementation.
func lineOverlap(a, b string) int {
	if a == "" || b == "" {
		return 0
	}
	prefix := 0
	for prefix < len(a) && prefix < len(b) && a[prefix] == b[prefix] {
		prefix++
	}
	suffix := 0
	for suffix < len(a)-prefix && suffix < len(b)-prefix && a[len(a)-1-suffix] == b[len(b)-1-suffix] {
		suffix++
	}
	minLen := len(a)
	if len(b) < minLen {
		minLen = len(b)
	}
	s := prefix + suffix
	if s > minLen {
		s = minLen
	}
	return s
}

// foldCount counts case-insensitive occurrences (strings.Count over the
// folded pair — old is short, content is bounded by a workspace file).
func foldCount(content, old string) int {
	if old == "" {
		return 0
	}
	lc := strings.ToLower(content)
	lo := strings.ToLower(old)
	count := 0
	from := 0
	for {
		i := strings.Index(lc[from:], lo)
		if i < 0 {
			break
		}
		count++
		from += i + len(lo)
	}
	return count
}
