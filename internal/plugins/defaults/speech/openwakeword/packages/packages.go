// Python set openWakeWord's models and TensorFlow Lite library come in: pyopen-wakeword
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
