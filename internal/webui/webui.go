// Package webui holds the static files of the X-Node web interface.
//
// The files are embedded into the binary, so the interface works on a server
// without internet access and without a Node.js toolchain. The HTML is a single
// page; app.js talks to the JSON API under the configured path.
package webui

import (
	"embed"
	"io/fs"
)

//go:embed assets
var embedded embed.FS

// Assets returns the static files rooted at the assets directory.
func Assets() fs.FS {
	sub, err := fs.Sub(embedded, "assets")
	if err != nil {
		// The embed directive guarantees the directory exists; a failure here
		// is a programming error, not a runtime condition.
		panic(err)
	}
	return sub
}
