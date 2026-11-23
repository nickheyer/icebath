package config_test

import (
	"context"
	"fmt"
	"reflect"
	"strconv"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/config/roundtrip"
	"github.com/nickheyer/icebath/pkg/settings"
)

// Links the settings registry into the test binary, so the facade under test writes and reads real settings as the program does, and gives the round trip tests values each setting takes
func init() { config.SettingSample = settingSample }

// Texts a person could type for a setting of the kind, most specific first
func sampleTexts(e settings.Entry, g *roundtrip.RNG) []string {
	switch e.Kind() {
	case settings.KindBool:
		return []string{"true", "false"}
	case settings.KindEnum:
		var out []string
		for _, choice := range settings.Describe(context.Background(), e).Choices {
			out = append(out, choice.Value)
		}
		return out
	case settings.KindInt, settings.KindDuration:
		out := []string{strconv.Itoa(g.RandInt(1, 1000000))}
		for _, n := range []int{g.RandInt(1, 500), g.RandInt(1, 50), 7, 3, 2, 1, 0} {
			out = append(out, strconv.Itoa(n))
		}
		return out
	case settings.KindFloat:
		out := []string{roundtrip.FloatRepr(roundtrip.Round(g.Uniform(1, 999), 2) + 0.25)}
		for _, x := range []float64{roundtrip.Round(g.Uniform(1, 9), 2), 0.75, 0.5, 0.25} {
			out = append(out, roundtrip.FloatRepr(x))
		}
		return out
	case settings.KindString, settings.KindPath:
		return []string{fmt.Sprintf("c18-%s-%d", roundtrip.Choice(g, roundtrip.Words), g.RandRange(1000000))}
	}
	return nil
}

// Text to type for a registered scalar setting and the value config.yaml then holds: the first sample the setting accepts that differs from the current value
func settingSample(key string, current any, g *roundtrip.RNG) (string, any, bool) {
	e := settings.FindKey(key)
	if e == nil {
		return "", nil, false
	}
	for _, text := range sampleTexts(e, g) {
		stored, err := e.Coerce(text)
		if err != nil || stored == nil || reflect.DeepEqual(stored, current) || roundtrip.Equal(stored, current) {
			continue
		}
		if validate := e.Info().Validate; validate != nil && validate(stored) != nil {
			continue
		}
		return text, stored, true
	}
	return "", nil, false
}
