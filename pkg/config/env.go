package config

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"unicode"
	"unicode/utf8"
)

var envNameRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Fails when key is not a portable environment variable name
func ValidateEnvName(key string) error {
	if !envNameRe.MatchString(key) {
		return fmt.Errorf("invalid environment variable name %q", key)
	}
	return nil
}

// Value with a dotenv inline comment removed, quoted values scanned to their closing quote
func stripInlineComment(value string) string {
	value = strings.TrimSpace(value)
	if value == "" {
		return value
	}
	quote := value[0]
	if quote == '\'' || quote == '"' {
		for i := 1; i < len(value); i++ {
			ch := value[i]
			if quote == '"' && ch == '\\' {
				i++
				continue
			}
			if ch == quote {
				rest := strings.TrimLeft(value[i+1:], " \t")
				if strings.HasPrefix(rest, "#") {
					return value[:i+1]
				}
				return value
			}
		}
		return value
	}
	if idx := strings.Index(value, " #"); idx >= 0 {
		return strings.TrimSpace(value[:idx])
	}
	if idx := strings.Index(value, "\t#"); idx >= 0 {
		return strings.TrimSpace(value[:idx])
	}
	return value
}

// Dotenv value with surrounding quotes removed and double quote escapes reversed
func ParseEnvValue(raw string) string {
	value := strings.TrimSpace(raw)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		quoted := value[1 : len(value)-1]
		var out strings.Builder
		for i := 0; i < len(quoted); i++ {
			if quoted[i] == '\\' && i+1 < len(quoted) && (quoted[i+1] == '"' || quoted[i+1] == '\\') {
				out.WriteByte(quoted[i+1])
				i++
				continue
			}
			out.WriteByte(quoted[i])
		}
		return out.String()
	}
	if len(value) >= 2 && value[0] == '\'' && value[len(value)-1] == '\'' {
		return value[1 : len(value)-1]
	}
	return value
}

// Assignments of dotenv text, export prefixes and comments honored
func ParseEnv(text string) map[string]string {
	out := map[string]string{}
	for _, raw := range strings.Split(text, "\n") {
		line := strings.TrimSpace(strings.TrimRight(raw, "\r"))
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		if strings.HasPrefix(line, "export ") {
			line = strings.TrimLeft(line[len("export "):], " \t")
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" {
			continue
		}
		out[key] = ParseEnvValue(stripInlineComment(value))
	}
	return out
}

// Text of a dotenv file, the BOM dropped and invalid UTF-8 read as Latin-1
func DecodeEnvBytes(raw []byte) string {
	raw = bytes.TrimPrefix(raw, []byte("\xef\xbb\xbf"))
	if utf8.Valid(raw) {
		return string(raw)
	}
	runes := make([]rune, len(raw))
	for i, b := range raw {
		runes[i] = rune(b)
	}
	return string(runes)
}

// Lines with endings normalized to one newline, assignments trimmed and comments or blanks kept verbatim
func SanitizeEnvLines(lines []string) []string {
	out := make([]string, 0, len(lines))
	for _, line := range lines {
		raw := strings.TrimRight(line, "\r\n")
		stripped := strings.TrimFunc(raw, isPySpace)
		if stripped == "" || strings.HasPrefix(stripped, "#") {
			out = append(out, raw+"\n")
			continue
		}
		out = append(out, stripped+"\n")
	}
	return out
}

// Reports whether r is whitespace to Python str.isspace
func isPySpace(r rune) bool {
	return unicode.IsSpace(r) || (r >= 0x1c && r <= 0x1f)
}

// Reports whether a line assigns key, with or without an export prefix
func envLineDefines(line, key string) bool {
	trimmed := strings.TrimFunc(line, isPySpace)
	if strings.HasPrefix(trimmed, "export ") {
		trimmed = strings.TrimLeftFunc(trimmed[len("export "):], isPySpace)
	}
	name, _, ok := strings.Cut(trimmed, "=")
	return ok && envVarPolicyName(strings.TrimFunc(name, isPySpace)) == envVarPolicyName(key)
}

// Value quoted for a dotenv line when it carries whitespace, quotes, a hash or a backslash
func QuoteEnvValue(value string) string {
	if value == "" {
		return `""`
	}
	if !strings.ContainsAny(value, " \t\n\r\"'#\\$`") {
		return value
	}
	escaped := strings.ReplaceAll(value, `\`, `\\`)
	escaped = strings.ReplaceAll(escaped, `"`, `\"`)
	return `"` + escaped + `"`
}

// Error of a credential whose bytes are not plain ASCII
type NonASCIICredentialError struct {
	Key string
}

func (e *NonASCIICredentialError) Error() string {
	return e.Key + " contains non ASCII characters, paste the credential again from its source"
}

// Fails when a secret carries characters HTTP headers cannot carry
func checkASCIICredential(key, value string) error {
	for _, r := range value {
		if r > 127 {
			return &NonASCIICredentialError{Key: key}
		}
	}
	return nil
}

var envWriteMu sync.Mutex

// Writes or replaces one assignment in a dotenv file, keeping every other line
func SetEnvValue(path, key, value string) error {
	if err := ValidateEnvName(key); err != nil {
		return err
	}
	if err := checkASCIICredential(key, value); err != nil {
		return err
	}
	envWriteMu.Lock()
	defer envWriteMu.Unlock()
	lines := readEnvLines(path)
	assignment := key + "=" + QuoteEnvValue(value)
	replaced := false
	for i, line := range lines {
		if envLineDefines(line, key) {
			if !replaced {
				lines[i] = assignment
				replaced = true
			} else {
				lines[i] = ""
			}
		}
	}
	if !replaced {
		if n := len(lines); n > 0 && strings.TrimSpace(lines[n-1]) != "" {
			lines = append(lines, "")
		}
		lines = append(lines, assignment)
	}
	return writeEnvLines(path, lines)
}

// Removes every assignment of key from a dotenv file, reporting whether one existed
func RemoveEnvValue(path, key string) (bool, error) {
	envWriteMu.Lock()
	defer envWriteMu.Unlock()
	lines := readEnvLines(path)
	var kept []string
	removed := false
	for _, line := range lines {
		if envLineDefines(line, key) {
			removed = true
			continue
		}
		kept = append(kept, line)
	}
	if !removed {
		return false, nil
	}
	return true, writeEnvLines(path, kept)
}

// Lines of a dotenv file without their line endings, none when it is absent
func readEnvLines(path string) []string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil
	}
	text := strings.TrimRight(DecodeEnvBytes(raw), "\n")
	if text == "" {
		return nil
	}
	lines := strings.Split(text, "\n")
	for i, l := range lines {
		lines[i] = strings.TrimRight(l, "\r")
	}
	return lines
}

// Writes dotenv lines atomically with owner only permissions
func writeEnvLines(path string, lines []string) error {
	var out []string
	for _, l := range lines {
		if strings.TrimSpace(l) == "" {
			out = append(out, "")
			continue
		}
		out = append(out, l)
	}
	for len(out) > 0 && out[len(out)-1] == "" {
		out = out[:len(out)-1]
	}
	content := strings.Join(out, "\n")
	if content != "" {
		content += "\n"
	}
	return AtomicWrite(path, []byte(content), 0o600)
}

// Writes data to path through a temporary sibling and a rename
func AtomicWrite(path string, data []byte, mode os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, "."+filepath.Base(path)+".*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpName, mode); err != nil {
		cleanup()
		return err
	}
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// Setting shown in the setup checklist and the keys page, with the file it routes to
type EnvVar struct {
	Description string
	Prompt      string
	URL         string
	Help        string
	// Masked when typed and shown
	Password bool
	// provider, tool, skill, messaging or setting
	Category string
	// Model tools the variable unlocks
	Tools []string
	// Hidden from the setup checklist
	Advanced bool
}

// Optional secrets and settings of the upstream table in its declaration order
var optionalEnvDecl = []NamedEnvVar{
	{"NOUS_BASE_URL", EnvVar{Description: "Nous Portal base URL override", Prompt: "Nous Portal base URL (leave empty for default)", Category: "provider", Advanced: true}},
	{"ICEBATH_ANON_API_SECRET", EnvVar{Description: "Shared secret for the Nous free-tier sign-up endpoints while they are in their gated integration phase (not needed once the gate is removed)", Prompt: "Nous free-tier shared secret (leave empty unless given one)", Password: true, Category: "provider", Advanced: true}},
	{"OPENROUTER_API_KEY", EnvVar{Description: "OpenRouter API key (for vision, web scraping helpers, and MoA)", Prompt: "OpenRouter API key", URL: "https://openrouter.ai/keys", Tools: []string{"vision_analyze"}, Password: true, Category: "provider", Advanced: true}},
	{"GOOGLE_API_KEY", EnvVar{Description: "Google AI Studio API key (also recognized as GEMINI_API_KEY)", Prompt: "Google AI Studio API key", URL: "https://aistudio.google.com/app/apikey", Password: true, Category: "provider", Advanced: true}},
	{"GEMINI_API_KEY", EnvVar{Description: "Google AI Studio API key (alias for GOOGLE_API_KEY)", Prompt: "Gemini API key", URL: "https://aistudio.google.com/app/apikey", Password: true, Category: "provider", Advanced: true}},
	{"GEMINI_BASE_URL", EnvVar{Description: "Google AI Studio base URL override", Prompt: "Gemini base URL (leave empty for default)", Category: "provider", Advanced: true}},
	{"VERTEX_CREDENTIALS_PATH", EnvVar{Description: "Path to a Google Cloud service account JSON for Vertex AI (Gemini). Vertex uses OAuth2, not a static API key — this points at the credentials Icebath mints short-lived tokens from. Falls back to GOOGLE_APPLICATION_CREDENTIALS, then to ADC (gcloud auth application-default login). Set project/region under vertex: in config.yaml.", Prompt: "Vertex service account JSON path (leave empty to use ADC / GOOGLE_APPLICATION_CREDENTIALS)", URL: "https://cloud.google.com/iam/docs/keys-create-delete", Category: "provider", Advanced: true}},
	{"XAI_API_KEY", EnvVar{Description: "xAI API key", Prompt: "xAI API key", URL: "https://console.x.ai/", Password: true, Category: "provider", Advanced: true}},
	{"XAI_BASE_URL", EnvVar{Description: "xAI base URL override", Prompt: "xAI base URL (leave empty for default)", Category: "provider", Advanced: true}},
	{"NVIDIA_API_KEY", EnvVar{Description: "NVIDIA NIM API key (build.nvidia.com or local NIM endpoint)", Prompt: "NVIDIA NIM API key", URL: "https://build.nvidia.com/", Password: true, Category: "provider", Advanced: true}},
	{"NVIDIA_BASE_URL", EnvVar{Description: "NVIDIA NIM base URL override (e.g. http://localhost:8000/v1 for local NIM)", Prompt: "NVIDIA NIM base URL (leave empty for default)", Category: "provider", Advanced: true}},
	{"LM_API_KEY", EnvVar{Description: "LM Studio bearer token for auth-enabled local servers", Prompt: "LM Studio API key / bearer token", Password: true, Category: "provider", Advanced: true}},
	{"LM_BASE_URL", EnvVar{Description: "LM Studio base URL override", Prompt: "LM Studio base URL (leave empty for default)", Category: "provider", Advanced: true}},
	{"GLM_API_KEY", EnvVar{Description: "Z.AI / GLM API key (also recognized as ZAI_API_KEY / Z_AI_API_KEY)", Prompt: "Z.AI / GLM API key", URL: "https://z.ai/", Password: true, Category: "provider", Advanced: true}},
	{"ZAI_API_KEY", EnvVar{Description: "Z.AI API key (alias for GLM_API_KEY)", Prompt: "Z.AI API key", URL: "https://z.ai/", Password: true, Category: "provider", Advanced: true}},
	{"Z_AI_API_KEY", EnvVar{Description: "Z.AI API key (alias for GLM_API_KEY)", Prompt: "Z.AI API key", URL: "https://z.ai/", Password: true, Category: "provider", Advanced: true}},
	{"GLM_BASE_URL", EnvVar{Description: "Z.AI / GLM base URL override", Prompt: "Z.AI / GLM base URL (leave empty for default)", Category: "provider", Advanced: true}},
	{"KIMI_API_KEY", EnvVar{Description: "Kimi / Moonshot API key", Prompt: "Kimi API key", URL: "https://platform.moonshot.cn/", Password: true, Category: "provider", Advanced: true}},
	{"KIMI_BASE_URL", EnvVar{Description: "Kimi / Moonshot base URL override", Prompt: "Kimi base URL (leave empty for default)", Category: "provider", Advanced: true}},
	{"KIMI_CN_API_KEY", EnvVar{Description: "Kimi / Moonshot China API key", Prompt: "Kimi (China) API key", URL: "https://platform.moonshot.cn/", Password: true, Category: "provider", Advanced: true}},
	{"STEPFUN_API_KEY", EnvVar{Description: "StepFun Step Plan API key", Prompt: "StepFun Step Plan API key", URL: "https://platform.stepfun.com/", Password: true, Category: "provider", Advanced: true}},
	{"STEPFUN_BASE_URL", EnvVar{Description: "StepFun Step Plan base URL override", Prompt: "StepFun Step Plan base URL (leave empty for default)", Category: "provider", Advanced: true}},
	{"ARCEEAI_API_KEY", EnvVar{Description: "Arcee AI API key", Prompt: "Arcee AI API key", URL: "https://chat.arcee.ai/", Password: true, Category: "provider", Advanced: true}},
	{"ARCEE_BASE_URL", EnvVar{Description: "Arcee AI base URL override", Prompt: "Arcee base URL (leave empty for default)", Category: "provider", Advanced: true}},
	{"GMI_API_KEY", EnvVar{Description: "GMI Cloud API key", Prompt: "GMI Cloud API key", URL: "https://www.gmicloud.ai/", Password: true, Category: "provider", Advanced: true}},
	{"GMI_BASE_URL", EnvVar{Description: "GMI Cloud base URL override", Prompt: "GMI Cloud base URL (leave empty for default)", Category: "provider", Advanced: true}},
	{"ACTUAL_API_KEY", EnvVar{Description: "Actual Computer inference key (ac_...)", Prompt: "Actual Computer inference key", URL: "https://actual.inc/user/keys", Password: true, Category: "provider", Advanced: true}},
	{"FIREWORKS_API_KEY", EnvVar{Description: "Fireworks AI API key", Prompt: "Fireworks AI API key", URL: "https://app.fireworks.ai/settings/users/api-keys", Password: true, Category: "provider", Advanced: true}},
	{"MINIMAX_API_KEY", EnvVar{Description: "MiniMax API key (international)", Prompt: "MiniMax API key", URL: "https://www.minimax.io/", Password: true, Category: "provider", Advanced: true}},
	{"MINIMAX_BASE_URL", EnvVar{Description: "MiniMax base URL override", Prompt: "MiniMax base URL (leave empty for default)", Category: "provider", Advanced: true}},
	{"MINIMAX_CN_API_KEY", EnvVar{Description: "MiniMax API key (China endpoint)", Prompt: "MiniMax (China) API key", URL: "https://www.minimaxi.com/", Password: true, Category: "provider", Advanced: true}},
	{"MINIMAX_CN_BASE_URL", EnvVar{Description: "MiniMax (China) base URL override", Prompt: "MiniMax (China) base URL (leave empty for default)", Category: "provider", Advanced: true}},
	{"DEEPSEEK_API_KEY", EnvVar{Description: "DeepSeek API key for direct DeepSeek access", Prompt: "DeepSeek API Key", URL: "https://platform.deepseek.com/api_keys", Password: true, Category: "provider"}},
	{"DEEPSEEK_BASE_URL", EnvVar{Description: "Custom DeepSeek API base URL (advanced)", Prompt: "DeepSeek Base URL", URL: "", Category: "provider"}},
	{"DASHSCOPE_API_KEY", EnvVar{Description: "Alibaba Cloud DashScope API key (Qwen + multi-provider models)", Prompt: "DashScope API Key", URL: "https://modelstudio.console.alibabacloud.com/", Password: true, Category: "provider"}},
	{"DASHSCOPE_BASE_URL", EnvVar{Description: "Custom DashScope base URL (default: coding-intl OpenAI-compat endpoint)", Prompt: "DashScope Base URL", URL: "", Category: "provider", Advanced: true}},
	{"ICEBATH_QWEN_BASE_URL", EnvVar{Description: "Qwen Portal base URL override (default: https://portal.qwen.ai/v1)", Prompt: "Qwen Portal base URL (leave empty for default)", Category: "provider", Advanced: true}},
	{"OPENCODE_ZEN_API_KEY", EnvVar{Description: "OpenCode Zen API key (pay-as-you-go access to curated models)", Prompt: "OpenCode Zen API key", URL: "https://opencode.ai/auth", Password: true, Category: "provider", Advanced: true}},
	{"COMMANDCODE_API_KEY", EnvVar{Description: "CommandCode API key (GOAT/Pro/Max/Provider plans — 30+ models via one key)", Prompt: "CommandCode API key", URL: "https://commandcode.ai/studio/", Password: true, Category: "provider", Advanced: true}},
	{"OPENCODE_ZEN_BASE_URL", EnvVar{Description: "OpenCode Zen base URL override", Prompt: "OpenCode Zen base URL (leave empty for default)", Category: "provider", Advanced: true}},
	{"OPENCODE_GO_API_KEY", EnvVar{Description: "OpenCode Go API key ($10/month subscription for open models)", Prompt: "OpenCode Go API key", URL: "https://opencode.ai/auth", Password: true, Category: "provider", Advanced: true}},
	{"OPENCODE_GO_BASE_URL", EnvVar{Description: "OpenCode Go base URL override", Prompt: "OpenCode Go base URL (leave empty for default)", Category: "provider", Advanced: true}},
	{"HF_TOKEN", EnvVar{Description: "Hugging Face token for Inference Providers (20+ open models via router.huggingface.co)", Prompt: "Hugging Face Token", URL: "https://huggingface.co/settings/tokens", Password: true, Category: "provider"}},
	{"HF_BASE_URL", EnvVar{Description: "Hugging Face Inference Providers base URL override", Prompt: "HF base URL (leave empty for default)", Category: "provider", Advanced: true}},
	{"OLLAMA_API_KEY", EnvVar{Description: "Ollama Cloud API key (ollama.com — cloud-hosted open models)", Prompt: "Ollama Cloud API key", URL: "https://ollama.com/settings", Password: true, Category: "provider", Advanced: true}},
	{"OLLAMA_BASE_URL", EnvVar{Description: "Ollama Cloud base URL override (default: https://ollama.com/v1)", Prompt: "Ollama base URL (leave empty for default)", Category: "provider", Advanced: true}},
	{"XIAOMI_API_KEY", EnvVar{Description: "Xiaomi MiMo API key for MiMo models (mimo-v2.5-pro, mimo-v2.5, mimo-v2-pro, mimo-v2-omni, mimo-v2-flash)", Prompt: "Xiaomi MiMo API Key", URL: "https://platform.xiaomimimo.com", Password: true, Category: "provider"}},
	{"XIAOMI_BASE_URL", EnvVar{Description: "Xiaomi MiMo base URL override (default: https://api.xiaomimimo.com/v1)", Prompt: "Xiaomi base URL (leave empty for default)", Category: "provider", Advanced: true}},
	{"UPSTAGE_API_KEY", EnvVar{Description: "Upstage API key for Solar LLM models", Prompt: "Upstage API Key", URL: "https://console.upstage.ai/api-keys", Password: true, Category: "provider"}},
	{"UPSTAGE_BASE_URL", EnvVar{Description: "Upstage base URL override (default: https://api.upstage.ai/v1)", Prompt: "Upstage base URL (leave empty for default)", Category: "provider", Advanced: true}},
	{"AWS_REGION", EnvVar{Description: "AWS region for Bedrock API calls (e.g. us-east-1, eu-central-1)", Prompt: "AWS Region", URL: "https://docs.aws.amazon.com/bedrock/latest/userguide/bedrock-regions.html", Category: "provider", Advanced: true}},
	{"AWS_PROFILE", EnvVar{Description: "AWS named profile for Bedrock authentication (from ~/.aws/credentials)", Prompt: "AWS Profile", Category: "provider", Advanced: true}},
	{"AZURE_FOUNDRY_API_KEY", EnvVar{Description: "Azure Foundry API key for custom Azure endpoints", Prompt: "Azure Foundry API Key", URL: "https://ai.azure.com/", Password: true, Category: "provider"}},
	{"AZURE_FOUNDRY_BASE_URL", EnvVar{Description: "Azure Foundry base URL (set via 'icebath model' for endpoint-specific config)", Prompt: "Azure Foundry base URL", Category: "provider", Advanced: true}},
	{"EXA_API_KEY", EnvVar{Description: "Exa API key for AI-native web search and contents", Prompt: "Exa API key", URL: "https://exa.ai/", Tools: []string{"web_search", "web_extract"}, Password: true, Category: "tool"}},
	{"PARALLEL_API_KEY", EnvVar{Description: "Parallel API key for AI-native web search and extract", Prompt: "Parallel API key", URL: "https://parallel.ai/", Tools: []string{"web_search", "web_extract"}, Password: true, Category: "tool"}},
	{"FIRECRAWL_API_KEY", EnvVar{Description: "Firecrawl API key for web search and scraping", Prompt: "Firecrawl API key", URL: "https://firecrawl.dev/", Tools: []string{"web_search", "web_extract"}, Password: true, Category: "tool"}},
	{"FIRECRAWL_API_URL", EnvVar{Description: "Firecrawl API URL for self-hosted instances (optional)", Prompt: "Firecrawl API URL (leave empty for cloud)", Category: "tool", Advanced: true}},
	{"FIRECRAWL_GATEWAY_URL", EnvVar{Description: "Exact Firecrawl tool-gateway origin override for Nous Subscribers only (optional)", Prompt: "Firecrawl gateway URL (leave empty to derive from domain)", Category: "tool", Advanced: true}},
	{"TOOL_GATEWAY_URL", EnvVar{Description: "Exact shared tool-gateway origin for on-origin vendors and media uploads (optional)", Prompt: "Shared tool-gateway URL (leave empty to derive from domain)", Category: "tool", Advanced: true}},
	{"CONNECTOR_GATEWAY_URL", EnvVar{Description: "Exact connector-gateway origin for the connectors API (optional)", Prompt: "Connector-gateway URL (leave empty to derive from domain)", Category: "tool", Advanced: true}},
	{"TOOL_GATEWAY_DOMAIN", EnvVar{Description: "Shared tool-gateway domain suffix for Nous Subscribers only, used to derive vendor hosts, e.g. nousresearch.com -> firecrawl-gateway.nousresearch.com", Prompt: "Tool-gateway domain suffix", Category: "tool", Advanced: true}},
	{"TOOL_GATEWAY_SCHEME", EnvVar{Description: "Shared tool-gateway URL scheme for Nous Subscribers only, used to derive vendor hosts (`https` by default, set `http` for local gateway testing)", Prompt: "Tool-gateway URL scheme", Category: "tool", Advanced: true}},
	{"TOOL_GATEWAY_USER_TOKEN", EnvVar{Description: "Explicit Nous Subscriber access token for tool-gateway requests (optional; otherwise read from the Icebath auth store)", Prompt: "Tool-gateway user token", Password: true, Category: "tool", Advanced: true}},
	{"TAVILY_API_KEY", EnvVar{Description: "Tavily API key for AI-native web search and extract (optional — keyless works when Tavily is selected)", Prompt: "Tavily API key", URL: "https://app.tavily.com/home", Tools: []string{"web_search", "web_extract"}, Password: true, Category: "tool"}},
	{"PERPLEXITY_API_KEY", EnvVar{Description: "Perplexity API key for the Search API web backend (ranked results + query-relevant page snippets)", Prompt: "Perplexity API key", URL: "https://www.perplexity.ai/account/api", Tools: []string{"web_search", "web_extract"}, Password: true, Category: "tool"}},
	{"KEENABLE_API_KEY", EnvVar{Description: "Keenable API key for fast independent-index web search and page fetch (optional — keyless free tier works without it)", Prompt: "Keenable API key", URL: "https://keenable.ai", Tools: []string{"web_search", "web_extract"}, Password: true, Category: "tool"}},
	{"SEARXNG_URL", EnvVar{Description: "URL of your SearXNG instance for free self-hosted web search", Prompt: "SearXNG URL (e.g. http://localhost:8080)", URL: "https://searxng.github.io/searxng/", Tools: []string{"web_search"}, Category: "tool"}},
	{"BRAVE_SEARCH_API_KEY", EnvVar{Description: "Brave Search API subscription token (free tier: 2,000 queries/mo)", Prompt: "Brave Search subscription token", URL: "https://brave.com/search/api/", Tools: []string{"web_search"}, Password: true, Category: "tool"}},
	{"BROWSERBASE_API_KEY", EnvVar{Description: "Browserbase API key for cloud browser (optional — local browser works without this)", Prompt: "Browserbase API key", URL: "https://browserbase.com/", Tools: []string{"browser_exec"}, Password: true, Category: "tool"}},
	{"BROWSERBASE_PROJECT_ID", EnvVar{Description: "Browserbase project ID (optional — only needed for cloud browser)", Prompt: "Browserbase project ID", URL: "https://browserbase.com/", Tools: []string{"browser_exec"}, Category: "tool"}},
	{"BROWSER_USE_API_KEY", EnvVar{Description: "Browser Use API key for cloud browser (optional — local browser works without this)", Prompt: "Browser Use API key", URL: "https://browser-use.com/", Tools: []string{"browser_exec"}, Password: true, Category: "tool"}},
	{"FIRECRAWL_BROWSER_TTL", EnvVar{Description: "Firecrawl browser session TTL in seconds (optional, default 300)", Prompt: "Browser session TTL (seconds)", Tools: []string{"browser_exec"}, Category: "tool"}},
	{"FAL_KEY", EnvVar{Description: "FAL API key for image and video generation", Prompt: "FAL API key", URL: "https://fal.ai/", Tools: []string{"image_generate", "video_generate"}, Password: true, Category: "tool"}},
	{"KREA_API_KEY", EnvVar{Description: "Krea API key for Krea 3 and Krea 2 image generation", Prompt: "Krea API key", URL: "https://www.krea.ai/settings/api-tokens", Tools: []string{"image_generate"}, Password: true, Category: "tool"}},
	{"VOICE_TOOLS_OPENAI_KEY", EnvVar{Description: "OpenAI API key for voice transcription (Whisper) and OpenAI TTS", Prompt: "OpenAI API Key (for Whisper STT + TTS)", URL: "https://platform.openai.com/api-keys", Tools: []string{"voice_transcription", "openai_tts"}, Password: true, Category: "tool"}},
	{"ELEVENLABS_API_KEY", EnvVar{Description: "ElevenLabs API key for premium text-to-speech voices and Scribe transcription", Prompt: "ElevenLabs API key", URL: "https://elevenlabs.io/", Tools: []string{"elevenlabs_tts", "voice_transcription"}, Password: true, Category: "tool"}},
	{"MISTRAL_API_KEY", EnvVar{Description: "Mistral API key for Voxtral TTS and transcription (STT)", Prompt: "Mistral API key", URL: "https://console.mistral.ai/", Password: true, Category: "tool"}},
	{"PORCUPINE_ACCESS_KEY", EnvVar{Description: "Picovoice access key for the Porcupine 'Hey Icebath' wake word engine (optional; openWakeWord is the free default)", Prompt: "Picovoice access key", URL: "https://console.picovoice.ai/", Password: true, Category: "tool"}},
	{"GITHUB_TOKEN", EnvVar{Description: "GitHub token for Skills Hub (higher API rate limits, skill publish)", Prompt: "GitHub Token", URL: "https://github.com/settings/tokens", Password: true, Category: "tool"}},
	{"NOTION_API_KEY", EnvVar{Description: "Notion integration token (used by the `notion` skill)", Prompt: "Notion API key", URL: "https://www.notion.so/my-integrations", Password: true, Category: "skill", Advanced: true}},
	{"LINEAR_API_KEY", EnvVar{Description: "Linear personal API key (used by the `linear` skill)", Prompt: "Linear API key", URL: "https://linear.app/settings/account/security", Password: true, Category: "skill", Advanced: true}},
	{"AIRTABLE_API_KEY", EnvVar{Description: "Airtable personal access token (used by the `airtable` skill)", Prompt: "Airtable API key", URL: "https://airtable.com/create/tokens", Password: true, Category: "skill", Advanced: true}},
	{"TENOR_API_KEY", EnvVar{Description: "Tenor API key for GIF search (used by the `gif-search` skill)", Prompt: "Tenor API key", URL: "https://developers.google.com/tenor/guides/quickstart", Password: true, Category: "skill", Advanced: true}},
	{"HONCHO_API_KEY", EnvVar{Description: "Honcho API key for AI-native persistent memory", Prompt: "Honcho API key", URL: "https://app.honcho.dev", Tools: []string{"honcho_context"}, Password: true, Category: "tool"}},
	{"HONCHO_BASE_URL", EnvVar{Description: "Base URL for self-hosted Honcho instances (no API key needed)", Prompt: "Honcho base URL (e.g. http://localhost:8000)", Category: "tool"}},
	{"HINDSIGHT_API_KEY", EnvVar{Description: "Hindsight API key for graph-aware persistent memory", Prompt: "Hindsight API key", URL: "https://hindsight.vectorize.io", Tools: []string{"hindsight_recall"}, Password: true, Category: "tool"}},
	{"HINDSIGHT_API_URL", EnvVar{Description: "Base URL for the Hindsight API (default: https://api.hindsight.vectorize.io)", Prompt: "Hindsight API URL", Category: "tool", Advanced: true}},
	{"SUPERMEMORY_API_KEY", EnvVar{Description: "Supermemory API key for conversation-scoped persistent memory", Prompt: "Supermemory API key", URL: "https://supermemory.ai", Tools: []string{"supermemory_search"}, Password: true, Category: "tool"}},
	{"MEM0_API_KEY", EnvVar{Description: "Mem0 Platform API key for semantic persistent memory", Prompt: "Mem0 API key", URL: "https://app.mem0.ai", Tools: []string{"mem0_search"}, Password: true, Category: "tool"}},
	{"RETAINDB_API_KEY", EnvVar{Description: "RetainDB API key for persistent memory", Prompt: "RetainDB API key", URL: "https://retaindb.com", Tools: []string{"retaindb_search"}, Password: true, Category: "tool"}},
	{"RETAINDB_BASE_URL", EnvVar{Description: "Base URL for self-hosted RetainDB instances (default: https://api.retaindb.com)", Prompt: "RetainDB base URL", Category: "tool", Advanced: true}},
	{"BRV_API_KEY", EnvVar{Description: "ByteRover API key (optional, for cloud sync — local-first by default)", Prompt: "ByteRover API key", URL: "https://app.byterover.dev", Tools: []string{"brv_query"}, Password: true, Category: "tool"}},
	{"OPENVIKING_API_KEY", EnvVar{Description: "OpenViking API key (leave blank for local dev mode)", Prompt: "OpenViking API key", Tools: []string{"viking_search"}, Password: true, Category: "tool"}},
	{"OPENVIKING_ENDPOINT", EnvVar{Description: "OpenViking server URL (default: http://127.0.0.1:1933)", Prompt: "OpenViking endpoint", Category: "tool", Advanced: true}},
	{"TELEGRAM_BOT_TOKEN", EnvVar{Description: "Complete Telegram bot token created by @BotFather (numeric bot ID followed by a colon and secret)", Prompt: "Telegram bot token", URL: "https://t.me/BotFather", Password: true, Category: "messaging"}},
	{"TELEGRAM_ALLOWED_USERS", EnvVar{Description: "Optional comma-separated numeric Telegram user IDs allowed immediately; leave blank to approve new users through DM pairing", Prompt: "Allowed Telegram user IDs (comma-separated)", URL: "https://t.me/userinfobot", Category: "messaging"}},
	{"TELEGRAM_PROXY", EnvVar{Description: "Proxy URL for Telegram connections (overrides HTTPS_PROXY). Supports http://, https://, socks5://", Prompt: "Telegram proxy URL (optional)", Category: "messaging"}},
	{"DISCORD_BOT_TOKEN", EnvVar{Description: "Discord bot token from Developer Portal", Prompt: "Discord bot token", URL: "https://discord.com/developers/applications", Password: true, Category: "messaging"}},
	{"DISCORD_ALLOWED_USERS", EnvVar{Description: "Comma-separated Discord user IDs allowed to use the bot", Prompt: "Allowed Discord user IDs (comma-separated)", Category: "messaging"}},
	{"DISCORD_REPLY_TO_MODE", EnvVar{Description: "Discord reply threading mode: 'off' (no reply references), 'first' (reply on first message only, default), 'all' (reply on every chunk)", Prompt: "Discord reply mode (off/first/all)", Category: "messaging"}},
	{"SLACK_BOT_TOKEN", EnvVar{Description: "Slack bot token (xoxb-). Get from OAuth & Permissions after installing your app. Required scopes: chat:write, app_mentions:read, channels:history, groups:history, im:history, im:read, im:write, mpim:history, mpim:read, users:read, files:read, files:write", Prompt: "Slack Bot Token (xoxb-...)", Help: "In your Slack app, add the required bot scopes, install the app to the workspace, then copy OAuth & Permissions > Bot User OAuth Token.", URL: "https://api.slack.com/apps", Password: true, Category: "messaging"}},
	{"SLACK_APP_TOKEN", EnvVar{Description: "Slack app-level token (xapp-) for Socket Mode. Get from Basic Information → App-Level Tokens. Also ensure Event Subscriptions include: message.im, message.channels, message.groups, message.mpim, app_mention", Prompt: "Slack App Token (xapp-...)", Help: "In your Slack app, enable Socket Mode, then create Basic Information > App-Level Tokens with the connections:write scope.", URL: "https://api.slack.com/apps", Password: true, Category: "messaging"}},
	{"SLACK_ALLOWED_USERS", EnvVar{Description: "Comma-separated Slack member IDs allowed to use Icebath, e.g. U01ABC2DEF3. Without this, Slack may connect but deny messages by default.", Prompt: "Allowed Slack member IDs", Help: "In Slack, open your profile, choose More or the three-dot menu, then Copy member ID. Add multiple IDs comma-separated.", URL: "https://api.slack.com/apps", Category: "messaging"}},
	{"MATTERMOST_URL", EnvVar{Description: "Mattermost server URL (e.g. https://mm.example.com)", Prompt: "Mattermost server URL", URL: "https://mattermost.com/deploy/", Category: "messaging"}},
	{"MATTERMOST_TOKEN", EnvVar{Description: "Mattermost bot token or personal access token", Prompt: "Mattermost bot token", Password: true, Category: "messaging"}},
	{"MATTERMOST_ALLOWED_USERS", EnvVar{Description: "Comma-separated Mattermost user IDs allowed to use the bot", Prompt: "Allowed Mattermost user IDs (comma-separated)", Category: "messaging"}},
	{"MATTERMOST_REQUIRE_MENTION", EnvVar{Description: "Require @mention in Mattermost channels (default: true). Set to false to respond to all messages.", Prompt: "Require @mention in channels", Category: "messaging"}},
	{"MATTERMOST_FREE_RESPONSE_CHANNELS", EnvVar{Description: "Comma-separated Mattermost channel IDs where bot responds without @mention", Prompt: "Free-response channel IDs (comma-separated)", Category: "messaging"}},
	{"MATRIX_HOMESERVER", EnvVar{Description: "Matrix homeserver URL (e.g. https://matrix.example.org)", Prompt: "Matrix homeserver URL", URL: "https://matrix.org/ecosystem/servers/", Category: "messaging"}},
	{"MATRIX_ACCESS_TOKEN", EnvVar{Description: "Matrix access token (preferred over password login)", Prompt: "Matrix access token", Password: true, Category: "messaging"}},
	{"MATRIX_USER_ID", EnvVar{Description: "Matrix user ID (e.g. @icebath:example.org)", Prompt: "Matrix user ID (@user:server)", Category: "messaging"}},
	{"MATRIX_ALLOWED_USERS", EnvVar{Description: "Comma-separated Matrix user IDs allowed to use the bot (@user:server format)", Prompt: "Allowed Matrix user IDs (comma-separated)", Category: "messaging"}},
	{"MATRIX_REQUIRE_MENTION", EnvVar{Description: "Require @mention in Matrix rooms (default: true). Set to false to respond to all messages.", Prompt: "Require @mention in rooms (true/false)", Category: "messaging", Advanced: true}},
	{"MATRIX_FREE_RESPONSE_ROOMS", EnvVar{Description: "Comma-separated Matrix room IDs where bot responds without @mention", Prompt: "Free-response room IDs (comma-separated)", Category: "messaging", Advanced: true}},
	{"MATRIX_AUTO_THREAD", EnvVar{Description: "Auto-create threads for messages in Matrix rooms (default: true)", Prompt: "Auto-create threads in rooms (true/false)", Category: "messaging", Advanced: true}},
	{"MATRIX_DM_AUTO_THREAD", EnvVar{Description: "Auto-create threads for DM messages in Matrix (default: false)", Prompt: "Auto-create threads in DMs (true/false)", Category: "messaging", Advanced: true}},
	{"MATRIX_DEVICE_ID", EnvVar{Description: "Stable Matrix device ID for E2EE persistence across restarts (e.g. ICEBATH_BOT)", Prompt: "Matrix device ID (stable across restarts)", Category: "messaging", Advanced: true}},
	{"MATRIX_RECOVERY_KEY", EnvVar{Description: "Matrix recovery key for cross-signing verification after device key rotation (from Element: Settings → Security → Recovery Key)", Prompt: "Matrix recovery key", Password: true, Category: "messaging", Advanced: true}},
	{"BLUEBUBBLES_SERVER_URL", EnvVar{Description: "BlueBubbles server URL for iMessage integration (e.g. http://192.168.1.10:1234)", Prompt: "BlueBubbles server URL", URL: "https://bluebubbles.app/", Category: "messaging"}},
	{"BLUEBUBBLES_PASSWORD", EnvVar{Description: "BlueBubbles server password (from BlueBubbles Server → Settings → API)", Prompt: "BlueBubbles server password", Password: true, Category: "messaging"}},
	{"BLUEBUBBLES_ALLOWED_USERS", EnvVar{Description: "Comma-separated iMessage addresses (email or phone) allowed to use the bot", Prompt: "Allowed iMessage addresses (comma-separated)", Category: "messaging"}},
	{"BLUEBUBBLES_ALLOW_ALL_USERS", EnvVar{Description: "Allow all BlueBubbles users without allowlist", Prompt: "Allow All BlueBubbles Users", Category: "messaging"}},
	{"QQ_APP_ID", EnvVar{Description: "QQ Bot App ID from QQ Open Platform (q.qq.com)", Prompt: "QQ App ID", URL: "https://q.qq.com", Category: "messaging"}},
	{"QQ_CLIENT_SECRET", EnvVar{Description: "QQ Bot Client Secret from QQ Open Platform", Prompt: "QQ Client Secret", Password: true, Category: "messaging"}},
	{"QQ_ALLOWED_USERS", EnvVar{Description: "Comma-separated QQ user IDs allowed to use the bot", Prompt: "QQ Allowed Users", Category: "messaging"}},
	{"QQ_GROUP_ALLOWED_USERS", EnvVar{Description: "Comma-separated QQ group IDs allowed to interact with the bot", Prompt: "QQ Group Allowed Users", Category: "messaging"}},
	{"QQ_ALLOW_ALL_USERS", EnvVar{Description: "Allow all QQ users without an allowlist (true/false)", Prompt: "Allow All QQ Users", Category: "messaging"}},
	{"QQBOT_HOME_CHANNEL", EnvVar{Description: "Default QQ channel/group for cron delivery and notifications", Prompt: "QQ Home Channel", Category: "messaging"}},
	{"QQBOT_HOME_CHANNEL_NAME", EnvVar{Description: "Display name for the QQ home channel", Prompt: "QQ Home Channel Name", Category: "messaging"}},
	{"QQ_SANDBOX", EnvVar{Description: "Enable QQ sandbox mode for development testing (true/false)", Prompt: "QQ Sandbox Mode", Category: "messaging"}},
	{"IRC_SERVER", EnvVar{Description: "IRC server hostname (e.g. irc.libera.chat)", Prompt: "IRC server", Category: "messaging"}},
	{"IRC_CHANNEL", EnvVar{Description: "IRC channel to join (e.g. #icebath)", Prompt: "IRC channel", Category: "messaging"}},
	{"IRC_NICKNAME", EnvVar{Description: "Bot nickname on IRC (default: icebath-bot)", Prompt: "IRC nickname", Category: "messaging"}},
	{"IRC_SERVER_PASSWORD", EnvVar{Description: "IRC server password (if required)", Prompt: "IRC server password", Password: true, Category: "messaging", Advanced: true}},
	{"IRC_NICKSERV_PASSWORD", EnvVar{Description: "NickServ password for nick identification", Prompt: "NickServ password", Password: true, Category: "messaging", Advanced: true}},
	{"GATEWAY_ALLOW_ALL_USERS", EnvVar{Description: "Allow all users to interact with messaging bots (true/false). Default: false.", Prompt: "Allow all users (true/false)", Category: "messaging", Advanced: true}},
	{"API_SERVER_ENABLED", EnvVar{Description: "Enable the OpenAI-compatible API server (true/false). Allows frontends like Open WebUI, LobeChat, etc. to connect.", Prompt: "Enable API server (true/false)", Category: "messaging", Advanced: true}},
	{"API_SERVER_KEY", EnvVar{Description: "Bearer token for API server authentication. Required whenever the API server is enabled; server refuses to start without it.", Prompt: "API server auth key", Password: true, Category: "messaging", Advanced: true}},
	{"API_SERVER_PORT", EnvVar{Description: "Port for the API server (default: 8642).", Prompt: "API server port", Category: "messaging", Advanced: true}},
	{"API_SERVER_HOST", EnvVar{Description: "Host/bind address for the API server (default: 127.0.0.1). API_SERVER_KEY is still required even on loopback binds.", Prompt: "API server host", Category: "messaging", Advanced: true}},
	{"API_SERVER_MODEL_NAME", EnvVar{Description: "Model name advertised on /v1/models. Defaults to the profile name (or 'icebath-agent' for the default profile). Useful for multi-user setups with OpenWebUI.", Prompt: "API server model name", Category: "messaging", Advanced: true}},
	{"GATEWAY_PROXY_URL", EnvVar{Description: "URL of a remote Icebath API server to forward messages to (proxy mode). When set, the gateway handles platform I/O only — all agent work is delegated to the remote server. Use for Docker E2EE containers that relay to a host agent. Also configurable via gateway.proxy_url in config.yaml.", Prompt: "Remote Icebath API server URL (e.g. http://192.168.1.100:8642)", Category: "messaging", Advanced: true}},
	{"GATEWAY_PROXY_KEY", EnvVar{Description: "Bearer token for authenticating with the remote Icebath API server (proxy mode). Must match the API_SERVER_KEY on the remote host.", Prompt: "Remote API server auth key", Password: true, Category: "messaging", Advanced: true}},
	{"WEBHOOK_ENABLED", EnvVar{Description: "Enable the webhook platform adapter for receiving events from GitHub, GitLab, etc.", Prompt: "Enable webhooks (true/false)", Category: "messaging"}},
	{"WEBHOOK_PORT", EnvVar{Description: "Port for the webhook HTTP server (default: 8644).", Prompt: "Webhook port", Category: "messaging"}},
	{"WEBHOOK_SECRET", EnvVar{Description: "Global HMAC secret for webhook signature validation (overridable per route in config.yaml).", Prompt: "Webhook secret", Password: true, Category: "messaging"}},
	{"SUDO_PASSWORD", EnvVar{Description: "Sudo password for terminal commands requiring root access; set to an explicit empty string to try empty without prompting", Prompt: "Sudo password", Password: true, Category: "setting"}},
	{"ICEBATH_PREFILL_MESSAGES_FILE", EnvVar{Description: "Path to JSON file with ephemeral prefill messages for few-shot priming", Prompt: "Prefill messages file path", Category: "setting"}},
	{"ICEBATH_EPHEMERAL_SYSTEM_PROMPT", EnvVar{Description: "Ephemeral system prompt injected at API-call time (never persisted to sessions)", Prompt: "Ephemeral system prompt", Category: "setting"}},
}

// Optional env table of setup, the keys page and the child env policy, provider and platform entries joining later
var OptionalEnvVars = NewOptionalEnvTable(optionalEnvDecl)

// Reports whether name is a registered .env variable or a provider base url override
func IsEnvKey(name string) bool {
	return OptionalEnvVars.Has(name)
}

// Secrets of one home, the .env file layered over the process environment
type Secrets struct {
	path string
	mu   sync.Mutex
	raw  []byte
	vals map[string]string
}

// Secret resolver for a home directory
func NewSecrets(home string) *Secrets {
	return &Secrets{path: filepath.Join(home, ".env")}
}

// Path of the .env file behind the resolver
func (s *Secrets) Path() string { return s.path }

// Current .env assignments, reparsed whenever the file bytes change
func (s *Secrets) File() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()
	raw, err := os.ReadFile(s.path)
	if err != nil {
		s.raw, s.vals = nil, map[string]string{}
		return map[string]string{}
	}
	if s.vals == nil || !bytes.Equal(raw, s.raw) {
		s.raw = raw
		s.vals = ParseEnv(DecodeEnvBytes(raw))
	}
	out := make(map[string]string, len(s.vals))
	for k, v := range s.vals {
		out[k] = v
	}
	return out
}

// Value of name from the .env file, then the process environment
func (s *Secrets) Get(name string) (string, bool) {
	if v, ok := s.File()[name]; ok {
		return v, true
	}
	v, ok := os.LookupEnv(name)
	return v, ok
}

// Value of name or an empty string
func (s *Secrets) String(name string) string {
	v, _ := s.Get(name)
	return v
}

// Lookup function over the resolver
func (s *Secrets) Lookup() Lookup { return s.Get }

// Writes a secret to the .env file
func (s *Secrets) Set(name, value string) error { return SetEnvValue(s.path, name, value) }

// Removes a secret from the .env file
func (s *Secrets) Remove(name string) (bool, error) { return RemoveEnvValue(s.path, name) }

// First of several variable names that resolves, with the name that answered
func (s *Secrets) First(names ...string) (string, string, bool) {
	for _, n := range names {
		if v, ok := s.Get(n); ok && strings.TrimSpace(v) != "" {
			return v, n, true
		}
	}
	return "", "", false
}

// Error of a .env file that is not owner only readable
var ErrEnvPermissions = errors.New(".env file is readable by other users")

// Tightens the .env file to owner only permissions
func SecureEnvFile(path string) error {
	st, err := os.Stat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	if st.Mode().Perm()&0o077 != 0 {
		return os.Chmod(path, 0o600)
	}
	return nil
}
