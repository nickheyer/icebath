package auxiliary

import (
	"cmp"
	"slices"
	"strings"

	"github.com/spf13/cast"

	auxiliaryconfig "github.com/nickheyer/icebath/internal/auxiliary/config"
)

// Settings of a task as they apply to a call
type settings struct {
	provider, model, baseURL, apiKey, keyEnv, apiMode, effort string
	// Seconds a call may go without output, and once the reply started
	timeout, stall int
	extra          map[string]any
	chain          []string
}

// Route settings, all dropped from the task inherited once the task names its own provider, model or endpoint
var routeKeys = []string{auxiliaryconfig.Provider, auxiliaryconfig.Model, auxiliaryconfig.BaseURL, auxiliaryconfig.APIKey, auxiliaryconfig.KeyEnv,
	auxiliaryconfig.APIMode, auxiliaryconfig.ReasoningEffort}

// Settings of the task, those it leaves unset taken from the task it inherits
func (c *Client) settings(task Task) settings {
	return c.resolve(task, 0)
}

func (c *Client) resolve(task Task, depth int) settings {
	stored := func(name string) (any, bool) {
		value := c.Config.Stored(task.Key(name))
		switch value := value.(type) {
		case nil:
			return nil, false
		case string:
			return value, strings.TrimSpace(value) != ""
		case []any:
			return value, len(value) > 0
		case map[string]any:
			return value, len(value) > 0
		}
		return value, cast.ToInt(value) != 0
	}
	base, inherits := Task{}, false
	if task.Inherit != "" && depth < 8 {
		base, inherits = c.Tasks.Task(task.Inherit)
	}
	var inherited settings
	if inherits {
		inherited = c.resolve(base, depth+1)
	}
	provider, _ := stored(auxiliaryconfig.Provider)
	_, pinnedModel := stored(auxiliaryconfig.Model)
	_, pinnedBase := stored(auxiliaryconfig.BaseURL)
	pinned := pinnedModel || pinnedBase || provider != nil && cast.ToString(provider) != "" && cast.ToString(provider) != auxiliaryconfig.Auto
	// Own value when set, else the inherited one, else the task's default
	text := func(name, from string) string {
		if value, ok := stored(name); ok {
			return strings.TrimSpace(cast.ToString(value))
		}
		if inherits && !(pinned && slices.Contains(routeKeys, name)) {
			return from
		}
		return strings.TrimSpace(c.Config.GetString(task.Key(name)))
	}
	s := settings{
		provider: text(auxiliaryconfig.Provider, inherited.provider),
		model:    text(auxiliaryconfig.Model, inherited.model),
		baseURL:  text(auxiliaryconfig.BaseURL, inherited.baseURL),
		apiKey:   text(auxiliaryconfig.APIKey, inherited.apiKey),
		keyEnv:   text(auxiliaryconfig.KeyEnv, inherited.keyEnv),
		apiMode:  text(auxiliaryconfig.APIMode, inherited.apiMode),
		effort:   strings.ToLower(text(auxiliaryconfig.ReasoningEffort, inherited.effort)),
		timeout:  c.Config.GetInt(task.Key(auxiliaryconfig.Timeout)),
		stall:    c.Config.GetInt(task.Key(auxiliaryconfig.NoProgressTimeout)),
		extra:    cast.ToStringMap(c.Config.Get(task.Key(auxiliaryconfig.ExtraBody))),
		chain:    c.Config.GetStringSlice(task.Key(auxiliaryconfig.FallbackChain)),
	}
	if inherits {
		if _, ok := stored(auxiliaryconfig.Timeout); !ok {
			s.timeout = inherited.timeout
		}
		if _, ok := stored(auxiliaryconfig.NoProgressTimeout); !ok {
			s.stall = inherited.stall
		}
		if _, ok := stored(auxiliaryconfig.ExtraBody); !ok {
			s.extra = inherited.extra
		}
		if _, ok := stored(auxiliaryconfig.FallbackChain); !ok {
			s.chain = inherited.chain
		}
	}
	if s.timeout <= 0 {
		s.timeout = cmp.Or(task.Timeout, 30)
	}
	if s.model == auxiliaryconfig.Auto {
		s.model = ""
	}
	return s
}

// Key the task's endpoint is called with: the one kept, else the one in its variable of the profile's environment
func (c *Client) apiKey(s settings) string {
	if s.apiKey != "" || s.keyEnv == "" {
		return s.apiKey
	}
	return strings.TrimSpace(c.Config.Getenv(s.keyEnv))
}

// Whether the task names its own route rather than leaving the choice to icebath
func (s settings) explicit() bool {
	return s.provider != "" && s.provider != auxiliaryconfig.Auto || s.model != "" || s.baseURL != ""
}
