package web

import (
	"bytes"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestBrandAssetsEmbedded ensures the branding assets are compiled into the
// binary via embed.FS, so they serve with no on-disk path dependency.
func TestBrandAssetsEmbedded(t *testing.T) {
	want := []string{
		"index.html",
		"brand.css",
		"app.css",
		"config-forms.css",
		"config-forms.js",
		"markdown.js",
		"logo-mark.svg",
		"favicon.svg",
		"fonts/nunito-variable.woff2",
		"admin.html",
		"admin-email.html",
		"admin-users.html",
	}
	for _, p := range want {
		f, err := FS.Open("static/" + p)
		if err != nil {
			t.Fatalf("missing embedded asset static/%s: %v", p, err)
		}
		st, err := f.Stat()
		if err != nil {
			t.Fatalf("stat static/%s: %v", p, err)
		}
		if !st.Mode().IsRegular() || st.Size() == 0 {
			t.Errorf("static/%s is empty or not a regular file", p)
		}
		f.Close()
	}
}

// TestModelProfileUISurface pins the model-profile wiring in the shared form
// components (regression guard, same style as the email-port coercion test):
// the profile is a first-class field on both model kinds, its select renders
// from the build's registry (fetched — never hardcoded), and it joins the
// display-name lock (same model, same behavior).
func TestModelProfileUISurface(t *testing.T) {
	read := func(name string) []byte {
		t.Helper()
		f, err := FS.Open("static/" + name)
		if err != nil {
			t.Fatalf("open %s: %v", name, err)
		}
		defer f.Close()
		var b bytes.Buffer
		if _, err := io.Copy(&b, f); err != nil {
			t.Fatalf("read %s: %v", name, err)
		}
		return b.Bytes()
	}
	forms := read("config-forms.js")
	// Both model kinds carry the profile, defaulting to general.
	for _, want := range []string{
		`profile:"general"`,
		`"Model profile"`,
		`loadProfiles()`,
		`profileField(`,
		// The model list (card) shows each model's profile in its meta grid.
		`metaItem("Profile", m.profile||"general")`,
		// The display-name lock inherits the profile (same model, same
		// behavior — a locked decision, not a form detail).
		`m.profile=match.profile`,
	} {
		if !bytes.Contains(forms, []byte(want)) {
			t.Errorf("config-forms.js missing the profile surface: %s", want)
		}
	}
	// The dashboard's Models page points the catalog at its own gated
	// endpoint + loads it on init (the model editor is inlined there — the
	// dedicated /admin/models page is gone, it redirects to /admin#models).
	page := read("admin.html")
	for _, want := range []string{
		`const PROFILES_URL = "/admin/api/profiles"`,
		`loadProfiles().then(render)`,
	} {
		if !bytes.Contains(page, []byte(want)) {
			t.Errorf("admin.html missing: %s", want)
		}
	}
}

// TestIndexInlineJSValid syntax-checks every inline <script> block in the
// main user page with `node --check` (the page is the only UI with
// hand-maintained shell logic — the sidebar/drawer wiring — so a typo there
// would break the whole page silently). Skipped where node is unavailable.
func TestIndexInlineJSValid(t *testing.T) {
	checkInlineJSValid(t, "index.html")
}

// TestAdminInlineJSValid — same guard for the admin dashboard (its Models
// page inlines the model editor: state, per-action commits, and the shared
// form wiring, so a typo there breaks model management silently).
func TestAdminInlineJSValid(t *testing.T) {
	checkInlineJSValid(t, "admin.html")
}

// checkInlineJSValid syntax-checks every inline <script> block of an embedded
// page with `node --check`. Skipped where node is unavailable.
func checkInlineJSValid(t *testing.T, pageName string) {
	if _, err := exec.LookPath("node"); err != nil {
		t.Skip("node not available")
	}
	f, err := FS.Open("static/" + pageName)
	if err != nil {
		t.Fatalf("open %s: %v", pageName, err)
	}
	defer f.Close()
	var b bytes.Buffer
	if _, err := io.Copy(&b, f); err != nil {
		t.Fatalf("read %s: %v", pageName, err)
	}
	page := b.String()

	// Inline script bodies: <script> … </script> (the src= scripts are
	// external files and match "<script src=", not "<script>").
	var bodies []string
	for i := 0; ; {
		s := strings.Index(page[i:], "<script>")
		if s < 0 {
			break
		}
		s += i
		e := strings.Index(page[s:], "</script>")
		if e < 0 {
			break
		}
		bodies = append(bodies, page[s+len("<script>"):s+e])
		i = s + e
	}
	if len(bodies) == 0 {
		t.Fatal("no inline script blocks found in " + pageName)
	}
	for i, body := range bodies {
		tmp := filepath.Join(t.TempDir(), "inline.js")
		if err := os.WriteFile(tmp, []byte(body), 0o600); err != nil {
			t.Fatalf("write temp script: %v", err)
		}
		out, err := exec.Command("node", "--check", tmp).CombinedOutput()
		if err != nil {
			t.Errorf("%s: inline script %d failed node --check: %v\n%s", pageName, i, err, out)
		} else {
			t.Logf("%s: inline script %d: OK", pageName, i)
		}
	}
}
