package auxiliary

import (
	"cmp"
	"context"
	"errors"
	"fmt"
	"net/url"
	"slices"
	"strings"
	"time"

	auxiliaryconfig "github.com/nickheyer/icebath/internal/auxiliary/config"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/credentials"
	"github.com/nickheyer/icebath/internal/models"
	"github.com/nickheyer/icebath/internal/provider"
	"github.com/nickheyer/icebath/internal/transport"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
)

const (
	// OpenRouter model a task falls back to when none is set
	freeModel = "nvidia/nemotron-3-ultra-550b-a55b:free"
	// Model of a custom endpoint when nothing names one
	endpointModel = "gpt-4o-mini"
	// How long a provider out of credit is skipped, and a fallback that failed for want of capacity
	outageTTL     = 10 * time.Minute
	quarantineTTL = time.Minute
)

// Calls of tasks for one session or job, on the models of its profile. The agent makes one for each turn, following the session's main model
type Client struct {
	Tasks       *Registry
	Config      *config.Config
	Providers   *provider.Registry
	Transports  *transport.Registry
	Credentials *credentials.Registry
	State       *State
	// Main model of the session, where auto and main start; no model when the session has none it can use
	Main provider.Route
	// Whether the user chose the main provider, rather than icebath taking the first one connected
	Chosen bool
	// Whether the main model sees images, as a setting says; found out from the model when nil
	MainSees *bool
	// How images reach the main model: auto, native or text
	ImageInput string
	// Routes of the main model's fallback chain, tried after a task's own chain when icebath picks the task's route
	Fallbacks []provider.Route
	// Told of the tokens each call that answered took, by the task it ran for and the route it ran on; nil when nobody counts them
	Used func(task string, route provider.Route, usage provider.Usage)
}

// One call of a task
type Request struct {
	Task     string
	Messages []*sessionsv1.Message
	// Model over the task's own
	Model     string
	MaxTokens int
	// Sampling temperature, left out for models that refuse one
	Temperature *float64
	// Shape the reply must take, dropped where a route refuses it
	Format *provider.Format
	// Time a call may go without output, over the task's timeout
	Timeout time.Duration
	// Request fields over the task's own extra fields
	Extra map[string]any
	// Reasoning effort over the task's own, such as none to keep a model writing a title from thinking first; the task's when nil
	Reasoning *string
	// Runs on the session's main model alone, whatever the task's settings say, for a task whose own model failed it
	Main bool
	// Runs on this provider:model of the profile alone, whatever the task's settings say, such as an entry of the task's fallback chain
	Entry string
	// Told each time the reply streams more, for a caller that gives up a call that stalls
	Progress func()
}

// What a call came back with
type Answer struct {
	Text string
	// Why the model stopped, such as length for a reply cut off at its output cap
	Finish string
	// Route that answered
	Route provider.Route
	// Route that failed before another answered, and why; no model when none did
	Failed  provider.Route
	Failure string
}

// Messages of a call made of a system prompt, a prompt and the images or videos it is about
func Prompt(system, prompt string, media ...*sessionsv1.Media) []*sessionsv1.Message {
	var messages []*sessionsv1.Message
	if system != "" {
		messages = append(messages, &sessionsv1.Message{Role: sessionsv1.Role_ROLE_SYSTEM, Text: system})
	}
	return append(messages, &sessionsv1.Message{Role: sessionsv1.Role_ROLE_USER, Text: prompt, Media: media})
}

// Route a call can take
type candidate struct {
	route provider.Route
}

func (c candidate) key() string {
	if c.route.Spec == auxiliaryconfig.Custom {
		return auxiliaryconfig.Custom + ":" + strings.TrimRight(c.route.BaseURL, "/")
	}
	return c.route.Provider
}

func (c candidate) same(other candidate) bool {
	return c.key() == other.key() && c.route.Model == other.route.Model && c.route.BaseURL == other.route.BaseURL
}

// Text the task's model answers the request with. A call that fails is retried where that can help, then falls back along the task's routes,
// and the first failure is the one given when every route fails
func (c *Client) Complete(ctx context.Context, request Request) (string, error) {
	answer, err := c.Ask(ctx, request)
	return answer.Text, err
}

// Answer of the task's model to the request, with the route that gave it and the one that failed before it. A call is retried and falls
// back as Complete's does; one pinned to the main model or to an entry runs there alone
func (c *Client) Ask(ctx context.Context, request Request) (Answer, error) {
	// Every attempt of the call, on every route, is one request to the auxiliary hooks
	ctx = withAuxCall(ctx)
	task := c.task(request.Task)
	s := c.settings(task)
	if request.Main || request.Entry != "" {
		route, err := c.pinned(request)
		if err != nil {
			return Answer{}, err
		}
		return c.attempt(ctx, task, s, candidate{route: route}, request)
	}
	routes, err := c.routes(ctx, task, s, request.Model)
	if err != nil {
		return Answer{}, err
	}
	primary := routes[0]
	answer, err := c.attempt(ctx, task, s, primary, request)
	if err == nil || ctx.Err() != nil {
		return answer, err
	}
	reason := fallbackReason(err)
	explicit := s.explicit()
	if reason == "" || explicit && reason == reasonAuth && len(s.chain) == 0 {
		return Answer{}, err
	}
	if reason == reasonPayment {
		c.State.mark(c.Config.Profile()+"/"+primary.key(), "payment / credit error", outageTTL)
	}
	tried := []candidate{primary}
	for _, next := range append(routes[1:], c.fallbacks(ctx, task, s, explicit, request.Model)...) {
		if slices.ContainsFunc(tried, next.same) || c.down(next) {
			continue
		}
		tried = append(tried, next)
		answer, failure := c.attempt(ctx, task, s, next, request)
		if failure == nil || ctx.Err() != nil {
			answer.Failed, answer.Failure = primary.route, err.Error()
			return answer, failure
		}
		why := fallbackReason(failure)
		if why == "" {
			return Answer{}, failure
		}
		ttl := outageTTL
		if why == reasonRateLimit || why == reasonConnection || why == reasonInvalid {
			ttl = quarantineTTL
		}
		c.State.mark(c.Config.Profile()+"/"+next.key(), why, ttl)
	}
	return Answer{}, err
}

// Route a pinned request runs on: the session's main model, or the entry it names
func (c *Client) pinned(request Request) (provider.Route, error) {
	if request.Entry != "" {
		route, ok := c.entry(request.Entry, c.task(request.Task).Vision)
		if !ok {
			return route, fmt.Errorf("%s is not a provider of the profile %s with one of its models. Write it as provider:model.", request.Entry,
				c.Config.Profile())
		}
		return route, nil
	}
	main, ok := c.main(request.Model, false)
	if !ok {
		return provider.Route{}, errors.New("The session has no main model to run the task on.")
	}
	return main.route, nil
}

// Route a call of the task goes to first
func (c *Client) Route(ctx context.Context, name string) (provider.Route, error) {
	task := c.task(name)
	routes, err := c.routes(ctx, task, c.settings(task), "")
	if err != nil {
		return provider.Route{}, err
	}
	return routes[0].route, nil
}

// Model the provider named runs side jobs on when a task names none, empty when it has none
func (c *Client) Helper(name string) string {
	name = normalized(name)
	if instance, ok := c.Providers.Instance(c.Config, name); ok {
		account, _ := c.Credentials.Route(instance, "")
		return c.helper(instance.Spec, account, false)
	}
	spec, ok := c.Providers.Spec(name)
	if !ok {
		return ""
	}
	credential, _ := c.Credentials.Detect(c.Config, spec)
	return c.helper(spec, provider.Route{Provider: name, BaseURL: cmp.Or(credential.BaseURL, spec.BaseURL), APIKey: credential.APIKey}, false)
}

// Route to the endpoint at the address for the task, with the key and wire its settings give: through the provider named when the profile
// has it, else as a custom endpoint
func (c *Client) Endpoint(task, name, model, base string) (provider.Route, error) {
	s := c.settings(c.task(task))
	s.baseURL = base
	return c.endpoint(normalized(name), model, s)
}

// Entries of the task's own fallback chain, each provider:model, in the order they are tried
func (c *Client) Chain(name string) []string {
	return c.settings(c.task(name)).chain
}

// Whether a call of the task has a model to go to
func (c *Client) Resolves(ctx context.Context, name string) bool {
	task := c.task(name)
	_, err := c.routes(ctx, task, c.settings(task), "")
	return err == nil
}

func (c *Client) task(name string) Task {
	if task, ok := c.Tasks.Task(name); ok {
		return task
	}
	return Task{Name: name}
}

func (c *Client) down(next candidate) bool {
	_, _, out := c.State.out(c.Config.Profile() + "/" + next.key())
	return out
}

// Routes of the task in the order they are tried, the first the one its settings name
func (c *Client) routes(ctx context.Context, task Task, s settings, model string) ([]candidate, error) {
	model = cmp.Or(model, s.model)
	name := normalized(s.provider)
	if name == auxiliaryconfig.Main {
		name = cmp.Or(c.Main.Provider, auxiliaryconfig.Custom)
	}
	switch {
	case s.baseURL != "":
		route, err := c.endpoint(name, model, s)
		if err != nil {
			return nil, err
		}
		return []candidate{{route: route}}, nil
	case name == auxiliaryconfig.Auto:
		routes := c.auto(ctx, task, model)
		if len(routes) == 0 {
			routes = c.fallbacks(ctx, task, s, false, model)
		}
		if len(routes) > 0 {
			return routes, nil
		}
	default:
		route, err := c.named(name, model, task.Vision, true)
		if err == nil && c.apiKey(s) != "" {
			route.APIKey = c.apiKey(s)
		}
		if err == nil {
			return []candidate{{route: route}}, nil
		}
		// A vision provider that can't be reached gives way to those that can; OpenRouter and custom to whatever is connected
		if task.Vision || name == "openrouter" || name == auxiliaryconfig.Custom {
			routes := c.auto(ctx, task, s.model)
			if len(routes) == 0 {
				routes = c.fallbacks(ctx, task, s, false, s.model)
			}
			if len(routes) > 0 {
				return routes, nil
			}
		}
		var routes []candidate
		for _, entry := range s.chain {
			if route, ok := c.entry(entry, task.Vision); ok {
				routes = append(routes, candidate{route: route})
			}
		}
		if len(routes) > 0 {
			return routes, nil
		}
		return nil, err
	}
	return nil, &NoProvider{Job: cmp.Or(task.Title, task.Name)}
}

// Routes when icebath picks: the session's main model, then for a vision task the providers that see images
func (c *Client) auto(ctx context.Context, task Task, model string) []candidate {
	var routes []candidate
	if main, ok := c.main(model, task.Vision); ok && !c.down(main) {
		if spec, _ := c.Providers.Spec(main.route.Spec); !task.Vision || !spec.Blind && c.mayAnswer(ctx, main.route) {
			routes = append(routes, main)
		}
	}
	if task.Vision {
		for _, kind := range []string{"openrouter", "deepinfra"} {
			if route, err := c.named(kind, model, true, false); err == nil && !c.down(candidate{route: route}) {
				routes = append(routes, candidate{route: route})
			}
		}
	}
	return routes
}

// Whether the main model may see images: yes unless it is known not to
func (c *Client) mayAnswer(ctx context.Context, route provider.Route) bool {
	sees, known := c.Sees(ctx, route)
	return sees || !known
}

// The session's main model, with the task's model in its place when it names one. A vision task takes the vision model of the main kind
func (c *Client) main(model string, vision bool) (candidate, bool) {
	route := c.Main
	if route.Model == "" {
		return candidate{}, false
	}
	if spec, _ := c.Providers.Spec(route.Spec); vision {
		model = cmp.Or(model, spec.VisionHelper)
	}
	// A vendor/model name only means something to an aggregator
	if model != "" && !(strings.Contains(model, "/") && route.Spec != "openrouter" && !c.aggregator(route.Spec)) {
		route.Model = model
		if spec, ok := c.Providers.Spec(route.Spec); ok && spec.PerModelMode != nil {
			route.Mode = cmp.Or(spec.PerModelMode(model), route.Mode)
		}
	}
	return candidate{route: route}, true
}

func (c *Client) aggregator(kind string) bool {
	spec, ok := c.Providers.Spec(kind)
	return ok && spec.Aggregator
}

// Routes a failed call goes on to: the task's fallback chain, then for a route icebath picked any connected provider while the user chose
// none, and for a route the task names the session's main model
func (c *Client) fallbacks(ctx context.Context, task Task, s settings, explicit bool, model string) []candidate {
	var routes []candidate
	for _, entry := range s.chain {
		if route, ok := c.entry(entry, task.Vision); ok {
			routes = append(routes, candidate{route: route})
		}
	}
	if explicit {
		if main, ok := c.main("", false); ok && (!task.Vision || c.mayAnswer(ctx, main.route)) {
			routes = append(routes, main)
		}
		return routes
	}
	for _, route := range c.Fallbacks {
		if !task.Vision || c.mayAnswer(ctx, route) {
			routes = append(routes, candidate{route: route})
		}
	}
	if c.Chosen && c.Main.Model != "" {
		return routes
	}
	return append(routes, c.discover(model, task.Vision)...)
}

// Connected providers a task can fall back to when the user chose no provider: OpenRouter first, then servers on this machine, then the first
// provider with a model for side jobs
func (c *Client) discover(model string, vision bool) []candidate {
	var routes []candidate
	add := func(name, model string) {
		if route, err := c.named(name, model, vision, false); err == nil {
			routes = append(routes, candidate{route: route})
		}
	}
	add("openrouter", model)
	instances := c.Providers.Instances(c.Config)
	for _, instance := range instances {
		if instance.Spec.Local && instance.Known {
			add(instance.ID, cmp.Or(model, instance.Spec.DefaultModel))
		}
	}
	if base := strings.TrimSpace(c.Config.Getenv("OPENAI_BASE_URL")); base != "" {
		if route, err := c.endpoint(auxiliaryconfig.Custom, model, settings{baseURL: base}); err == nil {
			routes = append(routes, candidate{route: route})
		}
	}
	for _, instance := range instances {
		if instance.Known && instance.Spec.Keyed() && instance.Spec.DefaultAuxModel != "" {
			if _, ok := c.Credentials.Credential(credentials.Of(instance, "")); ok {
				add(instance.ID, model)
				break
			}
		}
	}
	return routes
}

// Route of a fallback chain entry, provider:model with the provider named as in the profile
func (c *Client) entry(entry string, vision bool) (provider.Route, bool) {
	name, model, ok := strings.Cut(strings.TrimSpace(entry), ":")
	if !ok || strings.TrimSpace(model) == "" {
		return provider.Route{}, false
	}
	route, err := c.named(strings.TrimSpace(name), strings.TrimSpace(model), vision, false)
	return route, err == nil
}

// Route to the provider named: one of the profile's, else a kind connected through the environment. With no model the kind's own model for
// side jobs is taken, then when allowed the session's
func (c *Client) named(name, model string, vision, mainModel bool) (provider.Route, error) {
	name = normalized(name)
	if instance, ok := c.Providers.Instance(c.Config, name); ok {
		if model == "" {
			account, _ := c.Credentials.Route(instance, "")
			model = c.helper(instance.Spec, account, vision)
		}
		if model == "" && mainModel {
			model = c.Main.Model
		}
		if model == "" {
			return provider.Route{}, fmt.Errorf("No model is set for %s. Pick one in /settings under Helper models.", instance.Label())
		}
		if err := c.free(instance.Spec, model); err != nil {
			return provider.Route{}, err
		}
		route, err := c.Credentials.Route(instance, model)
		if err != nil {
			return route, missing(instance.ID, instance.Spec)
		}
		return route, nil
	}
	if name == "openai" {
		return c.endpoint(auxiliaryconfig.Custom, model, settings{baseURL: cmp.Or(strings.TrimSpace(c.Config.Getenv("OPENAI_BASE_URL")), "https://api.openai.com/v1")})
	}
	spec, ok := c.Providers.Spec(name)
	if !ok || spec.Hidden {
		return provider.Route{}, fmt.Errorf("The profile %s has no provider named %s. Add it in /providers.", c.Config.Profile(), name)
	}
	credential, found := c.Credentials.Detect(c.Config, spec)
	if !found && !spec.Local && spec.Auth != provider.AuthNone {
		return provider.Route{}, missing(name, spec)
	}
	model = cmp.Or(model, c.helper(spec, provider.Route{Provider: name, BaseURL: cmp.Or(credential.BaseURL, spec.BaseURL), APIKey: credential.APIKey}, vision))
	if model == "" && mainModel {
		model = c.Main.Model
	}
	if model == "" {
		return provider.Route{}, fmt.Errorf("No model is set for %s. Pick one in /settings under Helper models.", spec.Label())
	}
	if err := c.free(spec, model); err != nil {
		return provider.Route{}, err
	}
	route := provider.Route{Provider: spec.Name, Spec: spec.Name, Model: model, BaseURL: cmp.Or(credential.BaseURL, spec.BaseURL), APIKey: credential.APIKey,
		Headers: spec.Headers, Mode: spec.Mode, Auth: cmp.Or(credential.Auth, spec.Auth), Local: spec.Local}
	if spec.PerModelMode != nil {
		route.Mode = cmp.Or(spec.PerModelMode(model), route.Mode)
	}
	if route.BaseURL == "" {
		return route, fmt.Errorf("%s has no address set. Set its Base URL in /providers.", spec.Label())
	}
	return route, nil
}

// Model a kind runs side jobs on when the task names none: for one on images, the one its plugin names that sees them, on the route its
// account takes
func (c *Client) helper(spec provider.Spec, route provider.Route, vision bool) string {
	if vision {
		if spec.VisionHelper != "" {
			return spec.VisionHelper
		}
		if catalog := c.Tasks.catalog(); catalog != nil {
			route.Spec = spec.Name
			if model, named := catalog.VisionModel(context.Background(), c.Config, route); named {
				return model
			}
		}
	}
	if spec.Name == "openrouter" {
		return cmp.Or(strings.TrimSpace(c.Config.GetString(auxiliaryconfig.OpenRouterModel)), freeModel)
	}
	return spec.DefaultAuxModel
}

// Refusal of a paid OpenRouter model while side jobs may only use free ones
func (c *Client) free(spec provider.Spec, model string) error {
	if spec.Name != "openrouter" || !c.Config.GetBool(auxiliaryconfig.FreeOnly) || strings.HasSuffix(model, ":free") || strings.HasPrefix(model, "stealth/") {
		return nil
	}
	return fmt.Errorf("Free models only is on, but the OpenRouter model %s is not free, so OpenRouter is skipped. Set OpenRouter model in "+
		"/settings under Helper models to a free one, such as %s, or turn off Free models only.", model, freeModel)
}

// Error of a provider the task names that has no credential
func missing(name string, spec provider.Spec) error {
	switch {
	case spec.Auth == provider.AuthOAuth:
		return fmt.Errorf("The provider %s is set for this helper job, but it has no account. Sign in with /providers, or pick another provider "+
			"for the job in /settings under Helper models.", name)
	case len(spec.KeyEnvNames()) > 0:
		return fmt.Errorf("The provider %s is set for this helper job, but it has no API key. Set %s or add a key with /providers, or pick "+
			"another provider for the job in /settings under Helper models.", name, spec.KeyEnvNames()[0])
	}
	return fmt.Errorf("The provider %s is set for this helper job, but it has no account. Connect it with /providers, or pick another provider "+
		"for the job in /settings under Helper models.", name)
}

// Route to the endpoint the task's base URL names, through the provider of the profile it names when there is one
func (c *Client) endpoint(name, model string, s settings) (provider.Route, error) {
	if instance, ok := c.Providers.Instance(c.Config, name); ok && name != auxiliaryconfig.Custom {
		route, err := c.Credentials.Route(instance, cmp.Or(model, c.helper(instance.Spec, provider.Route{}, false), c.Main.Model))
		if err != nil && route.Model == "" {
			return route, err
		}
		route.BaseURL = s.baseURL
		route.APIKey = cmp.Or(c.apiKey(s), route.APIKey)
		route.Mode = cmp.Or(provider.APIMode(s.apiMode), route.Mode)
		return route, nil
	}
	model = cmp.Or(model, c.Main.Model, endpointModel)
	mode := cmp.Or(provider.APIMode(s.apiMode), wire(s.baseURL, model))
	base := s.baseURL
	if mode == provider.ModeChatCompletions {
		base = openAIBase(base)
	}
	key := c.apiKey(s)
	switch {
	case key != "":
	case models.Local(base):
	case c.Config.Getenv("OPENAI_API_KEY") != "":
		key = strings.TrimSpace(c.Config.Getenv("OPENAI_API_KEY"))
	case origin(c.Main.BaseURL) == origin(base):
		key = c.Main.APIKey
	}
	return provider.Route{Provider: auxiliaryconfig.Custom, Spec: auxiliaryconfig.Custom, Model: model, BaseURL: base, APIKey: key, Mode: mode, Auth: provider.AuthAPIKey, Local: models.Local(base)}, nil
}

// API an endpoint speaks, told from its address
func wire(base, model string) provider.APIMode {
	host, path := provider.BaseHost(base), provider.BasePath(base)
	switch {
	case strings.HasSuffix(path, "/anthropic") || strings.HasSuffix(path, "/anthropic/v1") || host == "api.anthropic.com" ||
		host == "api.kimi.com" && strings.HasPrefix(path, "/coding"):
		return provider.ModeAnthropicMessages
	case host == "api.openai.com" && strings.Contains(strings.ToLower(model), "codex"):
		return provider.ModeCodexResponses
	}
	return provider.ModeChatCompletions
}

// Address of the OpenAI style API of an endpoint given by another of its addresses
func openAIBase(base string) string {
	host, path := provider.BaseHost(base), provider.BasePath(base)
	trimmed := strings.TrimRight(base, "/")
	switch {
	case (host == "api.z.ai" || host == "open.bigmodel.cn") && strings.HasSuffix(path, "/anthropic"):
		return strings.TrimSuffix(trimmed, "/anthropic") + "/coding/paas/v4"
	case strings.Contains(host, "minimax") && path != "/v1":
		return strings.TrimSuffix(trimmed, path) + "/v1"
	case host == "api.kimi.com" && path == "/coding":
		return trimmed + "/v1"
	case models.Local(base) && path == "":
		return trimmed + "/v1"
	}
	return base
}

// Scheme, host and port of an address
func origin(base string) string {
	parsed, err := url.Parse(strings.TrimSpace(base))
	if err != nil || parsed.Host == "" {
		return ""
	}
	return strings.ToLower(parsed.Scheme + "://" + parsed.Host)
}

// Provider name as a setting may spell it
func normalized(name string) string {
	name = strings.ToLower(strings.TrimSpace(name))
	switch {
	case name == "":
		return auxiliaryconfig.Auto
	case name == "codex":
		return "openai-codex"
	}
	if rest, ok := strings.CutPrefix(name, "custom:"); ok {
		return cmp.Or(rest, auxiliaryconfig.Custom)
	}
	return name
}

// Refusal of a helper job no provider can do
type NoProvider struct {
	// Title of the job
	Job string
}

func (e *NoProvider) Error() string {
	job := "this helper job"
	if e.Job != "" {
		job = "the helper job " + e.Job
	}
	return "No provider can do " + job + ". Connect one with /providers, or set one for the job in /settings under Helper models."
}
