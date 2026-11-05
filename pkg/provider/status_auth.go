package provider

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/tools"
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

// StatusContext is the state icebath status sections share, Auth Providers filling the Nous facts the gateway section reads
type StatusContext struct {
	Ctx                  context.Context
	Out                  StatusSink
	Deep                 bool
	Config               map[string]any
	NousLoggedIn         bool
	NousInferencePresent bool
	NousAccountInfo      *NousPortalAccountInfo
}

func (c *StatusContext) ctx() context.Context {
	if c.Ctx == nil {
		return context.Background()
	}
	return c.Ctx
}

func (c *StatusContext) env() PoolEnv { return modelsPoolEnv(c.ctx()) }

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

// Nous Tool Gateway state of one feature, the first matching rule naming it
func statusFeatureState(f NousFeatureState, nousAuth bool) string {
	switch {
	case f.ManagedByNous:
		return "active via Nous subscription"
	case f.Active:
		provider := f.CurrentProvider
		if provider == "" {
			provider = "configured provider"
		}
		return "active via " + provider
	case f.IncludedByDefault && nousAuth:
		return "included by subscription, not currently selected"
	case f.Key == "modal" && nousAuth:
		return "available via subscription (optional)"
	}
	return "not configured"
}

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

// First usable Anthropic credential over the registry key env vars, the profile .env winning over the shell
func getAnthropicKey(env PoolEnv) string {
	profile, ok := registryProfileCtx(env.ctx(), "anthropic")
	if !ok {
		return ""
	}
	for _, name := range profile.KeyEnv {
		if v := GetEnvPreferDotenv(env, name); v != "" {
			return v
		}
	}
	return ""
}

// Prints the API Keys section with every known key redacted and Anthropic through its dedicated lookup
func StatusRenderAPIKeys(c *StatusContext) error {
	c.Out.Section("API Keys")
	for _, row := range statusAPIKeys {
		value := c.Out.FirstEnvValue(row.envs...)
		c.Out.Row(row.name, value != "", config.RedactKey(value), 12, "  ")
	}
	value := getAnthropicKey(c.env())
	c.Out.Row("Anthropic", value != "", config.RedactKey(value), 12, "  ")
	return nil
}

// Prints the Auth Providers section from refresh free snapshots and records the Nous login facts on the context
func StatusRenderAuthProviders(c *StatusContext) error {
	c.Out.Section("Auth Providers")
	env := c.env()
	nous := GetNousAuthStatusLocal(env)
	if nous == nil {
		nous = map[string]any{}
	}
	statuses := make([]map[string]any, len(statusOAuthBlocks))
	for i, block := range statusOAuthBlocks {
		if statuses[i] = block.status(env); statuses[i] == nil {
			statuses[i] = map[string]any{}
		}
	}
	var info *NousPortalAccountInfo
	for _, k := range []string{"logged_in", "access_token", "portal_base_url", "inference_credential_present", "error_code"} {
		if pyTruthy(nous[k]) {
			info = GetNousPortalAccountInfo(c.ctx(), false)
			break
		}
	}
	c.NousAccountInfo = info
	loggedIn := pyTruthy(nous["logged_in"]) || (info != nil && info.LoggedIn)
	inference := pyTruthy(nous["inference_credential_present"]) || (info != nil && info.InferenceCredentialPresent)
	c.NousLoggedIn, c.NousInferencePresent = loggedIn, inference
	renderBlocks := func() error {
		for i, block := range statusOAuthBlocks {
			if err := statusOAuthBlockRender(c.Out, block, statuses[i]); err != nil {
				return err
			}
		}
		return nil
	}
	if pyTruthy(nous["free_tier"]) {
		// The free tier never renders as an account login, no account ids and no refresh row
		c.Out.Row("Nous Portal", true, FreeTierLabel+" · "+GuestModel, 12, "  ")
		c.Out.Detail("", UpgradeHint)
		if inferenceURL := nous["inference_base_url"]; pyTruthy(inferenceURL) {
			c.Out.Detail("Inference:", inferenceURL)
		}
		return renderBlocks()
	}
	nousError := nous["error"]
	text := "not logged in (run: icebath portal)"
	if loggedIn {
		text = "logged in"
	} else if inference {
		text = "not logged in (Nous inference key configured)"
	}
	c.Out.Row("Nous Portal", loggedIn, text, 12, "  ")
	var portalURL any = "(unknown)"
	if pyTruthy(nous["portal_base_url"]) {
		portalURL = nous["portal_base_url"]
	}
	var inferenceURL any = nous["inference_base_url"]
	if !pyTruthy(inferenceURL) {
		inferenceURL = nil
		if info != nil {
			inferenceURL = info.InferenceBaseURL
		}
	}
	details := []struct {
		label string
		value any
		show  bool
	}{
		{"Portal URL:", portalURL, loggedIn || portalURL != "(unknown)" || pyTruthy(nousError)},
		{"Inference:", inferenceURL, inference && pyTruthy(inferenceURL)},
		{"Access exp:", statusFormatISOTimestamp(nous["access_expires_at"]), loggedIn || pyTruthy(nous["access_expires_at"])},
		{"Key exp:", statusFormatISOTimestamp(nous["agent_key_expires_at"]), loggedIn || inference || pyTruthy(nous["agent_key_expires_at"])},
		{"Refresh:", map[bool]string{true: "yes", false: "no"}[pyTruthy(nous["has_refresh_token"])], loggedIn || pyTruthy(nous["has_refresh_token"])},
		{"Error:", nousError, pyTruthy(nousError)},
	}
	for _, d := range details {
		if d.show {
			c.Out.Detail(d.label, d.value)
		}
	}
	return renderBlocks()
}

// Managed Nous tools gate of the installed account source, false when none is installed
func statusManagedNousToolsEnabled() bool {
	return tools.NousAccount != nil && tools.NousAccount.ManagedToolsEnabled(nil, false)
}

// Prints the Nous Tool Gateway section, per feature state when entitled and the entitlement message otherwise
func StatusRenderNousGateway(c *StatusContext) error {
	if statusManagedNousToolsEnabled() {
		features := GetNousSubscriptionFeatures(c.env(), c.Config, false)
		c.Out.Section("Nous Tool Gateway")
		if features.NousAuthPresent {
			c.Out.Print("  Nous Portal   ✓ managed tools available")
		} else {
			c.Out.Print("  Nous Portal   ✗ not logged in")
		}
		for _, f := range features.Items() {
			c.Out.Row(f.Label, f.Available || f.Active || f.ManagedByNous, statusFeatureState(f, features.NousAuthPresent), 15, " ")
		}
		return nil
	}
	if c.NousLoggedIn || c.NousInferencePresent {
		// Nous OAuth without entitlement or an opaque inference key cannot enable the Tool Gateway
		c.Out.Section("Nous Tool Gateway")
		message := FormatNousPortalEntitlementMessage(c.NousAccountInfo, "managed web, image, TTS, STT, browser, and Modal tools", false)
		for _, line := range pySplitLines(message) {
			c.Out.Print("  " + line)
		}
	}
	return nil
}

// Prints the API-Key Providers section and probes LM Studio only when it is the effective provider
func StatusRenderAPIKeyProviders(c *StatusContext) error {
	c.Out.Section("API-Key Providers")
	for _, row := range statusAPIKeyProviders {
		configured := c.Out.FirstEnvValue(row.envs...) != ""
		text := "not configured (run: icebath model)"
		if configured {
			text = "configured"
		}
		c.Out.Row(row.name, configured, text, 16, " ")
	}
	if c.Out.EffectiveProviderLabel() != "LM Studio" {
		return nil
	}
	var base any
	if modelCfg, ok := c.Config["model"].(map[string]any); ok {
		base = modelCfg["base_url"]
	}
	if !pyTruthy(base) {
		base = c.Out.EnvValue("LM_BASE_URL")
	}
	if !pyTruthy(base) {
		base = "http://127.0.0.1:1234/v1"
	}
	baseText := pyStr(base)
	ok, msg := false, ""
	models, found, err := ProbeLMStudioModels(c.ctx(), c.Out.EnvValue("LM_API_KEY"), baseText, 1500*time.Millisecond)
	var authErr *AuthError
	switch {
	case errors.As(err, &authErr):
		ok, msg = false, "auth rejected — set LM_API_KEY"
	case err != nil:
		return err
	case found:
		ok, msg = true, fmt.Sprintf("reachable (%d model(s)) at %s", len(models), baseText)
	default:
		msg = "unreachable at " + baseText
	}
	c.Out.Row("LM Studio", ok, msg, 16, " ")
	return nil
}
