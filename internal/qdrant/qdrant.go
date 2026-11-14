// Package qdrant is a thin REST client for the Qdrant vector database
// (the loopback sidecar the setup provisions; see setup/sidecar.go). It
// covers exactly what bevin needs: ensure a collection, upsert points,
// and vector-search them. No other Qdrant features (shards, aliases,
// snapshots, gRPC) are used.
//
// Point IDs are the SQLite message IDs (integer IDs are natively
// supported and returned verbatim on search — a string ID would be
// hashed by Qdrant and the mapping lost).
package qdrant

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"
)

// Client talks to one Qdrant endpoint (loopback; no auth on the local
// sidecar).
type Client struct {
	BaseURL string // e.g. http://127.0.0.1:6333
	HTTP    *http.Client
}

// New builds a client for the endpoint. An empty BaseURL yields a nil
// client (vector search is unavailable — callers must treat a nil client
// as "off", not an error).
func New(baseURL string) *Client {
	if baseURL == "" {
		return nil
	}
	return &Client{BaseURL: baseURL, HTTP: &http.Client{Timeout: 30 * time.Second}}
}

// Point is one vector + its payload.
type Point struct {
	ID      int64     // the SQLite message id
	Vector  []float32 // the embedding (collection dim)
	Payload map[string]any
}

// Hit is one search result.
type Hit struct {
	ID      int64          `json:"id"`
	Score   float64        `json:"score"`
	Payload map[string]any `json:"payload"`
}

// EnsureCollection creates the collection (fixed dim, cosine) when it
// does not exist yet. Idempotent.
func (c *Client) EnsureCollection(ctx context.Context, name string, dim int) error {
	// GET /collections/{name} — 404 means create.
	r, err := http.NewRequestWithContext(ctx, http.MethodGet, c.BaseURL+"/collections/"+name, nil)
	if err != nil {
		return err
	}
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return err
	}
	ioClose(resp.Body)
	if resp.StatusCode == http.StatusOK {
		return nil
	}
	if resp.StatusCode != http.StatusNotFound {
		return fmt.Errorf("qdrant: GET /collections/%s: %s", name, resp.Status)
	}

	body, _ := json.Marshal(map[string]any{
		"vectors": map[string]any{"size": dim, "distance": "Cosine"},
	})
	// The current Qdrant API puts the collection name in the path.
	r, err = http.NewRequestWithContext(ctx, http.MethodPut, c.BaseURL+"/collections/"+name, bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err = c.HTTP.Do(r)
	if err != nil {
		return err
	}
	ioClose(resp.Body)
	if resp.StatusCode != http.StatusCreated && resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusAccepted {
		return fmt.Errorf("qdrant: create collection %s: %s", name, resp.Status)
	}
	return nil
}

// Upsert inserts (or overwrites) the points. wait=true so the points are
// searchable immediately on return (small per-message batches; latency is
// negligible on the loopback sidecar).
func (c *Client) Upsert(ctx context.Context, name string, pts []Point) error {
	if len(pts) == 0 {
		return nil
	}
	out := make([]map[string]any, len(pts))
	for i, p := range pts {
		out[i] = map[string]any{"id": p.ID, "vector": p.Vector, "payload": p.Payload}
	}
	body, _ := json.Marshal(map[string]any{"points": out, "wait": true})
	r, err := http.NewRequestWithContext(ctx, http.MethodPut, c.BaseURL+"/collections/"+name+"/points", bytes.NewReader(body))
	if err != nil {
		return err
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return err
	}
	ioClose(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("qdrant: upsert %s: %s", name, resp.Status)
	}
	return nil
}

// Search returns the limit nearest vectors (cosine, as the collection is
// created) with their payloads.
func (c *Client) Search(ctx context.Context, name string, vector []float32, limit int) ([]Hit, error) {
	if limit <= 0 {
		limit = 10
	}
	body, _ := json.Marshal(map[string]any{
		"vector": vector, "limit": limit, "with_payload": true,
	})
	r, err := http.NewRequestWithContext(ctx, http.MethodPost, c.BaseURL+"/collections/"+name+"/points/search", bytes.NewReader(body))
	if err != nil {
		return nil, err
	}
	r.Header.Set("Content-Type", "application/json")
	resp, err := c.HTTP.Do(r)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		// The collection does not exist yet (nothing indexed) — no hits,
		// not an error.
		return nil, nil
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("qdrant: search %s: %s", name, resp.Status)
	}
	var sr struct {
		Result []Hit `json:"result"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&sr); err != nil {
		return nil, err
	}
	return sr.Result, nil
}

func ioClose(b interface{ Close() error }) { _ = b.Close() }
