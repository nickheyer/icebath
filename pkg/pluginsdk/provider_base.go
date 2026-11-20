package pluginsdk

import (
	"context"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"unicode"
)

// Seconds a secret source fetch may take when its config names no positive timeout
const defaultFetchTimeoutSeconds = 120.0

// Members a provider of one kind must define itself and the ones the base behaviour of icebath stands in for when it does not
type providerBase struct {
	required []string
	attrs    func(o *Object) map[string]any
	methods  func(o *Object) map[string]Func
	// Record type icebath reads the answer of a method as, a plain mapping the method answers crossing under that type
	records map[string]string
	// Record type icebath reads every item of the list a method answers as
	itemRecords map[string]string
}

// Value as the record of a type, a plain mapping given the type and a record of another type or any other value refused
func asRecord(kind, method, record string, value any) (any, error) {
	switch x := value.(type) {
	case Record:
		if x.Type == record {
			return x, nil
		}
	case map[string]any:
		return Record{Type: record, Fields: x}, nil
	}
	return nil, &Error{Type: "TypeError", Message: fmt.Sprintf("%s of a %s provider answers a pluginsdk.Record of type %s or a mapping of its fields", method, kind, record)}
}

// Method whose answer crosses as the record icebath declares for it, nothing staying nothing
func recordAnswer(kind, method, record string, fn Func) Func {
	return func(ctx context.Context, args []any, kwargs map[string]any) (any, error) {
		value, err := fn(ctx, args, kwargs)
		if err != nil || value == nil {
			return value, err
		}
		return asRecord(kind, method, record, value)
	}
}

// Method whose answer is a list crossing with every item as the record icebath declares for it
func recordItemsAnswer(kind, method, record string, fn Func) Func {
	return func(ctx context.Context, args []any, kwargs map[string]any) (any, error) {
		value, err := fn(ctx, args, kwargs)
		if err != nil || value == nil {
			return value, err
		}
		items, ok := value.([]any)
		if !ok {
			return nil, &Error{Type: "TypeError", Message: fmt.Sprintf("%s of a %s provider answers a list of %s records", method, kind, record)}
		}
		out := make([]any, len(items))
		for i, item := range items {
			if out[i], err = asRecord(kind, method, record, item); err != nil {
				return nil, err
			}
		}
		return out, nil
	}
}

// Callback answering one fixed value
func constant(value any) Func {
	return func(context.Context, []any, map[string]any) (any, error) { return value, nil }
}

// Object a base method runs for, the one the host addressed before the one the method was made for
func receiver(ctx context.Context, made *Object) *Object {
	if self := Self(ctx); self != nil {
		return self
	}
	return made
}

// Text of the name attribute of a provider
func providerName(o *Object) string {
	name, _ := o.Attr("name")
	return text(name)
}

// Display name of a provider, its own attribute before the fallback
func providerDisplayName(o *Object, fallback func(string) string) string {
	if display, ok := o.Attr("display_name"); ok && text(display) != "" {
		return text(display)
	}
	return fallback(providerName(o))
}

// Name with the first letter of every word in upper case and the rest in lower case
func titleCase(name string) string {
	var b strings.Builder
	start := true
	for _, r := range name {
		if !unicode.IsLetter(r) {
			start = true
			b.WriteRune(r)
			continue
		}
		if start {
			b.WriteRune(unicode.ToUpper(r))
		} else {
			b.WriteRune(unicode.ToLower(r))
		}
		start = false
	}
	return b.String()
}

// Name as it stands, the display name of a provider base that does not restyle it
func sameName(name string) string { return name }

// Setup schema every provider answers unless it declares its own
func setupSchema(o *Object, display func(string) string) Func {
	return func(ctx context.Context, _ []any, _ map[string]any) (any, error) {
		return map[string]any{"name": providerDisplayName(receiver(ctx, o), display), "badge": "", "tag": "", "env_vars": []any{}}, nil
	}
}

// Id of the first model a provider lists, nothing when it lists none
func firstModel(o *Object) Func {
	return func(ctx context.Context, _ []any, _ map[string]any) (any, error) {
		list := receiver(ctx, o).Methods["list_models"]
		if list == nil {
			return nil, nil
		}
		models, err := list(ctx, []any{}, map[string]any{})
		if err != nil {
			return nil, err
		}
		if rows := asList(models); len(rows) > 0 {
			return asMap(rows[0])["id"], nil
		}
		return nil, nil
	}
}

// Callback refusing a capability a provider does not implement
func unsupported(o *Object, format string) Func {
	return func(ctx context.Context, _ []any, _ map[string]any) (any, error) {
		return nil, &Error{Type: "NotImplementedError", Message: fmt.Sprintf(format, providerName(receiver(ctx, o)))}
	}
}

// Whole number of an attribute or wire value, zero for anything else
func wholeNumber(v any) int64 {
	switch x := v.(type) {
	case int:
		return int64(x)
	case int64:
		return x
	case float64:
		return int64(x)
	}
	return 0
}

// Number of an attribute or wire value, numeric text included
func floatNumber(v any) (float64, bool) {
	switch x := v.(type) {
	case int:
		return float64(x), true
	case int64:
		return float64(x), true
	case float64:
		return x, true
	case string:
		f, err := strconv.ParseFloat(strings.TrimSpace(x), 64)
		return f, err == nil
	}
	return 0, false
}

// Whole number attribute of an object
func wholeAttr(o *Object, name string) int64 {
	value, _ := o.Attr(name)
	return wholeNumber(value)
}

// Copy of an attribute value whose mappings and lists are its own, so a clone never changes what the original holds
func copyValue(v any) any {
	switch x := v.(type) {
	case map[string]any:
		out := make(map[string]any, len(x))
		for k, item := range x {
			out[k] = copyValue(item)
		}
		return out
	case []any:
		out := make([]any, len(x))
		for i, item := range x {
			out[i] = copyValue(item)
		}
		return out
	case []string:
		return append([]string(nil), x...)
	case []byte:
		return append([]byte(nil), x...)
	}
	return v
}

// Threshold a context engine compresses at for a model, the longest matching key of its per model table before its configured percent
func modelThreshold(model string, table map[string]any, fallback float64, provider string) float64 {
	provider = strings.ToLower(strings.TrimSpace(provider))
	best, bestLength, bestScoped := "", -1, -1
	for key := range table {
		scope, substr, scoped := strings.Cut(key, ":")
		length, rank := len(key), 0
		switch {
		case !scoped && strings.Contains(model, key):
		case scoped && strings.ToLower(strings.TrimSpace(scope)) == provider && strings.Contains(model, substr):
			length, rank = len(substr), 1
		default:
			continue
		}
		if length > bestLength || (length == bestLength && rank > bestScoped) || (length == bestLength && rank == bestScoped && key > best) {
			best, bestLength, bestScoped = key, length, rank
		}
	}
	if threshold, ok := floatNumber(table[best]); ok && bestLength >= 0 && model != "" {
		return threshold
	}
	return fallback
}

// Base members of a context engine, the budget fields and every hook the engine may leave to icebath
func contextEngineBase() providerBase {
	return providerBase{
		required: []string{"name", "update_from_response", "should_compress", "compress"},
		attrs: func(*Object) map[string]any {
			return map[string]any{"last_prompt_tokens": int64(0), "last_completion_tokens": int64(0), "last_total_tokens": int64(0),
				"threshold_tokens": int64(0), "context_length": int64(0), "compression_count": int64(0), "threshold_percent": 0.75,
				"protect_first_n": int64(3), "protect_last_n": int64(6), "emit_automatic_compaction_status": true}
		},
		methods: func(o *Object) map[string]Func {
			return map[string]Func{
				"should_compress_info": func(ctx context.Context, args []any, kwargs map[string]any) (any, error) {
					verdict, err := receiver(ctx, o).Methods["should_compress"](ctx, args, kwargs)
					return []any{verdict, nil}, err
				},
				"prune_tool_results_only": func(_ context.Context, args []any, _ map[string]any) (any, error) {
					return []any{argAt(args, 0), int64(0)}, nil
				},
				"select_context":                       constant(nil),
				"on_turn_complete":                     constant(nil),
				"should_compress_preflight":            constant(false),
				"should_defer_preflight_to_real_usage": constant(false),
				"get_automatic_compaction_status_message": func(ctx context.Context, _ []any, kwargs map[string]any) (any, error) {
					if emit, _ := receiver(ctx, o).Attr("emit_automatic_compaction_status"); !truthy(emit) {
						return nil, nil
					}
					return kwargs["default_message"], nil
				},
				"has_content_to_compress": constant(true),
				"on_session_start":        constant(nil),
				"on_session_end":          constant(nil),
				"on_session_reset": func(ctx context.Context, _ []any, _ map[string]any) (any, error) {
					for _, name := range []string{"last_prompt_tokens", "last_completion_tokens", "last_total_tokens", "compression_count"} {
						receiver(ctx, o).SetAttr(name, int64(0))
					}
					return nil, nil
				},
				"get_tool_schemas": constant([]any{}),
				"handle_tool_call": func(_ context.Context, args []any, _ map[string]any) (any, error) {
					out, err := json.Marshal(map[string]any{"error": "Unknown context engine tool: " + argText(args, 0)})
					return string(out), err
				},
				"get_status": func(ctx context.Context, _ []any, _ map[string]any) (any, error) {
					o := receiver(ctx, o)
					prompt, length := max(wholeAttr(o, "last_prompt_tokens"), 0), wholeAttr(o, "context_length")
					var usage any = int64(0)
					if length != 0 {
						usage = min(100, float64(prompt)/float64(length)*100)
					}
					return map[string]any{"last_prompt_tokens": prompt, "threshold_tokens": wholeAttr(o, "threshold_tokens"), "context_length": length,
						"usage_percent": usage, "compression_count": wholeAttr(o, "compression_count")}, nil
				},
				"clone_for_agent": func(ctx context.Context, _ []any, _ map[string]any) (any, error) {
					o := receiver(ctx, o)
					clone := &Object{Type: o.Type, Attrs: map[string]any{}, Methods: o.Methods, Next: o.Next}
					attrsMu.Lock()
					for name, value := range o.Attrs {
						clone.Attrs[name] = copyValue(value)
					}
					attrsMu.Unlock()
					return clone, nil
				},
				"update_model": func(ctx context.Context, args []any, kwargs map[string]any) (any, error) {
					o := receiver(ctx, o)
					length := wholeNumber(argAt(args, 1))
					provider := argText(args, 4)
					if provider == "" {
						provider = text(kwargs["provider"])
					}
					configured, snapshotted := o.Attr("_config_threshold_percent")
					if !snapshotted {
						configured, _ = o.Attr("threshold_percent")
						o.SetAttr("_config_threshold_percent", configured)
					}
					fallback, _ := floatNumber(configured)
					table, _ := o.Attr("model_thresholds")
					percent := modelThreshold(argText(args, 0), asMap(table), fallback, provider)
					o.SetAttr("context_length", length)
					o.SetAttr("_base_threshold_percent", percent)
					o.SetAttr("threshold_percent", percent)
					o.SetAttr("threshold_tokens", int64(float64(length)*percent))
					return nil, nil
				},
			}
		},
	}
}

// Base members of a memory provider, every hook it may leave unanswered
func memoryBase() providerBase {
	return providerBase{
		required: []string{"name", "is_available", "initialize", "get_tool_schemas"},
		records:  map[string]string{"recall_status": "RecallStatus"},
		methods: func(o *Object) map[string]Func {
			return map[string]Func{
				"unavailable_reason": constant(""), "system_prompt_block": constant(""), "prefetch": constant(""), "queue_prefetch": constant(nil),
				"recall_status": constant(nil), "sync_turn": constant(nil), "shutdown": constant(nil), "on_turn_start": constant(nil),
				"identity_signature": constant(map[string]any{}), "on_session_end": constant(nil), "on_session_switch": constant(nil),
				"on_pre_compress": constant(""), "on_delegation": constant(nil), "get_config_schema": constant([]any{}),
				"save_config": constant(nil), "on_memory_write": constant(nil), "backup_paths": constant([]any{}),
				"on_profile_created": constant(nil), "on_profile_renamed": constant(nil),
				"handle_tool_call": func(ctx context.Context, args []any, _ map[string]any) (any, error) {
					return nil, &Error{Type: "NotImplementedError", Message: fmt.Sprintf("Provider %s does not handle tool %s", providerName(receiver(ctx, o)), argText(args, 0))}
				},
			}
		},
	}
}

// Base members of each provider kind, the ones icebath calls without asking whether the provider defines them
var providerBases = map[string]providerBase{
	"image_gen": {required: []string{"name", "generate"}, methods: func(*Object) map[string]Func {
		return map[string]Func{"is_available": constant(true)}
	}},
	"video_gen": {required: []string{"name", "generate"}, methods: func(*Object) map[string]Func {
		return map[string]Func{"is_available": constant(true)}
	}},
	"web_search": {required: []string{"name", "is_available"},
		attrs: func(o *Object) map[string]any { return map[string]any{"display_name": providerName(o)} },
		methods: func(o *Object) map[string]Func {
			return map[string]Func{"supports_search": constant(true), "is_keyless_available": constant(false), "supports_extract": constant(false),
				"search":  unsupported(o, "%s does not support search (override supports_search)"),
				"extract": unsupported(o, "%s does not support extract (override supports_extract)")}
		}},
	"browser": {required: []string{"name", "is_available", "create_session", "close_session", "emergency_cleanup"},
		attrs:   func(o *Object) map[string]any { return map[string]any{"display_name": providerName(o)} },
		methods: func(o *Object) map[string]Func { return map[string]Func{"get_setup_schema": setupSchema(o, sameName)} }},
	"terminal_environment": {required: []string{"name", "is_available", "create_environment"}},
	"cron_scheduler":       {required: []string{"name", "start"}},
	"computer_use": {required: []string{"name", "is_available", "create_backend"},
		attrs: func(o *Object) map[string]any { return map[string]any{"display_name": providerName(o)} },
		methods: func(o *Object) map[string]Func {
			return map[string]Func{"get_setup_schema": setupSchema(o, sameName), "doctor": constant(nil)}
		}},
	"secret_source": {required: []string{"name", "fetch"}, records: map[string]string{"fetch": "FetchResult"},
		attrs: func(*Object) map[string]any { return map[string]any{"api_version": int64(1), "shape": "mapped"} },
		methods: func(*Object) map[string]Func {
			return map[string]Func{
				"is_enabled": func(_ context.Context, args []any, _ map[string]any) (any, error) {
					return truthy(asMap(argAt(args, 0))["enabled"]), nil
				},
				"fetch_timeout_seconds": func(_ context.Context, args []any, _ map[string]any) (any, error) {
					if seconds, ok := floatNumber(asMap(argAt(args, 0))["timeout_seconds"]); ok && seconds > 0 {
						return seconds, nil
					}
					return defaultFetchTimeoutSeconds, nil
				},
			}
		}},
	"tts": {required: []string{"name", "synthesize"}},
	"transcription": {required: []string{"name", "transcribe"}, methods: func(o *Object) map[string]Func {
		return map[string]Func{"get_setup_schema": setupSchema(o, titleCase), "is_available": constant(true),
			"list_models": constant([]any{}), "default_model": firstModel(o)}
	}},
	"delegation": {required: []string{"name", "route_task"}, methods: func(o *Object) map[string]Func {
		return map[string]Func{"get_setup_schema": setupSchema(o, sameName), "is_available": constant(true),
			"task_parameters": constant(map[string]any{}), "description": constant(""),
			"settings": func(ctx context.Context, args []any, _ map[string]any) (any, error) {
				own := asMap(asMap(asMap(argAt(args, 0))["backends"])[providerName(receiver(ctx, o))])
				return mapOrEmpty(own), nil
			}}
	}},
	"context_engine":    contextEngineBase(),
	"context_reference": {required: []string{"prefix", "autocomplete", "expand"}, itemRecords: map[string]string{"autocomplete": "ContextCompletionItem"}},
	"memory":            memoryBase(),
	"login_backend": {required: []string{"name", "prefix", "list_items", "get_meta", "resolve_password"},
		attrs: func(o *Object) map[string]any {
			return map[string]any{"display_name": providerName(o), "needs_unlock": false}
		},
		methods: func(o *Object) map[string]Func {
			return map[string]Func{
				"owns": func(ctx context.Context, args []any, _ map[string]any) (any, error) {
					prefix, _ := receiver(ctx, o).Attr("prefix")
					return strings.HasPrefix(argText(args, 0), text(prefix)), nil
				},
				"is_unlocked": constant(true),
				"resolve_otp": constant(nil),
				"resolve_secret": func(ctx context.Context, args []any, kwargs map[string]any) (any, error) {
					password, err := receiver(ctx, o).Methods["resolve_password"](ctx, args, kwargs)
					return map[string]any{"password": password}, err
				},
				"fill_enter": constant(int64(0)),
				"fill_exit":  constant(nil),
			}
		}},
	"browser_page_backend": {required: []string{"name", "has_page", "evaluate"},
		attrs:   func(o *Object) map[string]any { return map[string]any{"display_name": providerName(o)} },
		methods: func(o *Object) map[string]Func { return map[string]Func{"get_setup_schema": setupSchema(o, sameName)} }},
}

// Checks that a provider defines what its kind requires and fills the members it left to the base behaviour
func completeProvider(kind string, o *Object) error {
	base := providerBases[kind]
	for _, name := range base.required {
		value, isAttr := o.Attr(name)
		if _, isMethod := o.Methods[name]; isMethod || (isAttr && text(value) != "") {
			continue
		}
		return fmt.Errorf("a %s provider needs %s", kind, name)
	}
	if base.attrs != nil {
		for name, value := range base.attrs(o) {
			if _, set := o.Attr(name); !set {
				o.SetAttr(name, value)
			}
		}
	}
	if base.methods != nil {
		if o.Methods == nil {
			o.Methods = map[string]Func{}
		}
		for name, fn := range base.methods(o) {
			if o.Methods[name] == nil {
				o.Methods[name] = fn
			}
		}
	}
	for name, record := range base.records {
		if fn := o.Methods[name]; fn != nil {
			o.Methods[name] = recordAnswer(kind, name, record, fn)
		}
	}
	for name, record := range base.itemRecords {
		if fn := o.Methods[name]; fn != nil {
			o.Methods[name] = recordItemsAnswer(kind, name, record, fn)
		}
	}
	return nil
}
