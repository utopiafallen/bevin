package browser

import (
	"strings"
	"testing"
)

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

// TestPlaywrightRedirect pins the REJECTION of the Playwright selector
// forms the model writes by habit (decided 2026-11-24: accepting them
// would teach the model to write more — the tool's own vocabulary is
// #N, CSS, or the element's text). The observed car-research failure:
// button:has-text("See Inventory") died as a raw querySelector
// SyntaxError; now it gets one line pointing at the tool's forms.
func TestPlaywrightRedirect(t *testing.T) {
	playwrightish := []string{
		`button:has-text("See Inventory")`,
		`:has-text("See Inventory")`,
		`.card:has-text("Buy now")`,
		`text=Sign in`,
		`text="Sign in"`,
		`xpath=//button`,
		`//button`,
		`aria/Save`,
		`role=button`,
		`css=#see-btn`,
		`id=sic`,
	}
	for _, key := range playwrightish {
		msg, isPw := playwrightRedirect(key)
		if !isPw {
			t.Errorf("playwrightRedirect(%q) = not Playwright, want a redirect", key)
			continue
		}
		if msg == "" {
			t.Errorf("playwrightRedirect(%q) gave no message", key)
		}
	}
	// The two specific redirects.
	if msg, _ := playwrightRedirect("id=sic"); !strings.Contains(msg, "#sic") {
		t.Errorf("id= redirect = %q, want the #sic pointer", msg)
	}
	if msg, _ := playwrightRedirect(`css=#see-btn`); !strings.Contains(msg, "css=") {
		t.Errorf("css= redirect = %q, want the drop-the-prefix pointer", msg)
	}
	// Plain tool-vocabulary keys must NOT be flagged (they still resolve).
	plain := []string{"#12", "#foo", "button", ".card .buy", "input[type=text]", "See Inventory", "a[aria-label=Back] > span"}
	for _, key := range plain {
		if _, isPw := playwrightRedirect(key); isPw {
			t.Errorf("playwrightRedirect(%q) flagged a plain key", key)
		}
	}
}

func TestIsSimpleID(t *testing.T) {
	ok := []string{"foo", "foo-bar", "foo_bar", "f1", "F-B_2"}
	for _, s := range ok {
		if !isSimpleID(s) {
			t.Errorf("isSimpleID(%q) = false, want true", s)
		}
	}
	bad := []string{"", "foo bar", "foo#bar", "foo.bar", "föö"}
	for _, s := range bad {
		if isSimpleID(s) {
			t.Errorf("isSimpleID(%q) = true, want false", s)
		}
	}
}
