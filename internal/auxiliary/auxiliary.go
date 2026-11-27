package auxiliary

import (
	"cmp"
	"strconv"
	"time"

	auxiliaryconfig "github.com/nickheyer/icebath/internal/auxiliary/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/provider"
)

// Side job a feature runs on a model of its own rather than the session's, such as describing an image or titling a session. Its settings
// are kept under auxiliary and its name: the provider, model and endpoint it runs on, how long it may take, how hard the model thinks, and
// where it falls back to
type Task struct {
	Name  string
	Title string
	About string
	// Seconds a call may go without output when the task's timeout is not set, 30 when none is given
	Timeout int
	// Least seconds a call gets whatever the timeout says, unless the caller names its own
	MinTimeout int
	// Task whose settings this one takes for those it leaves unset. Naming its own provider, model or endpoint drops every route setting of
	// that task
	Inherit string
	// Needs a model that sees images
	Vision bool
	// On the critical path: a call that used its whole time falls back at once rather than trying the same provider again
	Critical bool
	// Reasoning effort the task runs at when its setting is unset, such as none for a task that must answer fast; empty for the model's own
	Effort string
	// More settings the task keeps, keyed by their names below the task's own
	Settings []config.Setting
}

// Key of one of the task's settings
func (t Task) Key(name string) string {
	return auxiliaryconfig.Key + "." + t.Name + "." + name
}

type Registrant interface {
	Tasks() []Task
}

type Registry struct {
	registrants []Registrant
	// Host the registry is part of
	host *Auxiliary
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Tasks() []Task {
	var tasks []Task
	for _, registrant := range r.registrants {
		tasks = append(tasks, registrant.Tasks()...)
	}
	return tasks
}

// Task registered under the name
func (r *Registry) Task(name string) (Task, bool) {
	for _, task := range r.Tasks() {
		if task.Name == name {
			return task, true
		}
	}
	return Task{}, false
}

type Auxiliary struct {
	Registry
	// What is known of models, such as which see images and which a kind runs side jobs on images with
	models *models.Models
	// Hook points that hear of each attempt of a call
	hooks *hooks.Hooks
}

func New(cfg *config.Config, providers *provider.Registry, catalog *models.Models, points *hooks.Hooks) *Auxiliary {
	a := &Auxiliary{models: catalog, hooks: points}
	a.Registry.host = a
	auxiliaryconfig.Register(cfg, sections{tasks: &a.Registry, providers: providers})
	return a
}

// Adds a registrant; a registry nested in the host reaches it through its tasks
func (a *Auxiliary) Register(registrant Registrant) {
	if nested, ok := registrant.(*Registry); ok {
		nested.host = a
	}
	a.Registry.Register(registrant)
}

// What is known of models, nil for tasks of no host
func (r *Registry) catalog() *models.Models {
	if r == nil || r.host == nil {
		return nil
	}
	return r.host.models
}

// Hook points of the host, nil for tasks of no host
func (r *Registry) hooking() *hooks.Hooks {
	if r == nil || r.host == nil {
		return nil
	}
	return r.host.hooks
}

// One settings section for each task registered, under the helper models
type sections struct {
	tasks     *Registry
	providers *provider.Registry
}

func (s sections) Sections(values *config.Config) []config.Section {
	var out []config.Section
	for _, task := range s.tasks.Tasks() {
		out = append(out, config.Section{Key: auxiliaryconfig.Key + "." + task.Name, Parent: auxiliaryconfig.Key, Title: task.Title, About: task.About,
			Settings: s.settings(values, task)})
	}
	return out
}

// Settings every task keeps, then the task's own, each keyed below the task
func (s sections) settings(values *config.Config, task Task) []config.Setting {
	title := task.Title
	choice, timeout, timeoutAbout := auxiliaryconfig.Auto, cmp.Or(task.Timeout, 30), "Seconds a call may go without output"
	first := config.Option{Value: auxiliaryconfig.Auto, Title: "Auto", About: "The session's model first, then the fallbacks"}
	if task.Inherit != "" {
		base, _ := s.tasks.Task(task.Inherit)
		choice, timeout, timeoutAbout = "", 0, "Seconds a call may go without output; 0 uses those of "+cmp.Or(base.Title, task.Inherit)
		first = config.Option{Title: "Same as " + cmp.Or(base.Title, task.Inherit)}
	}
	if task.MinTimeout > 0 {
		timeoutAbout += ", at least " + strconv.Itoa(task.MinTimeout)
	}
	options := s.providers.Options(values)
	efforts := []config.Option{{Title: "Model default"}}
	for _, effort := range provider.Efforts {
		efforts = append(efforts, config.Option{Value: effort, Title: effort})
	}
	settings := []config.Setting{
		{Key: auxiliaryconfig.Provider, Title: title + " provider", About: "Provider of the profile " + title + " runs on", Default: choice, Options: func() []config.Option {
			return append([]config.Option{first,
				{Value: auxiliaryconfig.Main, Title: "Main", About: "The session's provider"},
				{Value: auxiliaryconfig.Custom, Title: "Custom endpoint", About: "The base URL below"}}, options()...)
		}},
		{Key: auxiliaryconfig.Model, Title: title + " model", About: "Model of " + title + "; empty picks one for you", Default: ""},
		{Key: auxiliaryconfig.BaseURL, Title: title + " URL", About: "Endpoint of " + title + ", which then runs there with the key below", Default: ""},
		{Key: auxiliaryconfig.APIKey, Title: title + " key", About: "API key of the endpoint; empty uses the provider's account", Default: "", Secret: true},
		{Key: auxiliaryconfig.KeyEnv, Title: title + " key variable", About: "Environment variable the key is read from when none is kept", Default: ""},
		{Key: auxiliaryconfig.APIMode, Title: title + " API", About: "API the endpoint speaks; empty tells it from the URL", Default: "", Options: config.Options(
			config.Option{Title: "From the URL"},
			config.Option{Value: string(provider.ModeChatCompletions), Title: "Chat Completions"},
			config.Option{Value: string(provider.ModeAnthropicMessages), Title: "Anthropic Messages"},
			config.Option{Value: string(provider.ModeCodexResponses), Title: "Responses"},
		)},
		{Key: auxiliaryconfig.Timeout, Title: title + " timeout", About: timeoutAbout, Default: time.Duration(timeout) * time.Second},
		{Key: auxiliaryconfig.NoProgressTimeout, Title: title + " stall limit", About: "Seconds a reply may stop sending once it started; 0 for 60",
			Default: time.Duration(0)},
		{Key: auxiliaryconfig.ReasoningEffort, Title: title + " thinking", About: "How hard the model of " + title + " thinks", Default: task.Effort, Options: func() []config.Option { return efforts }},
		{Key: auxiliaryconfig.ExtraBody, Title: title + " fields", About: "Extra request fields of " + title, Default: map[string]any{}},
		{Key: auxiliaryconfig.FallbackChain, Title: title + " fallbacks", About: "Models tried in turn when " + title + " fails, each provider:model with the provider named as in this profile", Default: []string{}},
	}
	settings = append(settings, task.Settings...)
	for i := range settings {
		settings[i].Key = task.Key(settings[i].Key)
	}
	return settings
}
