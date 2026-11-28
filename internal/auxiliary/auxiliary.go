package auxiliary

import (
	"cmp"
	"slices"
	"strings"
	"time"

	auxiliaryconfig "github.com/nickheyer/icebath/internal/auxiliary/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/provider"
	providerconfig "github.com/nickheyer/icebath/internal/provider/config"
	"github.com/nickheyer/icebath/pkg/strs"
)

// Side job a feature runs on a model of its own rather than the session's, such as describing an image or titling a session. Its settings
// are kept under auxiliary and its name: the provider, model and endpoint it runs on, how long it may take, how hard the model thinks, and
// where it falls back to
type Task struct {
	Name  string
	Title string
	About string
	// Place of its section among the helper models, lowest first; one of no place after the rest
	Order int
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
	cfg.Guard(auxiliaryconfig.Key, config.Guard{Write: writeExtra})
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
			Order: cmp.Or(task.Order, unplaced), Settings: s.settings(values, task)})
	}
	return out
}

// Place of the section of a task that names none, after those that do
const unplaced = 1000

// Stall limit of a call when its task sets none
const unsetStall = time.Minute

// Settings of the task, each keyed below it: its own everyday ones, the provider and model every task keeps, then those every task keeps
// behind Advanced, then its own behind Advanced
func (s sections) settings(values *config.Config, task Task) []config.Setting {
	choice, timeout := auxiliaryconfig.Auto, cmp.Or(task.Timeout, 30)
	first := config.Option{Value: auxiliaryconfig.Auto, Title: "Automatic", About: "The session's model first, then the fallback models"}
	effort := config.Option{Title: "The model's default"}
	format := config.Option{Title: "From the address", About: "Told from the address of the server"}
	// What each setting left empty comes to
	model, address, limit, stalls, follows := "Automatic", "The provider's address", config.Span(time.Duration(timeout)*time.Second), config.Span(unsetStall), ""
	if task.Inherit != "" {
		base, _ := s.tasks.Task(task.Inherit)
		name := cmp.Or(base.Title, task.Inherit)
		choice, timeout, follows = "", 0, as(name)
		first = config.Option{Title: "Same as " + name, About: "What " + name + " uses, for each setting left empty"}
		effort, format = config.Option{Title: "Same as " + name}, config.Option{Title: "Same as " + name}
		model, address, limit, stalls = follows, follows, follows, follows
	}
	help := ""
	if task.MinTimeout > 0 {
		help = "A call always gets at least " + config.Span(time.Duration(task.MinTimeout)*time.Second) + "."
	}
	options := s.providers.Options(values)
	efforts := provider.EffortOptions(effort)
	common := []config.Setting{
		{Key: auxiliaryconfig.Provider, Title: "Provider", About: "Which provider runs this job.", Default: choice, Picks: providerconfig.PickProvider,
			Options: func() []config.Option {
				return append([]config.Option{first,
					{Value: auxiliaryconfig.Main, Title: "The session's provider", About: "The provider of the session the job is for"},
					{Value: auxiliaryconfig.Custom, Title: "Your own server", About: "The server at the address, under Advanced"}}, options()...)
			}},
		{Key: auxiliaryconfig.Model, Title: "Model", About: "The model that does this job.", Default: "", Zero: model, Picks: providerconfig.PickModel},
		{Key: auxiliaryconfig.BaseURL, Title: "Address", About: "The address of a server of your own that does this job.",
			Help: "When set, the job runs on this server whatever the model says, such as http://localhost:8080/v1.", Default: "", Zero: address,
			Starts: []string{"http://", "https://"}, Advanced: true},
		{Key: auxiliaryconfig.APIKey, Title: "API key", About: "Your API key for the server at the address.", Help: "Left empty, the account of the provider is used.",
			Default: "", Secret: true, Advanced: true},
		{Key: auxiliaryconfig.KeyEnv, Title: "Key variable", About: "The environment variable that holds the API key.",
			Help:    "Read when no API key is kept here, such as MY_SERVER_KEY.",
			Default: "", Zero: follows, Advanced: true},
		{Key: auxiliaryconfig.APIMode, Title: "Request format", About: "Which API the server at the address speaks.", Default: "", Advanced: true,
			Options: config.Options(format,
				config.Option{Value: string(provider.ModeChatCompletions), Title: "OpenAI chat completions"},
				config.Option{Value: string(provider.ModeAnthropicMessages), Title: "Anthropic messages"},
				config.Option{Value: string(provider.ModeCodexResponses), Title: "OpenAI responses"},
			)},
		{Key: auxiliaryconfig.Timeout, Title: "Time limit", About: "How long a call may wait for the first output of the model.", Help: help,
			Default: time.Duration(timeout) * time.Second, Zero: limit, Advanced: true},
		{Key: auxiliaryconfig.NoProgressTimeout, Title: "Stall limit", About: "How long a reply may pause once it has started.", Default: time.Duration(0),
			Zero: stalls, Advanced: true},
		{Key: auxiliaryconfig.ReasoningEffort, Title: "Reasoning effort", About: "How much the model thinks before it answers.", Default: task.Effort,
			Options: func() []config.Option { return efforts }, Picks: auxiliaryconfig.PickEffort, Advanced: true},
		{Key: auxiliaryconfig.ExtraBody, Title: "Extra request fields", About: "Fields added as they are to every request of this job.",
			Default: map[string]any{}, Zero: follows, Entry: &config.Entry{Noun: "field", Name: "Field name",
				Fields: []config.Setting{auxiliaryconfig.RequestField}}, Advanced: true},
		{Key: auxiliaryconfig.FallbackChain, Title: "Fallback models", About: "Models to try in order when this job fails.", Default: []string{},
			Zero: cmp.Or(follows, "None"), Picks: providerconfig.PickModel, Ordered: true, Entry: &config.Entry{Noun: "model"},
			Advanced: true},
	}
	var everyday, advanced []config.Setting
	for _, setting := range task.Settings {
		if setting.Advanced {
			advanced = append(advanced, setting)
		} else {
			everyday = append(everyday, setting)
		}
	}
	settings := slices.Concat(everyday, common, advanced)
	for i := range settings {
		settings[i].Key = task.Key(settings[i].Key)
	}
	return settings
}

// Extra request fields of a task, the whole set, one field or a value inside one, kept with each value read as it is sent
func writeExtra(cfg *config.Config, key string, value any) (bool, error) {
	parts := strings.Split(key, ".")
	if len(parts) < 3 || parts[2] != auxiliaryconfig.ExtraBody {
		return false, nil
	}
	return auxiliaryconfig.WriteFields(strings.Join(parts[:3], "."))(cfg, key, value)
}

// Words for what a task takes from the task it follows, such as As image analysis: the title of that task, its first letter lower case
// unless it begins a name in capitals
func as(title string) string {
	return "As " + strs.Lower(title)
}
