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
		})
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
