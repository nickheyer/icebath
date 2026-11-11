package selfupdate

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"time"
	"unicode/utf8"

	"github.com/nickheyer/icebath/pkg/cliui"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/home"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/internal/pyurl"
	"github.com/nickheyer/icebath/pkg/pm"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/selfupdate/updaterecord"
)

var (
	doctorAPIKeyProvidersMu    sync.Mutex
	doctorAPIKeyProvidersCache []doctorAPIKeyProvider
)

// One printed probe row, the colored glyph, the label and the dim detail
type doctorProbeLine struct {
	Glyph, Label, Detail string
}

// Result of one connectivity probe, the rows to print and the issues to append
type doctorProbeResult struct {
	Label  string
	Lines  []doctorProbeLine
	Issues []string
}

// One labelled connectivity probe of the doctor table
type doctorProbe struct {
	Label string
	Run   func() (doctorProbeResult, error)
}

// One API key provider health check row, its key variables, models URL, base URL variable and health check support
type doctorAPIKeyProvider struct {
	Name                string
	EnvVars             []string
	DefaultURL          string
	BaseEnv             string
	SupportsHealthCheck bool
}

// Dependencies the connectivity probes of one doctor run reach the network and the resolver through
type doctorProbeEnv struct {
	ctx        context.Context
	d          *Doctor
	client     *http.Client
	lookupIPv6 func(ctx context.Context, host string) ([]net.IP, error)
	tcpConnect func(addr string, timeout time.Duration) error
}

// Probe dependencies of a doctor run, a plain client, the system resolver and a real IPv6 connect
func newDoctorProbeEnv(ctx context.Context, d *Doctor) *doctorProbeEnv {
	return &doctorProbeEnv{ctx: ctx, d: d, client: pyhttpx.PlainClient(false), tcpConnect: doctorTCPConnect,
		lookupIPv6: func(ctx context.Context, host string) ([]net.IP, error) {
			return net.DefaultResolver.LookupIP(ctx, "ip6", host)
		}}
}

var doctorGlyph = map[string][2]string{"ok": {"✓", cliui.Green}, "warn": {"⚠", cliui.Yellow}, "fail": {"✗", cliui.Red}}

// One row probe result of a status, with its dim detail, its issues and an optional printed label
func (e *doctorProbeEnv) doctorConnectivityRow(name, status, detail string, issues []string, label string) doctorProbeResult {
	c := &cliui.Console{Out: e.d.W}
	g := doctorGlyph[status]
	if label == "" {
		label = name
	}
	if detail != "" {
		detail = c.Color(detail, cliui.Dim)
	}
	return doctorProbeResult{Label: name, Lines: []doctorProbeLine{{Glyph: c.Color(g[0], g[1]), Label: label, Detail: detail}},
		Issues: append([]string{}, issues...)}
}

// Probe result that prints no row
func doctorConnectivitySkip(name string) doctorProbeResult {
	return doctorProbeResult{Label: name, Lines: []doctorProbeLine{}, Issues: []string{}}
}

// Whether the OAuth path of the same provider family is logged in, making a failed API key probe non blocking
func doctorHasHealthyOAuthFallbackForAPIKeyProvider(ctx context.Context, h, providerLabel string) bool {
	env := doctorPoolEnv(ctx, h)
	var status map[string]any
	switch pystr.Lower(pystr.Strip(providerLabel)) {
	case "minimax":
		status = provider.GetMinimaxOAuthAuthStatus(env)
	case "xai":
		status = provider.GetXAIOAuthAuthStatus(env)
	default:
		return false
	}
	return pyrepr.Truthy(status["logged_in"])
}

// Builds the API key provider health check list, the static rows then every api_key profile not already present
func doctorBuildAPIKeyProvidersList(ctx context.Context) []doctorAPIKeyProvider {
	static := []doctorAPIKeyProvider{
		{"Z.AI / GLM", []string{"GLM_API_KEY", "ZAI_API_KEY", "Z_AI_API_KEY"}, "https://api.z.ai/api/paas/v4/models", "GLM_BASE_URL", true},
		{"Kimi / Moonshot", []string{"KIMI_API_KEY"}, "https://api.moonshot.ai/v1/models", "KIMI_BASE_URL", true},
		{"StepFun Step Plan", []string{"STEPFUN_API_KEY"}, "https://api.stepfun.ai/step_plan/v1/models", "STEPFUN_BASE_URL", true},
		{"Kimi / Moonshot (China)", []string{"KIMI_CN_API_KEY"}, "https://api.moonshot.cn/v1/models", "", true},
		{"Arcee AI", []string{"ARCEEAI_API_KEY"}, "https://api.arcee.ai/api/v1/models", "ARCEE_BASE_URL", true},
		{"GMI Cloud", []string{"GMI_API_KEY"}, "https://api.gmi-serving.com/v1/models", "GMI_BASE_URL", true},
		{"DeepSeek", []string{"DEEPSEEK_API_KEY"}, "https://api.deepseek.com/v1/models", "DEEPSEEK_BASE_URL", true},
		{"Hugging Face", []string{"HF_TOKEN"}, "https://router.huggingface.co/v1/models", "HF_BASE_URL", true},
		{"NVIDIA NIM", []string{"NVIDIA_API_KEY"}, "https://integrate.api.nvidia.com/v1/models", "NVIDIA_BASE_URL", true},
		{"Alibaba/DashScope", []string{"DASHSCOPE_API_KEY"}, "https://dashscope-intl.aliyuncs.com/compatible-mode/v1/models", "DASHSCOPE_BASE_URL", true},
		{"MiniMax", []string{"MINIMAX_API_KEY"}, "https://api.minimax.io/v1/models", "MINIMAX_BASE_URL", true},
		{"MiniMax (China)", []string{"MINIMAX_CN_API_KEY"}, "https://api.minimaxi.com/v1/models", "MINIMAX_CN_BASE_URL", false},
		{"Vercel AI Gateway", []string{"AI_GATEWAY_API_KEY"}, "https://ai-gateway.vercel.sh/v1/models", "AI_GATEWAY_BASE_URL", true},
		{"Kilo Code", []string{"KILOCODE_API_KEY"}, "https://api.kilo.ai/api/gateway/models", "KILOCODE_BASE_URL", true},
		{"OpenCode Zen", []string{"OPENCODE_ZEN_API_KEY"}, "https://opencode.ai/zen/v1/models", "OPENCODE_ZEN_BASE_URL", true},
		{"OpenCode Go", []string{"OPENCODE_GO_API_KEY"}, "", "OPENCODE_GO_BASE_URL", false},
	}
	knownNames := map[string]bool{}
	for _, row := range static {
		knownNames[row.Name] = true
	}
	dedicatedCanonical := map[string]bool{"anthropic": true, "openrouter": true, "bedrock": true}
	knownCanonical := map[string]bool{
		"zai": true, "kimi-coding": true, "stepfun": true, "kimi-coding-cn": true, "arcee": true, "gmi": true, "deepseek": true,
		"huggingface": true, "nvidia": true, "alibaba": true, "minimax": true, "minimax-cn": true, "ai-gateway": true,
		"kilocode": true, "opencode-zen": true, "opencode-go": true, "anthropic": true, "openrouter": true, "bedrock": true,
	}
	isURL := func(v string) bool { return strings.HasSuffix(v, "_BASE_URL") || strings.HasSuffix(v, "_URL") }
	for _, pp := range provider.ListProviders(ctx) {
		envVars := provider.ProfileEnvVars(pp)
		if pp.AuthType != provider.AuthTypeAPIKey || len(envVars) == 0 {
			continue
		}
		label := pp.Label()
		if knownNames[label] || knownCanonical[pp.Name] {
			continue
		}
		dedicated := false
		for _, alias := range append([]string{pp.Name}, pp.Aliases...) {
			dedicated = dedicated || dedicatedCanonical[provider.NormalizeProviderDef(ctx, alias)]
		}
		if dedicated {
			continue
		}
		var keyVars []string
		baseVar := ""
		for _, v := range envVars {
			switch {
			case !isURL(v):
				keyVars = append(keyVars, v)
			case baseVar == "":
				baseVar = v
			}
		}
		if len(keyVars) == 0 {
			continue
		}
		modelsURL := ""
		if pp.BaseURL != "" {
			modelsURL = pp.ModelsURL
			if modelsURL == "" {
				modelsURL = strings.TrimRight(pp.BaseURL, "/") + "/models"
			}
		}
		static = append(static, doctorAPIKeyProvider{label, keyVars, modelsURL, baseVar, pp.HealthCheck})
	}
	return static
}

// Detail and issue of each OpenRouter failure status the probe names
var doctorOpenRouterStatus = map[int][2]string{
	401: {"(invalid API key)", "Check OPENROUTER_API_KEY in .env"},
	402: {"(out of credits — payment required)",
		"OpenRouter account has insufficient credits. " +
			"Fix: run 'icebath config set model.provider <provider>' " +
			"to switch providers, or fund your OpenRouter account " +
			"at https://openrouter.ai/settings/credits"},
	429: {"(rate limited)", "OpenRouter rate limit hit — consider switching to a different provider or waiting"},
}

// Sends one request the way a bare httpx call does, a ten second limit per phase and no redirects followed
func (e *doctorProbeEnv) send(method, rawURL string, headers [][2]string, body []byte) (*pyhttpx.Response, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(e.ctx, method, rawURL, reader)
	if err != nil {
		return nil, err
	}
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}
	return pyhttpx.Do(e.client, req, 10*time.Second)
}

// Probes the OpenRouter models endpoint with the configured key
func (e *doctorProbeEnv) probeOpenRouter() (doctorProbeResult, error) {
	name := "OpenRouter API"
	key := os.Getenv("OPENROUTER_API_KEY")
	if key == "" {
		return e.doctorConnectivityRow(name, "warn", "(not configured)", nil, ""), nil
	}
	r, err := e.send(http.MethodGet, home.OpenRouterModelsURL, [][2]string{{"Authorization", "Bearer " + key}}, nil)
	if err != nil {
		return e.doctorConnectivityRow(name, "fail", "("+pyhttpx.ErrorText(err)+")", []string{"Check network connectivity"}, ""), nil
	}
	if r.StatusCode == 200 {
		return e.doctorConnectivityRow(name, "ok", "", nil, ""), nil
	}
	if known, ok := doctorOpenRouterStatus[r.StatusCode]; ok {
		return e.doctorConnectivityRow(name, "fail", known[0], []string{known[1]}, ""), nil
	}
	return e.doctorConnectivityRow(name, "fail", fmt.Sprintf("(HTTP %d)", r.StatusCode), nil, ""), nil
}

// Probes the Anthropic models endpoint with the API key or OAuth token in use
func (e *doctorProbeEnv) probeAnthropic() (doctorProbeResult, error) {
	name := "Anthropic API"
	key := provider.GetAnthropicKey(e.ctx)
	if key == "" {
		return doctorConnectivitySkip(name), nil
	}
	isOAuth := provider.IsOAuthToken(key)
	full, reduced := provider.AnthropicOAuthProbeBetas()
	headers := func(betas string) [][2]string {
		if isOAuth {
			return [][2]string{{"anthropic-version", "2023-06-01"}, {"Authorization", "Bearer " + key}, {"anthropic-beta", betas}}
		}
		return [][2]string{{"anthropic-version", "2023-06-01"}, {"x-api-key", key}}
	}
	target := "https://api.anthropic.com/v1/models"
	r, err := e.send(http.MethodGet, target, headers(full), nil)
	if err == nil && isOAuth && r.StatusCode == 400 {
		if text := pystr.Lower(r.Text()); strings.Contains(text, "long context beta") && strings.Contains(text, "not yet available") {
			r, err = e.send(http.MethodGet, target, headers(reduced), nil)
		}
	}
	if err != nil {
		return e.doctorConnectivityRow(name, "warn", "("+pyhttpx.ErrorText(err)+")", nil, ""), nil
	}
	switch r.StatusCode {
	case 200:
		return e.doctorConnectivityRow(name, "ok", "", nil, ""), nil
	case 401:
		return e.doctorConnectivityRow(name, "fail", "(invalid API key)", nil, ""), nil
	}
	return e.doctorConnectivityRow(name, "warn", "(couldn't verify)", nil, ""), nil
}

// Pads text with spaces on the right to a width counted in characters
func doctorLjust(text string, width int) string {
	if n := utf8.RuneCountInString(text); n < width {
		return text + strings.Repeat(" ", width-n)
	}
	return text
}

// Probes one generic bearer auth provider through its models endpoint or its Anthropic messages route
func (e *doctorProbeEnv) probeAPIKeyProvider(p doctorAPIKeyProvider) (doctorProbeResult, error) {
	key := ""
	for _, ev := range p.EnvVars {
		if key = os.Getenv(ev); key != "" {
			break
		}
	}
	if key == "" {
		return doctorConnectivitySkip(p.Name), nil
	}
	label := doctorLjust(p.Name, 20)
	if !p.SupportsHealthCheck {
		return e.doctorConnectivityRow(p.Name, "ok", "(key configured)", nil, label), nil
	}
	warn := func(err error) (doctorProbeResult, error) {
		return e.doctorConnectivityRow(p.Name, "warn", "("+pyhttpx.ErrorText(err)+")", nil, label), nil
	}
	base, target, headers, err := e.doctorAPIKeyRequest(key, p.BaseEnv, p.DefaultURL)
	if err != nil {
		return warn(err)
	}
	var r *pyhttpx.Response
	if strings.HasSuffix(strings.TrimRight(base, "/"), "/anthropic") {
		if r, err = e.doctorAnthropicMessagesProbe(base, key); err != nil {
			return warn(err)
		}
		switch r.StatusCode {
		case 400:
			return e.doctorConnectivityRow(p.Name, "ok", "", nil, label), nil
		case 403:
			return e.doctorConnectivityRow(p.Name, "fail", "(access denied)", []string{"Check " + p.EnvVars[0] + " in .env"}, label), nil
		}
	} else {
		if target == "" {
			return warn(errors.New("Invalid type for url.  Expected str or httpx.URL, got <class 'NoneType'>: None"))
		}
		if r, err = e.send(http.MethodGet, target, headers, nil); err != nil {
			return warn(err)
		}
	}
	if p.Name == "Alibaba/DashScope" && base == "" && r.StatusCode == 401 {
		if r, err = e.send(http.MethodGet, "https://dashscope.aliyuncs.com/compatible-mode/v1/models", headers, nil); err != nil {
			return warn(err)
		}
	}
	switch r.StatusCode {
	case 401:
		return e.doctorConnectivityRow(p.Name, "fail", "(invalid API key)", []string{"Check " + p.EnvVars[0] + " in .env"}, label), nil
	case 200:
		return e.doctorConnectivityRow(p.Name, "ok", "", nil, label), nil
	}
	return e.doctorConnectivityRow(p.Name, "warn", fmt.Sprintf("(HTTP %d)", r.StatusCode), nil, label), nil
}

// Posts a one token message to the Anthropic route of a base URL with the auth family the runtime uses
func (e *doctorProbeEnv) doctorAnthropicMessagesProbe(base, key string) (*pyhttpx.Response, error) {
	normalized, defaultQuery := provider.AnthropicBaseClientQuery(base)
	agent, err := updaterecord.UserAgent("icebath-cli")
	if err != nil {
		return nil, err
	}
	headers := [][2]string{{"anthropic-version", "2023-06-01"}, {"User-Agent", agent}}
	if provider.AnthropicRequiresBearerAuth(normalized) {
		headers = append(headers, [2]string{"Authorization", "Bearer " + key})
	} else {
		headers = append(headers, [2]string{"x-api-key", key})
	}
	model := "claude-sonnet-4-5"
	if cfgModel := e.doctorModelCfg()["default"]; pyrepr.Truthy(cfgModel) && pystr.Strip(pyrepr.Str(cfgModel)) != "" {
		model = pystr.Strip(pyrepr.Str(cfgModel))
	}
	var body bytes.Buffer
	enc := json.NewEncoder(&body)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(struct {
		Model     string              `json:"model"`
		MaxTokens int                 `json:"max_tokens"`
		Messages  []map[string]string `json:"messages"`
	}{model, 1, []map[string]string{{"role": "user", "content": "ping"}}}); err != nil {
		return nil, err
	}
	var params pyhttpx.Params
	for k, v := range defaultQuery {
		params = append(params, [2]string{k, v})
	}
	target := pyhttpx.MergeParams(normalized+"/v1/messages", params)
	headers = append(headers, [2]string{"Content-Type", "application/json"})
	return e.send(http.MethodPost, target, headers, bytes.TrimRight(body.Bytes(), "\n"))
}

// The model section of the read only config, empty when it is absent or does not load
func (e *doctorProbeEnv) doctorModelCfg() map[string]any {
	cfg, err := config.LoadReadonly(e.ctx, e.d.Home, nil)
	if err != nil {
		return map[string]any{}
	}
	if modelCfg, ok := cfg["model"].(map[string]any); ok {
		return modelCfg
	}
	return map[string]any{}
}

// Effective base, models URL and headers of a generic bearer auth probe with the vendor rewrites applied
func (e *doctorProbeEnv) doctorAPIKeyRequest(key, baseEnv, defaultURL string) (string, string, [][2]string, error) {
	base := ""
	if baseEnv != "" {
		base = os.Getenv(baseEnv)
	}
	text := func(v any) string {
		if !pyrepr.Truthy(v) {
			return ""
		}
		return pystr.Strip(pyrepr.Str(v))
	}
	if base == "" && baseEnv == "AZURE_FOUNDRY_BASE_URL" && pystr.Lower(text(e.doctorModelCfg()["provider"])) == "azure-foundry" {
		base = text(e.doctorModelCfg()["base_url"])
	}
	if base == "" && strings.HasPrefix(key, "sk-kimi-") {
		base = "https://api.kimi.com/coding/v1"
	}
	if base != "" && strings.HasSuffix(strings.TrimRight(base, "/"), "/anthropic") {
		base = provider.ToOpenAIBaseURL(base)
	}
	kimi, err := pyurl.BaseURLHostMatches(base, "api.kimi.com")
	if err != nil {
		return "", "", nil, err
	}
	if kimi && strings.HasSuffix(strings.TrimRight(base, "/"), "/coding") {
		base = strings.TrimRight(base, "/") + "/v1"
	}
	target := defaultURL
	if base != "" {
		target = strings.TrimRight(base, "/") + "/models"
	}
	agent, err := updaterecord.UserAgent("icebath-cli")
	if err != nil {
		return "", "", nil, err
	}
	if kimi, err = pyurl.BaseURLHostMatches(base, "api.kimi.com"); err != nil {
		return "", "", nil, err
	}
	if kimi {
		agent = "claude-code/0.1.0"
	}
	headers := [][2]string{{"Authorization", "Bearer " + key}, {"User-Agent", agent}}
	if target != "" {
		studio, err := pyurl.BaseURLHostMatches(target, "generativelanguage.googleapis.com")
		if err != nil {
			return "", "", nil, err
		}
		vertex, err := pyurl.BaseURLHostMatches(target, "aiplatform.googleapis.com")
		if err != nil {
			return "", "", nil, err
		}
		if studio || vertex {
			root := target
			if i := strings.LastIndex(target, "/models"); i >= 0 {
				root = target[:i]
			}
			if studio || provider.IsVertexExpressBaseURL(root) {
				target = provider.NormalizeGeminiBaseURL(root) + "/models"
				headers = [][2]string{{"User-Agent", agent}, {"x-goog-api-key", key}}
			}
		}
	}
	return base, target, headers, nil
}

// Probes the Bedrock control plane with the AWS credentials in use
func (e *doctorProbeEnv) probeBedrock() (doctorProbeResult, error) {
	name := "AWS Bedrock"
	if !provider.HasAWSCredentials(true) {
		return doctorConnectivitySkip(name), nil
	}
	authVar, region, label := provider.ResolveAWSAuthEnvVar(), provider.ResolveBedrockRegion(), doctorLjust(name, 20)
	n, err := provider.BedrockFoundationModelCount(e.ctx, region)
	if err != nil {
		errName := provider.AWSErrorClassName(err)
		return e.doctorConnectivityRow(name, "warn", "("+errName+": "+err.Error()+")",
			[]string{"AWS Bedrock: " + errName + " — check IAM permissions for bedrock:ListFoundationModels"}, label), nil
	}
	return e.doctorConnectivityRow(name, "ok", fmt.Sprintf("(%s, %s, %d models)", authVar, region, n), nil, label), nil
}

// Probes Azure Foundry Entra ID auth when the active model config selects it
func (e *doctorProbeEnv) probeAzureEntra() (doctorProbeResult, error) {
	name := "Azure Foundry (Entra ID)"
	label := doctorLjust(name, 28)
	text := func(v any) string {
		if !pyrepr.Truthy(v) {
			return ""
		}
		return pystr.Strip(pyrepr.Str(v))
	}
	cfg, err := config.LoadContext(e.ctx, e.d.Home, nil)
	if err != nil {
		return doctorConnectivitySkip(name), nil
	}
	modelCfg, ok := cfg["model"].(map[string]any)
	if !ok || pystr.Lower(text(modelCfg["provider"])) != "azure-foundry" || pystr.Lower(text(modelCfg["auth_mode"])) != "entra_id" {
		return doctorConnectivitySkip(name), nil
	}
	if !provider.HasAzureIdentityInstalled() {
		return e.doctorConnectivityRow(name, "warn", "(azure-identity not installed)", []string{"From the Icebath environment, run: " +
			pm.InstallHint("azure-identity") + ". " +
			"Then restart Icebath."}, label), nil
	}
	scope := ""
	if entraCfg, isMap := modelCfg["entra"].(map[string]any); isMap {
		scope = text(entraCfg["scope"])
	}
	if scope == "" {
		scope = provider.ScopeAIAzureDefault
	}
	entra := provider.NewEntraIdentityConfig(scope, true)
	info := provider.DescribeActiveEntraCredential(e.ctx, &entra, "", 10*time.Second)
	if pyrepr.Truthy(info["ok"]) {
		var sources []string
		switch list := info["env_sources"].(type) {
		case []string:
			sources = list
		case []any:
			for _, s := range list {
				sources = append(sources, pyrepr.Str(s))
			}
		}
		tag := strings.Join(sources, ", ")
		if tag == "" {
			tag = "default credential chain"
		}
		return e.doctorConnectivityRow(name, "ok", "("+tag+", scope="+scope+")", nil, label), nil
	}
	errText := "credential chain exhausted"
	if pyrepr.Truthy(info["error"]) {
		errText = pyrepr.Str(info["error"])
	}
	hint := "Run `az login`, set AZURE_TENANT_ID/AZURE_CLIENT_ID/AZURE_CLIENT_SECRET, or attach a managed identity to this VM."
	if pyrepr.Truthy(info["hint"]) {
		hint = pyrepr.Str(info["hint"])
	}
	return e.doctorConnectivityRow(name, "warn", "("+errText+")", []string{"Azure Foundry Entra: " + errText + ". " + hint}, label), nil
}

// The network section of the read only config, empty when it is absent or does not load
func (e *doctorProbeEnv) doctorLoadNetworkConfig() map[string]any {
	cfg, err := config.LoadReadonly(e.ctx, e.d.Home, nil)
	if err != nil {
		return map[string]any{}
	}
	if network, ok := cfg["network"].(map[string]any); ok {
		return network
	}
	return map[string]any{}
}

// Opens and closes one IPv6 TCP connection within the timeout
func doctorTCPConnect(addr string, timeout time.Duration) error {
	conn, err := (&net.Dialer{Timeout: timeout}).Dial("tcp6", addr)
	if err != nil {
		return err
	}
	return conn.Close()
}

const doctorIPv6ProbeTimeout = 2 * time.Second

// Socket error numbers that mean the host has no IPv6 route at all
func doctorNoIPv6RouteErrnos() []syscall.Errno {
	if runtime.GOOS == "windows" {
		return []syscall.Errno{10051, 10065, 10049}
	}
	return []syscall.Errno{syscall.ENETUNREACH, syscall.EHOSTUNREACH, syscall.EADDRNOTAVAIL}
}

// Detects an advertised IPv6 route that only times out and names the force_ipv4 remedy
func (e *doctorProbeEnv) probeIPv6Path() (doctorProbeResult, error) {
	name := "IPv6 route"
	if pyrepr.Truthy(e.doctorLoadNetworkConfig()["force_ipv4"]) {
		return doctorConnectivitySkip(name), nil
	}
	parsed, err := url.Parse(home.OpenRouterModelsURL)
	if err != nil {
		return doctorProbeResult{}, err
	}
	host := parsed.Hostname()
	addrs, err := e.lookupIPv6(e.ctx, host)
	if err != nil || len(addrs) == 0 {
		return doctorConnectivitySkip(name), nil
	}
	cerr := e.tcpConnect(net.JoinHostPort(addrs[0].String(), "443"), doctorIPv6ProbeTimeout)
	if cerr != nil {
		var netErr net.Error
		if errors.Is(cerr, os.ErrDeadlineExceeded) || errors.Is(cerr, syscall.ETIMEDOUT) || (errors.As(cerr, &netErr) && netErr.Timeout()) {
			remedy := "set `network.force_ipv4: true` in config.yaml (or fix the IPv6 route)"
			return e.doctorConnectivityRow(name, "warn", fmt.Sprintf("(IPv6 route to %s advertised but dead: connect timed out after "+
				"%gs — %s)", host, doctorIPv6ProbeTimeout.Seconds(), remedy),
				[]string{"Dead IPv6 route: every IPv6-first connect stalls before IPv4 answers. Fix: " + remedy}, ""), nil
		}
		var errno syscall.Errno
		if errors.As(cerr, &errno) {
			for _, none := range doctorNoIPv6RouteErrnos() {
				if errno == none {
					return e.doctorConnectivityRow(name, "ok", "(no IPv6 route — IPv4 only)", nil, ""), nil
				}
			}
		}
	}
	return e.doctorConnectivityRow(name, "ok", "(IPv6 path to "+host+" reachable)", nil, ""), nil
}

// Endpoint every GitHub token type can reach without spending quota
var doctorGitHubAPIProbeURL = "https://api.github.com/rate_limit"

// Validates a configured GITHUB_TOKEN or GH_TOKEN against api.github.com and names where a dead one lives
func (e *doctorProbeEnv) probeGitHubToken() (doctorProbeResult, error) {
	name := "GitHub token"
	variable, token := "", ""
	for _, v := range []string{"GITHUB_TOKEN", "GH_TOKEN"} {
		value, _, err := config.GetEnvValue(e.ctx, e.d.Home, v)
		if err != nil {
			return doctorProbeResult{}, err
		}
		if value != "" {
			variable, token = v, value
			break
		}
	}
	if variable == "" {
		return doctorConnectivitySkip(name), nil
	}
	where := "the environment"
	if _, inFile := config.LoadEnv(e.d.Home)[variable]; inFile {
		where = e.d.DisplayHome + "/.env"
	}
	agent, err := updaterecord.UserAgent("icebath-cli")
	if err != nil {
		return e.doctorConnectivityRow(name, "fail", "("+err.Error()+")", []string{"Check network connectivity"}, ""), nil
	}
	r, err := e.send(http.MethodGet, doctorGitHubAPIProbeURL, [][2]string{
		{"Authorization", "Bearer " + token}, {"User-Agent", agent}, {"Accept", "application/vnd.github+json"}}, nil)
	if err != nil {
		return e.doctorConnectivityRow(name, "fail", "("+pyhttpx.ErrorText(err)+")", []string{"Check network connectivity"}, ""), nil
	}
	switch r.StatusCode {
	case 200:
		return e.doctorConnectivityRow(name, "ok", "("+variable+" from "+where+" accepted by api.github.com)", nil, ""), nil
	case 401:
		return e.doctorConnectivityRow(name, "fail", "("+variable+" in "+where+" rejected by api.github.com — expired or revoked)",
			[]string{variable + " in " + where + " is expired or revoked: remove it (gh CLI login is used instead) or paste a fresh token"}, ""), nil
	}
	return e.doctorConnectivityRow(name, "fail", fmt.Sprintf("(HTTP %d from api.github.com)", r.StatusCode), nil, ""), nil
}

// Labelled probes in display order, the API key provider list built once per process
func (e *doctorProbeEnv) doctorBuildProbes() []doctorProbe {
	doctorAPIKeyProvidersMu.Lock()
	if doctorAPIKeyProvidersCache == nil {
		doctorAPIKeyProvidersCache = doctorBuildAPIKeyProvidersList(e.ctx)
	}
	rows := doctorAPIKeyProvidersCache
	doctorAPIKeyProvidersMu.Unlock()
	probes := []doctorProbe{
		{"IPv6 route", e.probeIPv6Path},
		{"OpenRouter API", e.probeOpenRouter}, {"Anthropic API", e.probeAnthropic},
	}
	for _, row := range rows {
		probes = append(probes, doctorProbe{row.Name, func() (doctorProbeResult, error) { return e.probeAPIKeyProvider(row) }})
	}
	return append(probes,
		doctorProbe{"AWS Bedrock", e.probeBedrock}, doctorProbe{"Azure Foundry (Entra ID)", e.probeAzureEntra},
		doctorProbe{"GitHub token", e.probeGitHubToken})
}

// Runs every probe on at most eight workers and returns the results in submission order
func doctorRunProbes(probes []doctorProbe) ([]doctorProbeResult, error) {
	provider.NormalizeProxyEnvVars()
	imdsPrev, imdsSet := os.LookupEnv("AWS_EC2_METADATA_DISABLED")
	os.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	defer func() {
		if imdsSet {
			os.Setenv("AWS_EC2_METADATA_DISABLED", imdsPrev)
		} else {
			os.Unsetenv("AWS_EC2_METADATA_DISABLED")
		}
	}()
	results := make([]doctorProbeResult, len(probes))
	errs := make([]error, len(probes))
	slots := make(chan struct{}, 8)
	var wg sync.WaitGroup
	for i, p := range probes {
		wg.Add(1)
		go func() {
			defer wg.Done()
			slots <- struct{}{}
			defer func() { <-slots }()
			results[i], errs[i] = p.Run()
		}()
	}
	wg.Wait()
	for _, err := range errs {
		if err != nil {
			return nil, err
		}
	}
	return results, nil
}
