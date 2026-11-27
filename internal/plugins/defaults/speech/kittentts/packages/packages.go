// Python set KittenTTS runs in: the model, soundfile, torch and the eSpeak NG loader its phonemizer reads
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
