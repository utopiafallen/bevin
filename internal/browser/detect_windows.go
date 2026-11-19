//go:build windows

package browser

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"

	"golang.org/x/sys/windows/registry"
)

// ErrNoBrowser is the detection failure the web_browser tool surfaces as
// its "unavailable" result (neutral wording — the user is the one who
// installs a browser; the tool has no other backend).
var ErrNoBrowser = fmt.Errorf("no supported desktop browser found on this host (looked for Chrome, Edge, Brave and Chromium in the standard install locations, the registry, and on PATH); install one to enable the web_browser tool")

// Detection is a usable host browser: the executable + its family name.
type Detection struct {
	Name string // chrome / edge / brave (the family, for diagnostics)
	Path string // absolute path to the executable
}

// Detect finds a supported Chromium-based browser on a NATIVE Windows host:
// the standard install paths first (where installers put them), then the
// registry Uninstall/Appx keys (installers register themselves there, and
// per-user installs live outside Program Files), then PATH.
func Detect() (Detection, error) {
	paths := []struct{ name, path string }{
		{"chrome", filepath.Join(`C:\Program Files\Google\Chrome\Application`, `chrome.exe`)},
		{"chrome", filepath.Join(`C:\Program Files (x86)\Google\Chrome\Application`, `chrome.exe`)},
		{"edge", filepath.Join(`C:\Program Files (x86)\Microsoft\Edge\Application`, `msedge.exe`)},
		{"edge", filepath.Join(`C:\Program Files\Microsoft\Edge\Application`, `msedge.exe`)},
		{"brave", filepath.Join(`C:\Program Files\BraveSoftware\Brave-Browser\Application`, `brave.exe`)},
	}
	for _, c := range paths {
		if isExecutable(c.path) {
			return Detection{Name: c.name, Path: c.path}, nil
		}
	}
	// Registry: the uninstall keys carry InstallLocation (per-user installs
	// land under HKCU). Both views (64-bit root + WOW6432Node) for Chrome.
	for _, key := range []string{
		`Software\Microsoft\Windows\CurrentVersion\Uninstall\Google Chrome`,
		`Software\WOW6432Node\Microsoft\Windows\CurrentVersion\Uninstall\Google Chrome`,
	} {
		if loc := registryInstallLocation(key, `chrome.exe`); loc != "" {
			return Detection{Name: "chrome", Path: loc}, nil
		}
	}
	// Edge (packaged app): the Appx key's InstallLocation.
	if loc := registryInstallLocation(`Software\Microsoft\Windows\CurrentVersion\Appx\AppxAllUserStore\Packages\Microsoft.MicrosoftEdge_8wekyb3d8bbwe`, `msedge.exe`); loc != "" {
		return Detection{Name: "edge", Path: loc}, nil
	}
	for _, c := range []struct{ name, bin string }{
		{"chrome", "chrome"},
		{"edge", "msedge"},
		{"brave", "brave"},
	} {
		if p, err := exec.LookPath(c.bin); err == nil {
			return Detection{Name: c.name, Path: p}, nil
		}
	}
	return Detection{}, ErrNoBrowser
}

// registryInstallLocation reads the InstallLocation value of an uninstall
// key and reports <location>\<exe> when the file is there ("" otherwise).
func registryInstallLocation(key, exe string) string {
	k, err := registry.OpenKey(registry.LOCAL_MACHINE, key, registry.QUERY_VALUE)
	if err != nil {
		if ku, err := registry.OpenKey(registry.CURRENT_USER, key, registry.QUERY_VALUE); err == nil {
			defer ku.Close()
			loc, _, err := ku.GetStringValue(`InstallLocation`)
			if err != nil {
				return ""
			}
			p := filepath.Join(loc, exe)
			if isExecutable(p) {
				return p
			}
			return ""
		}
		return ""
	}
	defer k.Close()
	loc, _, err := k.GetStringValue(`InstallLocation`)
	if err != nil {
		return ""
	}
	p := filepath.Join(loc, exe)
	if isExecutable(p) {
		return p
	}
	return ""
}

// isExecutable reports whether p is a regular file. Windows has NO execute
// bit — Go reports 0666 for every regular file there — so the unix
// Mode()&0o111 test is always false on a native Windows host and every
// standard-path probe failed even with Edge installed (2026-11-19: the
// live box had msedge.exe at the standard path and detection still
// reported no browser). File existence IS the test: whatever an installer
// put at these paths is the executable.
func isExecutable(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.Mode().IsRegular()
}
