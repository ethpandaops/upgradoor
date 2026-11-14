//go:build webui

// Package web embeds the web UI build output (web/dist). Build the UI with
// `npm ci && npm run build` in this directory, then compile with the `webui`
// build tag. Without the tag, Dist serves a placeholder page.
package web

import (
	"embed"
	"fmt"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the built web UI, rooted at the directory that holds
// index.html.
func Dist() (fs.FS, error) {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil, fmt.Errorf("web: open embedded dist: %w", err)
	}

	return sub, nil
}
