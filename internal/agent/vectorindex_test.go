package agent

import (
	"context"
	"encoding/json"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"unicode/utf8"

	"github.com/utopiafallen/bevin/internal/provider"
	"github.com/utopiafallen/bevin/internal/qdrant"
	"github.com/utopiafallen/bevin/internal/store"
)

func newQdrantForTest(baseURL string) *qdrant.Client { return qdrant.New(baseURL) }

// embedFake records the embed inputs and returns a fixed 768-dim vector
// (the embedder's full native output is stored as-is).
type embedFake struct {
	inputs []string
	vec    []float32
}

func (e *embedFake) Name() string { return "embedfake" }
func (e *embedFake) Chat(context.Context, string, provider.ChatRequest) (*provider.Result, error) {
	return nil, nil
}
func (e *embedFake) Stream(context.Context, string, provider.ChatRequest, func(provider.Chunk) error) (*provider.Result, error) {
	return nil, nil
}
func (e *embedFake) Models(context.Context) ([]provider.ModelInfo, error) { return nil, nil }
func (e *embedFake) Embed(_ context.Context, _ string, inputs []string) ([][]float32, error) {
	e.inputs = append(e.inputs, inputs...)
	out := make([][]float32, len(inputs))
	for i := range inputs {
		out[i] = e.vec
	}
	return out, nil
}

// qdrantFake records upserts and answers searches from a canned hit list.
type qdrantFake struct {
	t        *testing.T
	upserts  []map[string]any // each upsert's points array
	searches []map[string]any
	hits     []map[string]any
}

func (q *qdrantFake) handler() http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/collections/user_alice":
			_, _ = w.Write([]byte(`{"result":{"status":"green"}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/collections/user_alice/points":
			var b struct {
				Points []map[string]any `json:"points"`
				Wait   bool             `json:"wait"`
			}
			body, _ := io.ReadAll(r.Body)
			if err := json.Unmarshal(body, &b); err != nil {
				q.t.Fatalf("upsert body: %v", err)
			}
			if !b.Wait {
				q.t.Fatalf("upsert wait = false, want true")
			}
			q.upserts = append(q.upserts, b.Points...)
			_, _ = w.Write([]byte(`{"result":{"status":"acknowledged"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/collections/user_alice/points/search":
			var sb map[string]any
			body, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(body, &sb)
			q.searches = append(q.searches, sb)
			res := make([]map[string]any, len(q.hits))
			copy(res, q.hits)
			b, _ := json.Marshal(map[string]any{"result": res})
			_, _ = w.Write(b)
		default:
			q.t.Errorf("unexpected qdrant request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}
}

func TestVectorIndexIndexMessage(t *testing.T) {
	vec := make([]float32, 768)
	for i := range vec {
		vec[i] = 0.5
	}
	emb := &embedFake{vec: vec}
	qd := &qdrantFake{t: t}
	srv := httptest.NewServer(qd.handler())
	defer srv.Close()

	vi := newVectorIndex(emb, "emb-model", newQdrantForTest(srv.URL))
	ctx := context.Background()

	msg := &store.Message{ID: 42, ConversationID: "cv1", Role: roleUser, Content: "how do I bake sourdough", Seq: 3, CreatedAt: "2026-11-13T12:00:00Z"}
	if err := vi.IndexMessage(ctx, "alice", *msg, "Baking"); err != nil {
		t.Fatal(err)
	}

	// The embed input carries the chunk task prefix (title + text).
	if len(emb.inputs) != 1 || emb.inputs[0] != "title: Baking | text: how do I bake sourdough" {
		t.Fatalf("embed input = %q, want the title/text chunk prefix", emb.inputs)
	}
	if len(qd.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1", len(qd.upserts))
	}
	p := qd.upserts[0]
	if p["id"] != float64(42) {
		t.Fatalf("point id = %v, want the message id 42", p["id"])
	}
	v := p["vector"].([]any)
	if len(v) != 768 {
		t.Fatalf("vector dim = %d, want the full native 768 (no truncation)", len(v))
	}
	pl := p["payload"].(map[string]any)
	if pl["conversation_id"] != "cv1" || pl["seq"] != float64(3) || pl["role"] != "user" || pl["content"] != "how do I bake sourdough" {
		t.Fatalf("payload = %v", pl)
	}
}

func TestVectorIndexSearch(t *testing.T) {
	emb := &embedFake{vec: []float32{1, 0, 0}}
	qd := &qdrantFake{t: t, hits: []map[string]any{{"id": 9, "score": 0.87, "payload": map[string]any{"conversation_id": "cv9", "seq": 4.0, "role": "assistant", "content": "use 2 cups of flour", "created_at": "2026-11-12T09:00:00Z", "model_id": "m1"}}}}
	srv := httptest.NewServer(qd.handler())
	defer srv.Close()

	vi := newVectorIndex(emb, "emb-model", newQdrantForTest(srv.URL))
	hits, err := vi.Search(context.Background(), "alice", "how much flour do I need", 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 {
		t.Fatalf("hits = %v", hits)
	}
	h := hits[0]
	if h.ConversationID != "cv9" || h.Seq != 4 || h.Role != "assistant" || h.Content != "use 2 cups of flour" || h.ModelID != "m1" || h.Score != 0.87 {
		t.Fatalf("hit = %+v", h)
	}
	// The query embed carries the search task prefix.
	if len(emb.inputs) != 1 || emb.inputs[0] != queryPrefix+"how much flour do I need" {
		t.Fatalf("query embed input = %q, want the search task prefix", emb.inputs)
	}
	// The search went to the user's own collection (per-user isolation).
	if len(qd.searches) != 1 {
		t.Fatalf("searches = %d, want 1", len(qd.searches))
	}
}

func TestVectorIndexSkipsNonConversationRows(t *testing.T) {
	emb := &embedFake{vec: []float32{1}}
	qd := &qdrantFake{t: t}
	srv := httptest.NewServer(qd.handler())
	defer srv.Close()
	vi := newVectorIndex(emb, "emb-model", newQdrantForTest(srv.URL))

	for _, role := range []string{roleTool, roleSummary} {
		msg := &store.Message{ID: 1, Role: role, Content: "not searchable"}
		if err := vi.IndexMessage(context.Background(), "alice", *msg, "T"); err != nil {
			t.Fatal(err)
		}
	}
	// Empty content is skipped too.
	if err := vi.IndexMessage(context.Background(), "alice", store.Message{ID: 2, Role: roleUser, Content: "   "}, "T"); err != nil {
		t.Fatal(err)
	}
	if len(emb.inputs) != 0 || len(qd.upserts) != 0 {
		t.Fatalf("tool/summary/empty rows must not be embedded: inputs=%v upserts=%d", emb.inputs, len(qd.upserts))
	}
}

// chunkedEmbedFake returns a DIFFERENT vector per input (dim0 = the input
// index, dim1 = 1), so the average + renormalization across chunked inputs
// is verifiable.
type chunkedEmbedFake struct {
	embedFake
}

func (e *chunkedEmbedFake) Embed(_ context.Context, _ string, inputs []string) ([][]float32, error) {
	e.inputs = append(e.inputs, inputs...)
	out := make([][]float32, len(inputs))
	for i := range inputs {
		v := make([]float32, 2)
		v[0] = float32(i)
		v[1] = 1
		out[i] = v
	}
	return out, nil
}

func TestChunkForEmbedding(t *testing.T) {
	// Content that fits comes back as one unchanged piece (the common path —
	// the exact input string the embedder always received).
	one := chunkForEmbedding("hello world")
	if len(one) != 1 || one[0] != "hello world" {
		t.Fatalf("short content = %v, want one unchanged piece", one)
	}

	// Long content: several pieces, each within the per-input rune budget,
	// none whitespace-only, and lossless when reassembled.
	long := strings.Repeat("word ", 2000) // 10,000 runes
	chunks := chunkForEmbedding(long)
	if len(chunks) < 2 {
		t.Fatalf("long content = %d piece(s), want several", len(chunks))
	}
	var total int
	for i, c := range chunks {
		if n := utf8.RuneCountInString(c); n > embedMaxRuneBudget {
			t.Fatalf("piece %d has %d runes, over the budget %d", i, n, embedMaxRuneBudget)
		}
		if strings.TrimSpace(c) == "" {
			t.Fatalf("whitespace-only piece %d in %v", i, chunks)
		}
		total += utf8.RuneCountInString(c)
	}
	if total != 0 {
		// Whitespace-only fragments are intentionally dropped (they carry no
		// content — the last cut can leave a lone trailing space), so
		// reassembly equals the content minus those: compare trimmed.
		got := strings.TrimSpace(strings.Join(chunks, ""))
		want := strings.TrimSpace(long)
		if got != want {
			t.Fatalf("pieces lose content: %d vs %d runes", len(got), len(want))
		}
	}

	// A single unbreakable run is hard-cut at the budget.
	blob := strings.Repeat("x", embedMaxRuneBudget*2+1)
	hc := chunkForEmbedding(blob)
	if len(hc) != 3 {
		t.Fatalf("hard-cut pieces = %d, want 3 (budget*2+1 runes)", len(hc))
	}
	if strings.Join(hc, "") != blob {
		t.Fatal("hard-cut pieces do not reconstruct the input")
	}
}

// TestIndexMessageChunksLongContent pins the long-message path: a message
// longer than one embedder input is sent as several chunk inputs (each
// keeping the title/text prefix, each within the per-input budget) and the
// stored point is the L2-normalized average of the per-chunk vectors.
func TestIndexMessageChunksLongContent(t *testing.T) {
	emb := &chunkedEmbedFake{}
	qd := &qdrantFake{t: t}
	srv := httptest.NewServer(qd.handler())
	defer srv.Close()
	vi := newVectorIndex(emb, "emb-model", newQdrantForTest(srv.URL))

	// ~21k runes — well past one embedder input (budget 4,096 runes).
	content := strings.Repeat("long message content ", 1000)
	msg := &store.Message{ID: 7, ConversationID: "cv7", Role: roleAssistant, Content: content, Seq: 1, CreatedAt: "2026-11-14T10:00:00Z"}
	if err := vi.IndexMessage(context.Background(), "alice", *msg, "Long"); err != nil {
		t.Fatal(err)
	}
	if len(emb.inputs) < 2 {
		t.Fatalf("embed inputs = %d, want several for a long message", len(emb.inputs))
	}
	const prefix = "title: Long | text: "
	for i, in := range emb.inputs {
		if !strings.HasPrefix(in, prefix) {
			t.Fatalf("input %d = %q, want the title/text prefix on every chunk", i, in)
		}
		if n := utf8.RuneCountInString(in[len(prefix):]); n > embedMaxRuneBudget {
			t.Fatalf("input %d carries %d content runes, over the per-input budget", i, n)
		}
	}
	if len(qd.upserts) != 1 {
		t.Fatalf("upserts = %d, want 1 (one point per message, not per chunk)", len(qd.upserts))
	}
	v := qd.upserts[0]["vector"].([]any)
	norm := 0.0
	for _, x := range v {
		norm += x.(float64) * x.(float64)
	}
	if math.Abs(norm-1) > 1e-4 {
		t.Fatalf("stored vector norm = %v, want ~1 (renormalized average)", norm)
	}
}

// TestSearchChunksLongQuery pins the query side: an over-budget query is
// chunked the same way (every piece keeps the search prefix).
func TestSearchChunksLongQuery(t *testing.T) {
	emb := &chunkedEmbedFake{}
	qd := &qdrantFake{t: t}
	srv := httptest.NewServer(qd.handler())
	defer srv.Close()
	vi := newVectorIndex(emb, "emb-model", newQdrantForTest(srv.URL))

	query := strings.Repeat("search term ", 1500) // 13,500 runes
	if _, err := vi.Search(context.Background(), "alice", query, 5); err != nil {
		t.Fatal(err)
	}
	if len(emb.inputs) < 2 {
		t.Fatalf("query embed inputs = %d, want several for a long query", len(emb.inputs))
	}
	for i, in := range emb.inputs {
		if !strings.HasPrefix(in, queryPrefix) {
			t.Fatalf("query input %d = %q, want the search prefix on every chunk", i, in)
		}
		if n := utf8.RuneCountInString(in[len(queryPrefix):]); n > embedMaxRuneBudget {
			t.Fatalf("query input %d carries %d runes, over the per-input budget", i, n)
		}
	}
}
