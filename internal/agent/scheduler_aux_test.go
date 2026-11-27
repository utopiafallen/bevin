package agent

import (
	"context"
	"testing"
	"time"

	"github.com/utopiafallen/bevin/internal/config"
)

// auxSlotTestConfig is the agent fixture for the aux pool tests: one main
// model + two aux models — one an EXACT match of the main model (same
// provider + base URL + model id → it shares the main model's pool) and one
// on its own endpoint (its own derived pool).
func auxSlotTestConfig() *config.ServerConfig {
	return &config.ServerConfig{
		Version: config.ConfigVersion,
		Auth:    config.Auth{Mode: "internal", SessionSecret: "test-secret"},
		Models: []config.Model{
			{ID: "m1", Provider: "llama", BaseURL: "http://llama:8081/v1", ModelID: "vision-27b", DisplayName: "Vision", MaxContext: 1000, Slots: 3, Enabled: true},
		},
		Aux: []config.AuxModel{
			{Kinds: []string{"vision"}, Provider: "llama", BaseURL: "http://llama:8081/v1", ModelID: "vision-27b", Slots: 1},
			{Kinds: []string{"audio"}, Provider: "llama", BaseURL: "http://llama:8082/v1", ModelID: "audio-9b", Slots: 2},
		},
	}
}

// TestModelSlotsAuxPools — the scheduler's slot callback resolves the aux
// pool ids: an unmatched aux resolves to its OWN slot count, and an exact
// match of a main model resolves through the main model's pool (the main's
// slots are the bound — perception requests share the model's conversations
// pool, never their own count).
func TestModelSlotsAuxPools(t *testing.T) {
	cfg := auxSlotTestConfig()
	a, err := New(cfg, t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	// New() derived the pool ids (the config here skipped Validate — the
	// defensive re-derivation is the point).
	if got := cfg.Aux[0].PoolID; got != "m1" {
		t.Fatalf("shared aux PoolID = %q, want m1", got)
	}
	if got := cfg.Aux[1].PoolID; got != "aux-audio-9b" {
		t.Fatalf("own aux PoolID = %q, want aux-audio-9b", got)
	}
	if got := a.modelSlots("m1"); got != 3 {
		t.Fatalf("main pool slots = %d, want 3", got)
	}
	// The shared aux has no pool of its own: a job submitted to "m1" draws
	// the main model's 3 slots (not the aux's 1).
	if got := a.modelSlots(cfg.Aux[0].PoolID); got != 3 {
		t.Fatalf("shared-pool slots = %d, want 3 (the main model's count)", got)
	}
	// The aux's own pool uses the aux's count.
	if got := a.modelSlots(cfg.Aux[1].PoolID); got != 2 {
		t.Fatalf("own aux pool slots = %d, want 2", got)
	}
	// A pool removed from the config still resolves (1) — its queued jobs
	// complete quickly instead of waiting on a missing pool.
	if got := a.modelSlots("gone"); got != 1 {
		t.Fatalf("unknown pool slots = %d, want 1", got)
	}
}

// TestPoolsIncludesAuxPools — the admin dashboard's pool list carries every
// main model's pool + each aux model's OWN pool (Aux=true, the aux's own
// slot count); a shared aux adds no row — its main model's row names it in
// Auxes instead (one pool, not two).
func TestPoolsIncludesAuxPools(t *testing.T) {
	cfg := auxSlotTestConfig()
	a, err := New(cfg, t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	pools := a.Pools()
	byID := map[string]PoolStatus{}
	for _, p := range pools {
		byID[p.ID] = p
	}
	// Two rows: m1's pool + the audio aux's own pool (NOT three — the
	// shared vision aux is folded into m1's row).
	if len(pools) != 2 {
		t.Fatalf("pool rows = %d, want 2 (m1 + the audio aux's own pool): %+v", len(pools), pools)
	}
	m1, ok := byID["m1"]
	if !ok {
		t.Fatalf("m1's pool row missing: %+v", pools)
	}
	if m1.Aux || m1.Slots != 3 {
		t.Fatalf("m1 row = %+v, want the main pool (3 slots, not an aux row)", m1)
	}
	if len(m1.Auxes) != 1 || m1.Auxes[0] != "vision-27b" {
		t.Fatalf("m1 row Auxes = %v, want [vision-27b] (the exact-match aux shares it)", m1.Auxes)
	}
	audio, ok := byID["aux-audio-9b"]
	if !ok {
		t.Fatalf("the audio aux's own pool row missing: %+v", pools)
	}
	if !audio.Aux || audio.Slots != 2 || audio.DisplayName != "audio-9b" {
		t.Fatalf("audio row = %+v, want the aux's own pool (2 slots)", audio)
	}
}

// TestReloadPokesAuxPools — an aux slot edit reaches the running scheduler
// without a restart: a job queued in the aux's pool starts the moment
// Reload swaps in the config with the raised count (the Poke on the aux
// pool).
func TestReloadPokesAuxPools(t *testing.T) {
	cfg := auxSlotTestConfig()
	a, err := New(cfg, t.TempDir())
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer a.Close()
	pool := cfg.Aux[1].PoolID // the audio aux's own pool (2 slots)
	// Hold both slots of the aux pool (the holders block until released).
	release := make(chan struct{})
	for i := 0; i < 2; i++ {
		go func() {
			_ = a.sched.Submit(context.Background(), JobInfo{Handle: "u", ConvID: "hold", ModelID: pool}, func(context.Context) error {
				<-release
				return nil
			})
		}()
	}
	// Wait until BOTH holders actually hold a slot (a job submitted earlier
	// could steal one otherwise), then a third job queues behind them.
	deadline := time.After(2 * time.Second)
	for {
		if l := a.sched.Load()[pool]; l.Busy == 2 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("the slot holders never both started (load: %+v)", a.sched.Load()[pool])
		case <-time.After(5 * time.Millisecond):
		}
	}
	ran := make(chan struct{})
	go func() {
		_ = a.sched.Submit(context.Background(), JobInfo{Handle: "u", ConvID: "wait", ModelID: pool}, func(context.Context) error {
			close(ran)
			return nil
		})
	}()
	deadline = time.After(2 * time.Second)
	for {
		if l := a.sched.Load()[pool]; len(l.Waiting) == 1 {
			break
		}
		select {
		case <-deadline:
			t.Fatalf("the third job never queued (load: %+v)", a.sched.Load()[pool])
		case <-time.After(5 * time.Millisecond):
		}
	}
	// The admin raises the aux's slot count + saves: Reload must poke the
	// aux pool so the queued job starts immediately (no restart, no waiting
	// for a release).
	cfg.Aux[1].Slots = 3
	if err := a.Reload(cfg); err != nil {
		t.Fatalf("Reload: %v", err)
	}
	select {
	case <-ran:
		// The queued job ran on the newly freed slot.
	case <-time.After(2 * time.Second):
		t.Fatalf("the queued job did not start after the aux slot raise + Reload (load: %+v)", a.sched.Load()[pool])
	}
	close(release)
}
