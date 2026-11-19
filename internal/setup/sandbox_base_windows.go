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
// Registry protocol notes (verified against live MCR 2026-11-18):
//   - MCR serves the public image ANONYMOUSLY (manifest + blob 200
//     without auth). The client still follows a 401 +
//     WWW-Authenticate: Bearer challenge by fetching an anonymous token
//     from the named realm — the standard registry v2 flow — in case the
//     registry (or a future image source) requires it.
//   - The old eager token URL (/oauth2/token?service=...) is DEAD: MCR's
//     edge now answers it with the registry's HTML discovery portal
//     (HTTP 200, content-type text/html), which made the token parse
//     fail with "invalid character '<' looking for beginning of value".
//     Never hardcode a token URL — only follow the 401 challenge.
//   - The layers are now GZIP-COMPRESSED (mediaType
//     ...rootfs.diff.tar.gzip; MCR used to serve them uncompressed).
//     extractTarLayer (sandbox_base_common.go) detects gzip by magic.
//
// NOTE: this path is type-checked by cross-compilation + the shared
// pieces are unit-tested on linux, but has NOT been exercised
// end-to-end on a real Windows host yet — the first real windows setup
// is the integration test.

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
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

	// winBaseUserAgent identifies the client (registries see it; a
	// default Go UA draws no attention but some network filters treat
	// it specially).
	winBaseUserAgent = "bevin (github.com/utopiafallen/bevin)"
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

// provisionSandboxBase pulls the servercore image (manifest → layers —
// a token is fetched only if the registry challenges with 401) and
// extracts every layer under <data>/sandboxes/base/<layer>/, then writes
// base.json. It is resumable: an existing, non-empty layer dir is
// skipped (a re-run after a failed download continues where it left
// off).
func provisionSandboxBase(ctx context.Context, dataDir string, progress func(frac float64, detail string)) error {
	baseDir := sandbox.BaseDir(dataDir)
	if err := os.MkdirAll(baseDir, 0o755); err != nil {
		return err
	}

	manifestDigest, err := resolveManifestDigest(ctx)
	if err != nil {
		return fmt.Errorf("resolving the base image manifest: %w", err)
	}
	body, err := registryGet(ctx, "/v2/"+winBaseImage+"/manifests/"+manifestDigest, manifestAccepts...)
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
		if err := registryDownload(ctx, "/v2/"+winBaseImage+"/blobs/"+l.Digest, blob, func(frac float64, detail string) {
			if progress != nil {
				progress((float64(got)+frac*float64(l.Size))/float64(total),
					fmt.Sprintf("layer %d of %d: %s", i+1, len(manifest.Layers), detail))
			}
		}); err != nil {
			return fmt.Errorf("downloading layer %d (%s): %w", i+1, id, err)
		}
		// Lower layers (already extracted, in image order) — a layer's
		// hard link may target a file a LOWER layer created (cross-layer
		// hard links are normal in windows layered images: a role layer
		// hard-links a GAC file the base layer installed).
		lowers := make([]string, len(bi.Layers))
		for j, l := range bi.Layers {
			lowers[j] = l.Path
		}
		// Windows registry layers are tar — gzip-compressed today,
		// uncompressed historically (extractTarLayer detects the magic).
		if err := extractTarLayer(blob, dest, lowers...); err != nil {
			os.Remove(blob)
			os.RemoveAll(dest) // a PARTIAL extraction must not masquerade as "done" on resume
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

// registryDo performs a GET against the registry API, following the
// registry's own auth challenge: on a 401 with a Bearer
// WWW-Authenticate header it fetches an anonymous token from the realm
// the header names and retries once. Registries may also serve public
// content anonymously (MCR does today) — in that case no token is ever
// fetched.
func registryDo(ctx context.Context, path string, accepts []string) (*http.Response, error) {
	doOnce := func(token string) (*http.Response, error) {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, winBaseRegistry+path, nil)
		if err != nil {
			return nil, err
		}
		req.Header.Set("User-Agent", winBaseUserAgent)
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		for _, a := range accepts {
			req.Header.Add("Accept", a)
		}
		return http.DefaultClient.Do(req)
	}
	resp, err := doOnce("")
	if err != nil {
		return nil, err
	}
	if resp.StatusCode == http.StatusUnauthorized {
		header := resp.Header.Get("WWW-Authenticate")
		resp.Body.Close()
		token, terr := registryAuth(ctx, header)
		if terr != nil {
			return nil, terr
		}
		resp, err = doOnce(token)
		if err != nil {
			return nil, err
		}
	}
	return resp, nil
}

// registryAuth fetches an anonymous pull token from the realm named by a
// 401's WWW-Authenticate header (Bearer realm="…",service="…",scope="…" —
// the registry is the authority on where its token endpoint is; never
// hardcode the URL — MCR's old /oauth2/token URL now 200s an HTML
// portal page).
func registryAuth(ctx context.Context, header string) (string, error) {
	params := map[string]string{}
	for _, part := range strings.Split(strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(header), "Bearer")), ",") {
		kv := strings.SplitN(strings.TrimSpace(part), "=", 2)
		if len(kv) == 2 {
			params[kv[0]] = strings.Trim(kv[1], `"`)
		}
	}
	realm := params["realm"]
	if realm == "" {
		return "", fmt.Errorf("the registry's 401 challenge names no token realm (WWW-Authenticate: %q)", header)
	}
	u, err := url.Parse(realm)
	if err != nil {
		return "", err
	}
	q := u.Query()
	if s := params["service"]; s != "" {
		q.Set("service", s)
	}
	if s := params["scope"]; s != "" {
		q.Set("scope", s)
	}
	u.RawQuery = q.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, u.String(), nil)
	if err != nil {
		return "", err
	}
	req.Header.Set("User-Agent", winBaseUserAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d from the registry's token endpoint (%s)", resp.StatusCode, u.Host)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return "", err
	}
	if isHTMLBody(body, resp.Header.Get("Content-Type")) {
		return "", fmt.Errorf("the registry's token endpoint (%s) returned an HTML page instead of a JSON token — a network filter, proxy, or captive portal is probably intercepting the connection; the request must reach %s unfiltered", u.Host, u.Host)
	}
	var t struct {
		Token       string `json:"token"`
		AccessToken string `json:"access_token"`
	}
	if err := json.Unmarshal(body, &t); err != nil {
		return "", fmt.Errorf("parsing the token response from %s: %w", u.Host, err)
	}
	if t.Token != "" {
		return t.Token, nil
	}
	if t.AccessToken != "" {
		return t.AccessToken, nil
	}
	return "", fmt.Errorf("the registry's token endpoint (%s) returned no token", u.Host)
}

// resolveManifestDigest resolves the tag to a concrete amd64 windows
// manifest digest: the tag may be an image INDEX (multi-arch) — find the
// amd64/windows entry — or a single manifest (use the content digest,
// computed as the sha256 of the manifest content — its definition).
func resolveManifestDigest(ctx context.Context) (string, error) {
	body, err := registryGet(ctx, "/v2/"+winBaseImage+"/manifests/"+winBaseTag, manifestAccepts...)
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
	sum := sha256.Sum256(body)
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

// registryGet performs an authenticated registry GET (see registryDo)
// and returns the JSON body. A 200 with an HTML body is the signature
// of network interception and is reported as such, not as a JSON parse
// error.
func registryGet(ctx context.Context, path string, accepts ...string) ([]byte, error) {
	resp, err := registryDo(ctx, path, accepts)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("HTTP %d for %s", resp.StatusCode, path)
	}
	body, err := io.ReadAll(io.LimitReader(resp.Body, 16<<20))
	if err != nil {
		return nil, err
	}
	if isHTMLBody(body, resp.Header.Get("Content-Type")) {
		return nil, fmt.Errorf("the registry returned an HTML page for %s instead of JSON — a network filter, proxy, or captive portal is probably intercepting the connection to %s; the request must reach the registry unfiltered", path, winBaseRegistry)
	}
	return body, nil
}

// registryDownload streams a registry blob to dest (the same
// .part-then-rename atomicity as the plain downloader).
func registryDownload(ctx context.Context, path, dest string, progress func(frac float64, detail string)) error {
	resp, err := registryDo(ctx, path, nil)
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

// dirIsNonEmpty reports whether dir exists and holds at least one entry
// (the resume check for an already-extracted layer).
func dirIsNonEmpty(dir string) bool {
	entries, err := os.ReadDir(dir)
	return err == nil && len(entries) > 0
}
