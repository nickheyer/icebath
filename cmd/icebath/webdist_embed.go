//go:build webdist

package main

import (
	"embed"
	"io/fs"

	"github.com/nickheyer/icebath/pkg/webbundle"
)

//go:embed all:web_dist
var webDistFS embed.FS

// Hands the dashboard bundle built into cmd/icebath/web_dist to the server of a release binary
func init() {
	sub, err := fs.Sub(webDistFS, "web_dist")
	if err != nil {
		panic(err)
	}
	webbundle.Embedded = sub
}
