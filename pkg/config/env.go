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
func decodeEnvBytes(raw []byte) string {
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

// Assignments of a dotenv file, empty when it is absent or unreadable
func LoadEnvFile(path string) map[string]string {
	raw, err := os.ReadFile(path)
	if err != nil {
		return map[string]string{}
	}
	return ParseEnv(decodeEnvBytes(raw))
}

// Reports whether a line assigns key, with or without an export prefix
func envLineDefines(line, key string) bool {
	trimmed := strings.TrimSpace(line)
	if strings.HasPrefix(trimmed, "export ") {
		trimmed = strings.TrimLeft(trimmed[len("export "):], " \t")
	}
	name, _, ok := strings.Cut(trimmed, "=")
	return ok && strings.TrimSpace(name) == key
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
	text := strings.TrimRight(decodeEnvBytes(raw), "\n")
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
	// provider, tool, skill or setting
	Category string
	// Model tools the variable unlocks
	Tools []string
	// Hidden from the setup checklist
	Advanced bool
}

func prov(desc, prompt, url string) EnvVar {
	return EnvVar{Description: desc, Prompt: prompt, URL: url, Password: true, Category: "provider"}
}

func provURL(label string) EnvVar {
	return EnvVar{Description: "Custom " + label + " base URL override", Prompt: label + " base URL (leave empty for default)", Category: "provider", Advanced: true}
}

func tool(desc, prompt, url string, tools ...string) EnvVar {
	return EnvVar{Description: desc, Prompt: prompt, URL: url, Password: true, Category: "tool", Tools: tools}
}

func skill(desc, prompt, url string) EnvVar {
	return EnvVar{Description: desc, Prompt: prompt, URL: url, Password: true, Category: "skill"}
}

// Every optional secret and setting the .env file may carry, keyed by variable name
var OptionalEnvVars = map[string]EnvVar{
	"OPENAI_API_KEY":        prov("OpenAI API key", "OpenAI API key", "https://platform.openai.com/api-keys"),
	"OPENAI_BASE_URL":       provURL("OpenAI"),
	"ANTHROPIC_API_KEY":     prov("Anthropic API key", "Anthropic API key", "https://console.anthropic.com/settings/keys"),
	"ANTHROPIC_BASE_URL":    provURL("Anthropic"),
	"OPENROUTER_API_KEY":    prov("OpenRouter API key for 200+ models through one endpoint", "OpenRouter API key", "https://openrouter.ai/keys"),
	"OPENROUTER_BASE_URL":   provURL("OpenRouter"),
	"NOUS_API_KEY":          prov("Nous Research inference API key", "Nous API key", "https://nousresearch.com/"),
	"NOUS_BASE_URL":         provURL("Nous Portal"),
	"GOOGLE_API_KEY":        prov("Google AI Studio API key, also read as GEMINI_API_KEY", "Google AI Studio API key", "https://aistudio.google.com/app/apikey"),
	"GEMINI_API_KEY":        prov("Google AI Studio API key, alias of GOOGLE_API_KEY", "Gemini API key", "https://aistudio.google.com/app/apikey"),
	"GEMINI_BASE_URL":       provURL("Gemini"),
	"XAI_API_KEY":           prov("xAI API key", "xAI API key", "https://console.x.ai/"),
	"XAI_BASE_URL":          provURL("xAI"),
	"DEEPSEEK_API_KEY":      prov("DeepSeek API key", "DeepSeek API key", "https://platform.deepseek.com/api_keys"),
	"DEEPSEEK_BASE_URL":     provURL("DeepSeek"),
	"GROQ_API_KEY":          prov("Groq API key", "Groq API key", "https://console.groq.com/keys"),
	"GROQ_BASE_URL":         provURL("Groq"),
	"MISTRAL_API_KEY":       prov("Mistral API key", "Mistral API key", "https://console.mistral.ai/"),
	"MISTRAL_BASE_URL":      provURL("Mistral"),
	"KIMI_API_KEY":          prov("Kimi Moonshot API key", "Kimi API key", "https://platform.moonshot.cn/"),
	"KIMI_BASE_URL":         provURL("Kimi"),
	"GLM_API_KEY":           prov("Z.AI GLM API key, also read as ZAI_API_KEY", "Z.AI GLM API key", "https://z.ai/"),
	"ZAI_API_KEY":           prov("Z.AI API key, alias of GLM_API_KEY", "Z.AI API key", "https://z.ai/"),
	"GLM_BASE_URL":          provURL("Z.AI GLM"),
	"MINIMAX_API_KEY":       prov("MiniMax API key", "MiniMax API key", "https://www.minimax.io/"),
	"MINIMAX_BASE_URL":      provURL("MiniMax"),
	"NVIDIA_API_KEY":        prov("NVIDIA NIM API key", "NVIDIA NIM API key", "https://build.nvidia.com/"),
	"NVIDIA_BASE_URL":       provURL("NVIDIA NIM"),
	"FIREWORKS_API_KEY":     prov("Fireworks AI API key", "Fireworks AI API key", "https://app.fireworks.ai/settings/users/api-keys"),
	"HF_TOKEN":              prov("Hugging Face token for Inference Providers", "Hugging Face token", "https://huggingface.co/settings/tokens"),
	"HF_BASE_URL":           provURL("Hugging Face Inference Providers"),
	"OLLAMA_API_KEY":        prov("Ollama Cloud API key", "Ollama Cloud API key", "https://ollama.com/settings"),
	"OLLAMA_BASE_URL":       provURL("Ollama"),
	"LM_API_KEY":            prov("LM Studio bearer token for auth enabled local servers", "LM Studio API key", ""),
	"LM_BASE_URL":           provURL("LM Studio"),
	"TOGETHER_API_KEY":      tool("Together AI API key for image generation in the hallmark skill", "Together AI API key", "https://api.together.ai/settings/api-keys", "image_generate"),
	"EXA_API_KEY":           tool("Exa API key for web search and contents", "Exa API key", "https://exa.ai/", "web_search", "web_extract"),
	"PARALLEL_API_KEY":      tool("Parallel API key for web search and extract", "Parallel API key", "https://parallel.ai/", "web_search", "web_extract"),
	"FIRECRAWL_API_KEY":     tool("Firecrawl API key for web search and scraping", "Firecrawl API key", "https://firecrawl.dev/", "web_search", "web_extract"),
	"FIRECRAWL_API_URL":     {Description: "Firecrawl API URL for a self hosted instance", Prompt: "Firecrawl API URL (leave empty for cloud)", Category: "tool", Advanced: true},
	"TAVILY_API_KEY":        tool("Tavily API key for web search and extract", "Tavily API key", "https://app.tavily.com/home", "web_search", "web_extract"),
	"PERPLEXITY_API_KEY":    tool("Perplexity API key for the Search API web backend", "Perplexity API key", "https://www.perplexity.ai/account/api", "web_search", "web_extract"),
	"KEENABLE_API_KEY":      tool("Keenable API key for web search and page fetch", "Keenable API key", "https://keenable.ai", "web_search", "web_extract"),
	"SEARXNG_URL":           {Description: "URL of a SearXNG instance for self hosted web search", Prompt: "SearXNG URL (e.g. http://localhost:8080)", URL: "https://searxng.github.io/searxng/", Category: "tool", Tools: []string{"web_search"}},
	"BRAVE_SEARCH_API_KEY":  tool("Brave Search API subscription token", "Brave Search subscription token", "https://brave.com/search/api/", "web_search"),
	"FAL_KEY":               tool("FAL API key for image generation", "FAL API key", "https://fal.ai/", "image_generate"),
	"GITHUB_TOKEN":          tool("GitHub token for the skills hub rate limits and publishing", "GitHub token", "https://github.com/settings/tokens"),
	"NOTION_API_KEY":        skill("Notion integration token used by the notion skill", "Notion API key", "https://www.notion.so/my-integrations"),
	"LINEAR_API_KEY":        skill("Linear personal API key used by the linear skill", "Linear API key", "https://linear.app/settings/account/security"),
	"AIRTABLE_API_KEY":      skill("Airtable personal access token used by the airtable skill", "Airtable API key", "https://airtable.com/create/tokens"),
	"TENOR_API_KEY":         skill("Tenor API key for GIF search used by the gif-search skill", "Tenor API key", "https://developers.google.com/tenor/guides/quickstart"),
	"SUDO_PASSWORD":         {Description: "Sudo password for terminal commands needing root, an explicit empty string tries empty without prompting", Prompt: "Sudo password", Password: true, Category: "setting"},
	"ICEBATH_PREFILL_MESSAGES_FILE": {Description: "Path to a JSON file of ephemeral prefill messages for few shot priming", Prompt: "Prefill messages file path", Category: "setting"},
	"ICEBATH_EPHEMERAL_SYSTEM_PROMPT": {Description: "Ephemeral system prompt injected at API call time and never persisted", Prompt: "Ephemeral system prompt", Category: "setting"},
}

// Reports whether name is a registered .env variable or a provider base url override
func IsEnvKey(name string) bool {
	_, ok := OptionalEnvVars[name]
	return ok
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
		s.vals = ParseEnv(decodeEnvBytes(raw))
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
