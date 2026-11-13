package tui

import (
	"encoding/json"
	"errors"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/google/uuid"
)

// Session creation policy the gateway describes, the sources it accepts and the parameters it takes
type creationContract struct {
	Sources    []string
	Parameters []string
}

// Shared control identity a prompt carries back on its answer
type sharedControl struct {
	SessionID           string
	ExecutionGeneration int64
	PromptID            string
}

// Order the launch options take on session.create
var creationOptionOrder = []string{
	"model", "provider", "cwd", "toolsets", "skills", "client_env", "max_turns", "yolo", "ignore_rules",
	"safe_mode", "ignore_user_config", "checkpoints", "pass_session_id", "accept_hooks", "source", "request_id",
}

var (
	toolProgressModes = map[string]bool{"off": true, "new": true, "all": true, "verbose": true}
	skillSplitRe      = regexp.MustCompile(`[,\n]`)
	submitMethods     = map[string]bool{"prompt.submit": true, "session.steer": true, "session.redirect": true}
	resumeMethods     = map[string]bool{"session.create": true, "session.resume": true, "session.activate": true}
)

// Keys of params in creation option order, unknown keys after them sorted
func orderedKeys(params map[string]any) []string {
	var keys, rest []string
	for _, k := range creationOptionOrder {
		if _, ok := params[k]; ok {
			keys = append(keys, k)
		}
	}
	for k := range params {
		if !slices.Contains(creationOptionOrder, k) {
			rest = append(rest, k)
		}
	}
	sort.Strings(rest)
	return append(keys, rest...)
}

// Params of a shared control answer, empty without one
func sharedControlParams(ctl *sharedControl) map[string]any {
	if ctl == nil {
		return map[string]any{}
	}
	return map[string]any{"session_id": ctl.SessionID, "execution_generation": ctl.ExecutionGeneration, "prompt_id": ctl.PromptID}
}

// Request rewritten for the canonical gateway, a tui session.create checked against its contract
func canonicalRequest(method string, original map[string]any, contract *creationContract) (string, map[string]any, error) {
	params := make(map[string]any, len(original))
	for k, v := range original {
		params[k] = v
	}
	if method == "session.create" {
		delete(params, "cols")
		params["source"] = "tui"
		if params["request_id"] == nil {
			params["request_id"] = uuid.NewString()
		}
		if contract == nil || !slices.Contains(contract.Sources, "tui") {
			return "", nil, errors.New(tr("canonical.launch.noTuiPolicy"))
		}
		if !slices.Contains(contract.Parameters, "client_env") {
			delete(params, "client_env")
		}
		var unsupported []string
		for _, k := range orderedKeys(params) {
			if !slices.Contains(contract.Parameters, k) {
				unsupported = append(unsupported, k)
			}
		}
		if len(unsupported) > 0 {
			return "", nil, errors.New(tr("canonical.launch.unsupportedOptions", strings.Join(unsupported, ", ")))
		}
	}
	if submitMethods[method] && truthyJS(params["submission_id"]) {
		params["input_id"] = params["submission_id"]
		delete(params, "submission_id")
	}
	if method == "session.resume" || method == "session.activate" {
		method = "session.resume"
		renders := !truthyJS(params["omit_messages"])
		delete(params, "cols")
		delete(params, "omit_messages")
		if renders {
			params["display_projection"] = true
		}
	}
	return method, params, nil
}

// Value JavaScript treats as true
func truthyJS(v any) bool {
	switch t := v.(type) {
	case nil:
		return false
	case bool:
		return t
	case string:
		return t != ""
	case float64:
		return t != 0 && t == t
	case int:
		return t != 0
	case int64:
		return t != 0
	}
	return true
}

// Copy of a map with extra keys laid over it
func withKeys(base map[string]any, kv ...any) map[string]any {
	out := make(map[string]any, len(base)+len(kv)/2)
	for k, v := range base {
		out[k] = v
	}
	for i := 0; i+1 < len(kv); i += 2 {
		out[kv[i].(string)] = kv[i+1]
	}
	return out
}

// Result reshaped for the UI, receipts carrying their target and resumes their display rows
func canonicalResult(method string, value any, request map[string]any) any {
	obj, isDict := value.(map[string]any)
	if !isDict {
		return value
	}
	if submitMethods[method] && truthyJS(obj["ref"]) {
		ref, _ := obj["ref"].(map[string]any)
		return withKeys(obj, "input_id", request["input_id"], "target_profile_home", ref["profile_id"], "target_session_id", ref["session_id"])
	}
	if !resumeMethods[method] {
		return value
	}
	var stored any = obj["messages"]
	if rows, isList := obj["messages"].([]any); isList {
		out := make([]any, len(rows))
		for i, row := range rows {
			if r, isRow := row.(map[string]any); isRow {
				out[i] = withKeys(r, "text", rowText(r["content"]), "name", r["tool_name"])
			} else {
				out[i] = row
			}
		}
		stored = out
	}
	rest := make(map[string]any, len(obj))
	for k, v := range obj {
		if k != "display_messages" {
			rest[k] = v
		}
	}
	messages := stored
	if display, isList := obj["display_messages"].([]any); isList {
		messages = display
	}
	info, _ := obj["info"].(map[string]any)
	epoch, present := obj["authority_epoch"]
	epochText := "undefined"
	if present {
		epochText = "null"
		if epoch != nil {
			epochText = jsTemplateString(epoch)
		}
	}
	return withKeys(rest, "messages", messages, "stored_messages", stored, "info", withKeys(info,
		"stored_session_id", obj["stored_session_id"], "execution_epoch", epochText,
		"execution_generation", obj["execution_generation"], "running", obj["running"],
		"pending_submissions", pendingSubmissions(obj["pending"])))
}

// Text of a value as a JavaScript template literal renders it
func jsTemplateString(v any) string {
	switch t := v.(type) {
	case nil:
		return "undefined"
	case map[string]any:
		return "[object Object]"
	case []any:
		parts := make([]string, len(t))
		for i, p := range t {
			if p != nil {
				parts[i] = jsTemplateString(p)
			}
		}
		return strings.Join(parts, ",")
	}
	return jsString(v)
}

// Display text of stored message content, text parts joined and media parts named
func rowText(content any) any {
	parts, isList := content.([]any)
	if !isList {
		return content
	}
	var out []string
	for _, part := range parts {
		var text string
		switch p := part.(type) {
		case string:
			text = p
		case map[string]any:
			if s, isString := p["text"].(string); isString {
				text = s
			} else if truthyJS(p["type"]) {
				kind := jsTemplateString(p["type"])
				if strings.Contains(kind, "image") {
					text = "[image]"
				} else {
					text = "[" + kind + "]"
				}
			}
		}
		if text != "" {
			out = append(out, text)
		}
	}
	return strings.Join(out, "\n")
}

// Pending submissions with their text and target, nil when the payload has no list
func pendingSubmissions(pending any) any {
	rows, isList := pending.([]any)
	if !isList {
		return nil
	}
	out := []any{}
	for _, row := range rows {
		r, isRow := row.(map[string]any)
		if !isRow {
			continue
		}
		ref, _ := r["ref"].(map[string]any)
		out = append(out, withKeys(r, "user", r["text"], "target_profile_home", ref["profile_id"], "target_session_id", ref["session_id"]))
	}
	return out
}

// Event with its pending submissions reshaped like a resume result
func canonicalEvent(ev gatewayEventMsg) gatewayEventMsg {
	if pending := pendingSubmissions(ev.Payload["pending"]); pending != nil {
		ev.Payload = withKeys(ev.Payload, "pending_submissions", pending)
	}
	return ev
}

// Session creation options of the launch environment, flags only when truthy
func localCreationOptions(env func(string) string) map[string]any {
	options := map[string]any{}
	for _, f := range [][2]string{{"model", "ICEBATH_MODEL"}, {"provider", "ICEBATH_TUI_PROVIDER"}, {"cwd", "ICEBATH_CWD"}} {
		if v := env(f[1]); v != "" {
			options[f[0]] = v
		}
	}
	if v := env("ICEBATH_TUI_TOOLSETS"); v != "" {
		var toolsets []any
		for _, name := range strings.Split(v, ",") {
			toolsets = append(toolsets, name)
		}
		options["toolsets"] = toolsets
	}
	var skills []any
	seen := map[string]bool{}
	for _, name := range skillSplitRe.Split(env("ICEBATH_TUI_SKILLS"), -1) {
		if name = strings.TrimFunc(name, isJSSpace); name != "" && !seen[name] {
			seen[name] = true
			skills = append(skills, name)
		}
	}
	if len(skills) > 0 {
		options["skills"] = skills
	}
	if clientEnv := parseClientEnv(env("ICEBATH_TUI_CLIENT_ENV")); clientEnv != nil {
		options["client_env"] = clientEnv
	}
	if raw := env("ICEBATH_TUI_MAX_TURNS"); raw != "" {
		if turns, ok := jsNumber(raw); ok && turns == float64(int64(turns)) {
			options["max_turns"] = int64(turns)
		} else {
			options["max_turns"] = strings.TrimFunc(raw, isJSSpace)
		}
	}
	truthy := func(v string) bool {
		switch strings.ToLower(strings.TrimFunc(v, isJSSpace)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	for _, f := range [][2]string{
		{"yolo", "ICEBATH_YOLO_MODE"}, {"ignore_rules", "ICEBATH_IGNORE_RULES"}, {"safe_mode", "ICEBATH_SAFE_MODE"},
		{"ignore_user_config", "ICEBATH_IGNORE_USER_CONFIG"}, {"checkpoints", "ICEBATH_TUI_CHECKPOINTS"},
		{"pass_session_id", "ICEBATH_TUI_PASS_SESSION_ID"}, {"accept_hooks", "ICEBATH_ACCEPT_HOOKS"},
	} {
		if truthy(env(f[1])) {
			options[f[0]] = true
		}
	}
	return options
}

// Number of a string as JavaScript Number parses it, false for NaN
func jsNumber(raw string) (float64, bool) {
	s := strings.TrimFunc(raw, isJSSpace)
	if s == "" {
		return 0, true
	}
	if len(s) > 2 && s[0] == '0' {
		base := map[byte]int{'x': 16, 'X': 16, 'o': 8, 'O': 8, 'b': 2, 'B': 2}[s[1]]
		if base != 0 {
			n, err := strconv.ParseUint(s[2:], base, 64)
			return float64(n), err == nil
		}
	}
	if s == "Infinity" || s == "+Infinity" || s == "-Infinity" {
		return 0, false
	}
	if strings.Trim(s, "0123456789.eE+-") != "" {
		return 0, false
	}
	n, err := strconv.ParseFloat(s, 64)
	return n, err == nil
}

// Client environment of the launcher, only a JSON object of strings
func parseClientEnv(raw string) map[string]any {
	if raw == "" {
		return nil
	}
	var value any
	if json.Unmarshal([]byte(raw), &value) != nil {
		return nil
	}
	obj, isDict := value.(map[string]any)
	if !isDict {
		return nil
	}
	for _, v := range obj {
		if _, isString := v.(string); !isString {
			return nil
		}
	}
	return obj
}

// Tool progress mode pinned at launch, empty when unset and an error for an unknown mode
func launchToolProgress(env func(string) string) (string, error) {
	raw := strings.ToLower(strings.TrimFunc(env("ICEBATH_TUI_TOOL_PROGRESS"), isJSSpace))
	if raw == "" {
		return "", nil
	}
	if !toolProgressModes[raw] {
		return "", errors.New(tr("canonical.launch.invalidToolProgress", raw))
	}
	return raw, nil
}
