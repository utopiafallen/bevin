package setup

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
