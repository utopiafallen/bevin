// vectorIndex embeds conversation messages into the user's Qdrant
// collection so conversation history is searchable semantically.
//
// The split (user directive): the per-user SQLite store keeps the
// authoritative ordered transcript (the P3 messages), but it carries NO
// vectors and does no similarity math — conversation history grows over a
// long time, so the searchable representation lives in the per-user vector
// database (the Qdrant sidecar) as embedded points. The embedder is the
// local llama-server (an OpenAI-compatible /v1/embeddings endpoint, always
// the same one the setup derives — see setup.deriveEmbedder).
//
// Everything here is best-effort by design: an embedding or upsert failure
// is logged and the turn proceeds (chat must never break on an index
// outage); a search against a missing/empty collection returns no hits.
package agent

import (
	"context"
	"fmt"
	"log"
	"math"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"

	"github.com/utopiafallen/bevin/internal/config"
	"github.com/utopiafallen/bevin/internal/provider"
	"github.com/utopiafallen/bevin/internal/qdrant"
	"github.com/utopiafallen/bevin/internal/store"
)

const (
	// The conversation index stores the embedder's FULL native output (the
	// local embeddinggemma-2 returns 768-dim) — user directive: conversation
	// history is a small per-user corpus, so there is no reason to MRL-
	// truncate it (the 512-dim MRL storage remains the design for the large
	// workspace-file corpus, where point size matters at scale). The
	// collection dimension is learned from the first embedding, so a
	// different-dim embedder later works without code changes.
	//
	// queryPrefix is the task prefix the embedding model expects on search
	// queries (text only, per the model card).
	queryPrefix = "task: search result | query: "

	// embedMaxRuneBudget caps the runes per input sent to the embedder.
	// Runes, not bytes/4: a gemma-family tokenizer (262k vocab) maps ~1:1
	// or less from rune to token, so the rune count is an upper bound on
	// the token count — and half of the model's max input
	// (config.EmbeddingMaxTokens) leaves a 2× margin for tokenizer
	// variance. Every chunk is therefore guaranteed to fit one embedding
	// call.
	embedMaxRuneBudget = config.EmbeddingMaxTokens / 2
)

// SearchHit is one semantically-matched conversation message (one vector
// point in the user's collection).
type SearchHit struct {
	ConversationID string  `json:"conversation_id"`
	Seq            int     `json:"seq"`
	Role           string  `json:"role"`
	Content        string  `json:"content"`
	CreatedAt      string  `json:"created_at"`
	ModelID        string  `json:"model_id,omitempty"`
	Score          float64 `json:"score"`
}

// vectorIndex embeds + indexes conversation messages per user.
type vectorIndex struct {
	emb      provider.Provider // the llama embedder
	embModel string
	qd       *qdrant.Client

	mu sync.Mutex
	// collDim learns each collection's dimension from the first embedding
	// (the embedder's output dim is the source of truth).
	collDim map[string]int
}

func newVectorIndex(emb provider.Provider, embModel string, qd *qdrant.Client) *vectorIndex {
	return &vectorIndex{emb: emb, embModel: embModel, qd: qd, collDim: map[string]int{}}
}

// collection is the user's collection name (one per user: "per-user vector
// db" — a user only ever sees/searches their own history).
func (v *vectorIndex) collection(handle string) string {
	return "user_" + handle
}

// IndexMessage embeds the message content and upserts it into the user's
// collection (point id = the message id, so re-indexing overwrites). Tool and
// summary rows are skipped — the searchable "conversation" is the user +
// assistant turns. Empty content is skipped.
func (v *vectorIndex) IndexMessage(ctx context.Context, handle string, msg store.Message, title string) error {
	if msg.Role != roleUser && msg.Role != roleAssistant {
		return nil
	}
	if strings.TrimSpace(msg.Content) == "" {
		return nil
	}
	// Long messages are chunked BEFORE the document prefix is applied (each
	// piece keeps the title); the pieces combine into the one point for the
	// message (embedInputs). A short message is a single piece — the exact
	// same input string as before chunking existed.
	chunks := chunkForEmbedding(msg.Content)
	inputs := make([]string, len(chunks))
	for i, c := range chunks {
		inputs[i] = "title: " + title + " | text: " + c
	}
	vec, err := v.embedInputs(ctx, inputs)
	if err != nil {
		return err
	}
	coll := v.collection(handle)
	if err := v.ensureCollection(ctx, coll, vec); err != nil {
		return err
	}
	payload := map[string]any{
		"conversation_id": msg.ConversationID,
		"seq":             msg.Seq,
		"role":            msg.Role,
		"content":         msg.Content,
		"created_at":      msg.CreatedAt,
	}
	if msg.ModelID != nil {
		payload["model_id"] = *msg.ModelID
	}
	return v.qd.Upsert(ctx, coll, []qdrant.Point{{ID: msg.ID, Vector: vec, Payload: payload}})
}

// DeleteMessages removes the points of the given messages from the user's
// collection (the conversation-delete path: the SQLite rows go first, the
// vectors follow). Point ids are the message ids, so the list needs no
// other mapping.
func (v *vectorIndex) DeleteMessages(ctx context.Context, handle string, msgIDs []int64) error {
	return v.qd.DeletePoints(ctx, v.collection(handle), msgIDs)
}

// Search embeds the query and returns the user's nearest message points.
// A query longer than one embedder input is chunked like a message (each
// piece keeps the search prefix) and combined the same way.
func (v *vectorIndex) Search(ctx context.Context, handle, query string, limit int) ([]SearchHit, error) {
	chunks := chunkForEmbedding(query)
	inputs := make([]string, len(chunks))
	for i, c := range chunks {
		inputs[i] = queryPrefix + c
	}
	vec, err := v.embedInputs(ctx, inputs)
	if err != nil {
		return nil, err
	}
	hits, err := v.qd.Search(ctx, v.collection(handle), vec, limit)
	if err != nil {
		return nil, err
	}
	out := make([]SearchHit, 0, len(hits))
	for _, h := range hits {
		ph := SearchHit{ConversationID: str(h.Payload["conversation_id"]), Seq: int64to(h.Payload["seq"]), Role: str(h.Payload["role"]), Content: str(h.Payload["content"]), CreatedAt: str(h.Payload["created_at"]), ModelID: str(h.Payload["model_id"]), Score: h.Score}
		out = append(out, ph)
	}
	return out, nil
}

// chunkForEmbedding splits content into pieces of at most
// embedMaxRuneBudget runes, preferring a whitespace boundary (an
// unbreakable run is hard-cut). Content that already fits comes back as a
// single piece — the common path, byte-identical to pre-chunking behavior.
// The caller applies its prefix to each piece (a titled document keeps its
// title on every piece).
func chunkForEmbedding(content string) []string {
	if utf8.RuneCountInString(content) <= embedMaxRuneBudget {
		return []string{content}
	}
	rs := []rune(content)
	var chunks []string
	for start := 0; start < len(rs); {
		end := start + embedMaxRuneBudget
		if end > len(rs) {
			end = len(rs)
		}
		// Break at the last whitespace inside the window (the next chunk
		// then starts right at/after the space); none found: hard cut.
		cut := end
		for i := end - 1; i > start; i-- {
			if unicode.IsSpace(rs[i]) {
				cut = i
				break
			}
		}
		chunks = append(chunks, string(rs[start:cut]))
		start = cut
	}
	// Drop whitespace-only fragments (a window ending on a run of spaces
	// could otherwise yield a final " " chunk — nothing to embed).
	out := chunks[:0]
	for _, c := range chunks {
		if strings.TrimSpace(c) != "" {
			out = append(out, c)
		}
	}
	return out
}

// embedInputs embeds the inputs and returns ONE combined vector. A single
// input returns exactly what the embedder gave (the common path — behavior
// unchanged); multiple inputs (a chunked message or query) are averaged and
// L2-normalized, so a long input maps to the same single, unit-length point
// the rest of the corpus has (a mean of unit vectors is not unit-length, and
// cosine ranking needs it normalized back).
func (v *vectorIndex) embedInputs(ctx context.Context, inputs []string) ([]float32, error) {
	vecs, err := v.emb.Embed(ctx, v.embModel, inputs)
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("embed: no vectors returned")
	}
	if len(vecs) == 1 {
		return vecs[0], nil
	}
	if len(vecs) != len(inputs) {
		return nil, fmt.Errorf("embed: %d vectors for %d inputs", len(vecs), len(inputs))
	}
	dim := len(vecs[0])
	avg := make([]float32, dim)
	for _, vec := range vecs {
		if len(vec) != dim {
			return nil, fmt.Errorf("embed: inconsistent vector dims (%d vs %d)", len(vec), dim)
		}
		for i := range vec {
			avg[i] += vec[i]
		}
	}
	for i := range avg {
		avg[i] /= float32(len(vecs))
	}
	norm := float32(0)
	for _, x := range avg {
		norm += x * x
	}
	if norm > 0 {
		inv := float32(1 / math.Sqrt(float64(norm)))
		for i := range avg {
			avg[i] *= inv
		}
	}
	return avg, nil
}

func (v *vectorIndex) ensureCollection(ctx context.Context, name string, vec []float32) error {
	v.mu.Lock()
	defer v.mu.Unlock()
	dim := len(vec)
	if d, ok := v.collDim[name]; ok {
		dim = d
	} else {
		v.collDim[name] = dim
	}
	return v.qd.EnsureCollection(ctx, name, dim)
}

func str(v any) string {
	s, _ := v.(string)
	return s
}

func int64to(v any) int {
	switch n := v.(type) {
	case float64:
		return int(n)
	case int:
		return n
	case int64:
		return int(n)
	}
	return 0
}

// logVectorIndexError is the single place index/search failures are logged
// (best-effort — see the package doc).
func logVectorIndexError(op string, err error) {
	log.Printf("vector index %s: %v", op, err)
}
