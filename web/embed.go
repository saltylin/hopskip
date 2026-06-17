// Package web embeds the built Vue shell so the daemon ships as a single binary.
// At runtime an optional HOPSKIP_WEB_DIR overlay is resolved disk-first, falling
// back to this embedded tree (see spec/frontend-extension-protocol.md §2).
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:public
var assets embed.FS

// FS returns the embedded web/public tree rooted at its top level.
func FS() fs.FS {
	sub, err := fs.Sub(assets, "public")
	if err != nil {
		panic(err)
	}
	return sub
}
