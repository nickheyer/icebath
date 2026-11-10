package provider

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"math"
	"math/big"
	"os"
	"os/signal"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyjson"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/tools"
)

// CodexToolsMCPServerName is the mcp_servers key the runtime migration registers the icebath tools server under
const CodexToolsMCPServerName = "icebath-tools"

// CodexToolsMCPServerEntry is the argv[1] that turns the icebath binary into the tools MCP server
const CodexToolsMCPServerEntry = "__icebath_tools_mcp_server__"

const codexToolsInstructions = "Icebath Agent's tool surface, exposed for use inside a Codex " +
	"session. Use these for capabilities Codex's built-in toolset " +
	"doesn't cover: web search/extract, " +
	"subagent delegation, vision, image generation, persistent " +
	"memory, skills, and cross-session search."

// Handshake revisions the server echoes, newest last, and the stateless discover revision
var codexToolsHandshakeVersions = []string{"2024-11-05", "2025-03-26", "2025-06-18", "2025-11-25"}

const codexToolsDiscoverVersion = "2026-07-28"

// Major and minor of the pydantic release whose error documentation links the validation text cites
const codexToolsPydanticDocsVersion = "2.14"

var codexToolsJSONToPy = map[string]string{"string": "str", "integer": "int", "number": "float", "boolean": "bool", "array": "list", "object": "dict"}

// codexToolsExposedTools is the curated tool subset codex reaches through the server, never its own builtins
var codexToolsExposedTools = []string{
	"web_search", "web_extract",
	"vision_analyze", "image_generate", "skill_view", "skills_list", "text_to_speech",
	"kanban_complete", "kanban_block", "kanban_schedule", "kanban_request_review",
	"kanban_request_changes", "kanban_comment",
	"kanban_heartbeat", "kanban_show", "kanban_list",
	"kanban_create", "kanban_unblock", "kanban_link",
}

// CodexToolsExposedTools returns a copy of the curated tool names the codex tools server exposes
func CodexToolsExposedTools() []string { return slices.Clone(codexToolsExposedTools) }

// One keyword only parameter of a handler signature, an optional one defaults to None
type codexToolsParam struct {
	Name     string
	PyType   string
	Required bool
}

// Annotation renders the parameter annotation the way typing prints it
func (p codexToolsParam) Annotation() string {
	if p.Required {
		return p.PyType
	}
	return "Optional[" + p.PyType + "]"
}

type codexToolsSignature struct {
	Params           []codexToolsParam
	ReturnAnnotation string
}

// Property names of a schema in wire order for a decoded object and sorted for a plain map
func codexToolsPropertyNames(props any) []string {
	switch p := props.(type) {
	case *pyjson.Object:
		return slices.Clone(p.Keys)
	case map[string]any:
		names := make([]string, 0, len(p))
		for k := range p {
			names = append(names, k)
		}
		sort.Strings(names)
		return names
	}
	return nil
}

// Builds the keyword only signature and annotations of a JSON schema with optional params defaulting to None
func codexToolsSignatureFromSchema(schema any) (codexToolsSignature, map[string]string) {
	props := cxOr(cxGet(cxOr(schema, map[string]any{}), "properties"), map[string]any{})
	required := map[string]bool{}
	for _, r := range cxList(cxOr(cxGet(cxOr(schema, map[string]any{}), "required"), []any{})) {
		required[pyrepr.Str(r)] = true
	}
	sig := codexToolsSignature{ReturnAnnotation: "str"}
	annots := map[string]string{}
	for _, name := range codexToolsPropertyNames(props) {
		if strings.HasPrefix(name, "_") {
			continue
		}
		py := "Any"
		if t, ok := cxGet(cxOr(cxGet(props, name), map[string]any{}), "type").(string); ok && codexToolsJSONToPy[t] != "" {
			py = codexToolsJSONToPy[t]
		}
		p := codexToolsParam{Name: name, PyType: py, Required: required[name]}
		annots[name] = p.Annotation()
		sig.Params = append(sig.Params, p)
	}
	return sig, annots
}

// Pydantic title of a field or model name
func codexToolsTitle(name string) string {
	return strings.TrimSpace(strings.ReplaceAll(pystr.Title(name), "_", " "))
}

// JSON schema pydantic emits for one bare Python type, keys already in sorted order
func codexToolsTypeSchema(py string) *pyjson.Object {
	o := pyjson.NewObject()
	switch py {
	case "str":
		o.Set("type", "string")
	case "int":
		o.Set("type", "integer")
	case "float":
		o.Set("type", "number")
	case "bool":
		o.Set("type", "boolean")
	case "list":
		o.Set("items", pyjson.NewObject())
		o.Set("type", "array")
	case "dict":
		o.Set("additionalProperties", true)
		o.Set("type", "object")
	}
	return o
}

// Pydantic model JSON schema of a signature, the input schema FastMCP derives for a handler
func codexToolsInputSchema(toolName string, sig codexToolsSignature) *pyjson.Object {
	props := pyjson.NewObject()
	var required []any
	for _, p := range sig.Params {
		field := pyjson.NewObject()
		inner := codexToolsTypeSchema(p.PyType)
		if p.Required {
			for _, k := range inner.Keys {
				field.Set(k, inner.Vals[k])
			}
			field.Set("title", codexToolsTitle(p.Name))
			field.Keys = codexSortedKeys(field.Keys)
			required = append(required, p.Name)
		} else {
			null := pyjson.NewObject()
			null.Set("type", "null")
			field.Set("anyOf", []any{inner, null})
			field.Set("default", nil)
			field.Set("title", codexToolsTitle(p.Name))
		}
		props.Set(p.Name, field)
	}
	schema := pyjson.NewObject()
	schema.Set("properties", props)
	if len(required) > 0 {
		schema.Set("required", required)
	}
	schema.Set("title", toolName+"Arguments")
	schema.Set("type", "object")
	return schema
}

func codexSortedKeys(keys []string) []string {
	out := slices.Clone(keys)
	sort.Strings(out)
	return out
}

// Wrapped output schema FastMCP derives for a handler annotated to return str
func codexToolsOutputSchema(toolName string) *pyjson.Object {
	result := pyjson.NewObject()
	result.Set("title", "Result")
	result.Set("type", "string")
	props := pyjson.NewObject()
	props.Set("result", result)
	schema := pyjson.NewObject()
	schema.Set("properties", props)
	schema.Set("required", []any{"result"})
	schema.Set("title", toolName+"Output")
	schema.Set("type", "object")
	return schema
}

// One pydantic line error of argument validation
type codexToolsLineError struct {
	loc, kind, msg string
	input          any
}

var codexToolsErrorMessages = map[string]string{
	"missing":        "Field required",
	"string_type":    "Input should be a valid string",
	"int_type":       "Input should be a valid integer",
	"int_parsing":    "Input should be a valid integer, unable to parse string as an integer",
	"int_from_float": "Input should be a valid integer, got a number with a fractional part",
	"float_type":     "Input should be a valid number",
	"float_parsing":  "Input should be a valid number, unable to parse string as a number",
	"finite_number":  "Input should be a finite number",
	"bool_type":      "Input should be a valid boolean",
	"bool_parsing":   "Input should be a valid boolean, unable to interpret input",
	"list_type":      "Input should be a valid list",
	"dict_type":      "Input should be a valid dictionary",
}

// Pydantic ValidationError text with the input value repr truncated the way pydantic-core prints it
type codexToolsValidationError struct {
	title  string
	errors []codexToolsLineError
}

func (e *codexToolsValidationError) Error() string {
	var b strings.Builder
	plural := ""
	if len(e.errors) != 1 {
		plural = "s"
	}
	fmt.Fprintf(&b, "%d validation error%s for %s", len(e.errors), plural, e.title)
	for _, le := range e.errors {
		r := []rune(pyrepr.Repr(le.input))
		value := string(r)
		if len(r) > 50 {
			value = string(r[:25]) + "..." + string(r[len(r)-24:])
		}
		fmt.Fprintf(&b, "\n%s\n  %s [type=%s, input_value=%s, input_type=%s]\n    For further information visit https://errors.pydantic.dev/%s/v/%s",
			le.loc, codexToolsErrorMessages[le.kind], le.kind, value, pyrepr.TypeName(le.input), codexToolsPydanticDocsVersion, le.kind)
	}
	return b.String()
}

// Lax mode pydantic coercion of one value to a bare Python type, or the error type it raises
func codexToolsCoerce(py string, v any) (any, string) {
	_, isBool := v.(bool)
	switch py {
	case "Any":
		return v, ""
	case "str":
		if s, ok := v.(string); ok {
			return s, ""
		}
		return nil, "string_type"
	case "int":
		if isBool {
			if v.(bool) {
				return int64(1), ""
			}
			return int64(0), ""
		}
		if n, ok := pyrepr.Int(v); ok {
			return pyjson.ParseInt(n.String()), ""
		}
		if f, ok := v.(float64); ok {
			if math.IsNaN(f) || math.IsInf(f, 0) {
				return nil, "finite_number"
			}
			if f != math.Trunc(f) {
				return nil, "int_from_float"
			}
			return pyjson.ParseInt(strconv.FormatFloat(f, 'f', 0, 64)), ""
		}
		if s, ok := v.(string); ok {
			t := strings.TrimSpace(s)
			if n, ok := codexToolsParseIntText(t); ok {
				return n, ""
			}
			if f, err := strconv.ParseFloat(t, 64); err == nil && f == math.Trunc(f) && !math.IsInf(f, 0) {
				return pyjson.ParseInt(strconv.FormatFloat(f, 'f', 0, 64)), ""
			}
			return nil, "int_parsing"
		}
		return nil, "int_type"
	case "float":
		if isBool {
			if v.(bool) {
				return 1.0, ""
			}
			return 0.0, ""
		}
		if f, ok := v.(float64); ok {
			return f, ""
		}
		if n, ok := pyrepr.Int(v); ok {
			f, _ := new(big.Float).SetInt(n).Float64()
			return f, ""
		}
		if s, ok := v.(string); ok {
			if f, err := strconv.ParseFloat(strings.ReplaceAll(strings.TrimSpace(s), "_", ""), 64); err == nil {
				return f, ""
			}
			return nil, "float_parsing"
		}
		return nil, "float_type"
	case "bool":
		if isBool {
			return v, ""
		}
		if n, ok := pyrepr.Int(v); ok {
			if n.IsInt64() && (n.Int64() == 0 || n.Int64() == 1) {
				return n.Int64() == 1, ""
			}
			return nil, "bool_parsing"
		}
		if f, ok := v.(float64); ok {
			if f == 0 || f == 1 {
				return f == 1, ""
			}
			return nil, "bool_parsing"
		}
		if s, ok := v.(string); ok {
			switch strings.ToLower(strings.TrimSpace(s)) {
			case "0", "off", "f", "false", "n", "no":
				return false, ""
			case "1", "on", "t", "true", "y", "yes":
				return true, ""
			}
			return nil, "bool_parsing"
		}
		return nil, "bool_type"
	case "list":
		if l, ok := v.([]any); ok {
			return l, ""
		}
		return nil, "list_type"
	case "dict":
		if cxIsObject(v) {
			return v, ""
		}
		return nil, "dict_type"
	}
	return v, ""
}

// Python int() text parsing with sign and underscores between digits
func codexToolsParseIntText(t string) (any, bool) {
	body := strings.TrimLeft(t, "+-")
	if len(t)-len(body) > 1 || body == "" || strings.HasPrefix(body, "_") || strings.HasSuffix(body, "_") || strings.Contains(body, "__") {
		return nil, false
	}
	for _, r := range body {
		if (r < '0' || r > '9') && r != '_' {
			return nil, false
		}
	}
	return pyjson.ParseInt(strings.TrimPrefix(strings.ReplaceAll(t, "_", ""), "+")), true
}

// FastMCP pre parse that decodes JSON text sent for a field not annotated as plain str
func codexToolsPreParse(sig codexToolsSignature, args *pyjson.Object) *pyjson.Object {
	out := pyjson.NewObject()
	for _, k := range args.Keys {
		out.Set(k, args.Vals[k])
	}
	for _, p := range sig.Params {
		s, ok := args.Vals[p.Name].(string)
		if !ok || p.Annotation() == "str" {
			continue
		}
		parsed, err := pyjson.Loads(s)
		if err != nil {
			continue
		}
		switch parsed.(type) {
		case string, float64, bool:
			continue
		}
		if _, isInt := pyrepr.Int(parsed); isInt {
			continue
		}
		out.Set(p.Name, parsed)
	}
	return out
}

// Validates call arguments against the signature model and returns every field in signature order
func codexToolsValidate(toolName string, sig codexToolsSignature, args *pyjson.Object) (*pyjson.Object, error) {
	data := codexToolsPreParse(sig, args)
	out := pyjson.NewObject()
	verr := &codexToolsValidationError{title: toolName + "Arguments"}
	for _, p := range sig.Params {
		v, present := data.Vals[p.Name]
		if !present {
			if p.Required {
				verr.errors = append(verr.errors, codexToolsLineError{loc: p.Name, kind: "missing", input: data})
			} else {
				out.Set(p.Name, nil)
			}
			continue
		}
		if v == nil && !p.Required {
			out.Set(p.Name, nil)
			continue
		}
		coerced, kind := codexToolsCoerce(p.PyType, v)
		if kind != "" {
			verr.errors = append(verr.errors, codexToolsLineError{loc: p.Name, kind: kind, input: v})
			continue
		}
		out.Set(p.Name, coerced)
	}
	if len(verr.errors) > 0 {
		return nil, verr
	}
	return out, nil
}

// One tool the server lists and dispatches through the registry
type codexToolsTool struct {
	name, description string
	sig               codexToolsSignature
	handler           func(kwargs *pyjson.Object) string
}

// CodexToolsServer is a stdio MCP server over the curated icebath tool subset
type CodexToolsServer struct {
	name         string
	instructions string
	tools        []*codexToolsTool
	in           io.Reader
	out          io.Writer
	writeMu      sync.Mutex
}

type codexToolsRunner interface{ Run() error }

// Creates the MCP server with the exposed registry tools attached
var codexToolsBuildServer = func() (codexToolsRunner, error) {
	ctx := context.Background()
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
	env := &tools.Env{Home: h, Config: tree, Secrets: secrets, Cwd: cwd}
	return codexToolsAssemble(tools.Default.ToolDefinitions(ctx, env, tools.DefinitionOptions{Quiet: true}),
		func(name string, args map[string]any) string {
			return tools.NewDispatcher(tools.Default).Dispatch(ctx, &tools.Call{Name: name, Args: args, Env: env})
		}), nil
}

// Attaches every exposed tool the definitions carry, dropping None arguments and reporting a raised error as JSON
func codexToolsAssemble(defs []map[string]any, call func(name string, args map[string]any) string) *CodexToolsServer {
	server := &CodexToolsServer{name: CodexToolsMCPServerName, instructions: codexToolsInstructions, in: os.Stdin, out: os.Stdout}
	all := map[string]any{}
	for _, td := range defs {
		if cxGet(td, "type") == "function" {
			all[pyrepr.Str(cxGet(cxGet(td, "function"), "name"))] = cxGet(td, "function")
		}
	}
	for _, name := range codexToolsExposedTools {
		spec, ok := all[name]
		if !ok || spec == nil {
			slog.Debug(fmt.Sprintf("skipping %s — not registered in this Icebath process", name))
			continue
		}
		description, _ := cxGet(spec, "description").(string)
		if description == "" {
			description = fmt.Sprintf("Icebath %s tool", name)
		}
		params := cxGet(spec, "parameters")
		if !cxTruthy(params) {
			params = map[string]any{"type": "object", "properties": map[string]any{}}
		}
		sig, _ := codexToolsSignatureFromSchema(params)
		toolName := name
		server.tools = append(server.tools, &codexToolsTool{name: name, description: description, sig: sig,
			handler: func(kwargs *pyjson.Object) (result string) {
				defer func() {
					if r := recover(); r != nil {
						slog.Error(fmt.Sprintf("tool %s raised", toolName), "panic", r)
						result = cxMustDumps(map[string]any{"error": pyrepr.Str(r), "tool": toolName})
					}
				}()
				args := map[string]any{}
				for _, k := range kwargs.Keys {
					if kwargs.Vals[k] != nil {
						args[k] = kwargs.Vals[k]
					}
				}
				return call(toolName, args)
			}})
	}
	slog.Info(fmt.Sprintf("icebath-tools MCP server registered %d/%d tools", len(server.tools), len(codexToolsExposedTools)))
	return server
}

func (s *CodexToolsServer) send(msg *pyjson.Object) error {
	text, err := pyjson.Dumps(msg, pyjson.Separators(",", ":"), pyjson.EnsureASCII(false))
	if err != nil {
		return err
	}
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	_, err = io.WriteString(s.out, text+"\n")
	return err
}

func codexToolsObject(kv ...any) *pyjson.Object {
	o := pyjson.NewObject()
	for i := 0; i+1 < len(kv); i += 2 {
		o.Set(kv[i].(string), kv[i+1])
	}
	return o
}

// Answers one JSON-RPC request, nil result with a code means an error reply
func (s *CodexToolsServer) handle(method string, params any) (any, int, string) {
	switch method {
	case "initialize", "server/discover":
		version := codexToolsDiscoverVersion
		if method == "initialize" {
			version = codexToolsHandshakeVersions[len(codexToolsHandshakeVersions)-1]
			if requested, ok := cxGet(params, "protocolVersion").(string); ok && slices.Contains(codexToolsHandshakeVersions, requested) {
				version = requested
			}
		}
		caps := codexToolsObject("experimental", pyjson.NewObject(),
			"prompts", codexToolsObject("listChanged", false),
			"resources", codexToolsObject("subscribe", false, "listChanged", false),
			"tools", codexToolsObject("listChanged", false))
		return codexToolsObject("protocolVersion", version, "capabilities", caps,
			"serverInfo", codexToolsObject("name", s.name, "version", Version), "instructions", s.instructions), 0, ""
	case "ping":
		return pyjson.NewObject(), 0, ""
	case "tools/list":
		var list []any
		for _, t := range s.tools {
			list = append(list, codexToolsObject("name", t.name, "description", t.description,
				"inputSchema", codexToolsInputSchema(t.name, t.sig), "outputSchema", codexToolsOutputSchema(t.name)))
		}
		if list == nil {
			list = []any{}
		}
		return codexToolsObject("tools", list), 0, ""
	case "tools/call":
		return s.callTool(params), 0, ""
	case "resources/list":
		return codexToolsObject("resources", []any{}), 0, ""
	case "resources/templates/list":
		return codexToolsObject("resourceTemplates", []any{}), 0, ""
	case "prompts/list":
		return codexToolsObject("prompts", []any{}), 0, ""
	}
	return nil, -32601, "Method not found"
}

// Runs a tool and wraps its text as content plus the structured result, failures as an error result
func (s *CodexToolsServer) callTool(params any) *pyjson.Object {
	name := pyrepr.Str(cxGet(params, "name"))
	errResult := func(text string) *pyjson.Object {
		return codexToolsObject("content", []any{codexToolsObject("type", "text", "text", text)}, "isError", true)
	}
	var tool *codexToolsTool
	for _, t := range s.tools {
		if t.name == name {
			tool = t
		}
	}
	if tool == nil {
		return errResult("Unknown tool: " + name)
	}
	args, ok := cxGet(params, "arguments").(*pyjson.Object)
	if !ok {
		args = pyjson.NewObject()
		if m, isMap := cxGet(params, "arguments").(map[string]any); isMap {
			for _, k := range codexSortedKeys(slices.Collect(func(yield func(string) bool) {
				for k := range m {
					if !yield(k) {
						return
					}
				}
			})) {
				args.Set(k, m[k])
			}
		}
	}
	kwargs, err := codexToolsValidate(tool.name, tool.sig, args)
	if err != nil {
		return errResult(fmt.Sprintf("Error executing tool %s: %s", tool.name, err.Error()))
	}
	text := tool.handler(kwargs)
	return codexToolsObject("content", []any{codexToolsObject("type", "text", "text", text)},
		"structuredContent", codexToolsObject("result", text), "isError", false)
}

// Serves newline delimited JSON-RPC on stdio until the client closes stdin
func (s *CodexToolsServer) Run() error {
	reader := bufio.NewReaderSize(s.in, 1<<20)
	for {
		line, err := reader.ReadString('\n')
		if strings.TrimSpace(line) != "" {
			s.dispatchLine(line)
		}
		if err == io.EOF {
			return nil
		}
		if err != nil {
			return err
		}
	}
}

func (s *CodexToolsServer) dispatchLine(line string) {
	msg, err := pyjson.Loads(line)
	if err != nil || !cxIsObject(msg) {
		slog.Error(fmt.Sprintf("Received exception from stream: invalid JSON-RPC message %s", pyrepr.Repr(strings.TrimSpace(line))))
		return
	}
	method, hasMethod := cxGet(msg, "method").(string)
	if !hasMethod || !cxHas(msg, "id") {
		return
	}
	id := cxGet(msg, "id")
	result, code, message := s.handle(method, cxGet(msg, "params"))
	reply := codexToolsObject("jsonrpc", "2.0", "id", id)
	if result == nil {
		reply.Set("error", codexToolsObject("code", code, "message", message))
	} else {
		reply.Set("result", result)
	}
	if err := s.send(reply); err != nil {
		slog.Error("icebath-tools MCP server write failed", "error", err)
	}
}

// Raised by a run that a keyboard interrupt ended
var errCodexToolsInterrupted = errors.New("KeyboardInterrupt")

// CodexToolsImportError is a server build failure main reports with exit status 2
type CodexToolsImportError struct{ Msg string }

func (e *CodexToolsImportError) Error() string { return e.Msg }

// RunCodexToolsMCPServer is the tools MCP server entry, 2 when it cannot start and 1 when it crashes
func RunCodexToolsMCPServer(argv []string) int {
	level := slog.LevelWarn
	if slices.Contains(argv, "--verbose") || slices.Contains(argv, "-v") {
		level = slog.LevelInfo
	}
	slog.SetDefault(slog.New(&codexToolsLogHandler{level: level, out: os.Stderr, name: "agent.transports.icebath_tools_mcp_server"}))
	for key, value := range map[string]string{"ICEBATH_QUIET": "1", "ICEBATH_REDACT_SECRETS": "true"} {
		if _, set := os.LookupEnv(key); !set {
			os.Setenv(key, value)
		}
	}
	server, err := codexToolsBuildServer()
	if err != nil {
		var importErr *CodexToolsImportError
		if errors.As(err, &importErr) {
			fmt.Fprintf(os.Stderr, "icebath-tools MCP server cannot start: %s\n", err.Error())
			return 2
		}
		slog.Error("icebath-tools MCP server crashed", "error", err)
		fmt.Fprintf(os.Stderr, "icebath-tools MCP server error: %s\n", err.Error())
		return 1
	}
	if err := codexToolsRunInterruptible(server); err != nil && !errors.Is(err, errCodexToolsInterrupted) {
		slog.Error("icebath-tools MCP server crashed", "error", err)
		fmt.Fprintf(os.Stderr, "icebath-tools MCP server error: %s\n", err.Error())
		return 1
	}
	return 0
}

// Runs the server until it returns, a SIGINT ending it the way KeyboardInterrupt does
func codexToolsRunInterruptible(server codexToolsRunner) (err error) {
	sigs := make(chan os.Signal, 1)
	signal.Notify(sigs, os.Interrupt)
	defer signal.Stop(sigs)
	done := make(chan error, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				if e, ok := r.(error); ok {
					done <- e
					return
				}
				done <- fmt.Errorf("%v", r)
			}
		}()
		done <- server.Run()
	}()
	select {
	case err = <-done:
		return err
	case <-sigs:
		return errCodexToolsInterrupted
	}
}

// DispatchCodexToolsMCPServerEntry runs the tools MCP server and exits when argv names its entry
func DispatchCodexToolsMCPServerEntry() {
	if len(os.Args) > 1 && os.Args[1] == CodexToolsMCPServerEntry {
		os.Exit(RunCodexToolsMCPServer(os.Args[2:]))
	}
}

// Stderr log lines in the asctime levelname name message layout of logging.basicConfig
type codexToolsLogHandler struct {
	level slog.Level
	out   io.Writer
	name  string
	mu    sync.Mutex
}

func (h *codexToolsLogHandler) Enabled(_ context.Context, l slog.Level) bool { return l >= h.level }
func (h *codexToolsLogHandler) WithAttrs([]slog.Attr) slog.Handler           { return h }
func (h *codexToolsLogHandler) WithGroup(string) slog.Handler                { return h }
func (h *codexToolsLogHandler) Handle(_ context.Context, r slog.Record) error {
	names := map[slog.Level]string{slog.LevelDebug: "DEBUG", slog.LevelInfo: "INFO", slog.LevelWarn: "WARNING", slog.LevelError: "ERROR"}
	msg := r.Message
	r.Attrs(func(a slog.Attr) bool { msg += " " + a.Key + "=" + a.Value.String(); return true })
	t := r.Time
	if t.IsZero() {
		t = time.Now()
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	_, err := fmt.Fprintf(h.out, "%s,%03d [%s] %s: %s\n", t.Format("2006-01-02 15:04:05"), t.Nanosecond()/1e6, names[r.Level], h.name, msg)
	return err
}
