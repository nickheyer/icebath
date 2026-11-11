package selfupdate

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"time"

	"github.com/nickheyer/icebath/pkg/browser"
	"github.com/nickheyer/icebath/pkg/config"
	"github.com/nickheyer/icebath/pkg/internal/pyhttpx"
	"github.com/nickheyer/icebath/pkg/internal/pyint"
	"github.com/nickheyer/icebath/pkg/internal/pyrepr"
	"github.com/nickheyer/icebath/pkg/internal/pystr"
	"github.com/nickheyer/icebath/pkg/mcp"
	"github.com/nickheyer/icebath/pkg/provider"
	"github.com/nickheyer/icebath/pkg/tools"
)

const doctorLiveDefaultProbeTimeout = 10.0

// One metadata endpoint probe, its URL, the variable holding its key and the auth scheme
type doctorLiveKeyedSpec struct {
	Name, URL, EnvVar, Scheme string
}

// Metadata only endpoints of the keyed tool backends in probe order
var doctorLiveKeyedProbes = []doctorLiveKeyedSpec{
	{"Firecrawl", "https://api.firecrawl.dev/v2/team/credit-usage", "FIRECRAWL_API_KEY", "Bearer"},
	{"FAL", "https://fal.ai/api/models?page=1", "FAL_KEY", "Key"},
}

// TTS and STT providers that never touch the network
var doctorLiveLocalAudioProviders = map[string]bool{"": true, "local": true, "edge": true, "neutts": true, "kittentts": true, "piper": true}

// Metadata endpoints of the remote TTS and STT providers
var doctorLiveAudioProbes = map[string]doctorLiveKeyedSpec{
	"openai":     {URL: "https://api.openai.com/v1/models", EnvVar: "OPENAI_API_KEY", Scheme: "Bearer"},
	"groq":       {URL: "https://api.groq.com/openai/v1/models", EnvVar: "GROQ_API_KEY", Scheme: "Bearer"},
	"elevenlabs": {URL: "https://api.elevenlabs.io/v1/voices", EnvVar: "ELEVENLABS_API_KEY", Scheme: "xi"},
}

// Outcome of one live backend probe, the status being pass, warn, fail or skip
type doctorLiveProbeResult struct {
	Name, Status, Detail string
}

// Seams the live probes of one doctor run reach the config, the network, the browser and MCP servers through
type doctorLiveEnv struct {
	ctx                context.Context
	d                  *Doctor
	loadConfig         func() (map[string]any, error)
	httpGet            func(target string, headers [][2]string, timeout float64) (int, error)
	browserAvailable   func() bool
	launchBrowserProbe func(timeout float64) (bool, string)
	launchChromium     func(key string, opts browser.LaunchOptions) (*browser.LaunchedBrowser, string)
	stopBrowser        func(key string)
	probeMCPServer     func(name string, cfg map[string]any, timeout float64) (int, error)
}

// Live probe seams of a doctor run bound to the real config, network, browser launcher and MCP client
func newDoctorLiveEnv(ctx context.Context, d *Doctor) *doctorLiveEnv {
	e := &doctorLiveEnv{ctx: ctx, d: d}
	e.loadConfig = func() (map[string]any, error) {
		cfg, err := config.LoadReadonly(ctx, d.Home, nil)
		return map[string]any(cfg), err
	}
	e.httpGet = e.doctorLiveHTTPGet
	e.browserAvailable = e.doctorLiveBrowserAvailable
	e.launchBrowserProbe = e.doctorLaunchBrowserProbe
	e.launchChromium = func(key string, opts browser.LaunchOptions) (*browser.LaunchedBrowser, string) {
		return browser.LaunchChromium(ctx, e.toolsEnv(), key, opts)
	}
	e.stopBrowser = func(key string) { browser.StopBrowser(ctx, key) }
	e.probeMCPServer = e.doctorProbeMCPServer
	return e
}

// Tool environment of the doctor home carrying the read only config
func (e *doctorLiveEnv) toolsEnv() *tools.Env {
	cfg, err := config.LoadReadonly(e.ctx, e.d.Home, nil)
	if err != nil {
		return &tools.Env{Home: e.d.Home}
	}
	return &tools.Env{Home: e.d.Home, Config: cfg}
}

// Sends one metadata GET after sanitizing the proxy environment and returns the response status
func (e *doctorLiveEnv) doctorLiveHTTPGet(target string, headers [][2]string, timeout float64) (int, error) {
	provider.NormalizeProxyEnvVars()
	req, err := http.NewRequestWithContext(e.ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, err
	}
	for _, h := range headers {
		req.Header.Set(h[0], h[1])
	}
	resp, err := pyhttpx.Do(pyhttpx.PlainClient(false), req, time.Duration(timeout*float64(time.Second)))
	if err != nil {
		return 0, errors.New(pyhttpx.ErrorText(err))
	}
	return resp.StatusCode, nil
}

// Reports whether a Chromium exists for the local browser launcher
func (e *doctorLiveEnv) doctorLiveBrowserAvailable() bool {
	return browser.ChromiumExecutable(e.toolsEnv(), true) != ""
}

// Launches Chromium through the browser launcher, waits for its CDP endpoint and always stops it
func (e *doctorLiveEnv) doctorLaunchBrowserProbe(timeout float64) (bool, string) {
	key := "doctor-live-probe"
	limit := time.Duration(timeout * float64(time.Second))
	launched, launchErr := func() (*browser.LaunchedBrowser, string) {
		defer e.stopBrowser(key)
		return e.launchChromium(key, browser.LaunchOptions{Ephemeral: true, Timeout: &limit})
	}()
	if launched == nil {
		if launchErr == "" {
			launchErr = "launch failed"
		}
		return false, launchErr
	}
	return true, "launched + CDP ready + stopped"
}

// Connects one configured MCP server, lists its tools and returns how many it offers
func (e *doctorLiveEnv) doctorProbeMCPServer(name string, cfg map[string]any, timeout float64) (int, error) {
	found, err := mcp.ProbeSingleServerTimeout(e.ctx, e.toolsEnv(), name, cfg, timeout)
	return len(found), err
}

// Classifies a metadata response status, naming the key variable on an auth failure
func doctorLiveClassifyHTTP(name string, code int, keyHint string) doctorLiveProbeResult {
	if code >= 200 && code < 300 {
		return doctorLiveProbeResult{name, "pass", fmt.Sprintf("(HTTP %d)", code)}
	}
	if code == 401 || code == 403 {
		return doctorLiveProbeResult{name, "fail", fmt.Sprintf("(HTTP %d — check %s)", code, keyHint)}
	}
	return doctorLiveProbeResult{name, "fail", fmt.Sprintf("(HTTP %d)", code)}
}

// Probes one metadata endpoint authenticated by a single env var, skipped when the var is unset
func (e *doctorLiveEnv) doctorLiveKeyedProbe(spec doctorLiveKeyedSpec, timeout float64) (doctorLiveProbeResult, error) {
	key := pystr.Strip(os.Getenv(spec.EnvVar))
	if key == "" {
		return doctorLiveProbeResult{spec.Name, "skip", "(not configured)"}, nil
	}
	code, err := e.httpGet(spec.URL, [][2]string{{"Authorization", spec.Scheme + " " + key}}, timeout)
	if err != nil {
		return doctorLiveProbeResult{}, err
	}
	return doctorLiveClassifyHTTP(spec.Name, code, spec.EnvVar), nil
}

// Launches and stops the local browser when one is available
func (e *doctorLiveEnv) doctorLiveProbeBrowser(timeout float64) (doctorLiveProbeResult, error) {
	if !e.browserAvailable() {
		return doctorLiveProbeResult{"Browser", "skip", "(not configured)"}, nil
	}
	ok, detail := e.launchBrowserProbe(timeout)
	status := "fail"
	if ok {
		status = "pass"
	}
	return doctorLiveProbeResult{"Browser", status, "(" + detail + ")"}, nil
}

// Probes the voices or models list of the configured TTS or STT provider
func (e *doctorLiveEnv) doctorLiveProbeAudio(kind string, cfg map[string]any, timeout float64) (doctorLiveProbeResult, error) {
	name := pystr.Upper(kind)
	providerName := ""
	if section := cfg[kind]; pyrepr.Truthy(section) {
		block, ok := section.(map[string]any)
		if !ok {
			return doctorLiveProbeResult{}, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(section))
		}
		if raw := block["provider"]; pyrepr.Truthy(raw) {
			text, isStr := raw.(string)
			if !isStr {
				return doctorLiveProbeResult{}, fmt.Errorf("'%s' object has no attribute 'strip'", pyrepr.TypeName(raw))
			}
			providerName = pystr.Lower(pystr.Strip(text))
		}
	}
	if doctorLiveLocalAudioProviders[providerName] {
		shown := providerName
		if shown == "" {
			shown = "local"
		}
		return doctorLiveProbeResult{name, "skip", "(provider '" + shown + "' — no remote backend to probe)"}, nil
	}
	spec, known := doctorLiveAudioProbes[providerName]
	if !known {
		return doctorLiveProbeResult{name, "skip", "(provider '" + providerName + "' — no live probe implemented)"}, nil
	}
	key := pystr.Strip(os.Getenv(spec.EnvVar))
	if key == "" {
		return doctorLiveProbeResult{name, "warn", "(provider '" + providerName + "' configured but " + spec.EnvVar + " is not set)"}, nil
	}
	headers := [][2]string{{"Authorization", "Bearer " + key}}
	if spec.Scheme == "xi" {
		headers = [][2]string{{"xi-api-key", key}}
	}
	code, err := e.httpGet(spec.URL, headers, timeout)
	if err != nil {
		return doctorLiveProbeResult{}, err
	}
	result := doctorLiveClassifyHTTP(name, code, spec.EnvVar)
	result.Detail = "(" + providerName + ") " + result.Detail
	return result, nil
}

// Prints the row of one live probe result and records an issue for a failed one
func (e *doctorLiveEnv) doctorLiveReport(result doctorLiveProbeResult, issues *[]string) {
	switch result.Status {
	case "pass":
		doctorCheckOK(e.d.W, result.Name, result.Detail)
	case "warn":
		doctorCheckWarn(e.d.W, result.Name, result.Detail)
	case "fail":
		doctorCheckFail(e.d.W, result.Name, result.Detail)
		*issues = append(*issues, "Live probe failed: "+result.Name+" "+result.Detail)
	default:
		doctorCheckInfo(e.d.W, result.Name+" "+result.Detail+" — skipped")
	}
}

// Reports whether an error is one Python raises as the builtin TimeoutError
func doctorLiveIsTimeout(err error) bool {
	var timeout *pyhttpx.TimeoutError
	return errors.Is(err, context.DeadlineExceeded) || errors.Is(err, os.ErrDeadlineExceeded) || errors.Is(err, syscall.ETIMEDOUT) ||
		errors.As(err, &timeout)
}

// Runs one probe and reports it, a probe error becoming a fail row instead of ending the doctor run
func (e *doctorLiveEnv) doctorLiveRunOne(name string, fn func() (doctorLiveProbeResult, error), issues *[]string) doctorLiveProbeResult {
	result, err := fn()
	if err != nil {
		msg := err.Error()
		switch {
		case doctorLiveIsTimeout(err):
			result = doctorLiveProbeResult{name, "fail", "(timed out: " + msg + ")"}
		default:
			if msg == "" {
				t := reflect.TypeOf(err)
				for t.Kind() == reflect.Pointer {
					t = t.Elem()
				}
				msg = t.Name()
			}
			if strings.Contains(pystr.Lower(msg), "time") {
				result = doctorLiveProbeResult{name, "fail", "(timed out: " + msg + ")"}
			} else {
				result = doctorLiveProbeResult{name, "fail", "(" + msg + ")"}
			}
		}
	}
	e.doctorLiveReport(result, issues)
	return result
}

// Probe timeout in seconds from doctor.live_probe_timeout, the default for a value float rejects, at least one second
func doctorLiveTimeout(cfg map[string]any) (float64, error) {
	timeout := doctorLiveDefaultProbeTimeout
	if section := cfg["doctor"]; pyrepr.Truthy(section) {
		block, ok := section.(map[string]any)
		if !ok {
			return 0, fmt.Errorf("'%s' object has no attribute 'get'", pyrepr.TypeName(section))
		}
		if raw, has := block["live_probe_timeout"]; has {
			switch v := raw.(type) {
			case float64:
				timeout = v
			case int:
				timeout = float64(v)
			case int64:
				timeout = float64(v)
			case bool:
				timeout = 0
				if v {
					timeout = 1
				}
			case string:
				if parsed, err := pyint.ParseFloat(v); err == nil {
					timeout = parsed
				}
			}
		}
	}
	return max(1.0, timeout), nil
}

// Runs one bounded read only probe per configured tool backend in order, appending an issue per failed probe
func (e *doctorLiveEnv) doctorRunLiveChecks(issues *[]string) ([]doctorLiveProbeResult, error) {
	cfg, err := e.loadConfig()
	if err != nil {
		return nil, err
	}
	timeout, err := doctorLiveTimeout(cfg)
	if err != nil {
		return nil, err
	}
	doctorSection(e.d.W, "Live Backend Probes (opt-in, real calls)")
	var results []doctorLiveProbeResult
	for _, spec := range doctorLiveKeyedProbes {
		results = append(results, e.doctorLiveRunOne(spec.Name, func() (doctorLiveProbeResult, error) {
			return e.doctorLiveKeyedProbe(spec, timeout)
		}, issues))
	}
	results = append(results, e.doctorLiveRunOne("Browser", func() (doctorLiveProbeResult, error) {
		return e.doctorLiveProbeBrowser(timeout)
	}, issues))
	servers, _ := cfg["mcp_servers"].(map[string]any)
	if len(servers) > 0 {
		names := make([]string, 0, len(servers))
		for name := range servers {
			names = append(names, name)
		}
		sort.Strings(names)
		for _, name := range names {
			results = append(results, e.doctorLiveRunOne("MCP: "+name, func() (doctorLiveProbeResult, error) {
				entry, ok := servers[name].(map[string]any)
				if !ok {
					return doctorLiveProbeResult{"MCP: " + name, "skip", "(malformed config entry)"}, nil
				}
				n, perr := e.probeMCPServer(name, entry, timeout)
				if perr != nil {
					return doctorLiveProbeResult{}, perr
				}
				return doctorLiveProbeResult{"MCP: " + name, "pass", fmt.Sprintf("(%d tool(s))", n)}, nil
			}, issues))
		}
	} else {
		results = append(results, doctorLiveProbeResult{"MCP", "skip", "(no servers configured)"})
		e.doctorLiveReport(results[len(results)-1], issues)
	}
	for _, kind := range []string{"tts", "stt"} {
		results = append(results, e.doctorLiveRunOne(pystr.Upper(kind), func() (doctorLiveProbeResult, error) {
			return e.doctorLiveProbeAudio(kind, cfg, timeout)
		}, issues))
	}
	return results, nil
}

// Runs the live probes only when the live flag is set, printing a failed run as a warning
func (e *doctorLiveEnv) doctorMaybeRunLiveChecks(ns map[string]any, issues *[]string) []doctorLiveProbeResult {
	if !pyrepr.Truthy(ns["live"]) {
		return nil
	}
	results, err := e.doctorRunLiveChecks(issues)
	if err != nil {
		doctorCheckWarn(e.d.W, "Live backend probes crashed", "("+err.Error()+")")
		return nil
	}
	return results
}
