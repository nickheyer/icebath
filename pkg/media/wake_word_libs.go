package media

import (
	"context"

	"github.com/nickheyer/icebath/pkg/internal/openwakeword"
	"github.com/nickheyer/icebath/pkg/internal/porcupine"
	"github.com/nickheyer/icebath/pkg/internal/sherpaonnx"
	"github.com/nickheyer/icebath/pkg/media/speech"
)

// Whether the process runs the speech engine on the GPU, the Piper setting every engine user follows
func speechOnGPU(ctx context.Context) bool {
	return pyTruthy(dictGetDefault(ttsSection(wakeLoadTTSConfig(ctx), "piper"), "use_cuda", false))
}

// Native speech engine of the process, its library fetched on first use
func wakeSpeechEngine(ctx context.Context) (*sherpaonnx.Module, error) {
	return speech.Engine(ctx, speechOnGPU(ctx))
}

// Loads the Porcupine library, fetching its package on first use
func LoadPorcupineLibrary(ctx context.Context) error {
	_, err := porcupine.Load(ctx)
	return err
}

// Whether the speech engine behind the sherpa wake word publishes a library for this platform
func SherpaOnnxAvailable() bool { return speech.Supported() }

// Loads the TensorFlow Lite library of openWakeWord, fetching its package on first use
func LoadOpenWakeWordLibrary(ctx context.Context) error {
	_, err := openwakeword.Load(ctx)
	return err
}
