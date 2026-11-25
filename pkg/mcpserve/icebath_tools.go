package mcpserve

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/plugins"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
	anyutil "github.com/nickheyer/icebath/pkg/utils/any"
	jsonutil "github.com/nickheyer/icebath/pkg/utils/json"
	regexutil "github.com/nickheyer/icebath/pkg/utils/regex"
)

// Fixed allowlist --tools offers, never terminal, file, code execution, process, kanban or delegation tools
var ExposedTools = []string{
	"web_search", "web_extract",
	"browser_navigate", "browser_click", "browser_type", "browser_press", "browser_snapshot", "browser_scroll",
	"browser_back", "browser_get_images", "browser_console", "browser_vision",
	"vision_analyze", "image_generate", "skill_view", "skills_list", "text_to_speech",
}

// Arguments an MCP client may not set because they write outside the icebath cache
var droppedArgs = map[string][]string{"text_to_speech": {"output_path"}}

// Argument that must hold web URLs and whether data URLs pass too
type urlArg struct {
	arg       string
	allowData bool
}

var urlArgs = map[string]urlArg{
	"browser_navigate": {"url", false}, "web_extract": {"urls", false}, "vision_analyze": {"image_url", true},
}

var schemeRE = regexutil.Lazy(regexp.MustCompile, `^[\s\v\x{85}\p{Z}]*([a-zA-Z][a-zA-Z0-9+.-]*):(?:\P{Nd}|$)`)

// Reports whether a tool drops the named argument
func isDropped(toolName, arg string) bool {
	for _, a := range droppedArgs[toolName] {
		if a == arg {
			return true
		}
	}
	return false
}

// Reports an http or https URL or a bare host with an optional port, data URLs only when allowed
func isWebURL(value any, allowData bool) bool {
	text := ""
	if anyutil.IsTrue(value) {
		text = anyutil.String(value)
	}
	m := schemeRE().FindStringSubmatch(text)
	if m == nil {
		rest := strings.TrimLeftFunc(text, unicode.IsSpace)
		return strings.TrimSpace(text) != "" && !strings.HasPrefix(rest, "/") && !strings.HasPrefix(rest, "~") &&
			!strings.HasPrefix(rest, "\\") && !strings.HasPrefix(rest, ".")
	}
	scheme := strings.ToLower(m[1])
	return scheme == "http" || scheme == "https" || (allowData && scheme == "data")
}

// Why args may not reach the tool from an MCP client, empty when they may
func refusal(toolName string, args map[string]any) string {
	ua, ok := urlArgs[toolName]
	if !ok {
		return ""
	}
	value := args[ua.arg]
	if value == nil {
		return ""
	}
	values, isList := value.([]any)
	if !isList {
		values = []any{value}
	}
	for _, v := range values {
		if !isWebURL(v, ua.allowData) {
			kinds := "http(s) URLs"
			if ua.allowData {
				kinds = "http(s) or data: URLs"
			}
			return fmt.Sprintf("%s over MCP accepts only %s, not local files: %s", toolName, kinds, anyutil.Quote(v))
		}
	}
	return ""
}

// Handler forwarding set arguments to the registry, refusing local file URLs and reporting a raised error as JSON
func makeToolHandler(ctx context.Context, env *tools.Env, toolName string) func(kw *jsonutil.Object) string {
	return func(kw *jsonutil.Object) (result string) {
		args := map[string]any{}
		for _, k := range kw.Keys() {
			if v := kw.Value(k); v != nil && !isDropped(toolName, k) {
				args[k] = v
			}
		}
		if reason := refusal(toolName, args); reason != "" {
			return dumps(jsonutil.ObjectOf("error", reason, "tool", toolName), false)
		}
		defer func() {
			if r := recover(); r != nil {
				pkgLog().Error(fmt.Sprintf("tool %s raised\n%s", toolName, raisedText(r)))
				result = dumps(jsonutil.ObjectOf("error", raisedText(r), "tool", toolName), false)
			}
		}()
		return tools.NewDispatcher(tools.Default).Dispatch(ctx, &tools.Call{Name: toolName, Args: args, Env: env})
	}
}

// Tool environment of the home ctx names, its config loaded with secrets resolved
func toolEnv(ctx context.Context) (*tools.Env, error) {
	h := home.Dir(ctx)
	secrets := config.NewSecrets(h)
	tree, err := config.Load(h, secrets.Lookup())
	if err != nil {
		return nil, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return nil, err
	}
	return &tools.Env{Home: h, Config: tree, Secrets: secrets, Cwd: cwd}, nil
}

// One available exposed tool and its function spec with the dropped arguments removed
type toolSpec struct {
	name string
	spec *jsonutil.Object
}

// Removes key from a dict value in place, raising what d.pop(key, None) raises for a value that is no dict
func dictPop(d any, key string) {
	switch t := d.(type) {
	case *jsonutil.Object:
		t.Delete(key)
	case map[string]any:
		delete(t, key)
	case []any:
		panic(&kindError{"TypeError", "pop expected at most 1 argument, got 2"})
	default:
		noAttribute(d, "pop")
	}
}

// Function specs of every exposed tool this process can dispatch within the toolsets the profile enables for the CLI
func availableToolSpecs(ctx context.Context, env *tools.Env) []toolSpec {
	enabled := tools.Default.PlatformTools(ctx, tools.ScopeKey(env.Home), env.Config, "cli", tools.PlatformToolsOptions{IncludeDefaultMCPServers: false})
	toolsets := make([]string, 0, len(enabled))
	for name, on := range enabled {
		if on {
			toolsets = append(toolsets, name)
		}
	}
	sort.Strings(toolsets)
	defs := map[string]any{}
	for _, td := range tools.Default.ToolDefinitions(ctx, env, tools.DefinitionOptions{Enabled: toolsets, Quiet: true}) {
		if isStr(td["type"], "function") {
			fn := elementAt(td, "function")
			defs[anyutil.String(elementAt(fn, "name"))] = fn
		}
	}
	specs := []toolSpec{}
	for _, name := range ExposedTools {
		def, ok := defs[name]
		if !ok {
			continue
		}
		spec, isDict := asDict(jsonutil.Clone(def))
		if !isDict {
			noAttribute(def, "setdefault")
		}
		if _, has := spec.Get("parameters"); !has {
			spec.Set("parameters", jsonutil.ObjectOf("type", "object", "properties", jsonutil.NewObject()))
		}
		params := spec.Value("parameters")
		for _, arg := range droppedArgs[name] {
			if props := lookup(params, "properties", nil); anyutil.IsTrue(props) {
				dictPop(props, arg)
			}
			if required, ok := lookup(params, "required", nil).([]any); ok {
				for i, r := range required {
					if isStr(r, arg) {
						required = append(required[:i:i], required[i+1:]...)
						if o, isObj := params.(*jsonutil.Object); isObj {
							o.Set("required", required)
						} else {
							params.(map[string]any)["required"] = required
						}
						break
					}
				}
			}
		}
		specs = append(specs, toolSpec{name: name, spec: spec})
	}
	return specs
}

// Registers every available exposed tool not in reserved after the bridge tools and returns how many it added
func registerIcebathTools(ctx context.Context, server *provider.MCPToolServer, reserved map[string]bool) (int, error) {
	env, err := toolEnv(ctx)
	if err != nil {
		return 0, err
	}
	count := 0
	for _, s := range availableToolSpecs(ctx, env) {
		if reserved[s.name] {
			continue
		}
		description := fmt.Sprintf("Icebath %s tool", s.name)
		if d := s.spec.Value("description"); anyutil.IsTrue(d) {
			description = anyutil.String(d)
		}
		params := s.spec.Value("parameters")
		if !anyutil.IsTrue(params) {
			params = jsonutil.ObjectOf("type", "object", "properties", jsonutil.NewObject())
		}
		server.AddTool(s.name, description, provider.MCPToolParamsFromSchema(params), params, makeToolHandler(ctx, env, s.name))
		count++
	}
	pkgLog().Info(fmt.Sprintf("registered %d/%d Icebath tools", count, len(ExposedTools)))
	return count, nil
}

// Process setup for --tools, redacted output, the profile shell hooks and outbound webhooks registered
func prepareToolProcess(ctx context.Context, acceptHooks bool) error {
	tools.ForceSecretRedaction()
	if _, err := plugins.RegisterShellHooksFromConfig(ctx, acceptHooks, false); err != nil {
		return err
	}
	_, err := plugins.RegisterOutboundWebhooksFromConfig(ctx)
	return err
}
