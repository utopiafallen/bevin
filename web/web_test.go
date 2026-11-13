package web

import "testing"

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
