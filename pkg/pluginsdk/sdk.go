package pluginsdk

import "context"

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

// One messaging platform, its adapter built per connection from the platform config
type Platform struct {
	Name           string
	Label          string
	RequiredEnv    []string
	InstallHint    string
	Extra          map[string]any
	Check          func(ctx context.Context) bool
	ValidateConfig func(ctx context.Context, config map[string]any) (bool, error)
	Adapter        func(ctx context.Context, config map[string]any) (*Object, error)
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
	ID     string
	Help   string
	Usage  string
	Mode   string
	Zone   string
	Width  int
	Open   func(ctx context.Context, arg string) (state any, ok bool, err error)
	Input  func(ctx context.Context, state any, in WidgetInput) (next any, open bool, err error)
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

// Everything a plugin can register on icebath and ask of it
type Host interface {
	PluginID() string
	PluginKey() string
	Name() string
	Dir() string
	ProfileName() string
	Manifest() map[string]any

	Config(ctx context.Context, key string, def any) (any, error)
	SetConfig(ctx context.Context, key string, value any) error
	HasPlugin(ctx context.Context, id string) (bool, error)
	HasCapability(ctx context.Context, capability string) (bool, error)

	RegisterTool(tool Tool) (*Handle, error)
	RegisterHook(name string, fn HookFunc, holdStream bool) (*Handle, error)
	RegisterMiddleware(kind string, fn HookFunc) (*Handle, error)
	RegisterCommand(cmd Command) (*Handle, error)
	RegisterProvider(kind string, provider *Object) (*Handle, error)
	RegisterPlatform(platform Platform) (*Handle, error)
	RegisterSlackActionHandler(actionID any, fn SlackActionFunc) (*Handle, error)
	RegisterApprovalTransport(name string, present ApprovalFunc) error
	RegisterWidget(widget Widget) (*Handle, error)
	RegisterSkill(name, path, description string, frontmatter map[string]any) (*Handle, error)
	RegisterLocale(lang string, source any, locale Locale) (*Handle, error)
	RegisterLocaleDir(path string, metadata map[string]map[string]any) error
	RegisterSystemPromptSection(id string, content any, section Section) (*Handle, error)
	RegisterAutomationBlueprint(key string, blueprint Blueprint) (*Handle, error)
	RegisterAuxiliaryTask(key string, task AuxiliaryTask) (*Handle, error)
	RegisterRedactionPatterns(patterns []string) (int, error)
	Subscribe(event string, fn HookFunc) error
	Emit(ctx context.Context, event string, payload map[string]any) (int, error)
	OnUnload(fn func() error)

	DispatchTool(ctx context.Context, name string, args map[string]any) (string, error)
	CallMCP(ctx context.Context, server, tool string, args map[string]any, timeoutSeconds float64) (map[string]any, error)
	InjectMessage(ctx context.Context, content, role, sessionKey string) (bool, error)
	PostMessage(ctx context.Context, text string) (any, error)
	AskChoice(ctx context.Context, question string, choices []string) (choice string, ok bool, err error)
	SideQuestion(ctx context.Context, token, prompt string, timeoutSeconds float64) (answer string, ok bool, err error)
	SessionCwd(ctx context.Context) (string, error)
	SessionSurface(ctx context.Context) (string, error)
	CurrentCronExecution(ctx context.Context) (any, error)
	Facade(ctx context.Context, facade, method string, args []any, kwargs map[string]any) (any, error)
	Call(ctx context.Context, method string, args []any, kwargs map[string]any) (any, error)

	ReadFile(ctx context.Context, path string) ([]byte, error)
	WriteFile(ctx context.Context, path string, data []byte) error
	ListDir(ctx context.Context, path string) ([]DirEntry, error)
	Remove(ctx context.Context, path string) error
	HTTP(ctx context.Context, req HTTPRequest) (*HTTPResponse, error)
	Env(ctx context.Context, name string) (value string, ok bool, err error)
}

// Registration the host holds for this plugin, removable before unload
type Handle struct {
	peer *peer
	id   int64
	Kind string
	Key  string
}

// Removes the registration on the host
func (h *Handle) Dispose(ctx context.Context) error {
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
