package provider

import (
	"context"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
)

// ---- OpenRouter and bare custom fallback ----

// Scheme, host, and port of a URL, the host empty on a bad port so a bound key matches nothing
func baseURLOrigin(raw string) (string, string, string) {
	parsed, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || parsed.Hostname() == "" {
		return "", "", ""
	}
	scheme := strings.ToLower(parsed.Scheme)
	port := parsed.Port()
	if port == "" {
		switch scheme {
		case "http":
			port = "80"
		case "https":
			port = "443"
		}
	}
	return scheme, strings.ToLower(parsed.Hostname()), port
}

// Terminal resolver, OpenRouter or a bare and aliased custom endpoint
func (r *resolver) resolveOpenRouterRuntime(requestedProvider, explicitAPIKey, explicitBaseURL string) *Runtime {
	modelCfg := r.modelConfig()
	cfgBaseURL, _ := modelCfg["base_url"].(string)
	cfgProvider := ""
	if s, ok := modelCfg["provider"].(string); ok {
		cfgProvider = strings.ToLower(strings.TrimSpace(s))
	}
	cfgAPIKey := ""
	for _, v := range []any{modelCfg["api_key"], modelCfg["api"]} {
		if s, ok := v.(string); ok && strings.TrimSpace(s) != "" {
			cfgAPIKey = strings.TrimSpace(s)
			break
		}
	}
	requestedNorm := strings.ToLower(strings.TrimSpace(requestedProvider))
	if requestedNorm != "" && requestedNorm != "custom" && r.resolvesToCustom(requestedNorm) {
		requestedNorm = "custom"
	}
	envOpenRouterBase := strings.TrimSpace(r.secret("OPENROUTER_BASE_URL"))
	envCustomBase := strings.TrimSpace(r.secret("CUSTOM_BASE_URL"))
	useConfigBaseURL := strings.TrimSpace(cfgBaseURL) != "" && explicitBaseURL == "" && ((requestedNorm == "auto" && (cfgProvider == "" || cfgProvider == "auto")) ||
		(requestedNorm == "custom" && r.configBaseURLTrustworthyForBareCustom(cfgBaseURL, cfgProvider)) ||
		(requestedNorm == "openrouter" && cfgProvider == "openrouter"))
	baseURL := strings.TrimSpace(explicitBaseURL)
	if baseURL == "" {
		baseURL = envCustomBase
	}
	if baseURL == "" && useConfigBaseURL {
		baseURL = strings.TrimSpace(cfgBaseURL)
	}
	if baseURL == "" {
		baseURL = envOpenRouterBase
	}
	if baseURL == "" {
		baseURL = OpenRouterBaseURL
	}
	baseURL = strings.TrimRight(baseURL, "/")
	isOpenRouterURL := HostMatches(baseURL, "openrouter.ai")
	envORTrimmed := strings.TrimRight(envOpenRouterBase, "/")
	isOpenRouterContext := isOpenRouterURL || (requestedNorm == "openrouter" && ((useConfigBaseURL && cfgProvider == "openrouter" && baseURL == strings.TrimRight(strings.TrimSpace(cfgBaseURL), "/")) ||
		((envOpenRouterBase != "" || baseURL == envOpenRouterBase) && baseURL == envORTrimmed)))
	declared, entryKey := false, ""
	var entrySrc *CommandTokenSource
	if requestedNorm == "custom" {
		declared, entryKey, entrySrc = r.owningEntryCredential(baseURL)
	}
	var candidates []string
	if isOpenRouterContext {
		openaiKey := r.envPreferDotenv("OPENAI_API_KEY")
		openaiBaseURL := strings.TrimSpace(r.secret("OPENAI_BASE_URL"))
		openaiKeyOK := false
		if Hostname(openaiBaseURL) != "" {
			s1, h1, p1 := baseURLOrigin(openaiBaseURL)
			s2, h2, p2 := baseURLOrigin(baseURL)
			openaiKeyOK = h1 != "" && s1 == s2 && h1 == h2 && p1 == p2
		} else {
			openaiKeyOK = !isOpenRouterURL || looksLikeOpenRouterKey(openaiKey)
		}
		candidates = []string{explicitAPIKey}
		if !declared {
			bound := ""
			if openaiKeyOK {
				bound = openaiKey
			}
			candidates = append(candidates, r.envPreferDotenv("OPENROUTER_API_KEY"), bound)
		}
	} else {
		candidates = []string{explicitAPIKey}
		if useConfigBaseURL {
			candidates = append(candidates, cfgAPIKey, r.modelCfgKeyEnvFor(modelCfg, baseURL))
		} else {
			candidates = append(candidates, "", "")
		}
		if !declared {
			candidates = append(candidates, r.hostGatedEnvKeyCandidates(baseURL, true)...)
		}
	}
	apiKey := ""
	for _, c := range candidates {
		if hasUsableSecret(c) {
			apiKey = strings.TrimSpace(c)
			break
		}
	}
	if apiKey == "" {
		apiKey = entryKey
	}
	source := "env/config"
	if explicitAPIKey != "" || explicitBaseURL != "" {
		source = "explicit"
	}
	cfgAPIMode := r.parseAPIMode(modelCfg["api_mode"])
	if requestedNorm != "custom" {
		mode := cfgAPIMode
		if mode == "" {
			mode = detectAPIModeForURL(baseURL)
		}
		if mode == "" {
			mode = ModeChatCompletions
		}
		return r.runtime("openrouter", mode, baseURL, apiKey, source)
	}
	if baseURL != "" && !hasUsableSecret(explicitAPIKey) {
		if pooled := r.tryResolveFromCustomPool(baseURL, "custom", cfgAPIMode, ""); pooled != nil {
			return pooled
		}
	}
	if apiKey == "" && entrySrc == nil && !isOpenRouterURL {
		apiKey = NoKeyPlaceholder
	}
	rt := r.runtime("custom", r.resolvePlainCustomAPIMode(modelCfg, baseURL), baseURL, apiKey, source)
	if apiKey == "" && entrySrc != nil {
		rt.TokenSource = entrySrc.Token
		rt.KeyCmd = entrySrc.Command
	}
	return rt
}

// ---- AWS Bedrock ----

var explicitBedrockNames = map[string]bool{"bedrock": true, "aws": true, "aws-bedrock": true, "amazon-bedrock": true, "amazon": true}

// AWS Bedrock with Mantle Responses for bare OpenAI ids, Anthropic Messages for Claude, else Converse
func (r *resolver) resolveBedrockRuntime(requestedProvider string, modelCfg map[string]any, targetModel string) (*Runtime, error) {
	isExplicit := explicitBedrockNames[requestedProvider]
	if !isExplicit && !hasAWSCredentials(false) {
		return nil, newAuthErr("bedrock", "No AWS credentials found for Bedrock. Configure one of:\n"+
			"  - AWS_ACCESS_KEY_ID + AWS_SECRET_ACCESS_KEY\n"+
			"  - AWS_PROFILE (for SSO / named profiles)\n"+
			"  - IAM instance role (EC2, ECS, Lambda)\n"+
			"Or run 'aws configure' to set up credentials.", "no_aws_credentials", false)
	}
	bedrockCfg, _ := r.cfg["bedrock"].(map[string]any)
	if bedrockCfg == nil {
		bedrockCfg = map[string]any{}
	}
	scoped := map[string]any{"bedrock": bedrockCfg}
	region := resolveBedrockRuntimeRegion(scoped)
	authSource := resolveAWSAuthEnvVar()
	if authSource == "" {
		authSource = "aws-sdk-default-chain"
	}
	guardrail := bedrockGuardrailConfig(scoped)
	currentModel := strings.TrimSpace(targetModel)
	if currentModel == "" {
		currentModel = strings.TrimSpace(cfgString(modelCfg["default"]))
	}
	hasBearer := strings.TrimSpace(os.Getenv("AWS_BEARER_TOKEN_BEDROCK")) != ""
	rt := r.runtime("bedrock", modeBedrockConverse, "https://bedrock-runtime."+region+".amazonaws.com", "aws-sdk", authSource)
	rt.Region = region
	rt.RequestedProvider = requestedProvider
	if bedrockOpenAIUsesMantle(currentModel) {
		bearer := resolveBedrockBearerToken()
		rt.APIMode = ModeCodexResponses
		rt.BaseURL = bedrockOpenAIBaseURL(region)
		rt.APIKey = "aws-sdk"
		if bearer != "" {
			rt.APIKey = bearer
			rt.Source = "AWS_BEARER_TOKEN_BEDROCK"
		}
		rt.Model = currentModel
		rt.BedrockOpenAI = true
	} else if isAnthropicBedrockModel(currentModel) && !hasBearer {
		rt.APIMode = ModeAnthropicMessages
		rt.BedrockAnthropic = true
	}
	if len(guardrail) > 0 {
		rt.GuardrailConfig = guardrail
	}
	return rt, nil
}

// ---- external process providers ----

// ExternalProcessSpec describes the CLI an agent over stdio provider launches
type ExternalProcessSpec struct {
	Command        string
	Args           []string
	CommandEnvVars []string
	ArgsEnvVar     string
}

var (
	externalProcessMu    sync.RWMutex
	externalProcessSpecs = map[string]ExternalProcessSpec{}
)

// RegisterExternalProcessSpec declares the launch details of an external process provider profile
func RegisterExternalProcessSpec(provider string, spec ExternalProcessSpec) {
	externalProcessMu.Lock()
	defer externalProcessMu.Unlock()
	externalProcessSpecs[strings.ToLower(strings.TrimSpace(provider))] = spec
}

func externalProcessSpecFor(provider string) (ExternalProcessSpec, bool) {
	externalProcessMu.RLock()
	defer externalProcessMu.RUnlock()
	spec, ok := externalProcessSpecs[provider]
	return spec, ok
}

// Keyed on the registered provider's auth type
func isExternalProcessProvider(provider string) bool {
	name := strings.ToLower(strings.TrimSpace(provider))
	if name == "" {
		return false
	}
	if p, ok := registryProfile(name); ok {
		return p.AuthType == AuthTypeExternalProcess
	}
	return false
}

// ExternalProcessLaunchEnvNames lists the env names that choose an external provider's executable or argv
func ExternalProcessLaunchEnvNames() map[string]bool {
	externalProcessMu.RLock()
	defer externalProcessMu.RUnlock()
	out := map[string]bool{}
	for _, spec := range externalProcessSpecs {
		for _, v := range spec.CommandEnvVars {
			out[v] = true
		}
		if spec.ArgsEnvVar != "" {
			out[spec.ArgsEnvVar] = true
		}
	}
	return out
}

// Absolute path of the provider CLI command, empty when nothing executable matches
func resolveExternalProcessCommand(command string) string {
	if command == "" {
		return ""
	}
	lookup := command
	if strings.Contains(command, string(os.PathSeparator)) {
		expanded := os.ExpandEnv(command)
		if strings.HasPrefix(expanded, "~") {
			if h, err := os.UserHomeDir(); err == nil {
				expanded = filepath.Join(h, strings.TrimPrefix(expanded, "~"))
			}
		}
		if abs, err := filepath.Abs(expanded); err == nil {
			lookup = abs
		}
	}
	if found, err := exec.LookPath(lookup); err == nil {
		return filepath.Clean(found)
	}
	if h, err := os.UserHomeDir(); err == nil && !strings.Contains(lookup, string(os.PathSeparator)) {
		candidate := filepath.Join(h, ".local", "bin", lookup)
		if info, err := os.Stat(candidate); err == nil && !info.IsDir() && info.Mode()&0o111 != 0 {
			return candidate
		}
	}
	return findClaudeCodeCLI(command)
}

// Split of a shell like argument string on whitespace honouring simple quotes
func splitArgs(raw string) []string {
	var out []string
	var cur strings.Builder
	quote := rune(0)
	flush := func() {
		if cur.Len() > 0 {
			out = append(out, cur.String())
			cur.Reset()
		}
	}
	for _, c := range raw {
		switch {
		case quote != 0:
			if c == quote {
				quote = 0
			} else {
				cur.WriteRune(c)
			}
		case c == '"' || c == '\'':
			quote = c
		case c == ' ' || c == '\t' || c == '\n':
			flush()
		default:
			cur.WriteRune(c)
		}
	}
	flush()
	return out
}

// Command, args, base URL, resolved command, and command env names for an external process provider
func (r *resolver) externalProcessSpec(pconfig Profile) (string, []string, string, string, []string) {
	baseURL := r.providerEnvBaseURL(pconfig)
	if baseURL == "" {
		baseURL = pconfig.BaseURL
	}
	spec, _ := externalProcessSpecFor(pconfig.Name)
	command := ""
	for _, v := range spec.CommandEnvVars {
		if val := strings.TrimSpace(os.Getenv(v)); val != "" {
			command = val
			break
		}
	}
	if command == "" {
		command = spec.Command
	}
	args := append([]string{}, spec.Args...)
	if spec.ArgsEnvVar != "" {
		if raw := strings.TrimSpace(os.Getenv(spec.ArgsEnvVar)); raw != "" {
			args = splitArgs(raw)
		}
	}
	return command, args, baseURL, resolveExternalProcessCommand(command), spec.CommandEnvVars
}

// Runtime details for a local subprocess backed provider, the subprocess owning real auth
func (r *resolver) resolveExternalProcessRuntime(provider, requestedProvider string) (*Runtime, error) {
	pconfig, ok := registryProfile(provider)
	if !ok || pconfig.AuthType != AuthTypeExternalProcess {
		return nil, newAuthErr(provider, fmt.Sprintf("Provider '%s' is not an external-process provider.", provider), "invalid_provider", false)
	}
	command, args, baseURL, resolved, envVars := r.externalProcessSpec(pconfig)
	if resolved == "" && !strings.HasPrefix(baseURL, "acp+tcp://") {
		hint := ""
		if len(envVars) > 0 {
			hint = " or set " + strings.Join(envVars, "/")
		}
		shown := command
		if shown == "" {
			shown = "(none configured)"
		}
		return nil, newAuthErr(provider, fmt.Sprintf("Could not find the '%s' CLI command '%s'. Install it%s.", provider, shown, hint), "missing_external_process_cli", false)
	}
	launch := resolved
	if launch == "" {
		launch = command
	}
	rt := r.runtime(provider, ModeChatCompletions, strings.TrimRight(baseURL, "/"), pconfig.Name, "process")
	rt.Command = launch
	rt.Args = args
	rt.RequestedProvider = requestedProvider
	return rt, nil
}

// Token minted by the forwarded source for the Azure Foundry explicit path
func materializeToken(ctx context.Context, src func(context.Context) (string, error)) string {
	if src == nil {
		return ""
	}
	token, err := src(ctx)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(token)
}
