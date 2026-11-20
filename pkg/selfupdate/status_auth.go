package selfupdate

import (
	"errors"
	"fmt"
	"math/big"
	"strings"
	"time"

	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/errchain"
	"github.com/nickheyer/icebath/pkg/internal/pydatetime"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/provider"
)

var (
	statusOAuthGetters = map[string]func(provider.PoolEnv) map[string]any{
		"get_codex_auth_status":         provider.GetCodexAuthStatus,
		"get_qwen_auth_status":          provider.GetQwenAuthStatus,
		"get_minimax_oauth_auth_status": provider.GetMinimaxOAuthAuthStatus,
		"get_xai_oauth_auth_status":     provider.GetXAIOAuthAuthStatus,
	}
	statusProbeLMStudio = provider.ProbeLMStudioModels
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
	statuses := map[string]map[string]any{}
	if failed := statusRecover(func() error {
		got := map[string]map[string]any{}
		for _, b := range statusOAuthBlocks[:3] {
			st, ok := statusCallGetter(statusOAuthGetters[b.getter], env)
			if !ok {
				return errors.New("no status reader for " + b.getter)
			}
			got[b.getter] = st
		}
		statuses = got
		return nil
	}); failed {
		statuses = map[string]map[string]any{}
	}
	if st, ok := statusCallGetter(statusOAuthGetters["get_xai_oauth_auth_status"], env); ok && st != nil {
		statuses["get_xai_oauth_auth_status"] = st
	} else {
		statuses["get_xai_oauth_auth_status"] = map[string]any{}
	}
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
		return fmt.Errorf("the local provider base URL is %s, expected text", errchain.Kind(base))
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
