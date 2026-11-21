// Package web embeds the built web interface. Build it with "make web" (or
// "npm run build" in this directory), which writes it to web/dist; without
// it the server shows a placeholder page.
package web

import (
	"embed"
	"io/fs"
)

//go:embed all:dist
var dist embed.FS

// Dist returns the built web interface, or nil if it has not been built.
func Dist() fs.FS {
	sub, err := fs.Sub(dist, "dist")
	if err != nil {
		return nil
	}
	if _, err := fs.Stat(sub, "index.html"); err != nil {
		return nil
	}
	return sub
}
