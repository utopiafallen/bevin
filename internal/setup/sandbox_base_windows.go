//go:build windows

package setup

// The windows base image: a Windows servercore image pulled from the
// Microsoft Container Registry (mcr.microsoft.com) — the same images
// docker desktop pulls for Windows containers. A Hyper-V isolated
// container is created from the image's read-only layers + a per-user
// read-write layer, so the setup extracts every layer into its own
// directory under <data>/sandboxes/base/ and records the layer list in
// base.json (the HCS Layers config).
//
// NOTE: this path is type-checked by cross-compilation but has NOT been
// exercised end-to-end on a real Windows host yet — the first real
// windows setup is the integration test.

import (
	"archive/tar"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"

	"github.com/utopiafallen/bevin/internal/sandbox"
)

const (
	winBaseRegistry = "https://mcr.microsoft.com"
	winBaseImage    = "windows/servercore"
	// winBaseTag is the latest Windows Server LTSC line (user directive:
	// track the latest LTSC; a new bevin release bumps this when
	// Microsoft cuts a new one). ltsc2025 = Windows Server 2025.
	winBaseTag = "ltsc2025"
)

var manifestAccepts = []string{
	"application/vnd.docker.distribution.manifest.list.v2+json",
	"application/vnd.oci.image.index.v1+json",
	"application/vnd.docker.distribution.manifest.v2+json",
	"application/vnd.oci.image.manifest.v1+json",
}

// sandboxBaseImage is base.json — the extracted base image layers (in
// image order, read-only) for the HCS Layers list.
type sandboxBaseImage struct {
	Layers []sandboxBaseLayer `json:"layers"`
}

type sandboxBaseLayer struct {
	ID   string `json:"id"`
	Path string `json:"path"`
}

// sandboxBaseReady reports whether the windows base image is provisioned.
func sandboxBaseReady(dataDir string) bool {
	_, err := os.Stat(filepath.Join(sandbox.BaseDir(dataDir), "base.json"))
	return err == nil
}

// provisionSandboxBase pulls the servercore image (token → manifest →
// layers) and extracts every layer under <data>/sandboxes/base/<layer>/,
// then writes base.json. It is resumable: an existing, non-empty layer
// dir is skipped (a re-run after a failed download continues where it
// left off).
func provisionSandboxBase(ctx context.Context, dataDir string, progress func(frac float64, detail string)) error {
	baseDir := sandbox.BaseDir(dataDir)
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return err
	}

	token, err := mcrToken(ctx)
	if err != nil {
		return fmt.Errorf("registry auth: %w", err)
	}
	manifestDigest, err := resolveManifestDigest(ctx, token)
	if err != nil {
		return fmt.Errorf("resolving the base image manifest: %w", err)
	}
	body, err := registryGet(ctx, token, "/v2/"+winBaseImage+"/manifests/"+manifestDigest, manifestAccepts...)
	if err != nil {
		return fmt.Errorf("fetching the image manifest: %w", err)
	}
	var manifest struct {
		Layers []struct {
			Digest string `json:"digest"`
			Size   int64  `json:"size"`
		} `json:"layers"`
	}
	if err := json.Unmarshal(body, &manifest); err != nil {
		return fmt.Errorf("parsing the image manifest: %w", err)
	}
	if len(manifest.Layers) == 0 {
		return fmt.Errorf("the base image manifest has no layers")
	}

	var total int64
	for _, l := range manifest.Layers {
		total += l.Size
	}

	var bi sandboxBaseImage
	var got int64
	for i, l := range manifest.Layers {
		id := strings.TrimPrefix(l.Digest, "sha256:")
		dest := filepath.Join(baseDir, id)
		if dirIsNonEmpty(dest) {
			// Already extracted (a resume) — record + keep going.
			bi.Layers = append(bi.Layers, sandboxBaseLayer{ID: id, Path: dest})
			got += l.Size
			continue
		}
		if progress != nil {
			progress(float64(got)/float64(total),
				fmt.Sprintf("layer %d of %d: %s", i+1, len(manifest.Layers), humanBytes(l.Size)))
		}
		blob := filepath.Join(baseDir, id+".blob")
		if err := registryDownload(ctx, token, "/v2/"+winBaseImage+"/blobs/"+l.Digest, blob, func(frac float64, detail string) {
			if progress != nil {
				progress((float64(got)+frac*float64(l.Size))/float64(total),
					fmt.Sprintf("layer %d of %d: %s", i+1, len(manifest.Layers), detail))
			}
		}); err != nil {
			return fmt.Errorf("downloading layer %d (%s): %w", i+1, id, err)
		}
		// Windows registry blobs are UNCOMPRESSED tars.
		if err := extractTarPlain(blob, dest); err != nil {
			os.Remove(blob)
			return fmt.Errorf("extracting layer %d (%s): %w", i+1, id, err)
		}
		os.Remove(blob)
		bi.Layers = append(bi.Layers, sandboxBaseLayer{ID: id, Path: dest})
		got += l.Size
	}

	data, err := json.MarshalIndent(bi, "", "  ")
	if err != nil {
		return err
	}
	if err := os.WriteFile(filepath.Join(baseDir, "base.json"), data, 0o644); err != nil {
		return err
	}
	if progress != nil {
		progress(1, "base image ready")
	}
	return nil
}

// mcrToken fetches an anonymous pull token for the image (MCR allows
// anonymous pulls of public images — no credentials).
func mcrToken(ctx context.Context) (string, error) {
	body, err := registryGet(ctx, "", "/oauth2/token?service=mcr.microsoft.com&scope=repository:"+winBaseImage+":pull")
	if err != nil {
		return "", err
	}
	var t struct {
		Token string `json:"token"`
		// MCR also answers the legacy "access_token" field.
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &t); err != nil {
		return "", err
	}
	if t.Token == "" {
		t.Token = t.AccessToken
	}
	if t.Token == "" {
		return "", fmt.Errorf("the registry returned no token")
	}
	return t.Token, nil
}

// resolveManifestDigest resolves the tag to a concrete amd64 windows
// manifest digest: the tag may be an image INDEX (multi-arch) — find the
// amd64/windows entry — or a single manifest (use the content digest).
func resolveManifestDigest(ctx context.Context, token string) (string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet,
		winBaseRegistry+"/v2/"+winBaseImage+"/manifests/"+winBaseTag, nil)
	if err != nil {
		return "", err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for _, a := range manifestAccepts {
		req.Header.Add("Accept", a)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d for the image tag", resp.StatusCode)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
	if err != nil {
		return "", err
	}
	// An index: pick the amd64/windows entry.
	var index struct {
		Manifests []struct {
			Digest   string `json:"digest"`
			Platform struct {
				Architecture string `json:"architecture"`
				OS           string `json:"os"`
			} `json:"platform"`
		} `json:"manifests"`
	}
	if err := json.Unmarshal(body, &index); err == nil && len(index.Manifests) > 0 {
		for _, m := range index.Manifests {
			if m.Platform.OS == "windows" && m.Platform.Architecture == "amd64" {
				return m.Digest, nil
			}
		}
		return "", fmt.Errorf("the image index has no amd64/windows entry")
	}
	// A single manifest: the content digest is its identity.
	digest := resp.Header.Get("Docker-Content-Digest")
	if digest == "" {
		return "", fmt.Errorf("the registry returned no content digest")
	}
	return digest, nil
}

// registryGet performs an authenticated registry GET (empty token =
// anonymous — the token endpoint itself).
func registryGet(ctx context.Context, token, path string, accepts ...string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, winBaseRegistry+path, nil)
	if err != nil {
		return nil, err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for _, a := range accepts {
		req.Header.Add("Accept", a)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, path)
	}
	return io.ReadAll(io.LimitReader(resp.Body, 16<<20))
}

// registryDownload streams a registry blob to dest (the same
// .part-then-rename atomicity as the plain downloader).
func registryDownload(ctx context.Context, token, path, dest string, progress func(frac float64, detail string)) error {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, winBaseRegistry+path, nil)
	if err != nil {
		return err
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d for the blob", resp.StatusCode)
	}
	part := dest + ".part"
	out, err := os.Create(part)
	if err != nil {
		return err
	}
	pw := &sandboxProgressWriter{w: out, total: resp.ContentLength, progress: progress}
	written, err := io.Copy(pw, resp.Body)
	cerr := out.Close()
	if err != nil {
		os.Remove(part)
		return err
	}
	if cerr != nil {
		os.Remove(part)
		return cerr
	}
	if resp.ContentLength > 0 && written != resp.ContentLength {
		os.Remove(part)
		return fmt.Errorf("incomplete blob download (%d of %d bytes)", written, resp.ContentLength)
	}
	return os.Rename(part, dest)
}

// extractTarPlain extracts an UNCOMPRESSED tar (windows registry blobs)
// into destDir.
func extractTarPlain(archive, destDir string) error {
	f, err := os.Open(archive)
	if err != nil {
		return err
	}
	defer f.Close()
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}
	tr := tar.NewReader(f)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return err
		}
		if err := writeRootfsTarEntry(destDir, hdr, tr); err != nil {
			return err
		}
	}
	return nil
}

// dirIsNonEmpty reports whether dir exists and holds at least one entry
// (the resume check for an already-extracted layer).
func dirIsNonEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}
