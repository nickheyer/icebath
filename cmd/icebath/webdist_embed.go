//go:build webdist

package main

import (
	"embed"
	"io/fs"

	"github.com/nickheyer/icebath/pkg/webbundle"
)

//go:embed all:web_dist all:webapp_dist
var webDistFS embed.FS

// Hands the dashboard and Webapp bundles make web built into cmd/icebath to the servers of this binary
func init() {
	for bundle, dir := range map[webbundle.Bundle]string{webbundle.Dashboard: "web_dist", webbundle.Webapp: "webapp_dist"} {
		sub, err := fs.Sub(webDistFS, dir)
		if err != nil {
			panic(err)
		}
		webbundle.Embedded[bundle] = sub
	}
}
