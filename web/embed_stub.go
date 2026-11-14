//go:build !webui

// Package web embeds the web UI build output (web/dist). Without the `webui`
// build tag it serves a placeholder page, so the module builds and tests
// without node.
package web

import (
	"embed"
	"fmt"
	"io/fs"
)

//go:embed placeholder/index.html
var placeholder embed.FS

// Dist returns a file system that holds only a placeholder index.html saying
// that the UI was not built into this binary.
func Dist() (fs.FS, error) {
	sub, err := fs.Sub(placeholder, "placeholder")
	if err != nil {
		return nil, fmt.Errorf("web: open placeholder: %w", err)
	}

	return sub, nil
}
