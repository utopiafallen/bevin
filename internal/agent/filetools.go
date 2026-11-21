package agent

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"path"
	"path/filepath"
	"runtime"
	"strings"

	"github.com/utopiafallen/bevin/internal/sandbox"
)

// edit_file (WS4): exact-match file edits anywhere in the USER'S CONTAINER
// (container STATE — the container is the shared medium between the user's
// parallel conversations). Paths are container-absolute. The match is
// verified against the file's CURRENT content at execution time, so an edit
// can never be applied to content that isn't there: if the file changed
// since the model last looked, it gets the closest current region back
// (the self-healing signal) — never a blind overwrite. All failure modes
// are RESULTS back to the model, never a turn error — the same contract as
// web_search/web_browser.
//
// The harness tracks NO state between reads and writes: the exact-match
// anchor IS the change detector, against every writer (another conversation
// of the same user, the user's own editor, a script). Whole-file reads and
// writes are shell commands' job; edit_file is the surgical edit.

// editArgs is the raw argument shape (unknown fields are ignored, missing
// optional ones zero out).
type editArgs struct {
	Path       string `json:"path"`
	OldString  string `json:"old_string"`
	NewString  string `json:"new_string"`
	ReplaceAll bool   `json:"replace_all"`
}

func registerEditTool(r *ToolRegistry, a *Agent) error {
	return r.Register(Tool{
		Name:        "edit_file",
		Description: "Replace an exact string in a text file anywhere in the user's container (absolute path: /... on linux, C:\\... on windows; workspaces live under /data/workspaces/<id>/). The match is verified against the file's CURRENT content — if it changed since you last looked, you get the closest current region back instead of a silent overwrite.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"path": map[string]any{
					"type":        "string",
					"description": "absolute path inside the user's container",
				},
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

// containerPath normalizes + validates a container-absolute path (the
// platform-native form inside the container). The error text is
// model-facing.
func containerPath(p string) (string, error) {
	p = strings.TrimSpace(p)
	if p == "" {
		return "", errors.New("path is required")
	}
	if strings.ContainsRune(p, '\x00') {
		return "", errors.New("invalid path")
	}
	if runtime.GOOS == "windows" {
		if len(p) < 3 || !isASCIILetter(p[0]) || p[1] != ':' || (p[2] != '\\' && p[2] != '/') {
			return "", errors.New(`path must be absolute inside the container (e.g. C:\data\workspaces\<id>\app.go)`)
		}
		// A colon after the drive letter targets an NTFS alternate data
		// stream (content invisible to listings, retrievable by name).
		if strings.ContainsRune(p[3:], ':') {
			return "", errors.New("paths must not contain ':' after the drive letter (on Windows a colon is the NTFS data-stream separator)")
		}
		p = strings.ReplaceAll(p, "/", `\`)
		return filepath.Clean(p), nil
	}
	if !strings.HasPrefix(p, "/") {
		return "", errors.New("path must be absolute inside the container (e.g. /data/workspaces/<id>/app.go)")
	}
	return path.Clean(p), nil
}

func isASCIILetter(c byte) bool {
	return (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z')
}

// refreshWorkspaceFileMeta updates the P12 metadata row when the edited
// path is a WORKSPACE file (the default layout: a workspace folder under
// the mapped data dir) so search/ingest sees the agent's edit. Non-
// workspace container files have no P12 rows. Best-effort — the file is on
// disk and listings walk from disk; a metadata failure is logged, never
// returned.
func (a *Agent) refreshWorkspaceFileMeta(handle, containerPath, dataDir string, content []byte) {
	// Compare in slash form (the data dir is C:\data on windows, /data on
	// linux).
	cp := strings.ReplaceAll(containerPath, `\`, "/")
	prefix := strings.TrimRight(strings.ReplaceAll(dataDir, `\`, "/"), "/") + "/"
	if !strings.HasPrefix(cp, prefix) {
		return
	}
	rel, _ := strings.CutPrefix(cp, prefix) // "workspaces/<id>/<rest>"
	rest, ok := strings.CutPrefix(rel, "workspaces/")
	if !ok {
		return
	}
	wsID, wsRel, cut := strings.Cut(rest, "/")
	if !cut || wsID == "" || wsRel == "" {
		return
	}
	us, err := a.userStore(handle)
	if err != nil {
		return
	}
	ws, err := us.GetWorkspace(wsID)
	if err != nil {
		return
	}
	if ws.FolderPath != "" {
		return // custom folder: not the mapped default layout
	}
	sum := sha256.Sum256(content)
	if err := us.UpsertWorkspaceFile(ws.ID, path.Base(wsRel), wsRel, int64(len(content)), hex.EncodeToString(sum[:]), "agent", "doc"); err != nil {
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
	p, err := containerPath(ea.Path)
	if err != nil {
		return err.Error(), nil
	}
	bk := a.containerBackend()
	if bk == nil {
		return "file editing is unavailable on this platform (no container backend).", nil
	}
	if a.diskIsFrozen(handle) {
		return "the user is over their disk cap — file writes are paused. Ask them to free up disk space first.", nil
	}
	data, err := bk.ReadFile(ctx, handle, p)
	if err != nil {
		switch {
		case errors.Is(err, sandbox.ErrNotRunning):
			return "the user's container isn't running — file editing needs it (start it in the container UI).", nil
		case errors.Is(err, sandbox.ErrFileNotFound):
			return fmt.Sprintf("no file at %s — create it first (edit_file only edits existing files).", p), nil
		case errors.Is(err, sandbox.ErrIsDir):
			return fmt.Sprintf("%s is a directory, not a file.", p), nil
		default:
			return fmt.Sprintf("could not read %s: %v", p, err), nil
		}
	}
	s := string(data)
	count := strings.Count(s, ea.OldString)
	switch {
	case count == 0:
		return "no exact match in " + p + " — " + editNearMiss(s, ea.OldString), nil
	case count > 1 && !ea.ReplaceAll:
		return fmt.Sprintf("%d occurrences in %s — include more surrounding context to make the match unique, or set replace_all to replace them all.", count, p), nil
	}
	n := count
	if ea.ReplaceAll {
		n = -1
	}
	updated := []byte(strings.Replace(s, ea.OldString, ea.NewString, n))
	if err := bk.WriteFileAtomic(ctx, handle, p, updated); err != nil {
		return fmt.Sprintf("could not write %s: %v", p, err), nil
	}
	a.refreshWorkspaceFileMeta(handle, p, bk.ContainerDataDir(), updated)
	if ea.ReplaceAll {
		return fmt.Sprintf("replaced all %d occurrences in %s.", count, p), nil
	}
	return fmt.Sprintf("replaced the match in %s (%d bytes written).", p, len(updated)), nil
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
// folded pair — old is short, content is bounded by a container text file).
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
