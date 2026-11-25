package agent

import (
	"errors"
	"strings"
	"testing"
)

// TestUnavailableHint pins the failure-wording split (the observed
// car-research failure: a DNS error on a guessed dealer domain came back
// with "If the browser window was closed, the next call relaunches it" —
// a retry of a DNS failure is a wasted call, and the hint pointed the
// model at the wrong cause). Page-level network errors (Chrome's
// net::ERR_* errorText — the browser is fine, the URL/network is not)
// must NOT carry the relaunch hint; transport failures (the browser
// actually died) must.
func TestUnavailableHint(t *testing.T) {
	pageErr := unavailable("navigate", errors.New("net::ERR_NAME_NOT_RESOLVED at https://kia.dealerfire.com"))
	if !strings.Contains(pageErr, "page/network error") {
		t.Errorf("page error must say it is a page/network error: %q", pageErr)
	}
	if strings.Contains(pageErr, "relaunches it") {
		t.Errorf("page error must not carry the relaunch hint: %q", pageErr)
	}
	if !strings.Contains(pageErr, "navigate failed") {
		t.Errorf("the command name and the underlying error must survive: %q", pageErr)
	}
	transportErr := unavailable("read", errors.New("websocket: close 1006 (abnormal closure)"))
	if !strings.Contains(transportErr, "relaunches it") {
		t.Errorf("transport error must keep the relaunch hint: %q", transportErr)
	}
	if strings.Contains(transportErr, "page/network error") {
		t.Errorf("transport error must not be mislabeled a page error: %q", transportErr)
	}
}
