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
// recall = on-demand retrieval (the memory model: the long tail is QUERIED,
// never injected); remember = consolidation-on-write (near-duplicates are
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

// recall scoring blend (internal constants): cosine is the primary signal
// (the query's one embedding call), confidence the model's stated certainty,
// recency the half-life decay, the FTS hit the lexical floor (it keeps a
// keyword match rankable when the semantics are weak). No embedder
// configured → lexical mode (FTS hits only, confidence + recency blend).
const (
	recallWcosine         = 0.55
	recallWconf           = 0.20
	recallWrecency        = 0.15
	recallWfts            = 0.10
	recallRecencyHalfLife = 30 * 24 * time.Hour
	recallNearDup         = 0.85 // remember's consolidation threshold
	recallDefaultTop      = 5
	recallMaxTop          = 20
)

func registerMemoryTools(r *ToolRegistry, a *Agent) error {
	if err := r.Register(Tool{
		Name:        "recall",
		Description: "Search the user's stored memories (durable facts about the user and their world) by meaning. Returns the closest matches.",
		Parameters: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{
					"type":        "string",
					"description": "what to recall, in natural language",
				},
				"top": map[string]any{
					"type":        "number",
					"description": "how many to return (default 5, max 20)",
				},
			},
			"required": []string{"query"},
		},
		Execute: a.execRecall,
	}); err != nil {
		return err
	}
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
					"description": "the memory id (from recall)",
				},
			},
			"required": []string{"id"},
		},
		Execute: a.execForget,
	})
}

// recallScore is one memory's blend score + its parts (for the result text).
type recallScore struct {
	mem     store.Memory
	score   float64
	cosine  float64
	ftsHit  bool
	recency float64
}

func (a *Agent) execRecall(ctx context.Context, handle string, args json.RawMessage) (string, error) {
	var p struct {
		Query string `json:"query"`
		Top   int    `json:"top"`
	}
	if err := json.Unmarshal(args, &p); err != nil {
		return fmt.Sprintf("invalid arguments: %v — \"query\" is required.", err), nil
	}
	if strings.TrimSpace(p.Query) == "" {
		return "recall needs a query (what to recall, in natural language).", nil
	}
	top := p.Top
	if top <= 0 {
		top = recallDefaultTop
	}
	if top > recallMaxTop {
		top = recallMaxTop
	}
	us, err := a.userStore(handle)
	if err != nil {
		return "internal error: could not open the user store.", nil
	}
	mems, err := us.ListActiveMemories()
	if err != nil {
		return "internal error: could not list memories.", nil
	}
	if len(mems) == 0 {
		return "you have no stored memories yet.", nil
	}
	// The one embedding call per recall (the DECIDED contract): the query's
	// vector for cosine. Absent embedder → lexical mode.
	var qvec []float32
	if a.vec != nil {
		if v, err := a.vec.embedInputs(ctx, []string{p.Query}); err == nil {
			qvec = v
		}
	}
	// The lexical floor: which candidates the FTS pre-filter hit.
	ftsHits := map[int64]bool{}
	if fq := ftsQuery(p.Query); fq != "" {
		if hits, err := us.SearchMemories(fq, recallMaxTop); err == nil {
			for _, h := range hits {
				ftsHits[h.ID] = true
			}
		}
	}
	now := time.Now()
	scored := make([]recallScore, 0, len(mems))
	for _, m := range mems {
		s := recallScore{mem: m, ftsHit: ftsHits[m.ID]}
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
		halfLifeHours := recallRecencyHalfLife.Hours()
		s.recency = halfLifeHours / (halfLifeHours + age.Hours())
		if qvec != nil {
			s.score = recallWcosine*s.cosine + recallWconf*m.Confidence + recallWrecency*s.recency
			if s.ftsHit {
				s.score += recallWfts
			}
		} else {
			// Lexical mode: only the FTS hits are surfaced.
			if !s.ftsHit {
				continue
			}
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
	if len(scored) == 0 {
		return "no stored memories match that — you have memories, but none match the query.", nil
	}
	if len(scored) > top {
		scored = scored[:top]
	}
	var b strings.Builder
	for _, s := range scored {
		fmt.Fprintf(&b, "#%d [%s] %s (confidence %.2f, %s)\n", s.mem.ID, s.mem.Facet, s.mem.Statement, s.mem.Confidence, memoryAge(now, s.mem.UpdatedAt))
	}
	return strings.TrimRight(b.String(), "\n"), nil
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
				if c := cosine(emb, m.Embedding); c >= recallNearDup {
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
	if p.SupersedeID != nil {
		old, err := us.GetMemory(*p.SupersedeID)
		if err != nil {
			return fmt.Sprintf("remembered as #%d, but supersede_id %d does not name an existing memory — recall to find the right id.", created.ID, *p.SupersedeID), nil
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
		return "forget needs \"id\" (the memory id, from recall).", nil
	}
	us, err := a.userStore(handle)
	if err != nil {
		return "internal error: could not open the user store.", nil
	}
	mem, err := us.GetMemory(p.ID)
	if err != nil {
		return fmt.Sprintf("no memory with id %d — recall to find the right one.", p.ID), nil
	}
	if mem.Status != "active" {
		return fmt.Sprintf("memory %d is not active (already superseded or forgotten).", p.ID), nil
	}
	if err := us.ForgetMemory(p.ID); err != nil {
		return "could not forget the memory (internal error) — try again once.", nil
	}
	return fmt.Sprintf("forgotten #%d (%s).", p.ID, mem.Statement), nil
}
