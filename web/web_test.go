package web

import (
	"bytes"
	"encoding/json"
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
		"disk.html",
		// PWA (stage 1): the installable app shell — manifest, service
		// worker, and the icon set (any + maskable + iOS touch icon).
		"manifest.webmanifest",
		"sw.js",
		"icon-192.png",
		"icon-512.png",
		"icon-maskable-512.png",
		"apple-touch-icon.png",
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
		`metaItem("Profile", m.profile || "general")`,
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

// TestPwaShellWiring pins the PWA stage-1 wiring in the user page: the
// manifest link + iOS meta tags, the secure-context service-worker
// registration, and the install-prompt hook in the signed-in funnel
// (the prompt must never show a signed-out visitor).
func TestPwaShellWiring(t *testing.T) {
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
	page := read("index.html")
	for _, want := range []string{
		`<link rel="manifest" href="/manifest.webmanifest">`,
		`<meta name="apple-mobile-web-app-capable" content="yes">`,
		`<link rel="apple-touch-icon" href="/apple-touch-icon.png">`,
		`navigator.serviceWorker.register("/sw.js")`,
		`refreshPwaPrompt();`,
	} {
		if !bytes.Contains(page, []byte(want)) {
			t.Errorf("index.html missing the PWA wiring: %s", want)
		}
	}
	// The SW must keep all API traffic (chat + SSE) out of the cache.
	if !bytes.Contains(read("sw.js"), []byte(`url.pathname.startsWith("/api/")`)) {
		t.Error("sw.js missing the /api/ exclusion")
	}
	// The manifest must parse and be standalone-installable.
	var man map[string]any
	if err := json.Unmarshal(read("manifest.webmanifest"), &man); err != nil {
		t.Fatalf("manifest.webmanifest is not valid JSON: %v", err)
	}
	if man["display"] != "standalone" || man["start_url"] == nil || man["scope"] == nil || man["icons"] == nil {
		t.Errorf("manifest.webmanifest missing installability fields: %v", man)
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

// TestMarkdownNodeTests runs the parser suite (markdown_test.js — block
// cut points, settle-then-render consistency, and the inline marker
// branches against a minimal fake DOM) under node, so the streaming
// renderer's core is gated like the inline UI scripts. Skipped where node
// is unavailable.
func TestMarkdownNodeTests(t *testing.T) {
	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node not available")
	}
	out, err := exec.Command(node, "markdown_test.js").CombinedOutput()
	if err != nil {
		t.Fatalf("markdown_test.js: %v\n%s", err, out)
	}
	if !bytes.Contains(out, []byte("ALL PASS")) {
		t.Fatalf("markdown_test.js:\n%s", out)
	}
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
