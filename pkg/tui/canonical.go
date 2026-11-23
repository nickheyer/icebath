package tui

import (
	"encoding/json"
	"errors"
	"regexp"
	"strconv"
	"strings"

	"github.com/google/uuid"

	"github.com/nickheyer/icebath/pkg/tuigateway"
	"github.com/nickheyer/icebath/pkg/ui/txt"
)

// Shared control identity a prompt carries back on its answer
type sharedControl struct {
	SessionID           string
	ExecutionGeneration int64
	PromptID            string
}

var (
	toolProgressModes = map[string]bool{"off": true, "new": true, "all": true, "verbose": true}
	skillSplitRe      = regexp.MustCompile(`[,\n]`)
	unlimitedTurns    = map[string]bool{"none": true, "null": true, "unlimited": true, "infinite": true, "infinity": true, "inf": true, "\u221e": true}
)

// Params of a session.create as this window sends them, under a request id a retry can name
func createRequest(original map[string]any) map[string]any {
	params := make(map[string]any, len(original)+2)
	for k, v := range original {
		params[k] = v
	}
	params["source"] = "tui"
	if params["request_id"] == nil {
		params["request_id"] = uuid.NewString()
	}
	return params
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

// Session creation options of the launch environment, flags only when set and a cap refused when it is no whole number or unlimited spelling
func localCreationOptions(env func(string) string) (tuigateway.CanonicalCreateParams, error) {
	var options tuigateway.CanonicalCreateParams
	for _, f := range []struct {
		into **string
		env  string
	}{{&options.Model, "ICEBATH_MODEL"}, {&options.Provider, "ICEBATH_TUI_PROVIDER"}, {&options.Cwd, "ICEBATH_CWD"},
		{&options.BaseURL, "ICEBATH_TUI_BASE_URL"}, {&options.APIKey, "ICEBATH_TUI_API_KEY"}, {&options.Reasoning, "ICEBATH_TUI_REASONING"}} {
		if v := env(f.env); v != "" {
			*f.into = &v
		}
	}
	if v := env("ICEBATH_TUI_TOOLSETS"); v != "" {
		options.Toolsets = strings.Split(v, ",")
	}
	seen := map[string]bool{}
	for _, name := range skillSplitRe.Split(env("ICEBATH_TUI_SKILLS"), -1) {
		if name = strings.TrimFunc(name, isJSSpace); name != "" && !seen[name] {
			seen[name] = true
			options.Skills = append(options.Skills, name)
		}
	}
	options.ClientEnv = parseClientEnv(env("ICEBATH_TUI_CLIENT_ENV"))
	if raw := strings.TrimFunc(env("ICEBATH_TUI_MAX_TURNS"), isJSSpace); raw != "" {
		turns, err := strconv.ParseInt(raw, 10, 64)
		switch {
		case err == nil:
		case unlimitedTurns[strings.ToLower(raw)]:
			turns = 0
		default:
			return options, errors.New(tr(txt.CanonicalLaunchInvalidMaxTurns, raw))
		}
		options.MaxTurns = &turns
	}
	truthy := func(v string) bool {
		switch strings.ToLower(strings.TrimFunc(v, isJSSpace)) {
		case "1", "true", "yes", "on":
			return true
		}
		return false
	}
	for _, f := range []struct {
		into *bool
		env  string
	}{{&options.Yolo, "ICEBATH_YOLO_MODE"}, {&options.IgnoreRules, "ICEBATH_IGNORE_RULES"}, {&options.SafeMode, "ICEBATH_SAFE_MODE"},
		{&options.IgnoreUserConfig, "ICEBATH_IGNORE_USER_CONFIG"}, {&options.Checkpoints, "ICEBATH_TUI_CHECKPOINTS"},
		{&options.PassSessionID, "ICEBATH_TUI_PASS_SESSION_ID"}, {&options.AcceptHooks, "ICEBATH_ACCEPT_HOOKS"}} {
		*f.into = truthy(env(f.env))
	}
	return options, nil
}

// Client environment of the launcher, only a JSON object of strings
func parseClientEnv(raw string) map[string]string {
	var env map[string]string
	if raw == "" || json.Unmarshal([]byte(raw), &env) != nil {
		return nil
	}
	return env
}

// Tool progress mode pinned at launch, empty when unset and an error for an unknown mode
func launchToolProgress(env func(string) string) (string, error) {
	raw := strings.ToLower(strings.TrimFunc(env("ICEBATH_TUI_TOOL_PROGRESS"), isJSSpace))
	if raw == "" {
		return "", nil
	}
	if !toolProgressModes[raw] {
		return "", errors.New(tr(txt.CanonicalLaunchInvalidToolProgress, raw))
	}
	return raw, nil
}
