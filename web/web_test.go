package web

import (
	"bytes"
	"io"
	"testing"
)

// TestBrandAssetsEmbedded ensures the branding assets are compiled into the
// binary via embed.FS, so they serve with no on-disk path dependency.
func TestBrandAssetsEmbedded(t *testing.T) {
	want := []string{
		"index.html",
		"brand.css",
		"config-forms.css",
		"config-forms.js",
		"markdown.js",
		"logo-mark.svg",
		"favicon.svg",
		"fonts/nunito-variable.woff2",
		"admin.html",
		"admin-models.html",
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
		`el("th",{}, "Profile")`,
		// The display-name lock inherits the profile (same model, same
		// behavior — a locked decision, not a form detail).
		`m.profile=match.profile`,
	} {
		if !bytes.Contains(forms, []byte(want)) {
			t.Errorf("config-forms.js missing the profile surface: %s", want)
		}
	}
	// The admin page points the catalog at its own gated endpoint + loads it
	// on init.
	page := read("admin-models.html")
	for _, want := range []string{
		`const PROFILES_URL = "/admin/api/profiles"`,
		`loadProfiles().then(render)`,
	} {
		if !bytes.Contains(page, []byte(want)) {
			t.Errorf("admin-models.html missing: %s", want)
		}
	}
}
