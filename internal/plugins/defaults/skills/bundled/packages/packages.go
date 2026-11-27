// Python dependency sets the bundled skills' scripts run on: the document skills, PDF extraction, Google Workspace and YouTube transcripts
package packages

import (
	_ "embed"

	"github.com/nickheyer/icebath/internal/packages"
)

//go:embed extras.json
var extras []byte

func Register(registry *packages.Registry) {
	registry.Register(packages.Declared{Sets: packages.Extras(extras)})
}
