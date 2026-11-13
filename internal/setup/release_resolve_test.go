package setup

import (
	"context"
	"strings"
	"testing"
	"time"
)

// TestReleaseAssetResolution verifies the GitHub releases API resolution finds
// the correct asset for the current OS (a live network test).
func TestReleaseAssetResolution(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	// Qdrant.
	url, err := qdrantDownloadURL(ctx)
	if err != nil {
		t.Fatalf("qdrantDownloadURL: %v", err)
	}
	if url == "" {
		t.Fatalf("qdrantDownloadURL returned an empty URL for %s", osArch())
	}
	if !strings.Contains(url, "qdrant") {
		t.Errorf("qdrant URL doesn't look right: %s", url)
	}
	t.Logf("qdrant: %s", url)

	// llama-server.
	lurl, err := llamaDownloadURL(ctx)
	if err != nil {
		t.Fatalf("llamaDownloadURL: %v", err)
	}
	if lurl == "" {
		t.Fatalf("llamaDownloadURL returned an empty URL for %s", osArch())
	}
	if !strings.Contains(lurl, "llama") {
		t.Errorf("llama URL doesn't look right: %s", lurl)
	}
	t.Logf("llama-server: %s", lurl)
}
