package browser

import (
	"os"
	"regexp"
	"strings"
)

// toBrowserPath converts a bevin path (the per-user browser profile dir
// under the data dir) into the form the BROWSER PROCESS needs for
// --user-data-dir. The browser may be a different OS than bevin:
//
//   - native (the browser runs on bevin's own OS): the path as-is;
//   - WSL (a Linux bevin launching the user's WINDOWS browser — the common
//     desktop-box case: bevin under WSLg, Chrome/Edge on the Windows side):
//     a drive-mounted path /mnt/<x>/... becomes <X>:\... (the Windows app
//     sees the drives, not the WSL filesystem), and a WSL-filesystem path
//     becomes \\wsl.localhost\<distro>\<path> (the interop UNC prefix).
func toBrowserPath(p string) string {
	if !isWSL() {
		return p
	}
	if m := reMntDrive.FindStringSubmatch(p); m != nil {
		drive := strings.ToUpper(m[1])
		rest := m[2]
		if rest == "" {
			return drive + `:\`
		}
		return drive + `:\` + strings.TrimPrefix(rest, "/")
	}
	return `\\wsl.localhost\` + wslDistro() + `\` + strings.TrimPrefix(p, "/")
}

var reMntDrive = regexp.MustCompile(`^/mnt/([a-zA-Z])(/.*)?$`)

// isWSL reports whether bevin runs under Windows Subsystem for Linux (the
// interop makes a Windows browser launchable from this process).
func isWSL() bool {
	if os.Getenv("WSL_DISTRO_NAME") != "" {
		return true
	}
	b, err := os.ReadFile("/proc/sys/fs/binfmt_misc/WSLInterop")
	return err == nil && len(b) > 0
}

// wslDistro is the interop UNC path's distro segment (WSL_DISTRO_NAME, or
// the /etc/os-release name as a fallback — wrong there just means the UNC
// form misses, which the error message will name).
func wslDistro() string {
	if d := os.Getenv("WSL_DISTRO_NAME"); d != "" {
		return d
	}
	if b, err := os.ReadFile("/etc/os-release"); err == nil {
		for _, line := range strings.Split(string(b), "\n") {
			if name, ok := strings.CutPrefix(line, "PRETTY_NAME="); ok {
				pretty := strings.Trim(name, `"'`)
				if i := strings.IndexAny(pretty, " "); i > 0 {
					pretty = pretty[:i]
				}
				if pretty != "" {
					return pretty
				}
			}
		}
	}
	return "Ubuntu"
}
