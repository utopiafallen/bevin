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
	"strings"
	"sync"

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
	vec, err := v.embed(ctx, "title: "+title+" | text: "+msg.Content)
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

// Search embeds the query and returns the user's nearest message points.
func (v *vectorIndex) Search(ctx context.Context, handle, query string, limit int) ([]SearchHit, error) {
	vec, err := v.embed(ctx, queryPrefix+query)
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

func (v *vectorIndex) embed(ctx context.Context, text string) ([]float32, error) {
	vecs, err := v.emb.Embed(ctx, v.embModel, []string{text})
	if err != nil {
		return nil, fmt.Errorf("embed: %w", err)
	}
	if len(vecs) == 0 {
		return nil, fmt.Errorf("embed: no vectors returned")
	}
	return vecs[0], nil
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
