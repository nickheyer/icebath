package provider

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// StatusSink is the icebath status printing surface the credential sections render through
type StatusSink interface {
	Section(title string)
	Row(name string, ok bool, text string, width int, sep string)
	Detail(label string, value any)
	Print(line string)
	EnvValue(name string) string
	FirstEnvValue(names ...string) string
	EffectiveProviderLabel() string
}

// StatusContext is the state icebath status sections share
type StatusContext struct {
	Ctx    context.Context
	Out    StatusSink
	Deep   bool
	Config map[string]any
}

// ISO timestamp in the local zone as date, time and zone name, the raw value when it does not parse
func statusFormatISOTimestamp(value any) any {
	s, _ := value.(string)
	text := strings.TrimSpace(s)
	if text == "" {
		return "(unknown)"
	}
	parsed, ok := parseISO(text)
	if !ok {
		return value
	}
	return parsed.Local().Format("2006-01-02 15:04:05 MST")
}

// Millisecond epoch as a UTC isoformat string, an error where Python int or fromtimestamp raises
func statusQwenExpiry(expiresAtMS any) (string, error) {
	ms, err := pluginOAuthInt(expiresAtMS)
	if err != nil {
		return "", err
	}
	t := time.UnixMilli(ms).UTC()
	if t.Year() < 1 || t.Year() > 9999 {
		return "", fmt.Errorf("year %d is out of range", t.Year())
	}
	if t.Nanosecond() != 0 {
		return t.Format("2006-01-02T15:04:05.000000-07:00"), nil
	}
	return t.Format("2006-01-02T15:04:05-07:00"), nil
}

type statusOAuthRow struct {
	label  string
	key    string
	format func(any) (any, error)
	// nil prints regardless of login, otherwise only while the login state equals it
	gate *bool
}

type statusOAuthBlock struct {
	name   string
	status func(PoolEnv) map[string]any
	hint   string
	rows   []statusOAuthRow
}

var (
	statusGateTrue, statusGateFalse = true, false
	statusFileRefreshRows           = []statusOAuthRow{
		{label: "Auth file:", key: "auth_store"},
		{label: "Refreshed:", key: "last_refresh", format: func(v any) (any, error) { return statusFormatISOTimestamp(v), nil }},
		{label: "Error:", key: "error", gate: &statusGateFalse},
	}
	statusOAuthBlocks = []statusOAuthBlock{
		{"OpenAI Codex", GetCodexAuthStatus, "icebath model", statusFileRefreshRows},
		{"Qwen OAuth", getQwenAuthStatus, "qwen auth qwen-oauth", []statusOAuthRow{
			{label: "Auth file:", key: "auth_file"},
			{label: "Access exp:", key: "expires_at_ms", format: func(v any) (any, error) { return statusQwenExpiry(v) }},
			{label: "Error:", key: "error", gate: &statusGateFalse},
		}},
		{"MiniMax OAuth", GetMinimaxOAuthAuthStatus, "icebath auth add minimax-oauth", []statusOAuthRow{
			{label: "Region:", key: "region", gate: &statusGateTrue},
			{label: "Access exp:", key: "expires_at"},
			{label: "Error:", key: "error", gate: &statusGateFalse},
		}},
		{"xAI OAuth", GetXAIOAuthAuthStatus, "icebath auth add xai-oauth", statusFileRefreshRows},
	}
	// Each row is a display name and its env var alternates, the first set one winning
	statusAPIKeys = []struct {
		name string
		envs []string
	}{
		{"OpenRouter", []string{"OPENROUTER_API_KEY"}}, {"OpenAI", []string{"OPENAI_API_KEY"}},
		{"Google / Gemini", []string{"GOOGLE_API_KEY", "GEMINI_API_KEY"}}, {"DeepSeek", []string{"DEEPSEEK_API_KEY"}},
		{"xAI / Grok", []string{"XAI_API_KEY"}}, {"NVIDIA NIM", []string{"NVIDIA_API_KEY"}}, {"Z.AI / GLM", []string{"GLM_API_KEY"}},
		{"Kimi", []string{"KIMI_API_KEY"}}, {"StepFun Step Plan", []string{"STEPFUN_API_KEY"}}, {"MiniMax", []string{"MINIMAX_API_KEY"}},
		{"MiniMax-CN", []string{"MINIMAX_CN_API_KEY"}}, {"DeepInfra", []string{"DEEPINFRA_API_KEY"}}, {"Firecrawl", []string{"FIRECRAWL_API_KEY"}},
		{"Tavily", []string{"TAVILY_API_KEY"}}, {"Perplexity", []string{"PERPLEXITY_API_KEY"}}, {"Keenable", []string{"KEENABLE_API_KEY"}},
		{"Browser Use", []string{"BROWSER_USE_API_KEY"}}, {"Browserbase", []string{"BROWSERBASE_API_KEY"}},
		{"FAL", []string{"FAL_KEY"}}, {"ElevenLabs", []string{"ELEVENLABS_API_KEY"}}, {"GitHub", []string{"GITHUB_TOKEN"}},
	}
	statusAPIKeyProviders = []struct {
		name string
		envs []string
	}{
		{"Z.AI / GLM", []string{"GLM_API_KEY", "ZAI_API_KEY", "Z_AI_API_KEY"}}, {"Kimi / Moonshot", []string{"KIMI_API_KEY"}},
		{"StepFun Step Plan", []string{"STEPFUN_API_KEY"}}, {"MiniMax", []string{"MINIMAX_API_KEY"}},
		{"MiniMax (China)", []string{"MINIMAX_CN_API_KEY"}}, {"DeepInfra", []string{"DEEPINFRA_API_KEY"}},
	}
)

// OAuth provider row with its detail lines, each printing when its value is truthy and its gate matches the login
func statusOAuthBlockRender(out StatusSink, block statusOAuthBlock, status map[string]any) error {
	loggedIn := pyTruthy(status["logged_in"])
	text := "logged in"
	if !loggedIn {
		text = fmt.Sprintf("not logged in (run: %s)", block.hint)
	}
	out.Row(block.name, loggedIn, text, 12, "  ")
	for _, row := range block.rows {
		raw := status[row.key]
		if !pyTruthy(raw) || (row.gate != nil && *row.gate != loggedIn) {
			continue
		}
		value := raw
		if row.format != nil {
			formatted, err := row.format(raw)
			if err != nil {
				return err
			}
			value = formatted
		}
		out.Detail(row.label, value)
	}
	return nil
}
