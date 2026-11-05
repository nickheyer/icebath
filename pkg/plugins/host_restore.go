package plugins

import (
	"fmt"
	"math"
	"strings"
	"sync"
)

// Parsed Python annotation, a name with its subscript arguments
type pyHint struct {
	name string
	args []*pyHint
	sub  bool
}

// Dataclass is a Record rebuilt as the dataclass a provider base declares, every init field present
type Dataclass struct {
	Type   string
	Fields Record
}

// PySet is a set or frozenset rebuilt from a wire list, first occurrences kept in order
type PySet []any

type dataclassField struct {
	name     string
	hint     string
	required bool
	def      func() any
}

func emptyMap() any  { return map[string]any{} }
func emptyList() any { return []any{} }
func constant(v any) func() any {
	return func() any { return v }
}

// Init fields of the dataclasses provider bases declare as return or field types
var hostDataclasses = map[string][]dataclassField{
	"FetchResult": {
		{name: "secrets", hint: "dict[str, str]", def: emptyMap}, {name: "applied", hint: "list[str]", def: emptyList},
		{name: "skipped", hint: "list[str]", def: emptyList}, {name: "warnings", hint: "list[str]", def: emptyList},
		{name: "error", hint: "Optional[str]", def: constant(nil)}, {name: "error_kind", hint: "Optional[ErrorKind]", def: constant(nil)},
		{name: "binary_path", hint: "Optional[Path]", def: constant(nil)},
	},
	"RecallStatus": {
		{name: "provider_label", hint: "str", required: true}, {name: "count", hint: "int", required: true},
		{name: "glyph", hint: "str", def: constant("\U0001f9e0")},
	},
	"ContextCompletionItem": {
		{name: "text", hint: "str", required: true}, {name: "display", hint: "str", def: constant("")},
		{name: "meta", hint: "str", def: constant("")},
	},
	"ContextReference": {
		{name: "raw", hint: "str", required: true}, {name: "kind", hint: "str", required: true},
		{name: "target", hint: "str", required: true}, {name: "start", hint: "int", required: true},
		{name: "end", hint: "int", required: true}, {name: "line_start", hint: "int | None", def: constant(nil)},
		{name: "line_end", hint: "int | None", def: constant(nil)},
	},
	"ContextReferenceResult": {
		{name: "message", hint: "str", required: true}, {name: "original_message", hint: "str", required: true},
		{name: "references", hint: "list[ContextReference]", def: emptyList}, {name: "warnings", hint: "list[str]", def: emptyList},
		{name: "injected_tokens", hint: "int", def: constant(int64(0))}, {name: "expanded", hint: "bool", def: constant(false)},
		{name: "blocked", hint: "bool", def: constant(false)},
	},
}

var providerBaseReturns = map[string]string{"name": "str", "display_name": "str", "get_setup_schema": "dict[str, Any]", "is_available": "bool"}
var catalogBaseReturns = map[string]string{"list_models": "list[dict[str, Any]]", "default_model": "Optional[str]"}

func returnsOf(tables ...map[string]string) map[string]string {
	out := map[string]string{}
	for _, t := range tables {
		for k, v := range t {
			out[k] = v
		}
	}
	return out
}

// Return annotation of every method and property each provider base declares, inherited ones included
var hostDeclaredReturns = map[string]map[string]string{
	"agent.image_gen_provider:ImageGenProvider": returnsOf(providerBaseReturns, catalogBaseReturns,
		map[string]string{"capabilities": "dict[str, Any]", "generate": "dict[str, Any]"}),
	"agent.video_gen_provider:VideoGenProvider": returnsOf(providerBaseReturns, catalogBaseReturns,
		map[string]string{"capabilities": "dict[str, Any]", "generate": "dict[str, Any]"}),
	"agent.web_search_provider:WebSearchProvider": returnsOf(providerBaseReturns, map[string]string{"supports_search": "bool",
		"is_keyless_available": "bool", "supports_extract": "bool", "search": "dict[str, Any]", "extract": "Any"}),
	"agent.browser_provider:BrowserProvider": returnsOf(providerBaseReturns, map[string]string{"create_session": "dict[str, object]",
		"close_session": "bool", "emergency_cleanup": "None"}),
	"agent.terminal_env_provider:TerminalEnvironmentProvider": returnsOf(providerBaseReturns, map[string]string{"description": "str",
		"skip_container_guards": "bool", "cache_path_base": "Optional[str]", "default_cwd": "Optional[str]", "strip_env_keys": "frozenset",
		"env_description": "str", "check_requirements": "bool", "probe": "tuple[str, str]", "setup_instructions": "list[str]",
		"post_setup": "None", "doctor_checks": "list[tuple[bool, str, str]]"}),
	"agent.secret_sources.base:SecretSource": {"fetch": "FetchResult", "is_enabled": "bool", "override_existing": "bool",
		"token_env": "str", "protected_env_vars": "frozenset[str]", "fetch_timeout_seconds": "float", "config_schema": "dict",
		"remediation": "str"},
	"agent.tts_provider:TTSProvider": returnsOf(providerBaseReturns, catalogBaseReturns, map[string]string{"list_voices": "list[dict[str, Any]]",
		"default_voice": "Optional[str]", "synthesize": "str", "stream": "Iterator[bytes]", "warm": "None", "release": "None",
		"voice_compatible": "bool"}),
	"agent.transcription_provider:TranscriptionProvider": returnsOf(providerBaseReturns, catalogBaseReturns,
		map[string]string{"transcribe": "dict[str, Any]", "streaming_capable": "bool", "open_stream_session": "TranscriptionStreamSession"}),
	"agent.delegation_provider:DelegationProvider": returnsOf(providerBaseReturns, map[string]string{"settings": "Mapping[str, Any]",
		"summary": "str", "task_parameters": "dict[str, dict[str, Any]]", "description": "str", "route_task": "dict[str, Any]"}),
	"agent.context_engine:ContextEngine": {"name": "str", "update_from_response": "None", "should_compress": "bool",
		"should_compress_info": "tuple[bool, str | None]", "compress": "list[dict[str, Any]]",
		"prune_tool_results_only": "tuple[list[dict[str, Any]], int]", "select_context": "list[dict[str, Any]]",
		"on_turn_complete": "None", "should_compress_preflight": "bool", "should_defer_preflight_to_real_usage": "bool",
		"get_automatic_compaction_status_message": "str | None", "has_content_to_compress": "bool", "on_session_start": "None",
		"on_session_end": "None", "on_session_reset": "None", "get_tool_schemas": "list[dict[str, Any]]", "handle_tool_call": "str",
		"get_status": "dict[str, Any]", "clone_for_agent": "'ContextEngine'", "update_model": "None"},
	"agent.context_references:ContextReferenceProvider": {"autocomplete": "list[ContextCompletionItem]", "expand": "str | None"},
	"agent.memory_provider:MemoryProvider": {"name": "str", "is_available": "bool", "initialize": "None",
		"unavailable_reason": "str", "system_prompt_block": "str", "prefetch": "str", "queue_prefetch": "None",
		"recall_status": "Optional[RecallStatus]", "sync_turn": "None", "get_tool_schemas": "list[dict[str, Any]]",
		"handle_tool_call": "str", "shutdown": "None", "on_turn_start": "None", "identity_signature": "dict[str, Any]",
		"on_session_end": "None", "on_session_switch": "None", "on_pre_compress": "str", "on_delegation": "None",
		"get_config_schema": "list[dict[str, Any]]", "save_config": "None", "on_memory_write": "None", "backup_paths": "list[str]"},
}

var parsedHints sync.Map

// Return type a provider base declares for a method or property, nil when undeclared
func declaredReturn(base, name string) *pyHint {
	src, ok := hostDeclaredReturns[base][name]
	if !ok {
		return nil
	}
	return cachedHint(src)
}

func cachedHint(src string) *pyHint {
	if v, ok := parsedHints.Load(src); ok {
		return v.(*pyHint)
	}
	h := parsePyHint(src)
	parsedHints.Store(src, h)
	return h
}

// Provider base ref a hint names when a hosted object may stand in for it, empty otherwise
func hostObjectBase(hint *pyHint) string {
	if hint == nil || hint.sub {
		return ""
	}
	for _, ref := range sortedKeys(HostObjectBases) {
		base := HostObjectBases[ref]
		if base[strings.LastIndex(base, ":")+1:] == hint.name {
			return base
		}
	}
	return ""
}

type hintParser struct {
	s string
	i int
}

// Parses an annotation such as Optional[dict[str, Any]] or 'ContextEngine' into a hint tree
func parsePyHint(src string) *pyHint {
	p := &hintParser{s: src}
	return p.union()
}

func (p *hintParser) ws() {
	for p.i < len(p.s) && p.s[p.i] == ' ' {
		p.i++
	}
}

func (p *hintParser) at(c byte) bool {
	p.ws()
	return p.i < len(p.s) && p.s[p.i] == c
}

func (p *hintParser) union() *pyHint {
	arms := []*pyHint{p.atom()}
	for p.at('|') {
		p.i++
		arms = append(arms, p.atom())
	}
	if len(arms) == 1 {
		return arms[0]
	}
	return &pyHint{name: "Union", args: arms, sub: true}
}

func (p *hintParser) atom() *pyHint {
	p.ws()
	if p.i < len(p.s) && (p.s[p.i] == '\'' || p.s[p.i] == '"') {
		quote := p.s[p.i]
		end := strings.IndexByte(p.s[p.i+1:], quote)
		if end < 0 {
			end = len(p.s) - p.i - 1
		}
		inner := p.s[p.i+1 : p.i+1+end]
		p.i += end + 2
		return parsePyHint(inner)
	}
	start := p.i
	for p.i < len(p.s) && strings.IndexByte("[],| ", p.s[p.i]) < 0 {
		p.i++
	}
	name := p.s[start:p.i]
	for _, prefix := range []string{"typing.", "collections.abc.", "abc."} {
		name = strings.TrimPrefix(name, prefix)
	}
	h := &pyHint{name: name}
	if p.at('[') {
		p.i++
		h.sub = true
		for !p.at(']') && p.i < len(p.s) {
			h.args = append(h.args, p.union())
			if p.at(',') {
				p.i++
			}
		}
		p.i++
	}
	if h.name == "Optional" {
		h = &pyHint{name: "Union", args: append(h.args, &pyHint{name: "None"}), sub: true}
	}
	return h
}

// Container kind of a hint whose origin rebuilds a wire value, the typing aliases counting without subscript
func hintContainer(h *pyHint) string {
	aliases := map[string]string{"Dict": "dict", "Mapping": "dict", "List": "list", "Sequence": "list", "Iterable": "list",
		"Tuple": "tuple", "Set": "set", "FrozenSet": "frozenset", "AbstractSet": "frozenset"}
	if kind, ok := aliases[h.name]; ok {
		return kind
	}
	if h.sub {
		switch h.name {
		case "dict", "list", "tuple", "set", "frozenset":
			return h.name
		}
	}
	return ""
}

// Rebuilds a decoded wire value as the declared hint, a value that does not fit staying as decoded
func restoreHinted(value any, hint *pyHint) (any, error) {
	if value == nil || hint == nil || hint.name == "Any" {
		return value, nil
	}
	if hint.name == "Union" {
		var arms []*pyHint
		for _, arm := range hint.args {
			if arm.name != "None" {
				arms = append(arms, arm)
			}
		}
		if len(arms) == 1 {
			return restoreHinted(value, arms[0])
		}
		return value, nil
	}
	if kind := hintContainer(hint); kind != "" {
		return restoreContainer(value, kind, hint.args)
	}
	if hint.sub {
		return value, nil
	}
	fields, ok := hostDataclasses[hint.name]
	rec, isRecord := value.(Record)
	if !ok || !isRecord {
		return value, nil
	}
	out := Record{}
	for _, f := range fields {
		raw, present := rec[f.name]
		if !present {
			if f.required {
				return value, nil
			}
			out[f.name] = f.def()
			continue
		}
		restored, err := restoreHinted(raw, cachedHint(f.hint))
		if err != nil {
			return value, nil
		}
		out[f.name] = restored
	}
	return &Dataclass{Type: hint.name, Fields: out}, nil
}

func restoreContainer(value any, kind string, args []*pyHint) (any, error) {
	if kind == "dict" {
		var items map[string]any
		switch v := value.(type) {
		case Record:
			items = v
		case map[string]any:
			items = v
		default:
			return value, nil
		}
		var itemHint *pyHint
		if len(args) == 2 {
			itemHint = args[1]
		}
		out := make(map[string]any, len(items))
		for k, v := range items {
			restored, err := restoreHinted(v, itemHint)
			if err != nil {
				return nil, err
			}
			out[k] = restored
		}
		return out, nil
	}
	list, ok := value.([]any)
	if !ok {
		return value, nil
	}
	out := make([]any, 0, len(list))
	if kind == "tuple" && len(args) > 0 && args[len(args)-1].name != "..." {
		if len(args) != len(list) {
			return append(out, list...), nil
		}
		for i, v := range list {
			restored, err := restoreHinted(v, args[i])
			if err != nil {
				return nil, err
			}
			out = append(out, restored)
		}
		return out, nil
	}
	var itemHint *pyHint
	if len(args) > 0 {
		itemHint = args[0]
	}
	for _, v := range list {
		restored, err := restoreHinted(v, itemHint)
		if err != nil {
			return nil, err
		}
		out = append(out, restored)
	}
	if kind == "set" || kind == "frozenset" {
		return pySetOf(out)
	}
	return out, nil
}

// Set of values with Python equality, an unhashable element raising TypeError
func pySetOf(items []any) (PySet, error) {
	seen := map[any]bool{}
	out := PySet{}
	for _, item := range items {
		var key any
		switch v := item.(type) {
		case []any, PySet:
			return nil, &HostBuiltinError{Type: "TypeError", Message: "unhashable type: 'list'"}
		case map[string]any, Record:
			return nil, &HostBuiltinError{Type: "TypeError", Message: "unhashable type: 'dict'"}
		case bool:
			key = int64(0)
			if v {
				key = int64(1)
			}
		case float64:
			key = v
			if v == math.Trunc(v) && math.Abs(v) < 1<<62 {
				key = int64(v)
			}
		case []byte:
			key = "b:" + string(v)
		case string:
			key = "s:" + v
		default:
			key = fmt.Sprintf("%T:%v", v, v)
			if _, isInt := v.(int64); isInt {
				key = v
			}
		}
		if !seen[key] {
			seen[key] = true
			out = append(out, item)
		}
	}
	return out, nil
}
