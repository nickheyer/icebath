package config

// What one session sets over the settings of its profile, such as what a launch of the command line asked for its run: values by key over
// the files and below the environment, and whether the files are left out, their defaults standing in for them
type Layer struct {
	Values map[string]any
	Bare   bool
}

// Settings of the same profile with the layer over them; a nil layer gives them as they are
func (c *Config) With(layer *Layer) *Config {
	return &Config{core: c.core, profile: c.profile, plain: c.plain, session: layer, making: c.making}
}

// Value the session's layer gives the key as the settings keep it, and whether it decides it: a value it sets, or nothing in place of the
// files under a bare layer
func (c *Config) layered(key string) (any, bool) {
	if c.session == nil {
		return nil, false
	}
	if value, ok := c.session.Values[key]; ok {
		return value, true
	}
	return nil, c.session.Bare
}
