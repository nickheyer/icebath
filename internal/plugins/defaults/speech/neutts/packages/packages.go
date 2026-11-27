// Python set NeuTTS runs in: the voice cloning model with llama.cpp, torch, torchaudio and torchao
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
