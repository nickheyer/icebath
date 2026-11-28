package tools

import (
	"bytes"
	"cmp"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/nickheyer/icebath/internal/auxiliary"
	"github.com/nickheyer/icebath/internal/config"
	"github.com/nickheyer/icebath/internal/hooks"
	"github.com/nickheyer/icebath/internal/provider"
	toolsconfig "github.com/nickheyer/icebath/internal/tools/config"
	"github.com/nickheyer/icebath/internal/tools/redact"
	sessionsv1 "github.com/nickheyer/icebath/pkg/gen/icebath/sessions/v1"
	"github.com/nickheyer/icebath/pkg/strs"
)

type Handler func(ctx context.Context, call *Call) string

// Glyphs the app can mark a tool with
const (
	IconRead   = "read"
	IconWrite  = "write"
	IconEdit   = "edit"
	IconFind   = "find"
	IconSearch = "search"
	IconShell  = "shell"
	IconOutput = "output"
	IconKill   = "kill"
	IconFetch  = "fetch"
	IconAgent  = "agent"
	IconRecord = "record"
	IconPlan   = "plan"
	IconServer = "server"
	IconAsk    = "ask"
)

// What running a tool does to the world
type Effect int

const (
	// May change something; nothing is known
	MayChange Effect = iota
	// Only reads: a call stopped midway or run again changes nothing
	ReadOnly
	// Changes files, the machine or the session's state, so a call that succeeds is progress
	Mutates
)

// Tool a plugin adds: what the model sees, what runs, and how the app shows it
type Def struct {
	Name    string
	Toolset string
	// Name shown in the app, the tool name when empty
	Title string
	// What the tool lets the agent do, a phrase for a person with no period, at most 70 characters: "Read a file of the project".
	// The description in Schema is written for the model and is not shown in its place
	About string
	// Heading the app lists the tools of the toolset under, such as Files; one tool of a toolset saying it is enough
	Group string
	// Glyph the app marks the tool with, one of the Icon names, a generic one when empty
	Icon string
	// Argument whose value the app shows next to the title, such as "path"; the first text argument when empty
	Subject string
	// Kinds of risky action the tool asks approval for, each the reason it hands to Env.Approve, offered in the approvals allowlist
	Risks  []string
	Schema map[string]any
	// Schema for the session in place of Schema, such as one that depends on its main model; nil, or a nil result, keeps Schema
	Dynamic func(env *Env) map[string]any
	Handler Handler
	Check   func(env *Env) bool
	// What the tool lacks while Check says it can't run, in a person's words, such as "It needs a web search service that is ready."
	Lacks          func(env *Env) string
	MaxResultChars int
	// What a call does to the world: a read only call may be stopped or repeated freely, a mutating one that succeeds is progress
	Effect Effect
	// Whether a call may run at the same time as the other calls of its reply: tools that read and keep no state of the session
	Parallel bool
	// Paths a call reads or writes and whether it writes them, for a tool that may run alongside others on paths of its own: calls on
	// overlapping paths where one writes run in turn, and a call that names none runs alone
	Paths func(env *Env, args map[string]any) (paths []string, writes bool)
	// Failing is part of the tool's normal output, such as a command that exits non zero, so failures alone are no sign of a loop
	Fallible bool
	// What the tool returns comes from outside, such as a web page, so the model is handed it as data, never as instructions
	Untrusted bool
	// The tool is polled, so calling it again with the same arguments is no sign of being stuck
	Polls bool
	// Longest a call may take when longer than the agent's own limit, for a tool with a budget of its own; negative for a tool that
	// watches its own liveness and is given no limit. Such a call runs alone
	Timeout time.Duration
	// Left out of sessions in the coding focus posture, as having nothing to do with code, such as pictures, speech or scheduled jobs
	Unfocused bool
	// Most calls of a batch running at once while one of the tool's calls is in it, as a setting says, such as pictures drawn at the same
	// time a service's rate limits allow; nil leaves the batch at its own limit
	Workers func(env *Env) int
	// Profile whose sessions alone get the tool, such as one of a server the profile set up; every profile when empty
	Profile string
}

// Heading the app lists tools under, and where the options of its tools are: declared by the plugins whose tools or settings belong to it,
// the declarations of one title making one group
type Group struct {
	Title string
	// Keys of the settings sections that hold options of its tools: the options of the first are listed on its page, each other one is a
	// row there that opens its own
	Sections []string
	// Key of the area of the settings menu its options are, for a group whose options are an area of their own, and the argument the page of
	// that area is opened with
	Area, Opens string
	// Its tools are turned on and off on the page of its area, their one home, so the group lists none of them
	Elsewhere bool
	// Place among the groups, lowest first
	Order int
	// Profile the group is of, every profile when empty
	Profile string
}

// Registrant that declares the groups its tools are listed under
type Grouper interface {
	Groups() []Group
}

// Every group the registrants declare, declarations of one title and profile joined: the sections of the one that gives the group its place
// first, then the others in order, and the first area
func (r *Registry) Groups() []Group {
	var out []Group
	for _, registrant := range r.registrants {
		grouper, ok := registrant.(Grouper)
		if !ok {
			continue
		}
		for _, group := range grouper.Groups() {
			at := slices.IndexFunc(out, func(g Group) bool { return g.Title == group.Title && g.Profile == group.Profile })
			if at < 0 {
				out = append(out, group)
				continue
			}
			joined := &out[at]
			// The plugin that owns the group gives it its place, and its own sections lead
			if joined.Order == 0 && group.Order != 0 {
				joined.Order, joined.Sections = group.Order, append(slices.Clone(group.Sections), joined.Sections...)
			} else {
				joined.Sections = append(joined.Sections, group.Sections...)
			}
			joined.Area, joined.Opens = cmp.Or(joined.Area, group.Area), cmp.Or(joined.Opens, group.Opens)
			joined.Elsewhere = joined.Elsewhere || group.Elsewhere
		}
	}
	return out
}

// Name shown in the app
func (d Def) Label() string { return cmp.Or(d.Title, d.Name) }

// Heading of each toolset by its name: the one a tool of it gives, else its name in words
func Groups(defs []Def) map[string]string {
	groups := map[string]string{}
	for _, def := range defs {
		if def.Group != "" && groups[def.Toolset] == "" {
			groups[def.Toolset] = def.Group
		}
	}
	for _, def := range defs {
		if groups[def.Toolset] == "" {
			groups[def.Toolset] = strs.Capital(strings.NewReplacer("_", " ", "-", " ").Replace(def.Toolset))
		}
	}
	return groups
}

// Schema the model of the session sees
func (d Def) For(env *Env) map[string]any {
	if d.Dynamic != nil {
		if schema := d.Dynamic(env); schema != nil {
			return schema
		}
	}
	return d.Schema
}

type Call struct {
	Name       string
	Args       map[string]any
	RawArgs    string
	ToolCallID string
	SessionID  string
	Env        *Env
	// Images or videos the result hands the model along with its text, which reach it only when Env.Aux says the main model takes them
	Media []*sessionsv1.Media
	// What the user is shown of the result in place of its text, such as the change a write made; never sent to the model
	Display string
	// Tells the user how far a long call got: progress out of total, total nil when not known, and what it is doing now; nil where
	// nobody watches
	Progress func(progress float64, total *float64, message string)
}

type Env struct {
	Cwd    string
	Config *config.Config
	// Asks the user to allow a flagged action, as the approvals decide, the answer remembered under the reason: nil when it may go ahead,
	// else a *Refusal saying why not; nil where no one can be asked
	Approve func(ctx context.Context, action, reason string) error
	// Asks as Approve does, with the options of the gate, such as asking every time
	Gate func(ctx context.Context, gate Gate) error
	// Checks a shell command against the approval rules before it runs, asking the user when they flag it; isolated for a backend whose
	// commands can't reach this machine, where only the always-deny patterns apply
	Command func(ctx context.Context, command string, isolated bool) Verdict
	// Asks the user the questions and waits for the answers, nil where no one can be asked
	Clarify func(ctx context.Context, questions []Question) (Reply, error)
	// Helper models of the session's profile, routed from its main model; nil where no session runs
	Aux *auxiliary.Client
	// Adds a message to the session from outside a turn, such as one a background process sends as it ends: the agent answers it in a
	// turn of its own, after the one running; an error when the session can't take it; nil where nothing can reach the session later
	Notify func(notice Notice) error
	// Whether the user sent a message while the tool runs, consumed by the first tool that asks: a long tool hands its work to the
	// background and returns, so the agent can answer; nil where no one can send one
	Yield func() bool
	// Surface the session is shown on, such as "tui", whose own disabled skills are left out; none where not known
	Platform string
	// The turn answers one query and the session ends, such as a one-shot run of the command line: what tools wait on to set up gets
	// longer
	OneShot bool
	// Shell the user's client runs in, as the session froze it from the first message it was sent; nil when it was sent none
	Client *Client
	// Tokens the session's main model takes in at once, which sizes how much of a tool result stays in the chat; 0 when not known
	Window int
	// Path the session's commands see a file of the profile's caches at, such as a saved tool result inside a sandbox; nil where host
	// paths hold
	Visible func(host string) string
	// The session runs as a subagent another session's agent delegated a task to; nil for a session of its own
	Delegated *Delegated
}

// What a session running as a subagent knows of the agent that delegated to it
type Delegated struct {
	// Session of the agent that delegated to it
	Parent string
	// Its id in the spawn tree, such as sa-0-1a2b3c4d
	ID string
	// How deep it nests: 1 for a child of a session of its own
	Depth int
	// Background processes of the session handed over to the parent so far, and records one more, which the parent hears the end of in its
	// place
	Handoffs func() int
	Handoff  func(process, command, note string)
	// Tells the parent's session something out of turn, such as the end of a background process it took over
	Notify func(notice Notice) error
}

// Shell of the user's client as a session froze it: the variables of it the session keeps, and the rules they were kept by
type Client struct {
	Env map[string]string `json:"env"`
	// The client runs on this machine, so its toolchain holds here; of a remote one only the behaviour variables that name nothing of a
	// machine are kept
	Local bool `json:"local"`
	// Behaviour variables kept, toolchain variables kept by name and by prefix, and the fragments that mark a name as a credential
	Behaviour []string `json:"behaviour"`
	Names     []string `json:"names"`
	Prefixes  []string `json:"prefixes"`
	Secret    []string `json:"secret"`
}

// Main model of the session: its provider, by name and kind, and its model; empty where no session runs
func (e *Env) Main() provider.Route {
	if e.Aux == nil {
		return provider.Route{}
	}
	return e.Aux.Main
}

// Message a session gets from outside a turn
type Notice struct {
	// What the model reads
	Text string
	// Short line the user sees for it, none when empty
	Title string
}

type Registrant interface {
	Tools() []Def
}

// Registrant that hears of every call dispatched, before its tool runs
type Watcher interface {
	Dispatched(call *Call)
}

// Registrant that hears of every call once its tool returned, with the result the model reads, such as a write that landed
type Finisher interface {
	Finished(call *Call, result string)
}

// Registrant that keeps something for each reply of the model, such as the folders snapshotted before its calls changed them: told the
// calls of a new reply of the session are about to run
type Stepper interface {
	Stepped(session string)
}

// Registrant that keeps what the model was shown in a session, which it forgets once the session's context is compacted and the model
// no longer holds those results
type Compacter interface {
	Compacted(cfg *config.Config, session string)
}

// Registrant whose tools keep state of a session the model must still see once the messages that showed it are compacted away, such as a
// task list: the text it hands back is put into the compacted context, its first line marking it so an older copy is replaced
type Carrier interface {
	Carried(env *Env, session string) string
}

// Registrant whose tools follow what the session's settings set up, such as the servers a profile connects: readied for the session
// each time its tools are listed or a call is dispatched
type Preparer interface {
	Prepare(env *Env)
}

// Registrant whose tools leave work running past their call that tells the session when it ends, such as a background process that
// notifies on completion: a one-shot session waits for it before it ends
type Lingerer interface {
	// Waits until the session's work that will tell it of its end has, its notices handed to Env.Notify, the deadline passed or the
	// context ended; whether there was any such work, and whether some of it still runs
	Linger(ctx context.Context, env *Env, session string, deadline time.Time) (waited, running bool)
}

// Registrant whose tools learn more of a call after its result returned, such as a language server that answers an edit after the write
// did: the agent hands the notes to the model with the next results it stores and with the next message of the session, and before a turn
// ends waits for those still coming, five seconds at most, going on with the turn to answer them three times at most
type Noter interface {
	// Notes of the session, each handed out once, oldest first; with a deadline ahead, first waits until the session's work that will note
	// something settled, the deadline passed or the context ended
	Notes(ctx context.Context, session string, deadline time.Time) []string
}

// Registrant that checks what the file tools write, such as the language servers
type Checker interface {
	// Whether it takes files of the extension, so a write keeps the content it replaces to compare against
	Handles(cfg *config.Config, extension string) bool
	// Whether it checks the file, so the per file shell check that floods false errors is left out
	Checks(cfg *config.Config, path string) bool
	// Diagnostics the edit brought in as a block the model reads, empty when there are none or the verdict comes after the call; a late
	// verdict is handed to the session, the owner
	Check(ctx context.Context, cfg *config.Config, path string, before, after *string, owner string, started time.Time) string
}

// Registrant whose tools keep something for one turn only, such as a browser session: told the session's turn ended, whatever way it
// ended, so it lets go of what the next turn makes again
type Ender interface {
	Ended(env *Env, session string)
}

type Registry struct {
	registrants []Registrant
	// Host the registry is nested in, nil for one of no host
	host *Tools
}

func (r *Registry) Register(registrant Registrant) {
	r.registrants = append(r.registrants, registrant)
}

func (r *Registry) Tools() []Def {
	var defs []Def
	for _, registrant := range r.registrants {
		defs = append(defs, registrant.Tools()...)
	}
	return defs
}

// Definition of the tool with the name, false when no registrant has one
func (r *Registry) Def(name string) (Def, bool) {
	for _, def := range r.Tools() {
		if def.Name == name {
			return def, true
		}
	}
	return Def{}, false
}

// Readies the registrants whose tools follow the session's settings
func (r *Registry) Prepare(env *Env) {
	for _, registrant := range r.registrants {
		if preparer, ok := registrant.(Preparer); ok {
			preparer.Prepare(env)
		}
	}
}

// Tells the registrants that watch calls of one about to run
func (r *Registry) Dispatched(call *Call) {
	for _, registrant := range r.registrants {
		if watcher, ok := registrant.(Watcher); ok {
			watcher.Dispatched(call)
		}
	}
}

// Tells the registrants that hear of calls of one whose tool returned
func (r *Registry) Finished(call *Call, result string) {
	for _, registrant := range r.registrants {
		if finisher, ok := registrant.(Finisher); ok {
			finisher.Finished(call, result)
		}
	}
}

// Tells the registrants that keep something for each reply of the model that the calls of a new reply of the session are about to run,
// which the agent calls before it runs the tool calls of each reply
func (r *Registry) Stepped(session string) {
	for _, registrant := range r.registrants {
		if stepper, ok := registrant.(Stepper); ok {
			stepper.Stepped(session)
		}
	}
}

// Tells the registrants that keep what the model was shown that the session's context was compacted, which the compaction of a session
// calls once it folded messages away
func (r *Registry) Compacted(cfg *config.Config, session string) {
	for _, registrant := range r.registrants {
		if compacter, ok := registrant.(Compacter); ok {
			compacter.Compacted(cfg, session)
		}
	}
}

// Texts the registrants that keep state of the session hand back after its context was compacted, those with nothing to carry left out
func (r *Registry) Carried(env *Env, session string) []string {
	var texts []string
	for _, registrant := range r.registrants {
		if carrier, ok := registrant.(Carrier); ok {
			if text := strings.TrimSpace(carrier.Carried(env, session)); text != "" {
				texts = append(texts, text)
			}
		}
	}
	return texts
}

// Tells the registrants that keep something for one turn only that the session's turn ended, which the agent calls as each turn of a
// session finishes
func (r *Registry) Ended(env *Env, session string) {
	for _, registrant := range r.registrants {
		if ender, ok := registrant.(Ender); ok {
			ender.Ended(env, session)
		}
	}
}

// Waits for the work every registrant that leaves some running has of the session, until the deadline; whether there was any, and whether
// some of it still runs
func (r *Registry) Linger(ctx context.Context, env *Env, session string, deadline time.Time) (waited, running bool) {
	for _, registrant := range r.registrants {
		if lingerer, ok := registrant.(Lingerer); ok {
			w, still := lingerer.Linger(ctx, env, session, deadline)
			waited, running = waited || w, running || still
		}
	}
	return waited, running
}

// Notes every registrant that learns more of calls has for the session, waiting for those still coming until the deadline
func (r *Registry) Notes(ctx context.Context, session string, deadline time.Time) []string {
	var notes []string
	for _, registrant := range r.registrants {
		if noter, ok := registrant.(Noter); ok {
			notes = append(notes, noter.Notes(ctx, session, deadline)...)
		}
	}
	return notes
}

// Whether a registrant that checks writes takes files of the extension
func (r *Registry) Handles(cfg *config.Config, extension string) bool {
	for _, registrant := range r.registrants {
		if checker, ok := registrant.(Checker); ok && checker.Handles(cfg, extension) {
			return true
		}
	}
	return false
}

// Whether a registrant that checks writes checks the file
func (r *Registry) Checks(cfg *config.Config, path string) bool {
	for _, registrant := range r.registrants {
		if checker, ok := registrant.(Checker); ok && checker.Checks(cfg, path) {
			return true
		}
	}
	return false
}

// Diagnostics the edit brought in, the blocks of every registrant that checks writes one after another
func (r *Registry) Check(ctx context.Context, cfg *config.Config, path string, before, after *string, owner string, started time.Time) string {
	var blocks []string
	for _, registrant := range r.registrants {
		if checker, ok := registrant.(Checker); ok {
			if block := checker.Check(ctx, cfg, path, before, after, owner, started); block != "" {
				blocks = append(blocks, block)
			}
		}
	}
	return strings.Join(blocks, "\n\n")
}

// Stops what the registrants keep running past their calls, such as interpreters or drivers, last registered first, as icebath exits
func (r *Registry) Close() error {
	var errs []error
	for _, registrant := range slices.Backward(r.registrants) {
		if closer, ok := registrant.(io.Closer); ok {
			errs = append(errs, closer.Close())
		}
	}
	return errors.Join(errs...)
}

// Every kind of risky action the tools ask approval for, each about the tool that asks, which the approvals allowlist can hold
func (r *Registry) Risks() []config.Option {
	var options []config.Option
	for _, def := range r.Tools() {
		for _, risk := range def.Risks {
			if !slices.ContainsFunc(options, func(option config.Option) bool { return option.Value == risk }) {
				options = append(options, config.Option{Value: risk, Title: strs.Capital(risk), About: "Asked by the " + def.Label() + " tool"})
			}
		}
	}
	return options
}

func (r *Registry) Definitions(env *Env) []map[string]any {
	return Definitions(r.Offered(env), env)
}

// Tools offered to the session, by name
func (r *Registry) Offered(env *Env) []Def {
	defs := r.available(env)
	slices.SortFunc(defs, func(a, b Def) int { return cmp.Compare(a.Name, b.Name) })
	return defs
}

// What the model is sent of the tools
func Definitions(defs []Def, env *Env) []map[string]any {
	definitions := make([]map[string]any, len(defs))
	for i, def := range defs {
		schema := maps.Clone(def.For(env))
		schema["name"] = def.Name
		definitions[i] = map[string]any{"type": "function", "function": schema}
	}
	return definitions
}

func (r *Registry) Dispatch(ctx context.Context, call *Call) string {
	for _, def := range r.available(call.Env) {
		if def.Name == call.Name {
			return r.Run(ctx, def, call)
		}
	}
	return ErrorResult(fmt.Errorf("unknown tool %s", call.Name))
}

// Runs the call as Dispatch does for a caller that reads the whole result itself, such as a script calling tools, so none of it is saved
// aside for the chat
func (r *Registry) Invoke(ctx context.Context, call *Call) string {
	for _, def := range r.available(call.Env) {
		if def.Name == call.Name {
			if result := strings.ToValidUTF8(r.run(ctx, def, call), "�"); result != "" {
				return result
			}
			return "(no output)"
		}
	}
	return ErrorResult(fmt.Errorf("unknown tool %s", call.Name))
}

// Runs the call on the tool: its arguments read and checked against the schema, the watchers told, a result too large for the chat
// saved to a file the model can page through, its start kept
func (r *Registry) Run(ctx context.Context, def Def, call *Call) string {
	budget := BudgetFor(call.Env.Config, call.Env.Window)
	result := strings.ToValidUTF8(r.run(ctx, def, call), "�")
	if result == "" {
		return "(no output)"
	}
	return Spill(call.Env, result, def.Name, call.ToolCallID, budget.Threshold(def), budget.Preview)
}

func (r *Registry) run(ctx context.Context, def Def, call *Call) string {
	if err := parse(def, call); err != nil {
		return ErrorResult(err)
	}
	return r.hooked(ctx, def, call, func(ctx context.Context) string {
		r.Dispatched(call)
		// A value the vault filled into a page never reaches the model, whatever echoes it
		result := redact.Filled(call.Env.Config.Profile(), def.Handler(ctx, call))
		r.Finished(call, result)
		return result
	})
}

// Reads the call's arguments into Args, coerced to the tool's schema, refusing arguments that are no JSON object or lack a required one
func parse(def Def, call *Call) error {
	if err := json.Unmarshal([]byte(cmp.Or(call.RawArgs, "{}")), &call.Args); err != nil {
		return fmt.Errorf("arguments are not a JSON object: %w", err)
	}
	parameters, _ := def.For(call.Env)["parameters"].(map[string]any)
	if err := coerce(call, parameters); err != nil {
		return err
	}
	required, _ := parameters["required"].([]string)
	for _, name := range required {
		if _, ok := call.Args[name]; !ok {
			return fmt.Errorf("missing argument %s", name)
		}
	}
	return nil
}

func (r *Registry) available(env *Env) []Def {
	r.Prepare(env)
	disabled := env.Config.GetStringSlice(toolsconfig.Disabled)
	var defs []Def
	for _, def := range r.Tools() {
		if def.Schema != nil && def.Handler != nil && !slices.Contains(disabled, def.Name) && (def.Check == nil || def.Check(env)) {
			defs = append(defs, def)
		}
	}
	return defs
}

type Tools struct {
	Registry
	// Hook points every call runs between
	hooks *hooks.Hooks
}

func New(cfg *config.Config, hk *hooks.Hooks) *Tools {
	t := &Tools{hooks: hk}
	t.Registry.host = t
	t.Register(verifying{hooks: hk})
	guardDisabled(cfg)
	toolsconfig.Register(cfg, func() []config.Option {
		var options []config.Option
		for _, def := range t.Tools() {
			options = append(options, config.Option{Value: def.Name, Title: def.Label(), About: def.About})
		}
		return options
	})
	return t
}

// Adds a registrant; a registry nested in the host reaches the host's hooks
func (t *Tools) Register(registrant Registrant) {
	if nested, ok := registrant.(*Registry); ok {
		nested.host = t
	}
	t.Registry.Register(registrant)
}

func Bind[T any](run func(ctx context.Context, call *Call, args T) (string, error)) Handler {
	return func(ctx context.Context, call *Call) string {
		var args T
		if err := json.Unmarshal([]byte(cmp.Or(call.RawArgs, "{}")), &args); err != nil {
			return ErrorResult(err)
		}
		result, err := run(ctx, call, args)
		if err != nil {
			return ErrorResult(err)
		}
		return result
	}
}

func Schema(description string, required []string, properties map[string]any) map[string]any {
	return map[string]any{
		"description": description,
		"parameters":  map[string]any{"type": "object", "properties": properties, "required": required},
	}
}

func Property(kind, description string) map[string]any {
	return map[string]any{"type": kind, "description": description}
}

func ErrorResult(err error) string {
	result, _ := json.Marshal(map[string]string{"error": err.Error()})
	return string(result)
}

// Result as JSON on one line, text left unescaped, a value that cannot be encoded as the error result
func JSON(value any) string {
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(value); err != nil {
		return ErrorResult(err)
	}
	return strings.TrimSuffix(out.String(), "\n")
}

func Failed(result string) bool {
	var body map[string]json.RawMessage
	if json.Unmarshal([]byte(result), &body) != nil {
		return false
	}
	message, ok := body["error"]
	return ok && string(message) != "null"
}

// Longest error text a result carries before it is cut
const ErrorChars = 2048

// Error text of a result cut to the characters the model reads of an error
func BoundError(text string) string {
	if runes := []rune(text); len(runes) > ErrorChars {
		return string(runes[:ErrorChars]) + "… [truncated]"
	}
	return text
}

func Bound(text string, limit int) string {
	if len(text) <= limit {
		return text
	}
	output := NewCapture(limit)
	output.Write([]byte(text))
	return output.String()
}

type Capture struct {
	half       int
	head, tail []byte
	total      int
}

func NewCapture(limit int) *Capture {
	return &Capture{half: max(limit-64, 0) / 2}
}

func (c *Capture) Write(data []byte) (int, error) {
	c.total += len(data)
	keep := min(c.half-len(c.head), len(data))
	c.head = append(c.head, data[:keep]...)
	c.tail = append(c.tail, data[keep:]...)
	c.tail = c.tail[max(len(c.tail)-c.half, 0):]
	return len(data), nil
}

func (c *Capture) String() string {
	if omitted := c.total - len(c.head) - len(c.tail); omitted > 0 {
		return fmt.Sprintf("%s\n[%d bytes omitted]\n%s", c.head, omitted, c.tail)
	}
	return string(c.head) + string(c.tail)
}
