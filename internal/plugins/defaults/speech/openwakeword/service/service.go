// openWakeWord: the classifier of a model file trained for one phrase over the shared mel and embedding models, on the TensorFlow Lite
// library the pyopen-wakeword package of its Python set ships
package service

import (
	"context"
	"errors"
	"fmt"

	"github.com/nickheyer/icebath/internal/packages"
	"github.com/nickheyer/icebath/internal/plugins/defaults/speech/openwakeword/config"
	"github.com/nickheyer/icebath/internal/speech"
)

func Register(registry *speech.Registry) {
	registry.Register(speech.Services{Waking: []speech.Engine{{
		Name: config.Name, Aliases: []string{"oww", "local"}, Title: "openWakeWord", About: "Hears the phrase its model file was trained for",
		Rank: 1, Extra: config.Extra, Model: config.Model,
		Supported: func(sp *speech.Speech) bool {
			reason, known := sp.Packages().ExtraUnsupported(config.Extra)
			return known && reason == ""
		},
		Installed: func(sp *speech.Speech) bool {
			_, ok := sp.Packages().ExtraModule(config.Extra, "pyopen_wakeword")
			return ok
		},
		New: open,
	}}})
}

// Builds the engine on the model file set, a refusal naming the setting when none is
func open(_ context.Context, wake *speech.Wake) (speech.Hotword, error) {
	model := wake.Setting(config.Model)
	if model == "" {
		return nil, fmt.Errorf("%s is not set. openWakeWord detects only the phrase its model file was trained for; set the path of that file, or set "+
			"wake_word.provider to auto so the open vocabulary engine detects %s.", config.Model, wake.Phrase)
	}
	if _, ok := wake.Speech.Packages().ExtraPython(config.Extra); !ok {
		return nil, errors.New("the " + config.Extra + " wake engine is not provisioned. Run `" + packages.InstallHint(config.Extra) + "`")
	}
	dir, ok := wake.Speech.Packages().ExtraModule(config.Extra, "pyopen_wakeword")
	if !ok {
		return nil, errors.New("the Python package pyopen_wakeword is not installed in the wake word environment")
	}
	t, err := library(dir)
	if err != nil {
		return nil, err
	}
	f, err := newFeatures(t, dir)
	if err != nil {
		return nil, err
	}
	c, err := newClassifier(t, model)
	if err != nil {
		f.close()
		return nil, err
	}
	return &engine{features: f, classifier: c, threshold: wake.Sensitivity, needed: max(1, wake.Frames)}, nil
}

// Scores frames against the sensitivity as the raw threshold, firing after the confirmation frames in a row
type engine struct {
	features   *features
	classifier *classifier
	threshold  float64
	needed     int
	streak     int
}

func (e *engine) FrameLength() int { return chunk }

func (e *engine) Process(frame []int16) (bool, error) {
	over := false
	err := e.features.process(frame, func(embedding []float32) error {
		return e.classifier.process(embedding, func(score float32) {
			if float64(score) >= e.threshold {
				over = true
			}
		})
	})
	if err != nil {
		return false, err
	}
	if !over {
		e.streak = 0
		return false, nil
	}
	if e.streak++; e.streak >= e.needed {
		e.streak = 0
		return true, nil
	}
	return false, nil
}

// Clears the rolling buffers so audio from before a pause cannot fire
func (e *engine) Reset() error {
	e.streak = 0
	e.features.reset()
	e.classifier.reset()
	return nil
}

func (e *engine) Close() {
	e.Reset()
	e.features.close()
	e.classifier.close()
}
