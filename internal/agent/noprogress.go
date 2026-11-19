package agent

import (
	"encoding/json"
	"fmt"
	"hash/fnv"
)

// noProgress is the per-turn no-progress / stuck-detection state (the design
// log's "TOOL LOOP — NO-PROGRESS / STUCK-DETECTION" section). It REPLACES the
// hard maxToolIterations cap: instead of cutting a turn at an arbitrary
// count (which conflated "long but progressing" with "stuck" and cut both),
// it accumulates a CONFIDENCE score from cheap deterministic signals — L0
// (repeat: the same action-tool+args recurs) and L1 (no-state-change: the
// same result recurs) — plus the model's OWN progress declarations
// (report_progress), with DECAY on clean iterations, and stops the turn when
// the confidence crosses a threshold. It resets to a fresh state each turn (a
// stuck model in turn N does not carry confidence into turn N+1 — the user
// gave a fresh prompt).
//
// Framing: the model is the universal progress oracle (it knows the goal);
// the harness is the cheap deterministic trigger for when to consult it. The
// deterministic layers (L0/L1) are the UN-GAMEABLE ANCHOR — a model can fake
// its reported % but CANNOT fake that it is repeating a call or that the page
// isn't changing — so they carry the weight; the % CORROBORATES (it is
// gameable, so it never acts alone). A single layer firing once adds weight
// and triggers a check-in but does not stop; SUSTAINED multi-layer
// disagreement (with a gamed or declining %) is what climbs to the threshold.
// A long-but-progressing task fires no layers, decays, and runs free.
type noProgress struct {
	conf float64
	// lastPct is the % from the most recent COMMITTED declaration (the one
	// before this iteration's); hasPct is whether any declaration has landed.
	// declaredThisIter is the % the model reported THIS iteration (-1 = none)
	// — observe sets it, settle commits it into lastPct (so a DECLINING % is
	// measured against the previous declaration, not itself).
	lastPct          int
	hasPct           bool
	declaredThisIter int
	// checkinOut: a check-in was issued and is awaiting the model's
	// declaration; lastCheckin is the iteration that issued it (the rate-limit
	// — at most one check-in per K iterations, so a stuck model isn't spammed).
	checkinOut  bool
	lastCheckin int
	// window: the recent ACTION calls (report_progress excluded), each with a
	// callKey (tool+canonical args, for L0) and a resultKey (tool+result hash,
	// for L1). Bounded to the last W action calls.
	window []npRec

	cfg npCfg
}

// npRec is one action call's fingerprints for the L0/L1 detectors.
type npRec struct {
	callKey   string
	resultKey string
}

// npCfg are the detection + confidence parameters (the design log's starting
// values). The DETECTION params (N, W, K) are admin-configurable — they define
// WHAT COUNTS as no-progress, which is work-specific. The CONFIDENCE params
// (weights, decay, threshold) start as named internal constants — they are the
// harness's risk model, a design decision rather than a per-deployment dial
// (promote to config later if needed). Deliberately SIMPLE and LINEAR (not a
// multivariate non-linear risk model) so it is easy to reason about and debug.
type npCfg struct {
	N         int // L0: the same (action-tool + args) >= N times in the window fires
	W         int // sliding window size, in action calls (must be >= N and >= K)
	K         int // L1: the last K action calls all returning the same result fires
	WL0       float64
	WL1       float64
	WDecl     float64 // f_progress: scaled by (1-pct/100), applied when a layer fired
	WDecline  float64 // f_progress: a DECLINING % adds this fixed step
	WMiss     float64 // a missed check-in (no declaration when asked) adds this
	Decay     float64 // multiplied into confidence on each CLEAN iteration (<1)
	Threshold float64 // the turn stops when confidence >= Threshold
}

// defaultNPCfg is the v1 starting point (the design log's "Starting values"):
// threshold 0.9; decay 0.7/clean-iteration; w_L0 = w_L1 = 0.25 (EQUAL, low-ish
// — a harmless no-op like re-reading an already-read file should add a little
// confidence that decays away, not escalate); N 5, W 10, K 5. Tune live.
func defaultNPCfg() npCfg {
	return npCfg{
		N: 5, W: 10, K: 5,
		WL0: 0.25, WL1: 0.25,
		WDecl: 0.2, WDecline: 0.1, WMiss: 0.1,
		Decay: 0.7, Threshold: 0.9,
	}
}

// newNoProgress builds fresh per-turn state. Zero/invalid config fields fall
// back to the defaults (a partial admin config must not disable the detector);
// N and K are clamped to W (a threshold larger than the window could never
// fire).
func newNoProgress(cfg npCfg) *noProgress {
	d := defaultNPCfg()
	if cfg.N <= 0 {
		cfg.N = d.N
	}
	if cfg.W <= 0 {
		cfg.W = d.W
	}
	if cfg.K <= 0 {
		cfg.K = d.K
	}
	if cfg.WL0 <= 0 {
		cfg.WL0 = d.WL0
	}
	if cfg.WL1 <= 0 {
		cfg.WL1 = d.WL1
	}
	if cfg.WDecl <= 0 {
		cfg.WDecl = d.WDecl
	}
	if cfg.WDecline < 0 {
		cfg.WDecline = d.WDecline
	}
	if cfg.WMiss < 0 {
		cfg.WMiss = d.WMiss
	}
	if cfg.Decay <= 0 || cfg.Decay >= 1 {
		cfg.Decay = d.Decay
	}
	if cfg.Threshold <= 0 || cfg.Threshold > 1 {
		cfg.Threshold = d.Threshold
	}
	if cfg.N > cfg.W {
		cfg.N = cfg.W
	}
	if cfg.K > cfg.W {
		cfg.K = cfg.W
	}
	return &noProgress{declaredThisIter: -1, lastCheckin: -1e9, cfg: cfg}
}

// reportProgressTool is the name of the progress-declaration tool. It is
// EXCLUDED from the L0/L1 detectors (repeating it is not a "repeat loop," and
// it changes no state by design) — only ACTION tools feed the deterministic
// layers; this tool feeds the progress signal.
const reportProgressTool = "report_progress"

// stopped reports whether the confidence has crossed the threshold — the
// trigger for the forced text-only final call (the design's hard stop).
func (np *noProgress) stopped() bool {
	return np.conf >= np.cfg.Threshold
}

// confidence returns the current accumulated confidence (for the "stopped,
// not done" notice — transparency about why the turn stopped).
func (np *noProgress) confidence() float64 {
	return np.conf
}

// signals returns a human-readable list of the no-progress evidence active at
// stop time (for the "stopped, not done" notice).
func (np *noProgress) signals() []string {
	var out []string
	l0, l1 := np.fired()
	if l0 {
		out = append(out, fmt.Sprintf("repeated the same action %d times in the last %d", np.cfg.N, np.cfg.W))
	}
	if l1 {
		out = append(out, fmt.Sprintf("no observable change across the last %d actions", np.cfg.K))
	}
	if np.hasPct {
		out = append(out, fmt.Sprintf("self-reported %d%% progress", np.lastPct))
	}
	if len(out) == 0 {
		out = append(out, "no clear signal — the confidence accumulated from mixed indicators")
	}
	return out
}

// observe records one EXECUTED tool call. report_progress updates the pending
// declaration (declaredThisIter) and is NOT added to the action window; an
// action call (ok) is appended to the window and trimmed to W. A FAILED call
// (ok=false) is not recorded at all — a tool that errors is the model
// self-healing (the error is its signal), not "no progress."
func (np *noProgress) observe(name string, args json.RawMessage, resultText string, ok bool) {
	if name == reportProgressTool {
		if ok {
			var a struct {
				Percent int `json:"percent"`
			}
			if json.Unmarshal(args, &a) == nil {
				np.declaredThisIter = a.Percent
			}
		}
		return
	}
	if !ok {
		return
	}
	np.window = append(np.window, npRec{
		callKey:   npCallKey(name, args),
		resultKey: npResultKey(name, resultText),
	})
	if len(np.window) > np.cfg.W {
		np.window = np.window[len(np.window)-np.cfg.W:]
	}
}

// settle evaluates the layers after an iteration's calls, accumulates the
// confidence, applies decay, and returns (stop, checkin): whether the turn has
// crossed the threshold and whether a check-in should be issued for the next
// call. Called ONCE per tool-loop iteration, after observe has run for every
// call the model made that iteration.
func (np *noProgress) settle(iter int) (stop, checkin bool) {
	l0, l1 := np.fired()
	layersFired := l0 || l1
	declared := np.declaredThisIter >= 0

	// f_progress (v1 — simple/linear). A declaration only contributes when a
	// deterministic layer fired (honest progress contributes nothing — decay
	// handles it): a LOW % while stuck contributes proportionally, and a
	// DECLINING % (the model realizing it's stuck) adds a fixed step. The
	// self-delusion case (a high % while layers fire) contributes little here
	// by design — it is still caught by L0/L1 accumulation + no decay; a
	// dedicated delusion weight is a tuning follow-up.
	var f float64
	switch {
	case declared:
		if layersFired {
			f = np.cfg.WDecl * (1 - float64(np.declaredThisIter)/100)
			if np.hasPct && np.declaredThisIter < np.lastPct {
				f += np.cfg.WDecline
			}
		}
		// Commit the declaration as the new baseline for next time's decline.
		np.lastPct = np.declaredThisIter
		np.hasPct = true
		// A declaration (voluntary or in response to a check-in) satisfies an
		// outstanding check-in.
		np.checkinOut = false
	case np.checkinOut:
		// A check-in was outstanding and the model did NOT declare: a small
		// signal (won't even self-report when asked). The check-in is spent.
		f = np.cfg.WMiss
		np.checkinOut = false
	}
	np.declaredThisIter = -1

	// Accumulate the deterministic layers + the progress signal.
	if l0 {
		np.conf += np.cfg.WL0
	}
	if l1 {
		np.conf += np.cfg.WL1
	}
	np.conf += f

	// DECAY on a clean iteration (no layer fired) — the recovery mechanism: a
	// model that blipped once then makes progress decays back toward 0; a
	// stuck model never takes a clean step (its layers keep firing), so it
	// can't decay.
	if !layersFired {
		np.conf *= np.cfg.Decay
	}
	if np.conf < 0 {
		np.conf = 0
	}
	if np.conf > 1 {
		np.conf = 1
	}

	// CHECK-IN GATE: issue one if a layer fired, none is already outstanding,
	// and the rate-limit (at most one per K iterations) allows it. The
	// check-in REQUIRES the model to call report_progress next.
	checkin = layersFired && !np.checkinOut && (iter-np.lastCheckin) >= np.cfg.K
	if checkin {
		np.checkinOut = true
		np.lastCheckin = iter
	}

	stop = np.stopped()
	return stop, checkin
}

// fired reports whether L0 (repeat) and/or L1 (no-state-change) fire on the
// recent action calls. Both are CONSECUTIVE (the last N / last K), not ">= N
// anywhere in a wider window": a PAST blip of N identical calls must stop
// firing once the model makes a different call, or it would linger in the
// window, keep suppressing the decay, and push a RECOVERED model to the
// threshold anyway. Consecutive = "ongoing non-progress," which is what the
// stop is for.
//
//   - L0: the last N action calls are ALL the same call (tool + canonical
//     args) — the model is repeating the same request. (Same args, different
//     results — a flapping/loading tool — still fires: it's the same request.)
//   - L1: the last K action calls ALL returned the same result — "the world
//     isn't changing." (Different args, same result — clicking different
//     things but the page not moving — still fires.)
//
// The known v1 gap: an ALTERNATING loop (click→read→click→read, page
// unchanged) breaks the consecutive run, so neither fires on it; it is still
// bounded by the progress declaration + confidence (and is a tuning follow-up
// for a non-consecutive detector that decays correctly).
func (np *noProgress) fired() (l0, l1 bool) {
	if len(np.window) == 0 {
		return false, false
	}
	if len(np.window) >= np.cfg.N {
		tail := np.window[len(np.window)-np.cfg.N:]
		same := true
		for _, r := range tail[1:] {
			if r.callKey != tail[0].callKey {
				same = false
				break
			}
		}
		if same {
			l0 = true
		}
	}
	if len(np.window) >= np.cfg.K {
		tail := np.window[len(np.window)-np.cfg.K:]
		same := true
		for _, r := range tail[1:] {
			if r.resultKey != tail[0].resultKey {
				same = false
				break
			}
		}
		if same {
			l1 = true
		}
	}
	return l0, l1
}

// npCallKey is toolName + canonical args (the L0 fingerprint). Canonicalizing
// sorts the JSON object's keys so {"a":1,"b":2} and {"b":2,"a":1} hash
// identically (a model that repeats a call with reordered/whitespace-differing
// args is still the same call).
func npCallKey(name string, args json.RawMessage) string {
	return name + "\x00" + canonicalArgsJSON(args)
}

// canonicalArgsJSON re-marshals the args as a JSON object with sorted keys
// (Go's json.Marshal sorts map keys). Non-object or malformed args fall back
// to the raw bytes (they still compare stably).
func canonicalArgsJSON(args json.RawMessage) string {
	var m map[string]any
	if len(args) == 0 || json.Unmarshal(args, &m) != nil {
		return string(args)
	}
	out, err := json.Marshal(m)
	if err != nil {
		return string(args)
	}
	return string(out)
}

// npResultKey is toolName + a hash of the result text (the L1 fingerprint —
// "the same thing came back"). A full-text hash (not a prefix) so two different
// pages that share a long prefix don't collide.
func npResultKey(name, resultText string) string {
	h := fnv.New64a()
	h.Write([]byte(name))
	h.Write([]byte{0})
	h.Write([]byte(resultText))
	return fmt.Sprintf("%x", h.Sum64())
}
