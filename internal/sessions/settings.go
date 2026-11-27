package sessions

import (
	"encoding/json"
	"maps"

	"github.com/nickheyer/icebath/internal/config"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

// Key of a session's model settings holding the settings it runs with over those of its profile
const overridesKey = "_settings"

// Settings a session runs with over those of its profile, such as what a launch of the command line asked for its run: values by key,
// and whether the profile's config files are left out, their defaults standing in for them
type Overrides struct {
	Values map[string]any `json:"values,omitempty"`
	Bare   bool           `json:"bare,omitempty"`
}

// What the session's record keeps of its own settings, nil when it keeps none
func OverridesOf(session *sessionsv1.Session) *Overrides {
	var recorded struct {
		Overrides *Overrides `json:"_settings"`
	}
	if session.GetModelConfig() == "" || json.Unmarshal([]byte(session.GetModelConfig()), &recorded) != nil {
		return nil
	}
	return recorded.Overrides
}

// Layer the overrides set over the profile's settings, nil for none
func (o *Overrides) Layer() *config.Layer {
	if o == nil || len(o.Values) == 0 && !o.Bare {
		return nil
	}
	return &config.Layer{Values: o.Values, Bare: o.Bare}
}

// The overrides with the values over their own, a nil value dropping its key, and whether the files are left out as bare says when given
func (o *Overrides) Merge(values map[string]any, bare *bool) *Overrides {
	merged := &Overrides{Values: map[string]any{}}
	if o != nil {
		maps.Copy(merged.Values, o.Values)
		merged.Bare = o.Bare
	}
	for key, value := range values {
		if value == nil {
			delete(merged.Values, key)
		} else {
			merged.Values[key] = value
		}
	}
	if bare != nil {
		merged.Bare = *bare
	}
	return merged
}

// Change of a session's record that keeps the overrides as its own settings
func (o *Overrides) Change() Change {
	return Change{ModelConfig: map[string]any{overridesKey: o}}
}

// Settings of the session: those of its profile with its own over them
func Settings(cfg *config.Config, session *sessionsv1.Session) *config.Config {
	return cfg.In(session.GetProfile()).With(OverridesOf(session).Layer())
}
