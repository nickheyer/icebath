package pvporcupine

import (
	"context"
	"runtime"
	"sort"
	"strings"
)

func keepAlive(v any) { runtime.KeepAlive(v) }

// Built in keyword names in sorted order
func (m *Module) Keywords() []string {
	out := make([]string, 0, len(m.KeywordPaths))
	for k := range m.KeywordPaths {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// The optional arguments of Create, a nil field taking its default
type CreateOptions struct {
	LibraryPath   *string
	ModelPath     *string
	Device        *string
	KeywordPaths  []string
	Keywords      []string
	Sensitivities []float64
}

// Builds an engine from keyword files or built in keyword names, defaults filled from the package tree
func (m *Module) Create(accessKey string, o CreateOptions) (*Porcupine, error) {
	var libraryPath, modelPath string
	if o.LibraryPath == nil {
		path, err := m.PvLibraryPath("")
		if err != nil {
			return nil, err
		}
		libraryPath = path
	} else {
		libraryPath = *o.LibraryPath
	}
	if o.ModelPath == nil {
		modelPath = m.PvModelPath("")
	} else {
		modelPath = *o.ModelPath
	}
	device := "best"
	if o.Device != nil {
		device = *o.Device
	}
	keywordPaths := o.KeywordPaths
	if keywordPaths == nil {
		if o.Keywords == nil {
			return nil, &PyError{Class: "ValueError", Msg: "Either `keywords` or `keyword_paths` must be set."}
		}
		keywordPaths = []string{}
		for _, x := range o.Keywords {
			path, ok := m.KeywordPaths[x]
			if !ok {
				return nil, &PyError{Class: "ValueError", Msg: `One or more keywords are not available by default. Available default keywords are:\n` +
					strings.Join(m.Keywords(), ", ")}
			}
			keywordPaths = append(keywordPaths, path)
		}
	}
	sensitivities := o.Sensitivities
	if sensitivities == nil {
		sensitivities = make([]float64, len(keywordPaths))
		for i := range sensitivities {
			sensitivities[i] = 0.5
		}
	}
	if len(sensitivities) != len(keywordPaths) {
		return nil, &PyError{Class: "ValueError", Msg: "Number of keywords does not match the number of sensitivities."}
	}
	return NewPorcupine(accessKey, libraryPath, modelPath, device, keywordPaths, sensitivities)
}

// Devices the engine can run inference on, each usable as the device argument of Create
func (m *Module) AvailableDevices(libraryPath *string) ([]string, error) {
	path := ""
	if libraryPath == nil {
		got, err := m.PvLibraryPath("")
		if err != nil {
			return nil, err
		}
		path = got
	} else {
		path = *libraryPath
	}
	return ListHardwareDevices(path)
}

// Trains a wake word model for a phrase and saves it at outputPath
func (m *Module) TrainWakeWordFromPhrase(ctx context.Context, accessKey, outputPath, language, phrase string, platform *string) error {
	return m.PvTrainModel(ctx, accessKey, outputPath, language, phrase, platform)
}
