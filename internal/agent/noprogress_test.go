package agent

import (
	"encoding/json"
	"fmt"
	"testing"
)

// npJSON marshals a value to the raw JSON args a tool call would carry.
func npJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// runNP drives the accumulator through a sequence of iterations. Each step
// returns the action-call observe args + result (or nil for "no action call
// this iteration") and an optional report_progress percent (-1 = none). It
// returns the iteration at which the turn stopped (-1 = never).
func runNP(t *testing.T, step func(iter int) (name string, args json.RawMessage, result string, pct int)) int {
	t.Helper()
	np := newNoProgress(defaultNPCfg())
	for iter := 0; iter < 40; iter++ {
		name, args, result, pct := step(iter)
		if name != "" {
			np.observe(name, args, result, true)
		}
		if pct >= 0 {
			np.observe(reportProgressTool, npJSON(t, map[string]any{"percent": pct, "next_step": "n"}), fmt.Sprintf("noted %d%%", pct), true)
		}
		stop, _ := np.settle(iter)
		if stop {
			return iter
		}
	}
	return -1
}

// A model doing genuinely different work never accumulates confidence.
func TestNoProgressCleanProgress(t *testing.T) {
	np := newNoProgress(defaultNPCfg())
	for iter := 0; iter < 40; iter++ {
		np.observe("web_browser", npJSON(t, map[string]any{"command": "read", "selector": fmt.Sprintf("s%d", iter)}), fmt.Sprintf("page %d", iter), true)
		if stop, _ := np.settle(iter); stop {
			t.Fatalf("clean progress stopped at iter %d (conf %v)", iter, np.confidence())
		}
	}
	if np.confidence() != 0 {
		t.Errorf("clean progress should stay at 0, got %v", np.confidence())
	}
}

// Repeating the exact same call (same args, same result) fires L0+L1 (both,
// since the same call returns the same thing) and stops at the threshold —
// the double-fire case (~2 iterations of firing to 0.9).
func TestNoProgressRepeatStops(t *testing.T) {
	stopAt := runNP(t, func(iter int) (string, json.RawMessage, string, int) {
		return "web_browser", npJSON(t, map[string]any{"command": "read"}), "page A", -1
	})
	if stopAt != 5 {
		t.Errorf("double fire should stop at iter 5, got %d", stopAt)
	}
}

// Same args but different results (a flapping / still-loading tool): L0 fires
// (same request) but L1 does not (the result changes) — a SINGLE layer, so it
// takes longer to reach the threshold (~8 iterations).
func TestNoProgressRepeatOnlyL0(t *testing.T) {
	stopAt := runNP(t, func(iter int) (string, json.RawMessage, string, int) {
		return "web_browser", npJSON(t, map[string]any{"command": "read"}), fmt.Sprintf("loading %d", iter), -1
	})
	if stopAt != 7 {
		t.Errorf("single layer (L0) should stop at iter 7, got %d", stopAt)
	}
}

// Different args but the SAME result (clicking different things, the page not
// moving): L1 fires (no state change) but L0 does not (different requests) —
// a single layer, ~8 iterations.
func TestNoProgressNoStateChangeOnlyL1(t *testing.T) {
	stopAt := runNP(t, func(iter int) (string, json.RawMessage, string, int) {
		return "web_browser", npJSON(t, map[string]any{"command": "click", "element": fmt.Sprintf("#%d", iter)}), "page A unchanged", -1
	})
	if stopAt != 7 {
		t.Errorf("single layer (L1) should stop at iter 7, got %d", stopAt)
	}
}

// The recovery case (the bug the consecutive detectors fix): a blip of 5
// identical calls raises the confidence, but once the model makes a DIFFERENT
// call the consecutive run breaks, the layers stop firing, and the confidence
// DECAYS back — the recovered model is NOT stopped.
func TestNoProgressRecoversAfterBlip(t *testing.T) {
	np := newNoProgress(defaultNPCfg())
	for iter := 0; iter < 5; iter++ {
		np.observe("web_browser", npJSON(t, map[string]any{"command": "read"}), "page A", true)
		np.settle(iter)
	}
	if np.stopped() {
		t.Fatalf("a blip should not stop (conf %v)", np.confidence())
	}
	if np.confidence() <= 0 {
		t.Fatalf("a blip should raise the confidence, got %v", np.confidence())
	}
	peak := np.confidence()
	for iter := 5; iter < 40; iter++ {
		np.observe("web_browser", npJSON(t, map[string]any{"command": "read", "selector": fmt.Sprintf("s%d", iter)}), fmt.Sprintf("new page %d", iter), true)
		if stop, _ := np.settle(iter); stop {
			t.Fatalf("a recovered model stopped at iter %d (conf %v)", iter, np.confidence())
		}
	}
	if np.confidence() >= peak {
		t.Errorf("confidence should decay after recovery (peak %v, got %v)", peak, np.confidence())
	}
	if np.confidence() > 0.2 {
		t.Errorf("confidence should decay well below the peak after recovery, got %v", np.confidence())
	}
}

// A stuck model that gamingly reports 100% is STILL caught by the
// deterministic anchor (L0/L1) — the % contributes ~0 (WDecl*(1-1.0)=0) and
// never acts alone, so the stop is driven by the layers exactly as without it.
func TestNoProgressGamedPercent(t *testing.T) {
	stopAt := runNP(t, func(iter int) (string, json.RawMessage, string, int) {
		return "web_browser", npJSON(t, map[string]any{"command": "read"}), "page A", 100
	})
	if stopAt != 5 {
		t.Errorf("a gamed 100%% should stop on L0/L1 (iter 5), got %d", stopAt)
	}
}

// A LOW / DECLINING % while a layer fired CORROBORATES the deterministic
// signal (the model is realizing it's stuck) and adds confidence — stopping a
// single-layer (L0-only) loop FASTER than the layers alone (which would stop
// at iter 7).
func TestNoProgressDecliningPercent(t *testing.T) {
	pcts := []int{40, 30, 20, 10, 5, 0, 0, 0, 0, 0}
	stopAt := runNP(t, func(iter int) (string, json.RawMessage, string, int) {
		p := -1
		if iter < len(pcts) {
			p = pcts[iter]
		}
		return "web_browser", npJSON(t, map[string]any{"command": "read"}), fmt.Sprintf("loading %d", iter), p
	})
	if stopAt >= 7 {
		t.Errorf("a declining %% should stop the single-layer loop faster than the layers alone (iter 7); got %d", stopAt)
	}
}

// A check-in is issued on the first layer fire; if the model does NOT declare
// in response, the next settle adds the w_miss signal (won't even self-report
// when asked).
func TestNoProgressMissedCheckin(t *testing.T) {
	np := newNoProgress(defaultNPCfg())
	checkinAt := -1
	for iter := 0; iter < 5; iter++ {
		np.observe("web_browser", npJSON(t, map[string]any{"command": "read"}), "page A", true)
		_, checkin := np.settle(iter)
		if checkin && checkinAt == -1 {
			checkinAt = iter
		}
	}
	if checkinAt != 4 {
		t.Fatalf("expected a check-in at iter 4 (first fire), got %d", checkinAt)
	}
	peak := np.confidence() // 0.5 after the blip; a check-in is outstanding
	// The model does a different call (layers stop firing) and does NOT declare.
	np.observe("web_browser", npJSON(t, map[string]any{"command": "read", "selector": "different"}), "different page", true)
	np.settle(5)
	want := (peak + 0.1) * 0.7 // +w_miss, then decay (no layer fired)
	if diff := np.confidence() - want; diff > 1e-9 || diff < -1e-9 {
		t.Errorf("missed check-in: conf = %v, want %v", np.confidence(), want)
	}
}

// Reordered / whitespace-differing JSON args are the SAME call (canonical
// args) — L0 fires across them.
func TestNoProgressCanonicalArgs(t *testing.T) {
	np := newNoProgress(defaultNPCfg())
	a1 := json.RawMessage(`{"command":"read","selector":""}`)
	a2 := json.RawMessage(`{ "selector" : "" , "command" : "read" }`)
	for iter := 0; iter < 5; iter++ {
		args := a1
		if iter%2 == 1 {
			args = a2
		}
		np.observe("web_browser", args, "page A", true)
		np.settle(iter)
	}
	if l0, _ := np.fired(); !l0 {
		t.Error("reordered-key args should be the same call (L0 should fire)")
	}
}

// report_progress is NOT an action — repeating it does not fire L0/L1 and does
// not stop the turn (it feeds the progress signal, not the deterministic
// layers).
func TestNoProgressReportProgressExcludedFromLayers(t *testing.T) {
	np := newNoProgress(defaultNPCfg())
	for iter := 0; iter < 15; iter++ {
		np.observe(reportProgressTool, npJSON(t, map[string]any{"percent": 50, "next_step": "x"}), "noted", true)
		np.settle(iter)
	}
	if l0, l1 := np.fired(); l0 || l1 {
		t.Error("repeating report_progress should not fire L0/L1")
	}
	if np.stopped() {
		t.Errorf("repeating report_progress should not stop the turn (conf %v)", np.confidence())
	}
}

// Config sanitization: a zero/invalid config falls back to safe defaults
// (a partial admin config must not disable the detector), and N/K clamp to W.
func TestNoProgressCfgSanitize(t *testing.T) {
	np := newNoProgress(npCfg{}) // all zero -> defaults
	if np.cfg.Threshold != 0.9 || np.cfg.Decay != 0.7 || np.cfg.N != 5 || np.cfg.W != 10 || np.cfg.K != 5 {
		t.Errorf("zero config should fall back to defaults, got %+v", np.cfg)
	}
	np2 := newNoProgress(npCfg{N: 50, W: 10, K: 3}) // N > W -> clamp
	if np2.cfg.N != 10 {
		t.Errorf("N should clamp to W (10), got %d", np2.cfg.N)
	}
	np3 := newNoProgress(npCfg{Decay: 1.5, W: 5}) // Decay >= 1 -> default
	if np3.cfg.Decay != 0.7 {
		t.Errorf("Decay >= 1 should fall back to 0.7, got %v", np3.cfg.Decay)
	}
}
