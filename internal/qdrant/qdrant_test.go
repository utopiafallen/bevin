package qdrant

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestEnsureCollection — a 404 creates the collection (cosine, the given
// dim); a 200 does not; any other status is an error.
func TestEnsureCollection(t *testing.T) {
	var exists bool
	var createBody map[string]any
	var creates int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/collections/c1":
			if !exists {
				w.WriteHeader(http.StatusNotFound)
				_, _ = w.Write([]byte(`{"status":{"error":"Not found: collection 'c1' does not exist"}}`))
				return
			}
			_, _ = w.Write([]byte(`{"result":{"status":"green"}}`))
		case r.Method == http.MethodPut && r.URL.Path == "/collections/c1":
			creates++
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &createBody)
			exists = true
			w.WriteHeader(http.StatusCreated)
			_, _ = w.Write([]byte(`{"result":true}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := New(srv.URL)
	ctx := context.Background()

	// First call: 404 -> create.
	if err := c.EnsureCollection(ctx, "c1", 512); err != nil {
		t.Fatal(err)
	}
	if creates != 1 {
		t.Fatalf("creates = %d, want 1", creates)
	}
	v := createBody["vectors"].(map[string]any)
	if v["size"] != float64(512) || v["distance"] != "Cosine" {
		t.Fatalf("create body = %v, want 512-dim Cosine", createBody)
	}
	// Second call: exists -> no create.
	if err := c.EnsureCollection(ctx, "c1", 512); err != nil {
		t.Fatal(err)
	}
	if creates != 1 {
		t.Fatalf("creates = %d after idempotent call, want 1", creates)
	}
}

// TestUpsertAndSearch — the upsert body carries id/vector/payload +
// wait=true; the search body carries the vector + limit + with_payload and
// the response maps back to hits (id, score, payload). A search against a
// missing collection is an empty result, not an error.
func TestUpsertAndSearch(t *testing.T) {
	var upsertBody map[string]any
	var searchBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodPut && r.URL.Path == "/collections/c1/points":
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &upsertBody)
			_, _ = w.Write([]byte(`{"result":{"status":"acknowledged"}}`))
		case r.Method == http.MethodPost && r.URL.Path == "/collections/c1/points/search":
			b, _ := io.ReadAll(r.Body)
			_ = json.Unmarshal(b, &searchBody)
			_, _ = w.Write([]byte(`{"result":[{"id":7,"score":0.91,"payload":{"conversation_id":"cv1","content":"the cat sat"}}]}`))
		case r.Method == http.MethodPost && r.URL.Path == "/collections/nope/points/search":
			w.WriteHeader(http.StatusNotFound)
			_, _ = w.Write([]byte(`{"status":{"error":"not found"}}`))
		default:
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		}
	}))
	defer srv.Close()

	c := New(srv.URL)
	ctx := context.Background()

	if err := c.Upsert(ctx, "c1", []Point{{ID: 7, Vector: []float32{0.1, 0.2, 0.3}, Payload: map[string]any{"conversation_id": "cv1", "content": "the cat sat"}}}); err != nil {
		t.Fatal(err)
	}
	pts := upsertBody["points"].([]any)
	if len(pts) != 1 {
		t.Fatalf("points = %v", pts)
	}
	p0 := pts[0].(map[string]any)
	if p0["id"] != float64(7) {
		t.Fatalf("point id = %v, want 7 (the message id)", p0["id"])
	}
	if upsertBody["wait"] != true {
		t.Fatalf("wait = %v, want true (points must be searchable on return)", upsertBody["wait"])
	}
	if p0["payload"].(map[string]any)["conversation_id"] != "cv1" {
		t.Fatalf("payload = %v", p0["payload"])
	}

	hits, err := c.Search(ctx, "c1", []float32{0.9, 0.1, 0.1}, 5)
	if err != nil {
		t.Fatal(err)
	}
	if len(hits) != 1 || hits[0].ID != 7 || hits[0].Score != 0.91 {
		t.Fatalf("hits = %+v", hits)
	}
	if hits[0].Payload["content"] != "the cat sat" {
		t.Fatalf("hit payload = %v", hits[0].Payload)
	}
	if searchBody["limit"] != float64(5) || searchBody["with_payload"] != true {
		t.Fatalf("search body = %v", searchBody)
	}

	// Missing collection: empty, not an error.
	hits, err = c.Search(ctx, "nope", []float32{0.1}, 3)
	if err != nil {
		t.Fatalf("search on missing collection = %v, want nil", err)
	}
	if len(hits) != 0 {
		t.Fatalf("hits on missing collection = %v, want none", hits)
	}
}

// TestNewEmpty — a blank endpoint yields a nil client (vector search is
// "off", never an error path).
func TestNewEmpty(t *testing.T) {
	if New("") != nil {
		t.Fatal("New(\"\") must return nil")
	}
}
