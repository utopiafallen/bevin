//go:build !windows

package browser

import (
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// ErrNoBrowser is the detection failure the web_browser tool surfaces as
// its "unavailable" result (neutral wording — the user is the one who
// installs a browser; the tool has no other backend).
var ErrNoBrowser = fmt.Errorf("no supported desktop browser found on this host (looked for Chrome, Edge, Brave and Chromium in the standard install paths and on PATH); install one to enable the web_browser tool")

// Detection is a usable host browser: the executable + its family name.
type Detection struct {
	Name string // chrome / edge / brave (the family, for diagnostics)
	Path string // absolute path to the executable
}

// wslWindowsCandidates: the standard Windows install paths, probed through
// the /mnt/<drive> mount when bevin runs under WSL (the browser there is a
// Windows executable — WSL interop launches it, its window lands on the
// WSLg desktop). Probed BEFORE the PATH names: on a WSL desktop box the
// Windows-side browser is what the user sees.
var wslWindowsCandidates = []struct{ name, path string }{
	{"edge", `C:/Program Files (x86)/Microsoft/Edge/Application/msedge.exe`},
	{"edge", `C:/Program Files/Microsoft/Edge/Application/msedge.exe`},
	{"chrome", `C:/Program Files/Google/Chrome/Application/chrome.exe`},
	{"chrome", `C:/Program Files (x86)/Google/Chrome/Application/chrome.exe`},
	{"brave", `C:/Program Files/BraveSoftware/Brave-Browser/Application/brave.exe`},
}

// pathCandidates: native (Linux/darwin) browsers via PATH, in preference
// order.
var pathCandidates = []struct{ name, bin string }{
	{"chrome", "google-chrome"},
	{"chrome", "google-chrome-stable"},
	{"edge", "microsoft-edge"},
	{"edge", "microsoft-edge-stable"},
	{"brave", "brave-browser"},
	{"chrome", "chromium"},
	{"chrome", "chromium-browser"},
}

// Detect finds a supported Chromium-based browser on the host (the plan's
// WS5 requirement check). Windows install paths through the WSL mount
// first (a WSL desktop box), then PATH.
func Detect() (Detection, error) {
	if isWSL() {
		for _, c := range wslWindowsCandidates {
			// C:/Program Files/... → /mnt/c/Program Files/...
			p := "/mnt/" + strings.ToLower(string(c.path[0])) + c.path[2:]
			if isExecutable(p) {
				return Detection{Name: c.name, Path: p}, nil
			}
		}
	}
	for _, c := range pathCandidates {
		if p, err := exec.LookPath(c.bin); err == nil {
			return Detection{Name: c.name, Path: p}, nil
		}
	}
	return Detection{}, ErrNoBrowser
}

// isExecutable reports whether p is a regular file with an executable bit
// (a WSL-mounted Windows .exe presents the full 111 mode).
func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && !fi.IsDir() && fi.Mode()&0o111 != 0
}
