// Package browser is the host-side backing of the web_browser tool (the
// plan's WS5): the server launches a DEDICATED, VISIBLE desktop browser per
// user and drives it over CDP.
//
//   - dedicated --user-data-dir profile under the user's bevin data dir —
//     NEVER the user's own browser profile (isolation + clean state; the
//     user can still log in inside the visible profile, and the logins
//     persist across server restarts);
//   - a visible window: the user watches the agent browse, can log in, can
//     step in — the "real desktop browser" transparency property;
//   - loopback CDP endpoint: --remote-debugging-port=0, the actual port
//     discovered from the profile dir's DevToolsActivePort file (no listen
//     race);
//   - one CDP endpoint per user ⇒ no cross-user tab contention; the user's
//     own browser is never touched.
//
// Instances are lazy (the user's first web_browser call) and live for the
// server's lifetime; Manager.Close kills them all (server shutdown).

package browser

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"
)

// Manager launches and tracks one dedicated browser per user.
type Manager struct {
	dataDir string

	mu        sync.Mutex
	instances map[string]*Instance // by user handle
}

// NewManager creates the per-user browser manager rooted at the bevin data
// dir (each user's profile lands in <dataDir>/browser/<handle>/). It
// launches nothing — ForUser does, on the user's first use.
func NewManager(dataDir string) *Manager {
	return &Manager{dataDir: dataDir, instances: map[string]*Instance{}}
}

// detectBrowser is the host browser probe (a variable so tests can stub a
// "no browser" host or a fake binary).
var detectBrowser = Detect

// ForUser returns the user's browser instance, launching it on first use.
// A dead instance (the user closed the window, the CDP endpoint went down)
// is relaunched fresh on the next call.
func (m *Manager) ForUser(ctx context.Context, handle string) (*Instance, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if inst, ok := m.instances[handle]; ok {
		if inst.alive() {
			return inst, nil
		}
		inst.Close()
		delete(m.instances, handle)
	}
	det, err := detectBrowser()
	if err != nil {
		return nil, err
	}
	profileDir := filepath.Join(m.dataDir, "browser", handle, "profile")
	if err := os.MkdirAll(profileDir, 0o755); err != nil {
		return nil, err
	}
	inst, err := launchInstance(ctx, det, profileDir)
	if err != nil {
		return nil, err
	}
	m.instances[handle] = inst
	return inst, nil
}

// Close kills every launched instance (server shutdown — a browser child
// must not outlive the server).
func (m *Manager) Close() {
	m.mu.Lock()
	defer m.mu.Unlock()
	for h, inst := range m.instances {
		inst.Close()
		delete(m.instances, h)
	}
}

// Instances reports how many browsers are currently launched (operator
// diagnostics).
func (m *Manager) Instances() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.instances)
}

// Instance is one user's dedicated browser: the child process + its CDP
// connection + the per-user tab state. The instance is shared by ALL of the
// user's conversations (one visible browser per user), so the
// conversation-scoped state — each conversation's working tab (the tab-less
// default) and its #N element table — is keyed by conversation id
// (convTab/convRead), never instance-global: two conversations of the same
// user must not see each other's element tables or silently act on each
// other's tabs.
type Instance struct {
	browser    Detection
	cmd        *exec.Cmd
	port       int
	cdp        *Client
	profileDir string

	mu sync.Mutex
	// Per-conversation state (keyed by conversation id; "" = the shared
	// bucket for callers outside a turn — tests, live e2e):
	convTab  map[string]string             // conversation id → its working tab (the last tab it opened or named)
	convRead map[string]map[int]elementRef // conversation id → its last Read's #N element table
	sessions map[string]string             // tab target id → CDP session id
	netState map[string]*netCounter        // session id → in-flight request counter (the settle signal)

	closeOnce sync.Once
}

// launchInstance starts the browser child, discovers its CDP port from the
// DevToolsActivePort file, and connects the browser-level CDP endpoint.
func launchInstance(ctx context.Context, det Detection, profileDir string) (*Instance, error) {
	// The previous instance's DevToolsActivePort (if any) must go before
	// launch — a stale port would be read as this instance's.
	_ = os.Remove(filepath.Join(profileDir, "DevToolsActivePort"))
	args := []string{
		"--user-data-dir=" + toBrowserPath(profileDir),
		"--remote-debugging-port=0", // the browser picks a free loopback port...
		"--no-first-run",
		"--no-default-browser-check",
		"--disable-sync", // the dedicated profile must not prompt to sign in to sync (a fresh profile does — the prompt opens an edge:// tab that would clutter the tab list)
		"--window-size=1200,900",
		"about:blank",
	}
	cmd := exec.Command(det.Path, args...)
	setProcessGroup(cmd)
	// The child's stdout/stderr stay attached to the console it was started
	// with (nil = inherit); a GUI browser writes little, and bevin's own
	// logging is the operator's surface.
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("launching %s: %w", det.Path, err)
	}
	port, err := waitForCDPPort(ctx, profileDir, cmd)
	if err != nil {
		killBrowserTree(cmd, det, profileDir)
		return nil, err
	}
	wsURL, err := browserWSURL(ctx, port)
	if err != nil {
		killBrowserTree(cmd, det, profileDir)
		return nil, err
	}
	cdp, err := Dial(ctx, wsURL)
	if err != nil {
		killBrowserTree(cmd, det, profileDir)
		return nil, err
	}
	inst := &Instance{
		browser:    det,
		cmd:        cmd,
		port:       port,
		cdp:        cdp,
		profileDir: profileDir,
		convTab:    map[string]string{},
		convRead:   map[string]map[int]elementRef{},
		sessions:   map[string]string{},
		netState:   map[string]*netCounter{},
	}
	// The connection's event sink: the Network-domain counter the settle
	// wait consumes (the flat CDP client's first event consumer).
	cdp.SetEventSink(inst.handleNetEvent)
	return inst, nil
}

// waitForCDPPort polls the profile dir's DevToolsActivePort (the browser
// writes it once it has picked its debugging port) until it appears or the
// deadline passes — the race-free discovery for --remote-debugging-port=0.
func waitForCDPPort(ctx context.Context, profileDir string, cmd *exec.Cmd) (int, error) {
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if b, err := os.ReadFile(filepath.Join(profileDir, "DevToolsActivePort")); err == nil {
			line := strings.SplitN(strings.TrimSpace(string(b)), "\n", 2)[0]
			if p, err := strconv.Atoi(strings.TrimSpace(line)); err == nil && p > 0 {
				return p, nil
			}
		}
		select {
		case <-ctx.Done():
			return 0, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
	return 0, fmt.Errorf("the browser did not publish its CDP port (DevToolsActivePort) within 30 s — it may have failed to start (no desktop session?)")
}

// browserWSURL fetches the browser-level WebSocket URL from the CDP HTTP
// endpoint (/json/version).
func browserWSURL(ctx context.Context, port int) (string, error) {
	body, err := httpGet(ctx, fmt.Sprintf("http://127.0.0.1:%d/json/version", port))
	if err != nil {
		return "", err
	}
	var v struct {
		WebSocketDebuggerURL string `json:"webSocketDebuggerUrl"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return "", fmt.Errorf("parsing /json/version: %w", err)
	}
	if v.WebSocketDebuggerURL == "" {
		return "", fmt.Errorf("the CDP endpoint did not report a webSocketDebuggerUrl")
	}
	return v.WebSocketDebuggerURL, nil
}

// httpGet does a small loopback GET (the CDP HTTP endpoint).
func httpGet(ctx context.Context, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	buf := make([]byte, 0, 4096)
	tmp := make([]byte, 4096)
	for {
		n, err := resp.Body.Read(tmp)
		buf = append(buf, tmp[:n]...)
		if err != nil {
			break
		}
		if len(buf) > 1<<20 { // the /json target list is small; cap defensively
			break
		}
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("CDP endpoint %s: HTTP %d", url, resp.StatusCode)
	}
	return buf, nil
}

// alive reports whether the instance's CDP connection is still up (the user
// closing the browser window takes the endpoint down with it).
func (i *Instance) alive() bool { return i.cdp.Alive() }

// Close tears the instance down: the CDP connection, then the browser
// process tree. Idempotent.
func (i *Instance) Close() {
	i.closeOnce.Do(func() {
		if i.cdp != nil {
			i.cdp.Close()
		}
		killBrowserTree(i.cmd, i.browser, i.profileDir)
	})
}

// killBrowserTree tears down the browser process tree. Killing the direct
// child alone is NOT enough: a Windows browser (native or launched from WSL
// through the interop) is a LAUNCHER — the process bevin started hands the
// session to a real browser process and exits (or the WSL wrapper dies
// alone), so cmd.Process.Kill() leaves the browser running. A survivor is
// worse than a leak: it keeps the profile locked (a relaunch prints
// "Opening in existing browser session" and never publishes a new
// DevToolsActivePort — the dead-CWD relaunch hang) and it leaves a window
// on the user's desktop. So: kill the direct child, then — on the Windows
// side — sweep every browser process whose command line carries OUR profile
// path (never by image name — that would take down the user's own browser
// sessions); on Linux, kill the child's process group.
func killBrowserTree(cmd *exec.Cmd, det Detection, profileDir string) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	if runtime.GOOS == "windows" || isWSL() {
		sweepWindowsBrowserProcesses(det.Path, toBrowserPath(profileDir))
		return
	}
	killProcessGroup(cmd.Process.Pid)
}

// powershellPath resolves the PowerShell binary: PATH first (native
// Windows), then the drive-mounted System32 location (WSL — Go's
// LookPath does not do the WSL interop resolution that the shell does).
func powershellPath() string {
	for _, name := range []string{"powershell", "powershell.exe"} {
		if p, err := exec.LookPath(name); err == nil {
			return p
		}
	}
	for _, d := range []string{"c", "C"} {
		p := "/mnt/" + d + "/Windows/System32/WindowsPowerShell/v1.0/powershell.exe"
		if st, err := os.Stat(p); err == nil && !st.IsDir() {
			return p
		}
	}
	return "powershell" // native Windows: System32 is on PATH (the LookPath above missed only in a broken PATH)
}

// sweepWindowsBrowserProcesses kills every process of this browser family
// whose command line names our profile dir (the browser process carries the
// --user-data-dir of every launch; the marker is unique to this instance).
// It goes through PowerShell's Win32_Process view (tasklist has no command
// lines; wmic is removed from recent Windows builds).
func sweepWindowsBrowserProcesses(browserPath, profileDir string) {
	name := filepath.Base(browserPath) // e.g. msedge.exe
	var b strings.Builder
	b.WriteString(`Get-CimInstance Win32_Process -Filter "Name='`)
	b.WriteString(name)
	b.WriteString(`'" | Where-Object { $_.CommandLine -match [regex]::Escape('`)
	b.WriteString(profileDir)
	b.WriteString(`') } | ForEach-Object { taskkill /F /T /PID $_.ProcessId 2>$null | Out-Null }`)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if out, err := exec.CommandContext(ctx, powershellPath(), "-NoProfile", "-Command", b.String()).CombinedOutput(); err != nil {
		log.Printf("browser sweep (%s): %v: %s", name, err, strings.TrimSpace(string(out)))
	}
}

// elementRef is one entry of the latest read's numbered interactive-element
// table: the #N the model references, resolved server-side (the plan's WS5
// element model — the model never guesses selectors).
type elementRef struct {
	N        int    `json:"n"`
	Role     string `json:"role"`
	Name     string `json:"name"`
	Selector string `json:"selector"`
	Tag      string `json:"tag"`
}
