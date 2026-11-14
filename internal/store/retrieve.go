package store

import (
	"context"
	"sort"
	"strings"
)

// Recall is a scored memory hit.
type Recall struct {
	Score  float64
	Memory *Memory
}

// HistoryRecall is a scored conversation-history (message) hit.
type HistoryRecall struct {
	Score   float64
	Message *Message
}

// Retriever is the single seam for retrieval. The current implementation
// brute-forces the small per-user memory set (memories are intentionally
// NOT in the vector DB — small, per-user, confidence/recency-weighted);
// conversation-history retrieval is served by the per-user vector DB (the
// message rows carry no local embeddings).
//
// Constraint (skill): never scatter "load all vectors + brute force" across the
// codebase — route retrieval through this interface.
type Retriever interface {
	RecallMemories(ctx context.Context, query string, queryEmb []float32, limit int) ([]Recall, error)
	RecallHistory(ctx context.Context, query string, queryEmb []float32, limit int) ([]HistoryRecall, error)
}

// NewMemoryRetriever returns the v1 brute-force retriever over a UserStore.
func NewMemoryRetriever(s *UserStore) Retriever {
	return &memoryRetriever{store: s}
}

type memoryRetriever struct {
	store *UserStore
}

func (r *memoryRetriever) RecallMemories(ctx context.Context, query string, queryEmb []float32, limit int) ([]Recall, error) {
	if limit <= 0 {
		limit = 8
	}
	mems, err := r.store.ListActiveMemories()
	if err != nil {
		return nil, err
	}
	// Lexical (FTS5) hits boost the small in-memory set (not a hard prefilter,
	// so recall stays robust when FTS5 finds no lexical match).
	ftsHits := map[int64]bool{}
	if q := safeFTSQuery(query); q != "" {
		if m, err := r.store.SearchMemories(q, len(mems)); err == nil {
			for _, m := range m {
				ftsHits[m.ID] = true
			}
		}
	}
	out := make([]Recall, 0, len(mems))
	for i := range mems {
		m := &mems[i]
		var score float64
		if queryEmb != nil && len(m.Embedding) > 0 {
			score += 2.0 * cosine(queryEmb, m.Embedding)
		}
		if ftsHits[m.ID] {
			score += 1.0
		}
		score += m.Confidence
		out = append(out, Recall{Score: score, Memory: m})
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Score > out[j].Score })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (r *memoryRetriever) RecallHistory(ctx context.Context, query string, queryEmb []float32, limit int) ([]HistoryRecall, error) {
	if limit <= 0 {
		limit = 10
	}
	q := safeFTSQuery(query)
	if q == "" {
		return nil, nil
	}
	// Lexical (FTS5) candidate list only. Semantic history search does NOT run
	// over the SQLite rows: conversation messages live in the per-user vector
	// DB and are searched there (queryEmb is accepted for interface
	// uniformity; the vector-backed Retriever implementation consumes it).
	msgs, err := r.store.SearchMessages(q, limit)
	if err != nil {
		return nil, err
	}
	out := make([]HistoryRecall, 0, len(msgs))
	for i := range msgs {
		out = append(out, HistoryRecall{Message: &msgs[i]})
	}
	return out, nil
}

// safeFTSQuery turns plain text into a safe FTS5 query: each whitespace token
// becomes a quoted phrase, ORed together (a loose lexical candidate set).
// Returns "" when there are no usable tokens.
func safeFTSQuery(query string) string {
	var parts []string
	for _, t := range strings.Fields(query) {
		t = strings.ReplaceAll(t, `"`, "")
		if t == "" {
			continue
		}
		parts = append(parts, `"`+t+`"`)
	}
	if len(parts) == 0 {
		return ""
	}
	return strings.Join(parts, " OR ")
}
