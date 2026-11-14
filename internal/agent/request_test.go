package agent

import "testing"

func TestStripLeadingTranscriptTimestamp(t *testing.T) {
	cases := []struct {
		name string
		in   string
		want string
	}{
		{"strips echoed marker", "[2026-11-14T05:33:56Z] 1. Farmer takes the chicken", "1. Farmer takes the chicken"},
		{"strips marker with trailing space", "[2026-11-14T05:33:56Z] Paris", "Paris"},
		{"plain reply untouched", "Paris", "Paris"},
		{"empty untouched", "", ""},
		{"mid-text marker kept", "Paris. (sent at [2026-11-14T05:33:56Z])", "Paris. (sent at [2026-11-14T05:33:56Z])"},
		{"leading stripped, mid kept", "[2026-11-14T05:33:56Z] Paris, and [2026-11-14T05:34:00Z] Rome", "Paris, and [2026-11-14T05:34:00Z] Rome"},
		{"offset timezone not a transcript marker", "[2026-11-14T05:33:56+00:00] Paris", "[2026-11-14T05:33:56+00:00] Paris"},
		{"date-only bracket untouched", "[2026-11-14] Paris", "[2026-11-14] Paris"},
		{"millisecond precision untouched", "[2026-11-14T05:33:56.123Z] Paris", "[2026-11-14T05:33:56.123Z] Paris"},
		{"multi-line reply, marker on first line", "[2026-11-14T05:33:56Z] line one\nline two", "line one\nline two"},
	}
	for _, c := range cases {
		if got := stripLeadingTranscriptTimestamp(c.in); got != c.want {
			t.Errorf("%s: got %q, want %q", c.name, got, c.want)
		}
	}
}
