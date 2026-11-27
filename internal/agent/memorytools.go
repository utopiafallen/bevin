package agent

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"sort"
	"strings"
	"time"

	"github.com/utopiafallen/bevin/internal/store"
)

// The memory tools: the model's own surface over the user's memories (P5).
// Retrieval is the search tool's memory leg (rankMemories — recall was
// folded into search, 2026-11-27: it was the same search restricted to
// memories); remember = consolidation-on-write (near-duplicates are
// surfaced, the model decides via supersede_id); forget = supersede without
// a replacement. All failures are RESULTS back to the model.

// ctxConversationKey carries the running conversation's id to the tool
// executors (remember's provenance keys on it — mid-turn calls record the
// latest message of the running conversation). Harness plumbing, not part
// of the model-visible tool contract — runTurn sets it, the Tool.Execute
// signature stays unchanged.
type ctxConversationKeyT struct{}

var ctxConversationKey = ctxConversationKeyT{}

func conversationFromContext(ctx context.Context) string {
	v, _ := ctx.Value(ctxConversationKey).(string)
	return v
}

// The memory ranking blend (internal constants) — recall's scoring, now
// the search tool / search page's memory leg (recall was folded into
// search, 2026-11-27): cosine is the primary signal (the query's one
// embedding call), confidence the model's stated certainty, recency the
// half-life decay, the FTS hit the lexical floor (it keeps a keyword match
// rankable when the semantics are weak). No embedder configured → lexical
// mode (FTS hits only, confidence + recency blend).
const (
	memWcosine         = 0.55
	memWconf           = 0.20
	memWrecency        = 0.15
	memWfts            = 0.10
	memRecencyHalfLife = 30 * 24 * time.Hour
	memFTSCap          = 20   // the FTS candidate/bonus cap (both modes)
	memNearDup         = 0.85 // remember's consolidation threshold
)

func registerMemoryTools(r *ToolRegistry, a *Agent) error {
	if err := r.Register(Tool{
		Name:        "remember",
		Description: "Store a durable fact about the user or their world as a memory. Near-duplicates are reported back; pass supersede_id to replace an existing memory instead.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"facet": map[string]any{
					"type":        "string",
					"description": "a short label for the fact's topic (e.g. work, preferences, family)",
				},
				"statement": map[string]any{
					"type":        "string",
					"description": "the fact itself, one self-contained sentence",
				},
				"subject": map[string]any{
					"type":        "string",
					"description": "the person or thing the fact is mainly about, when there is one",
				},
				"confidence": map[string]any{
					"type":        "number",
					"description": "0.0-1.0, how sure you are (default 0.8)",
				},
				"supersede_id": map[string]any{
					"type":        "number",
					"description": "replace this existing memory id with the new fact",
				},
			},
			"required": []string{"facet", "statement"},
		},
		Execute: a.execRemember,
	}); err != nil {
		return err
	}
	return r.Register(Tool{
		Name:        "forget",
		Description: "Forget a stored memory (it is marked superseded and will no longer be recalled).",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{
					"type":        "number",
					"description": "the memory id (from the search tool's memory results)",
				},
			},
			"required": []string{"id"},
		},
		Execute: a.execForget,
	})
}

// rankedMemory is one memory's blend score + its parts (for ranking).
type rankedMemory struct {
	mem     store.Memory
	score   float64
	cosine  float64
	ftsHit  bool
	recency float64
}

// rankMemories is the search tool / search page's memory leg — recall's
// hybrid blend, folded into search (2026-11-27, user directive: recall was
// the same search restricted to memories). It scores the user's active
// memories for the query:
//
//	semantic mode (an embedder is configured): every active memory is a
//	candidate, scored 0.55·cosine + 0.20·confidence + 0.15·recency
//	(+ 0.10 for an FTS hit). The query is embedded RAW — memories are
//	stored raw, with no "task: search result" prefix (that belongs to the
//	conversation leg's query encoding), so the cosine is apples-to-apples.
//	lexical mode (no embedder, or the embed call fails): only the FTS hits
//	are candidates, scored 0.5·confidence + 0.5·recency.
//
// conversationID != "" restricts the CANDIDATE SET to memories whose
// provenance touches that conversation (the search scope) — before scoring,
// so a scoped search never surfaces another conversation's memories. The
// result is sorted best-first and capped at limit.
func (a *Agent) rankMemories(ctx context.Context, us *store.UserStore, query, conversationID string, limit int) ([]store.Memory, error) {
	if limit <= 0 {
		limit = 10
	}
	// The lexical floor: which memories the FTS pre-filter hit. In
	// semantic mode this is the +0.10 bonus set; in lexical mode it IS the
	// candidate set. The cap is the candidate budget, never the final list
	// size — a small `limit` must not pre-truncate the pool by id order and
	// defeat the ranking.
	fq := ftsQuery(query)
	ftsSet := map[int64]bool{}
	var lexicalHits []store.Memory
	if fq != "" {
		var err error
		if conversationID != "" {
			lexicalHits, err = us.SearchMemoriesInConversation(fq, conversationID, memFTSCap)
		} else {
			lexicalHits, err = us.SearchMemories(fq, memFTSCap)
		}
		if err != nil {
			return nil, err
		}
		for _, h := range lexicalHits {
			ftsSet[h.ID] = true
		}
	}
	// One embedding call per search (the query's vector for cosine). Absent
	// embedder (or a failed call) → lexical mode, best-effort.
	var qvec []float32
	if a.vec != nil {
		if v, err := a.vec.embedInputs(ctx, []string{query}); err == nil {
			qvec = v
		}
	}
	var candidates []store.Memory
	if qvec != nil {
		// Semantic mode: the whole active set is rankable (a keyword miss
		// can still be a semantic hit); the FTS bonus set (lexicalHits,
		// capped at the candidate budget) already covers the +0.10.
		var err error
		if conversationID != "" {
			candidates, err = us.ListActiveMemoriesInConversation(conversationID)
		} else {
			candidates, err = us.ListActiveMemories()
		}
		if err != nil {
			return nil, err
		}
	} else {
		candidates = lexicalHits
	}
	now := time.Now()
	scored := make([]rankedMemory, 0, len(candidates))
	for _, m := range candidates {
		s := rankedMemory{mem: m, ftsHit: ftsSet[m.ID]}
		if qvec != nil {
			s.cosine = cosine(qvec, m.Embedding)
		}
		age := time.Duration(0)
		if t, err := time.Parse(time.RFC3339, m.UpdatedAt); err == nil {
			age = now.Sub(t)
		}
		if age < 0 {
			age = 0
		}
		halfLifeHours := memRecencyHalfLife.Hours()
		s.recency = halfLifeHours / (halfLifeHours + age.Hours())
		if qvec != nil {
			s.score = memWcosine*s.cosine + memWconf*m.Confidence + memWrecency*s.recency
			if s.ftsHit {
				s.score += memWfts
			}
		} else {
			s.score = 0.5*m.Confidence + 0.5*s.recency
		}
		scored = append(scored, s)
	}
	sort.Slice(scored, func(i, j int) bool {
		if scored[i].score != scored[j].score {
			return scored[i].score > scored[j].score
		}
		return scored[i].mem.ID < scored[j].mem.ID
	})
	if len(scored) > limit {
		scored = scored[:limit]
	}
	out := make([]store.Memory, 0, len(scored))
	for _, s := range scored {
		out = append(out, s.mem)
	}
	return out, nil
}

// memoryAge renders an age for the result text.
func memoryAge(now time.Time, updatedAt string) string {
	t, err := time.Parse(time.RFC3339, updatedAt)
	if err != nil {
		return "unknown age"
	}
	d := now.Sub(t)
	switch {
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh old", int(d.Hours()))
	case d < 30*24*time.Hour:
		return fmt.Sprintf("%dd old", int(d.Hours()/24))
	default:
		return fmt.Sprintf("%dd old", int(d.Hours()/24))
	}
}

// cosine is the similarity of two vectors (0 when either is absent or the
// dimensions disagree — a missing embedding is "no signal", not an error).
func cosine(a, b []float32) float64 {
	if len(a) == 0 || len(a) != len(b) {
		return 0
	}
	var dot, na, nb float64
	for i := range a {
		dot += float64(a[i]) * float64(b[i])
		na += float64(a[i]) * float64(a[i])
		nb += float64(b[i]) * float64(b[i])
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

type rememberArgs struct {
	Facet       string   `json:"facet"`
	Statement   string   `json:"statement"`
	Subject     *string  `json:"subject"`
	Confidence  *float64 `json:"confidence"`
	SupersedeID *int64   `json:"supersede_id"`
}

func (a *Agent) execRemember(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var p rememberArgs
	if err := json.Unmarshal(args, &p); err != nil {
		return fmt.Sprintf("invalid arguments: %v — \"facet\" and \"statement\" are required.", err), nil
	}
	facet := strings.TrimSpace(p.Facet)
	statement := strings.TrimSpace(p.Statement)
	if facet == "" || statement == "" {
		return "remember needs both \"facet\" (a short topic label) and \"statement\" (the fact, one self-contained sentence).", nil
	}
	if len(facet) > 64 {
		facet = facet[:64]
	}
	if len(statement) > 1000 {
		return "the statement is too long (max 1000 chars) — one self-contained sentence.", nil
	}
	conf := 0.8
	if p.Confidence != nil {
		conf = *p.Confidence
	}
	if conf <= 0 || conf > 1 {
		conf = 0.8
	}
	us, err := a.userStore(handle)
	if err != nil {
		return "internal error: could not open the user store.", nil
	}
	// Consolidation on write: one embedding call, scan the active set for
	// near-duplicates (surfaced to the model — it decides via supersede_id).
	var emb []float32
	if a.vec != nil {
		if v, err := a.vec.embedInputs(ctx, []string{statement}); err == nil {
			emb = v
		}
	}
	var nearDups []store.Memory
	if emb != nil {
		if mems, err := us.ListActiveMemories(); err == nil {
			for _, m := range mems {
				if c := cosine(emb, m.Embedding); c >= memNearDup {
					nearDups = append(nearDups, m)
				}
			}
		}
	}
	created, err := us.CreateMemory(facet, statement, p.Subject, conf, "tool:remember", emb)
	if err != nil {
		return "could not store the memory (internal error) — try again once.", nil
	}
	// Provenance: the running conversation, when this is a mid-turn call.
	if conv := conversationFromContext(ctx); conv != "" {
		if msgs, err := us.ListMessages(conv); err == nil && len(msgs) > 0 {
			latest := msgs[len(msgs)-1].ID
			_ = us.SetMemoryProvenance(created.ID, []int64{latest})
		}
	}
	// The curation growth trigger (the memory store just grew by one row —
	// this is the only live growth path; see curation.go).
	a.maybeSpawnCuration(ctx, handle)
	if p.SupersedeID != nil {
		old, err := us.GetMemory(*p.SupersedeID)
		if err != nil {
			return fmt.Sprintf("remembered as #%d, but supersede_id %d does not name an existing memory — search your memories to find the right id.", created.ID, *p.SupersedeID), nil
		}
		if old.Status != "active" {
			return fmt.Sprintf("remembered as #%d, but memory %d is not active (already superseded or forgotten) — nothing was replaced.", created.ID, old.ID), nil
		}
		if err := us.SupersedeMemory(old.ID, created.ID); err != nil {
			return fmt.Sprintf("remembered as #%d, but superseding #%d failed — the old one is still active.", created.ID, old.ID), nil
		}
		return fmt.Sprintf("remembered as #%d; superseded #%d.", created.ID, old.ID), nil
	}
	out := fmt.Sprintf("remembered as #%d.", created.ID)
	if len(nearDups) > 0 {
		sort.Slice(nearDups, func(i, j int) bool { return nearDups[i].ID < nearDups[j].ID })
		var d strings.Builder
		for i, m := range nearDups {
			if i >= 3 {
				break
			}
			fmt.Fprintf(&d, " #%d (%s)", m.ID, m.Statement)
		}
		out += " Note: similar to existing memories" + d.String() + " — if this fact REPLACES one of them, call remember again with its supersede_id."
	}
	return out, nil
}

func (a *Agent) execForget(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var p struct {
		ID int64 `json:"id"`
	}
	if err := json.Unmarshal(args, &p); err != nil || p.ID <= 0 {
		return "forget needs \"id\" (the memory id — search your memories to find it).", nil
	}
	us, err := a.userStore(handle)
	if err != nil {
		return "internal error: could not open the user store.", nil
	}
	mem, err := us.GetMemory(p.ID)
	if err != nil {
		return fmt.Sprintf("no memory with id %d — search your memories to find the right one.", p.ID), nil
	}
	if mem.Status != "active" {
		return fmt.Sprintf("memory %d is not active (already superseded or forgotten).", p.ID), nil
	}
	if err := us.ForgetMemory(p.ID); err != nil {
		return "could not forget the memory (internal error) — try again once.", nil
	}
	return fmt.Sprintf("forgotten #%d (%s).", p.ID, mem.Statement), nil
}
