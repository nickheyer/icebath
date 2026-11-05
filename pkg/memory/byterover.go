package memory

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const byteroverManifest = `name: byterover
version: 1.0.0
description: "ByteRover — persistent knowledge tree with tiered retrieval via the brv CLI."
external_dependencies:
  - name: brv
    install: "curl -fsSL https://byterover.dev/install.sh | sh"
    check: "brv --version"
config_keys:
  - memory.byterover
  - memory.provider_config  # legacy block name early docs used
`

const (
	brvQueryTimeout  = 10 * time.Second
	brvCurateTimeout = 120 * time.Second
	brvStatusTimeout = 15 * time.Second
	brvMinQueryLen   = 10
	brvMinOutputLen  = 20
)

var boolWords = map[string]bool{"1": true, "true": true, "yes": true, "on": true, "0": false, "false": false, "no": false, "off": false}

// Truthiness of bools and numbers, common true and false words parsed, anything else the default
func coerceBool(value any, def bool) bool {
	switch v := value.(type) {
	case bool:
		return v
	case int:
		return v != 0
	case int64:
		return v != 0
	case float64:
		return v != 0
	case string:
		if b, ok := boolWords[pyLower(pyStrip(v))]; ok {
			return b
		}
	}
	return def
}

// The memory.byterover block, else the legacy memory.provider_config block, empty on error
func loadByteRoverConfig(ctx context.Context, env *tools.Env) map[string]any {
	tree, err := registryConfig(ctx, env)
	if err != nil || tree == nil {
		return map[string]any{}
	}
	memory, ok := tree.Get("memory").(map[string]any)
	if !ok {
		if _, present := tree.Lookup("memory"); present {
			return map[string]any{}
		}
		memory = map[string]any{}
	}
	for _, key := range []string{"byterover", "provider_config"} {
		block, isMap := memory[key].(map[string]any)
		if _, present := memory[key]; !present {
			block, isMap = map[string]any{}, true
		}
		if isMap && (len(block) > 0 || key == "provider_config") {
			out := make(map[string]any, len(block))
			for k, v := range block {
				out[k] = v
			}
			return out
		}
	}
	return map[string]any{}
}

// Resolved brv binary, nil before resolution and empty once resolved missing
var (
	brvPathMu     sync.Mutex
	cachedBrvPath *string
)

// The brv binary on PATH or a well known install location, resolved once with the lookup outside the lock
func resolveBrvPath() string {
	brvPathMu.Lock()
	if cachedBrvPath != nil {
		p := *cachedBrvPath
		brvPathMu.Unlock()
		return p
	}
	brvPathMu.Unlock()
	found, err := exec.LookPath("brv")
	if err != nil {
		found = ""
		userHome, _ := os.UserHomeDir()
		for _, c := range []string{filepath.Join(userHome, ".brv-cli", "bin", "brv"), "/usr/local/bin/brv", filepath.Join(userHome, ".npm-global", "bin", "brv")} {
			if _, err := os.Stat(c); err == nil {
				found = c
				break
			}
		}
	}
	brvPathMu.Lock()
	defer brvPathMu.Unlock()
	if cachedBrvPath == nil {
		cachedBrvPath = &found
	}
	return *cachedBrvPath
}

func forgetBrvPath() {
	brvPathMu.Lock()
	cachedBrvPath = nil
	brvPathMu.Unlock()
}

// Profile scoped working directory of the brv context tree
func brvCwd(ctx context.Context, env *tools.Env) string {
	return filepath.Join(registryHome(ctx, env), "byterover")
}

// Child env for brv bridged to the effective home, BRV values read through the served profile scope when multiplexed
func brvChildEnv(ctx context.Context, env *tools.Env, brvPath string) (map[string]string, error) {
	scoped := &tools.Env{}
	if env != nil {
		copied := *env
		scoped = &copied
	}
	scoped.Home = registryHome(ctx, env)
	child, err := tools.BuildSubprocessEnv(ctx, scoped, tools.SubprocessEnvOptions{InheritProfileHome: true})
	if err != nil {
		return nil, err
	}
	if provider.IsMultiplexActive(ctx) {
		if override := home.Override(ctx); override != "" {
			child = tools.StripLaunchProfileEnv(scoped, child, override)
		}
		for k := range child {
			if strings.HasPrefix(k, "BRV_") {
				delete(child, k)
			}
		}
		apiKey, _, err := provider.GetSecret(ctx, "BRV_API_KEY")
		var unscoped *provider.UnscopedSecretError
		if err != nil && !errors.As(err, &unscoped) {
			return nil, err
		}
		if err != nil {
			apiKey = ""
		}
		if apiKey != "" {
			child["BRV_API_KEY"] = apiKey
		}
	}
	child["PATH"] = filepath.Dir(brvPath) + string(os.PathListSeparator) + child["PATH"]
	return child, nil
}

// Outcome of one brv command, output on success and error otherwise
type brvResult struct {
	Success bool
	Output  string
	Error   string
}

// Bytes decoded as UTF-8 with each maximal ill formed subpart replaced by one replacement character
func decodeReplace(b []byte) string {
	var out strings.Builder
	for i := 0; i < len(b); {
		c := b[i]
		if c < 0x80 {
			out.WriteByte(c)
			i++
			continue
		}
		size, lo, hi := 0, byte(0x80), byte(0xBF)
		switch {
		case c >= 0xC2 && c <= 0xDF:
			size = 2
		case c == 0xE0:
			size, lo = 3, 0xA0
		case c >= 0xE1 && c <= 0xEC, c == 0xEE, c == 0xEF:
			size = 3
		case c == 0xED:
			size, hi = 3, 0x9F
		case c == 0xF0:
			size, lo = 4, 0x90
		case c >= 0xF1 && c <= 0xF3:
			size = 4
		case c == 0xF4:
			size, hi = 4, 0x8F
		}
		if size == 0 {
			out.WriteRune(utf8.RuneError)
			i++
			continue
		}
		valid := 0
		for k := 1; k < size && i+k < len(b); k++ {
			min, max := byte(0x80), byte(0xBF)
			if k == 1 {
				min, max = lo, hi
			}
			if b[i+k] < min || b[i+k] > max {
				break
			}
			valid++
		}
		if valid == size-1 {
			r, _ := utf8.DecodeRune(b[i : i+size])
			out.WriteRune(r)
			i += size
			continue
		}
		out.WriteRune(utf8.RuneError)
		i += 1 + valid
	}
	return out.String()
}

// OS error carrying the Python OSError text
func pyOSError(err error) error {
	return &pyError{class: "OSError", msg: osErrorText(err)}
}

// Runs one brv command in the context tree directory with a bounded timeout, a non string argument refused like subprocess
func runBrv(ctx context.Context, env *tools.Env, args []any, timeout time.Duration, cwd string) (brvResult, error) {
	brvPath := resolveBrvPath()
	if brvPath == "" {
		return brvResult{Error: "brv CLI not found. Install: npm install -g byterover-cli"}, nil
	}
	effective := cwd
	if effective == "" {
		effective = brvCwd(ctx, env)
	}
	if err := os.MkdirAll(effective, 0o777); err != nil {
		return brvResult{}, pyOSError(err)
	}
	childEnv, err := brvChildEnv(ctx, env, brvPath)
	if err != nil {
		return brvResult{}, err
	}
	argv := make([]string, len(args))
	for i, a := range args {
		s, ok := a.(string)
		if !ok {
			return brvResult{Error: fmt.Sprintf("expected str, bytes or os.PathLike object, not %s", pyTypeName(a))}, nil
		}
		argv[i] = s
	}
	runCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancel()
	cmd := exec.CommandContext(runCtx, brvPath, argv...)
	cmd.Dir = effective
	cmd.Env = envList(childEnv)
	var stdout, stderr bytes.Buffer
	cmd.Stdout, cmd.Stderr = &stdout, &stderr
	runErr := cmd.Run()
	if runCtx.Err() == context.DeadlineExceeded {
		return brvResult{Error: fmt.Sprintf("brv timed out after %ds", int(timeout.Seconds()))}, nil
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			if errors.Is(runErr, fs.ErrNotExist) {
				forgetBrvPath()
				return brvResult{Error: "brv CLI not found"}, nil
			}
			return brvResult{Error: osErrorText(runErr)}, nil
		}
	}
	out := pyStrip(decodeReplace(stdout.Bytes()))
	errText := pyStrip(decodeReplace(stderr.Bytes()))
	code := cmd.ProcessState.ExitCode()
	if code == 0 {
		return brvResult{Success: true, Output: out}, nil
	}
	switch {
	case errText != "":
		return brvResult{Error: errText}, nil
	case out != "":
		return brvResult{Error: out}, nil
	}
	return brvResult{Error: fmt.Sprintf("brv exited %d", code)}, nil
}

// Environment map as sorted KEY=value pairs
func envList(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k, v := range m {
		out = append(out, k+"="+v)
	}
	sort.Strings(out)
	return out
}

func byteroverSchema(name, description, arg, argDesc string) map[string]any {
	props := map[string]any{}
	required := []any{}
	if arg != "" {
		props[arg] = map[string]any{"type": "string", "description": argDesc}
		required = append(required, arg)
	}
	return map[string]any{"name": name, "description": description, "parameters": map[string]any{
		"type": "object", "properties": props, "required": required,
	}}
}

func byteroverSchemas() []map[string]any {
	return []map[string]any{
		byteroverSchema("brv_query", "Search ByteRover's persistent knowledge tree for relevant context. Returns memories, "+
			"project knowledge, architectural decisions, and patterns from previous sessions. Use for any question where past "+
			"context would help.", "query", "What to search for."),
		byteroverSchema("brv_curate", "Store important information in ByteRover's persistent knowledge tree. Use for "+
			"architectural decisions, bug fixes, user preferences, project patterns — anything worth remembering across "+
			"sessions. ByteRover's LLM automatically categorizes and organizes the memory.", "content", "The information to remember."),
		byteroverSchema("brv_status", "Check ByteRover status — CLI version, context tree stats, cloud sync state.", "", ""),
	}
}

// ByteRover persistent memory through the brv CLI
type ByteRoverProvider struct {
	BaseProvider
	env         *tools.Env
	config      map[string]any
	autoExtract bool

	mu         sync.Mutex
	cwd        string
	sessionID  string
	turnCount  int
	syncThread *ContextThread
}

// Provider over config, the memory.byterover block when config is nil
func NewByteRoverProvider(ctx context.Context, env *tools.Env, config map[string]any) *ByteRoverProvider {
	cfg := map[string]any{}
	if config != nil {
		for k, v := range config {
			cfg[k] = v
		}
	} else {
		cfg = loadByteRoverConfig(ctx, env)
	}
	return &ByteRoverProvider{env: env, config: cfg, autoExtract: coerceBool(cfg["auto_extract"], true)}
}

func (p *ByteRoverProvider) Name() string { return "byterover" }

// Whether the brv CLI is installed, no network
func (p *ByteRoverProvider) Available(ctx context.Context) bool { return resolveBrvPath() != "" }

func (p *ByteRoverProvider) ConfigSchema() []map[string]any {
	return []map[string]any{
		{"key": "api_key", "description": "ByteRover API key (optional, for cloud sync)", "secret": true,
			"env_var": "BRV_API_KEY", "url": "https://app.byterover.dev"},
		{"key": "auto_extract", "description": "Automatically curate completed turns and compression/memory hooks",
			"default": "true", "choices": []any{"true", "false"}},
	}
}

// Binds the context tree to the profile the manager names
func (p *ByteRoverProvider) Initialize(ctx context.Context, sessionID string, opts InitOptions) error {
	cwd := brvCwd(ctx, p.env)
	if opts.Home != "" {
		cwd = filepath.Join(opts.Home, "byterover")
	}
	p.mu.Lock()
	p.cwd, p.sessionID, p.turnCount = cwd, sessionID, 0
	p.mu.Unlock()
	if err := os.MkdirAll(cwd, 0o777); err != nil {
		return pyOSError(err)
	}
	return nil
}

func (p *ByteRoverProvider) SystemPromptBlock(ctx context.Context) (string, error) {
	if resolveBrvPath() == "" {
		return "", nil
	}
	return "# ByteRover Memory\nActive. Persistent knowledge tree with hierarchical context.\n" +
		"Use brv_query to search past knowledge, brv_curate to store important facts, brv_status to check state.", nil
}

func (p *ByteRoverProvider) boundCwd() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.cwd
}

func (p *ByteRoverProvider) query(ctx context.Context, query string) (brvResult, error) {
	return runBrv(ctx, p.env, []any{"query", "--", runePrefix(pyStrip(query), 5000)}, brvQueryTimeout, p.boundCwd())
}

func (p *ByteRoverProvider) curate(ctx context.Context, content any) (brvResult, error) {
	return runBrv(ctx, p.env, []any{"curate", "--", content}, brvCurateTimeout, p.boundCwd())
}

// Background curate of content, failures logged at debug and never raised
func (p *ByteRoverProvider) curateInBackground(ctx context.Context, content, name, what, onDone string) *ContextThread {
	t := SpawnContextThread(ctx, name, func(tctx context.Context) {
		err := callProvider(func() error {
			_, err := p.curate(tctx, content)
			return err
		})
		if err != nil {
			envLog(p.env).Debug(fmt.Sprintf("ByteRover %s failed: %s", what, err))
			return
		}
		if onDone != "" {
			envLog(p.env).Info(onDone)
		}
	})
	t.Start()
	return t
}

func (p *ByteRoverProvider) autoExtractEnabled(what string) bool {
	if !p.autoExtract {
		envLog(p.env).Debug(fmt.Sprintf("ByteRover %s skipped (auto_extract disabled)", what))
	}
	return p.autoExtract
}

// Synchronous brv query so context is ready before the first model call
func (p *ByteRoverProvider) Prefetch(ctx context.Context, query, sessionID string) (string, error) {
	if query == "" || runeLen(pyStrip(query)) < brvMinQueryLen {
		return "", nil
	}
	result, err := p.query(ctx, query)
	if err != nil {
		return "", err
	}
	output := ""
	if result.Success {
		output = pyStrip(result.Output)
	}
	if runeLen(output) > brvMinOutputLen {
		return "## ByteRover Context\n" + output, nil
	}
	return "", nil
}

// Curates a substantive turn in the background after the previous curate finished
func (p *ByteRoverProvider) SyncTurn(ctx context.Context, userContent, assistantContent string, opts SyncOptions) error {
	p.mu.Lock()
	p.turnCount++
	p.mu.Unlock()
	if !p.autoExtractEnabled("sync_turn") || runeLen(pyStrip(userContent)) < brvMinQueryLen {
		return nil
	}
	p.mu.Lock()
	previous := p.syncThread
	p.mu.Unlock()
	if previous != nil && previous.Alive() {
		previous.Join(5 * time.Second)
	}
	t := p.curateInBackground(ctx, "User: "+runePrefix(userContent, 2000)+"\nAssistant: "+runePrefix(assistantContent, 2000), "brv-sync", "sync", "")
	p.mu.Lock()
	p.syncThread = t
	p.mu.Unlock()
	return nil
}

// Mirrors built in memory adds and replaces into ByteRover
func (p *ByteRoverProvider) OnMemoryWrite(ctx context.Context, action, target, content string, metadata map[string]any) error {
	if p.autoExtractEnabled("memory mirror") && (action == "add" || action == "replace") && content != "" {
		label := "Agent memory"
		if target == "user" {
			label = "User profile"
		}
		p.curateInBackground(ctx, "["+label+"] "+content, "brv-memwrite", "memory mirror", "")
	}
	return nil
}

// Curates the last ten user and assistant messages before compression discards them
func (p *ByteRoverProvider) OnPreCompress(ctx context.Context, messages []map[string]any, requireCheckpoint bool) (string, error) {
	if !p.autoExtractEnabled("pre-compression flush") || len(messages) == 0 {
		return "", nil
	}
	tail := messages
	if len(tail) > 10 {
		tail = tail[len(tail)-10:]
	}
	var parts []string
	for _, msg := range tail {
		role, _ := msg["role"].(string)
		content, isStr := msg["content"].(string)
		if _, present := msg["content"]; !present {
			content, isStr = "", true
		}
		if (role == "user" || role == "assistant") && isStr && pyStrip(content) != "" {
			parts = append(parts, role+": "+runePrefix(content, 500))
		}
	}
	if len(parts) > 0 {
		p.curateInBackground(ctx, "[Pre-compression context]\n"+strings.Join(parts, "\n"), "brv-flush", "pre-compression flush",
			fmt.Sprintf("ByteRover pre-compression flush: %d messages", len(parts)))
	}
	return "", nil
}

func (p *ByteRoverProvider) ToolSchemas(ctx context.Context) ([]map[string]any, error) {
	return byteroverSchemas(), nil
}

// Query output trimmed, a no result note under the minimum and truncated past eight thousand characters
func formatBrvQueryOutput(output string) map[string]any {
	output = pyStrip(output)
	if runeLen(output) < brvMinOutputLen {
		return map[string]any{"result": "No relevant memories found."}
	}
	if runeLen(output) > 8000 {
		return map[string]any{"result": runePrefix(output, 8000) + "\n\n[... truncated]"}
	}
	return map[string]any{"result": output}
}

// One of the brv tools, a missing required argument refused before the CLI runs
func (p *ByteRoverProvider) HandleToolCall(ctx context.Context, toolName string, args map[string]any, opts ToolCallOptions) (string, error) {
	var (
		arg, failMsg string
		run          func(value any) (brvResult, error)
		onOK         func(output string) map[string]any
	)
	switch toolName {
	case "brv_query":
		arg, failMsg = "query", "Query failed"
		run = func(value any) (brvResult, error) {
			q, ok := value.(string)
			if !ok {
				return brvResult{}, fmt.Errorf("'%s' object has no attribute 'strip'", pyTypeName(value))
			}
			return p.query(ctx, q)
		}
		onOK = formatBrvQueryOutput
	case "brv_curate":
		arg, failMsg = "content", "Curate failed"
		run = func(value any) (brvResult, error) { return p.curate(ctx, value) }
		onOK = func(string) map[string]any { return map[string]any{"result": "Memory curated successfully."} }
	case "brv_status":
		failMsg = "Status check failed"
		run = func(any) (brvResult, error) {
			return runBrv(ctx, p.env, []any{"status"}, brvStatusTimeout, p.boundCwd())
		}
		onOK = func(out string) map[string]any { return map[string]any{"status": out} }
	default:
		return toolError("Unknown tool: "+toolName, nil), nil
	}
	var value any
	if arg != "" {
		v, present := args[arg]
		if !present {
			v = ""
		}
		if !pyTruthy(v) {
			return toolError(arg+" is required", nil), nil
		}
		value = v
	}
	result, err := run(value)
	if err != nil {
		return "", err
	}
	if result.Success {
		return tools.JSON(onOK(result.Output)), nil
	}
	if result.Error == "" {
		return toolError(failMsg, nil), nil
	}
	return toolError(result.Error, nil), nil
}

// Waits for the last turn curate to finish
func (p *ByteRoverProvider) Shutdown(ctx context.Context) error {
	p.mu.Lock()
	t := p.syncThread
	p.mu.Unlock()
	if t != nil && t.Alive() {
		t.Join(10 * time.Second)
	}
	return nil
}

// Registers ByteRover as a bundled memory provider
func registerByteRoverProvider() {
	RegisterProvider(ProviderRegistration{
		Name:     "byterover",
		Source:   SourceBundled,
		Manifest: byteroverManifest,
		New: func(ctx context.Context, env *tools.Env, opts LoadOptions) (Provider, error) {
			return NewByteRoverProvider(ctx, env, nil), nil
		},
	})
}
