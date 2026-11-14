package agent

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/utopiafallen/bevin/internal/provider"
	"github.com/utopiafallen/bevin/internal/qdrant"
	"github.com/utopiafallen/bevin/internal/store"
)

func newQdrantForTest(baseURL string) *qdrant.Client { return qdrant.New(baseURL) }

// embedFake records the embed inputs and returns a fixed 768-dim vector
// (so the MRL truncation path is exercised).
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
	if len(v) != storedDim {
		t.Fatalf("vector dim = %d, want %d (MRL-truncated)", len(v), storedDim)
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

func TestMRL(t *testing.T) {
	// A 768-dim unit-ish vector truncates to 512 and renormalizes.
	in := make([]float32, 768)
	for i := 0; i < 512; i++ {
		in[i] = 1
	}
	out := mrl(in)
	if len(out) != 512 {
		t.Fatalf("dim = %d, want %d", len(out), 512)
	}
	var norm float64
	for _, x := range out {
		norm += float64(x) * float64(x)
	}
	if norm < 0.999 || norm > 1.001 {
		t.Fatalf("renormalized norm = %v, want 1", norm)
	}
	// The input must not be mutated.
	if len(in) != 768 {
		t.Fatal("input mutated")
	}
	// Shorter vectors pass through unchanged.
	short := []float32{0.3, 0.4}
	if got := mrl(short); len(got) != 2 || got[0] != 0.3 || got[1] != 0.4 {
		t.Fatalf("short vector = %v, want unchanged", got)
	}
}
