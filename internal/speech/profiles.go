package speech

import (
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/profile"
	speechconfig "github.com/nickheyer/icebath/internal/speech/config"
)

// Speech as profiles are made: a new profile takes the recognizer, voice and voice input settings of the profile it was made from that it
// has none of its own of, unless it takes nothing from it
type Profiles struct {
	Config *config.Config
}

// Speech keeps what it has of a profile in the profile's folder, which goes with it
func (Profiles) Delete(string) error {
	return nil
}

func (p Profiles) Made(making profile.Making) ([]string, error) {
	if making.Bare || making.Launch == "" {
		return nil, nil
	}
	launch, target := p.Config.In(making.Launch).Held(false), p.Config.In(making.Profile)
	for _, key := range []string{speechconfig.STT, speechconfig.TTS, speechconfig.Voice} {
		held, ok := launch[key].(map[string]any)
		if !ok || len(held) == 0 || p.Config.Global(key) || target.Holds(key) {
			continue
		}
		if err := target.Set(key, held); err != nil {
			return nil, err
		}
	}
	return nil, nil
}
