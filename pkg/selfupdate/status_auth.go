package selfupdate

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

var (
	statusNousAuthStatusLocal = provider.GetNousAuthStatusLocal
	statusOAuthGetters        = map[string]func(provider.PoolEnv) map[string]any{
		"get_codex_auth_status":         provider.GetCodexAuthStatus,
		"get_qwen_auth_status":          provider.GetQwenAuthStatus,
		"get_minimax_oauth_auth_status": provider.GetMinimaxOAuthAuthStatus,
		"get_xai_oauth_auth_status":     provider.GetXAIOAuthAuthStatus,
	}
	statusNousAccountInfo = provider.GetNousPortalAccountInfo
	statusProbeLMStudio   = provider.ProbeLMStudioModels
)

// ISO timestamp in the local zone for status output, unknown when blank and the value itself when unparseable
func statusFormatISOTimestamp(value any) any {
	text := ""
	if s, ok := value.(string); ok {
		text = pystr.Strip(s)
	}
	if text == "" {
		return "(unknown)"
	}
	if strings.HasSuffix(text, "Z") {
		text = text[:len(text)-1] + "+00:00"
	}
	parsed, aware, err := pydatetime.ParseISOFormat(text)
	if err != nil {
		return value
	}
	if !aware {
		parsed = time.Date(parsed.Year(), parsed.Month(), parsed.Day(), parsed.Hour(), parsed.Minute(), parsed.Second(),
			parsed.Nanosecond(), time.UTC)
	}
	return config.SafeStrftime(parsed.Local(), "%Y-%m-%d %H:%M:%S %Z")
}

// UTC isoformat of a millisecond epoch
func statusQwenExpiry(expiresAtMs any) (any, error) {
	n, err := pyint.Of(expiresAtMs)
	if err != nil {
		return nil, err
	}
	seconds, _ := new(big.Rat).SetFrac(n, big.NewInt(1000)).Float64()
	t, err := pydatetime.FromTimestampIn(seconds, time.UTC, nil)
	if err != nil {
		return nil, err
	}
	return pydatetime.ISOFormatUTC(t), nil
}

// Detail row of an OAuth block, printed when its raw value is truthy and the gate is nil or equals the login state
type statusOAuthRow struct {
	label, key string
	format     func(any) (any, error)
	gate       *bool
}

type statusOAuthBlock struct {
	name, getter, hint string
	rows               []statusOAuthRow
}

var (
	statusGateLoggedIn  = true
	statusGateLoggedOut = false
	statusFileRefresh   = []statusOAuthRow{{"Auth file:", "auth_store", nil, nil},
		{"Refreshed:", "last_refresh", func(v any) (any, error) { return statusFormatISOTimestamp(v), nil }, nil},
		{"Error:", "error", nil, &statusGateLoggedOut}}
	statusOAuthBlocks = []statusOAuthBlock{
		{"OpenAI Codex", "get_codex_auth_status", "icebath model", statusFileRefresh},
		{"Qwen OAuth", "get_qwen_auth_status", "qwen auth qwen-oauth", []statusOAuthRow{{"Auth file:", "auth_file", nil, nil},
			{"Access exp:", "expires_at_ms", statusQwenExpiry, nil}, {"Error:", "error", nil, &statusGateLoggedOut}}},
		{"MiniMax OAuth", "get_minimax_oauth_auth_status", "icebath auth add minimax-oauth", []statusOAuthRow{
			{"Region:", "region", nil, &statusGateLoggedIn}, {"Access exp:", "expires_at", nil, nil},
			{"Error:", "error", nil, &statusGateLoggedOut}}},
		{"xAI OAuth", "get_xai_oauth_auth_status", "icebath auth add xai-oauth", statusFileRefresh},
	}
	statusAPIKeys = [][]string{{"OpenRouter", "OPENROUTER_API_KEY"}, {"OpenAI", "OPENAI_API_KEY"},
		{"Google / Gemini", "GOOGLE_API_KEY", "GEMINI_API_KEY"}, {"DeepSeek", "DEEPSEEK_API_KEY"}, {"xAI / Grok", "XAI_API_KEY"},
		{"NVIDIA NIM", "NVIDIA_API_KEY"}, {"Z.AI / GLM", "GLM_API_KEY"}, {"Kimi", "KIMI_API_KEY"},
		{"StepFun Step Plan", "STEPFUN_API_KEY"}, {"MiniMax", "MINIMAX_API_KEY"}, {"MiniMax-CN", "MINIMAX_CN_API_KEY"},
		{"DeepInfra", "DEEPINFRA_API_KEY"}, {"Firecrawl", "FIRECRAWL_API_KEY"}, {"Tavily", "TAVILY_API_KEY"},
		{"Perplexity", "PERPLEXITY_API_KEY"}, {"Keenable", "KEENABLE_API_KEY"}, {"Browser Use", "BROWSER_USE_API_KEY"},
		{"Browserbase", "BROWSERBASE_API_KEY"}, {"FAL", "FAL_KEY"}, {"ElevenLabs", "ELEVENLABS_API_KEY"},
		{"GitHub", "GITHUB_TOKEN"}}
	statusAPIKeyProviders = [][]string{{"Z.AI / GLM", "GLM_API_KEY", "ZAI_API_KEY", "Z_AI_API_KEY"},
		{"Kimi / Moonshot", "KIMI_API_KEY"}, {"StepFun Step Plan", "STEPFUN_API_KEY"}, {"MiniMax", "MINIMAX_API_KEY"},
		{"MiniMax (China)", "MINIMAX_CN_API_KEY"}, {"DeepInfra", "DEEPINFRA_API_KEY"}}
)

// OAuth provider row and the detail lines its gates allow
func (s *statusRun) oauthBlock(b statusOAuthBlock, st map[string]any) error {
	loggedIn := pyrepr.Truthy(st["logged_in"])
	text := "not logged in (run: " + b.hint + ")"
	if loggedIn {
		text = "logged in"
	}
	s.row(b.name, loggedIn, text, 12, "  ")
	for _, r := range b.rows {
		raw := st[r.key]
		if !pyrepr.Truthy(raw) || r.gate != nil && *r.gate != loggedIn {
			continue
		}
		value := raw
		if r.format != nil {
			var err error
			if value, err = r.format(raw); err != nil {
				return err
			}
		}
		s.detail(r.label, value)
	}
	return nil
}

func (s *statusRun) renderAPIKeys() error {
	s.section("API Keys")
	for _, entry := range statusAPIKeys {
		value, err := s.firstEnvValue(entry[1:]...)
		if err != nil {
			return err
		}
		s.row(entry[0], value != "", config.RedactKey(value), 12, "  ")
	}
	value := provider.GetAnthropicKey(s.ctx)
	s.row("Anthropic", value != "", config.RedactKey(value), 12, "  ")
	return nil
}

// Calls an auth status getter, a missing getter or a panic standing for the exception the try swallows
func statusCallGetter(getter func(provider.PoolEnv) map[string]any, env provider.PoolEnv) (out map[string]any, ok bool) {
	defer func() {
		if recover() != nil {
			out, ok = nil, false
		}
	}()
	return getter(env), true
}

func (s *statusRun) renderAuthProviders() error {
	s.section("Auth Providers")
	env := statusPoolEnv(s.ctx, s.h)
	nousStatus := map[string]any{}
	statuses := map[string]map[string]any{}
	if failed := statusRecover(func() error {
		local := statusNousAuthStatusLocal(env)
		got := map[string]map[string]any{}
		for _, b := range statusOAuthBlocks[:3] {
			st, ok := statusCallGetter(statusOAuthGetters[b.getter], env)
			if !ok {
				return errors.New("AttributeError: " + b.getter)
			}
			got[b.getter] = st
		}
		nousStatus, statuses = local, got
		return nil
	}); failed {
		nousStatus, statuses = map[string]any{}, map[string]map[string]any{}
	}
	if nousStatus == nil {
		nousStatus = map[string]any{}
	}
	if st, ok := statusCallGetter(statusOAuthGetters["get_xai_oauth_auth_status"], env); ok && st != nil {
		statuses["get_xai_oauth_auth_status"] = st
	} else {
		statuses["get_xai_oauth_auth_status"] = map[string]any{}
	}

	var info *provider.NousPortalAccountInfo
	for _, k := range []string{"logged_in", "access_token", "portal_base_url", "inference_credential_present", "error_code"} {
		if pyrepr.Truthy(nousStatus[k]) {
			statusRecover(func() error { info = statusNousAccountInfo(s.ctx, false); return nil })
			break
		}
	}
	s.nousAccountInfo = info
	loggedIn := pyrepr.Truthy(nousStatus["logged_in"]) || info != nil && info.LoggedIn
	inference := pyrepr.Truthy(nousStatus["inference_credential_present"]) || info != nil && info.InferenceCredentialPresent
	s.nousLoggedIn, s.nousInferencePresent = loggedIn, inference
	blocks := func() error {
		for _, b := range statusOAuthBlocks {
			st := statuses[b.getter]
			if st == nil {
				st = map[string]any{}
			}
			if err := s.oauthBlock(b, st); err != nil {
				return err
			}
		}
		return nil
	}
	if pyrepr.Truthy(nousStatus["free_tier"]) {
		s.row("Nous Portal", true, provider.FreeTierLabel+" · "+provider.GuestModel, 12, "  ")
		s.detail("", provider.UpgradeHint)
		if inferenceURL := nousStatus["inference_base_url"]; pyrepr.Truthy(inferenceURL) {
			s.detail("Inference:", inferenceURL)
		}
		return blocks()
	}
	nousError := nousStatus["error"]
	text := "not logged in (run: icebath portal)"
	switch {
	case loggedIn:
		text = "logged in"
	case inference:
		text = "not logged in (Nous inference key configured)"
	}
	s.row("Nous Portal", loggedIn, text, 12, "  ")
	portalURL := statusOr(nousStatus["portal_base_url"], "(unknown)")
	var infoInference any
	if info != nil {
		infoInference = info.InferenceBaseURL
	}
	inferenceURL := statusOr(nousStatus["inference_base_url"], infoInference)
	accessExp, keyExp := nousStatus["access_expires_at"], nousStatus["agent_key_expires_at"]
	refresh := "no"
	if pyrepr.Truthy(nousStatus["has_refresh_token"]) {
		refresh = "yes"
	}
	rows := []struct {
		label string
		value any
		show  bool
	}{
		{"Portal URL:", portalURL, loggedIn || !pyrepr.Equal(portalURL, "(unknown)") || pyrepr.Truthy(nousError)},
		{"Inference:", inferenceURL, inference && pyrepr.Truthy(inferenceURL)},
		{"Access exp:", statusFormatISOTimestamp(accessExp), loggedIn || pyrepr.Truthy(accessExp)},
		{"Key exp:", statusFormatISOTimestamp(keyExp), loggedIn || inference || pyrepr.Truthy(keyExp)},
		{"Refresh:", refresh, loggedIn || pyrepr.Truthy(nousStatus["has_refresh_token"])},
		{"Error:", nousError, pyrepr.Truthy(nousError)},
	}
	for _, r := range rows {
		if r.show {
			s.detail(r.label, r.value)
		}
	}
	return blocks()
}

// State text of one Nous Tool Gateway feature, the first matching rule or not configured
func statusFeatureState(f provider.NousFeatureState, nousAuth bool) string {
	switch {
	case f.ManagedByNous:
		return "active via Nous subscription"
	case f.Active:
		current := f.CurrentProvider
		if current == "" {
			current = "configured provider"
		}
		return "active via " + current
	case f.IncludedByDefault && nousAuth:
		return "included by subscription, not currently selected"
	case f.Key == "modal" && nousAuth:
		return "available via subscription (optional)"
	}
	return "not configured"
}

func (s *statusRun) renderNousGateway() error {
	if tools.ManagedNousToolsEnabled(&tools.Env{Home: s.h, Config: s.config}, false) {
		features := provider.GetNousSubscriptionFeatures(statusPoolEnv(s.ctx, s.h), s.config, false)
		s.section("Nous Tool Gateway")
		if features.NousAuthPresent {
			s.println("  Nous Portal   ✓ managed tools available")
		} else {
			s.println("  Nous Portal   ✗ not logged in")
		}
		for _, f := range features.Items() {
			s.row(f.Label, f.Available || f.Active || f.ManagedByNous, statusFeatureState(f, features.NousAuthPresent), 15, " ")
		}
		return nil
	}
	if s.nousLoggedIn || s.nousInferencePresent {
		s.section("Nous Tool Gateway")
		message := provider.FormatNousPortalEntitlementMessage(s.nousAccountInfo,
			"managed web, image, TTS, STT, browser, and Modal tools", false)
		for _, line := range pystr.Splitlines(message, false) {
			s.println("  ", line)
		}
	}
	return nil
}

func (s *statusRun) renderAPIKeyProviders() error {
	s.section("API-Key Providers")
	for _, entry := range statusAPIKeyProviders {
		value, err := s.firstEnvValue(entry[1:]...)
		if err != nil {
			return err
		}
		text := "not configured (run: icebath model)"
		if value != "" {
			text = "configured"
		}
		s.row(entry[0], value != "", text, 16, " ")
	}
	label, err := s.effectiveProviderLabel()
	if err != nil {
		return err
	}
	if label != "LM Studio" {
		return nil
	}
	var base any
	if m, ok := statusDict(s.config["model"]); ok {
		base = m["base_url"]
	}
	if !pyrepr.Truthy(base) {
		envBase, err := statusGetEnvValue(s.ctx, s.h, "LM_BASE_URL")
		if err != nil {
			return err
		}
		base = statusOr(envBase, "http://127.0.0.1:1234/v1")
	}
	baseURL, isStr := base.(string)
	if !isStr {
		return fmt.Errorf("AttributeError: '%s' object has no attribute 'strip'", pyrepr.TypeName(base))
	}
	apiKey, err := statusGetEnvValue(s.ctx, s.h, "LM_API_KEY")
	if err != nil {
		return err
	}
	models, ok, err := statusProbeLMStudio(s.ctx, apiKey, baseURL, 1500*time.Millisecond)
	msg := ""
	if err != nil {
		var authErr *provider.AuthError
		if !errors.As(err, &authErr) {
			return err
		}
		ok, msg = false, "auth rejected — set LM_API_KEY"
	} else if ok {
		msg = fmt.Sprintf("reachable (%d model(s)) at %s", len(models), baseURL)
	} else {
		msg = "unreachable at " + baseURL
	}
	s.row("LM Studio", ok, msg, 16, " ")
	return nil
}
