package tuigateway

// Shell line of the composer's !cmd and {!cmd}, run in the session's working directory when it names one
type ShellExecParams struct {
	SessionID string  `json:"session_id,omitzero"`
	Command   string  `json:"command"`
	Profile   *string `json:"profile,omitzero"`
}

// Shell line of a local session on the shared owner, run in its frozen launch cwd
type CanonicalShellExecParams struct {
	SessionID string  `json:"session_id"`
	Command   string  `json:"command"`
	Profile   *string `json:"profile,omitzero"`
}

// Redacted tails of what a shell line wrote and its exit code
type ShellExecResult struct {
	Stdout string `json:"stdout"`
	Stderr string `json:"stderr"`
	Code   int64  `json:"code"`
}

// Session whose profile and workspace the catalog binds to, or a profile for a session less draft
type CommandsCatalogParams struct {
	SessionID string  `json:"session_id,omitzero"`
	Profile   *string `json:"profile,omitzero"`
}

// One palette group with its command and description pairs
type CommandCategory struct {
	Name  string      `json:"name"`
	Pairs [][2]string `json:"pairs"`
}

// How often a skill ran and where it came from
type SkillCatalogEntry struct {
	Usage  int64  `json:"usage"`
	Origin string `json:"origin"`
}

// Slash catalog, every pair, subcommands, aliases to canonical names, groups, skills and plugin commands
type CommandsCatalogResult struct {
	Pairs                [][2]string                  `json:"pairs"`
	Sub                  map[string][]string          `json:"sub"`
	Canon                map[string]string            `json:"canon"`
	Categories           []CommandCategory            `json:"categories"`
	Skills               map[string]SkillCatalogEntry `json:"skills"`
	SkillCount           int64                        `json:"skill_count"`
	Warning              string                       `json:"warning,omitzero"`
	PluginDiscoveryError string                       `json:"plugin_discovery_error,omitzero"`
	PluginCommands       map[string]string            `json:"plugin_commands"`
	// Commands that run only in messaging chats with their descriptions, aliases included
	ChatCommands map[string]string `json:"chat_commands"`
}

type DispatchType string

func (DispatchType) Values() []string {
	return []string{"exec", "alias", "plugin", "send", "skill", "prefill"}
}

// Command a client could not run itself, its argument and the session it runs in
type CommandDispatchParams struct {
	Name      string  `json:"name"`
	Arg       string  `json:"arg,omitzero"`
	SessionID string  `json:"session_id,omitzero"`
	Profile   *string `json:"profile,omitzero"`
}

// One directive, exec and plugin carry output, alias a target, send, prefill and skill a message the client renders as display
type CommandDispatchResult struct {
	Type    DispatchType `json:"type"`
	Output  string       `json:"output,omitzero"`
	Target  string       `json:"target,omitzero"`
	Message string       `json:"message,omitzero"`
	Notice  string       `json:"notice,omitzero"`
	Display string       `json:"display,omitzero"`
	Name    string       `json:"name,omitzero"`
}

// Slash line of a session without its leading slash
type SlashExecParams struct {
	SessionID string  `json:"session_id"`
	Command   string  `json:"command"`
	Profile   *string `json:"profile,omitzero"`
}

// Worker or plugin text with its warning, or the directive of a command rerouted to command.dispatch with its type set
type SlashExecResult struct {
	Output  string       `json:"output,omitzero"`
	Warning string       `json:"warning,omitzero"`
	Type    DispatchType `json:"type,omitzero"`
	Target  string       `json:"target,omitzero"`
	Message string       `json:"message,omitzero"`
	Notice  string       `json:"notice,omitzero"`
	Display string       `json:"display,omitzero"`
	Name    string       `json:"name,omitzero"`
}

// Directive of a slash answer, false when it carries plain output
func (r SlashExecResult) Directive() (CommandDispatchResult, bool) {
	return CommandDispatchResult{Type: r.Type, Output: r.Output, Target: r.Target, Message: r.Message, Notice: r.Notice,
		Display: r.Display, Name: r.Name}, r.Type != ""
}

// Slash answer of a directive
func (d CommandDispatchResult) SlashResult() SlashExecResult {
	return SlashExecResult{Type: d.Type, Output: d.Output, Target: d.Target, Message: d.Message, Notice: d.Notice,
		Display: d.Display, Name: d.Name}
}

// Recognized command a client ran itself, named so the gateway fires its pre command hook
type CommandObserveParams struct {
	SessionID string  `json:"session_id"`
	Name      string  `json:"name"`
	Arg       string  `json:"arg,omitzero"`
	Profile   *string `json:"profile,omitzero"`
}

// Whether the hook was queued
type CommandObserveResult struct {
	Queued bool `json:"queued"`
}

// Session the env reload serves, or the launch profile without one
type ReloadEnvParams struct {
	SessionID string  `json:"session_id,omitzero"`
	Profile   *string `json:"profile,omitzero"`
}

// Count of env values the reload changed
type ReloadEnvResult struct {
	Updated int64 `json:"updated"`
}

type PingParams struct{}

type PingResult struct {
	Pong bool `json:"pong"`
}

// Whether the calling client answers server requests
type ClientCapabilitiesParams struct {
	ServerRequests bool `json:"server_requests,omitzero"`
}

// Server requests this backend may send, and whether it counts unseen declines and batches clarify answers
type ClientCapabilitiesResult struct {
	ServerRequests        []string `json:"server_requests"`
	DeclinesNotShown      bool     `json:"declines_not_shown,omitzero"`
	CanonicalClarifyBatch bool     `json:"canonical_clarify_batch,omitzero"`
}

// Runs one safe shell line for the composer, the dangerous and hardline ones refused
var ShellExec = NewMethod[ShellExecParams, ShellExecResult]("shell.exec")

// Runs one safe shell line of a local session on the shared owner
var CanonicalShellExec = NewCanonicalMethod[CanonicalShellExecParams, ShellExecResult]("shell.exec")

// Slash catalog of registry, quick, plugin and skill commands for the palette and completion
var CommandsCatalog = NewMethod[CommandsCatalogParams, CommandsCatalogResult]("commands.catalog")

// Runs a quick, plugin, bundle, skill or built in slash command and answers its directive
var CommandDispatch = NewMethod[CommandDispatchParams, CommandDispatchResult]("command.dispatch")

// Runs a slash line against the session's slash worker or a live or plugin shortcut
var SlashExec = NewMethod[SlashExecParams, SlashExecResult]("slash.exec")

// Tells the shared owner a client ran a recognized command itself
var CommandObserve = NewCanonicalMethod[CommandObserveParams, CommandObserveResult]("command.observe")

// Reads the profile's env file again, built agents keeping their pool until a new session
var ReloadEnv = NewMethod[ReloadEnvParams, ReloadEnvResult]("reload.env")

// Cheapest liveness probe, answered while every agent is mid turn
var Ping = NewMethod[PingParams, PingResult]("ping")

// What the calling client handles, sent once per connection after gateway.ready
var ClientCapabilities = NewMethod[ClientCapabilitiesParams, ClientCapabilitiesResult]("client.capabilities")

// The session store moved, the UI fetches the session list again
var SessionsChangedEvent = NewEvent[NoPayload]("sessions.changed")
