package pluginsdk

import (
	"context"
	"sync"
	"time"
)

// Version of the frame protocol this library speaks
const ProtocolVersion = 1

// Entry point of a plugin, run once per load with the host it registers on
type RegisterFunc func(Host) error

var registered RegisterFunc

// Names the function the host runs when it loads this plugin, called from an init function
func Register(fn RegisterFunc) { registered = fn }

// Callback the host invokes with positional and named arguments
type Func func(ctx context.Context, args []any, kwargs map[string]any) (any, error)

// Hook, middleware or event callback receiving its payload by name
type HookFunc func(ctx context.Context, payload map[string]any) (any, error)

// Object the host calls by method name and reads by attribute name, the form every provider takes
type Object struct {
	Type    string
	Attrs   map[string]any
	Methods map[string]Func
	// Yields the next item each time the host steps through the object, ok false once it has no more
	Next func(ctx context.Context) (item any, ok bool, err error)
	// Facts the host judges the object by before it runs any of its code
	identity map[string]any
	// Bridge to the gateway of an object that is a platform adapter
	gateway *Gateway
	// Value this library keeps beside an object it built
	held any
	// Callbacks handed out for the object, dropped when the host releases it
	owned []int64
}

type selfKey struct{}

// Object the host addressed with the call the context serves, nil outside a call on an object
func Self(ctx context.Context) *Object {
	if ctx == nil {
		return nil
	}
	self, _ := ctx.Value(selfKey{}).(*Object)
	return self
}

// Guards the attributes of every object, which the host reads and writes while plugin code runs
var attrsMu sync.Mutex

// Reads one attribute of the object, safe while the host reads and writes its attributes
func (o *Object) Attr(name string) (any, bool) {
	attrsMu.Lock()
	defer attrsMu.Unlock()
	value, ok := o.Attrs[name]
	return value, ok
}

// Sets one attribute of the object, safe while the host reads and writes its attributes
func (o *Object) SetAttr(name string, value any) {
	attrsMu.Lock()
	defer attrsMu.Unlock()
	if o.Attrs == nil {
		o.Attrs = map[string]any{}
	}
	o.Attrs[name] = value
}

// Mapping that crosses with its keys in the order given, for the places icebath reads that order such as tool schema properties
type Ordered []Pair

// One key and value of an ordered mapping
type Pair struct {
	Key   string
	Value any
}

// Typed value the host restores by its type name
type Record struct {
	Type   string
	Fields map[string]any
}

// Host value with no wire form, known only by type name and text
type Opaque struct {
	Type string
	Text string
}

// Failure the host reported for a request
type Error struct {
	Type    string
	Message string
}

func (e *Error) Error() string {
	if e.Message == "" {
		return e.Type
	}
	return e.Type + ": " + e.Message
}

// One tool the model can call
type Tool struct {
	Name        string
	Toolset     string
	Description string
	Emoji       string
	Schema      map[string]any
	RequiresEnv []string
	Override    bool
	Check       func(ctx context.Context) bool
	Handler     func(ctx context.Context, call ToolCall) (string, error)
}

// Arguments and session identity of one tool call
type ToolCall struct {
	Args      map[string]any
	TaskID    string
	SessionID string
	UserTask  string
}

// One slash command
type Command struct {
	Name         string
	Description  string
	ArgsHint     string
	ArgumentMode string
	Handler      func(ctx context.Context, args string) (string, error)
}

// Approval request a transport presents to the user
type ApprovalRequest struct {
	SchemaVersion  int64
	RequestID      string
	Digest         string
	Command        string
	Description    string
	PatternKey     string
	PatternKeys    []string
	Surface        string
	TimeoutSeconds float64
	AllowedChoices []string
}

// Presents an approval request and returns the choice made, empty when none was made
type ApprovalFunc func(ctx context.Context, req ApprovalRequest) (choice string, err error)

// Slack interactive action callback, ack answering Slack with an optional payload
type SlackActionFunc func(ctx context.Context, ack func(payload map[string]any) error, body, action map[string]any) error

// Named view the terminal UI renders, its state a JSON value the UI holds between calls
type Widget struct {
	ID    string
	Help  string
	Usage string
	Mode  string
	Zone  string
	Width int
	// Interval the terminal UI refreshes the widget at, running Tick when set and rendering again, zero for input only
	Refresh time.Duration
	Open    func(ctx context.Context, arg string) (state any, ok bool, err error)
	Input   func(ctx context.Context, state any, in WidgetInput) (next any, open bool, err error)
	// Next state at each refresh, nil for a widget that only renders again
	Tick   func(ctx context.Context, state any) (next any, err error)
	Render func(ctx context.Context, frame WidgetFrame) (WidgetView, error)
}

// One key press or text input delivered to a widget
type WidgetInput struct {
	Text  string `json:"text"`
	Key   string `json:"key"`
	Ctrl  bool   `json:"ctrl"`
	Alt   bool   `json:"alt"`
	Shift bool   `json:"shift"`
}

// Size and state a widget renders for
type WidgetFrame struct {
	Cols  int
	Rows  int
	State any
}

// Rendered lines of a widget
type WidgetView struct {
	Lines []WidgetLine `json:"lines"`
}

// One rendered line
type WidgetLine struct {
	Spans []WidgetSpan `json:"spans"`
}

// Run of text in one theme role and style
type WidgetSpan struct {
	Text      string `json:"text"`
	Role      string `json:"role,omitempty"`
	Bold      bool   `json:"bold,omitempty"`
	Dim       bool   `json:"dim,omitempty"`
	Italic    bool   `json:"italic,omitempty"`
	Underline bool   `json:"underline,omitempty"`
	Inverse   bool   `json:"inverse,omitempty"`
}

// Scheduled automation a user can instantiate
type Blueprint struct {
	Title            string
	Description      string
	ScheduleTemplate string
	PromptTemplate   string
	Category         string
	Slots            []any
	DeliverDefault   string
	Skills           []string
	Tags             []string
}

// Auxiliary model task a plugin declares
type AuxiliaryTask struct {
	DisplayName string
	Description string
	Defaults    map[string]any
	InheritFrom string
}

// Display metadata of a locale
type Locale struct {
	Endonym string
	RTL     bool
	Surface string
}

// Placement and size bound of a system prompt section
type Section struct {
	Position string
	MaxChars int
}

// One entry of a listed directory
type DirEntry struct {
	Name string
	Dir  bool
	Size int64
}

// HTTP request the host performs for the plugin
type HTTPRequest struct {
	Method         string
	URL            string
	Headers        map[string]string
	Body           []byte
	TimeoutSeconds float64
}

// HTTP response the host returned
type HTTPResponse struct {
	Status  int
	Headers map[string]string
	Body    []byte
}

// Request icebath sends to a provider with the sign in the plugin declares in requires_auth or oauth attached
type ProviderRequest struct {
	Provider       string
	Method         string
	URL            string
	Headers        map[string]string
	JSON           any
	TimeoutSeconds float64
}

// Everything a plugin can register on icebath and ask of it
type Host interface {
	// Id icebath knows the plugin by
	PluginID() string
	// Key of the plugin among the loaded plugins of its profile
	PluginKey() string
	// Name the plugin manifest declares
	Name() string
	// Directory the plugin was loaded from
	Dir() string
	// Name of the profile the plugin runs for
	ProfileName() string
	// Fields of the plugin manifest
	Manifest() map[string]any

	// Value of one key of the plugin settings, def when it is unset
	Config(ctx context.Context, key string, def any) (any, error)
	// Stores one key of the plugin settings
	SetConfig(ctx context.Context, key string, value any) error
	// Whether another plugin of that id or name is loaded and enabled
	HasPlugin(ctx context.Context, id string) (bool, error)
	// Whether a capability is granted to the plugin
	HasCapability(ctx context.Context, capability string) (bool, error)

	// Adds a tool the model can call
	RegisterTool(tool Tool) (*Handle, error)
	// Runs fn on every firing of a lifecycle hook, holdStream keeping the reply stream open while it runs
	RegisterHook(name string, fn HookFunc, holdStream bool) (*Handle, error)
	// Runs fn as middleware of one kind
	RegisterMiddleware(kind string, fn HookFunc) (*Handle, error)
	// Adds a slash command of a session
	RegisterCommand(cmd Command) (*Handle, error)
	// Adds an `icebath <name>` subcommand
	RegisterCLICommand(cmd CLICommand) (*Handle, error)
	// Registers a provider object of one kind, the members that kind leaves optional filled with their base behaviour: image_gen, video_gen, web_search, browser, terminal_environment, secret_source, tts, transcription, delegation, context_engine, context_reference, memory, login_backend, browser_page_backend, cron_scheduler or computer_use
	RegisterProvider(kind string, provider *Object) (*Handle, error)
	// Registers a browser vault backend that resolves saved logins
	RegisterLoginBackend(backend LoginBackend) (*Handle, error)
	// Registers a terminal backend whose environments run the commands of the terminal, code and file tools
	RegisterTerminalBackend(backend TerminalBackend) (*Handle, error)
	// Registers a cron scheduler that decides when due jobs fire
	RegisterCronScheduler(scheduler CronScheduler) (*Handle, error)
	// Adds a messaging platform to the gateway
	RegisterPlatform(platform Platform) (*Handle, error)
	// Runs fn for a Slack interactive action whose id matches a string, a *regexp.Regexp or a mapping of constraints
	RegisterSlackActionHandler(actionID any, fn SlackActionFunc) (*Handle, error)
	// Adds a transport that presents command approvals to the user
	RegisterApprovalTransport(name string, present ApprovalFunc) error
	// Adds a view the terminal UI renders
	RegisterWidget(widget Widget) (*Handle, error)
	// Adds a read only skill resolvable as <plugin>:<name>
	RegisterSkill(name, path, description string, frontmatter map[string]any) (*Handle, error)
	// Adds messages of one language from a YAML file path or a mapping of dotted keys
	RegisterLocale(lang string, source any, locale Locale) (*Handle, error)
	// Adds every language file of a directory
	RegisterLocaleDir(path string, metadata map[string]map[string]any) error
	// Adds a section to the system prompt, its content a string or a func(context.Context, map[string]any) (string, error)
	RegisterSystemPromptSection(id string, content any, section Section) (*Handle, error)
	// Adds a scheduled automation users can instantiate
	RegisterAutomationBlueprint(key string, blueprint Blueprint) (*Handle, error)
	// Declares an auxiliary model task with its own settings
	RegisterAuxiliaryTask(key string, task AuxiliaryTask) (*Handle, error)
	// Adds patterns whose matches are masked in output and returns how many were accepted
	RegisterRedactionPatterns(patterns []string) (int, error)
	// Runs fn for every event of a qualified name such as <plugin>:<event>
	Subscribe(event string, fn HookFunc) error
	// Publishes an event under the name of the plugin and returns how many subscribers it was scheduled for
	Emit(ctx context.Context, event string, payload map[string]any) (int, error)
	// Runs fn when the plugin unloads, the last one added first
	OnUnload(fn func() error)

	// Runs a tool by name and returns its result text
	DispatchTool(ctx context.Context, name string, args map[string]any) (string, error)
	// Calls a tool of a connected MCP server
	CallMCP(ctx context.Context, server, tool string, args map[string]any, timeoutSeconds float64) (map[string]any, error)
	// Queues a message for the session of the call or the one a session key names, true when a host accepted it
	InjectMessage(ctx context.Context, content, role, sessionKey string) (bool, error)
	// Queues a message for a new session at a chat of the plugin's own profile, true when a host accepted it
	InjectMessageAt(ctx context.Context, content, role string, origin SessionSource) (bool, error)
	// Posts text into the session of the call and returns its handle, a refusal reported by the handle and never as an error
	PostMessage(ctx context.Context, text string) (*Message, error)
	// Asks the user of a slash command to pick one choice, ok false when nobody can answer
	AskChoice(ctx context.Context, question string, choices []string) (choice string, ok bool, err error)
	// Asks the agent of the session about the turn that just ended, with the token its post_llm_call hook received, ok false when it gave no answer
	SideQuestion(ctx context.Context, token, prompt string, timeoutSeconds float64) (answer string, ok bool, err error)
	// Working directory of the session of the call
	SessionCwd(ctx context.Context) (string, error)
	// Client or messaging platform of the session of the call
	SessionSurface(ctx context.Context) (string, error)
	// Scheduled run the call belongs to, nil outside one
	CurrentCronExecution(ctx context.Context) (any, error)
	// Rewrites the latest user message into one retrieval question for a memory provider, empty when the message itself is the better query
	RewriteMemoryQuery(ctx context.Context, userMessage string) (string, error)
	// Reads an attribute or calls a method of one facade: state, llm, platform_actions, subagent_lifecycle or delegation_backends
	Facade(ctx context.Context, facade, method string, args []any, kwargs map[string]any) (any, error)
	// Calls one method of the plugin context by its wire name
	Call(ctx context.Context, method string, args []any, kwargs map[string]any) (any, error)

	// Reads a file the manifest grants read access to
	ReadFile(ctx context.Context, path string) ([]byte, error)
	// Writes a file the manifest grants write access to
	WriteFile(ctx context.Context, path string, data []byte) error
	// Lists a directory the manifest grants read access to
	ListDir(ctx context.Context, path string) ([]DirEntry, error)
	// Removes a file the manifest grants write access to
	Remove(ctx context.Context, path string) error
	// Performs an HTTP request to a host the manifest grants
	HTTP(ctx context.Context, req HTTPRequest) (*HTTPResponse, error)
	// Sends a provider request with the sign in of the plugin attached
	ProviderRequest(ctx context.Context, req ProviderRequest) (*HTTPResponse, error)
	// Value of an environment variable the manifest grants, ok false when it is unset
	Env(ctx context.Context, name string) (value string, ok bool, err error)
}

// Registration the host holds for this plugin, removable before unload
type Handle struct {
	peer *peer
	id   int64
	Kind string
	Key  string
}

// Removes the registration on the host, nothing for a nil handle of a registration the host answered none for
func (h *Handle) Dispose(ctx context.Context) error {
	if h == nil || h.peer == nil {
		return nil
	}
	_, err := h.peer.call(ctx, "dispose", map[string]any{"handle": h.id})
	return err
}

// Host callable handed to the plugin as an argument
type HostFunc struct {
	peer *peer
	id   int64
}

// Calls the host callable
func (f *HostFunc) Call(ctx context.Context, args []any, kwargs map[string]any) (any, error) {
	return f.peer.call(ctx, "parent_call", map[string]any{"id": f.id, "args": f.peer.encode(listOrEmpty(args)), "kwargs": f.peer.encode(mapOrEmpty(kwargs))})
}
