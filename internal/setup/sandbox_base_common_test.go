package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// makeLayerTar returns the bytes of a small layer tar (a directory, a
// file, a symlink) — plain or gzip-compressed, the two encodings MCR
// has served (uncompressed historically, .tar.gzip since the 2026 edge
// migration — see sandbox_base_windows.go).
func makeLayerTar(t *testing.T, compressed bool) []byte {
	t.Helper()
	var plain bytes.Buffer
	tw := tar.NewWriter(&plain)
	write := func(name string, typ byte, mode int64, content, link string) {
		t.Helper()
		hdr := &tar.Header{Name: name, Typeflag: typ, Mode: mode, Size: int64(len(content))}
		if link != "" {
			hdr.Linkname = link
		}
		if err := tw.WriteHeader(hdr); err != nil {
			t.Fatal(err)
		}
		if content != "" {
			if _, err := tw.Write([]byte(content)); err != nil {
				t.Fatal(err)
			}
		}
	}
	write("windows/", tar.TypeDir, 0o755, "", "")
	write("windows/system32", tar.TypeDir, 0o755, "", "")
	write("windows/system32/base.txt", tar.TypeReg, 0o644, "base layer\n", "")
	write("windows/link.txt", tar.TypeSymlink, 0, "", "system32/base.txt")
	write("windows/hard.txt", tar.TypeLink, 0o644, "", "windows/system32/base.txt")
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if !compressed {
		return plain.Bytes()
	}
	var gz bytes.Buffer
	gzw := gzip.NewWriter(&gz)
	if _, err := gzw.Write(plain.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err := gzw.Close(); err != nil {
		t.Fatal(err)
	}
	return gz.Bytes()
}

// TestExtractTarLayer covers both layer encodings: the plain tar (the
// historical MCR format) and the gzip-compressed tar (the current one)
// must both extract identically.
func TestExtractTarLayer(t *testing.T) {
	for _, compressed := range []bool{false, true} {
		label := "plain"
		if compressed {
			label = "gzip"
		}
		t.Run(label, func(t *testing.T) {
			dir := t.TempDir()
			blob := filepath.Join(dir, "layer.blob")
			if err := os.WriteFile(blob, makeLayerTar(t, compressed), 0o644); err != nil {
				t.Fatal(err)
			}
			dest := filepath.Join(dir, "extracted")
			if err := extractTarLayer(blob, dest); err != nil {
				t.Fatalf("extractTarLayer: %v", err)
			}
			got, err := os.ReadFile(filepath.Join(dest, "windows", "system32", "base.txt"))
			if err != nil {
				t.Fatalf("reading extracted file: %v", err)
			}
			if string(got) != "base layer\n" {
				t.Fatalf("content = %q, want %q", got, "base layer\n")
			}
			link, err := os.Readlink(filepath.Join(dest, "windows", "link.txt"))
			if err != nil {
				t.Fatalf("reading extracted symlink: %v", err)
			}
			if link != "system32/base.txt" {
				t.Fatalf("symlink target = %q, want %q", link, "system32/base.txt")
			}
			hard, err := os.ReadFile(filepath.Join(dest, "windows", "hard.txt"))
			if err != nil {
				t.Fatalf("reading extracted hardlink: %v", err)
			}
			if string(hard) != "base layer\n" {
				t.Fatalf("hardlink content = %q, want %q", hard, "base layer\n")
			}
		})
	}
}

// TestExtractTarLayerCrossLayerHardlink: a windows layered image's hard
// link may target a file created by a LOWER layer (the user's first real
// windows run hit exactly this: the servercore role layer linking a GAC
// assembly file the base layer installed — os.Link then failed with
// "The system cannot find the path specified"). The target must be
// materialized from the topmost lower layer that has it, and the link's
// parent dirs created (the tar declares no entry for them).
func TestExtractTarLayerCrossLayerHardlink(t *testing.T) {
	dir := t.TempDir()
	lower := filepath.Join(dir, "lower")
	upper := filepath.Join(dir, "upper")

	// The lower layer: Files/x.txt.
	var lb bytes.Buffer
	tw := tar.NewWriter(&lb)
	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	must(tw.WriteHeader(&tar.Header{Name: "Files/", Typeflag: tar.TypeDir, Mode: 0o755}))
	must(tw.WriteHeader(&tar.Header{Name: "Files/x.txt", Typeflag: tar.TypeReg, Mode: 0o644, Size: 6}))
	if _, err := tw.Write([]byte("lower\n")); err != nil {
		t.Fatal(err)
	}
	must(tw.Close())
	lbPath := filepath.Join(dir, "lower.tar")
	must(os.WriteFile(lbPath, lb.Bytes(), 0o644))
	must(extractTarLayer(lbPath, lower))

	// The upper layer: ONE entry — a hard link to Files/x.txt (not in
	// this layer) under an undeclared directory.
	var ub bytes.Buffer
	tw = tar.NewWriter(&ub)
	must(tw.WriteHeader(&tar.Header{Name: "Files/GAC_64/link.txt", Typeflag: tar.TypeLink, Mode: 0o644, Linkname: "Files/x.txt"}))
	must(tw.Close())
	ubPath := filepath.Join(dir, "upper.tar")
	must(os.WriteFile(ubPath, ub.Bytes(), 0o644))
	must(extractTarLayer(ubPath, upper, lower))

	got, err := os.ReadFile(filepath.Join(upper, "Files", "GAC_64", "link.txt"))
	if err != nil {
		t.Fatalf("reading materialized hardlink: %v", err)
	}
	if string(got) != "lower\n" {
		t.Fatalf("content = %q, want %q", got, "lower\n")
	}
	fi, err := os.Stat(filepath.Join(upper, "Files", "GAC_64", "link.txt"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode()&os.ModeType != 0 {
		t.Fatalf("materialized entry mode = %v, want regular file", fi.Mode())
	}

	// A target missing from ALL layers is a broken image: clear error.
	var bad bytes.Buffer
	tw = tar.NewWriter(&bad)
	must(tw.WriteHeader(&tar.Header{Name: "Files/y.txt", Typeflag: tar.TypeLink, Mode: 0o644, Linkname: "Files/nope.txt"}))
	must(tw.Close())
	badPath := filepath.Join(dir, "bad.tar")
	must(os.WriteFile(badPath, bad.Bytes(), 0o644))
	if err := extractTarLayer(badPath, filepath.Join(dir, "upper2"), lower); err == nil {
		t.Fatal("expected an error for a hardlink target missing from every layer")
	}
}

// TestFileSHA256Hex: the streaming hash (a GB-size blob never loads into
// memory) against a known vector.
func TestFileSHA256Hex(t *testing.T) {
	p := filepath.Join(t.TempDir(), "f.bin")
	if err := os.WriteFile(p, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	got, err := fileSHA256Hex(p)
	if err != nil {
		t.Fatal(err)
	}
	// sha256("hello\n") — a known vector, not computed in-test.
	want := "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"
	if got != want {
		t.Fatalf("sha256 = %s, want %s", got, want)
	}
	if _, err := fileSHA256Hex(filepath.Join(t.TempDir(), "missing")); err == nil {
		t.Fatal("expected an error for a missing file")
	}
}

// TestBlobMatchesDigest: the skip-the-download predicate (an
// already-fetched artifact is reused only when it verifies).
func TestBlobMatchesDigest(t *testing.T) {
	p := filepath.Join(t.TempDir(), "layer.blob")
	if err := os.WriteFile(p, []byte("hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	digest := "sha256:5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03"
	if !blobMatchesDigest(p, digest) {
		t.Fatal("a matching blob must verify")
	}
	if blobMatchesDigest(p, "sha256:0000000000000000000000000000000000000000000000000000000000000000") {
		t.Fatal("a mismatched blob must not verify")
	}
	if blobMatchesDigest(filepath.Join(t.TempDir(), "missing"), digest) {
		t.Fatal("a missing blob must not verify")
	}
}

// TestPublishedSHA256: the checksums-file lookup (the linux base's
// published digest comes from a SHA256SUMS file next to the artifact).
func TestPublishedSHA256(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03 *want.tar.xz\n"+
			"deadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeefdeadbeef  other.tar.xz\n")
	}))
	defer srv.Close()
	got, err := publishedSHA256(context.Background(), srv.URL, "want.tar.xz")
	if err != nil {
		t.Fatal(err)
	}
	if got != "5891b5b522d5df086d0ff0b110fbd9d21bb4fc7163af34d08286a2e846f6be03" {
		t.Fatalf("digest = %s", got)
	}
	// no such entry
	if _, err := publishedSHA256(context.Background(), srv.URL, "missing.tar.xz"); err == nil {
		t.Fatal("expected an error for a missing entry")
	}
	// non-200
	bad := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusInternalServerError)
	}))
	defer bad.Close()
	if _, err := publishedSHA256(context.Background(), bad.URL, "want.tar.xz"); err == nil {
		t.Fatal("expected an error for a non-200 response")
	}
}

// TestLayerExtracted: the provisioner's resume check must distinguish a
// COMPLETE extraction (non-empty dir + completion marker) from a stale
// PARTIAL dir — the user's first real run left exactly that behind
// (the pre-marker bevin failed mid-layer-2), and a partial dir must be
// re-extracted, not counted as done.
func TestLayerExtracted(t *testing.T) {
	baseDir := t.TempDir()
	id := "layer123"
	dest := filepath.Join(baseDir, id)

	// Nothing there yet.
	if layerExtracted(baseDir, id) {
		t.Fatal("layerExtracted with no layer dir")
	}
	// A PARTIAL dir (no marker) — must NOT count as done.
	if err := os.MkdirAll(filepath.Join(dest, "Files"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dest, "Files", "partial.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	if layerExtracted(baseDir, id) {
		t.Fatal("layerExtracted with a partial (unmarked) dir")
	}
	// A marker alone (no dir) — still not done.
	if err := markLayerComplete(baseDir, "markeronly"); err != nil {
		t.Fatal(err)
	}
	if layerExtracted(baseDir, "markeronly") {
		t.Fatal("layerExtracted with a marker but no layer dir")
	}
	// Marker + non-empty dir = done.
	if err := markLayerComplete(baseDir, id); err != nil {
		t.Fatal(err)
	}
	if !layerExtracted(baseDir, id) {
		t.Fatal("layerExtracted with a marked, non-empty dir")
	}
}

// TestIsHTMLBody: the 200-HTML guard that turns "a network filter
// served its own page" from an inscrutable JSON parse error into an
// actionable one.
func TestIsHTMLBody(t *testing.T) {
	cases := []struct {
		name        string
		body        []byte
		contentType string
		want        bool
	}{
		{"html content type", []byte(`{"token":"x"}`), "text/html; charset=utf-8", true},
		{"html body", []byte("<!doctype html><html><head>…"), "application/octet-stream", true},
		{"html body after whitespace + BOM", []byte("\xef\xbb\xbf  \n<html>"), "", true},
		{"json body", []byte(`{"token":"x"}`), "application/json", false},
		{"empty body", nil, "", false},
		{"plain text body", []byte("404 not found"), "text/plain", false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isHTMLBody(c.body, c.contentType); got != c.want {
				t.Fatalf("isHTMLBody(%q, %q) = %v, want %v", c.body, c.contentType, got, c.want)
			}
		})
	}
}
