package browser

import "testing"

// TestAllowedBrowseURL pins the scheme gate (security fix 2026-11-20):
// only http/https (and about:blank) may reach the browser — file:// would
// read arbitrary local files into the model's context, data: and friends
// can execute or smuggle content.
func TestAllowedBrowseURL(t *testing.T) {
	ok := []struct{ raw, want string }{
		{"", "about:blank"},
		{"about:blank", "about:blank"},
		{"https://example.com/a?b=c", "https://example.com/a?b=c"},
		{"http://127.0.0.1:6333", "http://127.0.0.1:6333"},
		{"http://example.com", "http://example.com"},
		{"example.com/page", "https://example.com/page"}, // schemeless → https
	}
	for _, tc := range ok {
		got, err := allowedBrowseURL(tc.raw)
		if err != nil {
			t.Errorf("allowedBrowseURL(%q) unexpected error: %v", tc.raw, err)
			continue
		}
		if got != tc.want {
			t.Errorf("allowedBrowseURL(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
	bad := []string{
		"file:///etc/passwd",
		"file:///C:/Users/x/secret.txt",
		"file://localhost/etc/shadow",
		"data:text/html,<script>alert(1)</script>",
		"javascript:alert(1)",
		"view-source:https://example.com",
		"chrome://settings",
		"edge://extensions",
		"devtools://devtools/bundled/inspector.html",
		"ftp://example.com/pub",
		"ws://example.com",
	}
	for _, raw := range bad {
		if got, err := allowedBrowseURL(raw); err == nil {
			t.Errorf("allowedBrowseURL(%q) should be rejected, got %q", raw, got)
		}
	}
}
