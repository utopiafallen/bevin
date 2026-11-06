// Package web embeds the built web UI assets into the binary.
package web

import "embed"

// FS holds the web UI static assets.
//
//go:embed static
var FS embed.FS
