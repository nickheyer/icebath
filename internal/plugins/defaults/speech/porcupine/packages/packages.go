// Python set Picovoice Porcupine's library, model and keywords come in: pvporcupine
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
