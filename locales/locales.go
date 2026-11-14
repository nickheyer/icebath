// Translation catalogs and key lists embedded in the binary as a read-only file system
package locales

import (
	"embed"
	"io/fs"
)

//go:embed *.yaml _keys.tui.json _keys.desktop.json
var files embed.FS

func FS() fs.FS {
	return files
}
